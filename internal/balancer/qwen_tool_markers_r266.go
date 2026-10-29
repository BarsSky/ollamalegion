// qwen_tool_markers_r266.go — R266 (2026-10-28): служебные метки qwen3.8 в
// видимом тексте.
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА (Cline через Ollama-протокол /api/chat, модель qwen3.8):
// «ответ приходит вперемешку с метками `<|tool|>`». В присланном логе видно
// буквально:
//
//	… </think> <|tool|> tool_call_result(run_commands): [{"query":"grep …"}]
//	… <|assistant|> The file has ~348 lines. Let me read …
//
// ПОЧЕМУ ЭТО ВООБЩЕ ПОПАДАЕТ КЛИЕНТУ. Ветка SSE→NDJSON (llamacpp_transport.go,
// originalPath == "/api/chat") распознаёт начало текстового tool-вызова только
// по пяти префиксам: `[`, `<tool_call>`, `<|python_tag|>`, `[TOOL_CALLS]`,
// `{name`. Меток qwen3.8 в этом списке нет, поэтому чанк, начинающийся с
// `<|tool|>` или `<|assistant|>`, спокойно уезжал в message.content NDJSON —
// то есть клиент видел шаблонные токены чата, которых в ответе быть не должно.
//
// ВТОРАЯ ЧАСТЬ ДЕФЕКТА. Даже когда накопление начиналось, буфер обрезался при
// `len(contentBuffer) > 4096` и ПРОСТО ВЫБРАСЫВАЛСЯ: для вызова, который пишет
// файл на сотни строк, это гарантированная потеря вызова и утечка обрывка в
// content — ровно «вперемешку с метками».
//
// РЕШЕНИЕ. (1) Метки — служебные: они вычищаются из видимого текста на всех
// путях; (2) если метка разрезана между чанками, хвост придерживается, а не
// отдаётся по частям; (3) превышение буфера больше не уничтожает текст — он
// отдаётся очищенным, а не выбрасывается.
package balancer

import (
	"sort"
	"strings"

	"ollama-loadbalancer/internal/settings"
)

// qwenServiceMarkers — ОДИН список служебных меток для всех путей: ядро
// qwen/harmonica (qwenCoreServiceMarkers) плюс нейтральные токены и маркеры
// рассуждений из egress-фильтра R256.
//
// ЗАЧЕМ СВОДИТЬ (R266.1, 2026-10-28). До этой правки списки жили отдельно:
// egress-фильтр `/v1` резал `</think>` и каналы, а NDJSON-ветка `/api/chat`
// знала только `<|tool|>`-токены и не знала `</think>`. Правка в одном месте
// не спасала другой путь — ровно та беда, на которую жаловался оператор.
// Теперь источник один, сортировка по длине применяется здесь же.
var qwenServiceMarkers = buildQwenServiceMarkers()

// qwenCoreServiceMarkers — метки чат-шаблона qwen3.8/harmonica, которые модель
// может эмитить в тексте.
var qwenCoreServiceMarkers = []string{
	"<|tool_call|>", "<|/tool_call|>", "<|tool_call_end|>",
	"<|tool|>", "<|/tool|>", "<|tool_end|>",
	"<|assistant|>", "<|/assistant|>", "<|end|>",
	"<|user|>", "<|system|>", "<|developer|>",
	"<|message|>", "<|constrain|>", "<|channel|>",
	"<|start_header_id|>", "<|end_header_id|>",
	// R266.1 (2026-10-28): маркеры размышлений. Живой факт: `</think>`
	// уезжал клиенту в message.content на пути /api/chat — его не было ни в
	// этом списке, ни в проверке (см. ниже про ранний выход по "<|").
	// Варианты с `|` и без перечислены оба: qwen отдаёт `</think>`, harmonica —
	// `<|/think|>`; встречались оба в живых логах.
	"</think>", "<think>", "<|/think|>", "<|think|>",
	"<|/analysis|>", "<|analysis|>",
}

