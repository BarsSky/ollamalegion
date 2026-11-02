//go:build llama_stub

// terminal_budget_wiring_r402_test.go — R402 (2026-11-05).
//
// РЕГРЕСС-СТОРОЖ КЛАССА R394. Живой дефект: обрезанный ответ уходил клиенту с
// finish_reason/done_reason = "stop", потому что бюджет вывода считался по
// числу ВЫЗОВОВ callback'а (safeStreamWriter.TokensSent), а не по токенам.
// Callback'ов меньше, чем токенов (живой факт: num_predict=16 → eval_count=17
// при 14 callback'ах), поэтому проверка «упёрлись в кап» не срабатывала.
//
// R394 починил native /api/chat, но OpenAI-путь и /api/generate остались на
// счётчике callback'ов — Cline (он ходит OpenAI-совместимым и native маршрутом)
// продолжал получать "stop" на обрезанном ответе.
//
// Этот тест ЧИТАЕТ ИСХОДНИКИ и падает, если кто-то вернёт счётчик callback'ов в
// решение о причине завершения или перестанет класть в eval_count все
// сгенерированные токены. Поведенчески проверить это без живой модели нельзя
// (нужен bridge), поэтому сторож текстовый — как source_hygiene тесты.
package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func readMainSourceR402(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("не читается %s: %v", name, err)
	}
	return string(b)
}

// TestR402_NoCallbackCounterForBudget — решение об усечении не имеет права
// опираться на sw.TokensSent().
func TestR402_NoCallbackCounterForBudget(t *testing.T) {
	openai := readMainSourceR402(t, "handlers_openai.go")
	if strings.Contains(openai, "TokensSent() >= int64(params.NPredict)") {
		t.Fatal("handlers_openai.go: бюджет снова считается по callback'ам " +
			"(sw.TokensSent()) — это класс R394: callback'ов меньше, чем токенов")
	}
	if !strings.Contains(openai, "toolsBudgetExhaustedR394(params.NPredict, generatedTokens)") {
		t.Fatal("handlers_openai.go: усечение обязано считаться через " +
			"toolsBudgetExhaustedR394(params.NPredict, generatedTokens) по сырому выводу")
	}
	if !regexp.MustCompile(`generatedTokens\s*:=\s*countTokensSafe\(modelName,\s*fullOutput\)`).MatchString(openai) {
		t.Fatal("handlers_openai.go: generatedTokens должен считаться токенизатором по fullOutput")
	}

	gen := readMainSourceR402(t, "handlers_generate.go")
	if !strings.Contains(gen, "toolsBudgetExhaustedR394(params.NPredict, generatedTokens)") {
		t.Fatal("handlers_generate.go: причина завершения обязана учитывать исчерпанный бюджет " +
			"(как в R394 для /api/chat)")
	}
	// Две streaming-поверхности (/api/generate и /api/ollama/generate) — в каждой
	// токены обязаны считаться токенизатором по сырому выводу.
	if got := strings.Count(gen, "generatedTokens := countTokensSafe(modelName, fullOutput)"); got < 2 {
		t.Fatalf("handlers_generate.go: generatedTokens по токенизатору найден %d раз(а), want 2 "+
			"(обе streaming-ветки)", got)
	}
	if strings.Contains(gen, "\"eval_count\":        tokens") {
		t.Fatal("handlers_generate.go: одна из streaming-веток снова кладёт в eval_count " +
			"счётчик callback'ов (tokens) вместо generatedTokens")
	}
}

// TestR402_EvalCountIncludesReasoning — eval_count обязан включать размышления,
// иначе клиент недооценивает заполненность контекста (симптом «видно не то
// значение» + «увеличьте контекст вручную»).
func TestR402_EvalCountIncludesReasoning(t *testing.T) {
	chat := readMainSourceR402(t, "handlers_chat.go")
	if !regexp.MustCompile(`"eval_count":\s*generatedTokens`).MatchString(chat) {
		t.Fatal("handlers_chat.go: eval_count обязан быть generatedTokens (сырой вывод, включая reasoning), " +
			"а не подсчёт по видимому тексту")
	}
	if strings.Contains(chat, "countTokensSafe(modelName, cleanedOutput)") {
		t.Fatal("handlers_chat.go: вернулся подсчёт eval_count по видимому выводу без размышлений")
	}

	gen := readMainSourceR402(t, "handlers_generate.go")
	if !regexp.MustCompile(`"eval_count":\s*generatedTokens`).MatchString(gen) {
		t.Fatal("handlers_generate.go: eval_count обязан быть generatedTokens")
	}
	if got := strings.Count(gen, "float64(generatedTokens) / duration.Seconds()"); got < 2 {
		t.Fatalf("handlers_generate.go: tokens_per_second по токенизатору найден %d раз(а), want 2 "+
			"(обе streaming-ветки)", got)
	}
}
