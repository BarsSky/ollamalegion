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
// Operator opt-in via LB_AUTO_CONTINUE_ON_TRUNCATION env var (default: off).
package balancer

import (
	"encoding/json"
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

// buildContinuePrompt — construct the "continue" user message
// sent to model in auto-retry. Must:
//  1. Reference the partial content (so model knows where to resume)
//  2. Explicitly ask to complete the code block
//  3. Be short (don't confuse the model with verbose instructions)
//
// Returns the user message content (not the full request body).
// Caller wraps it in an assistant message + user message pair.
func BuildContinuePrompt(partialContent string, reason string) string {
	var sb strings.Builder
	sb.WriteString("Продолжи точно с того места, где остановился. ")
	switch reason {
	case truncateReasonUnclosedCodeBlock:
		sb.WriteString("Кодовый блок остался незакрытым (нет финальной строки ```). ")
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
	// Include last 200 chars of partial content as context anchor.
	anchor := partialContent
	if len(anchor) > 200 {
		anchor = anchor[len(anchor)-200:]
	}
	sb.WriteString("\n\nПоследние 200 символов твоего ответа (для контекста):\n```\n...")
	sb.WriteString(anchor)
	sb.WriteString("\n```")
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
// Algorithm:
//  1. Parse originalBody as a JSON object with "messages" field.
//     (Works for both /api/chat (Ollama) and /v1/chat/completions (OpenAI).)
//  2. Append assistant message with accumulatedContent.
//  3. Append user message with BuildContinuePrompt(accumulatedContent, reason).
//  4. Re-marshal and POST non-streaming request to upstream.
//  5. Read response, extract content, return.
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
		timeout = 60 * time.Second
	}

	// 1. Parse original body
	var reqBody struct {
		Model    string        `json:"model"`
		Messages []chatMessage `json:"messages"`
		Stream   bool          `json:"stream"`
	}
	if err := json.Unmarshal(originalBody, &reqBody); err != nil {
		return "", 0, err
	}
	if len(reqBody.Messages) == 0 {
		return "", 0, errEmptyMessages
	}

	// 2-3. Append assistant + user messages
	continuationPrompt := BuildContinuePrompt(accumulatedContent, truncationReason)
	reqBody.Messages = append(reqBody.Messages,
		chatMessage{Role: "assistant", Content: accumulatedContent},
		chatMessage{Role: "user", Content: continuationPrompt},
	)
	// Force non-streaming for the continue request (simpler to handle).
	// Stream was true for original; we want to read the full response
	// before returning, since we already streamed the original to client.
	reqBody.Stream = false
	// If model accepts "max_tokens" / "num_predict", use the configured limit
	// to prevent runaway continuation.
	maxTokens := GetAutoContinueMaxTokens()
	newBody, err := json.Marshal(struct {
		Model      string        `json:"model"`
		Messages   []chatMessage `json:"messages"`
		Stream     bool          `json:"stream"`
		MaxTokens  int           `json:"max_tokens,omitempty"`
		NumPredict int           `json:"num_predict,omitempty"`
	}{
		Model:      reqBody.Model,
		Messages:   reqBody.Messages,
		Stream:     false,
		MaxTokens:  maxTokens,
		NumPredict: maxTokens,
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
		return "", 0, errUpstreamNonOK
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

// Sanitize for utf8RuneCount — no-op removed, no extra imports needed.
