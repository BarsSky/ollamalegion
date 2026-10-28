package balancer

// reasoning_egress_r256.go — R256 (2026-10-28): ОДНА точка выхода /v1-кадра
// клиенту, и размышления НИКОГДА не остаются в delta.content.
//
// ЖИВАЯ ЖАЛОБА (Cline): «опять мешаются с основным ответом размышления от
// балансера» — мысли модели видны в теле ответа.
//
// ПОЧЕМУ ЭТО ВОЗВРАЩАЛОСЬ. До R256 граница балансера умела только ВЫРЕЗАТЬ
// маркеры размышлений из content (llamacpp_content_filter.go): текст внутри
// `<|channel>thought … <channel|>` оставался в content и уезжал клиенту как
// обычный ответ. Плюс появившийся в R253 путь дообслуживания после reload
// (preflight_stream_dialog.go: streamSelfProxyChunked/Plain) переливал кадры
// внутреннего ответа КАК ЕСТЬ, вообще без фильтра — то есть даже маркеры на этом
// пути не удалялись.
//
// РЕШЕНИЕ. Здесь один конвейер (`filterAssistantEgressForClient`), который обязан
// пропускать КАЖДЫЙ кадр, уходящий клиенту на /v1:
//   - текст между маркерами размышлений уходит в `delta.reasoning_content`
//     (отдельный канал; Cline/OpenWebUI показывают его отдельно или игнорируют),
//     а в `delta.content` остаётся ТОЛЬКО видимый ответ;
//   - маркер может быть разрезан между чанками — хвост-возможный-префикс
//     удерживается до следующего кадра (та же идея, что в R243);
//   - служебные токены и остаточные маркеры вырезаются из видимого текста.
//
// Маркеры сознательно разделены на «переключают канал» и «просто вырезать».
// Проверка — только на маркер хода/канал, никогда на подстроку в прозе: обычный
// текст со словом «thinking» резать нельзя.

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"

	"ollama-loadbalancer/pkg/logger"
)

// reasoningEgressState — состояние ОДНОГО стрима для фильтра выхода.
type reasoningEgressState struct {
	// markerState — прежний фильтр маркеров/служебных токенов (R243).
	markerState reasoningStreamState
	// split — текущий канал и удержанный хвост маркера.
	split reasoningSplitState
	diag  sync.Once
	// cut — сколько кадров фильтр реально изменил (для тестов/диагностики).
	cut int

	// --- R261 (2026-10-28): «пустой ответ» не должен быть молчаливым ---
	//
	// ЖИВАЯ ЖАЛОБА (DSH): «генерация в бэкенде идёт, но клиенту ничего не
	// приходит даже после окончания». Замер на стенде: gemma-4 отдаёт ВЕСЬ
	// ответ внутри канала размышлений, R256 перекладывает его в
	// reasoning_content, а delta.content остаётся пустым на всю длину стрима
	// (content_len=0, reasoning_len=359, finish=length). Клиент, который не
	// рисует reasoning_content, видит пустоту — при том что модель ответила.
	//
	// Здесь считаем, что РЕАЛЬНО ушло клиенту по каждому каналу, и в конце
	// стрима принимаем решение (см. finalizeReasoningStream): не отдавать
	// молчаливую пустоту.
	visibleChars   int
	reasoningChars int
	// heldFinish — finish-кадр, удержанный до конца стрима. Нужен, чтобы
	// сообщение о пустом ответе пришло ДО finish_reason (строгие клиенты не
	// читают контент после finish).
	heldFinish []byte
	finalized  bool
}

// logReasoningCutOnce — диагностика «фильтр сработал» один раз на стрим.
func (st *reasoningEgressState) logReasoningCutOnce() {
	if st == nil || logger.Get() == nil {
		return
	}
	logger.Get().Debugw("reasoning filtered out of visible content (moved to reasoning_content)",
		"frames_cut", st.cut)
}

// reasoningSplitState — «сейчас в размышлениях?» + удержанный хвост маркера.
type reasoningSplitState struct {
	inReasoning bool
	pending     string
}

// reasoningStartMarkers — открывают канал размышлений (переводят текст в
// reasoning_content).
//
// ПОРЯДОК ВАЖЕН: длинные варианты раньше коротких, иначе `<|channel|>analysis`
// сматчится как `<|channel|>` и оставит в тексте `analysis`.
var reasoningStartMarkers = []string{
	"<|channel|>analysis\n", "<|channel|>analysis",
	"<|channel|>thought\n", "<|channel|>thought",
	"<|channel|>reasoning\n", "<|channel|>reasoning",
	"<|channel>analysis\n", "<|channel>analysis",
	"<|channel>thought\n", "<|channel>thought",
	"<|begin_of_thought|>",
	"<|thought|>",
	"<thinking>", "<analysis>", "<reasoning>", "<thought>", "<think>",
	"<|think>",
	"<|channel|>", "<|channel>",
	"[THINK]",
}

