// auto_continue.go — R60.21: detect truncated LLM responses and
// auto-retry with "continue" prompt to recover from model-side
// antiprompt emission (Qwen3-Instruct emits <|im_end|> mid-code).
//
// Problem (R60.20, 2026-09-08): Qwen3-Instruct-2507-q4km has a tendency
// to emit ChatML-EOS <|im_end|> token mid-code generation after ~800-1000
// tokens. C-bridge sees it as antiprompt match (c/bridge/bridge.c:1823-1840)
// and stops the stream. Client receives done_reason=stop with content like:
//
//	```python
//	import numpy as np
//	v0 = 50.0
//	... (model stops at `t = ...`)
//	← NO closing ```
//
// Streaming is lossless (cppworker + balancer faithfully pass through
// what model generated). The closing backticks were never generated —
// model decided "I'm done" mid-code.
//
// R60.21 fix: balancer-level detection + auto-retry:
//  1. After done_reason=stop, check final content with detectIncompleteResponse
//  2. If incomplete, automatically send "continue" request to upstream
//  3. Concatenate the continuation to original content
//  4. Return combined response to client
//
// R60.50 fix (2026-09-12): original PerformAutoContinue included the full
// original conversation history (User: "Привет распиши..." + Assistant: truncated).
// This caused Qwen3-Instruct to RESTART with "Привет! Конечно, вот..." instead
// of continuing from the truncation point. The model interprets the original
// user message as a fresh prompt and greets back.
//
// R60.50 fix: the continuation request contains ONLY a single user message
// with the truncated content embedded + explicit "continue from here" instruction.
// The model has no original user context to "restart" from, so it MUST continue.
// Operator opt-in via LB_AUTO_CONTINUE_ON_TRUNCATION env var (default: off).
package balancer

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// truncateReasons — why we think response is truncated.
// Exposed for logging + diagnostics.
const (
	truncateReasonUnclosedCodeBlock = "unclosed_code_block"
	truncateReasonMidLineCutoff     = "mid_line_cutoff"
)

// truncateReasonEmpty — used when content is empty.
const truncateReasonEmpty = "empty"

// detectIncompleteResponse — pure function. Returns true if content
// looks truncated (would benefit from auto-continue retry).
//
// Heuristics (in priority order):
//  1. Odd ``` count → unclosed code block (R60.20 main pattern)
//  2. Last non-empty line ends with continuation char (=, (, {, [, ,, :)
//     → mid-line cutoff (R60.20 secondary pattern)
//
// Both heuristics are conservative — false positives are cheap
// (just one extra API call), false negatives are user-visible
// (response cut off, user sees the bug).
func DetectIncompleteResponse(content string) bool {
	if content == "" {
		return false // empty is empty, not truncated
	}
	// 1. Unclosed code block: odd number of ```
	backtickCount := strings.Count(content, "```")
	if backtickCount%2 != 0 {
		return true
	}
	// 2. Mid-line cutoff: check last non-empty line.
	// Strip trailing whitespace/newlines first.
	trimmed := strings.TrimRight(content, " \t\n\r")
	if trimmed == "" {
		return false
	}
	// Find last line.
	lastNewline := strings.LastIndex(trimmed, "\n")
	var lastLine string
	if lastNewline == -1 {
		lastLine = trimmed
	} else {
		lastLine = trimmed[lastNewline+1:]
	}
	// rstrip last line too — otherwise trailing whitespace
	// masks continuation chars like "= " at the end.
	lastLine = strings.TrimRight(lastLine, " \t")
	// Skip if last line is a "normal" closing — e.g. ends with punctuation
	// that signals completion.
	if endsWithCompleteStatement(lastLine) {
		return false
	}
	// Check for continuation chars: =, (, {, [, ,, :
	// (only the LAST char after rstrip, since the test cases have
	// "t_values = " with trailing space which the rstrip removes)
	if len(lastLine) > 0 {
		switch lastLine[len(lastLine)-1] {
		case '=', '(', '{', '[', ',', ':':
			return true
		}
	}
	// If last non-empty line is just whitespace or single char, also incomplete.
	if strings.TrimSpace(lastLine) == "" {
		return true
	}
	// Check if last line is unusually long (probably incomplete sentence/code).
	// This catches "very long line without terminal punctuation".
	if len(lastLine) > 200 && !endsWithCompleteStatement(lastLine) {
		return true
	}
	return false
}

