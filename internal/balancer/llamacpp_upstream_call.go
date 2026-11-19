// llamacpp_upstream_call.go — R630 (2026-11-19): «вызов апстрима» — подготовка,
// ожидание допуска и загрузки модели, отправка, заголовки стрима.
//
// ЗАЧЕМ ВЫНЕСЕНО. `prepareUpstreamCall` и методы `upstreamCall` — единственное
// место, где стриминговый путь ждёт admission-очередь и загрузку модели, и где
// считается `X-LB-*` для клиента. Это самостоятельный слой с своими тестами.
//
// ПОВЕДЕНИЕ НЕ МЕНЯЛОСЬ: код перенесён как есть.
package balancer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"ollama-loadbalancer/pkg/logger"
	"strings"
	"time"
)

// prepareUpstreamCall — стадия prepare: бэкенд и его адрес, имя модели из
// контекста (+ atomic и функция для сторожей), схемы инструментов, нативный
// путь и тело для воркера.
//
// Возвращает (nil, nil), когда ответ уже отправлен отказом по семейству модели
// (R268): координатор в этом случае просто завершает ход.
func (p *Proxy) prepareUpstreamCall(w http.ResponseWriter, r *http.Request, backendID string, bodyBuf []byte) (*upstreamCall, error) {
	state, ok := p.backends[backendID]
	if !ok {
		return nil, fmt.Errorf("backend %s not found", backendID)
	}

	c := &upstreamCall{
		p:            p,
		w:            w,
		r:            r,
		backendID:    backendID,
		state:        state,
		targetURL:    p.getBackendBaseURL(state.Backend),
		originalPath: r.URL.Path,
		bodyBuf:      bodyBuf,
		llamaAttempt: 1,
	}
	if m, ok := r.Context().Value(modelContextKey).(string); ok {
		c.modelFromCtx = m
	}
	// R422 (2026-11-03): имя модели узнаётся из первого содержательного чанка
	// (в цикле стрима) и читается ещё и сторожем/onStall-горутинами. Голый
	// string — гонка (поймана -race на TestR298): пишем в основной горутине,
	// читаем в watcher. Держим отдельное atomic-значение; основной поток
	// продолжает пользоваться полем структуры без накладных расходов.
	initialModelName := c.modelFromCtx
	if c.modelFromCtx != "" {
		c.modelCtxAtomic.Store(c.modelFromCtx)
	}
	c.modelNameNow = func() string {
		if v, ok := c.modelCtxAtomic.Load().(string); ok && v != "" {
			return v
		}
		return initialModelName
	}

	// R186 (2026-10-20): схемы инструментов из ЗАПРОСА. Нужны, чтобы привести
	// аргументы вызова, который балансер сам собрал из текста модели (ниже,
	// extractToolCallsFromSSEContent): без этого клиент (Cline) отвергал вызов
	// «expected array, received string» и повторял его до остановки по циклу.
	c.toolSchemas = toolSchemasFromBody(bodyBuf)

	// R65d (2026-09-20): Ollama-пути идут в cppworker НАТИВНО (без трансляции
	// в OpenAI-формат). См. llamacpp_native_path.go для полного обоснования:
	// транслятор терял options.* / keep_alive / format / think / images, а
	// перенесённые поля без пары в cppworker-структурах (например top_k)
	// отвергались строгим декодером с HTTP 400.
	//
	// При нативном пути:
	//   - путь НЕ переписывается (остаётся /api/chat, а не /v1/chat/completions);
	//   - тело НЕ транслируется (options.* остаются на месте);
	//   - ответ уже приходит в Ollama-формате (NDJSON), поэтому passthrough-ветка
	//     цикла отдаёт строки клиенту как есть — без SSE→NDJSON перевода.
	c.nativePath = shouldRouteOllamaNative(c.originalPath)

	// R268 (2026-10-28): семейство модели решает, умеем ли мы вообще ПРЕВРАТИТЬ
	// ответ в Ollama API. На passthrough-путях (нативный Ollama-путь, /v1/*)
	// преобразования нет — ответ идёт как есть, и отказ там запрещён.
	//
	// Отказ ставится ДО запроса к модели: незачем занимать слот и минуты
	// генерации ради ответа, который мы не сможем собрать. Текст отказа называет
	// семейство, версию, поддержанный диапазон и порт OpenAI-passthrough —
	// оператор должен понять причину и что делать, а не увидеть «ошибка».
	hasTools := len(c.toolSchemas) > 0
	// R532 (R2 читаемости): отказ по семейству модели и лог решения вынесены.
	if p.refuseUnsupportedFamilyAndLog(w, c.originalPath, c.modelFromCtx, hasTools, c.nativePath) {
		return nil, nil
	}

	// R533 (R2 читаемости): путь и тело для cppworker — одной функцией.
	c.llamacppPath, c.isStreaming, c.translatedBody = makeUpstreamPathAndBody(c.originalPath, c.nativePath, c.bodyBuf)

	// F.0.4 (2026-06-28): session F — диагностика EOF.
	// Запоминаем startTime для расчёта duration_ms при ошибках upstream.
	c.llamaStartTime = time.Now()
	// R493-12a: если вход запроса (Proxy.ServeHTTP) отметил момент приёма — считаем
	// от него. llamaStartTime ставится уже ПОСЛЕ выбора бэкенда, авто-загрузки и
	// ожидания слота, поэтому «сколько клиент ждал» из него не видно вовсе
	// (живой замер: 44 с ожидания при total_duration 2.55 с).
	c.requestStartTime = requestStartR493(r.Context())
	if c.requestStartTime.IsZero() {
		c.requestStartTime = c.llamaStartTime
	}
	// R493-12a (2026-11-12): окна времени для статистики скорости токенов.
	//
	// ДО ЭТОЙ ПРАВКИ: prompt_eval_duration считался от llamaStartTime, то есть
	// включал очередь, авто-загрузку и synchronous preflight n_ctx (на 27B —
	// минуты), а первый контент-чанк не двигался reasoning-дельтами, поэтому
	// время размышлений тоже уходило в «время промпта», хотя их токены входили
	// в eval_count. Агент получал 21 t/s вместо ~400 и 104 t/s вместо ~8.
	//
	// ТЕПЕРЬ: upstreamSentTime — момент реальной отправки запроса воркеру (после
	// очереди/загрузки/preflight); firstDeltaTime (поле состояния стрима) —
	// первый дельта-чанк ЛЮБОГО вида (content, reasoning/thinking, tool_calls).
	// Разбор окна: docs/architecture/stats-and-context-check.ru.md,
	// код: stats_windows.go.
	return c, nil
}

