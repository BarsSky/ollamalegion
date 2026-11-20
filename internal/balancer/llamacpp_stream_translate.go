// llamacpp_stream_translate.go — R630 (2026-11-19): перевод кадров апстрима в
// формат клиента (SSE ↔ NDJSON), накопление дельт и буфер tool-call.
//
// ЗАЧЕМ ВЫНЕСЕНО. Это самая «плотная» часть стрима: здесь решается, что именно
// увидит клиент в каждом кадре — включая кросс-чанковую склейку JSON tool calls
// (Gemma-4 отдаёт их обычным текстом) и снятие дубля content в done-кадре.
// Держать это рядом с чтением сокета и с построением URL — значит прятать
// главную логику в середине 3000-строчного файла.
//
// ПОВЕДЕНИЕ НЕ МЕНЯЛОСЬ: код перенесён как есть.
package balancer

import (
	"encoding/json"
	"fmt"
	"net/http"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"strings"
	"time"
)

// normalizeNativeDoneLine — что именно уйдёт клиенту в этой строке: снятие
// дубля content в завершающем кадре (R91/R331), причина отмены (R354), окна
// статистики R493-12a и вычищение разрезанных служебных меток (R266.1).
func (st *llamaCppStreamState) normalizeNativeDoneLine(trimmed, streamedBeforeThisLine string) string {
	// R83 §9.7 (2026-09-27): нормализуем финальный чанк ПЕРЕД отдачей
	// клиенту. cppworker кладёт в done-чанк ПОЛНЫЙ текст ответа, хотя
	// дельты его уже отдали (измерено 2.00×: столько же байт в дельтах
	// и в done). Настоящая Ollama в done-чанке отдаёт `message.content:
	// ""`, и OpenWebUI с ней работает — значит дубль ломает именно
	// клиентов, конкатенирующих чанки. Оставляем все статы.
	//
	// R91 (2026-10-09): ТОЛЬКО если текст уже уехал дельтами. Раньше
	// content вычищался безусловно, и апстрим, отдавший весь ответ ОДНИМ
	// финальным чанком (mock/статический ответ/фолбэк cppworker, когда
	// модель ушла в незакрытый reasoning и текст отдан в done-чанке),
	// терял ответ целиком: клиент получал done:true с пустым content.
	// Именно это ловили tests/llamacpp_proxy (TestChatStreaming,
	// TestResponseNotMarkdownBold/streaming) и tests (TestProxyOllama_*).
	//
	// ВАЖНО: accumulatedPlainContent/upstreamDoneContent выше уже
	// накопили полный текст (для R60.21 auto-continue) — здесь правится
	// только то, что уходит по проводу.
	//
	// R331 (2026-10-30): ОДИН владелец content завершающего кадра.
	//
	// Раньше по проводу кадр правили ДВА независимых слоя подряд:
	// stripNativeDoneContent (R91: снять дубль уже отданного текста) и
	// nativeDoneHygieneR321 (R321: причина + пояснение, когда клиент не
	// видел ничего). Каждый из них не знал о другом: strip мог очистить
	// content, и тогда hygiene видела «пустой ответ» и вставляла пояснение
	// поверх нормального завершения (регрессия R321, ловил оператор).
	//
	// Теперь решение одно: finalizeNativeDoneFrameR331. Внутри — тот же
	// порядок (сначала снять дубль, потом решить про причину/пояснение), но
	// состояние считается ОДИН раз, поэтому слои не могут разойтись.
	outLine := trimmed
	if st.userCancelledDoneChunkR354 {
		// R354 (2026-10-31): ОДИН владелец кадра отмены. Правим ТОЛЬКО причину
		// (done_reason=cancelled из реестра R328, без error): model/created_at/
		// статистика остаются, клиент по ним закрывает ход и считает токены.
		//
		// Через finalizeNativeDoneFrameR331 такой кадр НЕ пускаем: тот видит
		// пустой response и вставил бы пояснение «ответа нет» (регрессия R321),
		// хотя клиент сам остановил генерацию.
		if fixed, changed := normalizeCancelDoneChunkR354(trimmed); changed {
			outLine = fixed
		}
	} else if st.originalPath == "/api/chat" || st.originalPath == "/api/generate" {
		fixed, changed := finalizeNativeDoneFrameR331(trimmed, streamedBeforeThisLine,
			st.nativeReasoningChars, nativeDoneContentStrippingEnabled())
		if changed {
			logger.Get().Infow("proxyRequestLlamaCpp[native]: done-кадр нормализован (R331)",
				"backend", st.backendID, "model", *st.modelFromCtx,
				"reasoning_chars", st.nativeReasoningChars,
				"visible_streamed_chars", len(strings.TrimSpace(streamedBeforeThisLine)))
			outLine = fixed
		} else {
			outLine = trimmed
		}
	} else {
		outLine = trimmed
	}
	// R493-12a (2026-11-12): нативный done-кадр несёт статистику cppworker,
	// и она не знает про очередь/авто-загрузку/preflight. Живой замер на
	// стенде: клиент ждал 62 с (модель грузилась), а получил total_duration
	// 2.15 с, load_duration 0 и вообще без tokens_per_second. Здесь
	// добавляется то, что видит ТОЛЬКО балансер: load_duration и
	// total = load + prompt + eval (см. applyStatsWindowsNativeR493).
	if st.originalPath == "/api/chat" || st.originalPath == "/api/generate" {
		if fixed, changed := applyStatsWindowsNativeR493(outLine, st.statsWindowsNative); changed {
			outLine = fixed
		}
	}
	// R266.1 (2026-10-28): на нативном пути cppworker уже отдаёт NDJSON,
	// и балансер писал строку клиенту как есть — поэтому служебные метки
	// шаблона (`<|assistant|>`, `</think>`) утекали в message.content
	// ровно здесь. Чистим строку перед отправкой, придерживая хвост
	// разрезанной метки (см. marker_hold_ndjson_r266_1.go).
	if st.originalPath == "/api/chat" || st.originalPath == "/api/generate" {
		outLine = filterNDJSONLineMarkers(outLine, &st.ndjsonHold)
	}
	return outLine
}

