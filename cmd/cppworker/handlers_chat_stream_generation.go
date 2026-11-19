// handlers_chat_stream_generation.go — R627 (2026-11-19): генерация инструментов и её
// журнал (старт, keepalive, сводка, ошибка).
//
// ЗАЧЕМ ВЫНЕСЕНО. После R625–R626 в `handlers_chat.go` оставалась вторая половина
// стрим-поверхности: запуск генерации инструментов, её keepalive, сводка в лог и
// запись ошибки. Это законченный слой «жизненный цикл tool-генерации» — он не нужен
// тем, кто читает HTTP-поверхность чата.
//
// ПОВЕДЕНИЕ НЕ МЕНЯЛОСЬ: код перенесён как есть; тесты `cmd/cppworker` (наборы
// tools-совместимости, бюджета рассуждений и терминального reason) зелёные до и после.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
)
func startToolsStreamGeneration(x *toolsStreamRun) bool {
	// Неизменяемые входы — локальными именами, чтобы перенесённое тело читалось
	// как прежде.
	modelName := x.modelName
	prompt := x.prompt
	reasoningEcho := x.reasoningEcho
	createdAt := x.createdAt
	params := x.params
	ctx := x.ctx
	w := x.w
	sw := x.sw
	// streamErr — результат генерации. Вызывающая функция читает его в своём
	// defer (recordLastPromptFromErrorWithContext), поэтому итог пишется в её
	// переменную при любом выходе.
	var streamErr error
	defer func() { *x.streamErr = streamErr }()

	emitContentDelta := func(delta string) {
		// R114b (2026-10-13): служебные токены шаблона — не текст ответа. Живой
		// случай: дельта «other coding task.\n<end_of_turn>» уехала клиенту
		// (маркер заканчивался в самом хвосте потока, а прежняя чистка
		// (cleanFinalContent) срабатывает только на финальном чанке).
		delta = stripServiceTokensKeepSpaces(delta)
		// R429 (2026-11-04): одиночные маркеры размышлений не отдаём и здесь.
		//
		// ЗАЧЕМ. Дельта на tools-пути приходит из ДВУХ мест: из нарезки буфера по
		// окну удержания (там R414 сам пропускает маркеры) и из предохранителей
		// 2/2b/2c — они отдают срез `buf[emittedLen:pos]`, где pos — начало
		// разметки вызова. `<channel|>` стоит РОВНО перед вызовом и попадал в
		// такой срез: живой прогон gemma-4 + Cline (R430) закончил потоковую
		// прозу на «…in the file `d:\cline-e2e-web\index.html`.<channel|>».
		// Чистка та же, что на не-tools путях (R114b): список маркеров один —
		// orphanReasoningTags.
		delta = stripReasoningTagsInStreamDelta(delta)
		if delta == "" {
			return
		}
		chunk := map[string]interface{}{
			"model":      modelName,
			"created_at": createdAt,
			"message": map[string]string{
				"role":    "assistant",
				"content": delta,
			},
			"done": false,
		}
		data, _ := json.Marshal(chunk)
		sw.Writef("%s\n", data)
		sw.Flush()
		*x.streamedBytes += len(delta)
		*x.anyStreamed = true
		*x.contentStreamed = true
		if (*x.firstContentAt).IsZero() {
			*x.firstContentAt = time.Now()
		}
	}

	// R404: thinking-дельта уходит сразу, не дожидаясь закрытия блока
	// размышлений (он может не закрыться: бюджет вывода кончается внутри).
	emitThinkingDelta := func(delta string) {
		if delta == "" {
			return
		}
		// R470 (2026-11-06): ЕДИНСТВЕННАЯ точка отправки thinking-дельт на
		// tools-пути. Если клиент не просил размышления (think не задан), не
		// отдаём их: Cline сохраняет их как текст нашего ответа, и модель на
		// следующем ходу видит свои размышления как свой прошлый ответ.
		if !reasoningEcho {
			return
		}
		chunk := toolsThinkingChunkR404(modelName, createdAt, delta)
		data, _ := json.Marshal(chunk)
		sw.Writef("%s\n", data)
		sw.Flush()
		*x.thinkingStreamed = true
		*x.anyStreamed = true
	}

	// R527: дельты и callback — методы на состоянии (funlen: у функции 100
	// вызовов, а не строк). Финализации нужны те же замыкания, поэтому
	// ссылки кладём в структуру сразу.
	x.emitContentDelta = emitContentDelta
	x.emitThinkingDelta = emitThinkingDelta
	callback := x.onStreamToken

	// /api/chat streaming with tools: full buffer, single final response.
	streamErr = generateStreamWithRamFallback(ctx, modelName, prompt, params, callback, false)

	// Если во время streaming произошла ошибка — отдаём финальный chunk с error,
	// как в writeChatStreamResponse. Tool calls в этом случае не анализируем.
	if streamErr != nil {
		writeToolsStreamGenerationError(w, sw, modelName, createdAt, streamErr)
		return false
	}

	x.emitContentDelta = emitContentDelta
	x.emitThinkingDelta = emitThinkingDelta
	x.callback = callback
	return true
}