// reasoningEndMarkers — закрывают канал размышлений.
//
// `<|channel|>final` / `<|channel>final` — переключение на видимый ответ
// (формат qwen3.8/harmonica: analysis → final).
var reasoningEndMarkers = []string{
	"<|channel|>final\n", "<|channel|>final",
	"<|channel>final\n", "<|channel>final",
	"<｜end▁of▁thinking｜>",
	"<|end_of_thought|>",
	"<|/thought|>",
	"</thinking>", "</analysis>", "</reasoning>", "</thought>", "</think>",
	"<think|>",
	"<channel|>",
	"[/THINK]",
}

// reasoningNeutralTokens — не меняют канал, но не должны попадать в видимый текст.
var reasoningNeutralTokens = []string{
	"<|message|>",
	"<|constrain|>",
}

// reasoningMarkerMatch — ближайший маркер в s (позиция, длина, эффект).
type reasoningMarkerMatch struct {
	pos    int
	length int
	// effect: +1 — начать размышления, -1 — закончить, 0 — нейтральный токен.
	effect int
}

// findReasoningMarker — ближайший из всех маркеров, начиная с from.
// Возвращает pos=-1, если маркеров не осталось.
func findReasoningMarker(s string, from int) reasoningMarkerMatch {
	if from < 0 {
		from = 0
	}
	if from >= len(s) {
		return reasoningMarkerMatch{pos: -1}
	}
	rest := s[from:]
	best := reasoningMarkerMatch{pos: -1}
	consider := func(marker string, effect int) {
		if marker == "" {
			return
		}
		idx := strings.Index(rest, marker)
		if idx < 0 {
			return
		}
		if best.pos < 0 || idx < best.pos || (idx == best.pos && len(marker) > best.length) {
			best = reasoningMarkerMatch{pos: from + idx, length: len(marker), effect: effect}
		}
	}
	for _, m := range reasoningStartMarkers {
		consider(m, +1)
	}
	for _, m := range reasoningEndMarkers {
		consider(m, -1)
	}
	for _, m := range reasoningNeutralTokens {
		consider(m, 0)
	}
	return best
}

// trailingMarkerPrefixLen — длина хвоста s, который может быть началом маркера
// (маркер разрезан между чанками). Возвращает 0, если такого хвоста нет.
func trailingMarkerPrefixLen(s string) int {
	maxLen := 0
	for _, group := range [][]string{reasoningStartMarkers, reasoningEndMarkers, reasoningNeutralTokens} {
		for _, m := range group {
			if len(m) > maxLen {
				maxLen = len(m)
			}
		}
	}
	if maxLen > len(s) {
		maxLen = len(s)
	}
	lower := strings.ToLower(s)
	for n := maxLen - 1; n >= 1; n-- {
		suffix := lower[len(lower)-n:]
		for _, group := range [][]string{reasoningStartMarkers, reasoningEndMarkers, reasoningNeutralTokens} {
			for _, m := range group {
				lm := strings.ToLower(m)
				if len(suffix) < len(lm) && strings.HasPrefix(lm, suffix) {
					return n
				}
			}
		}
	}
	return 0
}

// splitReasoningText — разделить текст чанка на видимый и reasoning.
//
// st хранит текущий канал и удержанный хвост (маркер, разрезанный между чанками).
// Возвращает (visible, reasoning).
func splitReasoningText(s string, st *reasoningSplitState) (string, string) {
	if st == nil {
		return s, ""
	}
	if st.pending != "" {
		s = st.pending + s
		st.pending = ""
	}
	if s == "" {
		return "", ""
	}

	var visible, reasoning strings.Builder
	rest := s
	for {
		m := findReasoningMarker(rest, 0)
		if m.pos < 0 {
			break
		}
		segment := rest[:m.pos]
		if st.inReasoning {
			reasoning.WriteString(segment)
		} else {
			visible.WriteString(segment)
		}
		rest = rest[m.pos+m.length:]
		switch m.effect {
		case +1:
			st.inReasoning = true
		case -1:
			st.inReasoning = false
		}
	}
	// Хвост, который может быть началом разрезанного маркера, удерживаем.
	if n := trailingMarkerPrefixLen(rest); n > 0 {
		st.pending = rest[len(rest)-n:]
		rest = rest[:len(rest)-n]
	}
	if st.inReasoning {
		reasoning.WriteString(rest)
	} else {
		visible.WriteString(rest)
	}
	return visible.String(), reasoning.String()
}