// writeClientLine — одна точка записи кадра в этом цикле (R493-12r).
// Возвращает (байт_записано, ошибка). При выключенном флаге это ровно прежний
// `fmt.Fprintf(w, "%s\n", line)` + `bytesForwarded.Add` + `Flush`.
func (st *llamaCppStreamState) writeClientLine(line []byte) (int, error) {
	if st.sink != nil {
		// Счётчик — у стока (единственный источник правды о том, сколько
		// байт ушло клиенту): сводка берёт РАЗНИЦУ, а не число из цикла,
		// иначе два счётчика снова начнут расходиться.
		before := st.sink.Written()
		_, err := st.sink.WriteLine(line)
		sent := int(st.sink.Written() - before)
		st.bytesForwarded.Add(int64(sent))
		return sent, err
	}
	n, err := fmt.Fprintf(st.w, "%s\n", string(line))
	st.bytesForwarded.Add(int64(n))
	if flusher, ok := st.w.(http.Flusher); ok {
		flusher.Flush()
	}
	return n, err
}

// flushBufferedText — отдать накопленный текст клиенту как обычный content
// (R292). Один помощник на все причины (предел буфера и «это проза»), чтобы
// ветки не разошлись в том, что клиент видит.
func (st *llamaCppStreamState) flushBufferedText(text, reason string) error {
	clean := strings.TrimSpace(stripQwenServiceMarkers(text))
	if clean == "" {
		return nil
	}
	chunk := map[string]interface{}{
		"model":      *st.modelFromCtx,
		"created_at": time.Now().UTC().Format(time.RFC3339),
		"done":       false,
		"message": map[string]interface{}{
			"role":    "assistant",
			"content": clean,
		},
	}
	out, errMarshal := json.Marshal(chunk)
	if errMarshal != nil {
		return nil
	}
	n, errWrite := st.writeClientLine(out)
	if errWrite != nil {
		st.lastForwardErr = errWrite
		if st.firstForwardErr == nil {
			st.firstForwardErr = errWrite
		}
		return fmt.Errorf("write NDJSON (buffered text flush/%s): %v", reason, errWrite)
	}
	_ = n
	st.accumulatedPlainContent += clean
	logger.Get().Infow("proxyRequestLlamaCpp: buffered text flushed to client",
		"backend", st.backendID, "model", *st.modelFromCtx,
		"reason", reason, "chars", len(clean))
	return nil
}

// recordTokenUsageFromUsageChunk — R65d: агрегируем статистику токенов со
// streaming-пути (cppworker присылает OpenAI usage-чанк при
// stream_options.include_usage).
//
// До R65d recordTokenUsage вызывался только из non-streaming путей,
// поэтому /api/v1/stats/tokens был пуст для всего реального трафика.
// cppworker присылает OpenAI usage-чанк ({prompt_tokens,
// completion_tokens}) при stream_options.include_usage (по умолчанию
// включено, handlers_openai.go:427-438). Флаг tokenUsageRecorded защищает от
// двойного учёта одного запроса: на нативном пути статистика приходит в
// done-чанке, в SSE — отдельным usage-чанком, и оба могут быть в одном стриме.
func (st *llamaCppStreamState) recordTokenUsageFromUsageChunk(chunk map[string]interface{}) {
	if st.tokenUsageRecorded {
		return
	}
	if usage, ok := chunk["usage"].(map[string]interface{}); ok {
		promptTokens := int64(intFromMap(usage, "prompt_tokens"))
		completionTokens := int64(intFromMap(usage, "completion_tokens"))
		if promptTokens > 0 || completionTokens > 0 {
			st.tokenUsageRecorded = true
			st.p.recordTokenUsage(*st.modelFromCtx, promptTokens, completionTokens)
		}
	}
}

