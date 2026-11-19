// llamacpp_stream_upstream.go — R630 (2026-11-19): чтение ответа llama.cpp —
// сторожевые горутины, разбор строк и накопление нативных кадров.
//
// ЗАЧЕМ ВЫНЕСЕНО. `llamacpp_transport.go` держал 2964 строки и пять независимых
// слоёв: построение запроса к воркеру, чтение его ответа, перевод кадров, хвост
// стрима и «вызов апстрима». Читать это одним файлом нельзя: чтобы ответить на
// вопрос «почему клиент не получил токены», нужно было прокрутить всё.
// Здесь — ровно слой ЧТЕНИЯ: `startUpstreamReaders`, `readUpstreamStream`,
// `abortBeforeFrame`, `decideNativeMode`, `handleNativeNDJSONLine`,
// `accumulateNativeChunk`.
//
// ПОВЕДЕНИЕ НЕ МЕНЯЛОСЬ: код перенесён как есть.
package balancer

import (
	"bufio"
	"encoding/json"
	"fmt"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"strings"
	"time"
)

// startUpstreamReaders готовит чтение тела ответа и обе горутины, которые это
// чтение разблокируют: сторож отмены (R60.58) и сторож тишины (R298).
// Обязан вызываться один раз, до readUpstreamStream.
func (st *llamaCppStreamState) startUpstreamReaders() {
	// R60.58: те же буфер и лимит строки, что и раньше.
	st.scanner = bufio.NewScanner(st.resp.Body)
	st.scanner.Buffer(make([]byte, 0), streamMaxLineBytes())

	// R493-12r (этап 6 «два мозга», флаг LB_STREAM_PIPE): единый сток стрима.
	// nil при выключенном флаге — тогда цикл пишет напрямую, как раньше
	// (историческое поведение и путь отката).
	if streamPipeEnabledR493() {
		st.sink = newStreamSinkR493(st.w)
	}

	// R292: состояние «подозрительного на tool-вызов» текста.
	//   contentBufferSince — когда буфер начали копить (для grace-порога);
	//   contentBufferGaveUp — мы уже решили, что это проза: больше не копим.
	// Без второго флага следующая порция текста снова ушла бы в буфер, и клиент
	// опять увидел бы «замер» — то есть лечение было бы одноразовым.
	// (R539: обе величины и сам буфер — поля состояния, см. llamaCppStreamState.)

	// R493-12a: окна для правки НАТИВНОГО done-кадра. Запрос воркеру уже ушёл
	// (upstreamSentTime), значит «ожидание» для клиента — это отрезок до него:
	// очередь, авто-загрузка модели и preflight. Раньше он не попадал никуда.
	st.statsWindowsNative = statsWindowsR493{RequestStart: st.requestStartTime, UpstreamSent: st.upstreamSentTime}

	// R60.58 watcher goroutine: разблокирует блокирующий scanner.Scan() при cancel/timeout.
	//
	// БЕЗ ЭТОГО (R60.58 bug): bufio.Scanner.Scan() — блокирующий вызов. Если upstream
	// (cppworker) застрял на llama_decode/OOM/deadlock — balancer тоже застрянет на чтении.
	// Select с default внутри loop БЕСПОЛЕЗЕН потому что мы застряли внутри Scan(),
	// до select. Клиент получает бесконечный hang до тех пор пока upstream не вернётся
	// сам (что может быть никогда при deadlock) или connection не умрёт по network timeout.
	//
	// РЕШЕНИЕ: отдельная горутина следит за ctx.Done() и закрывает resp.Body при
	// cancel/timeout. Close прерывает блокирующий Read() в scanner → scanner.Scan()
	// возвращает false → loop выходит → emit error chunk клиенту.
	//
	// Применяется ко всем путям: /v1/chat/completions (Cline), /api/chat (OpenWebUI),
	// /api/generate (Ollama CLI).
	st.watcherStop = make(chan struct{})
	go func() {
		select {
		case <-st.r.Context().Done():
			logger.Get().Infow("proxyRequestLlamaCpp: client cancelled, aborting stream",
				"backend", st.backendID, "model", st.modelNameNow(),
				"context_error", st.r.Context().Err(),
				"bytes_forwarded", st.bytesForwarded.Load())
			// R184: закрытие СВОЕГО чтения не останавливает воркер - он продолжает
			// генерировать и держит единственный слот (живой замер: следующий клиент
			// ждал 100 429 мс). Отмену надо ПРИСЛАТЬ: POST /api/cancel.
			st.p.cancelUpstreamGeneration(st.backendID, st.modelNameNow(), st.upstreamRequestIDR360, "client_cancel")
		case <-st.reqCtx.Done():
			// R60.58: streamTimeout сработал ИЛИ cancel — в обоих случаях
			// закрываем upstream чтобы разблокировать scanner.Scan().
			// R184: таймаут тоже обязан освободить слот на воркере.
			st.p.cancelUpstreamGeneration(st.backendID, st.modelNameNow(), st.upstreamRequestIDR360, "stream_timeout")
			logger.Get().Warnw("proxyRequestLlamaCpp: streamTimeout fired, aborting stream",
				"backend", st.backendID, "model", st.modelNameNow(),
				"context_error", st.reqCtx.Err(),
				"stream_timeout_sec", getEffectiveStreamTimeoutSec(st.p, st.modelNameNow(), st.isStreaming),
				"bytes_forwarded", st.bytesForwarded.Load())
		case <-st.watcherStop:
			return
		}
		if st.resp.Body != nil {
			_ = st.resp.Body.Close()
		}
	}()

	// R298 (2026-10-29): СТОРОЖ ТИШИНЫ на блокирующем пути.
	//
	// ЖИВОЙ ДЕФЕКТ. Здесь сторож не существовал ВООБЩЕ: `scanner.Scan()` —
	// блокирующий вызов, а на стенде стоит `LB_STREAMING_NEVER_TIMEOUT=1`, то
	// есть ни deadline, ни idle-таймаута нет. Мёртвый-но-не-закрытый upstream
	// (docker pause, зависший llama_decode) оставлял и балансер, и клиента ждать
	// вечно; при этом hijack-путь (/v1) сторож имел — то есть два протокола вели
	// себя по-разному, и Cline через /api/chat не получал ни кадра-причины, ни
	// терминатора.
	//
	// Разблокировать `Scan()` можно только закрытием тела ответа, поэтому здесь
	// сторож работает НЕ через канал, а через onStall: он закрывает resp.Body,
	// чтение возвращается, и штатный post-loop ниже отдаёт кадры через
	// abortFramesR296 — ровно тот же контракт терминатора, что и на других путях.
	st.stallWatch = newStreamStallWatch(streamStallAbortTimeout(), streamFirstFrameAbortTimeout(), func(ev stallEvent) {
		// Сначала разблокируем ЧТЕНИЕ (Close), и только потом отменяем генерацию:
		// отмена — сетевой round-trip к воркеру, и если он завис (docker pause),
		// ставить её перед Close означало бы держать клиента ещё дольше.
		if st.resp.Body != nil {
			_ = st.resp.Body.Close()
		}
		logger.Get().Warnw("proxyRequestLlamaCpp: upstream stalled — body closed to unblock scan (R298)",
			"backend", st.backendID, "model", st.modelNameNow(),
			"prefill", ev.prefill,
			"stalled_sec", int(ev.stalled.Seconds()),
			"original_path", st.originalPath,
			"bytes_forwarded", st.bytesForwarded.Load())
		// R184: молчащий воркер всё равно держит слот — отменяем генерацию,
		// иначе следующий клиент получит «busy».
		st.p.cancelUpstreamGeneration(st.backendID, st.modelNameNow(), st.upstreamRequestIDR360, "upstream_stall")
	})
	st.stallWatch.Start()
}

