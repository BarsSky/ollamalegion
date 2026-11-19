// llamacpp_stream_finalize.go — R630 (2026-11-19): хвост стрима — придержанный
// текст, авто-продолжение обрыва, гарантированный терминатор, диагностика.
//
// ЗАЧЕМ ВЫНЕСЕНО. Хвост — это отдельный класс дефектов («клиент не увидел done»,
// «ответ обрезан и никто не сказал почему»), и он проверяется отдельными тестами.
// Рядом с переводом кадров его правки каждый раз требовали читать чужой код.
//
// ПОВЕДЕНИЕ НЕ МЕНЯЛОСЬ: код перенесён как есть.
package balancer

import (
	"context"
	"fmt"
	"net/http"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"sync/atomic"
	"time"
)

// finalizeStream — хвост стрима после цикла: придержанный текст (R266.1/R279),
// авто-продолжение обрыва (R60.21/R187), гарантированный терминатор (R231),
// диагностика обрыва (R296/R298/R384), лог ошибки сканера, сброс счётчика
// n_ctx и пустой done-кадр для Ollama-путей без единого кадра.
func (st *llamaCppStreamState) finalizeStream() error {
	st.flushHeldTail()
	if err := st.maybeAutoContinue(); err != nil {
		return err
	}

	// R231 (2026-10-24): ЗАВЕРШАЮЩИЙ ЧАНК ОБЯЗАН БЫТЬ ОТДАН ВСЕГДА.
	//
	// Живой дефект: emitDeferredDone вызывался ТОЛЬКО внутри ветки
	// `if streamCompleted && IsAutoContinueOnTruncationEnabled() && content != ""`.
	// При выключенном авто-продолжении (дефолт — env не задан) маркер `[DONE]`
	// от cppworker НЕ пересылался клиенту и синтетический done не эмитился:
	// поток завершался молча. Cline/OpenWebUI, не увидев конца стрима, ждали
	// или рвали сессию («символ конца стрима затесался/пропал»). Теперь
	// завершение эмитится безусловно (идемпотентно, ровно один раз).
	if st.streamCompleted && st.deferredDoneChunk {
		st.emitTerminalDoneOnly()
	}

	if err := st.reportPrematureEnd(); err != nil {
		return err
	}

	if err := st.scanner.Err(); err != nil {
		logger.Get().Errorw("proxyRequestLlamaCpp: scanner error", "error", err, "backend", st.backendID)
	}

	if st.p.nctxReload != nil {
		st.p.nctxReload.ResetCycleCounter(st.backendID)
	}

	if st.lastData == "" && st.originalPath != "/v1/chat/completions" && !st.terminalAlreadySent {
		done := buildDoneResponse(st.originalPath, *st.modelFromCtx, 0)
		_, errFwd := fmt.Fprintf(st.w, "%s\n", string(done))
		if errFwd != nil {
			return fmt.Errorf("write empty-done NDJSON: %v", errFwd)
		}
		if flusher, ok := st.w.(http.Flusher); ok {
			flusher.Flush()
		}
	}

	return nil
}

// flushHeldTail — R266.1/R279: придержанный хвост служебных меток, который
// оказался обычным текстом, обязан уйти клиенту, а не в накопитель.
//
// R266.1 (2026-10-28): отдаём придержанный хвост, если он оказался обычным
// текстом. Без этого ответ, закончившийся на `<` (начало метки, которая так и
// не пришла), терял бы последние байты — тот же класс дефекта, что чинили в
// R261 на другом пути.
//
// R279 (2026-10-28): ЭТОТ ХВОСТ ОБЯЗАН УЙТИ КЛИЕНТУ, А НЕ В НАКОПИТЕЛЬ.
//
// ЖИВОЙ ДЕФЕКТ: здесь стояло `accumulatedPlainContent += held` — то есть
// придержанные байты попадали в статистику и в детектор автопродолжения, а в
// поток НЕ уходили вовсе. Если хвостом оказывался обычный текст финального
// блока (а удержание держит любой суффикс, похожий на начало метки, включая
// просто «<» или «[» из кода), клиент этот текст не получал никогда.
// Теперь хвост пишется отдельным NDJSON-чанком (done:false) до терминального
// чанка: порядок «content → done» сохраняется, второй done клиент не получает.
func (st *llamaCppStreamState) flushHeldTail() {
	if st.originalPath != "/api/chat" && st.originalPath != "/api/generate" {
		return
	}
	held := st.ndjsonHold.Flush()
	if held == "" {
		return
	}
	if tail := heldTailChunk(st.originalPath, *st.modelFromCtx, held); tail != nil {
		n, errTail := fmt.Fprintf(st.w, "%s\n", tail)
		st.bytesForwarded.Add(int64(n))
		if errTail != nil {
			if st.firstForwardErr == nil {
				st.firstForwardErr = errTail
			}
			st.lastForwardErr = errTail
		} else {
			if flusher, ok := st.w.(http.Flusher); ok {
				flusher.Flush()
			}
			logger.Get().Infow("proxyRequestLlamaCpp[native]: emitted held tail to client (R279)",
				"backend", st.backendID, "path", st.originalPath, "held_len", len(held))
		}
	}
	// В накопитель тоже добавляем: он отражает то, что клиент уже видел,
	// и по нему работают детектор обрыва и автопродолжение.
	st.accumulatedPlainContent += held
}