// logToolsStreamSummary — сводка tools-хода одной строкой и финальный кадр (R527).
//
// ЗАЧЕМ ВЫНЕСЕНО. Это последние ~110 строк writeChatStreamResponseWithTools:
// сбор полей сводки, сторож «слабый ход агента» (R496) и запись done-кадра. Тело
// перенесено дословно; поля, которые продолжают жить в вызывающей функции,
// читаются через toolsStreamRun.
//
// ВАЖНО: причина завершения уже посчитана вызывающей функцией (R394) — здесь
// она только печатается. Переносить сюда расчёт причины нельзя: сторож
// TestR394_ToolsPathUsesBudgetAwareReason проверяет вызовы именно в
// writeChatStreamResponseWithTools.
func logToolsStreamSummary(x *toolsStreamRun, sw *safeStreamWriter, r *http.Request, start time.Time,
	duration time.Duration, fullOutput string, finalChunk map[string]interface{},
	toolCalls []openAIToolCall, tools []openAITool, budgetTokens, generatedTokens int,
	reasoningOut string, toolCallsFromReasoning bool,
) {
	modelName := x.modelName
	params := x.params

	// R113 (2026-10-13): ЯВНОЕ ОТСЛЕЖИВАНИЕ ОТВЕТА. Одна строка на запрос, по
	// которой видно, ЧТО именно уехало клиенту: сколько content отдано дельтами,
	// сколько ушло в финальном чанке, какие вызовы и какого типа аргументы,
	// с каким done_reason. Раньше вопрос «инструмент в теле ответа?» приходилось
	// выяснять по логам клиента.
	toolNames := make([]string, 0, len(toolCalls))
	argKinds := make([]string, 0, len(toolCalls))
	for _, tc := range toolCalls {
		toolNames = append(toolNames, tc.Function.Name)
		argKinds = append(argKinds, toolArgShape(tc.Function.Arguments))
	}
	finalContent := ""
	if msg, ok := finalChunk["message"].(map[string]interface{}); ok {
		finalContent, _ = msg["content"].(string)
	}
	// R457 (2026-11-06): запрос был С инструментами, а ответ — телом файла
	// текстом и без вызова. Живой прогон: HTML-страница на 8587 символов ушла в
	// чат, файл не создан; в логах это выглядело как обычный done_reason=stop,
	// то есть причину «инструменты перестали работать» не было видно вообще.
	fileBodyInsteadOfCall := len(tools) > 0 && len(toolCalls) == 0 && looksLikeFileBodyR457(finalContent)
	if fileBodyInsteadOfCall {
		logger.Get().Warnw("R457: модель ответила телом файла вместо вызова инструмента",
			"model", modelName, "content_len", len(finalContent),
			"tools", len(tools), "done_reason", finalChunk["done_reason"])
	}
	// R474 (2026-11-07): аргументы вызова, выданные текстом. В сводке это
	// отдельное поле по той же причине, что и R457: снаружи такой ход выглядит
	// обычным done_reason=stop, и без поля причину «вызов не выполнен» в логах
	// не видно. Считаем по СЫРОМУ выводу: в finalContent уже лежит пояснение.
	_, _, dumpedArgsInsteadOfCall := looksLikeDumpedArgumentsR474(fullOutput)
	dumpedArgsInsteadOfCall = dumpedArgsInsteadOfCall && len(tools) > 0 && len(toolCalls) == 0
	// R476: снимок трассировки стрима (если включена CPPWORKER_STREAM_TRACE).
	streamTraceChunks, streamTraceBytes, streamTraceSHA := sw.StreamTrace()
	// R496 (2026-11-18): сторож «слабый ход агента» — та же проверка, что в
	// не-стриме, но здесь заголовки уже отправлены (поток идёт), поэтому диагноз
	// уходит в сводку и счётчик: строка с числами хода — единственное место, где
	// оператор сопоставит «пустой/обещающий ответ» с моделью и request_id.
	toolGuardVerdict := observeToolGuardR496(modelName, "/api/chat", ToolGuardInputR496{
		ToolsOffered:    len(tools) > 0,
		ToolCalls:       len(toolCalls),
		DoneReason:      doneReasonStringR496(finalChunk["done_reason"]),
		VisibleContent:  finalContent,
		BudgetExhausted: toolsBudgetExhaustedR394(params.NPredict, budgetTokens),
	})
	logArgs := []interface{}{
		"model", modelName,
		"path", "/api/chat",
		// R476 (2026-11-07): id, который выдал балансер — по нему ход находится
		// в логе балансера одной строкой `request trace`. Без него разбор
		// дефекта сводился к сопоставлению по времени.
		"request_id", getRequestID(r),
		"streamed_content_bytes", *x.streamedBytes,
		// R429: позиция разбора в буфере — отдельным полем. Раньше она печаталась
		// как streamed_content_bytes и расходилась с реально отданным текстом
		// (пропущенные маркеры размышлений, хвост из финального чанка).
		"emitted_len", *x.emittedLen,
		"final_content_len", len(finalContent),
		"thinking_len", len(reasoningOut),
		"tool_calls", len(toolCalls),
		"file_body_instead_of_call", fileBodyInsteadOfCall,
		"dumped_args_instead_of_call", dumpedArgsInsteadOfCall,
		"tool_names", strings.Join(toolNames, ","),
		"tool_arg_shapes", strings.Join(argKinds, ","),
		"done_reason", finalChunk["done_reason"],
		"raw_len", len(fullOutput),
		// R394: по этим полям следующий разбор «почему ход закрылся» решается
		// без догадок: видно клиентский лимит, сколько токенов реально
		// сгенерировано (упёрлись ли в лимит) и сколько символов размышлений
		// было в СЫРОМ выводе (в том числе скрытых через think=false).
		"n_predict", params.NPredict,
		"generated_tokens", generatedTokens,
		"budget_tokens", budgetTokens,
		"budget_exhausted", toolsBudgetExhaustedR394(params.NPredict, budgetTokens),
		"reasoning_chars_raw", reasoningCharsRawR394(fullOutput),
		"elapsed_ms", duration.Milliseconds(),
		// R476: побайтовое доказательство отдачи (CPPWORKER_STREAM_TRACE=1):
		// чанки, записанные байты и sha256 всего записанного. Пусто — трассировка
		// выключена (дефолт).
		"stream_chunks", streamTraceChunks,
		"stream_bytes", streamTraceBytes,
		"stream_sha256", streamTraceSHA,
		// R482: был ли вызов найден в reasoning и снималось ли удержание вывода.
		// Без этих полей живая жалоба «инструменты не применяются, ответ пришёл
		// одним куском» снова разбиралась бы по догадкам.
		"tool_calls_from_reasoning", toolCallsFromReasoning,
		"hold_released", *x.holdReleaseReason,
		"hold_released_ms", func() int64 {
			if (*x.holdReleasedAt).IsZero() || (*x.holdSince).IsZero() {
				return 0
			}
			return (*x.holdReleasedAt).Sub(*x.holdSince).Milliseconds()
		}(),
		"hold_released_bytes", *x.holdReleasedBytes,
		// R485: сторож сработал, но удержание оставлено, потому что оно вызвано
		// размышлениями. Без этого поля в архиве диагностики снова пришлось бы
		// выяснять по логам, почему ход «не потоковый».
		"hold_kept_reasoning", *x.holdKeptReasoning,
		"hold_max_sec", int(toolsHoldMaxHoldR482().Seconds()),
		"hold_max_bytes", toolsHoldMaxBytesR482(),
		"first_content_ms", func() int64 {
			if (*x.firstContentAt).IsZero() {
				return -1
			}
			return (*x.firstContentAt).Sub(start).Milliseconds()
		}(),
	}
	logArgs = append(logArgs, toolGuardFieldsForLogR496(toolGuardVerdict)...)
	logger.Get().Infow("/api/chat stream response summary", logArgs...)

	doneJSON, _ := json.Marshal(finalChunk)
	sw.Writef("%s\n", doneJSON)
	sw.Flush()
}