// awaitAdmissionAndLoad — стадия preflight: окно n_ctx (R447/R479) и учёт
// num_ctx запроса для будущих решений AutoTune (R54.9). true — ответ клиенту
// уже отправлен (413/404/503 из вердикта preflight).
func (c *upstreamCall) awaitAdmissionAndLoad() bool {
	// PREFLIGHT n_ctx auto-reload: если клиент (OpenWebUI) задал options.num_ctx,
	// а loaded n_ctx на бэкенде меньше — синхронно перезагружаем модель на нужный n_ctx
	// ДО проксирования. Это критично для OpenWebUI с tools: первый запрос
	// получает reload один раз (10-30 сек), последующие — instant response.
	// R447: слот под числа вердикта R301/R385 — writer тела (ниже) обязан
	// написать те же числа, что названы в тексте причины.
	// R479: стадия preflight — видно, что ход застрял на решении об окне.
	SetStageR477(c.r.Context(), stagePreflightR477)
	preflightCtx, _ := growthNumbersSlotR447(c.r.Context())
	preflightBody, preflightOK, preflightMsg, preflightStatus := c.p.preflightNCtxReloadIfNeeded(
		preflightCtx, c.backendID, c.modelFromCtx, c.bodyBuf, c.originalPath)
	// R530 (R2 читаемости): применение вердикта preflight вынесено в
	// applyPreflightOutcome — там ответ клиенту (413/404/503) и правка тела
	// запроса. handled=true означает, что ответ уже отправлен.
	bodyBuf, translatedBody, preflightHandled := c.p.applyPreflightOutcome(
		c.w, preflightCtx, c.backendID, c.modelFromCtx, c.originalPath, c.nativePath,
		preflightBody, preflightOK, preflightMsg, preflightStatus, c.bodyBuf, c.translatedBody)
	if preflightHandled {
		return true
	}
	c.bodyBuf, c.translatedBody = bodyBuf, translatedBody

	// R54.9 (2026-08-24): Workload tracking — record num_ctx from request body
	// for future AutoTune decisions. Неблокирующий, in-memory, sliding window.
	// Использует existing ExtractNumCtxFromBody (num_ctx_resolver.go) — уже
	// покрывает Ollama options.num_ctx + OpenAI top-level + JSON float decoding.
	if c.modelFromCtx != "" && c.p.WorkloadTracker() != nil {
		if numCtx := ExtractNumCtxFromBody(c.bodyBuf); numCtx > 0 {
			c.p.WorkloadTracker().Record(c.backendID, c.modelFromCtx, numCtx)
		}
	}
	return false
}