// emitDeferredDone — отдать отложенный done-чанк (обычный успешный конец).
// R231: вызывается из нескольких веток (агентский ход, авто-продолжение,
// happy-path), поэтому идемпотентен: клиент обязан получить РОВНО ОДИН
// завершающий чанк/[DONE].
func (st *llamaCppStreamState) emitDeferredDone() {
	if st.doneEmitted {
		return
	}
	st.doneEmitted = true
	errFwd := writeStreamingSSEDone(st.w, st.originalPath, *st.modelFromCtx, st.toolAccum,
		st.accumulatedPlainContent, st.upstreamDoneContent, st.upstreamHadFinishReason, st.usageChunkSeen)
	if errFwd != nil {
		logger.Get().Warnw("proxyRequestLlamaCpp: deferred writeStreamingSSEDone error",
			"backend", st.backendID, "error", errFwd)
		st.lastForwardErr = errFwd
		if st.firstForwardErr == nil {
			st.firstForwardErr = errFwd
		}
	}
	if st.originalPath != "/v1/chat/completions" {
		Flush(st.w)
	}
}

// emitTerminalDoneOnly — R231: завершающий чанк БЕЗ повторного content.
//
// Нужен там, где дельты уже отданы клиенту, а отложенный done не был эмитчен
// (например, авто-продолжение подавлено как перегенерация). Повтор текста в
// done-чанке — это дубль у клиентов, склеивающих все чанки (живой флейк:
// исходный ответ приходил дважды). Настоящая Ollama в done-чанке отдаёт
// message.content="" — повторяем её контракт; tool_calls сохраняем.
func (st *llamaCppStreamState) emitTerminalDoneOnly() {
	if st.doneEmitted {
		return
	}
	// R51.3/R53.1: на NDJSON-путях usage-чанк уже отдал canonical done:true
	// (translateUsageChunkToOllama) — второй done клиент получит как дубль.
	if st.usageChunkSeen && st.originalPath != "/v1/chat/completions" {
		st.doneEmitted = true
		return
	}
	st.doneEmitted = true
	// R377 (2026-11-01): кадр собирает единственный владелец. Пустой content —
	// осознанное решение этого места: дельты клиенту уже отданы, повтор текста
	// в терминальном кадре даёт дубль у клиентов, склеивающих чанки.
	//
	// ДО R377 кадр собирался здесь руками и расходился с хелпером в трёх
	// местах: tool_calls уходили строкой аргументов (нарушение R295 — Ollama
	// ждёт объект), /api/generate получал поле message вместо response,
	// done_reason ставился "stop" даже без отданного текста.
	if errFwd := writeTerminalFrameR377(st.w, terminalFrameR377{
		path:      st.originalPath,
		model:     *st.modelFromCtx,
		toolCalls: st.toolAccum,
	}); errFwd != nil {
		logger.Get().Warnw("proxyRequestLlamaCpp: terminal frame write error",
			"backend", st.backendID, "path", st.originalPath, "error", errFwd)
		st.lastForwardErr = errFwd
		if st.firstForwardErr == nil {
			st.firstForwardErr = errFwd
		}
	}
	Flush(st.w)
}