// forwardOpenAISSEPassthrough — SSE→SSE passthrough для /v1/chat/completions:
// кадр уходит клиенту как есть, но с вычищением служебных токенов (R231) и
// приведением tool_calls к объявленной схеме (R186/R254).
func (st *llamaCppStreamState) forwardOpenAISSEPassthrough(data string, chunk map[string]interface{}) error {
	// Накапливаем plain content для /v1/chat/completions (на случай будущего использования).
	if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if delta, ok := choice["delta"].(map[string]interface{}); ok {
				if c, ok := delta["content"].(string); ok && c != "" {
					st.accumulatedPlainContent += c
				}
			}
		}
	}
	// SSE → SSE passthrough
	//
	// R231 (2026-10-24): ЗДЕСЬ БЫЛА ОШИБКА ВЫЗОВА ФИЛЬТРА, из-за которой
	// он фактически не работал. filterOpenAIStreamingLine ожидает
	// SSE-строку "data: {...}" и проверяет HasPrefix("data:"), а сюда
	// передавался ГОЛЫЙ JSON (префикс уже срезан выше) — проверка не
	// срабатывала, и служебные токены/маркеры размышлений уезжали клиенту.
	// Второй дефект: возвращаемый bool означает «модифицирован», но
	// трактовался как «shouldSkip» — чанк с вырезанным токеном просто
	// выбрасывался (терялся текст ответа).
	// Теперь: собираем SSE-строку, фильтруем, и если контент почищен —
	// продолжаем обработку уже с очищенным JSON.
	if filteredLine, modified := filterAssistantEgressForClient([]byte("data: "+data), st.egressFilter); modified {
		cleaned := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(filteredLine)), "data:"))
		if cleaned != "" {
			data = cleaned
		}
	}
	if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if delta, ok := choice["delta"].(map[string]interface{}); ok {
				accumulateToolCallsFromDelta(delta, st.toolAccum)
			}
		}
	}
	if modified := extractToolCallsFromSSEContentWithSchema([]byte(data), st.toolSchemas, st.toolRepair); modified != nil {
		n, errFwd := fmt.Fprintf(st.w, "data: %s\n\n", string(modified))
		st.bytesForwarded.Add(int64(n))
		if errFwd != nil {
			st.lastForwardErr = errFwd
			if st.firstForwardErr == nil {
				st.firstForwardErr = errFwd
			}
			return fmt.Errorf("write modified SSE (tool calls): %v", errFwd)
		}
	} else if normalized := normalizeSSEToolCallChunks([]byte(data), st.toolSchemas, st.toolRepair); normalized != nil {
		// R186: нативные вызовы от воркера тоже приводим к форме OpenAI и к
		// объявленной схеме — валидатор клиента проверяет схему независимо
		// от того, кто собрал вызов.
		n, errFwd := fmt.Fprintf(st.w, "data: %s\n\n", string(normalized))
		st.bytesForwarded.Add(int64(n))
		if errFwd != nil {
			st.lastForwardErr = errFwd
			if st.firstForwardErr == nil {
				st.firstForwardErr = errFwd
			}
			return fmt.Errorf("write normalized SSE (tool calls): %v", errFwd)
		}
	} else {
		n, errFwd := fmt.Fprintf(st.w, "data: %s\n\n", data)
		st.bytesForwarded.Add(int64(n))
		if errFwd != nil {
			st.lastForwardErr = errFwd
			if st.firstForwardErr == nil {
				st.firstForwardErr = errFwd
			}
			return fmt.Errorf("write SSE: %v", errFwd)
		}
	}
	if flusher, ok := st.w.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

// translateSSEToNDJSON — SSE→NDJSON для /api/chat и /api/generate: накопление
// content, кросс-чанковое распознавание tool_calls, финальный кадр от воркера,
// перевод кадра в Ollama-форму и запись клиенту.
func (st *llamaCppStreamState) translateSSEToNDJSON(data string, chunk map[string]interface{}) error {
	// ==== SSE → NDJSON: для /api/generate и /api/chat =====
	var deltaContent string
	if st.originalPath == "/api/chat" {
		deltaContent = st.accumulateChatDelta(data, chunk)
		newDelta, skip, errBuf := st.absorbToolCallBuffer(deltaContent)
		if errBuf != nil {
			return errBuf
		}
		deltaContent = newDelta
		if skip {
			return nil
		}
		// Накапливаем plain content (вне tool-call accumulation) для финального
		// done-чанка — это спасает от пустого content если upstream прислал
		// полный ответ только в done-чанке.
		//
		// НЕ накапливаем чанки с finish_reason: cppworker прислал финальный
		// чанк с content=" llama.cpp!" + finish_reason="stop". Этот content
		// уже проксируется через writeStreamingSSEDone ниже (агрегированный
		// в upstreamDoneContent / accumulatedPlainContent). Без этого guard
		// в финальный done-чанк попадал дубликат:
		// "Hello from llama.cpp! llama.cpp!" (см. PF-6 fail 2026-06-27).
		if deltaContent != "" && !st.contentToolCallsProcessed && !hasFinishReason(data) {
			st.accumulatedPlainContent += deltaContent
		}
		st.absorbChatDoneChunk(data)
	} else if st.originalPath == "/api/generate" {
		st.accumulateGenerateDelta(data, chunk)
	}
	return st.translateAndWriteFrame(data)
}