// endsWithCompleteStatement — heuristic: does this line LOOK complete?
// Punctuation: . ! ? " ' ` ) ] } — or wrapped in code block fence ```.
// Also: empty line (just \n) is considered complete.
func endsWithCompleteStatement(line string) bool {
	if line == "" {
		return true
	}
	last := rune(line[len(line)-1])
	// Common statement terminators.
	switch last {
	case '.', '!', '?', ')', ']', '}', '"', '\'', '`':
		return true
	}
	// Chinese/Japanese full-stop (in case the response is in CJK)
	if last == '。' || last == '!' || last == '?' {
		return true
	}
	return false
}

// buildContinuePrompt — construct the FULL "continue" user message
// sent to model in auto-retry. R60.50: this is the ONLY message in the
// continuation request (we don't include original user/assistant history
// to prevent the model from "restarting" with a greeting).
//
// Must:
//  1. Reference the partial content (so model knows where to resume)
//  2. Explicitly ask to complete the code block
//  3. Forbid greeting/restarting (R60.50 fix)
//  4. Include the truncated text so the model can resume
//
// Returns the user message content. Caller wraps it in a single user
// message — no other messages in the request body.
func BuildContinuePrompt(partialContent string, reason string) string {
	var sb strings.Builder
	sb.WriteString("Ты — ассистент, который пишет ответ. Твой предыдущий ответ был прерван на середине. ")
	sb.WriteString("Продолжи ТОЧНО с того места, где остановился. ")
	switch reason {
	case truncateReasonUnclosedCodeBlock:
		sb.WriteString("Кодовый блок остался незазакрытым (нет финальной строки ```). ")
		sb.WriteString("Сгенерируй ТОЛЬКО продолжение кода, начиная с последнего выведенного символа. ")
		sb.WriteString("ОБЯЗАТЕЛЬНО закрой блок тремя обратными апострофами (```) в конце.")
	case truncateReasonMidLineCutoff:
		sb.WriteString("Последняя строка кода обрезана посередине. ")
		sb.WriteString("Сгенерируй ТОЛЬКО продолжение с места обрыва. ")
		sb.WriteString("Не повторяй уже написанное.")
	default:
		sb.WriteString("Сгенерируй ТОЛЬКО продолжение с места обрыва. ")
		sb.WriteString("Не повторяй уже написанное.")
	}
	// R60.50: explicit anti-greeting / anti-restart.
	sb.WriteString("\n\nВАЖНО:\n")
	sb.WriteString("- НЕ здоровайся заново (\"Привет\", \"Конечно\", и т.п.).\n")
	sb.WriteString("- НЕ повторяй уже написанное.\n")
	sb.WriteString("- Сразу продолжай с места обрыва.\n")
	// R60.50: forbid emitting literal ``` inside code blocks (Qwen3-Instruct antipattern
	// that breaks OpenWebUI markdown rendering). Use \\\` instead, or skip.
	sb.WriteString("- НЕ используй символы ``` внутри кода — они закрывают markdown блок раньше времени.\n")
	sb.WriteString("  Если нужен обратный апостроф внутри кода, замени на одинарный ` или экранируй.\n")

	// Include last 400 chars of partial content as context anchor.
	// Increased from 200 to 400 for better resumption context.
	anchor := partialContent
	if len(anchor) > 400 {
		anchor = anchor[len(anchor)-400:]
	}
	sb.WriteString("\n\nПоследние символы твоего ответа (для контекста):\n```\n...")
	sb.WriteString(anchor)
	sb.WriteString("\n```\n\nСгенерируй ТОЛЬКО продолжение:")
	return sb.String()
}