// maybeAutoContinue — R60.21: доработка обрыва. Только там, где терминатор
// УДЕРЖАН (R322): после уже отданного клиенту завершения писать в поток нельзя.
//
// ==== R60.21 Auto-Continue on Truncation (2026-09-08) ====
// After the main streaming loop ends cleanly (streamCompleted=true),
// check if the accumulated content looks truncated. If so AND
// LB_AUTO_CONTINUE_ON_TRUNCATION=1, send a "continue" request to
// upstream and emit the continuation to client.
//
// Default: OFF (opt-in). See docs/api/env-vars.ru.md.
// For Qwen3-Instruct-2507-q4km which has a tendency to emit
// ChatML-EOS <|im_end|> mid-code (R60.20), this auto-recovers
// from unclosed code blocks and mid-line cutoffs.
//
// R60.34 (2026-09-10): guard against spurious fire on empty / failed
// responses. Previous behavior:
//   - TruncateReason("") → "empty" → fire R60.21
//   - empty content (e.g. n_ctx overflow, model error) was treated
//     as "truncated, continue!" → garbage request to upstream
//   - R60.33 async-load 503: handleChat returned 503, but
//     proxyRequestLlamaCpp also fired R60.21 in background → cascade
//     of unload+reload cycles
//
// Fix: skip R60.21 when:
//  1. accumulatedPlainContent is empty
//  2. streamCompleted but no chunks were received
//  3. upstream errored (any non-empty err in errMsg)
//  4. n_predict/finish_reason is missing or "length" was not the cause
func (st *llamaCppStreamState) maybeAutoContinue() error {
	// R187 (2026-10-20): АГЕНТСКИЙ ХОД НЕ ПРОДОЛЖЕН БЫТЬ ПРОДЛЁН.
	//
	// ЖИВОЙ РАЗБОР (Cline + Qwen3.8). В клиенте видно так: модель «завершает»
	// ответ (клиент показывает completed), затем появляются размышления, затем
	// поток встаёт и сессия не закрывается. Причина — здесь: эвристика «ответ
	// выглядит усечённым» (незакрытый блок кода, обрыв строки) срабатывала на
	// НОРМАЛЬНОМ конце агентского хода, и балансер:
	//   1) уже отдал клиенту finish_reason (клиент показал completed);
	//   2) тянул ВТОРОЙ запрос генерации (на 27B-модели — десятки секунд);
	//   3) дописывал продолжение ПОСЛЕ finish (клиенты его игнорируют) либо
	//      подавлял его по политике smart — и [DONE] приходил с большой
	//      задержкой или не приходил вовсе.
	// Агентский ход завершается ТЕМ, ЧЕМ ЗАВЕРШИЛСЯ: текстом, вызовом
	// инструмента или лимитом, который поставил сам клиент. Продолжать за
	// клиента нельзя: он ведёт диалог сам, а ходы модели в агентской сессии
	// почти всегда кончаются «незакрытой» строкой (это вызов инструмента, а не
	// обрыв) — то есть ложное срабатывание здесь штатное, а не исключение.
	agentTurn := !AutoContinueAllowedForTurn(countClientTools(st.bodyBuf) > 0, len(st.toolAccum))

	// R322 (2026-10-30, найдено аудитом маршрутов): после уже отданного клиенту
	// терминатора писать в поток НЕЛЬЗЯ.
	//
	// ЖИВОЙ ДЕФЕКТ. На нативном пути терминальный кадр воркера (`done:true`)
	// уходит клиенту как есть (passthrough), а блок авто-продолжения ниже всё
	// равно дорабатывал ответ: `EmitContinuationNDJSON` печатает ещё один
	// content-кадр и СВОЙ `done:true` — то есть после завершения хода в потоке
	// появлялся второй терминатор. Клиенты (Cline, OpenWebUI) такой поток
	// считают битым: часть закрывает ход на первом `done`, часть — на втором, и
	// «ответ не завершён» выглядит как незавершённая задача (жалоба про
	// COMPLETE), а часть сообщений теряется.
	//
	// Авто-продолжение имеет смысл только там, где терминальный кадр УДЕРЖАН
	// (deferredDoneChunk): тогда клиент ещё не получил завершения и продолжение
	// чинит обрыв. Если терминатор уже ушёл — доработка запрещена.
	if !(st.streamCompleted && IsAutoContinueOnTruncationEnabled() && st.accumulatedPlainContent != "" && !st.priorDoneEmitted) {
		return nil
	}
	finalContent := st.accumulatedPlainContent
	if st.upstreamDoneContent != "" {
		// If cppworker sent full content in the done chunk (not chunked),
		// use that as the source of truth.
		finalContent = st.upstreamDoneContent
	}
	if agentTurn {
		// Агентский ход: продолжаем только если клиент НЕ ограничил ответ и
		// модель не вызвала инструмент. Здесь ничего не доделываем.
		recordAutoContinueSuppressed()
		logger.Get().Infow(i18n.T("balancer.llamacpp_transport.auto_continue_agent_turn_skipped_info"),
			"backend", st.backendID, "model", *st.modelFromCtx,
			"api_path", st.originalPath,
			"tools_in_request", countClientTools(st.bodyBuf) > 0,
			"tool_calls_in_response", len(st.toolAccum),
			"content_chars", len(finalContent))
		if st.deferredDoneChunk {
			st.emitDeferredDone()
		}
		return nil
	}
	// R174 (2026-10-18, найдено сценариями клиентов): КЛИЕНТ САМ ОГРАНИЧИЛ
	// ОТВЕТ — продолжать нельзя.
	//
	// Живой замер матрицы клиентов: `num_predict: 6` на стриминговом
	// Ollama-пути → балансер считал обрыв «усечением» и слал второй запрос, воркер
	// видел промпт на 45 токенов больше прямого вызова, клиент получал ответ
	// длиннее запрошенного и платил за двойной промпт. Агентные клиенты (Cline,
	// deepseek harness, hermes) ставят max_tokens на КАЖДЫЙ запрос.
	if capped, capField := ClientOutputCapped(st.bodyBuf); capped {
		recordAutoContinueSuppressed()
		logger.Get().Infow(i18n.T("balancer.llamacpp_transport.auto_continue_client_capped_info"),
			"backend", st.backendID, "model", *st.modelFromCtx,
			"api_path", st.originalPath, "cap_field", capField,
			"content_chars", len(finalContent))
		return nil
	}
	if blockedCtx, ctxErrText := autoContinueBlockedByContext(st.upstreamErrorText, st.upstreamDoneContent, finalContent); blockedCtx {
		// R289 (2026-10-29): это НЕ обрыв, а отказ по размеру контекста.
		// Живой дефект: при окне 4096 и промпте 3791 апстрим отвечал 413
		// `prompt too long`, эвристика видела «незакрытую строку» и
		// запускала продолжение — второй запрос тоже получал 413, а клиент
		// так и не узнал настоящую причину.
		recordAutoContinueSuppressed()
		windowTokens := st.p.loadedPerSeqNCtx(st.backendID, *st.modelFromCtx)
		writeContextTooSmallNotice(st.w, st.originalPath, *st.modelFromCtx,
			st.p.calibratedEstimateOnly(st.bodyBuf, st.originalPath), windowTokens)
		logger.Get().Infow("auto-continue suppressed: context overflow is not a truncation (R289)",
			"backend", st.backendID, "model", *st.modelFromCtx,
			"api_path", st.originalPath, "upstream_error", ctxErrText)
		if st.deferredDoneChunk {
			st.emitTerminalDoneOnly()
		}
		return nil
	}
	if reason := TruncateReason(finalContent); reason != "" {
		return st.continueTruncatedResponse(finalContent, reason)
	}
	if st.deferredDoneChunk && st.streamCompleted {
		// R60.55 (2026-09-13): no truncation OR auto-continue didn't fire
		// (e.g., continuation was suppressed as regen). Emit the deferred
		// done-chunk now with accumulated content. This is the standard
		// happy-path case where the model completed normally.
		st.emitDeferredDone()
	}
	return nil
}