// accumulateChatDelta — дельта /api/chat: фрагменты tool_calls, текст дельты,
// время первого content-чанка и content финального кадра воркера (R60.22).
func (st *llamaCppStreamState) accumulateChatDelta(data string, chunk map[string]interface{}) string {
	var deltaContent string
	if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if delta, ok := choice["delta"].(map[string]interface{}); ok {
				accumulateToolCallsFromDelta(delta, st.toolAccum)
				if c, ok := delta["content"].(string); ok {
					deltaContent = c
					// R60.22: capture time of first non-empty content
					// chunk (TTFT) for accurate prompt_eval_duration.
					if c != "" && st.firstContentTime.IsZero() {
						st.firstContentTime = time.Now()
					}
				}
			}
		}
	}

	// Если чанк содержит finish_reason (done:true) — извлекаем content и
	// done_reason из message, чтобы сохранить полный ответ cppworker'а.
	if upstreamHasDone, msgMap, _ := extractUpstreamDoneChunk(data); upstreamHasDone {
		if c, ok := msgMap["content"].(string); ok {
			st.upstreamDoneContent = c
			// R60.22: capture time of first non-empty content (TTFT).
			// cppworker иногда посылает весь ответ в одном done-чанке
			// (final chunk has both content and finish_reason). В этом
			// случае firstContentTime отражает время получения первого
			// значимого content, даже если он пришёл в done-чанке.
			if c != "" && st.firstContentTime.IsZero() {
				st.firstContentTime = time.Now()
			}
		}
	}
	return deltaContent
}

