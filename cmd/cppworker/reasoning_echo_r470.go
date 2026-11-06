// reasoning_echo_r470.go — R470 (2026-11-06): на агентском (tools) пути мы больше
// не навязываем клиенту размышления, о которых он не просил.
//
// ЖИВОЙ ДЕФЕКТ (Cline + gemma-4-E4B, /api/chat + tools, подтверждён историей
// запроса: /app/diag/request-last.json). Клиент присылал 6 инструментов и НЕ
// присылал `think`. Мы по умолчанию (`ReasoningWantedByClient(nil) == true`)
// стримили размышления дельтами `message.thinking`. Cline (провайдер ollama)
// кладёт их в ТЕКСТ сообщения ассистента: в истории следующего запроса
// assistant-сообщение длиной РОВНО в reasoning предыдущего хода и начинается с
// английского «The user is in 'act' mode…». То есть модель на следующем ходу
// видит собственные размышления как свой ПРЕДЫДУЩИЙ ОТВЕТ и продолжает в том же
// плановом голосе, смешивая языки.
//
// Снаружи это выглядит как «совмещённый ответ в двух режимах» и «обрыв с
// завершением»: контент хода с tool_calls очищается (так требует контракт), и в
// историю попадает только reasoning — без вызова, без ответа.
//
// ПРАВИЛО: размышления — внутреннее состояние модели; эхо клиенту нужно только
// тогда, когда он его ЯВНО попросил (`think:true`). Агентскому клиенту для
// выполнения вызова оно не нужно вовсе.
//
// Тумблер CPPWORKER_TOOLS_THINKING:
//
//	auto (по умолчанию) — эхо только при явном think:true;
//	on                 — прежнее поведение (эхо всегда);
//	off                — не эхо никогда.
package main

import (
	"os"
	"strings"

	"ollama-loadbalancer/pkg/logger"
)

// toolsThinkingEchoR470 — отдавать ли размышления клиенту на tools-пути.
func toolsThinkingEchoR470(thinkOverride *bool) bool {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("CPPWORKER_TOOLS_THINKING")))
	switch mode {
	case "on", "1", "true", "yes":
		return true
	case "off", "0", "false", "no":
		return false
	case "", "auto":
		// Клиент не просил — не навязываем. Просил явно (think:true) — отдаём.
		return thinkOverride != nil && *thinkOverride
	default:
		if logger.Get() != nil {
			logger.Get().Warnw("CPPWORKER_TOOLS_THINKING: неизвестное значение, используется auto",
				"value", mode)
		}
		return thinkOverride != nil && *thinkOverride
	}
}
