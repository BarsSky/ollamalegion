// handlers_chat_stream_tools.go — R626 (2026-11-19): стрим-ответ с инструментами
// (`writeChatStreamResponseWithTools`, 985 строк).
//
// ЗАЧЕМ ВЫНЕСЕНО. Это самая большая функция репозитория и одновременно главный
// вкладчик в метрику «функций >300 строк». Перенос в отдельный файл — первый шаг:
// функция остаётся ОДНОЙ (поведение и порядок кадров не меняются), но перестаёт
// делить файл с HTTP-поверхностью чата, а её правки больше не конфликтуют с правками
// `handleChat`/`writeChatStreamResponse`.
//
// ПОВЕДЕНИЕ НЕ МЕНЯЛОСЬ: код перенесён как есть; тесты `cmd/cppworker` (наборы
// tools-совместимости и golden-кадры SSE) зелёные до и после.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
)
func writeChatStreamResponseWithTools(w http.ResponseWriter, r *http.Request, modelName, prompt string, params bridge.GenerationParams, jsonMode bool, thinkOverride *bool, tools []openAITool, promptNoThink string, clientCapped bool) {
	// R83 (2026-09-26): тот же safeStreamWriter, что и в writeChatStreamResponse —
	// здесь тоже пишут две горутины (heartbeat + финальные чанки), и на длинной
	// генерации без мьютекса фреймы могли перемешаться.
	sw := newSafeStreamWriter(w, r, "writeChatStreamResponseWithTools", modelName)
	// 2026-06-24: preflight n_ctx check BEFORE flushing headers.
	if err := clampNPredictToFitContext(modelName, prompt, &params); err != nil {
		var perr *PromptExceedsNCtxError
		if errors.As(err, &perr) {
			writePromptExceedsNCtxResponse(w, perr)
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	sw.SetHeader("Content-Type", "application/x-ndjson")
	sw.SetHeader("Cache-Control", "no-cache")
	sw.SetHeader("Connection", "keep-alive")
	// Round 6 Fix 5: do NOT flush headers immediately — buffer the response.
	// Previously every prose token was sent on the wire and then the final
	// chunk tried to set content="" with tool_calls — clients got garbage
	// (raw JSON in chat). New approach: callback ONLY writes to outputBuf,
	// no flushes. After inference, parse tool_calls and emit one final
	// chunk (either tool_calls or content). Heartbeat goroutine keeps
	// the connection alive during long generations.
	sw.Flush()
	ctx := r.Context()
	// R63 (2026-09-15): per-infer AbortWatcher создаётся внутри GenerateStream
	// (inference.go:521), там где доступен abortFlag от C-bridge.
	start := time.Now()
	createdAt := time.Now().UTC().Format(time.RFC3339)
	// R66b: TTFT для prompt_eval_duration (см. writeChatStreamResponse).
	var firstTokenAt int64

	// Buffer full output for final tool_calls detection.
	var outputBuf strings.Builder

	// R482: сторож удержания вывода. См. tools_hold_max_bytes.go: без него шаблон,
	// который не закрывает канал размышлений до конца хода, держит ВЕСЬ ответ
	// до финального чанка («одним огромным куском», живая жалоба на 27B).
	var holdSince time.Time
	holdReleaseReason := ""
	holdReleasedAt := time.Time{}
	holdReleasedBytes := 0
	// R485: «удержание reasoning не снимали» — чтобы писать это в лог/кольцо один раз.
	holdKeptReasoning := false
	firstContentAt := time.Time{}

	// R404: размышления отдаём ДЕЛЬТАМИ, не дожидаясь закрытия блока.
	//
	// На tools-пути content удерживается окном (там может начаться разметка
	// tool call), но thinking вызовом инструмента быть не может — его можно
	// отдавать сразу. До R404 старт блока размышлений включал reasoningHold, и
	// при исчерпании бюджета ВНУТРИ блока клиент не получал ни байта минутами
	// (живой лог: 16647 символов размышлений, done_reason=length, 362 с, затем
	// повтор R188 ещё на столько же и обрыв).
	thinkingParser := NewReasoningStreamState()
	thinkingParser.SetImpliedOpen(PromptOpensReasoning(prompt))
	// R470 (2026-11-06): эхо размышлений на tools-пути — только по явной просьбе
	// клиента (think:true). Иначе Cline сохраняет их как текст нашего ответа, и
	// модель на следующем ходу видит свои размышления как свой прошлый ответ
	// (разбор — reasoning_echo.go). Разбор вывода и промпт не меняются.
	reasoningEcho := toolsThinkingEchoR470(thinkOverride)
	thinkingStreamed := false
	// anyStreamed — клиенту ушёл ХОТЬ ОДИН байт (thinking или content).
	// Нужен, чтобы не запускать R188-повтор после уже отданных размышлений:
	// повтор сгенерировал бы их заново и снова съел бюджет.
	anyStreamed := false
	// R406: видимый текст клиенту уже ушёл? Нужно, чтобы аварийный повтор без
	// размышлений не дублировал уже показанный ответ.
	contentStreamed := false

	// R527: кадр «запрос принят» и heartbeat-горутина вынесены вместе с их
	// разбором (startToolsStreamKeepalive): у функции остаётся только проводка.
	stopKeepalive := startToolsStreamKeepalive(sw, ctx, modelName, createdAt)
	defer stopKeepalive()

	// Snapshot for chat streaming (debug endpoint).
	var streamErr error
	defer func() {
		recordLastPromptFromErrorWithContext(modelName, "/api/chat", prompt, &params, true, streamErr, r)
	}()

	// R83/v56 (2026-10-01): потоковая отдача content.
	//
	// Round 6 Fix 5 буферизовал ответ целиком («клиент не должен видеть сырой
	// JSON tool call»), из-за чего на медленном инстансе клиент не получал ни
	// одного токена до конца генерации и отваливался по своему таймауту.
	// Теперь prose уходит по мере генерации, а маркер tool call удерживается
	// окном toolsStreamHoldback (см. tools_stream_content.go).
	streamContent := toolsStreamContentEnabled()
	// R190 (2026-10-20): удержание из-за блока размышлений снимается, когда блок ЗАКРЫТ.
	// Раньше 	oolsHold после reasoning-маркера больше не снимался, и видимый ответ
	// (content_len=378 в живом логе) уходил клиенту одним кадром в конце хода.
	reasoningHold := false
	var emittedLen int
	// R429 (2026-11-04): РЕАЛЬНО отданные клиенту байты content, а не позиция в
	// буфере. Раньше сводка печатала emittedLen, и она врала в двух случаях:
	//   - хвост из финального чанка (не-tool_calls) отдаётся без сдвига
	//     emittedLen → сводка занижала поток (живой пример сегодня:
	//     streamed_content_bytes=3714 при принятых клиентом 14133);
	//   - сдвиг за маркер размышлений (R414) двигает emittedLen на длину
	//     маркера, которого клиент не получал.
	// Диагностика, по которой разбирают «ответ оборван/не доехал», обязана
	// считать отправленное, а не индексы — иначе разбор ведёт не туда.
	var streamedBytes int
	var toolsHold bool
	// R527: начало tools-стрима (дельты, сторожа удержания, callback, сама
	// генерация) вынесено в startToolsStreamGeneration. Состояние остаётся
	// здесь: финализация ниже читает и меняет те же переменные, а callback
	// меняет их через указатели — поэтому копий состояния нет.
	run := &toolsStreamRun{
		params:             params,
		w:                  w,
		ctx:                ctx,
		modelName:          modelName,
		createdAt:          createdAt,
		prompt:             prompt,
		outputBuf:          &outputBuf,
		sw:                 sw,
		r:                  r,
		thinkingParserCell: &thinkingParser,
		streamErr:          &streamErr,
		firstTokenAt:       &firstTokenAt,
		emittedLen:         &emittedLen,
		streamedBytes:      &streamedBytes,
		anyStreamed:        &anyStreamed,
		contentStreamed:    &contentStreamed,
		thinkingStreamed:   &thinkingStreamed,
		reasoningHold:      &reasoningHold,
		toolsHold:          &toolsHold,
		holdReleasedBytes:  &holdReleasedBytes,
		holdKeptReasoning:  &holdKeptReasoning,
		holdSince:          &holdSince,
		holdReleasedAt:     &holdReleasedAt,
		firstContentAt:     &firstContentAt,
		holdReleaseReason:  &holdReleaseReason,
		streamContent:      streamContent,
		reasoningEcho:      reasoningEcho,
	}
	if !startToolsStreamGeneration(run) {
		return
	}
	emitContentDelta := run.emitContentDelta
	emitThinkingDelta := run.emitThinkingDelta
	callback := run.callback

	fullOutput := outputBuf.String()
	duration := time.Since(start)

	// ?????????? ?????????? tool_calls ?? ?????????? ??????.
	//
	// R106c (2026-10-12): СНАЧАЛА отделяем reasoning. С включёнными
	// размышлениями модель отдаёт
	//	<|channel>thought … <channel|>[{"name":"read_files", …}]
	// и раньше весь этот текст уходил в message.content: размышления не
	// отделялись, parseToolCallsFromOutput получал строку с thinking-префиксом и
	// часто не находил вызов → клиент (Cline) видел «команду инструмента в
	// квадратных скобках» вместо вызова инструмента.
	//
	// R108 (2026-10-13): разбор безусловный (по тегам, не по имени модели), а при
	// явном think=false размышления не отдаются вовсе — см. «Политика отдачи
	// reasoning» в reasoning_content.go.
	reasoningOut, outputForParse, toolCalls, hasReasoning, toolCallsFromReasoning := splitToolsOutputR482(fullOutput, thinkOverride, tools)
	if hasReasoning {
		logger.Get().Infow(i18n.T("cppworker.handlers_chat.tools_reasoning_split"),
			"model", modelName, "reasoning_len", len(reasoningOut), "reasoning_echo", reasoningEcho, "content_len", len(outputForParse),
			"reasoning_sent", reasoningOut != "" && reasoningEcho,
			"reasoning_echo", reasoningEcho)
	}
	// R406: токены первой попытки нужны для решения о повторе без размышлений
	// и для честного eval_count (если повтор состоится — сложим обе попытки).
	generatedTokens := countTokensSafe(modelName, fullOutput)
	// budgetTokens — токены ПОСЛЕДНЕЙ попытки: по ним решается «упёрлись в
	// n_predict». generatedTokens после повтора — сумма обеих попыток (usage),
	// и по ней бюджет всегда выглядел бы исчерпанным.
	budgetTokens := generatedTokens

	// R83/v59 (2026-10-01) + R110 (2026-10-13): повтор генерации, когда видимого
	// ответа не получилось.
	//
	// Живой факт (пробник debug/streamprobe, gemma-4-E4B-it-Q4_K_M, запрос Cline
	// с 18 tools): модель иногда завершает ход ПЕРВЫМ же токеном EOG — в
	// /debug-логе видно raw_len=0, то есть не сгенерировано ни байта. Клиенту
	// уходил чанк "model produced an empty response", и ни повторить, ни понять
	// причину он не мог.
	//
	// R110: тот же симптом даёт вывод из ОДНИХ служебных токенов шаблона
	// (`<end_of_turn>`) — после чистки видимого текста нет. Повтор безопасен
	// ровно тем же условием, что и раньше: клиенту не отправлено ни байта
	// (emittedLen == 0), иначе он получил бы два ответа подряд.
	//
	// Сэмплирование стохастично, поэтому повторный проход обычно даёт ответ.
	if len(toolCalls) == 0 {
		// R188 (2026-10-20): клиентский лимит длины ответа делает повтор бессмысленным —
		// тот же лимит отрежет тот же вывод. При ограниченном выводе разрешён максимум
		// один повтор (см. handlers_chat_retry.go).
		retryBudget := emptyOutputRetryBudget(clientCapped)
		if clientCapped {
			// R464 (2026-11-06): «клиент задал длину» — это только ЯВНЫЙ
			// num_predict/max_tokens. Наш собственный дефолт (8192 для
			// reasoning-моделей) таким ограничением не является: раньше лог
			// утверждал «клиент задал длину ответа n_predict=8192», хотя клиент
			// длину не задавал, и бюджет повторов урезался без причины.
			logger.Get().Infow(i18n.T("cppworker.handlers_chat.tools_retry_limited_r188"),
				"model", modelName, "n_predict", params.NPredict, "retries_allowed", retryBudget,
				"client_set_limit", true)
		}
		for attempt := 1; attempt <= retryBudget; attempt++ {
			// R404: клиенту уже ушли размышления (или текст) — повтор
			// сгенерировал бы их заново и снова съел бы весь бюджет. Живой
			// случай: 16647 символов thinking, 0 видимого текста, n_predict=4096,
			// повтор R188 — ещё 6 минут тишины и обрыв клиентом.
			if anyStreamed {
				logger.Get().Infow("writeChatStreamResponseWithTools: retry skipped — stream already started (R404)",
					"model", modelName, "attempt", attempt,
					"thinking_streamed", thinkingStreamed,
					"n_predict", params.NPredict)
				break
			}
			emptyOutput := shouldRetryEmptyStreamingOutput(fullOutput, emittedLen, ctx)
			// R113 (2026-10-13): повторяем и тогда, когда модель ВЫДАЛА разметку
			// вызова, но распознать её не удалось. Повтор безопасен по тому же
			// условию: клиенту не отправлено ни байта (emittedLen == 0) — в
			// tools-пути разметка удерживается окном, поэтому обычно так и есть.
			unparsedMarkup := shouldRetryUnparsedToolMarkup(fullOutput, emittedLen, ctx)
			if !emptyOutput && !unparsedMarkup {
				break
			}
			// R483: повтор из-за пустого/нераспознанного вывода — событие хода.
			recordWorkerEventR483(r, "/api/chat", "turn_retry",
				retryReasonLabel(emptyOutput, unparsedMarkup))
			logger.Get().Warnw(i18n.T("cppworker.handlers_chat.tools_retry"),
				"model", modelName, "attempt", attempt, "reason", retryReasonLabel(emptyOutput, unparsedMarkup),
				"raw_len", len(fullOutput),
				"raw_head", truncateForLog(fullOutput, 80),
				"prompt_tokens", countTokensSafe(modelName, prompt))
			outputBuf.Reset()
			emittedLen = 0
			toolsHold = false
			// R404: парсер thinking тоже начинается с чистого листа — иначе
			// дельты второй попытки смешались бы с состоянием первой.
			thinkingParser = NewReasoningStreamState()
			thinkingParser.SetImpliedOpen(PromptOpensReasoning(prompt))
			thinkingStreamed = false
			atomic.StoreInt64(&firstTokenAt, 0)
			if retryErr := generateStreamWithRamFallback(ctx, modelName, prompt, params, callback, false); retryErr != nil {
				logger.Get().Warnw(i18n.T("cppworker.handlers_chat.tools_retry_failed"),
					"model", modelName, "attempt", attempt, "error", retryErr)
				break
			}
			fullOutput = outputBuf.String()
			duration = time.Since(start)
			reasoningOut, outputForParse, toolCalls, hasReasoning, toolCallsFromReasoning = splitToolsOutputR482(fullOutput, thinkOverride, tools)
			logger.Get().Infow(i18n.T("cppworker.handlers_chat.tools_retry_done"),
				"model", modelName, "attempt", attempt, "raw_len", len(fullOutput),
				"tool_calls", len(toolCalls), "visible_len", len(cleanFinalContent(outputForParse)),
				"has_reasoning", hasReasoning)
			if len(toolCalls) > 0 {
				break
			}
		}
	}

	// R406 (2026-11-03): АВАРИЙНЫЙ ПОВТОР БЕЗ РАЗМЫШЛЕНИЙ.
	//
	// ЖИВОЙ ДЕФЕКТ (Cline): клиент задаёт max_tokens (4096), модель тратит весь
	// бюджет на thinking и не выдаёт видимого ответа. Мы честно отдаём
	// done_reason=length, Cline трактует это как «output token limit» и
	// запускает overflow-recovery: compacting → compacted, пересказ истории,
	// повтор — и часть переписки исчезает, а клиент падает с
	// «text part … not found».
	//
	// Повтор с ВЫКЛЮЧЕННЫМ thinking (тот же бюджет, тот же запрос, только
	// enable_thinking=false в chat template) даёт модели шанс ответить вместо
	// повторного ухода в размышления. Условие — ничего видимого ещё не ушло
	// (thinking-дельты не считаются: они уже показаны и повторять их не нужно).
	// R493: promptOpensReasoning — шаблон сам открыл <think> в конце промпта;
	// в таком случае сырой вывод БЕЗ маркеров — это размышления, и предикат
	// обязан их видеть (иначе живой «пустой ответ» после 8 минут thinking).
	// R527: повтора ещё не было — блок ниже единственный вход в него, поэтому
	// retried=false. Переменная-флаг удалена: присваивание после повтора ни на что
	// не влияло (ineffassign), и она не переживала свою же область чтения.
	if toolsShouldRetryWithoutThinkingR406(params.NPredict, generatedTokens, fullOutput,
		cleanFinalContent(outputForParse), promptNoThink, contentStreamed, len(toolCalls), false,
		PromptOpensReasoning(prompt), reasoningBudgetTokensR493()) {

		attempt1Tokens := generatedTokens
		logger.Get().Warnw("writeChatStreamResponseWithTools: reasoning exhausted the output budget — retrying with thinking disabled (R406)",
			"model", modelName, "n_predict", params.NPredict,
			"generated_tokens", generatedTokens,
			"reasoning_chars", reasoningCharsRawR394(fullOutput),
			"thinking_streamed", thinkingStreamed)
		outputBuf.Reset()
		emittedLen = 0
		toolsHold = false
		reasoningHold = false
		// thinkingStreamed НЕ сбрасываем: клиент уже видел размышления первой
		// попытки, в финале их дублировать нельзя.
		thinkingParser = NewReasoningStreamState()
		thinkingParser.SetImpliedOpen(false)
		atomic.StoreInt64(&firstTokenAt, 0)
		if retryErr := generateStreamWithRamFallback(ctx, modelName, promptNoThink, params, callback, false); retryErr != nil {
			logger.Get().Warnw("writeChatStreamResponseWithTools: no-thinking retry failed (R406)",
				"model", modelName, "error", retryErr)
		} else {
			fullOutput = outputBuf.String()
			duration = time.Since(start)
			reasoningOut, outputForParse, toolCalls, _, toolCallsFromReasoning = splitToolsOutputR482(fullOutput, thinkOverride, tools)
			// eval_count — сумма обеих попыток: клиент видел thinking первой и
			// ответ второй; занижать нельзя (по usage он считает контекст).
			// Для решения о бюджете берём ТОЛЬКО вторую попытку: первая уже
			// израсходовала n_predict на размышления, но ответ второй может
			// быть полным.
			retryTokens := countTokensSafe(modelName, fullOutput)
			generatedTokens = attempt1Tokens + retryTokens
			budgetTokens = retryTokens
			logger.Get().Infow("writeChatStreamResponseWithTools: no-thinking retry produced output (R406)",
				"model", modelName,
				"attempt1_tokens", attempt1Tokens,
				"attempt2_tokens", generatedTokens-attempt1Tokens,
				"tool_calls", len(toolCalls),
				"visible_len", len(cleanFinalContent(outputForParse)))
		}
	}

	// R247 (2026-10-26), этап A+F плана: STRICT-аргументы.
	//
	// Клиент объявил tools[].function.strict=true — значит аргументы обязаны
	// соответствовать схеме (structured outputs). Ремонт R246 покрывает типовые
	// расхождения, но не все; раньше невалидный вызов уходил клиенту, тот отвечал
	// ошибкой инструмента, и агент тратил ход.
	//
	// Здесь: проверить по схеме и, если клиенту ещё ничего не отправлено, один
	// раз повторить генерацию (тот же промпт — сэмплирование стохастично).
	// Выключатель: CPPWORKER_STRICT_ARGS_RETRY=0.
	if len(toolCalls) > 0 && emittedLen == 0 && !anyStreamed && strictArgsRetryEnabled() {
		if violations := strictToolCallViolations(toolCalls, tools); len(violations) > 0 {
			logger.Get().Warnw("strict tool args invalid, retrying generation once",
				"model", modelName, "tool", toolCalls[0].Function.Name,
				"violations", truncateForLog(strings.Join(violations, "; "), 300))
			outputBuf.Reset()
			toolsHold = false
			thinkingParser = NewReasoningStreamState()
			thinkingParser.SetImpliedOpen(PromptOpensReasoning(prompt))
			thinkingStreamed = false
			atomic.StoreInt64(&firstTokenAt, 0)
			if retryErr := generateStreamWithRamFallback(ctx, modelName, prompt, params, callback, false); retryErr == nil {
				fullOutput = outputBuf.String()
				duration = time.Since(start)
				reasoningOut, outputForParse, toolCalls, _, toolCallsFromReasoning = splitToolsOutputR482(fullOutput, thinkOverride, tools)
				if stillInvalid := strictToolCallViolations(toolCalls, tools); len(stillInvalid) > 0 {
					logger.Get().Warnw("strict tool args still invalid after retry",
						"model", modelName, "violations", truncateForLog(strings.Join(stillInvalid, "; "), 300))
				} else if len(toolCalls) > 0 {
					logger.Get().Infow("strict tool args valid after retry",
						"model", modelName, "tool", toolCalls[0].Function.Name)
				}
			} else {
				logger.Get().Warnw(i18n.T("cppworker.handlers_chat.tools_retry_failed"),
					"model", modelName, "error", retryErr)
			}
		}
	}

	// R404: выпустить хвост, который парсер thinking удерживал (возможный
	// неполный маркер). Без этого последние символы размышлений терялись бы:
	// они не входили ни в одну дельту, а в финальном чанке thinking мы больше
	// не дублируем, если он уже отдан дельтами.
	if thinkingTail, _ := thinkingParser.FinalizeForStream(); thinkingTail != "" {
		emitThinkingDelta(thinkingTail)
	}

	// R394.2 (2026-11-04): сколько токенов реально сгенерировано — считаем
	// ТОКЕНИЗАТОРОМ по сырому выводу (то же число, что уходит в eval_count).
	//
	// ЖИВОЙ ПРОГОН на стенде показал, почему НЕЛЬЗЯ считать вызовы callback'а:
	// при num_predict=16 воркер выдал eval_count=17, а callback'ов с непустым
	// токеном было 14 (часть токенов приходит пустой строкой/служебной).
	// Счётчик по callback'ам занижал вывод, бюджет считался неизрасходованным,
	// и обрезанный ответ снова закрывался как done_reason=stop — ровно то, что
	// чинит R394. Токенизатор даёт точное число независимо от того, как мост
	// режет поток на куски.
	//
	// R406: generatedTokens посчитан выше и уже учитывает возможный повтор без
	// размышлений (сумма обеих попыток) — второй раз не пересчитываем.

	// R113 (2026-10-13): РАЗМЕТКА ВЫЗОВА НЕ ЯВЛЯЕТСЯ ОТВЕТОМ.
	//
	// Проверка стоит ЗДЕСЬ, до ветки «сырой вывод как content»: cleanFinalContent
	// разметку вызова не знает и НЕ делает вывод пустым, поэтому прежняя ветка не
	// срабатывала и разметка уезжала клиенту через финальный чанк (живой прогон:
	// content 2456 символов с сырым JSON вызова). Агентский клиент видит тогда
	// финальный ответ вместо вызова и помечает задачу выполненной — жалоба
	// «инструмент опять оказался в теле ответа».
	//
	// R430 (2026-11-04): если разметка найдена, а вызов не распознан, а проза
	// ЕСТЬ (markup_stripped), клиенту мало одной прозы: агентский клиент считает
	// ход завершённым и задачу — выполненной, хотя инструмент не вызывался
	// (живая жалоба: «Cline пишет COMPLETED, файла нет»). Поэтому к прозе
	// добавляется пояснение — один раз, в одном месте (см. unparsedNoticeR430).
	var unparsedNoticeR430 string
	// R468 (2026-11-06): ПОЧЕМУ вызов не выполнен — две разные причины, и
	// клиенту нельзя называть не ту.
	//
	// Живой артефакт (RAW-20261106-153057): модель выдала нативный вызов
	// call:editor{…}, парсер его РАЗОБРАЛ, но в запросе было объявлено 5
	// инструментов без editor — R428 правильно отбросил вызов. Текст же
	// говорил «нераспознанный формат», и оператор шёл чинить парсер.
	// Здесь считается именно случай «распознали, но не объявлен».
	undeclaredToolCallNames := undeclaredCallNamesR468(outputForParse, tools)
	honestNotice := unparsedToolCallNoticeR430
	if len(undeclaredToolCallNames) > 0 {
		honestNotice = undeclaredCallNoticeR468(undeclaredToolCallNames, tools)
	}
	if len(toolCalls) == 0 && looksLikeToolCallMarkup(outputForParse) {
		visibleAfterStrip := strings.TrimSpace(stripToolCallMarkup(cleanFinalContent(outputForParse)))
		if visibleAfterStrip == "" {
			logger.Get().Warnw(i18n.T("cppworker.handlers_chat.tools_markup_unparsed"),
				"model", modelName,
				"raw_len", len(fullOutput),
				"emitted_len", emittedLen,
				"preview", truncateForLog(outputForParse, 300))
			// R427: СЫРОЙ вывод сохраняем ПЕРЕД принятием решения.
			// R468: reason называет настоящую причину — «вызов распознан, но
			// инструмент не объявлен» отдельно от «разметку не распознали».
			unparsedReason := "markup_unparsed"
			if len(undeclaredToolCallNames) > 0 {
				unparsedReason = "call_undeclared"
			}
			recordUnparsedCaptureFieldsR468(modelName, "/api/chat", unparsedReason,
				fullOutput, outputForParse, emittedLen, generatedTokens, params.NPredict, tools,
				undeclaredToolCallNames)
			if len(undeclaredToolCallNames) > 0 {
				logger.Get().Warnw("R468: вызов распознан, но инструмент не объявлен клиентом",
					"model", modelName,
					"called", strings.Join(undeclaredToolCallNames, ","),
					"declared_count", len(tools))
			}
			// R428: ХОД БОЛЬШЕ НЕ УБИВАЕТСЯ. Раньше здесь уходил errChunk с
			// done_reason:"error" и пустым content: клиент (Cline) сохранял наше
			// сообщение об ошибке как реплику ассистента, ход терялся, а следующий
			// ответ модели строился уже по испорченной истории (живая сессия:
			// после такого срыва обе модели начали писать файлы текстом).
			// Теперь отдаём честное пояснение как обычный финальный ответ —
			// клиент продолжает сессию, а сырой вывод лежит в диагностике.
			// R320: причина НЕ прошивается "stop" — считаем её по контракту
			// (обрезанный reasoning/бюджет = length), иначе клиент считает ход
			// законченным. Контракт R428: этот чанк — обычный финальный ответ.
			noticeReason := toolsTerminalReasonR394(fullOutput, outputForParse, prompt,
				len(toolCalls), params.NPredict, budgetTokens)
			noticeChunk := map[string]interface{}{
				"model":      modelName,
				"created_at": createdAt,
				"message": map[string]string{
					"role":    "assistant",
					"content": honestNotice,
				},
				"done":        true,
				"done_reason": noticeReason,
			}
			noticeJSON, _ := json.Marshal(noticeChunk)
			sw.Writef("%s\n", noticeJSON)
			sw.Flush()
			return
		} else {
			logger.Get().Warnw(i18n.T("cppworker.handlers_chat.tools_markup_stripped"),
				"model", modelName,
				"visible_len", len(visibleAfterStrip),
				"raw_len", len(fullOutput))
			// R427: разметка снята, но проза доставлена — фиксируем СЫРОЙ вывод:
			// так видно, в какой форме модель прислала вызов и почему он не распознан.
			unparsedReason := "markup_stripped"
			if len(undeclaredToolCallNames) > 0 {
				unparsedReason = "call_undeclared"
			}
			recordUnparsedCaptureFieldsR468(modelName, "/api/chat", unparsedReason,
				fullOutput, outputForParse, emittedLen, generatedTokens, params.NPredict, tools,
				undeclaredToolCallNames)
			if len(undeclaredToolCallNames) > 0 {
				logger.Get().Warnw("R468: вызов распознан, но инструмент не объявлен клиентом",
					"model", modelName,
					"called", strings.Join(undeclaredToolCallNames, ","),
					"declared_count", len(tools))
			}
			// R430: пояснение добавляется к прозе (см. unparsedNoticeR430 выше) —
			// клиент видит, что вызов не выполнен, а проза не теряется.
			// R468: текст зависит от причины — «не объявлен» вместо
			// «нераспознанный формат», когда мы его распознали.
			unparsedNoticeR430 = honestNotice
		}
	}

	// 2026-06-24: ???????? ?? ?????? ????? ????? cleanFinalContent (gemma antiprompt).
	if len(toolCalls) == 0 && reasoningOut == "" && strings.TrimSpace(cleanFinalContent(outputForParse)) == "" {
		// R83/v51 (2026-10-12): не врём клиенту "empty response", если модель
		// реально что-то сгенерировала — просто наша очистка/парсер не смогли
		// распознать вывод (например, незакрытый tool-call блок, который срезал
		// antiprompt, или служебные токены, оставшиеся без полезного текста).
		// Отдаём сырой (но безопасный для клиента) вывод как content, чтобы
		// пользователь видел, что именно ответила модель.
		rawPreview := strings.TrimSpace(sanitizeStreamText(outputForParse))
		meaningful := stripKnownServiceTokens(rawPreview)
		// R113 (2026-10-13): РАЗМЕТКА ВЫЗОВА НЕ ЯВЛЯЕТСЯ ОТВЕТОМ.
		//
		// Раньше здесь сырой вывод отдавался как content — и клиент получал в теле
		// ответа текст вызова инструмента (живая жалоба: «инструмент опять оказался
		// в теле ответа»). Для агентского клиента это худший исход: вызова он не
		// видит (tool_calls пуст), считает сообщение финальным ответом и помечает
		// задачу выполненной после первой итерации (COMPLETE).
		//
		// Правильное поведение: если в выводе есть обёртка вызова, а вызов не
		// распознан — сообщить об ошибке, а не отдавать разметку текстом.
		if looksLikeToolCallMarkup(outputForParse) && stripToolCallMarkup(meaningful) == "" {
			logger.Get().Warnw(i18n.T("cppworker.handlers_chat.tools_markup_unparsed"),
				"model", modelName,
				"raw_len", len(fullOutput),
				"emitted_len", emittedLen,
				"preview", truncateForLog(outputForParse, 300))
			// R427: второй вход в ту же ошибку (путь «пустой видимый вывод»).
			// Сырой вывод сохраняем для диагностики.
			recordUnparsedCapture(modelName, "/api/chat", "empty_after_strip",
				fullOutput, outputForParse, emittedLen, generatedTokens, params.NPredict, tools)
			// R428: ход не убиваем — уходим в обычную ветку ниже, которая отдаст
			// пояснение вместо done_reason:"error" (см. комментарий выше).
			// R320: причина НЕ прошивается "stop" — считаем её по контракту
			// (обрезанный reasoning/бюджет = length), иначе клиент считает ход
			// законченным. Контракт R428: этот чанк — обычный финальный ответ.
			noticeReason := toolsTerminalReasonR394(fullOutput, outputForParse, prompt,
				len(toolCalls), params.NPredict, budgetTokens)
			noticeChunk := map[string]interface{}{
				"model":      modelName,
				"created_at": createdAt,
				"message": map[string]string{
					"role":    "assistant",
					"content": honestNotice,
				},
				"done":        true,
				"done_reason": noticeReason,
			}
			noticeJSON, _ := json.Marshal(noticeChunk)
			sw.Writef("%s\n", noticeJSON)
			sw.Flush()
			return
		}
		// R394.2 (2026-11-04): бюджет ушёл в размышления, а клиент их не видит
		// (think=false) — видимого вывода нет вообще. Раньше такой ход закрывался
		// ошибкой «model produced an empty response» (или пустым stop), и агентский
		// клиент терял ход: модель на самом деле рассуждала и упёрлась в лимит.
		// Отдаём пояснение и честную причину — клиент продолжит ход вместо
		// «пустого ответа». Разметку вызова по-прежнему считаем ошибкой (выше).
		// R417 (2026-11-03): если РАСПОЗНАН tool_calls — это валидный ход, а не
		// «пустой ответ». Пояснение «модель сгенерировала только размышления»
		// здесь не нужно: клиент получит его как текст ответа рядом с вызовом
		// инструмента и сохранит в истории (живой случай: Qwen3.8-9B, вызов
		// read_files + пояснение в content).
		if emittedLen == 0 {
			if notice := toolsTerminalContentReasoningR493("", fullOutput, PromptOpensReasoning(prompt)); notice != "" {
				reason := toolsTerminalReasonR394(fullOutput, outputForParse, prompt,
					len(toolCalls), params.NPredict, budgetTokens)
				logger.Get().Warnw(i18n.T("cppworker.handlers_chat.tools_reasoning_only_notice"),
					"model", modelName, "raw_len", len(fullOutput),
					"reasoning_chars", reasoningCharsRawR394(fullOutput),
					"n_predict", params.NPredict, "generated_tokens", generatedTokens,
					"done_reason", reason)
				noticeChunk := map[string]interface{}{
					"model":       modelName,
					"created_at":  createdAt,
					"message":     map[string]string{"role": "assistant", "content": notice},
					"done":        true,
					"done_reason": reason,
				}
				noticeJSON, _ := json.Marshal(noticeChunk)
				sw.Writef("%s\n", noticeJSON)
				sw.Flush()
				return
			}
		}
		if rawPreview != "" && meaningful != "" {
			// R83/v56: если часть ответа уже ушла дельтами, повторно её не
			// отправляем — иначе клиент, складывающий чанки, увидит текст дважды.
			tail := rawPreview
			if emittedLen > 0 && emittedLen <= len(fullOutput) {
				// R429/R430: и здесь режем по началу разметки вызова — иначе
				// «сырой вывод как content» снова вернул бы тело вызова клиенту.
				tail, _ = visibleTailR429(fullOutput, emittedLen)
			}
			// R113: даже в «сыром» тексте вырезаем разметку вызова — она не ответ.
			tail = stripToolCallMarkup(tail)
			logger.Get().Warnw(i18n.T("cppworker.handlers_chat.tools_unparsed_raw_content"),
				"model", modelName,
				"raw_len", len(fullOutput),
				"clean_len", len(cleanFinalContent(outputForParse)),
				"emitted_len", emittedLen,
				"preview", truncateForLog(tail, 300))
			// R483: «модель выдала разметку, но вызов не распознан» — в кольцо.
			recordWorkerEventR483(r, "/api/chat", "unparsed_tool_markup",
				fmt.Sprintf("raw_len=%d clean_len=%d emitted=%d", len(fullOutput), len(cleanFinalContent(outputForParse)), emittedLen))
			for _, part := range splitTerminalContentR482(tail, 1024) {
				emitContentDelta(part)
			}
			doneChunk := map[string]interface{}{
				"model":      modelName,
				"created_at": createdAt,
				"message":    map[string]string{"role": "assistant", "content": ""},
				"done":       true,
				// R394: причина считается с учётом исчерпанного клиентского лимита
				// (R320 знал только про незакрытый reasoning и давал stop при обрыве).
				"done_reason": toolsTerminalReasonR394(fullOutput, outputForParse, prompt,
					len(toolCalls), params.NPredict, budgetTokens),
			}
			doneJSON, _ := json.Marshal(doneChunk)
			sw.Writef("%s\n", doneJSON)
			sw.Flush()
			return
		}
		// R83/v56: если что-то уже отдано дельтами, «пустым» ответ назвать
		// нельзя — закрываем стрим без ложной ошибки. Причина при этом честная:
		// при исчерпанном бюджете это length, а не stop (R394).
		if emittedLen > 0 {
			reason := toolsTerminalReasonR394(fullOutput, outputForParse, prompt,
				len(toolCalls), params.NPredict, budgetTokens)
			logger.Get().Warnw(i18n.T("cppworker.handlers_chat.tools_empty_after_prefix"),
				"model", modelName, "emitted_len", emittedLen, "raw_len", len(fullOutput),
				"n_predict", params.NPredict, "generated_tokens", generatedTokens,
				"visible_len", len(strings.TrimSpace(cleanFinalContent(outputForParse))),
				"reasoning_chars", reasoningCharsRawR394(fullOutput),
				"done_reason", reason)
			doneChunk := map[string]interface{}{
				"model":       modelName,
				"created_at":  createdAt,
				"message":     map[string]string{"role": "assistant", "content": ""},
				"done":        true,
				"done_reason": reason,
			}
			doneJSON, _ := json.Marshal(doneChunk)
			sw.Writef("%s\n", doneJSON)
			sw.Flush()
			return
		}
		// R83/v51: сырой вывод тоже пуст или состоит только из служебных
		// токенов — только тогда честно сообщаем об ошибке.
		logger.Get().Warnw(i18n.T("cppworker.handlers_chat.tools_empty_inference"),
			"model", modelName,
			"raw_len", len(fullOutput))
		errChunk := map[string]interface{}{
			"model":       modelName,
			"created_at":  createdAt,
			"message":     map[string]string{"role": "assistant", "content": ""},
			"done":        true,
			"done_reason": "error",
			"error":       "model produced an empty response (inference succeeded but output is empty). This may indicate n_ctx too small, prompt too long, or model issue.",
		}
		errJSON, _ := json.Marshal(errChunk)
		sw.Writef("%s\n", errJSON)
		sw.Flush()
		return
	}

	// R65d (2026-09-20): реальные token counts вместо eval_count:0
	// (та же причина, что и в writeChatStreamResponse — OpenWebUI считает
	// tokens/sec из этих полей и показывал N/A).
	//
	// R394.2: не считаем второй раз — generatedTokens (выше) это ровно то же
	// число токенов сырого вывода, посчитанное тем же токенизатором.
	fullOutputCount := generatedTokens
	promptTokensTools := countTokensSafe(modelName, prompt)

	// R66b (2026-09-22): отделяем prefill (TTFT) от генерации. До фикса
	// eval_duration == total_duration включал prefill, а prompt_eval_duration
	// отсутствовал вовсе — tokens/sec у клиентов считался неверно.
	totalNs := duration.Nanoseconds()
	var promptEvalNsTools int64
	if ts := atomic.LoadInt64(&firstTokenAt); ts > 0 {
		if ttft := ts - start.UnixNano(); ttft >= 0 && ttft <= totalNs {
			promptEvalNsTools = ttft
		}
	}

	// R248 (2026-10-27): доехать хвост удержания.
	//
	// Живой симптом у Cline: текст ответа заканчивается посреди фразы, а следом
	// сразу идёт блок завершения — «ответ оборван». Причина: в tools-стриме
	// последние байты держатся в окне toolsStreamHoldback (чтобы не отдать
	// половину маркера вызова), и если ход закончился, пока они в удержании,
	// они НЕ отправлялись клиенту: финальный чанк закрывал стрим, а видимый
	// текст оставался обрезанным. Флашим остаток прозы (разметку вызова
	// вырезаем — она не часть ответа).
	if len(toolCalls) > 0 && emittedLen > 0 && emittedLen < len(fullOutput) {
		// R429 (2026-11-04): добор берёт ТОЛЬКО прозу ДО разметки вызова.
		//
		// Живой дефект (gemma-4 + Cline, сборка R428): вызов распознан
		// (done_reason=tool_calls, editor), но клиент получил в content всё тело
		// вызова — HTML файла целиком, 3254 байта — ОДНОЙ дельтой отсюда.
		// Причина: R248 брал `fullOutput[emittedLen:]` целиком и «чистил»
		// stripToolCallMarkup, а тот снимает только теговые обёртки и
		// ЗАВЕРШЁННЫЕ JSON-объекты; незавершённый JSON вызова (с экранированными
		// кавычками внутри new_text) оставался как есть вместе с `<channel|>`.
		// Теперь хвост режется по началу разметки: тело вызова в content не
		// попадает никогда, а проза перед ним — попадает.
		tail, callStart := visibleTailR429(fullOutput, emittedLen)
		if tail != "" {
			for _, part := range splitTerminalContentR482(tail, 1024) {
				emitContentDelta(part)
			}
			logger.Get().Infow("writeChatStreamResponseWithTools: flushed held tail before done",
				"model", modelName, "tail_len", len(tail), "emitted_len", emittedLen,
				"call_start", callStart, "raw_len", len(fullOutput))
		}
		// Позиция разбора встаёт на начало разметки вызова: всё до неё уже
		// отдано, всё после — тело вызова, его место в message.tool_calls.
		emittedLen = callStart
	}

	finalChunk := map[string]interface{}{
		"model":                modelName,
		"created_at":           createdAt,
		"message":              map[string]interface{}{"role": "assistant"},
		"done":                 true,
		"total_duration":       totalNs,
		"load_duration":        loadDurationNsR493(r.Context()),
		"prompt_eval_count":    promptTokensTools,
		"prompt_eval_duration": promptEvalNsTools,
		"eval_count":           fullOutputCount,
		"eval_duration":        totalNs - promptEvalNsTools,
	}

	if len(toolCalls) > 0 {
		// R440: что именно уходит клиенту (включается CPPWORKER_DEBUG_TOOL_ARGS=1).
		logOutgoingToolCalls(modelName, "/api/chat", toolCalls, fullOutput)
		// Tool calls обнаружены — эмитим message.tool_calls и очищаем content.
		// Это критично: OpenWebUI при `done_reason: "tool_calls"` ожидает
		// что content пустой, а tool_calls содержит вызовы функций.
		msg := finalChunk["message"].(map[string]interface{})
		msg["content"] = ""
		// R66b (2026-09-22): нативный Ollama-формат — arguments это JSON-ОБЪЕКТ,
		// а не строка (OpenAI-форма). Cline CLI (провайдер "ollama")/ollama-python
		// читают именно объект; со строкой клиент повторно сериализовал аргументы
		// и tool call приезжал «пустым» → "Model returned empty response".
		msg["tool_calls"] = ollamaToolCallsFromOpenAI(toolCalls)
		// R106c: размышления — отдельным полем (Ollama: message.thinking), а не
		// в content: клиент показывает их как «thinking», а не как ответ.
		// R108: отдаём оба имени (`thinking` — канон §4.1, `reasoning` — legacy).
		// R404: если thinking уже ушёл дельтами — в финале НЕ дублируем его.
		// R470: эхо размышлений — только если клиент их попросил (см. reasoning_echo.go).
		if reasoningEcho && reasoningOut != "" && !thinkingStreamed {
			msg["thinking"] = reasoningOut
			msg["reasoning"] = reasoningOut
		}
		finalChunk["done_reason"] = "tool_calls"
		logger.Get().Infow("writeChatStreamResponseWithTools: detected tool_calls in streamed output",
			"model", modelName, "tool_calls_count", len(toolCalls))
	} else {
		// Нет tool calls — обычный текстовый ответ. КРИТИЧНО: возвращаем
		// полный output в финальном чанке, иначе OpenWebUI видит пустой
		// content и завершает сессию ("один источник найден, ответа нет").
		//
		// Раньше здесь был bug: финальный чанк имел content: "", хотя
		// текст уже был отправлен в streaming chunks. OpenWebUI после
		// done:true берёт content из финального чанка (не агрегирует
		// из streaming chunks), поэтому видел пустой ответ.
		//
		// Делаем минимальную очистку: удаляем хвостовые служебные токены,
		// которые модель могла эмитить (</tool_call>, [TOOL_CALLS], и т.п.).
		msg := finalChunk["message"].(map[string]interface{})
		// R113: разметка вызова не является ответом — вырезаем её из видимого
		// текста, даже если рядом с ней есть проза (иначе клиент получает JSON
		// вызова в теле ответа; живой прогон: 2456 символов такого текста).
		content := stripToolCallMarkup(cleanFinalContent(outputForParse))
		// R66b: format:"json" — снимаем markdown-обёртку ```json ... ```.
		// Отдельного поля в ответе не добавляем: схема Ollama /api/chat его не
		// содержит, а лишние поля ломают строгих клиентов (см. историю с
		// unknown field в R65d).
		if jsonMode {
			content = stripJSONFence(content)
		}
		// R394.1 (2026-11-04): пояснение вместо пустого content — ОДНО место на
		// все ветки ниже. R320 ставил его только при `reasoningOut != ""`, и при
		// явном think=false (размышления клиенту не отдаются, reasoningOut пуст)
		// клиент получал content:"" + done_reason:stop — то же молчание, от
		// которого R314 защищает остальные поверхности. Признак «размышления
		// были» берём из СЫРОГО вывода (reasoningCharsRawR394), до политики.
		// R417: при распознанном tool_calls content обязан остаться пустым.
		// Пояснение «только размышления» — для случая «клиенту нечего показать»;
		// рядом с вызовом инструмента оно превращается в текст ответа и попадает
		// в историю клиента (живой случай: Qwen3.8-9B + read_files).
		content = terminalContentR417(content, fullOutput, len(toolCalls))
		if strings.TrimSpace(content) == "" && len(toolCalls) == 0 {
			content = toolsTerminalContentReasoningR493(content, fullOutput, PromptOpensReasoning(prompt))
		}
		// R430: разметку вызова не распознал парсер, но она ЕСТЬ — видимый текст
		// собираем до её начала + пояснение (см. unparsedNoticeR430 выше). Один раз,
		// ДО всех веток отдачи: и content финального чанка, и единственная дельта
		// короткого ответа (R424) идут из этого значения — без этого тело вызова
		// уезжало клиенту именно этим каналом (живая проба).
		if unparsedNoticeR430 != "" {
			content = visibleContentBeforeMarkupR430(outputForParse)
		}

		// R474 (2026-11-07): АРГУМЕНТЫ ВЫЗОВА, ВЫДАННЫЕ ТЕКСТОМ.
		//
		// ЖИВОЙ ДЕФЕКТ (Cline + Qwen3.8-9B, /api/chat): ход закончился
		// final_content_len=7095, tool_calls=0 — клиент получил ~7 КБ кода как
		// ответ, а в конце шли сериализованные аргументы
		// («…"path":"d:\cline-e2e-web\public\app.js"}]}}»). Файл не создан, Cline
		// следом сообщил «text part … not found» (его реакция на такой ответ).
		// Ни обёрток вызова, ни тела файла в начале текста здесь нет, поэтому
		// прежние детекторы (R430, R457) молчали. Разбор — text_arguments_dump.go.
		// Условие len(tools) > 0 — как у R457: если клиент инструментов не
		// объявлял, «код + JSON в конце» может быть обычным ответом на просьбу
		// «напиши файл», и подменять его пояснением нельзя.
		if len(tools) > 0 && len(toolCalls) == 0 && unparsedNoticeR430 == "" {
			if hint, toolName, dumped := looksLikeDumpedArgumentsR474(outputForParse); dumped {
				logger.Get().Warnw("R474: аргументы вызова выданы текстом — вызов не выполнен",
					"model", modelName, "tool", toolName, "hint", hint,
					"raw_len", len(fullOutput), "visible_len", len(content))
				recordUnparsedCapture(modelName, "/api/chat", "arguments_as_text",
					fullOutput, outputForParse, emittedLen, generatedTokens, params.NPredict, tools)
				content = dumpedArgumentsNoticeR474(toolName, hint)
			}
		}

		// R404: thinking уже мог уйти дельтами — тогда в финале его НЕ дублируем.
		thinkingField := reasoningOut
		if thinkingStreamed || !reasoningEcho {
			thinkingField = ""
		}
		if reasoningOut != "" {
			// R106c: размышления отделены — их место в message.thinking, а в
			// content только ответ.
			// R108: `thinking` — канон (контракт §4.1), `reasoning` — legacy-алиас.
			// R404: если thinking шёл дельтами, оставляем только content; при
			// этом content либо уже стримился (тогда в финал кладём пустое и
			// отдаём хвост), либо его не было — кладём целиком с пояснением.
			if thinkingField != "" {
				msg["thinking"] = thinkingField
				msg["reasoning"] = thinkingField
			}
			if streamContent && emittedLen > 0 && emittedLen <= len(fullOutput) {
				// R429/R430: хвост режется по началу разметки вызова — тело
				// вызова в content не попадает никогда.
				tail, _ := visibleTailR429(fullOutput, emittedLen)
				if unparsedNoticeR430 != "" {
					if tail == "" {
						tail = unparsedNoticeR430
					} else {
						tail = tail + "\n\n" + unparsedNoticeR430
					}
				}
				if jsonMode {
					tail = stripJSONFence(tail)
				}
				tail = stripToolCallMarkup(tail)
				if strings.TrimSpace(tail) == "" {
					// Резать нечего: остаток был только разметкой вызова. Тогда
					// видимый ответ обязан прийти в content финального чанка —
					// иначе клиент не получит ничего (R424: «text part not found»).
					msg["content"] = content
				} else {
					for _, part := range splitTerminalContentR482(tail, 1024) {
						emitContentDelta(part)
					}
					msg["content"] = ""
				}
			} else {
				msg["content"] = content
			}
			logger.Get().Debugw(i18n.T("cppworker.handlers_chat.tools_reasoning_separate_final_chunk"),
				"model", modelName, "reasoning_len", len(reasoningOut), "reasoning_echo", reasoningEcho,
				"thinking_streamed", thinkingStreamed, "content_len", len(content),
				"emitted_len", emittedLen)
		} else if streamContent && emittedLen > 0 {
			// R83/v56: префикс ответа уже ушёл дельтами. В финальный чанк кладём
			// только остаток (клиент, складывающий чанки, не должен получить
			// текст дважды), а сам остаток отдаём отдельной дельтой.
			//
			// Если маркер tool call всё-таки встречался (toolsHold), остаток
			// чистим от служебных токенов; иначе отдаём как есть — иначе
			// cleanFinalContent срезал бы значимый хвостовой пробел/перевод строки.
			tail := content
			if emittedLen <= len(fullOutput) {
				// R429/R430: хвост режется по началу разметки вызова. До этого
				// здесь отдавался ВЕСЬ остаток буфера: живой прогон gemma-4 + Cline
				// (сборка R429) отдал клиенту 3441 из 3442 байт — тело вызова
				// (HTML файла целиком) уехало как «ответ» и осело в истории
				// Cline. Чистки на месте (cleanFinalContent / stripToolCallMarkup)
				// разметку не знают: она была переэкранирована (`\"name\"`) и без
				// баланса скобок, поэтому единственный надёжный признак — её
				// НАЧАЛО, по нему и режем.
				visibleTail, _ := visibleTailR429(fullOutput, emittedLen)
				tail = visibleTail
				if jsonMode {
					tail = stripJSONFence(tail)
				}
				// R113: хвост тоже чистим от разметки вызова.
				tail = stripToolCallMarkup(tail)
			}
			if strings.TrimSpace(tail) == "" {
				// Остаток был только разметкой вызова — видимый текст отдаём в
				// финальном чанке, иначе клиент не получит ничего (R424).
				msg["content"] = content
			} else {
				for _, part := range splitTerminalContentR482(tail, 1024) {
					emitContentDelta(part)
				}
				msg["content"] = ""
			}
			logger.Get().Debugw(i18n.T("cppworker.handlers_chat.tools_text_response_streamed"),
				"model", modelName, "emitted_len", emittedLen, "tail_len", len(tail),
				"tools_hold", toolsHold, "json_mode", jsonMode)
		} else {
			// R424 (2026-11-03): КОРОТКИЙ ОТВЕТ ЦЕЛИКОМ В ОКНЕ УДЕРЖАНИЯ.
			//
			// ЖИВОЙ ДЕФЕКТ (воспроизведён прогоном Cline CLI 3.0.74 через
			// балансер, сессия conv_1793730119921_sd9evze). Tools-путь держит
			// последние toolsStreamHoldback=32 байта вывода, чтобы не отдать
			// клиенту половину маркера вызова. Если весь видимый ответ короче
			// окна (у нас — "FINAL: needle-7f3a", 18 байт), за ход не уходит
			// НИ ОДНОЙ дельты: cppworker писал streamed_content_bytes=0,
			// final_content_len=18. Дальше балансер по умолчанию вычищает
			// content из done-чанка (LB_OLLAMA_DONE_CONTENT=strip), и клиент,
			// читающий ответ дельтами, не получает ничего: Cline CLI падал с
			// «text part <id> not found» — ровно тот симптом, на который
			// жаловался оператор.
			//
			// R248 закрыл ту же дырку для случая «есть tool_calls»; здесь — для
			// обычного короткого ответа. Порядок как в соседних ветках: сначала
			// дельта, затем финальный чанк БЕЗ дубля content (клиент, читающий
			// done, тоже получает текст — но ровно один раз).
			if content != "" && streamContent {
				emitContentDelta(content)
				msg["content"] = ""
			} else {
				msg["content"] = content
			}
			logger.Get().Debugw("writeChatStreamResponseWithTools: text response (no tool_calls)",
				"model", modelName, "content_len", len(content), "json_mode", jsonMode,
				"stream_content", streamContent, "emitted_len", emittedLen)
		}
		// R394 (2026-11-04): причина остановки — с учётом исчерпанного
		// клиентского лимита. R320 ставил "length" только при незакрытом блоке
		// размышлений; если блок закрыт, а видимый ответ оборван на n_predict,
		// он давал "stop" — по контракту Ollama это «модель закончила», и Cline
		// закрывал ход, не получив продолжения («флаг завершения раньше ответа»).
		finalChunk["done_reason"] = toolsTerminalReasonR394(fullOutput, outputForParse, prompt,
			len(toolCalls), params.NPredict, budgetTokens)
	}

	// R113 (2026-10-13): ЯВНОЕ ОТСЛЕЖИВАНИЕ ОТВЕТА — сводка хода одной строкой и
	// финальный кадр вынесены в logToolsStreamSummary (R527): у вызывающей
	// функции осталась только проводка, а поля сводки по-прежнему печатаются
	// там, где значения уже посчитаны.
	logToolsStreamSummary(run, sw, r, start, duration, fullOutput, finalChunk, toolCalls, tools,
		budgetTokens, generatedTokens, reasoningOut, toolCallsFromReasoning)
}

// chatStreamFinish — состояние финальной части /api/chat-стрима (R526).
//
// Вынесено из writeChatStreamResponse: ~230 строк финализации перестали быть
// частью 519-строчной функции. Поведение не менялось: тело перенесено дословно,
// изменён только способ доступа к состоянию (поля вместо локальных переменных).
type chatStreamFinish struct {
	// Порядок полей — под fieldalignment (R527: «368 pointer bytes could be 320»):
	// сначала указатели/интерфейсы, затем строки, затем значения и флаги. Порядок
	// ни на что не влияет: конструктор использует именованные поля.
	start                time.Time
	prefillStartTime     time.Time
	ctx                  context.Context
	firstTokenAt         *int64
	thinkOverride        *bool
	streamErr            *error
	contentSeen          *strings.Builder
	callback             func(string) bool
	sw                   *safeStreamWriter
	r                    *http.Request
	emittedContentRawLen *int
	outputBuf            *strings.Builder
	rcParser             *ReasoningStreamState
	modelName            string
	createdAt            string
	prompt               string
	params               bridge.GenerationParams
	rcSendReasoning      bool
}
