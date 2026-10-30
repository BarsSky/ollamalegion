// native_done_frame_r331.go — R331 (2026-10-30): ОДИН владелец content
// завершающего NDJSON-кадра нативной поверхности.
//
// ЗАЧЕМ. Аудит маршрутов нашёл пять независимых правил, решающих судьбу
// message.content в завершающем кадре (воркер R108/R320, strip R91, гигиена
// R321, marker-hold R266.1, writeStreamingSSEDone). На нативном пути по проводу
// кадр правили ДВА слоя подряд: stripNativeDoneContent (снять дубль уже
// отданного текста) и nativeDoneHygieneR321 (причина + пояснение). Каждый не
// знал о другом — и вот живая регрессия: strip очищал content, hygiene видела
// «пустой ответ после размышлений» и вставляла пояснение ПОВЕРХ нормального
// завершения. Оператор увидел это как «в конце каждого ответа предупреждение».
//
// РЕШЕНИЕ. Одна функция решает всё: сначала дубль (если включено), затем причина
// и пояснение по состоянию, посчитанному ОДИН раз.
package balancer

import "strings"

// finalizeNativeDoneFrameR331 — итоговый кадр завершения для /api/chat|generate.
//
// Параметры:
//   - line            — кадр воркера (NDJSON, уже без префикса data:);
//   - streamedContent — ВИДИМЫЙ текст, уже отданный дельтами до этого кадра
//     (нужен и для снятия дубля, и для решения «клиент что-то видел?»);
//   - reasoningChars  — сколько символов размышлений уехало клиенту;
//   - stripDuplicate  — включено ли снятие дубля (LB_OLLAMA_DONE_CONTENT).
//
// Возвращает (кадр, изменён ли он). Идемпотентна: повторный проход по уже
// обработанному кадру ничего не меняет.
func finalizeNativeDoneFrameR331(line, streamedContent string, reasoningChars int, stripDuplicate bool) (string, bool) {
	out := line
	changed := false
	// Шаг 1: дубль уже отданного текста (R91). Делается ПЕРВЫМ, потому что
	// решение шага 2 зависит от того, остался ли в кадре видимый текст.
	if stripDuplicate {
		if stripped := stripNativeDoneContent(out, streamedContent); stripped != out {
			out = stripped
			changed = true
		}
	}
	// Шаг 2: контрактная причина и пояснение вместо пустого ответа (R321/R328).
	visibleChars := len(strings.TrimSpace(streamedContent))
	if fixed, hygieneChanged := nativeDoneHygieneR321(out, reasoningChars, visibleChars); hygieneChanged {
		out = fixed
		changed = true
	}
	return out, changed
}