// continueTruncatedResponse — R60.21: запросить продолжение у воркера и отдать
// клиенту ОДИН связный ответ (без двух done-кадров).
func (st *llamaCppStreamState) continueTruncatedResponse(finalContent, reason string) error {
	logger.Get().Infow("proxyRequestLlamaCpp: R60.21 detected truncation, auto-continuing",
		"backend", st.backendID, "model", *st.modelFromCtx,
		"api_path", st.originalPath,
		"reason", reason,
		"content_chars", len(finalContent))
	// R60.25: use env-configured timeout (default 10 min, was
	// hardcoded 60s which timed out on long code generation).
	// The 60s default was the failure mode for "model emits
	// unclosed code block, auto-continue triggers, but the
	// continue request itself hits the same time limit and
	// times out before the model finishes the continuation".
	autoContTimeout := GetAutoContinueTimeout()
	continuation, contEval, contErr := PerformAutoContinue(
		st.fullURL, st.translatedBody, finalContent, reason,
		&http.Client{Timeout: autoContTimeout}, autoContTimeout)
	if contErr != nil || continuation == "" {
		recordAutoContinueFailed()
		logger.Get().Warnw("proxyRequestLlamaCpp: R60.21 auto-continue failed (emitting truncated as-is)",
			"backend", st.backendID, "model", *st.modelFromCtx,
			"reason", reason, "error", contErr,
			"url", st.fullURL)
		return nil
	}
	// R83 (2026-09-25, D3): политика != smart ОТКЛЮЧАЕТ подавление
	// перегенерации, то есть при `always` дубли ответа гарантированы.
	// Раньше это было видно только по отсутствию строки «suppressing
	// duplicate» в логе — теперь есть явное предупреждение и счётчик,
	// который уходит в /api/v1/metrics (см. auto_continue_stats.go).
	if policy := GetAutoContinueChatPolicy(); policy != "smart" {
		recordAutoContinuePolicyDisabled()
		logger.Get().Warnw(i18n.T("balancer.llamacpp_transport.regeneration_suppression_disabled_warn"),
			"backend", st.backendID, "model", *st.modelFromCtx,
			"api_path", st.originalPath, "policy", policy,
			"hint", i18n.T("balancer.llamacpp_transport.regeneration_suppression_hint"))
	}
	// R60.55 (2026-09-13): if "smart" chat-policy and continuation
	// looks like a regeneration (model restarted with greeting),
	// SUPPRESS the continuation. Sending it would create a duplicate
	// response in OpenWebUI.
	if GetAutoContinueChatPolicy() == "smart" && IsContinuationARegeneration(finalContent, continuation) {
		recordAutoContinueSuppressed()
		logger.Get().Warnw("proxyRequestLlamaCpp: R60.55 continuation looks like regeneration (chat model antipattern); suppressing duplicate",
			"backend", st.backendID, "model", *st.modelFromCtx,
			"api_path", st.originalPath,
			"policy", "smart",
			"original_chars", len(finalContent),
			"continuation_chars", len(continuation))
		// Don't emit continuation. Client already received done-chunk
		// with truncated original content. Set flag to skip downstream
		// truncation alert (we did our best).
		st.streamCompleted = true
		return nil
	}
	recordAutoContinueEmitted()
	logger.Get().Infow("proxyRequestLlamaCpp: R60.21 auto-continue succeeded",
		"backend", st.backendID, "model", *st.modelFromCtx,
		"api_path", st.originalPath,
		"continuation_chars", len(continuation),
		"cont_eval", contEval)
	// Emit continuation as a single final done-chunk with
	// combined content. This is the safest approach — the client
	// gets one coherent response (no double done-chunks).
	var flusher http.Flusher
	if f, ok := st.w.(http.Flusher); ok {
		flusher = f
	}
	totalEval := 0
	if contEval > 0 {
		totalEval = contEval
	}
	emitErr := EmitContinuationNDJSON(
		st.w, st.originalPath, *st.modelFromCtx,
		finalContent, continuation, totalEval, flusher)
	if emitErr != nil {
		logger.Get().Warnw("proxyRequestLlamaCpp: R60.21 emit continuation error",
			"backend", st.backendID, "error", emitErr)
	}
	// Mark stream as completed so downstream truncation
	// detection doesn't fire.
	st.streamCompleted = true
	// R231: если continuation реально отдана, терминальный чанк
	// уже ушёл клиенту (combined content + done). Отложенный
	// done больше не нужен — иначе в конце сработает
	// безусловная эмиссия и клиент получит ДУБЛЬ исходного
	// текста (живой флейк TestAutoContinue_Regeneration...).
	if emitErr == nil {
		st.deferredDoneChunk = false
	}
	return nil
}