// absorbToolCallBuffer — кросс-чанковое накопление content для JSON tool calls
// (Gemma-4/Qwen/Hermes выводят вызов обычным текстом, разбитым на чанки).
//
// Кросс-чанковое накопление content для детекции JSON tool calls.
// Поддерживаемые триггеры накопления:
//   - "[" — стандартный JSON-массив tool_calls
//   - "<tool_call>" / "<|python_tag|>" — Hermes/Qwen/Llama-3 форматы
//   - "{name" — начало одиночного JSON-объекта tool_call
//   - contentBuffer не пуст — продолжаем накапливать уже начатый tool call
//
// Возвращает (новый deltaContent, skip, ошибка). skip=true — кадр уже
// обработан этим блоком (накопление продолжается или текст отдан как проза),
// писать его штатным переводом не нужно. Ошибка — только запись клиенту.
func (st *llamaCppStreamState) absorbToolCallBuffer(deltaContent string) (string, bool, error) {
	if deltaContent == "" || st.contentToolCallsProcessed {
		return deltaContent, false, nil
	}
	// R266 (2026-10-28): служебные метки qwen3.8 (`<|tool|>`,
	// `<|assistant|>`, `<|tool_call|>`, …) в видимом тексте.
	//
	// ЖИВОЙ ФАКТ (Cline, /api/chat, qwen3.8): в ответе приходило
	// «… </think> <|tool|> tool_call_result(run_commands): [...] <|assistant|>
	// The file has ~348 lines …». Триггеры накопления ниже знали только
	// `[`, `<tool_call>`, `<|python_tag|>`, `[TOOL_CALLS]`, `{name`, поэтому
	// чанк с меткой уходил в message.content как обычный текст.
	//
	// Метки вычищаем ДО разбора: после этого остаётся либо чистый текст,
	// либо JSON вызова, который распознаётся штатным детектором.
	deltaContent = stripQwenServiceMarkers(deltaContent)
	trimmed := strings.TrimSpace(deltaContent)
	startToolCallAccum := strings.HasPrefix(trimmed, "[") ||
		strings.HasPrefix(trimmed, "<tool_call>") ||
		strings.HasPrefix(trimmed, "<|python_tag|>") ||
		strings.HasPrefix(trimmed, "[TOOL_CALLS]") ||
		strings.HasPrefix(trimmed, "{name") ||
		hasQwenServiceMarker(trimmed) ||
		// Метка разрезана между чанками: `<|tool_` + `call|>`. Хвост
		// придерживаем, иначе первая половина уедет клиенту текстом.
		splitMarkerSuffixLen(trimmed) > 0

	// R114c (2026-10-13): JSON-структура НАЧАЛАСЬ ПОСРЕДИ чанка
	// (проза + вызов в одном чанке). Раньше такой чанк уходил
	// клиенту целиком — в теле ответа оказывался обрывок вызова
	// («…task complete. [{"id":"call_submit_and_exit",…»). Проза
	// отдаётся сразу, а накопление начинается со скобки.
	if !startToolCallAccum && st.contentBuffer == "" {
		if pos, ok := jsonStructStartPos(deltaContent); ok && pos > 0 {
			prose := deltaContent[:pos]
			proseChunk := map[string]interface{}{
				"model":      *st.modelFromCtx,
				"created_at": time.Now().UTC().Format(time.RFC3339),
				"done":       false,
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": prose,
				},
			}
			outProse, _ := json.Marshal(proseChunk)
			nProse, errProse := fmt.Fprintf(st.w, "%s\n", string(outProse))
			st.bytesForwarded.Add(int64(nProse))
			if errProse != nil {
				if st.firstForwardErr == nil {
					st.firstForwardErr = errProse
				}
				return deltaContent, false, fmt.Errorf("write NDJSON (prose before JSON struct): %v", errProse)
			}
			if flusher, ok := st.w.(http.Flusher); ok {
				flusher.Flush()
			}
			st.accumulatedPlainContent += prose
			if st.firstContentTime.IsZero() {
				st.firstContentTime = time.Now()
			}
			deltaContent = deltaContent[pos:]
			trimmed = strings.TrimSpace(deltaContent)
			startToolCallAccum = true
		}
	}

	if st.contentBufferGaveUp || (!startToolCallAccum && st.contentBuffer == "") {
		return deltaContent, false, nil
	}
	st.contentBuffer += deltaContent
	if st.contentBufferSince.IsZero() {
		st.contentBufferSince = time.Now()
	}

	// Кросс-чанковая склейка может оставить "лишний" символ '}' перед
	// закрывающим </tool_call> (когда JSON закрывается в одном чанке, а
	// '}'</tool_call>' приходит в следующем). Схлопываем дубликат.
	st.contentBuffer = collapseDuplicateClosingBrace(st.contentBuffer)

	// Проверяем, не накопился ли полный JSON tool call
	if detectedTC, remainingTC, found := detectAndExtractToolCallsFromContent(st.contentBuffer); found {
		st.contentToolCallsProcessed = true
		st.contentBuffer = ""
		logger.Get().Infow("proxyRequestLlamaCpp: cross-chunk tool_calls detected in content",
			"backend", st.backendID, "model", *st.modelFromCtx,
			"tool_calls_count", len(detectedTC))

		cleanRemaining := stripServiceTokens(remainingTC)
		cleanRemaining = cleanContentAfterToolCallExtraction(cleanRemaining)

		msgMap := map[string]interface{}{
			"role":       "assistant",
			"content":    cleanRemaining,
			"tool_calls": detectedTC,
		}
		ollamaChunk := map[string]interface{}{
			"model":      *st.modelFromCtx,
			"created_at": time.Now().UTC().Format(time.RFC3339),
			"done":       false,
			"message":    msgMap,
		}
		out, _ := json.Marshal(ollamaChunk)
		n, errFwd := fmt.Fprintf(st.w, "%s\n", string(out))
		st.bytesForwarded.Add(int64(n))
		if errFwd != nil {
			st.lastForwardErr = errFwd
			if st.firstForwardErr == nil {
				st.firstForwardErr = errFwd
			}
			return deltaContent, false, fmt.Errorf("write NDJSON (cross-chunk tool calls): %v", errFwd)
		}
		if flusher, ok := st.w.(http.Flusher); ok {
			flusher.Flush()
		}
		return deltaContent, true, nil
	}

	// Если JSON массив закрыт (найден ']' на правильной глубине),
	// но tool call не обнаружен — это не tool call JSON.
	// Сбрасываем буфер и даём чанку пройти через нормальный translate.
	if endPos := findMatchingClosingBracket(st.contentBuffer, 0); endPos >= 0 {
		st.contentBuffer = ""
		st.contentBufferSince = time.Time{}
		return deltaContent, false, nil
	}
	if len(st.contentBuffer) > toolCallBufferLimit() {
		// R266: раньше здесь буфер ВЫБРАСЫВАЛСЯ (жёсткие 4096 байт),
		// и вызов инструмента, который пишет файл на сотни строк,
		// терялся, а его обрывок с метками уезжал клиенту как текст.
		// Теперь текст не теряем: отдаём очищенным от служебных меток
		// как обычный content и продолжаем стрим.
		overflow := st.contentBuffer
		st.contentBuffer = ""
		st.contentBufferSince = time.Time{}
		logger.Get().Warnw("proxyRequestLlamaCpp: tool-call buffer limit reached, text flushed",
			"backend", st.backendID, "model", *st.modelFromCtx,
			"limit_bytes", toolCallBufferLimit(), "flushed_chars", len(overflow))
		if errFlush := st.flushBufferedText(overflow, "buffer_limit"); errFlush != nil {
			return deltaContent, false, errFlush
		}
		return deltaContent, true, nil
	}
	if shouldFlushToolBufferAsProseR292(st.contentBuffer, st.contentBufferSince) {
		// R292: текст лишь НАЧАЛСЯ как JSON/маркер, но признаков
		// вызова в нём нет (ни "name":, ни <|tool|>, …) и он уже
		// пересидел grace/объём. Это проза — отдаём клиенту и
		// прекращаем накопление до конца стрима.
		//
		// ЖИВОЙ СИМПТОМ, который это лечит: ответ начинался на `[`
		// (маркдаун-список/массив в тексте), детектор держал его в
		// буфере до 64 КиБ, и клиент видел «план вместе с
		// размышлениями» одним куском в конце, а не потоком.
		prose := st.contentBuffer
		st.contentBuffer = ""
		st.contentBufferSince = time.Time{}
		st.contentBufferGaveUp = true
		logger.Get().Infow("proxyRequestLlamaCpp: buffered text is prose, streaming it",
			"backend", st.backendID, "model", *st.modelFromCtx,
			"chars", len(prose), "grace_ms", toolCallProseGrace().Milliseconds())
		if errFlush := st.flushBufferedText(prose, "prose_not_toolcall"); errFlush != nil {
			return deltaContent, false, errFlush
		}
		return deltaContent, true, nil
	}
	// Всё ещё накапливаем — пропускаем этот чанк
	return deltaContent, true, nil
}