// readUpstreamStream — цикл чтения ответа воркера: нативный NDJSON идёт клиенту
// как есть (R65d), OpenAI-SSE переводится в NDJSON (или проксируется как SSE).
// Ошибку возвращает только запись клиенту; обрыв потока обрабатывается в
// finalizeStream (кадры-причины R296).
func (st *llamaCppStreamState) readUpstreamStream() error {
	// Локальная ссылка на сканер: тело цикла остаётся буквально прежним
	// (`for scanner.Scan()`, `scanner.Text()`), и сторож R298 узнаёт
	// блокирующее чтение по имени сканера.
	scanner := st.scanner
	for scanner.Scan() {
		// R298: данные пришли — отсчёт тишины сбрасывается.
		st.stallWatch.Touch()
		// Round 23 (2026-08-04): проверяем отмену клиента ПЕРЕД чтением следующего
		// чанка: при отмене клиент уже не ждёт, и кадр-терминатор ему не нужен.
		//
		// R53.6 (2026-08-24): добавлен случай reqCtx.Done() — streamTimeout
		// (per-model profile, default 600s): без него балансер зависает на
		// генерации длинного контекста.
		//
		// R65c (2026-09-16): клиенту отдаётся truncated-кадр, чтобы парсер
		// клиента (aiohttp) увидел терминатор до tear-down соединения.
		if st.abortBeforeFrame() {
			return nil
		}

		line := scanner.Text()

		// R66c (2026-09-22): нативный passthrough валиден ТОЛЬКО если апстрим
		// реально отвечает Ollama-NDJSON. Если cppworker ответил OpenAI-SSE
		// (сборка без нативного пути, LB_OLLAMA_NATIVE_PATH=0 или путь был
		// переписан апстримом), то сырые OpenAI-чанки уходили Ollama-клиенту
		// КАК NDJSON: вместо message.content приходили choices/delta, а
		// финального done:true не было вообще — клиент зависает либо считает
		// ответ пустым ("Model returned empty response").
		//
		// Определяем по СТРОКЕ, а не по Content-Type: cppworker исторически
		// отдаёт NDJSON и с неверным/отсутствующим заголовком, поэтому
		// заголовку здесь доверять нельзя. Первая же строка с префиксом
		// "data:" означает SSE — переключаемся на трансляцию SSE→NDJSON ниже.
		st.decideNativeMode(line)

		// ==== R65d (2026-09-20): нативный путь — Ollama NDJSON passthrough ====
		//
		// cppworker's нативные обработчики (/api/chat, /api/generate) отдают
		// application/x-ndjson: одна JSON-строка на строку, БЕЗ префикса "data:".
		// Формат уже тот, который ждёт Ollama-клиент, поэтому перевод не нужен —
		// отдаём строку как есть (плюс '\n', он же был в исходной строке).
		//
		// Что здесь отслеживаем (для диагностики, не для преобразования):
		//   - streamCompleted: встретили done:true → стрим завершён штатно,
		//     downstream truncation-detection не должен срабатывать;
		//   - accumulatedPlainContent: накапливаем message.content / response
		//     для R60.21 auto-continue и для логов;
		//   - usageChunkSeen: нативный done-чанк уже содержит eval_count /
		//     total_duration, поэтому writeStreamingSSEDone не должен писать
		//     второй done-чанк.
		//
		// ВАЖНО: keepalive-строки {"keepalive":true} проходят насквозь — это
		// валидный NDJSON для Ollama-клиентов (см. handlers_chat.go:861).
		if st.nativePath {
			if err := st.handleNativeNDJSONLine(line); err != nil {
				return err
			}
			continue
		}

		if !strings.HasPrefix(line, "data:") {
			continue
		}

		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			st.streamCompleted = true
			// R60.55 (2026-09-13): DEFER writing the done-chunk until AFTER
			// we check for truncation. Otherwise OpenWebUI receives done=true
			// for the original truncated content, then sees ANOTHER done=true
			// for the combined content (after auto-continue) → displays as
			// duplicate response. We mark `deferredDoneChunk` and emit later.
			st.deferredDoneChunk = true
			break
		}

		st.lastData = data

		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			logger.Get().Debugw("proxyRequestLlamaCpp: skipping non-JSON SSE data",
				"data", data, "error", err)
			continue
		}

		if *st.modelFromCtx == "" {
			if m, ok := chunk["model"].(string); ok && m != "" {
				*st.modelFromCtx = m
			}
		}

		// R493-12a: первый дельта-чанк ЛЮБОГО вида закрывает префилл. Reasoning
		// считается так же, как content: время размышлений — это генерация, а не
		// «оценка промпта» (их токены всегда входят в eval_count).
		if st.firstDeltaTime.IsZero() && chunkHasStatsDeltaR493(chunk) {
			st.firstDeltaTime = time.Now()
		}

		// R65d (2026-09-20): агрегируем статистику токенов со streaming-пути.
		// cppworker присылает OpenAI usage-чанк ({prompt_tokens,
		// completion_tokens}) при stream_options.include_usage.
		st.recordTokenUsageFromUsageChunk(chunk)

		// Для SSE→SSE passthrough (/v1/chat/completions) — проксируем как есть,
		// дополнительно накапливая delta.content на случай если клиент захочет
		// полный контент после [DONE].
		if st.originalPath == "/v1/chat/completions" {
			if err := st.forwardOpenAISSEPassthrough(data, chunk); err != nil {
				return err
			}
			continue
		}
		if err := st.translateSSEToNDJSON(data, chunk); err != nil {
			return err
		}
	}
	return nil
}