// buildQwenServiceMarkers — объединение всех известных маркеров без дублей,
// отсортированное от длинных к коротким (иначе после удаления короткого
// совпадения остаётся хвост вида `tool_call|>`).
func buildQwenServiceMarkers() []string {
	seen := make(map[string]bool, 64)
	out := make([]string, 0, 64)
	add := func(m string) {
		if m == "" || seen[m] {
			return
		}
		seen[m] = true
		out = append(out, m)
	}
	for _, m := range qwenCoreServiceMarkers {
		add(m)
	}
	for _, m := range reasoningNeutralTokens {
		add(m)
	}
	for _, m := range reasoningStartMarkers {
		add(m)
	}
	for _, m := range reasoningEndMarkers {
		add(m)
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// stripQwenServiceMarkers удаляет служебные метки шаблона из текста.
//
// Зачем отдельная функция, а не дополнение stripServiceTokens: тот вызывается
// ПОСЛЕ извлечения вызова (на «остатке») и не должен трогать структуру JSON.
// Здесь же метки вычищаются из того, что уходит клиенту видимым текстом.
// Пробелы по краям схлопываются, чтобы после удаления метки не оставалось
// «висячих» двойных пробелов в начале строки.
func stripQwenServiceMarkers(s string) string {
	// R266.1: раньше здесь стоял ранний выход по `!strings.Contains(s, "<|")`.
	// Из-за него метки БЕЗ `|` (`</think>`, `<think>`) не вычищались никогда —
	// ровно та утечка, которую поймали живьём на /api/chat. Проверяем оба
	// варианта начала метки, чтобы не гонять ReplaceAll на обычном тексте.
	if s == "" || (!strings.Contains(s, "<|") && !strings.Contains(s, "<think") && !strings.Contains(s, "</think")) {
		return s
	}
	out := s
	for _, m := range qwenServiceMarkers {
		out = strings.ReplaceAll(out, m, "")
	}
	// R279 (2026-10-28): НЕ съедаем пробелы видимого текста.
	//
	// ЖИВОЙ ДЕФЕКТ (нашёл тест R279): склейка «итог<|as» + «sistant|> plan» давала
	// «итог<|assistant|> plan» → после удаления метки «итог plan», а безусловный
	// TrimLeft отрезал пробел и получалось «итогplan» — текст склеивался.
	// Теперь whitespace схлопывается только если после удаления меток не осталось
	// ничего, кроме пробелов (метка пришла отдельным чанком — «висячие» пробелы).
	if strings.TrimSpace(out) == "" {
		return ""
	}
	return out
}

// hasQwenServiceMarker — есть ли в строке хоть одна служебная метка.
func hasQwenServiceMarker(s string) bool {
	if s == "" {
		return false
	}
	for _, m := range qwenServiceMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// splitMarkerSuffixLen — длина хвоста s, который может быть НАЧАЛОМ служебной
// метки (метка разрезана между чанками).
//
// ЗАЧЕМ. llama.cpp режет поток по токенам, и `<|tool_call|>` вполне может
// прийти как `<|tool_` + `call|>`. Без удержания хвоста первая половина уедет
// клиенту текстом, а вторая будет вычищена — то есть в ответе появится
// обрывок `<|tool_`, ровно та «перемешка», на которую жалуется оператор.
func splitMarkerSuffixLen(s string) int {
	if s == "" {
		return 0
	}
	max := 0
	for _, m := range qwenServiceMarkers {
		limit := len(m) - 1
		if limit > len(s) {
			limit = len(s)
		}
		for n := limit; n > max; n-- {
			if strings.HasSuffix(s, m[:n]) {
				max = n
				break
			}
		}
	}
	return max
}

// toolCallBufferLimit — предел кросс-чанкового буфера tool-вызова.
//
// ЖИВОЙ ДЕФЕКТ (R266): жёсткие 4096 байт обрезали вызов, который пишет файл на
// сотни строк, и буфер ВЫБРАСЫВАЛСЯ. Теперь предел настраивается
// (LB_TOOL_CALL_BUFFER_BYTES, дефолт 64 КиБ), а при превышении текст не
// теряется — см. flushBufferedContentAsChunk.
//
// R278: значение из реестра настроек (override из WebUI → env → дефолт), то есть
// меняется без рестарта. Лояльность к мусору (<4 КиБ → дефолт) живёт в
// settings.NormalizeEnv.
func toolCallBufferLimit() int {
	const def = 64 * 1024
	if v, ok := settings.HotInt("LB_TOOL_CALL_BUFFER_BYTES"); ok {
		return int(v)
	}
	return def
}