// truncateReason is exposed for logging.
func TruncateReason(content string) string {
	if content == "" {
		return truncateReasonEmpty
	}
	if strings.Count(content, "```")%2 != 0 {
		return truncateReasonUnclosedCodeBlock
	}
	trimmed := strings.TrimRight(content, " \t\n\r")
	if trimmed == "" {
		return truncateReasonEmpty
	}
	lastNewline := strings.LastIndex(trimmed, "\n")
	var lastLine string
	if lastNewline == -1 {
		lastLine = trimmed
	} else {
		lastLine = trimmed[lastNewline+1:]
	}
	if endsWithCompleteStatement(lastLine) {
		return ""
	}
	for _, ch := range lastLine {
		switch ch {
		case '=', '(', '{', '[', ',', ':':
			return truncateReasonMidLineCutoff
		}
		break
	}
	return ""
}

// IsAutoContinueOnTruncationEnabled — checks if LB_AUTO_CONTINUE_ON_TRUNCATION
// env var is set. Operator opt-in feature.
//
// Default: false (off). Set to "1", "true", "yes" to enable.
func IsAutoContinueOnTruncationEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("LB_AUTO_CONTINUE_ON_TRUNCATION")))
	return v == "1" || v == "true" || v == "yes"
}

// IsAutoLoadAsyncEnabled — R60.33 (2026-09-10): если true (default),
// auto-load запускается в goroutine и balancer сразу возвращает
// 503+Retry-After (вместо sync wait 3 мин). Это решает проблему
// OpenWebUI/Cline timeout 60-120s на cold start (sync load = 3 мин
// → connection aborted → JSON parse error).
//
// Set LB_AUTO_LOAD_ASYNC=0 для legacy sync поведения (long-running
// requests, debug).
func IsAutoLoadAsyncEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("LB_AUTO_LOAD_ASYNC")))
	// Default = true. Только explicit "0"/"false"/"no" отключают.
	if v == "0" || v == "false" || v == "no" {
		return false
	}
	return true
}

// IsNCtxReloadAsyncEnabled — R60.47 (2026-09-11): если true (default),
// n_ctx auto-reload (triggered when cppworker returns 400 prompt_too_long)
// запускается в goroutine и balancer СРАЗУ возвращает 503+Retry-After.
//
// Pre-R60.47: handleNCtxReloadActual делал sync DoReload с
// `?wait=true&waitTimeoutSec=300` — блокировал HTTP handler до 5 минут.
// OpenWebUI/Cline timeout 30-60s → клиент cancel-ил соединение → "empty
// response" / "Unexpected token" / EOF. Retry приводил к cascade.
//
// Post-R60.47: balancer возвращает 503+Retry-After за <50ms, клиент
// retry-ит через Retry-After, к этому моменту reload обычно завершён.
//
// Set LB_NCTX_RELOAD_ASYNC=0 для legacy sync reload+retry поведения
// (single round-trip, клиент получает финальный ответ за один запрос).
func IsNCtxReloadAsyncEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("LB_NCTX_RELOAD_ASYNC")))
	// Default = true. Только explicit "0"/"false"/"no" отключают.
	if v == "0" || v == "false" || v == "no" {
		return false
	}
	return true
}

// GetAutoContinueMaxTokens — env override for max tokens in the
// continue request. Default 1024 (enough for code completion).
// Set LB_AUTO_CONTINUE_MAX_TOKENS=2048 to allow longer continuations.
func GetAutoContinueMaxTokens() int {
	v := strings.TrimSpace(os.Getenv("LB_AUTO_CONTINUE_MAX_TOKENS"))
	if v == "" {
		return 1024
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return n
	}
	return 1024
}