// prepareUpstreamRequest — URL для воркера, клиент с таймаутами, контекст и сам
// запрос. Возвращает cancel контекста: `defer` обязан выполнить вызывающий,
// иначе per-model таймаут стрима снимется сразу после создания запроса.
func (c *upstreamCall) prepareUpstreamRequest() (context.CancelFunc, error) {
	// R534 (R2 читаемости): URL и перенос name в query для /api/models/unload.
	c.fullURL, c.translatedBody = buildUpstreamURLAndBody(c.targetURL, c.llamacppPath, c.originalPath, c.backendID, c.translatedBody)
	logger.Get().Infow("proxyRequestLlamaCpp: sending request",
		"backend", c.backendID, "url", c.fullURL, "stream", c.isStreaming)

	// R537 (R2 читаемости): клиент запроса и его таймауты (per-model stream
	// timeout, FirstByteTimeout) вынесены в upstreamHTTPClientFor и
	// streamRequestContext.
	c.clientForReq = c.p.upstreamHTTPClientFor(c.isStreaming, c.modelFromCtx)

	reqCtx, cancelReq := c.p.streamRequestContext(c.r, c.isStreaming, c.backendID, c.modelFromCtx)
	c.reqCtx = reqCtx

	// R537 (R2 читаемости): запрос к воркеру и его заголовки — одной функцией.
	req, clientRealIP, err := c.p.newUpstreamRequest(reqCtx, c.r, c.fullURL, c.translatedBody, c.llamacppPath, c.backendID)
	if err != nil {
		cancelReq()
		return nil, fmt.Errorf("failed to create request: %v", err)
	}
	c.req = req
	c.clientRealIP = clientRealIP
	return cancelReq, nil
}

