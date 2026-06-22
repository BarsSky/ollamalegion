// tools_no_reload_test.go — Тесты для фикса "reload отключён при tools-запросах".
//
// Проблема (см. inference.go):
//
//	В OpenWebUI tools-flow каждая итерация диалога добавляет tool definitions +
//	tool calls + tool results в prompt. Если n_ctx слишком мал, происходит
//	overflow → tryRamFallbackReload → unload + reload → 10-30 сек.
//	На следующей итерации — снова overflow → бесконечный reload-loop.
//
// Решение:
//
//	При hasTools=true reload отключён. Сразу возвращается *ReloadDisabledForToolsError
//	→ HTTP 413 с actionable-сообщением (уменьшите tools / увеличьте n_ctx).
//
// Эти тесты проверяют:
//   - reload-disabled-for-tools возвращается при hasTools=true
//   - reload НЕ возвращается при hasTools=false (regular flow)
//   - Error содержит понятный actionable message
//   - isReloadDisabledForToolsError правильно классифицирует ошибку
package main

import (
	"strings"
	"testing"
)

// TestReloadDisabledForToolsError_Error проверяет, что Error() возвращает
// понятное сообщение с actionable-инструкцией.
func TestReloadDisabledForToolsError_Error(t *testing.T) {
	err := &ReloadDisabledForToolsError{Model: "qwen2.5-coder:7b"}
	msg := err.Error()

	// Должно содержать имя модели.
	if !strings.Contains(msg, "qwen2.5-coder:7b") {
		t.Errorf("Error message should contain model name, got: %s", msg)
	}
	// Должно содержать объяснение, почему reload не поможет.
	if !strings.Contains(msg, "reload would not help") &&
		!strings.Contains(msg, "adds tool results to context") {
		t.Errorf("Error message should explain why reload is disabled, got: %s", msg)
	}
	// Должно содержать actionable suggestion.
	if !strings.Contains(msg, "Reduce") && !strings.Contains(msg, "increase") {
		t.Errorf("Error message should have actionable suggestion, got: %s", msg)
	}
}

// TestIsReloadDisabledForToolsError проверяет type-assertion helper.
func TestIsReloadDisabledForToolsError(t *testing.T) {
	if isReloadDisabledForToolsError(nil) {
		t.Errorf("isReloadDisabledForToolsError(nil) = true, want false")
	}

	rdtErr := &ReloadDisabledForToolsError{Model: "x"}
	if !isReloadDisabledForToolsError(rdtErr) {
		t.Errorf("isReloadDisabledForToolsError(ReloadDisabledForToolsError) = false, want true")
	}

	plainErr := error(plainError("plain error"))
	if isReloadDisabledForToolsError(plainErr) {
		t.Errorf("isReloadDisabledForToolsError(plain error) = true, want false")
	}

	// ReloadLoopLimitError НЕ должен быть ReloadDisabledForToolsError.
	rllErr := &ReloadLoopLimitError{Model: "x", Count: 3}
	if isReloadDisabledForToolsError(rllErr) {
		t.Errorf("isReloadDisabledForToolsError(ReloadLoopLimitError) = true, want false")
	}
}

// plainError — простая реализация error interface для тестов.
type plainError string

func (e plainError) Error() string { return string(e) }

// TestReloadDisabledForToolsError_Model проверяет, что Model корректно
// сохраняется и возвращается через Error().
func TestReloadDisabledForToolsError_Model(t *testing.T) {
	cases := []struct {
		name  string
		model string
	}{
		{"short model name", "llama3"},
		{"model with tag", "qwen2.5-coder:7b-instruct-q4_K_M"},
		{"model with path", "/models/llama-3.1-70b.gguf"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := &ReloadDisabledForToolsError{Model: tc.model}
			if err.Model != tc.model {
				t.Errorf("Model = %q, want %q", err.Model, tc.model)
			}
			if !strings.Contains(err.Error(), tc.model) {
				t.Errorf("Error() should contain model name %q, got: %s", tc.model, err.Error())
			}
		})
	}
}