// abortBeforeFrame — отмена клиента или таймаут стрима между кадрами.
// true — клиенту уже отдан терминатор, цикл обязан завершиться.
//
// Round 23 (2026-08-04): проверяем отмену клиента ПЕРЕД чтением следующего
// чанка. Без этого streaming-прокси ждёт [DONE] от upstream (cppworker
// генерирует токены, пока cancel не дойдёт через отдельный канал), что
// приводит к зависанию при OpenWebUI cancel — клиент закрыл соединение, но
// прокси продолжает тянуть токены с cppworker и тратить CPU/GPU.
//
// Применяется ко всем путям: /v1/chat/completions (Cline), /api/chat
// (OpenWebUI), /api/generate (Ollama CLI). При срабатывании — закрываем
// upstream body (cppworker прекратит генерацию после текущего token'а)
// и выходим из loop.
//
// R53.6 (2026-08-24): добавлен case <-reqCtx.Done() — streamTimeout (per-model
// profile, default 600s). Без этого case balancer зависает на генерации
// длинного контекста (cppworker генерирует 0 токенов, балансер ждёт [DONE]).
// При срабатывании reqCtx балансер сам отменяет cppworker раньше client timeout
// → R53.5 writeStreamErrorChunk эмитит done:true chunk клиенту → клиент
// получает "stream proxy error" вместо "Did not receive done" (TCP close).
//
// R60.58: select с default БОЛЬШЕ не нужен внутри loop — watcher goroutine
// (см. startUpstreamReaders) закрывает resp.Body при cancel/timeout,
// scanner.Scan() возвращает false, loop выходит естественно. Здесь оставлен
// только для обратной совместимости и для early-detect cancel между чанками
// (быстрее чем ждать Close).
func (st *llamaCppStreamState) abortBeforeFrame() bool {
	select {
	case <-st.r.Context().Done():
		if st.resp.Body != nil {
			_ = st.resp.Body.Close()
		}
		// R65c (2026-09-16): client cancelled mid-stream — emit truncated
		// done chunk so the client's chunked parser (aiohttp) sees terminator
		// before connection tear-down. Without this, OpenWebUI gets
		// TransferEncodingError "Not enough data to satisfy transfer length header".
		st.streamCompleted = true
		emitTruncatedChunk(st.w, st.originalPath, *st.modelFromCtx, time.Since(st.llamaStartTime), "client_cancel")
		return true
	case <-st.reqCtx.Done():
		if st.resp.Body != nil {
			_ = st.resp.Body.Close()
		}
		// R65c (2026-09-16): stream timeout — emit truncated done chunk and
		// return nil so handleChat doesn't write ANOTHER error chunk on top.
		// Pre-R65c: function returned error here, which caused handleChat's
		// writeStreamErrorChunk to fail silently (data already buffered in
		// Go's http.ResponseWriter but never flushed because connection was
		// being torn down). Result: client sees chunked stream without done
		// chunk + without terminator → TransferEncodingError в aiohttp/OpenWebUI.
		st.streamCompleted = true
		emitTruncatedChunk(st.w, st.originalPath, *st.modelFromCtx, time.Since(st.llamaStartTime), "timeout")
		return true
	default:
		return false
	}
}