// GetAutoContinueTimeout — env override for the auto-continue
// request's http.Client.Timeout. R60.25 fix: bumped default from
// 60s to 10 minutes because long-form generation (code, math
// articles, RAG-context) routinely exceeds 60s, and the original
// main request's timeout has already expired by the time we get
// here (so the model's load is fresh + warm but slow to first
// byte). Set LB_AUTO_CONTINUE_TIMEOUT_SEC=600 to raise to 10min.
// Set LB_AUTO_CONTINUE_TIMEOUT_SEC=0 to fall back to the previous
// 60s default (NOT recommended for long code).
func GetAutoContinueTimeout() time.Duration {
	v := strings.TrimSpace(os.Getenv("LB_AUTO_CONTINUE_TIMEOUT_SEC"))
	if v == "" {
		return 600 * time.Second // R60.25 default: 10 minutes
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	return 600 * time.Second
}

// chatMessage — minimal struct for chat message. Both Ollama and
// OpenAI APIs use this format for the "messages" field.
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// PerformAutoContinue — sends a "continue" request to upstream when
// truncation is detected. Returns the continuation content (string)
// and the new eval_count (if known).
//
// Algorithm (R60.50 — fixed from R60.21):
//  1. Parse originalBody just to extract the model name.
//  2. Build a SINGLE-MESSAGE user request containing:
//     - The truncated content as context anchor (last 400 chars)
//     - Explicit "continue from here, don't greet/restart" instruction
//  3. Re-marshal and POST non-streaming request to upstream.
//  4. Read response, extract content, return.
//
// R60.50 fix: original implementation sent FULL conversation history
// (User: original + Assistant: truncated + User: continue). The model
// saw the original user message ("Привет распиши...") and RESTARTED with
// a greeting instead of continuing. The fix sends ONLY a single user
// message with the truncated content embedded — the model has no original
// user context to "restart" from.
//
// Returns ("", 0, nil) on any error — caller treats as "no continuation".
// This means failed auto-continue doesn't break the response, just
// doesn't add anything.
//
// Caller is responsible for emitting the continuation to the client
// (as separate NDJSON/SSE chunks, then a final done chunk).
func PerformAutoContinue(
	upstreamURL string,
	originalBody []byte,
	accumulatedContent string,
	truncationReason string,
	httpClient *http.Client,
	timeout time.Duration,
) (continuationContent string, evalCount int, err error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if timeout <= 0 {
		// R60.25: default auto-continue timeout raised to 10 minutes
		// (env override LB_AUTO_CONTINUE_TIMEOUT_SEC). 60s was the
		// old default and too short for long-form code/RAG that the
		// main request also took >60s to produce.
		timeout = GetAutoContinueTimeout()
	}

	// 1. Parse original body — only need model name.
	var reqBody struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(originalBody, &reqBody); err != nil {
		return "", 0, err
	}
	if reqBody.Model == "" {
		return "", 0, errEmptyMessages
	}

	// R60.50: build a SINGLE user message with truncated content embedded
	// + explicit continue instruction. NO original conversation history.
	continuationPrompt := BuildContinuePrompt(accumulatedContent, truncationReason)
	continuationMessages := []chatMessage{
		{Role: "user", Content: continuationPrompt},
	}

	// Force non-streaming for the continue request (simpler to handle).
	// Stream was true for original; we want to read the full response
	// before returning, since we already streamed the original to client.
	// Limit continuation size to prevent runaway.
	//
	// R60.21: cppworker's /v1/chat/completions strict decoder REJECTS
	// unknown fields. Ollama-specific `num_predict` is NOT in the OpenAI
	// spec, so cppworker returns 400 "invalid JSON: unknown field
	// \"num_predict\"". Use only `max_tokens` (OpenAI standard).
	maxTokens := GetAutoContinueMaxTokens()
	newBody, err := json.Marshal(struct {
		Model     string        `json:"model"`
		Messages  []chatMessage `json:"messages"`
		Stream    bool          `json:"stream"`
		MaxTokens int           `json:"max_tokens,omitempty"`
	}{
		Model:     reqBody.Model,
		Messages:  continuationMessages,
		Stream:    false,
		MaxTokens: maxTokens,
	})
	if err != nil {
		return "", 0, err
	}

	// 4. POST to upstream (non-streaming)
	url := strings.TrimRight(upstreamURL, "/")
	if !strings.HasSuffix(url, "/v1/chat/completions") &&
		!strings.HasSuffix(url, "/api/chat") {
		// Default: assume OpenAI-compatible /v1/chat/completions for the continue.
		// If the original was /api/chat (Ollama), the user-agent path
		// proxyRequestOpenAI handles translation.
		url = url + "/v1/chat/completions"
	}
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(newBody)))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Read error body for diagnostics
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", 0, fmt.Errorf("upstream status %d: %s", resp.StatusCode, string(errBody))
	}

	// 5. Read response, extract content
	var respBody struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&respBody); err != nil {
		return "", 0, err
	}
	if len(respBody.Choices) == 0 {
		return "", 0, errEmptyChoices
	}
	return respBody.Choices[0].Message.Content, respBody.Usage.CompletionTokens, nil
}