// reportPrematureEnd — R384: диагностика и кадры-причины, когда поток кончился
// до терминального маркера. Возвращает ошибку только в одном случае — клиент
// ушёл сам (`client_cancel`): писать в его соединение бессмысленно.
//
// ==== Stream-truncation detection (2026-06-26) ====
// Если мы вышли из цикла for без streamCompleted (т.е. cppworker оборвал
// стрим до того, как прислал финальный [DONE] маркер), явно логируем это
// и эмитим клиенту финальный чанк с error/done:true, чтобы клиент
// (Cline/OpenWebUI/Roo Code) корректно отобразил сбой вместо "успешного
// ответа" с обрезанным content.
//
// R384 (2026-11-01): признак «терминатор уже отправлен кадрами-причиной».
//
// ЖИВОЙ ДЕФЕКТ (стенд, сторож тишины): апстрим прислал заголовки и замолчал;
// сторож закрыл тело, abortFramesR296 отдал кадр-причину (upstream_stall) и
// терминатор `{"done":true,"done_reason":"error"}`, а затем исполнение
// доходило до блока «lastData == ""» в конце функции и писало ВТОРОЙ
// терминатор — `{"done":true,"done_reason":"stop"}`. Клиент видел два `done`:
// один с ошибкой, второй как успешное завершение.
func (st *llamaCppStreamState) reportPrematureEnd() error {
	if st.streamCompleted {
		return nil
	}
	// ==== Stream-truncation detection (2026-06-26) ====
	// Сценарии:
	//   1. scanner.Err() != nil → broken pipe / context canceled / timeout
	//   2. scanner.Err() == nil && streamCompleted == false → cppworker
	//      закрыл TCP без [DONE] (редкий случай, обычно после ошибки модели).
	//
	// В обоих случаях клиент уже получил HTTP 200 + Content-Type, поэтому
	// поменять HTTP-статус нельзя. Лучшее, что мы можем — дописать в стрим
	// финальный маркер и закрыть соединение, чтобы клиент не висел.
	scannerErr := st.scanner.Err()
	// R259: КТО завершил стрим. Наш лимит строки и наши отмены — НЕ
	// «upstream truncated»; клиенту и в лог уходит правдивая причина.
	abortReason := streamAbortReasonR259(scannerErr, st.r.Context().Err())
	// R298: если поток разорвал СТОРОЖ тишины, причина — не «ошибка чтения»
	// (её мы сами и создали, закрыв тело), а именно зависание upstream.
	// Проверяем ДО эвристики R271: сторож — наш факт, а не догадка.
	stallEventR298, stalledR298 := st.stallWatch.Fired()
	if stalledR298 {
		abortReason = streamAbortUpstreamStall
		logger.Get().Warnw("proxyRequestLlamaCpp: stream aborted by the stall watchdog (R298)",
			"backend", st.backendID, "model", *st.modelFromCtx,
			"prefill", stallEventR298.prefill,
			"stalled_sec", int(stallEventR298.stalled.Seconds()),
			"original_path", st.originalPath,
			"bytes_forwarded", st.bytesForwarded.Load(),
			"elapsed_sec", time.Since(st.llamaStartTime).Seconds())
	}
	// R271 (2026-10-29): если незадолго до обрыва МЫ выгружали/перезагружали
	// эту модель на этом бэкенде — виноват не upstream.
	//
	// Живой дефект: второй клиент запросил другое окно n_ctx, балансер
	// перезагрузил модель, поток первого закрылся без маркера — и клиент
	// получил «stream ended by upstream before completion», хотя воркер был
	// жив. Теперь причина называется своим именем.
	if streamAbortBlamesUpstream(abortReason) {
		if kind, ok := st.p.recentDestructiveOpR271(st.backendID, *st.modelFromCtx, 3*time.Minute); ok {
			// R280: причина зависит от ВИДА операции (reload по чужому окну /
			// AutoTune / unload) — таблица одна, в active_streams.go.
			abortReason = abortReasonForDestructiveOpR271(kind)
			logger.Get().Warnw("proxyRequestLlamaCpp: stream ended because the balancer reloaded the model",
				"reason", abortReason, "cause", kind,
				"backend", st.backendID, "model", *st.modelFromCtx,
				"original_path", st.originalPath,
				"bytes_forwarded", st.bytesForwarded.Load(),
				"elapsed_sec", time.Since(st.llamaStartTime).Seconds())
		}
	}
	logFields := []interface{}{
		"reason", abortReason,
		"backend", st.backendID, "model", *st.modelFromCtx,
		"original_path", st.originalPath,
		"bytes_forwarded", st.bytesForwarded.Load(),
		"elapsed_sec", time.Since(st.llamaStartTime).Seconds(),
		"scanner_err", errString(scannerErr),
		"first_forward_err", errString(st.firstForwardErr),
		"last_forward_err", errString(st.lastForwardErr),
	}
	if abortReason == streamAbortOurLimit {
		logFields = append(logFields, "line_limit_bytes", streamMaxLineBytes())
		logger.Get().Warnw("proxyRequestLlamaCpp: SSE line exceeded balancer buffer (our_limit, not upstream)",
			logFields...)
	} else if abortReason == streamAbortClientCancel {
		// Клиент ушёл: писать в его соединение бессмысленно.
		logger.Get().Infow("proxyRequestLlamaCpp: stream ended by client (client_cancel)", logFields...)
		return st.lastForwardErr
	} else {
		logger.Get().Warnw("proxyRequestLlamaCpp: stream ended before [DONE] marker",
			logFields...)
	}

	// Эмитим финальный чанк, чтобы клиент точно завершил стрим.
	// Пробуем дописать; если клиент уже отвалился (broken pipe) — игнорируем ошибку.
	// R296 (2026-10-29): КОНТРАКТ ТЕРМИНАТОРА. Раньше причина обрыва и
	// терминатор ехали ОДНИМ кадром (`finish_reason:"truncated"` + `error`).
	// Клиент (Cline) видел `finish_reason` — то есть «ответ завершён» — и не
	// успевал прочитать причину: выглядело как успешный, но пустой финал.
	// Теперь сначала кадр-ПРИЧИНА (без finish_reason, то есть поток жив),
	// затем ровно один терминатор. Кадры собирает abortFramesR296 (там же
	// тестируются по отдельности).
	for _, frame := range abortFramesR296(st.originalPath, *st.modelFromCtx, abortReason) {
		_, _ = st.w.Write(frame)
	}
	// R384: терминатор уже в этих кадрах — второй писать нельзя.
	st.terminalAlreadySent = true
	if st.originalPath == "/v1/chat/completions" {
		_, _ = fmt.Fprintf(st.w, "data: [DONE]\n\n")
	}
	Flush(st.w)
	return nil
}

