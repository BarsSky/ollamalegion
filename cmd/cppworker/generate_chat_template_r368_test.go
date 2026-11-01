// generate_chat_template_r368_test.go — R368 (2026-10-31).
//
// Проверяется правило: /api/generate собирает prompt ШАБЛОНОМ модели (как
// Ollama), а прежнее подмешивание system/инструкции остаётся только fallback'ом
// для моделей без шаблона.
//
// Живой дефект, из которого правило выросло: на «сыром» prompt gemma-4 изредка
// отвечала `***` и завершалась за три токена (1/12 напрямую, 3/12 через
// балансер), тогда как /api/chat с шаблоном — 0/8.
package main

import (
	"go/ast"
	"testing"
)

// TestR368_FallbackKeepsLegacyBehaviour — ветка «шаблона нет» обязана совпадать
// с прежним поведением эндпоинта, иначе модели без шаблона получат регресс.
func TestR368_FallbackKeepsLegacyBehaviour(t *testing.T) {
	req := generateRequest{Model: "m", Prompt: "вопрос"}

	if got := generatePromptFallbackR368(req, false); got != "вопрос" {
		t.Errorf("без system и без thinking prompt = %q, want исходный", got)
	}

	reqSys := generateRequest{Model: "m", Prompt: "вопрос", System: "ты ассистент"}
	if got := generatePromptFallbackR368(reqSys, false); got != "ты ассистент\nвопрос" {
		t.Errorf("system не подмешан: %q", got)
	}

	got := generatePromptFallbackR368(reqSys, true)
	want := generateThinkingInstructionR368 + "\n\nты ассистент\nвопрос"
	if got != want {
		t.Errorf("инструкция рассуждений подмешана не так:\n got=%q\nwant=%q", got, want)
	}

	// raw:true — ни system, ни инструкции (это escape hatch Ollama).
	rawReq := generateRequest{Model: "m", Prompt: "вопрос", System: "s", Raw: true}
	if got := generatePromptFallbackR368(rawReq, true); got != "вопрос" {
		t.Errorf("raw:true не отключает подмешивание: %q", got)
	}
}

// TestR368_RawRequestBypassesTemplate — raw:true не должен даже пытаться собрать
// шаблон: клиент просит ровно свои токены.
func TestR368_RawRequestBypassesTemplate(t *testing.T) {
	req := generateRequest{Model: "m", Prompt: "продолжи: раз два", Raw: true}
	got, source := promptForGenerateR368("m", req, true)
	if got != req.Prompt {
		t.Errorf("raw prompt изменён: %q", got)
	}
	if source != promptSourceRawR368 {
		t.Errorf("source = %q, want %q", source, promptSourceRawR368)
	}
}

// TestR368_NoTemplateFallsBack — в тестовой сборке (stub-мост) шаблона нет,
// значит путь обязан уйти в fallback, а не отдать пустой prompt.
func TestR368_NoTemplateFallsBack(t *testing.T) {
	req := generateRequest{Model: "m", Prompt: "вопрос", System: "s"}
	got, source := promptForGenerateR368("m", req, false)
	if source != promptSourceFallbackR368 {
		t.Skipf("мост вернул шаблон (source=%q) — в stub-сборке ожидался fallback", source)
	}
	if got != "s\nвопрос" {
		t.Errorf("fallback prompt = %q, want %q", got, "s\nвопрос")
	}
}

// TestR368STEWARD_GenerateUsesAssembler — стюард: runGenerateCore обязан брать
// prompt из общего сборщика, а не отправлять req.Prompt напрямую. Иначе шаблон
// снова перестанет применяться на одном из двух обработчиков (/api/generate и
// /api/ollama/generate).
func TestR368STEWARD_GenerateUsesAssembler(t *testing.T) {
	fn, pos := findFuncR365(t, parsePackageFilesR365(t), "runGenerateCore")
	calls := callNamesR364(fn)
	if !calls["promptForGenerateR368"] {
		t.Errorf("%s: runGenerateCore отправляет prompt без сборщика R368.\n"+
			"    Тогда /api/generate снова уйдёт в llama.cpp сырым промптом —\n"+
			"    живой дефект: `response:\"***\"`, tokens=3, EOS за 146 мс (1/12 напрямую, 3/12 через балансер).", pos)
	}
}

// TestR368STEWARD_NormalizeDoesNotInjectPrompt — стюард: normalizeGenerateRequest
// не должен подмешивать system/инструкцию в prompt. Подмешивание живёт в
// fallback'е; если оно вернётся в нормализацию, модели с шаблоном получат
// инструкцию ДВАЖДЫ (в шаблоне и в user-сообщении), а тесты этого не заметят.
func TestR368STEWARD_NormalizeDoesNotInjectPrompt(t *testing.T) {
	fn, pos := findFuncR365(t, parsePackageFilesR365(t), "normalizeGenerateRequest")
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, lhs := range assign.Lhs {
			sel, ok := lhs.(*ast.SelectorExpr)
			if !ok {
				continue
			}
			if sel.Sel.Name == "Prompt" {
				found = true
			}
		}
		return true
	})
	if found {
		t.Errorf("%s: normalizeGenerateRequest снова присваивает Prompt.\n"+
			"    Подмешивание system/инструкции обязано остаться в generatePromptFallbackR368:\n"+
			"    иначе у модели с шаблоном инструкция окажется в промпте дважды.", pos)
	}
}