// filterAssistantEgressForClient — ЕДИНСТВЕННАЯ функция фильтрации кадра,
// уходящего клиенту на OpenAI-поверхности (R256).
//
// Возвращает (кадр для клиента, былЛиИзменён). Если менять нечего — исходная
// строка и false (байт-в-байт, чтобы не ломать строгих клиентов).
func filterAssistantEgressForClient(line []byte, st *reasoningEgressState) ([]byte, bool) {
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return line, false
	}
	content, ok := extractSSEDeltaContent(line)
	if !ok {
		// Кадра без delta.content не трогаем: reasoning_content — чужой канал,
		// чистить его должен воркер (R231), а finish_reason/tool_calls не текст.
		return line, false
	}

	// Шаг 1 (R256): сначала РАЗДЕЛЕНИЕ по каналу, потом вырезание токенов.
	// Порядок принципиален: прежний фильтр вырезает маркеры ` <|channel>analysis`/
	// `<channel|>` из content ДО разделения — после этого границы reasoning уже не
	// найти, и мысли остаются в видимом ответе (живая жалоба Cline).
	var visible, reasoning string
	if st != nil {
		visible, reasoning = splitReasoningText(content, &st.split)
	} else {
		visible = content
	}

	// Шаг 2: из ВИДИМОГО текста вырезаем служебные токены и остаточные маркеры
	// (в т.ч. те, для которых пара не найдена). Для этого переиспользуем прежний
	// фильтр на синтетическом кадре — так правила остаются в одном месте.
	cleaned := visible
	if st != nil {
		if filtered, changed := filterOpenAIStreamingLineWithState(sseLineWithContent(visible), &st.markerState); changed {
			if c2, ok2 := extractSSEDeltaContent(filtered); ok2 {
				cleaned = c2
			}
		}
	} else {
		cleaned, _ = stripReasoningMarkersFromVisible(visible)
		if stripped, removed := stripLlamaCppServiceTokens(cleaned); removed > 0 {
			cleaned = stripped
		}
	}

	if reasoning == "" && cleaned == content {
		// Кадр не меняем, но счётчики ведём: по ним в конце стрима решается,
		// отдавать ли сообщение о пустом видимом ответе (R261).
		if st != nil {
			st.visibleChars += len(cleaned)
			st.reasoningChars += len(reasoning)
		}
		return line, false
	}
	out, ok := rewriteDeltaContentAndReasoning(line, cleaned, reasoning)
	if !ok {
		return line, false
	}
	if st != nil {
		st.cut++
		st.visibleChars += len(cleaned)
		st.reasoningChars += len(reasoning)
		st.diag.Do(st.logReasoningCutOnce)
	}
	return out, true
}

// sseLineWithContent — синтетический SSE-кадр с одним delta.content.
func sseLineWithContent(content string) []byte {
	chunk := map[string]interface{}{
		"choices": []interface{}{
			map[string]interface{}{"delta": map[string]interface{}{"content": content}},
		},
	}
	b, err := json.Marshal(chunk)
	if err != nil {
		return nil
	}
	return []byte("data: " + string(b) + "\n\n")
}

// splitReasoningInSSELine — видимый текст и reasoning для кадра.
//
// hasContent == false означает «в кадре нет delta.content» (например, кадр только
// с finish_reason или tool_calls) — такой кадр не трогаем.
func splitReasoningInSSELine(line []byte, st *reasoningEgressState) (visible, reasoning string, hasContent bool) {
	content, ok := extractSSEDeltaContent(line)
	if !ok {
		return "", "", false
	}
	if st == nil {
		// Без состояния канал не отследить — оставляем прежнее поведение
		// (маркеры уже вырезаны шагом 1).
		return content, "", true
	}
	v, r := splitReasoningText(content, &st.split)
	return v, r, true
}

// rewriteDeltaContentAndReasoning — пересобрать кадр: видимый текст в content,
// размышления — в reasoning_content (прибавка к уже накопленному в кадре).
func rewriteDeltaContentAndReasoning(line []byte, visible, reasoning string) ([]byte, bool) {
	payload := extractSSEDataPayload(bytes.TrimSpace(line))
	if payload == nil {
		return nil, false
	}
	var chunk map[string]interface{}
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return nil, false
	}
	choices, ok := chunk["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return nil, false
	}
	first, ok := choices[0].(map[string]interface{})
	if !ok {
		return nil, false
	}
	delta, ok := first["delta"].(map[string]interface{})
	if !ok {
		return nil, false
	}
	// Пустой content оставляем ключом (клиенты ожидают форму чанка), см. R231.
	delta["content"] = visible
	if reasoning != "" {
		prev, _ := delta["reasoning_content"].(string)
		delta["reasoning_content"] = prev + reasoning
	}
	first["delta"] = delta
	choices[0] = first
	chunk["choices"] = choices
	out, err := json.Marshal(chunk)
	if err != nil {
		return nil, false
	}
	return []byte("data: " + string(out) + "\n\n"), true
}

// extractSSEDeltaContent — delta.content из SSE-строки ("" и false, если нет).
func extractSSEDeltaContent(line []byte) (string, bool) {
	payload := extractSSEDataPayload(bytes.TrimSpace(line))
	if payload == nil {
		return "", false
	}
	var chunk struct {
		Choices []struct {
			Delta map[string]interface{} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil || len(chunk.Choices) == 0 {
		return "", false
	}
	delta := chunk.Choices[0].Delta
	if delta == nil {
		return "", false
	}
	content, ok := delta["content"].(string)
	if !ok {
		return "", false
	}
	return content, true
}