// sendRequest — стадия send: отправка запроса воркеру и ретраи ДО записи
// заголовков клиенту (connection-level с backoff и один EOF-повтор).
func (c *upstreamCall) sendRequest() error {
	// F.0.4 (2026-06-28): session F — EOF retry перед записью заголовков клиенту.
	//
	// Проблема: cppworker может вернуть EOF (закрытое TCP-соединение без ответа) в
	// редких ситуациях — например, когда модель ещё загружается (lazy load) или
	// произошёл panic в middleware ДО хендлера. В этом случае клиент видит EOF без
	// какой-либо диагностики.
	//
	// Решение: если получена EOF-ошибка ДО WriteHeader(200), пробуем один раз тот же
	// backend через 500ms (типичный случай — cppworker перезагружал модель ровно
	// во время первого запроса). После retry возвращаем расширенный error context
	// с backend_id, attempt и duration_ms для диагностики.
	//
	// 2026-07-01: добавляем waitForBackendModelLoaded retry при получении
	// HTTP 503 "model is loading" от cppworker. Lazy load теперь прозрачен для
	// клиента (OpenWebUI/Cline) — раньше стрим обрывался на первом запросе.
	//
	// После записи заголовков retry невозможен — TCP уже начат.
	// R493-12a: фиксируем момент отправки воркеру — с него начинается префилл,
	// и только он попадает в prompt_eval_duration. Все ожидания до этой строки
	// (очередь, авто-загрузка, preflight reload) уходят в load_duration.
	if c.upstreamSentTime.IsZero() {
		c.upstreamSentTime = time.Now()
	}
	resp, err := c.clientForReq.Do(c.req)
	// Round 8 (2026-07-10): connection-level error retry with exponential backoff
	// (только для non-streaming — для streaming headers ещё не записаны,
	// поэтому retry безопасен ДО первой записи в ResponseWriter).
	//
	// cppworker может быть в transition window (docker recreate, network blip):
	//   - connection refused (port 18092 ещё не listening)
	//   - connection reset (контейнер завершает keep-alive)
	//
	// 3 retry с backoff 1s/2s/4s перед сдачей. Не применяем к streaming после
	// записи headers (этот код срабатывает до WriteHeader, проверяется через
	// isStreaming + content-type).
	if err != nil && isConnectionLevelError(err) {
		maxConnectionRetries := 3
		connectionBackoffs := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}
		for retryIdx := 0; retryIdx < maxConnectionRetries; retryIdx++ {
			backoff := connectionBackoffs[retryIdx]
			logger.Get().Warnw("proxyRequestLlamaCpp: connection-level error, backing off before retry",
				"backend", c.backendID,
				"model", c.modelFromCtx,
				"attempt", c.llamaAttempt,
				"retry_idx", retryIdx+1,
				"max_retries", maxConnectionRetries,
				"backoff_ms", backoff.Milliseconds(),
				"error", err)
			select {
			case <-time.After(backoff):
			case <-c.reqCtx.Done():
				durationMs := time.Since(c.llamaStartTime).Milliseconds()
				return fmt.Errorf("llama.cpp request failed [backend=%s, attempt=%d, duration_ms=%d, error_type=%s, cancelled_during_backoff]: %v",
					c.backendID, c.llamaAttempt, durationMs, determineErrorType(err, c.reqCtx), err)
			}
			c.llamaAttempt++
			resp, err = c.clientForReq.Do(c.req)
			if err == nil {
				break // success
			}
			if !isConnectionLevelError(err) {
				break // error type changed, exit retry loop
			}
		}
	}
	// EOF-retry counter (1 = первая попытка). Используется в error context и для
	// retry на тот же бэкенд после 500ms (типичный случай — cppworker перезагружал
	// модель ровно во время первого запроса).
	const maxLlamaEOFRetries = 1
	if err != nil && c.llamaEOFRetries < maxLlamaEOFRetries {
		errType := determineErrorType(err, c.reqCtx)
		if errType == "unexpected_eof" {
			logger.Get().Warnw("proxyRequestLlamaCpp: EOF from upstream, retrying once on same backend",
				"backend", c.backendID, "model", c.modelFromCtx,
				"attempt", c.llamaAttempt, "error", err)
			// Публикуем EOF event в EventPublisher (если подключён).
			c.p.publishTransportEOF(c.backendID, c.modelFromCtx, c.originalPath, err, time.Since(c.llamaStartTime))
			// Задержка 500ms перед retry — типичное время reload-операций cppworker.
			time.Sleep(c.p.getStreamingRetryDelay())
			c.llamaEOFRetries++
			c.llamaAttempt++
			resp, err = c.clientForReq.Do(c.req)
		}
	}
	if err != nil {
		durationMs := time.Since(c.llamaStartTime).Milliseconds()
		errType := determineErrorType(err, c.reqCtx)
		logger.Get().Errorw("proxyRequestLlamaCpp: upstream Do() failed",
			"backend", c.backendID,
			"model", c.modelFromCtx,
			"url", c.fullURL,
			"attempt", c.llamaAttempt,
			"duration_ms", durationMs,
			"error_type", errType,
			"is_streaming", c.isStreaming,
			"error", err)
		// Публикуем EOF event (если это EOF и мы ещё не публиковали).
		if errType == "unexpected_eof" {
			c.p.publishTransportEOF(c.backendID, c.modelFromCtx, c.originalPath, err, time.Duration(durationMs)*time.Millisecond)
		}
		// Round 8 (2026-07-10): persistent connection_refused → mark backend unhealthy.
		if errType == "connection_refused" {
			c.p.markBackendConnectionFailed(c.backendID)
		}
		return fmt.Errorf("llama.cpp request failed [backend=%s, attempt=%d, duration_ms=%d, error_type=%s]: %v",
			c.backendID, c.llamaAttempt, durationMs, errType, err)
	}
	c.resp = resp
	return nil
}

