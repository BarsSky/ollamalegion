//go:build llama_stub

// r216_openai_buffered_reasoning_test.go — R216 (2026-10-21).
//
// ЖИВОЙ ДЕФЕКТ (жалоба оператора). На стенде с LB_IMAGE_TOOL_INJECT=off
// балансер всё равно объявлял инструмент (off разбирался как always, см.
// internal/balancer/image_tool_inject_mode_r216_test.go), поэтому каждый запрос
// OpenWebUI на /v1/chat/completions шёл в ветку «stream + tools». Эта ветка
// буферизирует ответ и отдавала результат ЦЕЛИКОМ в delta.content: модель с
// reasoning (gemma-4/Qwen3) писала `<think>...` прямо в тело ответа, и
// OpenWebUI показывал размышления как ответ без закрывающего маркера. При
// наличии tool_calls размышления пропадали вовсе.
//
// Тесты фиксируют контракт R216 на самих писателях SSE (без поднятия сервера и
// stub-модели): reasoning уходит отдельным delta.reasoning_content, в content
// остаётся только видимый ответ, role=assistant приходит один раз, а незакрытый
// reasoning не превращает ответ в пустышку.
package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// r216ParseSSE — распарсить SSE-тело в delta-объекты и собрать content/reasoning.
func r216ParseSSE(t *testing.T, raw string) (roleCount int, content, reasoning strings.Builder, finishReasons []string) {
	t.Helper()
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta        map[string]interface{} `json:"delta"`
				FinishReason *string                `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("невалидный SSE-чанк %q: %v", payload, err)
		}
		for _, c := range chunk.Choices {
			if c.Delta != nil {
				if _, ok := c.Delta["role"]; ok {
					roleCount++
				}
				if s, ok := c.Delta["content"].(string); ok {
					content.WriteString(s)
				}
				if s, ok := c.Delta["reasoning_content"].(string); ok {
					reasoning.WriteString(s)
				}
			}
			if c.FinishReason != nil {
				finishReasons = append(finishReasons, *c.FinishReason)
			}
		}
	}
	return roleCount, content, reasoning, finishReasons
}

// TestR216_StaticTextStream_SeparatesReasoning — главный регресс: размышления
// не попадают в content и не теряются.
func TestR216_StaticTextStream_SeparatesReasoning(t *testing.T) {
	rec := httptest.NewRecorder()
	writeStaticTextStream(rec, "gemma-4", "chatcmpl-1", 1,
		"сначала подумаю", "Видимый ответ.", "prompt", true)

	roleCount, content, reasoning, finish := r216ParseSSE(t, rec.Body.String())
	if strings.Contains(strings.ToLower(content.String()), "подумаю") {
		t.Errorf("размышления утекли в content: %q", content.String())
	}
	if got := reasoning.String(); got != "сначала подумаю" {
		t.Errorf("reasoning_content = %q, want %q", got, "сначала подумаю")
	}
	if got := content.String(); got != "Видимый ответ." {
		t.Errorf("content = %q, want %q", got, "Видимый ответ.")
	}
	if roleCount != 1 {
		t.Errorf("role=assistant пришла %d раз, want 1", roleCount)
	}
	if len(finish) == 0 || finish[len(finish)-1] != "stop" {
		t.Errorf("finish_reason = %v, want stop", finish)
	}
	if !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Error("поток не завершён [DONE]")
	}
}

// TestR216_StaticTextStream_UnclosedReasoningStaysInReasoning — незакрытый
// reasoning (обрезали max_tokens) обязан остаться в reasoning_content и НЕ
// дублироваться в content.
//
// R219 (2026-10-21) изменил политику: R91-фолбэк «незакрытый reasoning → content»
// на буферизированной ветке воспроизводил жалобу оператора (размышления в теле
// ответа). Видимого ответа нет — content пуст, это честно.
func TestR216_StaticTextStream_UnclosedReasoningStaysInReasoning(t *testing.T) {
	rec := httptest.NewRecorder()
	// reasoning без закрывающего маркера: видимого ответа нет вовсе.
	writeStaticTextStream(rec, "gemma-4", "chatcmpl-2", 1,
		"думаю и не закрываю", "", "prompt", false)

	_, content, reasoning, _ := r216ParseSSE(t, rec.Body.String())
	if strings.TrimSpace(reasoning.String()) == "" {
		t.Error("reasoning_content пуст при незакрытом reasoning")
	}
	if strings.TrimSpace(content.String()) != "" {
		t.Errorf("content = %q, want пусто: размышления не должны дублироваться в ответ", content.String())
	}
}

// TestR216_ToolCallsStream_KeepsReasoning — при tool_calls размышления раньше
// терялись полностью; теперь уходят отдельным чанком перед вызовом.
func TestR216_ToolCallsStream_KeepsReasoning(t *testing.T) {
	var one openAIToolCall
	one.ID = "call_1"
	one.Type = "function"
	one.Function.Name = "read_file"
	one.Function.Arguments = `{"path":"a.txt"}`
	calls := []openAIToolCall{one}

	rec := httptest.NewRecorder()
	writeToolCallsStream(rec, "gemma-4", "chatcmpl-3", 1, calls, "обдумываю вызов", "prompt", false)

	roleCount, _, reasoning, finish := r216ParseSSE(t, rec.Body.String())
	if got := reasoning.String(); got != "обдумываю вызов" {
		t.Errorf("reasoning_content = %q, want %q (иначе размышления теряются при tool_calls)", got, "обдумываю вызов")
	}
	if roleCount != 1 {
		t.Errorf("role=assistant пришла %d раз, want 1", roleCount)
	}
	if len(finish) == 0 || finish[len(finish)-1] != "tool_calls" {
		t.Errorf("finish_reason = %v, want tool_calls", finish)
	}
	if !strings.Contains(rec.Body.String(), "read_file") {
		t.Errorf("tool_call не попал в поток: %s", rec.Body.String())
	}
}
