// empty_output_retry_r83.go — R83/v59 (2026-10-01): повтор генерации, если модель
// не выдала ни одного байта.
//
// ЖИВОЙ ФАКТ. Пробник debug/streamprobe на запросе Cline (gemma-4-E4B-it-Q4_K_M,
// system-промпт Cline + 18 tools + plan-режим) иногда получает от модели
// мгновенный EOG: в логе cppworker
//
//	writeChatStreamResponseWithTools: inference вернула пустой вывод — отдаём
//	error-чанк, model=gemma-4-E4B-it-Q4_K_M, raw_len=0
//
// то есть не сгенерировано НИ ОДНОГО байта. Клиенту в этом случае уходил чанк
// «model produced an empty response (inference succeeded but output is empty)» —
// именно его показал Cline в первом сбое из журнала сессии
// (~/.cline/data/sessions/.../*.messages.json, сообщение 2).
//
// ПОЧЕМУ ПОВТОР. Сэмплирование стохастично (temperature > 0), и следующий проход
// обычно даёт нормальный ответ. Терять при повторе нечего: при raw_len=0 клиенту
// ещё не отправлено ни одного чанка — ни контентных дельт, ни финального.
//
// Отключается через CPPWORKER_EMPTY_OUTPUT_RETRY=false.
package main

import (
	"os"
	"strings"
)

// emptyOutputRetryEnabled — делать ли один повторный проход генерации, если
// модель вернула пустой вывод. По умолчанию включено.
func emptyOutputRetryEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CPPWORKER_EMPTY_OUTPUT_RETRY"))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}
