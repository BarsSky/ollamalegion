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
	"os"
	"sort"
	"strconv"
	"strings"
)

// qwenServiceMarkers — метки чат-шаблона qwen3.8/harmonica и родственных
// диалектов, которые модель может эмитить в тексте.
//
// ПОРЯДОК НЕ ВАЖЕН для вычищения: строки удаляются целиком и независимо.
// Длинные варианты перечислены раньше, чтобы обрезка не оставила хвост вида
// `tool_call|>` после удаления более короткого совпадения — поэтому список
// сортируется по убыванию длины в initServedMarkers.
var qwenServiceMarkers = []string{
	"<|tool_call|>", "<|/tool_call|>", "<|tool_call_end|>",
	"<|tool|>", "<|/tool|>", "<|tool_end|>",
	"<|assistant|>", "<|/assistant|>", "<|end|>",
	"<|user|>", "<|system|>", "<|developer|>",
	"<|message|>", "<|constrain|>", "<|channel|>",
	"<|start_header_id|>", "<|end_header_id|>",
}

// qwenSortedServiceMarkers — те же метки, отсортированные от длинных к коротким.
var qwenSortedServiceMarkers = func() []string {
	out := append([]string(nil), qwenServiceMarkers...)
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}()

// stripQwenServiceMarkers удаляет служебные метки шаблона из текста.
//
// Зачем отдельная функция, а не дополнение stripServiceTokens: тот вызывается
// ПОСЛЕ извлечения вызова (на «остатке») и не должен трогать структуру JSON.
// Здесь же метки вычищаются из того, что уходит клиенту видимым текстом.
// Пробелы по краям схлопываются, чтобы после удаления метки не оставалось
// «висячих» двойных пробелов в начале строки.
func stripQwenServiceMarkers(s string) string {
	if s == "" || !strings.Contains(s, "<|") {
		return s
	}
	out := s
	for _, m := range qwenSortedServiceMarkers {
		out = strings.ReplaceAll(out, m, "")
	}
	return strings.TrimLeft(out, " \t")
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
func toolCallBufferLimit() int {
	const def = 64 * 1024
	raw := strings.TrimSpace(os.Getenv("LB_TOOL_CALL_BUFFER_BYTES"))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 4096 {
		return def
	}
	return n
}