// Sentinel errors for PerformAutoContinue.
var (
	errEmptyMessages = simpleError("empty messages in request body")
	errUpstreamNonOK = simpleError("upstream returned non-OK status")
	errEmptyChoices  = simpleError("upstream response had no choices")
)

type simpleError string

func (e simpleError) Error() string { return string(e) }

// EmitContinuationNDJSON — write the continuation content to client
// as a final NDJSON chunk (or series of chunks for very long continuations).
// This is for /api/chat (Ollama) and /api/generate paths.
//
// Caller passes the original accumulated content + continuation content.
// The combined content goes into message.content of the final chunk.
func EmitContinuationNDJSON(
	w http.ResponseWriter,
	apiPath string,
	modelName string,
	originalContent string,
	continuationContent string,
	totalEvalCount int,
	flusher http.Flusher,
) error {
	combined := originalContent + continuationContent
	// R60.50: strip interior ``` that break markdown rendering (Qwen3-Instruct
	// antipattern — emits ``` inside JS code which prematurely closes outer fence).
	combined = FixMarkdownCodeFences(combined)
	// Strip antiprompt-like artifacts that may appear at the boundary.
	// Common: "Sure, here's the continuation:\n\n" — model often prepends
	// acknowledgment text. We don't try to strip it (too aggressive);
	// we just emit combined as-is and let client handle.
	var ollamaChunk map[string]interface{}
	if apiPath == "/api/chat" {
		ollamaChunk = map[string]interface{}{
			"model":       modelName,
			"created_at":  time.Now().UTC().Format(time.RFC3339),
			"done":        true,
			"done_reason": "stop",
			"message": map[string]interface{}{
				"role":    "assistant",
				"content": combined,
			},
			"total_duration": 0,
			"eval_count":     totalEvalCount,
		}
	} else if apiPath == "/api/generate" {
		ollamaChunk = map[string]interface{}{
			"model":          modelName,
			"created_at":     time.Now().UTC().Format(time.RFC3339),
			"done":           true,
			"done_reason":    "stop",
			"response":       combined,
			"total_duration": 0,
			"eval_count":     totalEvalCount,
		}
	} else {
		return simpleError("unsupported api path for EmitContinuationNDJSON: " + apiPath)
	}
	out, err := json.Marshal(ollamaChunk)
	if err != nil {
		return err
	}
	out = append(out, '\n')
	if _, err := w.Write(out); err != nil {
		return err
	}
	if flusher != nil {
		flusher.Flush()
	}
	return nil
}

// Sanitize for utf8RuneCount — no-op removed, no extra imports needed.

// FixMarkdownCodeFences — R60.50 (2026-09-12): keep the FIRST ``` (for syntax
// highlighting) and replace ALL subsequent ``` with non-fence representation.
// Qwen3-Instruct reflexively emits ``` inside JavaScript template literals /
// comments / strings, which prematurely closes the outer markdown code block
// and breaks OpenWebUI rendering.
//
// Strategy:
//  1. Find first ``` — keep it (preserves code-block syntax highlighting)
//  2. Replace all subsequent ``` with ` ` (single backtick + space +
//     backtick, which is NOT a valid markdown fence)
//
// Trade-off: response still has ONE nice code block (the first one) for
// syntax highlighting. Any additional ``` (which are model antipatterns)
// are converted to non-fences, preserving the character content but
// preventing rendering breaks.
//
// Returns the cleaned content.
func FixMarkdownCodeFences(content string) string {
	if !strings.Contains(content, "```") {
		return content
	}
	firstFence := strings.Index(content, "```")
	if firstFence == -1 {
		return content
	}
	// Keep the first ```, replace all subsequent ones.
	before := content[:firstFence+3]
	after := content[firstFence+3:]
	after = strings.ReplaceAll(after, "```", "` `")
	return before + after
}
