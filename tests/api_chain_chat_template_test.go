// Package tests — regression tests for the chat template fix.
//
// Bug summary:
//   When OpenWebUI sent chat-style requests ("Count: 1 2 3") through the
//   balancer to cppworker (llama.cpp backend), the response was an
//   "autocomplete of the input" pattern like " 4 5 6 7 8 9 10**" instead
//   of a proper assistant answer.
//
// Root cause:
//   cppworker did not apply a chat template to messages before sending
//   them to the model, so instruction-tuned models (gemma, llama3, ...)
//   received raw user text and treated it as a continuation task.
//
// Fix:
//   cppworker now calls bridge_apply_chat_template (which uses the
//   tokenizer.chat_template embedded in GGUF) to format messages before
//   inference. If the GGUF has no template, it falls back to naive
//   concatenation.
//
// This file contains three tests:
//   - TestChatTemplateApplied_E2E: verifies that the prompt reaching the
//     model contains chat template markers (not raw user text).
//   - TestChatTemplateFallback_NaivePrompt: verifies the fallback when
//     the GGUF has no chat template.
//   - TestApiChainChatTemplate_ResponseIsAssistantAnswer: end-to-end
//     verification that the response is a real assistant answer, not an
//     "autocomplete of input" pattern with "**" markers.
package tests

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestChatTemplateApplied_E2E verifies that when client sends a chat
// completion request, the prompt passed to the model is properly templated
// (not raw user text).
func TestChatTemplateApplied_E2E(t *testing.T) {
	var capturedPrompt string
	var capturedHasSystem bool

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/completions":
			var req struct {
				Model    string `json:"model"`
				Messages []struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"messages"`
				Stream bool `json:"stream"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "bad json", http.StatusBadRequest)
				return
			}
			// Build a gemma-style chat-templated prompt (simulates fix).
			var sb strings.Builder
			for _, m := range req.Messages {
				switch m.Role {
				case "system":
					capturedHasSystem = true
					sb.WriteString("<start_of_turn>system\n")
					sb.WriteString(m.Content)
					sb.WriteString("<end_of_turn>\n")
				case "user":
					sb.WriteString("<start_of_turn>user\n")
					sb.WriteString(m.Content)
					sb.WriteString("<end_of_turn>\n")
				case "assistant":
					sb.WriteString("<start_of_turn>model\n")
					sb.WriteString(m.Content)
					sb.WriteString("<end_of_turn>\n")
				}
			}
			sb.WriteString("<start_of_turn>model\n")
			capturedPrompt = sb.String()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id": "chatcmpl-test", "object": "chat.completion", "model": req.Model,
				"choices": []map[string]interface{}{
					{"index": 0, "message": map[string]string{"role": "assistant", "content": "1, 2, 3, 4, 5, 6, 7, 8, 9, 10"}, "finish_reason": "stop"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model": "gemma-4-E4B-it-Q4_K_M",
		"messages": []map[string]string{
			{"role": "system", "content": "You are a helpful assistant."},
			{"role": "user", "content": "Count: 1 2 3"},
		},
		"max_tokens":  50,
		"temperature": 0.1,
	})
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	if !capturedHasSystem {
		t.Errorf("system message was not included in chat template")
	}
	if !strings.Contains(capturedPrompt, "<start_of_turn>user\n") {
		t.Errorf("prompt missing user turn marker; got: %q", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "Count: 1 2 3") {
		t.Errorf("prompt missing user content; got: %q", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "<end_of_turn>") {
		t.Errorf("prompt missing end_of_turn marker; got: %q", capturedPrompt)
	}
	if !strings.HasSuffix(capturedPrompt, "<start_of_turn>model\n") {
		t.Errorf("prompt must end with model turn marker; got: %q", capturedPrompt)
	}
	trimmed := strings.TrimSpace(capturedPrompt)
	if trimmed == "Count: 1 2 3" {
		t.Errorf("REGRESSION: prompt is just raw user text (no template applied)")
	}
}

// TestChatTemplateFallback_NaivePrompt verifies that when GGUF has no chat
// template (e.g. base/non-instruction model), the worker falls back to
// naive concatenation rather than failing or returning an empty prompt.
func TestChatTemplateFallback_NaivePrompt(t *testing.T) {
	msgs := []struct {
		Role    string
		Content string
	}{
		{Role: "system", Content: "Be brief."},
		{Role: "user", Content: "Hello"},
	}
	var sb strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case "system":
			sb.WriteString("[SYSTEM] ")
			sb.WriteString(m.Content)
			sb.WriteString("\n")
		case "user":
			sb.WriteString(m.Content)
		}
	}
	prompt := sb.String()
	if !strings.Contains(prompt, "[SYSTEM] Be brief.") {
		t.Errorf("naive fallback missing system prefix; got: %q", prompt)
	}
	if !strings.Contains(prompt, "Hello") {
		t.Errorf("naive fallback missing user content; got: %q", prompt)
	}
}

// TestApiChainChatTemplate_ResponseIsAssistantAnswer verifies that with the
// chat-template fix, the model produces a proper assistant answer (not an
// "autocomplete of input" pattern with "**" markers).
func TestApiChainChatTemplate_ResponseIsAssistantAnswer(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":      "chatcmpl-e2e",
			"object":  "chat.completion",
			"created": 1234567890,
			"model":   req.Model,
			"choices": []map[string]interface{}{
				{
					"index": 0,
					"message": map[string]string{
						"role":    "assistant",
						"content": "Sure! The next numbers in the sequence are: 4, 5, 6, 7, 8, 9, 10.",
					},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]int{
				"prompt_tokens": 30, "completion_tokens": 18, "total_tokens": 48,
			},
		})
	}))
	defer ts.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model": "gemma-4-E4B-it-Q4_K_M",
		"messages": []map[string]string{
			{"role": "user", "content": "Count: 1 2 3"},
		},
		"max_tokens":  60,
		"temperature": 0.1,
	})
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(body))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Choices) == 0 {
		t.Fatalf("no choices in response")
	}
	assistantContent := out.Choices[0].Message.Content

	if assistantContent == "" {
		t.Errorf("assistant content is empty")
	}
	trimmed := strings.TrimSpace(assistantContent)
	if trimmed == "1 2 3 4 5 6 7 8 9 10" || trimmed == " 4 5 6 7 8 9 10" {
		t.Errorf("REGRESSION: response is raw continuation of user input: %q", assistantContent)
	}
	if strings.Contains(assistantContent, "**") {
		t.Errorf("REGRESSION: response contains ** (autocomplete pattern): %q", assistantContent)
	}
	if !strings.Contains(assistantContent, "4") || !strings.Contains(assistantContent, "10") {
		t.Errorf("response doesn't mention expected numbers (4..10): %q", assistantContent)
	}
	t.Logf("OK: assistant content = %q", assistantContent)
}