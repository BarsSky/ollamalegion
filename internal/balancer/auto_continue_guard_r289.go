// auto_continue_guard_r289.go — R289 (2026-10-29): auto-continue не должен
// повторять запрос, который заведомо не пройдёт по размеру контекста.
//
// ЖИВОЙ ДЕФЕКТ (стенд r282, окно 4096). Промпт был 3791 токен — почти всё окно.
// Апстрим ответил 413 `prompt too long`; балансер увидел ответ, похожий на
// оборванный (незакрытая строка), и запустил R60.21 auto-continue с причиной
// `mid_line_cutoff`. Продолжение, разумеется, тоже получило 413: мы дважды
// сходили в модель за заведомо обречённым ответом, а клиент так и не узнал
// настоящую причину — «контекст мал для этого промпта».
//
// ЧТО ЗДЕСЬ. Один предикат «это отказ по размеру контекста, а не обрыв» и один
// helper, который объясняет клиенту ситуацию числами. Предикат применяется ДО
// auto-continue: если он истинен, продолжение не запускается ни при какой
// эвристике — эвристика обрыва тут ни при чём.
package balancer

import (
	"fmt"
	"net/http"
	"strings"

	"ollama-loadbalancer/pkg/logger"
)

// contextOverflowMarkers — тексты, которыми cppworker/llama.cpp сообщают, что
// промпт не влез в окно. Регистр не важен (проверяем в нижнем регистре).
//
// Список намеренно узкий: «prompt too long», «exceeds … n_ctx», «context
// overflow», «prompt_too_long», код 3 моста (ErrCodePromptTooLong) в тексте.
// Широкие маркеры вроде «context» ловили бы обычные ответы про контекст.
var contextOverflowMarkers = []string{
	"prompt too long",
	"prompt_too_long",
	"prompt exceeds",
	"exceeds model's effective n_ctx",
	"context overflow",
	"context length exceeded",
	"n_ctx is too small",
	"code 3:",
}

// isContextOverflowText — «это отказ по размеру контекста?».
//
// Чистая функция: используется и для done-чанка с error, и для тела HTTP-ошибки,
// и для накопленного текста. Одна реализация — чтобы три места не разошлись
// (этот класс расхождений в серии ловили четыре раза).
func isContextOverflowText(s string) bool {
	if strings.TrimSpace(s) == "" {
		return false
	}
	low := strings.ToLower(s)
	for _, m := range contextOverflowMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// autoContinueBlockedByContext — итоговое решение перед R60.21.
//
// Возвращает (blocked, reason): blocked=true означает «продолжение запрещено,
// это не обрыв, а размер контекста».
//
// Проверяются три источника, потому что отказ приходит тремя путями:
//   - текст ошибки в done-чанке (`error` в NDJSON/SSE);
//   - тело HTTP-ошибки апстрима (413/400);
//   - накопленный текст ответа, если воркер вписал ошибку в content.
func autoContinueBlockedByContext(errorTexts ...string) (bool, string) {
	for _, t := range errorTexts {
		if isContextOverflowText(t) {
			return true, strings.TrimSpace(t)
		}
	}
	return false, ""
}

// estimatePromptTokensFromBody — оценка промпта тем же способом, что и preflight,
// чтобы числа в ответе и в решении о росте окна совпадали.
//
// R302 (2026-10-29): метод на Proxy, а не свободная функция. Оценка, которую
// видит клиент в сообщении, обязана проходить через ТУ ЖЕ калибровку, что и
// решение (p.extractRequestMeta): иначе в сообщении «промпт 66001» рядом с
// решением, принятым по 17616, — снова две правды об одном числе.
func (p *Proxy) estimatePromptTokensFromBody(body []byte, path string) int {
	if meta := p.extractRequestMeta(body, path); meta != nil {
		return meta.EstimatedPromptTokens
	}
	return 0
}

// writeContextTooSmallNotice — сказать клиенту правду вместо повторной попытки.
//
// Код 200 не годится: это не ответ модели, а отказ. Поэтому пишем error-чанк
// тем же путём, что и остальные ошибки стрима (клиент уже умеет его показать),
// с числами: сколько просили и сколько есть.
func writeContextTooSmallNotice(w http.ResponseWriter, originalPath, model string, promptTokens, windowTokens int) {
	msg := fmt.Sprintf(
		"context window is too small for this prompt: prompt %d tokens, window %d tokens; "+
			"raise the model context window or shorten the history",
		promptTokens, windowTokens)
	logger.Get().Warnw("auto-continue suppressed: context window too small (R289)",
		"model", model, "path", originalPath,
		"prompt_tokens", promptTokens, "window_tokens", windowTokens)
	writeStreamErrorChunk(w, originalPath, model, msg)
}