// absorbChatDoneChunk — финальный кадр cppworker на пути /api/chat: ПОЛНЫЙ
// content приходит в message.content, и tool call может прийти ЦЕЛИКОМ здесь.
func (st *llamaCppStreamState) absorbChatDoneChunk(data string) {
	// Финальный чанк cppworker (с finish_reason) присылает ПОЛНЫЙ content
	// в message.content — используем его как upstreamDoneContent. Это
	// перезаписывает накопленный accumulatedPlainContent чтобы избежать
	// дубликата ("Hello from llama.cpp!" + "Hello from llama.cpp!").
	if upstreamHasDone, msgMap, _ := extractUpstreamDoneChunk(data); upstreamHasDone {
		if c, ok := msgMap["content"].(string); ok && c != "" {
			st.upstreamDoneContent = c
		}
		// R83-фикс (2026-10-01): tool call может прийти ЦЕЛИКОМ в
		// финальном (done) чанке — ровно так делает cppworker на
		// пути /api/chat с tools: он буферизует ответ и отдаёт его
		// одним сообщением с finish_reason.
		//
		// Живой дефект (Cline → балансер → /api/chat): модель
		// выдала JSON-массив tool_calls, cppworker вернул его в
		// message.content последнего чанка, а балансер этот content
		// «вырезал» (он ждал tool call в deltaContent, где его не
		// было) и клиент получал пустой ответ:
		//   "model produced an empty response (inference succeeded
		//    but output is empty)" ← сообщение Cline.
		// Прямой запрос в cppworker при этом отдавал корректный
		// JSON, а OpenAI-путь (/v1/chat/completions) — корректный
		// tool_calls. Ломался только Ollama-путь.
		//
		// Теперь, как и для дельт, пытаемся извлечь tool calls из
		// финального content и, если получилось, отдаём их штатным
		// механизмом (writeStreamingSSEDone с toolAccum). Если не
		// получилось — content остаётся как есть и доезжает до
		// клиента: потерять ответ модели нельзя.
		if !st.contentToolCallsProcessed && st.upstreamDoneContent != "" {
			if detectedTC, remainingTC, found := detectAndExtractToolCallsFromContent(st.upstreamDoneContent); found && len(detectedTC) > 0 {
				// Кладём в тот же аккумулятор, что и дельты
				// (toolAccum — map[index]*accumulatedToolCall),
				// чтобы финальный NDJSON отдал их штатно.
				for i, raw := range detectedTC {
					tcm, ok := raw.(map[string]interface{})
					if !ok {
						continue
					}
					entry := &accumulatedToolCall{index: i, function: map[string]interface{}{}}
					if id, ok := tcm["id"].(string); ok {
						entry.id = id
					}
					if fn, ok := tcm["function"].(map[string]interface{}); ok {
						entry.function = fn
					}
					st.toolAccum[i] = entry
				}
				st.contentToolCallsProcessed = true
				st.upstreamDoneContent = cleanContentAfterToolCallExtraction(stripServiceTokens(remainingTC))
				logger.Get().Infow(i18n.T("balancer.llamacpp_transport.tool_calls_extracted_info"),
					"backend", st.backendID, "model", *st.modelFromCtx,
					"tool_calls_count", len(detectedTC),
					"remaining_content_len", len(st.upstreamDoneContent))
			}
		}
	}
}

// accumulateGenerateDelta — дельта /api/generate: текст кадра и content
// финального кадра (без дубликата " llama.cpp!").
func (st *llamaCppStreamState) accumulateGenerateDelta(data string, chunk map[string]interface{}) {
	// Для /api/generate content лежит в chunk["choices"][0].text или в done_chunk.response
	if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if t, ok := choice["text"].(string); ok && t != "" {
				// НЕ накапливаем чанки с finish_reason (аналогично /api/chat):
				// cppworker в финальном чанке шлёт ПОЛНЫЙ content=" llama.cpp!",
				// а accumulatedPlainContent уже содержит "Hello from". Без guard
				// финальный done-чанк получал дубликат ("Hello from llama.cpp! llama.cpp!").
				if !hasFinishReason(data) {
					st.accumulatedPlainContent += t
				}
			}
		}
	}
	// Финальный чанк cppworker с finish_reason: используем accumulatedPlainContent
	// (без дубликата " llama.cpp!") как upstreamDoneContent. Если cppworker
	// прислал в финальном чанке text=" llama.cpp!" И finish_reason="stop",
	// мы уже НЕ добавили его в accumulatedPlainContent (см. guard выше),
	// поэтому накопленный контент = "Hello from". Дописываем " llama.cpp!"
	// из финального чанка для полноты ответа.
	if hasFinishReason(data) {
		if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
			if choice, ok := choices[0].(map[string]interface{}); ok {
				if t, ok := choice["text"].(string); ok && t != "" {
					st.upstreamDoneContent = st.accumulatedPlainContent + t
				}
			}
		}
		if st.upstreamDoneContent == "" {
			st.upstreamDoneContent = st.accumulatedPlainContent
		}
	}
}

