// per_model_reasoning_r83_test.go — R83/v67 (2026-10-02).
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА: «флаг включить размышление был активен, однако при
// ответе размышление не применилось».
//
// Что происходило на стенде:
//  1. Галочка «размышления» в WebUI пишется в профиль модели
//     (enableReasoning=true) и честно доезжает до загрузки:
//     /api/models показывает reasoning_enabled=true;
//  2. НО сборка промпта (buildChatPromptWithOptions) смотрела только на
//     ГЛОБАЛЬНЫЙ config.EnableReasoning (по умолчанию false) и per-model
//     состояние (inst.reasoningEnabled) не читала вовсе;
//  3. итог: ни native enable_thinking, ни soft-инструкция не применялись —
//     проверено на стенде через /api/v1/cppworker/debug/last-prompt (промпт
//     приходил вообще без thinking-инструкции).
//
// Тесты ниже фиксируют приоритет: явный think клиента > per-model (WebUI) >
// глобальный config.EnableReasoning.
package main

import (
	"strings"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

// TestPerModelReasoningEnabled_ReadsInstance — флаг читается из состояния
// загруженной модели (именно его выставляет профиль/галочка WebUI).
func TestPerModelReasoningEnabled_ReadsInstance(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()

	backend = newTestBackend()
	const model = "gemma-4-E4B-it-Q4_K_M"

	if PerModelReasoningEnabled(model) {
		t.Fatal("до инжекта модель не загружена — ожидали false")
	}
	if err := backend.InjectLoadedModelForTest(model); err != nil {
		t.Fatalf("InjectLoadedModelForTest: %v", err)
	}
	if PerModelReasoningEnabled(model) {
		t.Fatal("инжектированная модель без enableReasoning — ожидали false")
	}
	if err := backend.SetModelReasoningEnabledForTest(model, true); err != nil {
		t.Fatalf("SetModelReasoningEnabledForTest: %v", err)
	}
	if !PerModelReasoningEnabled(model) {
		t.Fatal("enableReasoning=true в профиле обязан читаться как включённый reasoning")
	}
	// nil-safe и чужая модель.
	backend = nil
	if PerModelReasoningEnabled(model) {
		t.Fatal("при backend=nil ожидали false (nil-safe)")
	}
}

// TestPerModelReasoningEnabled_IgnoresWhitelist — функция НЕ должна включать
// reasoning по одному лишь имени модели: иначе thinking включался бы всем
// gemma-4 подряд, даже когда галочка снята (это и есть отличие от
// IsReasoningEnabledForRequest, который для парсера, а не для сборки промпта).
func TestPerModelReasoningEnabled_IgnoresWhitelist(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackend()

	// gemma-4 в whitelist по имени, но конкретная модель не загружена.
	if IsReasoningModel("gemma-4-E4B-it-Q4_K_M") != true {
		t.Fatal("тест сломан: gemma-4 должен матчиться whitelist'ом")
	}
	if PerModelReasoningEnabled("gemma-4-E4B-it-Q4_K_M") {
		t.Fatal("whitelist по имени НЕ должен считаться «оператор включил размышления»")
	}
}

// TestBuildChatPrompt_PerModelReasoning_InjectsThinking — главный регресс-тест
// жалобы: при включённой галочке WebUI (per-model) и ВЫКЛЮЧЕННОМ глобальном
// config.EnableReasoning промпт обязан содержать thinking-инструкцию.
func TestBuildChatPrompt_PerModelReasoning_InjectsThinking(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackend()

	const model = "gemma-4-E4B-it-Q4_K_M"
	if err := backend.InjectLoadedModelForTest(model); err != nil {
		t.Fatalf("InjectLoadedModelForTest: %v", err)
	}

	// Глобальный config выключен — раньше этого было достаточно, чтобы
	// thinking не применился вообще.
	prevCfg := currentConfig
	defer func() { currentConfig = prevCfg }()
	cfg := cppbackend.Config{}
	if prevCfg != nil {
		cfg = *prevCfg
	}
	cfg.EnableReasoning = false
	currentConfig = &cfg

	msgs := []chatMessage{{Role: "user", Content: "Сколько будет 17*23?"}}

	promptWithout, err := buildChatPromptWithOptions(msgs, model, chatPromptOptions{})
	if err != nil {
		t.Fatalf("buildChatPromptWithOptions (выключено): %v", err)
	}
	if containsThinkingInstruction(promptWithout) {
		t.Fatal("при выключенной галочке thinking-инструкция попадать в промпт не должна")
	}

	if err := backend.SetModelReasoningEnabledForTest(model, true); err != nil {
		t.Fatalf("SetModelReasoningEnabledForTest: %v", err)
	}
	promptWith, err := buildChatPromptWithOptions(msgs, model, chatPromptOptions{})
	if err != nil {
		t.Fatalf("buildChatPromptWithOptions (включено): %v", err)
	}
	if !containsThinkingInstruction(promptWith) {
		t.Fatalf("галочка «размышления» включена, но thinking-инструкции в промпте нет.\n"+
			"Промпт: %q\n"+
			"Именно это оператор видел как «флаг был активен, однако размышление не применилось».",
			promptWith)
	}
}

// TestBuildChatPrompt_ExplicitThinkWinsOverPerModel — явный think:false клиента
// обязан перекрывать per-model флаг (контракт R66b).
func TestBuildChatPrompt_ExplicitThinkWinsOverPerModel(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackend()

	const model = "gemma-4-E4B-it-Q4_K_M"
	if err := backend.InjectLoadedModelForTest(model); err != nil {
		t.Fatalf("InjectLoadedModelForTest: %v", err)
	}
	if err := backend.SetModelReasoningEnabledForTest(model, true); err != nil {
		t.Fatalf("SetModelReasoningEnabledForTest: %v", err)
	}

	no := false
	prompt, err := buildChatPromptWithOptions(
		[]chatMessage{{Role: "user", Content: "2+2?"}}, model,
		chatPromptOptions{ReasoningOverride: &no})
	if err != nil {
		t.Fatalf("buildChatPromptWithOptions: %v", err)
	}
	if containsThinkingInstruction(prompt) {
		t.Fatal("явный think:false клиента обязан перекрывать per-model флаг")
	}
}

// containsThinkingInstruction — маркер soft-инструкции (thinking_lang.go):
// требование обернуть рассуждение в теги <reasoning>.
func containsThinkingInstruction(prompt string) bool {
	return strings.Contains(prompt, "<reasoning>")
}