// awaitLoadedModel — R493/2026-07-01: прозрачная ленивая загрузка. cppworker на
// первый inference-запрос к незагруженной модели отвечает HTTP 503 с телом
// {"error":"model is loading: <name>","loading":true,...}. true — ответ клиенту
// уже отправлен (polling исчерпан, отдан 503).
func (c *upstreamCall) awaitLoadedModel() (bool, error) {
	// 2026-07-01: transparent lazy-load retry (см. loading_retry.go).
	//
	// Без retry OpenWebUI/Cline видел оборванный стрим. Теперь:
	//   1. Делаем 503 response body readable (нужно для анализа сигнала).
	//   2. Если 503 + loading — polling /api/models/load/progress до 30 сек.
	//   3. После успешного polling повторяем clientForReq.Do(req) с тем же body.
	// Retry возможен ТОЛЬКО до записи заголовков клиенту — что и делаем здесь.
	if c.resp.StatusCode != http.StatusServiceUnavailable {
		return false, nil
	}
	loadingBody, readErr := io.ReadAll(c.resp.Body)
	c.resp.Body.Close()
	if readErr != nil {
		loadingBody = []byte(fmt.Sprintf(`{"error":"upstream read error: %v"}`, readErr))
	}
	isLoading, loadingModelName, _ := loadingSignalFromBody(c.resp.StatusCode, loadingBody)
	if !isLoading {
		// 503 без признака loading — восстанавливаем body и идём по обычному пути.
		// respBody был прочитан в loadingBody выше, проксируем как upstream error.
		c.resp = &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       io.NopCloser(bytes.NewReader(loadingBody)),
			Header:     make(http.Header),
		}
		return false, nil
	}
	// Если из body не извлекли имя модели — используем из контекста.
	if loadingModelName == "" {
		loadingModelName = c.modelFromCtx
	}
	logger.Get().Infow("proxyRequestLlamaCpp: 503 'model is loading', polling for load completion",
		"backend", c.backendID, "model", loadingModelName,
		"attempt", c.llamaAttempt, "duration_ms", time.Since(c.llamaStartTime).Milliseconds())

	loaded, waitErr := c.p.waitForBackendModelLoaded(c.reqCtx, c.state.Backend, loadingModelName, c.backendID)
	if waitErr != nil {
		logger.Get().Warnw("proxyRequestLlamaCpp: waitForBackendModelLoaded error",
			"backend", c.backendID, "model", loadingModelName, "error", waitErr)
	}
	if !loaded {
		// Polling исчерпан — отдаём 503 клиенту с понятным сообщением.
		logger.Get().Warnw("proxyRequestLlamaCpp: load wait timeout, returning 503 to client",
			"backend", c.backendID, "model", loadingModelName)
		c.w.Header().Set("Content-Type", "application/json")
		c.w.Header().Set("Retry-After", "5")
		c.w.Header().Set("X-Model-Loading-Retry", "exhausted")
		errBody, _ := json.Marshal(map[string]interface{}{
			"error":        fmt.Sprintf("model is still loading after %ds: %s", c.p.loadingRetryBudgetSeconds(loadingModelName), loadingModelName),
			"loading":      true,
			"model":        loadingModelName,
			"retryAfterMs": 5000,
		})
		c.w.Header().Set("Content-Length", fmt.Sprintf("%d", len(errBody)))
		c.w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = c.w.Write(errBody)
		return true, nil
	}

	c.llamaAttempt++
	// Пересоздаём req: тело уже могло быть прочитано в io.ReadAll выше,
	// и Body указывает на закрытый reader. bytes.NewReader(translatedBody) снова.
	req2, reqErr := http.NewRequestWithContext(c.reqCtx, c.r.Method, c.fullURL, bytes.NewReader(c.translatedBody))
	if reqErr != nil {
		return false, fmt.Errorf("failed to create retry request: %v", reqErr)
	}
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Accept", "application/json, text/event-stream")
	for key, values := range c.r.Header {
		lowerKey := strings.ToLower(key)
		if lowerKey == "content-type" || lowerKey == "accept" || lowerKey == "content-length" || lowerKey == "host" {
			continue
		}
		for _, value := range values {
			req2.Header.Add(key, value)
		}
	}
	req2.Header.Set("X-Forwarded-For", c.clientRealIP)
	req2.Header.Set("X-Real-IP", c.clientRealIP)
	stampUpstreamRequestIDFromRequestR376(req2, c.r)
	// R307: административная ручка воркера закрыта токеном — ретрай тоже.
	applyWorkerAdminTokenR307(req2.Header, c.llamacppPath, c.p.workerAdminToken(c.backendID))
	resp, err := c.clientForReq.Do(req2)
	if err != nil {
		durationMs := time.Since(c.llamaStartTime).Milliseconds()
		errType := determineErrorType(err, c.reqCtx)
		logger.Get().Errorw("proxyRequestLlamaCpp: upstream Do() failed after loading retry",
			"backend", c.backendID, "model", c.modelFromCtx, "attempt", c.llamaAttempt,
			"duration_ms", durationMs, "error_type", errType, "error", err)
		return false, fmt.Errorf("llama.cpp request failed after load retry [backend=%s, attempt=%d, duration_ms=%d, error_type=%s]: %v",
			c.backendID, c.llamaAttempt, durationMs, errType, err)
	}
	c.resp = resp
	logger.Get().Infow("proxyRequestLlamaCpp: retry after load successful",
		"backend", c.backendID, "model", c.modelFromCtx,
		"attempt", c.llamaAttempt, "duration_ms", time.Since(c.llamaStartTime).Milliseconds())
	return false, nil
}