// translateAndWriteFrame — общий хвост NDJSON-путей: завершение по
// finish_reason/tool_calls, вычищение меток (R266.1), единый переводщик с
// четырьмя окнами статистики (R493-12a) и запись кадра клиенту.
func (st *llamaCppStreamState) translateAndWriteFrame(data string) error {
	// Подавляем перевод только если tool_calls уже были обработаны
	// (cross-chunk или delta) — для них writeStreamingSSEDone формирует
	// финальный NDJSON с правильным message.tool_calls.
	//
	// Для чанков с finish_reason НЕ делаем continue — translateOpenAISSEDataToOllama
	// сам корректно сформирует финальный NDJSON с done:true, done_reason:stop
	// и content из текста чанка. Это и есть требуемая passthrough-семантика
	// для streaming: каждый upstream чанк → отдельный NDJSON чанк клиенту,
	// включая финальный с done:true.
	//
	// PF-6 fix (2026-06-27): cppworker НЕ отправляет [DONE] маркер в
	// streaming-режиме — он закрывает TCP сразу после чанка с finish_reason.
	// Чтобы избежать ложного "truncation" alert, помечаем streamCompleted
	// при первом чанке с finish_reason.
	if st.contentToolCallsProcessed || len(st.toolAccum) > 0 {
		// tool_calls уже были обработаны выше — writeStreamingSSEDone возьмёт
		// их из toolAccum и отправит финальный NDJSON с tool_calls.
		// upstreamHadFinishReason=false: даже если cppworker прислал finish_reason
		// в этом же чанке, tool_calls должны быть эмитированы (предыдущая ветка
		// кода обработала cross-chunk tool calls в отдельном NDJSON-чанке).
		if !st.streamCompleted {
			st.streamCompleted = true
			errFwd := writeStreamingSSEDone(st.w, st.originalPath, *st.modelFromCtx, st.toolAccum,
				st.accumulatedPlainContent, st.upstreamDoneContent, false, st.usageChunkSeen)
			if errFwd != nil {
				logger.Get().Warnw("proxyRequestLlamaCpp: writeStreamingSSEDone on tool_calls error",
					"backend", st.backendID, "error", errFwd)
				st.lastForwardErr = errFwd
				if st.firstForwardErr == nil {
					st.firstForwardErr = errFwd
				}
			}
			// R65d (2026-09-20): помечаем canonical done как отправленный.
			//
			// БЕЗ ЭТОГО: writeStreamingSSEDone уже отдал клиенту done:true
			// (с tool_calls), но priorDoneEmitted/usageChunkSeen оставались
			// false. Далее cppworker присылает свой wrapper-чанк
			// (finish_reason без content) и usage-чанк — и
			// translateUsageChunkToOllama, не видя priorDoneEmitted,
			// эмитил ВТОРОЙ done:true. Ollama-клиенты (Cline, ollama npm)
			// на два done-чанка отвечают
			// "Did not receive done or success response in stream".
			//
			// H2 (2026-10-16): writeStreamingSSEDone НЕ пишет статистику,
			// поэтому statsEmitted здесь СОЗНАТЕЛЬНО не выставляется —
			// поздний usage-чанк должен дойти до клиента строкой с
			// done:false (иначе eval_count/prompt_eval_count теряются).
			st.usageChunkSeen = true
			st.priorDoneEmitted = true
			Flush(st.w)
		}
		return nil
	}
	if hasFinishReason(data) && !st.streamCompleted {
		// cppworker закрывает TCP после этого чанка — отмечаем что стрим
		// завершён нормально, чтобы ниже не сработал stream-truncation detection.
		st.streamCompleted = true
		// PF-6 (2026-06-27) + dedup streaming fix (2026-06-30):
		// translateOpenAISSEDataToOllama сам формирует финальный NDJSON с done:true
		// и content из финального чанка. Если writeStreamingSSEDone тоже запишет
		// финальный done-чанк (например, через [DONE] маркер от SSE upstream)
		// клиент увидит ДУБЛИКАТ done-чанка с тем же content. Чтобы этого избежать
		// передаём флаг upstreamSentFinishReason — writeStreamingSSEDone пропустит
		// финальный NDJSON/SSE если upstream уже отправил finish_reason и нет tool_calls.
		st.upstreamHadFinishReason = true
	}
	// R60.49 (2026-09-12): передаём accumulatedPlainContent в translator —
	// при получении usage-чанка message.content/response теперь заполняется
	// накопленным content (а не ""), чтобы OpenWebUI показывал полный ответ.
	//
	// R60.55 (2026-09-13): передаём priorDoneEmitted — если предыдущий
	// чанк уже эмитил done:true (например finish_reason+content safety-net
	// от R51.3), usage чанк не должен эмитить второй done:true.
	//
	// H2 (2026-10-16): передаём и statsEmitted — usage-чанк подавляется
	// только если статистика УЖЕ ушла; иначе она отдаётся строкой с
	// done:false (см. translateOpenAISSEDataToOllamaWithFlags).
	// R118.20 (2026-10-15): фрагменты tool-call'ов копим и отдаём ОДИН раз
	// целиком — в Ollama-форме частичных аргументов не бывает (аудит C5).
	// R266.1 (2026-10-28): вычищаем служебные метки шаблона из ВИДИМОГО
	// content ДО перевода в NDJSON и до любых веток (обычный текст, чанк с
	// finish_reason, чанк после распознанного tool call). Клиенту уходит
	// результат перевода из этих данных, поэтому чистить надо данные, а не
	// локальную переменную: иначе часть веток остаётся без фильтра — именно
	// так `<|assistant|>` утёк после того, как tool call уже был распознан.
	if st.originalPath == "/api/chat" {
		data = filterSSEDeltaContentMarkers(data, &st.ndjsonHold)
	}

	if filtered, ok := st.toolAcc.filterToolCallDelta(data); !ok {
		return nil
	} else {
		data = filtered
	}
	// R493-12a: окна передаём четырьмя моментами — иначе в «время промпта»
	// снова попали бы очередь и загрузка, а размышления выпали бы из
	// «времени генерации» (см. stats_time_window.go).
	statsWindows := statsWindowsForTransportR493(st.requestStartTime, st.upstreamSentTime, st.firstDeltaTime, st.firstContentTime, time.Now())
	// R493-12a: загрузку модели знает воркер (она внутри запроса) — берём
	// её из заголовка ответа, иначе 40+ секунд не попадут никуда.
	statsWindows.WorkerLoad = st.workerLoadNsR493
	ollamaChunk := translateOpenAISSEDataToOllamaWindowsR493(st.originalPath, []byte(data), *st.modelFromCtx, &st.seenReasoning, statsWindows, st.accumulatedPlainContent, &st.priorDoneEmitted, &st.statsEmitted)
	if len(ollamaChunk) == 0 {
		return nil
	}
	// Round 53.1 (2026-08-24): detect usage chunk for writeStreamingSSEDone fallback logic.
	// translateOpenAISSEDataToOllama уже эмитил canonical done:true с полными статами
	// из usage чанка — writeStreamingSSEDone на [DONE] должен skip чтобы избежать
	// double-done. Способ детекта: usage чанк всегда имеет eval_count + done:true
	// (translateUsageChunkToOllama эмитит prompt_eval_count и eval_count).
	// Не-usage чанки с done:true (safety net для length/content_filter в одном
	// чанке с content) тоже считаем — но это OK, writeStreamingSSEDone skip безопасен.
	//
	// R60.55: трекаем priorDoneEmitted для ВСЕХ done:true чанков (не только
	// usage).
	//
	// H2 (2026-10-16): статистика и done:true учитываются РАЗДЕЛЬНО.
	//   - usageChunkSeen/priorDoneEmitted — по полю done (даже для stats-only
	//     строки с done:false priorDoneEmitted не меняется: сам факт того,
	//     что done:true уже ушёл, зафиксирован предыдущим чанком);
	//   - statsEmitted — по наличию eval_count/prompt_eval_count, независимо
	//     от done. Именно это гасит повторные usage-чанки, а не priorDoneEmitted.
	var ollamaParsed map[string]interface{}
	if err := json.Unmarshal(ollamaChunk[:len(ollamaChunk)-1], &ollamaParsed); err == nil {
		if done, _ := ollamaParsed["done"].(bool); done {
			// Любой done:true chunk = canonical done (translator эмитил).
			// writeStreamingSSEDone должен skip.
			st.usageChunkSeen = true
			st.priorDoneEmitted = true
		}
		if _, ok := ollamaParsed["eval_count"]; ok {
			// H2: статистика уехала клиенту (в done:true чанке или в
			// stats-only строке с done:false) — второй раз не отдаём.
			st.statsEmitted = true
		} else if _, ok := ollamaParsed["prompt_eval_count"]; ok {
			st.statsEmitted = true
		}
	}
	n, errFwd := st.w.Write(ollamaChunk)
	st.bytesForwarded.Add(int64(n))
	if errFwd != nil {
		st.lastForwardErr = errFwd
		if st.firstForwardErr == nil {
			st.firstForwardErr = errFwd
		}
		return fmt.Errorf("write NDJSON: %v", errFwd)
	}

	if flusher, ok := st.w.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}
