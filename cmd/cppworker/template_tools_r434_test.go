//go:build llama_stub

// template_tools_r434_test.go — R434 (2026-11-05): инструменты уходят в chat
// template, преамбула — только fallback.
//
// Сам рендер шаблона проверяется живьём (в stub-сборке шаблона нет), здесь
// фиксируются два контракта, которые можно проверить без C:
//  1. рубильник CPPWORKER_TEMPLATE_TOOLS: по умолчанию native, "prompt" —
//     прежнее поведение;
//  2. если шаблон инструменты НЕ отрисовал (stub = шаблона нет вовсе), сборка
//     промпта всё равно обязана дойти до модели со списком инструментов —
//     иначе модель о них не узнает и ответит текстом.
package main

import (
	"strings"
	"testing"
)

// TestR434_TemplateToolsModeSwitch — рубильник.
func TestR434_TemplateToolsModeSwitch(t *testing.T) {
	t.Setenv("CPPWORKER_TEMPLATE_TOOLS", "")
	if mode := templateToolsMode(); mode != templateToolsNative {
		t.Errorf("по умолчанию режим = %v, ожидался native", mode)
	}
	for _, v := range []string{"native", "on", "1", "true", "NATIVE", " неизвестно "} {
		t.Setenv("CPPWORKER_TEMPLATE_TOOLS", v)
		if mode := templateToolsMode(); mode != templateToolsNative {
			t.Errorf("%q: режим = %v, ожидался native", v, mode)
		}
	}
	for _, v := range []string{"prompt", "legacy", "off", "0", "false", "no", "PROMPT"} {
		t.Setenv("CPPWORKER_TEMPLATE_TOOLS", v)
		if mode := templateToolsMode(); mode != templateToolsPrompt {
			t.Errorf("%q: режим = %v, ожидался prompt", v, mode)
		}
	}
}

// TestR434_FallbackStillReachesModelWithTools — шаблона нет (stub) → преамбула
// в промпте есть, то есть инструменты до модели доезжают.
func TestR434_FallbackStillReachesModelWithTools(t *testing.T) {
	t.Setenv("CPPWORKER_TEMPLATE_TOOLS", "")
	tools := formatTestTools()
	msgs := []chatMessage{{Role: "user", Content: "Прочитай TASK.md"}}

	prompt, err := buildChatPromptWithOptions(msgs, "gemma-4-test", chatPromptOptions{Tools: tools})
	if err != nil {
		t.Fatalf("сборка промпта: %v", err)
	}
	if prompt == "" {
		t.Fatal("пустой промпт")
	}
	for _, want := range []string{"read_files", "editor"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("в промпте нет инструмента %q — модель его не увидит:\n%s", want, prompt)
		}
	}
}

// TestR434_PromptModeKeepsOldBehaviour — рубильник "prompt" не ломает сборку.
func TestR434_PromptModeKeepsOldBehaviour(t *testing.T) {
	t.Setenv("CPPWORKER_TEMPLATE_TOOLS", "prompt")
	tools := formatTestTools()
	msgs := []chatMessage{{Role: "user", Content: "Прочитай TASK.md"}}

	prompt, err := buildChatPromptWithOptions(msgs, "gemma-4-test", chatPromptOptions{Tools: tools})
	if err != nil {
		t.Fatalf("сборка промпта: %v", err)
	}
	if !strings.Contains(prompt, "read_files") {
		t.Fatalf("в режиме prompt инструменты потеряны:\n%s", prompt)
	}
}