// ============================================================================
// R540 (R2 читаемости): подготовка, preflight и отправка запроса к воркеру.
//
// После R539 coordinator всё ещё держал 483 строки: кроме вызовов стадий в нём
// жили подготовка (бэкенд, имя модели, путь/тело), preflight окна, отправка с
// ретраями и прозрачная ленивая загрузка. Здесь они разложены по стадиям,
// названным в плане (docs/plans/2026-11-19-readability-refactor.md, §R2):
//
//	prepareUpstreamCall    — что решено ДО запроса (prepare)
//	awaitAdmissionAndLoad  — preflight n_ctx и учёт нагрузки (preflight)
//	prepareUpstreamRequest — URL, клиент с таймаутами, контекст, сам запрос
//	sendRequest            — отправка и ретраи (send)
//	awaitLoadedModel       — прозрачная ленивая загрузка (503 + polling)
//	writeStreamHeaders     — заголовки ответа до WriteHeader(200)
//
// Состояние стадии — upstreamCall: не копия локальных переменных, а их
// владелец; указатель живёт до конца запроса, потому что имя модели уточняется
// в цикле стрима (R422), а `defer` AutoTune читает его после возврата.
// Поведение перенесено дословно: порядок проверок, тексты и заголовки прежние.
// ============================================================================

