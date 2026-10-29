// final_block_r279.go — R279 (2026-10-28): финальный блок ответа не должен
// пропадать.
//
// ЖИВОЙ ДЕФЕКТ (Cline, /api/chat). Модель сгенерировала финальный блок ответа
// (итоговый разбор с планом), клиент получил вместо него размышления и маркер
// конца, а сам блок исчез.
//
// ПОЧЕМУ ТАК ВЫХОДИЛО — две независимые причины, обе в проводном пути.
//
//  1. `stripNativeDoneContent` (llamacpp_transport.go) снимал content из
//     done-чанка cppworker'а БЕЗУСЛОВНО, как только до этого ушла хоть одна
//     дельта: «дельты уже отдали текст, в done он дубль». Но cppworker кладёт в
//     done-чанк ПОЛНЫЙ ТЕКСТ ответа, и если последний блок модели пришёл ТОЛЬКО
//     в этом чанке (а дельтами ушли рассуждения и/или предыдущая проза), то
//     вычищался не дубль, а единственная копия блока. Страховка R91 проверяла
//     лишь «не отдано совсем ничего», а на «отдано что-то другое» писала WARN и
//     всё равно выбрасывала текст — то есть о потере знали и всё равно теряли.
//
//     Накопитель для анализа (`appendNativeContent`) при этом вёл себя правильно:
//     дубль не добавлял, НОВЫЙ текст добавлял. Проводной путь обязан вести себя
//     так же — расхождение между «что видит логика» и «что видит клиент» и было
//     корнем.
//
//  2. Придержанный хвост (`markerHold`, R266.1) на конце стрима отдавался
//     В НАКОПИТЕЛЬ, а не клиенту («if held := ndjsonHold.Flush(); held != "" {
//     accumulatedPlainContent += held }»). То есть байты, придержанные как
//     «возможный префикс метки», уходили в статистику и auto-continue, а в
//     поток — нет. Если хвостом оказывался обычный текст, он тихо исчезал.
//
// РЕШЕНИЕ — два правила, одно на оба дефекта: клиенту уходит ВСЁ, что не является
// точным дублем уже отданного, и придержанное на конце стрима отдаётся в поток
// (а не в накопитель).
package balancer

import (
	"encoding/json"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// dedupeDoneContent — сколько из content done-чанка НУЖНО ещё отдать клиенту.
//
// Возвращает остаток, которого клиент ещё не видел:
//   - done-текст целиком уже есть в отданном (дубль)  → "" (вычищаем, как R91);
//   - done-текст начинается с конца отданного (хвост дельт + продолжение) →
//     продолжение;
//   - общего нет (апстрим отдал в done другой текст) → весь текст.
//
// ПОЧЕМУ ИМЕННО ХВОСТ ОТДАННОГО, а не «contains»: cppworker отдаёт в done-чанке
// полный ответ, то есть отданные дельты — его ПРЕФИКС. Значит продолжение —
// это `done[длина общего префикса-хвоста:]`. Проверка `strings.Contains` из R91
// не даёт позиции и потому не могла ничего, кроме «вырезать всё».
func dedupeDoneContent(doneContent, streamedContent string) string {
	if doneContent == "" {
		return ""
	}
	// Ничего не отдано — done-чанк единственный носитель ответа.
	if strings.TrimSpace(streamedContent) == "" {
		return doneContent
	}
	// Полный дубль (самый частый случай): отданное содержит done-текст целиком.
	// Сравниваем без ведущих/хвостовых пробелов с ОБЕИХ сторон: cppworker любит
	// добавлять перевод строки в начало done-текста, и из-за этого «дубль» не
	// распознавался, а текст дублировался у клиента.
	trimmedDone := strings.TrimSpace(doneContent)
	if trimmedDone != "" && strings.Contains(streamedContent, trimmedDone) {
		return ""
	}
	return trimOverlapPrefix(streamedContent, doneContent)
}

// trimOverlapPrefix — убрать из doneContent ту часть, которая уже есть в конце
// streamedContent. Ищет самый длинный суффикс streamedContent, являющийся
// префиксом doneContent.
//
// ОГРАНИЧЕНИЕ СТОИМОСТИ. Проверка идёт от максимума вниз, и каждый шаг — это
// сравнение O(n) (`HasSuffix`). На ответе в сотни килобайт «общего нет» дало бы
// O(n²) — секунды на горячем пути. Поэтому:
//   - сначала проверяется полное перекрытие (типовой случай: done ⊃ streamed);
//   - затем поиск ограничен окном maxOverlapProbe: правдоподобное частичное
//     перекрытие больше этого окна без совпадения на максимуме не встречается, а
//     ошибка в эту сторону безопасна — вернём весь done-текст (потерять текст
//     хуже, чем показать дубль).
func trimOverlapPrefix(streamedContent, doneContent string) string {
	if doneContent == "" {
		return ""
	}
	maxLen := len(doneContent)
	if len(streamedContent) < maxLen {
		maxLen = len(streamedContent)
	}
	// Типовой случай: отданное — префикс done-текста (cppworker отдаёт в done
	// полный ответ). Одна проверка вместо перебора.
	if maxLen > 0 && strings.HasPrefix(doneContent, streamedContent[len(streamedContent)-maxLen:]) {
		return doneContent[maxLen:]
	}
	const maxOverlapProbe = 8192
	start := maxLen - 1
	if start > maxOverlapProbe {
		start = maxOverlapProbe
	}
	for n := start; n > 0; n-- {
		if strings.HasPrefix(doneContent, streamedContent[len(streamedContent)-n:]) {
			return doneContent[n:]
		}
	}
	return doneContent
}

// ApplyFinal — финальное применение удержания: отдаёт придержанное ВМЕСТЕ с
// content и НЕ придерживает ничего заново.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ МЕТОД. В `filterNDJSONLineMarkers` финальная строка
// обрабатывалась как `hold.Flush() + hold.Apply(c)`: Apply мог снова придержать
// хвост (если c заканчивался на «возможный префикс метки»), и этот новый хвост
// уже никто не отдавал — стрим кончился. Терялись последние байты финального
// блока, ровно там, где клиент ждёт текст ответа.
func (h *markerHold) ApplyFinal(content string) string {
	s := content
	if h != nil && h.pending != "" {
		s = h.pending + s
		h.pending = ""
	}
	if s == "" {
		return ""
	}
	return stripQwenServiceMarkers(s)
}

// heldTailChunk — NDJSON-чанк с придержанным хвостом, который нужно отдать
// клиенту на конце стрима.
//
// Формы те же, что у cppworker: /api/chat → message.content,
// /api/generate → response. `done:false` обязателен: терминальный чанк пишет
// отдельный код (writeStreamingSSEDone / done от воркера), и второй done:true
// клиент воспринял бы как «второй ответ» или как «пустой ответ» (живой дефект
// R66b: Cline на два done отвечал "Model returned empty response").
func heldTailChunk(originalPath, model string, held string) []byte {
	if held == "" || originalPath != "/api/chat" && originalPath != "/api/generate" {
		return nil
	}
	if model == "" {
		model = "unknown"
	}
	chunk := map[string]interface{}{
		"model":      model,
		"created_at": time.Now().UTC().Format(time.RFC3339),
		"done":       false,
	}
	if originalPath == "/api/generate" {
		chunk["response"] = held
	} else {
		chunk["message"] = map[string]interface{}{
			"role":    "assistant",
			"content": held,
		}
	}
	out, err := json.Marshal(chunk)
	if err != nil {
		logger.Get().Warnw("heldTailChunk: marshal failed", "error", err)
		return nil
	}
	return out
}