// startToolsStreamKeepalive — немедленный кадр «запрос принят» и heartbeat-горутина
// tools-стрима (R527).
//
// Тело перенесено дословно из writeChatStreamResponseWithTools, включая разбор
// причин (Round 6 Fix 5, Round 27, R189). Изменено одно: cleanup возвращается
// функцией, а вызывающая сторона ставит его через defer — иначе горутина
// пережила бы ответ и писала в закрытый writer.
func startToolsStreamKeepalive(sw *safeStreamWriter, ctx context.Context, modelName, createdAt string) func() {
	// Round 6 Fix 5: heartbeat goroutine prevents idle-timeout during
	// long generations.
	//
	// Round 27 (v0.5.14 follow-up): /api/chat (Ollama native) is NDJSON,
	// not SSE. SSE comment ": keepalive\n\n" was being parsed as JSON
	// by Cline and other strict NDJSON clients, causing "invalid json"
	// spam. Use a valid NDJSON object `{"keepalive":true}` instead —
	// NDJSON parsers parse it as a no-content chunk and continue.
	// Also bumped interval from 100ms to 15s default (same as OpenAI
	// SSE handler) — 100ms × N seconds of generation = thousands of
	// useless chunks. 15s × N still keeps TCP alive (typical proxy
	// idle-timeout is 30-60s).
	// R189 (2026-10-20): НЕМЕДЛЕННЫЙ кадр «запрос принят».
	//
	// ЖИВОЙ РАЗБОР. На tools-пути текст ответа уходит только по мере генерации
	// (а при размышлениях удерживается окном), поэтому клиент до первого токена
	// не получает НИЧЕГО. На 27B-модели с CPU-оффлоадом префилл + размышления
	// занимают десятки секунд-минуты, и Cline показывал «зависание», хотя
	// запрос обслуживался. Пустой NDJSON-кадр с done:false — валидный ответ
	// Ollama API: клиент видит, что стрим начался, и не рвёт соединение.
	if sw.Writef("{\"model\":%q,\"created_at\":%q,\"message\":{\"role\":\"assistant\",\"content\":\"\"},\"done\":false}\n", modelName, createdAt) {
		sw.Flush()
	}

	keepaliveInterval := getHeartbeatInterval(15 * time.Second)
	heartbeatStop := make(chan struct{})
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(keepaliveInterval)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatStop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !sw.Writef("{\"keepalive\":true}\n") {
					// Клиент отвалился — writer помечен broken, выходим.
					return
				}
				sw.Flush()
			}
		}
	}()
	return func() {
		close(heartbeatStop)
		select {
		case <-heartbeatDone:
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// releaseHoldIfOverdue — R482/R485: снять удержание вывода, если оно длится
// слишком долго или накопило слишком много (R527: вынесено методом из
// startToolsStreamGeneration; тело и комментарии перенесены дословно).
//
// releaseHoldIfOverdue — R482: снять удержание, если оно длится слишком
// долго или накопило слишком много. Накопленное НЕ отдаём (в нём могут быть
// размышления), но все следующие токены пойдут клиенту нормально.
//
// R485: если удержание вызвано РАЗМЫШЛЕНИЯМИ (открытый блок/канал открыт
// шаблоном), удержание НЕ снимаем — иначе размышления уходят в content,
// клиент сохраняет их как ответ ассистента, и на следующем ходу модель видит
// свои же размышления как прошлый ответ (живой архив: content_len=18720/65109
// у assistant-сообщений с tool_calls). Аварийный тумблер —
// CPPWORKER_TOOLS_HOLD_RELEASE_REASONING=1.
func (x *toolsStreamRun) releaseHoldIfOverdue() bool {
	modelName := x.modelName
	prompt := x.prompt
	reasoningEcho := x.reasoningEcho
	outputBuf := x.outputBuf
	r := x.r
	if !*x.reasoningHold && !*x.toolsHold {
		// Удержание закончилось само (R190 нашёл закрывающий маркер) —
		// следующий эпизод считается с нуля.
		*x.holdSince = time.Time{}
		return false
	}
	if (*x.holdSince).IsZero() {
		*x.holdSince = time.Now()
		return false
	}
	held := len(outputBuf.String()) - *x.emittedLen
	ok, why := toolsHoldShouldReleaseR482(*x.holdSince, held, time.Now())
	if !ok {
		return false
	}
	if toolsHoldReasoningR485(*x.reasoningHold, outputBuf.String(), prompt, modelName, reasoningEcho, *x.emittedLen) &&
		!toolsHoldReleaseReasoningR485() {
		if !*x.holdKeptReasoning {
			*x.holdKeptReasoning = true
			logger.Get().Warnw("R485: удержание reasoning не снимаем — иначе размышления уйдут в content",
				"model", modelName,
				"reason", why,
				"held_ms", time.Since(*x.holdSince).Milliseconds(),
				"held_bytes", held,
				"switch", "CPPWORKER_TOOLS_HOLD_RELEASE_REASONING=1 — снять принудительно (риск загрязнения истории)",
				"thinking_switch", "CPPWORKER_TOOLS_THINKING=on — отдавать размышления в поле thinking")
			recordWorkerEventR483(r, "/api/chat", "hold_kept_reasoning",
				fmt.Sprintf("%s held_ms=%d held_bytes=%d", why, time.Since(*x.holdSince).Milliseconds(), held))
		}
		return false
	}
	*x.reasoningHold = false
	*x.toolsHold = false
	*x.emittedLen = len(outputBuf.String())
	*x.holdReleaseReason = why
	*x.holdReleasedAt = time.Now()
	*x.holdReleasedBytes = held
	logger.Get().Warnw("R482: удержание вывода снято — ход снова потоковый",
		"model", modelName,
		"reason", why,
		"held_ms", (*x.holdReleasedAt).Sub(*x.holdSince).Milliseconds(),
		"held_bytes", held,
		"hold_max_sec", int(toolsHoldMaxHoldR482().Seconds()),
		"hold_max_bytes", toolsHoldMaxBytesR482(),
		"note", "накопленное не отдаётся (возможны размышления); последующие токены идут в content")
	// R483: событие в кольцо воркера — иначе «ответ одним куском» в архиве
	// диагностики не видно без чтения текста лога.
	recordWorkerEventR483(r, "/api/chat", "hold_released",
		fmt.Sprintf("%s held_ms=%d held_bytes=%d", why, (*x.holdReleasedAt).Sub(*x.holdSince).Milliseconds(), held))
	return true
}

// onStreamToken — callback генерации tools-пути (R527): чистит дельты, отдаёт
// thinking сразу, держит content окном до конца возможного маркера вызова.
// Тело перенесено дословно; thinkingParser читается из ячейки вызывающей
// функции на КАЖДОМ вызове — повтор R406 в финализации заменяет парсер.
func (x *toolsStreamRun) onStreamToken(token string) bool {
	modelName := x.modelName
	ctx := x.ctx
	r := x.r
	sw := x.sw
	outputBuf := x.outputBuf
	streamContent := x.streamContent
	// thinkingParser читаем у вызывающей функции на КАЖДОМ вызове: повтор
	// R406 в финализации заменяет парсер чистым, и callback обязан взять новый,
	// иначе дельты второй попытки смешались бы с состоянием первой.
	thinkingParser := *x.thinkingParserCell
	select {
	case <-ctx.Done():
		// R483: клиент отменил запрос — это событие хода (в кольцо сбоев).
		recordWorkerEventR483(r, r.URL.Path, "client_cancelled", "request context canceled by client")
		return false
	default:
	}
	// R83: writer broken (клиент ушёл) → прекращаем генерацию немедленно.
	if sw.IsBroken() {
		// R483: обрыв записи клиенту — событие хода, а не только строка в логе.
		recordWorkerEventR483(r, r.URL.Path, "client_gone", "stream write failed or client disconnected")
		return false
	}
	// R83: чистим управляющие символы (см. stream_sanitize.go).
	token = sanitizeStreamText(token)
	outputBuf.WriteString(token)
	// R482: если удержание вывода длится слишком долго (шаблон не закрывает
	// канал размышлений), снимаем его — иначе весь ответ уйдёт одним куском.
	if x.releaseHoldIfOverdue() {
		return true
	}
	// R404: thinking отдаём сразу — он не может быть вызовом инструмента,
	// поэтому не ждёт закрытия блока и не попадает под reasoningHold ниже.
	if td := toolsFeedThinkingR404(thinkingParser, token); td != "" {
		x.emitThinkingDelta(td)
	}
	// R66b: TTFT — время первого непустого токена.
	if token != "" {
		atomic.CompareAndSwapInt64(x.firstTokenAt, 0, time.Now().UnixNano())
	}
	// R190: пока открыт блок размышлений — держим; как только закрыт — отдаём дельты.
	if *x.reasoningHold {
		if end := reasoningBlockEnd(outputBuf.String()); end > 0 {
			*x.reasoningHold = false
			*x.toolsHold = false
			*x.emittedLen = end
			logger.Get().Debugw(i18n.T("cppworker.handlers_chat.tools_reasoning_block_closed"),
				"model", modelName, "reasoning_end", end)
		} else {
			return true
		}
	}
	if !streamContent || *x.toolsHold || token == "" {
		return true
	}
	// R527: предохранители и нарезка окна — отдельным методом (funlen).
	x.emitContentWindow(token)
	return true
}

// emitContentWindow — предохранители и нарезка окна удержания tools-пути
// (R527: вынесено из onStreamToken; порядок проверок и комментарии прежние).
// Дельта уходит клиенту только после всех предохранителей: разметка вызова,
// JSON-тело, служебные маркеры и размышления в content не попадают.
func (x *toolsStreamRun) emitContentWindow(token string) {
	modelName := x.modelName
	prompt := x.prompt
	reasoningEcho := x.reasoningEcho
	outputBuf := x.outputBuf
	buf := outputBuf.String()
	// Предохранитель 1: ответ начинается с JSON — парсер может счесть его
	// tool_calls целиком (стратегия 4), поэтому дельтами не отдаём.
	//
	// R106d (2026-10-12): то же для markdown-фенса (```json) — модели кладут
	if *x.emittedLen == 0 && (toolsStreamStartsWithJSON(buf) || toolsStreamStartsWithFence(buf)) {
		*x.toolsHold = true
		return
	}
	// R106d (2026-10-12): то же для markdown-фенса (```json) — вызывать после JSON-проверки.
	// Раньше здесь стояла вторая ветка guard-а; после R190 она объединена выше.
	// Предохранитель 1b (R106c, 2026-10-12): ответ начинается с reasoning-блока
	// (<think>, <thinking>, <reasoning>, <analysis>, <|channel>thought).
	// Размышления — НЕ ответ, и клиенту в content их отдавать нельзя: Cline
	// показывал thinking как ответ («команда инструмента в квадратных
	// скобках»), а сам tool call терялся. Держим дельты и отдаём reasoning
	// отдельным полем в финальном чанке (см. reasoningOut ниже).
	if *x.emittedLen == 0 && startsWithReasoningMarker(buf) {
		*x.reasoningHold = true
		*x.toolsHold = true
		logger.Get().Debugw(i18n.T("cppworker.handlers_chat.tools_starts_with_reasoning"),
			"model", modelName)
		return
	}
	// R471 (2026-11-06): ОТКРЫВАЮЩЕГО маркера может НЕ быть в тексте (канал
	// открыт шаблоном), и тогда начало вывода уходило клиенту как ответ.
	// Живой прогон: streamed_content_bytes=5363 при content_len=3323 — 2040
	// символов английских размышлений уехали в content вместе с русским
	// ответом, хотя финальный разбор считал их reasoning. Держим начало до
	// закрывающего маркера (см. reasoning_start_hold.go).
	if *x.emittedLen == 0 && reasoningStartHoldR471(prompt, modelName, reasoningEcho) {
		*x.reasoningHold = true
		*x.toolsHold = true
		logger.Get().Infow("R471: начало вывода удерживается до закрывающего маркера размышлений",
			"model", modelName,
			"why", "клиент не просил размышления, но модель их умеет и в промпте они включены",
			"switch", "CPPWORKER_TOOLS_THINKING")
		return
	}
	// Предохранитель 1c (R110, 2026-10-13): ответ начинается со СЛУЖЕБНОГО
	// токена шаблона (`<end_of_turn>`, `<|end|>`, `<start_of_turn>model`).
	// Такие дельты тоже не отдаём: служебный токен — не текст ответа, а когда
	// буфер перерастал окно удержания, `cut` попадал уже ПОСЛЕ токена и клиент
	// получал его как обычный текст (живой случай: «ERROR: EMPTY
	// CONTENT<end_of_turn>» в записи сессии Cline).
	if *x.emittedLen == 0 && toolsStreamStartsWithServiceToken(buf) {
		*x.toolsHold = true
		logger.Get().Debugw(i18n.T("cppworker.handlers_chat.tools_starts_with_service_token"),
			"model", modelName)
		return
	}
	// Предохранитель 2: в хвосте появился маркер tool call. Prose ПЕРЕД
	// маркером обязан уйти клиенту (он уже сгенерирован и не является
	// частью вызова) — иначе окно удержания съедало бы до 32 байт ответа.
	if pos, ok := toolsStreamMarkerPos(buf, toolsStreamHoldback+len(token)); ok {
		if pos > *x.emittedLen {
			x.emitContentDelta(buf[*x.emittedLen:pos])
			*x.emittedLen = pos
		}
		*x.toolsHold = true
		logger.Get().Debugw(i18n.T("cppworker.handlers_chat.tools_marker_found"),
			"model", modelName, "emitted", *x.emittedLen, "buffered", len(buf))
		return
	}
	// Предохранитель 2b (R113, 2026-10-13): JSON-форма вызова ВНУТРИ прозы.
	// Прежние проверки держали только вывод, НАЧИНАЮЩИЙСЯ с JSON; живой прогон
	// Cline-запроса дал прозу с последующим JSON вызова — и он уехал клиенту
	// дельтами (в сессии это «инструмент в теле ответа»).
	if pos, ok := toolsStreamJSONCallPos(buf, toolsStreamHoldback+len(token)); ok {
		if pos > *x.emittedLen {
			x.emitContentDelta(buf[*x.emittedLen:pos])
			*x.emittedLen = pos
		}
		*x.toolsHold = true
		logger.Get().Debugw(i18n.T("cppworker.handlers_chat.tools_json_call_in_prose"),
			"model", modelName, "emitted", *x.emittedLen, "buffered", len(buf))
		return
	}
	// Предохранитель 2c (R114b, 2026-10-13): в НЕПЕРЕДАННОМ хвосте началась
	// JSON-структура. Дельты прекращаем на её первой скобке: иначе начало
	// вызова уходит клиенту обрывком («…task complete. [{"id":
	// "call_submit_and_exit","type":"function",…» — живая сессия Cline:
	// объект начинался раньше, чем в окне удержания появлялся ключ, на
	// который смотрел прежний предохранитель).
	if pos, ok := toolsStreamJSONStructPos(buf, len(buf)-*x.emittedLen); ok {
		if pos > *x.emittedLen {
			x.emitContentDelta(buf[*x.emittedLen:pos])
			*x.emittedLen = pos
		}
		*x.toolsHold = true
		logger.Get().Debugw(i18n.T("cppworker.handlers_chat.tools_json_struct_in_tail"),
			"model", modelName, "emitted", *x.emittedLen, "buffered", len(buf))
		return
	}
	// Предохранитель 2d (R114b): открывающий маркер размышлений в хвосте —
	// блок начался после отданной прозы, и его тело не является ответом.
	if reason, hold := toolsStreamHoldsTailStart(buf[*x.emittedLen:]); hold {
		*x.toolsHold = true
		logger.Get().Debugw(i18n.T("cppworker.handlers_chat.tools_tail_held"),
			"model", modelName, "reason", reason,
			"emitted", *x.emittedLen, "buffered", len(buf))
		return
	}
	cut := len(buf) - toolsStreamHoldback
	if cut <= *x.emittedLen {
		return
	}
	cut = toolsStreamRuneSafeCut(buf, *x.emittedLen, cut)
	if cut <= *x.emittedLen {
		return
	}
	// R414 (2026-11-03): служебные маркеры размышлений (в частности
	// gemma-4 `<channel|>`) не должны уходить клиенту как текст ответа.
	// Живой случай: «…run the check command.<channel|>» в сессии Cline.
	// Сначала удерживаем хвост, который может быть НАЧАЛОМ маркера, затем
	// отдаём текст до первого ПОЛНОГО маркера и пропускаем сам маркер.
	delta := buf[*x.emittedLen:cut]
	if hb := toolsStreamMarkerPrefixHoldbackR414(delta); hb > 0 {
		cut -= hb
		if cut <= *x.emittedLen {
			return
		}
		cut = toolsStreamRuneSafeCut(buf, *x.emittedLen, cut)
		if cut <= *x.emittedLen {
			return
		}
		delta = buf[*x.emittedLen:cut]
	}
	if pos, markerLen, found := toolsStreamMarkerScanR414(delta); found {
		if pos > 0 {
			x.emitContentDelta(delta[:pos])
		}
		// Маркер не отдаём: сдвигаем позицию ЗА него, чтобы он не попал
		// в следующую дельту.
		*x.emittedLen += pos + markerLen
		return
	}
	x.emitContentDelta(delta)
	*x.emittedLen = cut
}

// writeToolsStreamGenerationError — кадры ошибки генерации tools-пути (R527):
// reload-loop-limit уходит с кодом и HTTP-статусом 413, остальное — общим кадром.
func writeToolsStreamGenerationError(w http.ResponseWriter, sw *safeStreamWriter, modelName, createdAt string, streamErr error) {
	maybeRestartOnMemorySlotError(streamErr, modelName)
	// Специальная обработка reload-loop-limit: HTTP-статус 413 в NDJSON-чанке
	// с error+code+model+attempts. OpenWebUI увидит error и завершит сессию
	// с понятным сообщением (вместо бесконечного цикла unload+reload).
	if rllErr, ok := streamErr.(*ReloadLoopLimitError); ok {
		errChunk := map[string]interface{}{
			"model":           modelName,
			"created_at":      createdAt,
			"message":         map[string]string{"role": "assistant", "content": ""},
			"done":            true,
			"done_reason":     "error",
			"error":           rllErr.Error(),
			"code":            "reload_loop_limit",
			"attempts":        rllErr.Count,
			"elapsed_seconds": rllErr.Elapsed.Seconds(),
			"http_status":     http.StatusRequestEntityTooLarge,
		}
		errJSON, _ := json.Marshal(errChunk)
		// Также ставим HTTP-статус в response (best-effort: если streaming уже начат — http.status будет проигнорирован клиентом, но код в NDJSON всё равно будет передан).
		w.Header().Set("X-CppWorker-Error", "reload_loop_limit")
		w.Header().Set("X-HTTP-Status", "413")
		sw.Writef("%s\n", errJSON)
		sw.Flush()
		return
	}
	errChunk := map[string]interface{}{
		"model":       modelName,
		"created_at":  createdAt,
		"message":     map[string]string{"role": "assistant", "content": ""},
		"done":        true,
		"done_reason": "error",
		"error":       streamErr.Error(),
	}
	errJSON, _ := json.Marshal(errChunk)
	sw.Writef("%s\n", errJSON)
	sw.Flush()
}