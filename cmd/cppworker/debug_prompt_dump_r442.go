// debug_prompt_dump_r442.go — R442 (2026-11-05): ПОЛНЫЙ дамп входящего запроса
// и собранного промпта. Нужен, чтобы разбирать агентский цикл по фактам, а не по
// скриншотам клиента.
//
// ЗАЧЕМ. Живой симптом (Cline + gemma-4): модель на каждом ходу снова пытается
// «создать» файл, клиент отвечает «файл уже существует, old_text не передан», и
// так по кругу. Чтобы понять, ПОЧЕМУ модель не учитывает результат инструмента,
// нужно видеть две вещи по отдельности:
//
//  1. ЧТО ПРИСЛАЛ КЛИЕНТ (role:"tool" с результатом; id вызова; история);
//  2. ЧТО МЫ ОТПРАВИЛИ В МОДЕЛЬ после рендера шаблона (попал ли туда результат
//     инструмента и в каком виде, нет ли дублей assistant-контента).
//
// Раньше в логе была только сводка (prompt_chars/tokens, head/tail 512/256), и
// отличить «клиент не прислал результат» от «мы потеряли его при рендере» было
// невозможно.
//
// Включение (обе выключены по умолчанию — это содержимое переписки):
//
//	CPPWORKER_DEBUG_PROMPT_FILE=/app/diag/prompt-last.txt   — собранный промпт
//	CPPWORKER_DEBUG_REQUEST_FILE=/app/diag/request-last.json — тело запроса
//	                                                           (messages+tools)
//
// Файлы перезаписываются на каждый запрос (последний ход), пишутся атомарно
// (через временный файл), права 0644: читает оператор из контейнера.
package main

import (
	"os"
	"path/filepath"
	"strings"

	"ollama-loadbalancer/pkg/logger"
)

// dumpPromptToFileR442 — записать собранный промпт (если включено).
func dumpPromptToFileR442(model, endpoint, prompt string) {
	path := filepath.Clean(os.Getenv("CPPWORKER_DEBUG_PROMPT_FILE"))
	if path == "" || path == "." {
		return
	}
	writeFileAtomicR442(path, []byte(prompt))
	if l := logger.Get(); l != nil {
		l.Infow("prompt dumped (CPPWORKER_DEBUG_PROMPT_FILE)",
			"model", model, "endpoint", endpoint, "file", path,
			"prompt_chars", len(prompt), "prompt_lines", countLinesR442(prompt),
			"tool_results", countOccurrencesR442(prompt, "<tool_response>")+
				countOccurrencesR442(prompt, "tool_call_result")+
				countOccurrencesR442(prompt, `"role": "tool"`))
	}
}

// dumpRequestBodyR442 — записать тело запроса клиента (если включено).
func dumpRequestBodyR442(model, endpoint string, body []byte) {
	path := filepath.Clean(os.Getenv("CPPWORKER_DEBUG_REQUEST_FILE"))
	if path == "" || path == "." {
		return
	}
	writeFileAtomicR442(path, body)
	if l := logger.Get(); l != nil {
		l.Infow("request body dumped (CPPWORKER_DEBUG_REQUEST_FILE)",
			"model", model, "endpoint", endpoint, "file", path, "bytes", len(body))
	}
}

// writeFileAtomicR442 — запись через временный файл, чтобы читатель никогда не
// увидел половину.
func writeFileAtomicR442(path string, data []byte) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// countLinesR442 — число строк промпта (для «длинная история?»).
func countLinesR442(s string) int {
	n := 1
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			n++
		}
	}
	return n
}

// countOccurrencesR442 — сколько раз подстрока встречается (для подсчёта
// результатов инструмента в промпте).
func countOccurrencesR442(s, sub string) int {
	if sub == "" {
		return 0
	}
	n, from := 0, 0
	for {
		i := indexFromR442(s, sub, from)
		if i < 0 {
			return n
		}
		n++
		from = i + len(sub)
	}
}

func indexFromR442(s, sub string, from int) int {
	if from >= len(s) {
		return -1
	}
	for i := from; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// debugRequestDumpEnabledR442 — включён ли дамп запроса (дешёвая проверка ДО
// маршалинга тела).
func debugRequestDumpEnabledR442() bool {
	return strings.TrimSpace(os.Getenv("CPPWORKER_DEBUG_REQUEST_FILE")) != ""
}

// debugPromptDumpEnabledR442 — включён ли дамп промпта.
func debugPromptDumpEnabledR442() bool {
	return strings.TrimSpace(os.Getenv("CPPWORKER_DEBUG_PROMPT_FILE")) != ""
}