// upstreamCall — решение о запросе: бэкенд, имя модели, путь/тело, клиент,
// контекст и сам запрос. Порядок полей — по fieldalignment.
type upstreamCall struct {
	// Моменты времени (R493-12a): старт прохода, приём запроса, отправка воркеру.
	llamaStartTime   time.Time
	requestStartTime time.Time
	upstreamSentTime time.Time

	// Тела: клиентское и переведённое для воркера.
	bodyBuf        []byte
	translatedBody []byte

	// Что решено до запроса.
	backendID    string
	targetURL    string
	originalPath string
	modelFromCtx string
	fullURL      string
	llamacppPath string
	clientRealIP string

	// Поверхности ввода-вывода и общий контекст запроса.
	w              http.ResponseWriter
	reqCtx         context.Context
	modelCtxAtomic atomic.Value

	// Бэкенд, запрос, ответ, клиент и схемы инструментов.
	p            *Proxy
	r            *http.Request
	state        *BackendState
	req          *http.Request
	resp         *http.Response
	clientForReq *http.Client
	toolSchemas  map[string]*toolParamSchema
	modelNameNow func() string

	// Счётчик попыток (1 = первая), число EOF-повторов и флаги стадии.
	llamaAttempt    int
	llamaEOFRetries int
	nativePath      bool
	isStreaming     bool
}