// writeStreamHeaders — заголовки успешного стримингового ответа. Обязаны быть
// выставлены строго ДО WriteHeader(200); здесь же возвращаются id генерации и
// время загрузки модели из заголовков воркера.
func (c *upstreamCall) writeStreamHeaders() (requestID string, workerLoadNs int64) {
	// R536 (R2 читаемости): заголовки ответа, которые обязаны уйти ДО
	// WriteHeader, выставляются одной функцией (порядок и состав прежние).
	applyStreamResponseHeaders(c.w, c.resp)
	// R360 (2026-10-31): id генерации на воркере. По нему адресная отмена
	// (cancelUpstreamGeneration): без него отмена по модели гасит ВСЕ генерации этой
	// модели на узле, включая чужие (аудит abort, A5).
	// R535 (R2 читаемости): id генерации и время загрузки — из заголовков.
	requestID, workerLoadNs = upstreamIdentityFromHeaders(c.resp)
	// R332.3 (2026-10-30, поймано живой матрицей): нативный стрим обязан
	// объявлять, КТО обслужил запрос.
	//
	// generic-путь выставляет X-Backend-ID/X-Session-ID (proxy_request.go), а этот
	// нативный passthrough — нет: клиент и монитор не узнавали бэкенд, и по
	// ответу /api/chat нельзя было понять, кто отвечал. Заголовки ставим строго
	// ДО WriteHeader (после него они уже не уйдут).
	c.w.Header().Set("X-Backend-ID", c.backendID)
	if sid := c.p.getSessionIDWithModel(c.r, c.p.getClientName(c.r), c.modelFromCtx); sid != "" {
		c.w.Header().Set("X-Session-ID", sid)
	}
	if c.originalPath == "/v1/chat/completions" {
		c.w.Header().Set("Content-Type", "text/event-stream")
	} else {
		c.w.Header().Set("Content-Type", "application/x-ndjson")
	}
	c.w.Header().Set("Connection", "keep-alive")
	c.w.WriteHeader(http.StatusOK)
	return requestID, workerLoadNs
}
