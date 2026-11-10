// tools_hold_r482.go — R482 (2026-11-10): удержание вывода на tools-пути не
// может быть бессрочным.
//
// ЖИВАЯ ЖАЛОБА (второй стенд, A10 ×2, Qwen3.8-27B, Cline): «замыкается в себя и
// не отдаёт сгенерированное, пока не сделает всё одним огромным куском».
//
// ПРИЧИНА. На tools-пути (writeChatStreamResponseWithTools) контент держится,
// пока открыт блок размышлений: R190 снимает удержание по ЗАКРЫВАЮЩЕМУ маркеру,
// R471 держит начало, если маркера нет (канал открыт шаблоном). Если шаблон
// 27B-модели использует маркер, которого нет в reasoningCloseMarkers, или модель
// вообще не закрывает канал до конца хода, удержание не снимается НИКОГДА:
// клиент видит тишину, а весь текст приходит финальным чанком. Это и есть
// «одним огромным куском».
//
// ЧТО ЗДЕСЬ. Сторож удержания: если удерживается дольше
// CPPWORKER_TOOLS_HOLD_MAX_SEC (default 20 c) или больше
// CPPWORKER_TOOLS_HOLD_MAX_BYTES (default 32 КБ), удержание снимается:
//   - уже накопленный кусок НЕ отдаётся (в нём могут быть размышления —
//     Cline сохраняет их как ответ, R470);
//   - все ПОСЛЕДУЮЩИЕ токены идут клиенту нормально, как content.
//
// Так ход снова становится потоковым: если модель после долгих размышлений
// выдаёт ответ, он уходит дельтами, а не одним куском в конце. Компромисс
// осознанный: хвост размышлений ПОСЛЕ порога может попасть в content — поэтому
// срабатывание пишется WARN-строкой и полем hold_released в сводке хода.
//
// CPPWORKER_TOOLS_HOLD_MAX_SEC=0 — сторож выключен (прежнее поведение R190/R471).
package main

import (
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	toolsHoldMaxBytesDefaultR482 = 32 * 1024
	toolsHoldMaxSecDefaultR482   = 20
)

// toolsHoldMaxBytesR482 — предел удерживаемого объёма.
func toolsHoldMaxBytesR482() int {
	raw := strings.TrimSpace(os.Getenv("CPPWORKER_TOOLS_HOLD_MAX_BYTES"))
	if raw == "" {
		return toolsHoldMaxBytesDefaultR482
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return toolsHoldMaxBytesDefaultR482
	}
	return n
}

// toolsHoldMaxHoldR482 — предел времени удержания. 0 = сторож выключен.
func toolsHoldMaxHoldR482() time.Duration {
	raw := strings.TrimSpace(os.Getenv("CPPWORKER_TOOLS_HOLD_MAX_SEC"))
	if raw == "" {
		return time.Duration(toolsHoldMaxSecDefaultR482) * time.Second
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return time.Duration(toolsHoldMaxSecDefaultR482) * time.Second
	}
	return time.Duration(n) * time.Second
}

// toolsHoldShouldReleaseR482 — пора ли снять удержание.
//
// Возвращает (снять, причина). Причина машинная: "hold_timeout" |
// "hold_bytes", чтобы её можно было искать в логах и архиве.
func toolsHoldShouldReleaseR482(holdSince time.Time, heldBytes int, now time.Time) (bool, string) {
	if holdSince.IsZero() {
		return false, ""
	}
	if maxBytes := toolsHoldMaxBytesR482(); maxBytes > 0 && heldBytes >= maxBytes {
		return true, "hold_bytes"
	}
	if maxDur := toolsHoldMaxHoldR482(); maxDur > 0 && now.Sub(holdSince) >= maxDur {
		return true, "hold_timeout"
	}
	return false, ""
}

// splitTerminalContentR482 — разрезать финальный «сырой» текст на части.
//
// ФИНАЛЬНЫЙ ФОЛБЭК (R83/v56) отдаёт нераспознанный текст одним
// emitContentDelta: при 8-30 КБ это выглядит как «ответ пришёл одним куском».
// Режем по границе руны на куски ~1 КБ и отдаём отдельными чанками с Flush
// (каждый emitContentDelta делает Flush) — клиент рисует прогрессивно.
func splitTerminalContentR482(text string, chunkRunes int) []string {
	if text == "" {
		return nil
	}
	if chunkRunes <= 0 {
		chunkRunes = 1024
	}
	runes := []rune(text)
	if len(runes) <= chunkRunes {
		return []string{text}
	}
	out := make([]string, 0, len(runes)/chunkRunes+1)
	for start := 0; start < len(runes); start += chunkRunes {
		end := start + chunkRunes
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[start:end]))
	}
	return out
}