// decideNativeMode — R66c: режим ответа определяется по ПЕРВОЙ содержательной
// строке, а не по Content-Type: cppworker исторически отдаёт NDJSON и с
// неверным/отсутствующим заголовком. Первая строка с префиксом "data:" означает
// SSE — переключаемся на трансляцию SSE→NDJSON.
func (st *llamaCppStreamState) decideNativeMode(line string) {
	if !st.nativePath || st.nativeModeDecided {
		return
	}
	st.nativeModeDecided = true
	firstLine := strings.TrimSpace(line)
	if strings.HasPrefix(firstLine, "data:") {
		st.nativePath = false
		logger.Get().Warnw("proxyRequestLlamaCpp: upstream answered with OpenAI SSE "+
			"while request was routed as Ollama-native — switching to SSE→NDJSON translation",
			"backend", st.backendID, "model", *st.modelFromCtx, "path", st.originalPath,
			"content_type", st.resp.Header.Get("Content-Type"))
	}
}

// handleNativeNDJSONLine — нативный passthrough (R65d): строка NDJSON уходит
// клиенту как есть, но статистика, дубль в done-кадре (R91/R331), причина
// отмены (R354), окна R493-12a и служебные метки (R266.1) правятся по дороге.
func (st *llamaCppStreamState) handleNativeNDJSONLine(line string) error {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return nil
	}
	// Терпимость к SSE-фреймингу: нативный обработчик cppworker отдаёт
	// «голый» NDJSON, но при смешанных версиях (или когда путь был
	// переписан апстримом) строка может прийти как `data: {...}`.
	// Снимаем префикс, чтобы разбор ниже работал единообразно.
	if strings.HasPrefix(trimmed, "data:") {
		trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if trimmed == "" || trimmed == "[DONE]" {
			// [DONE] — терминатор SSE-стрима; в NDJSON-мире его нет,
			// поэтому просто фиксируем завершение и не пересылаем
			// клиенту невалидную для NDJSON строку.
			if trimmed == "[DONE]" {
				st.streamCompleted = true
			}
			return nil
		}
	}
	// R91 (2026-10-09): текст, отданный ДЕЛЬТАМИ до этой строки. Нужен,
	// чтобы снимать дубль в done-чанке только тогда, когда ответ уже уехал
	// клиенту дельтами (см. stripNativeDoneContent).
	streamedBeforeThisLine := st.accumulatedPlainContent
	var nativeChunk map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &nativeChunk); err != nil {
		// Не JSON (например, SSE-комментарий от старой версии
		// cppworker) — отдаём как есть, клиент сам решит.
		logger.Get().Debugw("proxyRequestLlamaCpp[native]: non-JSON NDJSON line passed through",
			"backend", st.backendID, "line", trimmed)
	} else {
		if st.accumulateNativeChunk(nativeChunk) {
			return nil
		}
	}
	outLine := st.normalizeNativeDoneLine(trimmed, streamedBeforeThisLine)
	n, errFwd := st.writeClientLine([]byte(outLine))
	if errFwd != nil {
		st.lastForwardErr = errFwd
		if st.firstForwardErr == nil {
			st.firstForwardErr = errFwd
		}
		return fmt.Errorf("write native NDJSON: %v", errFwd)
	}
	_ = n
	// R66b (2026-09-22): отмечаем, что для нативного пути апстрим уже
	// отдал хотя бы одну NDJSON-строку. Без этого lastData оставался ""
	// (он заполняется только в SSE-ветке ниже), и финальный блок
	//   if lastData == "" && originalPath != "/v1/chat/completions"
	// в конце функции досылал клиенту ЛИШНИЙ синтетический done-чанк
	// ({"done":true,"done_reason":"stop","message":{"content":""}}) уже
	// ПОСЛЕ настоящего done от cppworker.
	//
	// Симптом на реальном клиенте: Cline CLI (ollama provider) получал
	// два done-чанка — первый с tool_calls, второй пустой — и в итоге
	// сообщал "Model returned empty response"; инструмент не вызывался.
	// Ollama-клиенты на два done отвечают
	// "Did not receive done or success response in stream".
	//
	// Флаш уже сделан writeClientLine (у стока — свой, у исторической
	// ветки — тот же http.Flusher). Отдельный флаш здесь означал бы второй
	// набор правил записи — ровно то, что этап 6 и убирает.
	st.lastData = trimmed
	return nil
}

