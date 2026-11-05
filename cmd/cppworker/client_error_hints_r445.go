// client_error_hints_r445.go — R445 (2026-11-05): подсказка модели по ОШИБКАМ
// КЛИЕНТА, которые уже лежат в истории как результаты инструментов.
//
// ЖИВОЙ ЦИКЛ (Cline + Qwen3-4B, стенд). Клиент отвергает вызов инструмента, а
// модель повторяет его без изменений:
//
//	{"error":"Editor operation failed: d:\\cline-e2e-web\\index.html already exists,
//	 but `old_text` was omitted. … Do not re-send this call unchanged.","success":false}
//	{"error":"Editor input too large: new_text was 7036 characters, exceeding the
//	 recommended limit of 6000. Split the edit into smaller tool calls …"}
//
// Что показала диагностика R442 (дампы запроса и промпта):
//   - клиент присылает эти ошибки в role:"tool" — доставка результата корректна;
//   - в собранном промпте ошибки ЕСТЬ (already exists ×9, Do not re-send ×7);
//   - слабая модель их просто не применяет.
//
// ЧТО ДЕЛАЕМ. Не переписываем протокол и не выдумываем вызовов — добавляем в
// system короткую инструкцию, ПРИВЯЗАННУЮ к конкретной ошибке из истории:
//   - файл существует → сначала read_files, затем editor с ТОЧНЫМ old_text;
//   - new_text больше лимита клиента → дробим на несколько вызовов editor.
//
// Это подсказка поверх данных клиента, а не подмена данных: текст ошибки модель
// по-прежнему видит в истории. Включается по умолчанию, выключается
// CPPWORKER_CLIENT_HINTS=0.
package main

import (
	"os"
	"strings"
)

// Подстроки ошибок клиента, для которых у нас есть точная инструкция.
const (
	clientErrOldTextOmittedR445  = "old_text` was omitted"
	clientErrOldTextOmitted2R445 = "old_text was omitted"
	clientErrAlreadyExistsR445   = "already exists"
	clientErrTooLargeR445        = "editor input too large"
	clientErrTooLarge2R445       = "exceeding the recommended limit"
	clientErrResendR445          = "do not re-send this call unchanged"
)

// clientErrorHintsEnabledR445 — включены ли подсказки (по умолчанию да).
func clientErrorHintsEnabledR445() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CPPWORKER_CLIENT_HINTS"))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// clientErrorHintsR445 — блок инструкций под найденные в истории ошибки клиента.
//
// Возвращает "" если ошибок нет или подсказки выключены. Блок добавляется ОДИН
// раз на запрос: истории свойственно накапливать прошлые ошибки, и повторять
// инструкцию на каждую было бы шумом.
func clientErrorHintsR445(msgs []chatMessage) string {
	if !clientErrorHintsEnabledR445() || len(msgs) == 0 {
		return ""
	}
	var existsErr, tooLargeErr bool
	// Смотрим только результаты инструментов и только «хвост» истории: старые
	// ошибки уже отработаны, важны последние.
	seen := 0
	for i := len(msgs) - 1; i >= 0 && seen < 8; i-- {
		if !strings.EqualFold(msgs[i].Role, "tool") {
			continue
		}
		seen++
		low := strings.ToLower(msgs[i].Content)
		if strings.Contains(low, clientErrOldTextOmittedR445) ||
			strings.Contains(low, clientErrOldTextOmitted2R445) ||
			strings.Contains(low, clientErrAlreadyExistsR445) {
			existsErr = true
		}
		if strings.Contains(low, clientErrTooLargeR445) ||
			strings.Contains(low, clientErrTooLarge2R445) {
			tooLargeErr = true
		}
	}
	if !existsErr && !tooLargeErr {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("IMPORTANT — the tool results above contain rejections from the client:\n")
	if existsErr {
		sb.WriteString("- A file you tried to create already exists. To change an existing file you MUST call read_files on it first, then call editor with `old_text` set to the exact fragment from that file that `new_text` replaces. `old_text: \"\"` or a missing `old_text` will be rejected, and repeating the same call wastes the turn.\n")
	}
	if tooLargeErr {
		sb.WriteString("- Your `new_text` exceeded the client limit (~6000 characters). Split the work: create the file with the first small chunk, then extend it with further editor calls (old_text from the previous chunk, or `insert_line`). Never send one huge file in a single call.\n")
	}
	sb.WriteString("- Read the tool result text before your next call; do not repeat a call that was rejected.\n")
	return sb.String()
}