// accumulateNativeChunk — разбор одного нативного NDJSON-кадра: имя модели,
// граница префилла, накопленный текст, завершение хода и статистика токенов.
// true — кадр запустил адресную перезагрузку по n_ctx (R83): поток завершён,
// ответ клиенту уже отдан.
func (st *llamaCppStreamState) accumulateNativeChunk(nativeChunk map[string]interface{}) bool {
	if *st.modelFromCtx == "" {
		if m, ok := nativeChunk["model"].(string); ok && m != "" {
			*st.modelFromCtx = m
			st.modelCtxAtomic.Store(m)
		}
	}
	// R493-12a: первый дельта-чанк ЛЮБОГО вида (включая thinking) —
	// граница префилла. Здесь же ловятся и native-чанки, у которых
	// текст лежит в message.thinking, а не в message.content.
	if st.firstDeltaTime.IsZero() && chunkHasStatsDeltaR493(nativeChunk) {
		st.firstDeltaTime = time.Now()
	}
	// Накапливаем видимый пользователю текст.
	//   /api/chat     → message.content (reasoning идёт в message.reasoning)
	//   /api/generate → response
	//
	// R118.17 (2026-10-15): done-чанк cppworker несёт ПОЛНЫЙ текст,
	// который уже ушёл дельтами (клиенту он не отдаётся — R91 снимает
	// дубль в stripNativeDoneContent). Раньше он ПРИКЛЕИВАЛСЯ к
	// накопленному, и детектор обрыва R60.21 получал текст в двух
	// копиях: число ``` становилось чётным, последняя строка —
	// «полной», и авто-продолжение не срабатывало, хотя клиент видел
	// обрыв. Живой замер до правки: 147 символов дельт + столько же в
	// done-чанке = 294 на входе детектора, truncate_reason="".
	chunkIsDone, _ := nativeChunk["done"].(bool)
	if msg, ok := nativeChunk["message"].(map[string]interface{}); ok {
		// R321: считаем размышления по НАИБОЛЬШЕМУ из двух имён —
		// cppworker отдаёт `thinking` (канон) и `reasoning` (legacy)
		// ОДНИМ И ТЕМ ЖЕ текстом, и суммирование дало бы вдвое больше
		// символов в пояснении клиенту.
		thinkChars := 0
		if t, ok := msg["thinking"].(string); ok {
			thinkChars = len(t)
		}
		if r, ok := msg["reasoning"].(string); ok && len(r) > thinkChars {
			thinkChars = len(r)
		}
		if thinkChars > 0 {
			st.nativeReasoningChars += thinkChars
		}
		if c, ok := msg["content"].(string); ok && c != "" {
			st.accumulatedPlainContent = appendNativeContent(st.accumulatedPlainContent, chunkIsDone, c)
			if st.firstContentTime.IsZero() {
				st.firstContentTime = time.Now()
			}
		}
	}
	if rsp, ok := nativeChunk["response"].(string); ok && rsp != "" {
		st.accumulatedPlainContent = appendNativeContent(st.accumulatedPlainContent, chunkIsDone, rsp)
		if st.firstContentTime.IsZero() {
			st.firstContentTime = time.Now()
		}
	}
	if done, ok := nativeChunk["done"].(bool); ok && done {
		st.streamCompleted = true
		st.usageChunkSeen = true
		st.priorDoneEmitted = true
		// H2 (2026-10-16): нативный done-чанк отдаётся как есть, поэтому
		// статистика считается дошедшей только если cppworker реально
		// положил её в чанк (eval_count/prompt_eval_count). Если полей
		// нет — statsEmitted остаётся false, и поздний SSE usage-чанк
		// не будет потерян.
		if _, hasEval := nativeChunk["eval_count"]; hasEval {
			st.statsEmitted = true
		} else if _, hasPrompt := nativeChunk["prompt_eval_count"]; hasPrompt {
			st.statsEmitted = true
		}
		st.upstreamHadFinishReason = true
		if c, ok := nativeChunk["error"].(string); ok && c != "" {
			// R354 (2026-10-31): отмена ПОЛЬЗОВАТЕЛЕМ (кнопка Stop) — не сбой
			// воркера. Такой кадр не идёт в upstreamErrorText (по нему
			// работают auto-continue и распознавание n_ctx) и не пишется как
			// Warn: клиенту уйдёт нормальный терминатор с done_reason=cancelled.
			if isUserCancelDoneChunkR354(c) {
				st.userCancelledDoneChunkR354 = true
				logger.Get().Infow("proxyRequestLlamaCpp[native]: генерация отменена клиентом (R354)",
					"backend", st.backendID, "model", *st.modelFromCtx)
			} else {
				// R289: запоминаем текст ошибки — по нему отличим «контекст
				// мал» от обрыва до того, как сработает auto-continue.
				st.upstreamErrorText = c
				logger.Get().Warnw("proxyRequestLlamaCpp[native]: upstream done-chunk carries error",
					"backend", st.backendID, "model", *st.modelFromCtx, "error", c)

				// R83 (2026-09-29): n_ctx-ошибка в done-чанке = устаревший кэш
				// n_ctx в балансере. Запускаем тот же auto-reload, что и для
				// HTTP 4xx ниже — иначе получается ДЕДЛОК.
				//
				// Живой сценарий (лог 2026-09-29): балансер держал в
				// LastKnownNCtx 128000, а cppworker после рестарта поднял
				// модель с 32768. Preflight видел «128000 >= required» и
				// возвращал NoOp (reload не запускался), а cppworker на каждый
				// запрос отвечал done-чанком «code 2: requested n_ctx=128000
				// exceeds model's effective n_ctx=32768». Клиент (Cline)
				// получал ошибку на каждом запросе, и так до следующего
				// удачного reload.
				//
				// cppworker отдаёт это как HTTP 200 + error в финальном чанке,
				// поэтому ветка `resp.StatusCode >= 400` ниже не срабатывала.
				//
				// ВАЖНО: ParseCppWorkerError намеренно возвращает nil для
				// status < 400, поэтому распознаём n_ctx-ошибку по тексту
				// самого сообщения («code 2» / «code 3» — те же коды, что
				// bridge.ErrCodeNCtxNeedsReload / ErrCodePromptTooLong).
				if st.p.nctxReload != nil && isNCtxDoneChunkError(c) {
					reloadModel := *st.modelFromCtx
					if reloadModel == "" {
						reloadModel = st.backendID
					}
					logger.Get().Infow(i18n.T("balancer.llamacpp_transport.done_chunk_nctx_error_reload_info"),
						"backend", st.backendID, "model", reloadModel, "error", c)
					nctxErr := newNCtxErrorFromDoneChunk(c, st.backendID)
					st.p.handleNCtxReload(st.r.Context(), st.w, st.r, st.backendID, reloadModel, nctxErr, st.bodyBuf)
					return true
				}
			}
		}
		// R65d: агрегируем статистику токенов для /api/v1/stats/tokens.
		// Нативный формат Ollama: prompt_eval_count / eval_count.
		if !st.tokenUsageRecorded {
			promptTokens := int64(intFromMap(nativeChunk, "prompt_eval_count"))
			evalTokens := int64(intFromMap(nativeChunk, "eval_count"))
			if promptTokens > 0 || evalTokens > 0 {
				st.tokenUsageRecorded = true
				st.p.recordTokenUsage(*st.modelFromCtx, promptTokens, evalTokens)
				// R550 (T-10 аудита): замер скорости генерации — РЕАЛЬНЫЙ.
				//
				// До R550 `ModelLatencyTracker.RecordSample` вызывался только из
				// тестов, поэтому «измеренная» скорость декода на карточке группы
				// (groupModelInfoR104) не могла появиться никогда — она всегда
				// показывала «н/д», а комментарии в proxy_timeout.go ссылались на
				// адаптивные тиры как на рабочие. Здесь есть всё, что нужно для
				// замера: число сгенерированных токенов и время старта стрима.
				st.p.recordGenerationSample(*st.modelFromCtx, evalTokens, st.llamaStartTime, st.firstDeltaTime)
				// R598 (R495 S2b): те же токены и те же окна — источник средней
				// скорости префилла/декода. Конец окна здесь: done-чанк уже пришёл.
				wThroughput := st.statsWindowsNative
				if wThroughput.End.IsZero() {
					wThroughput.End = time.Now()
				}
				if wThroughput.RequestStart.IsZero() {
					wThroughput.RequestStart = st.llamaStartTime
				}
				st.p.recordThroughputSampleR598(st.backendID, promptTokens, evalTokens, wThroughput)
			}
		}
	}
	return false
}
