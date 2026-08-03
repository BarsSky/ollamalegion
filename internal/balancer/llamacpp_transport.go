package balancer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/pkg/logger"
)

// proxyRequestLlamaCpp — основной прокси с поддержкой streaming.
// Корректно работает с SSE от llama.cpp и транслирует в NDJSON Ollama.
//
// Ключевая особенность: кросс-чанковая детекция JSON tool calls из content.
// Некоторые модели (Gemma-4) выводят JSON tool calls как обычный текст в content,
// разбитый на множество SSE чанков. Этот прокси аккумулирует content из смежных
// чанков, детектирует полный JSON, и эмитит чистый NDJSON чанк с tool_calls.
func (p *Proxy) proxyRequestLlamaCpp(w http.ResponseWriter, r *http.Request, backendID string, bodyBuf []byte) error {
	state, ok := p.backends[backendID]
	if !ok {
		return fmt.Errorf("backend %s not found", backendID)
	}

	targetURL := p.getBackendBaseURL(state.Backend)
	modelFromCtx := ""
	if m, ok := r.Context().Value(modelContextKey).(string); ok {
		modelFromCtx = m
	}

	originalPath := r.URL.Path
	llamacppPath := translatePathForLlamaCpp(originalPath)
	isStreaming := isStreamingFromBody(originalPath, bodyBuf)

	translatedBody, err := translateOllamaBodyToOpenAI(originalPath, bodyBuf)
	if err != nil {
		translatedBody = bodyBuf
	}

	// F.0.4 (2026-06-28): session F — диагностика EOF.
	// Запоминаем startTime для расчёта duration_ms при ошибках upstream.
	llamaStartTime := time.Now()
	// EOF-retry counter (1 = первая попытка). Используется в error context и для
	// retry на тот же бэкенд после 500ms (типичный случай — cppworker перезагружал
	// модель ровно во время первого запроса).
	llamaAttempt := 1
	llamaEOFRetries := 0
	const maxLlamaEOFRetries = 1
	_ = llamaEOFRetries // используется ниже в EOF retry-блоке
	_ = llamaAttempt    // используется ниже в error context
	_ = llamaStartTime  // используется ниже в error context

	// PREFLIGHT n_ctx auto-reload: если клиент (OpenWebUI) задал options.num_ctx,
	// а loaded n_ctx на бэкенде меньше — синхронно перезагружаем модель на нужный n_ctx
	// ДО проксирования. Это критично для OpenWebUI с tools: первый запрос
	// получает reload один раз (10-30 сек), последующие — instant response.
	preflightBody, preflightOK, preflightMsg, preflightStatus := p.preflightNCtxReloadIfNeeded(
		r.Context(), backendID, modelFromCtx, bodyBuf)
	if preflightOK {
		bodyBuf = preflightBody // preflight удалил num_ctx из body (cppworker возьмёт new n_ctx)
		logger.Get().Debugw("proxyRequestLlamaCpp: preflight n_ctx reload applied",
			"backend", backendID, "model", modelFromCtx, "new_body_len", len(bodyBuf))
	} else if preflightMsg != "" || preflightStatus != http.StatusOK {
		// preflight вернул needsProxy=false: асинхронный reload запущен в фоне.
		// Отдаём клиенту HTTP 503 Service Unavailable + Retry-After: 5.
		// Клиент повторит запрос через 5 секунд — к этому моменту reload обычно завершён.
		// Стрим НЕ обрывается посередине.
		logger.Get().Infow("proxyRequestLlamaCpp: preflight triggered async n_ctx reload, returning 503 to client",
			"backend", backendID, "model", modelFromCtx,
			"msg", preflightMsg, "status", preflightStatus)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "5")
		w.Header().Set("X-NCtx-Reload-Decision", "async-reload")
		body := []byte(fmt.Sprintf(`{"error":%q,"decision":"async_reload","retry_after_seconds":5}`,
			preflightMsg))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write(body)
		return nil
	}

	fullURL := targetURL + llamacppPath
	logger.Get().Infow("proxyRequestLlamaCpp: sending request",
		"backend", backendID, "url", fullURL, "stream", isStreaming)

	client := p.client
	if isStreaming {
		client = p.streamingClient
	}

	reqCtx := r.Context()
	if isStreaming {
		// Per-model адаптивный таймаут для streaming inference.
		streamTimeout := p.getModelStreamTimeout(modelFromCtx)
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(r.Context(), streamTimeout)
		defer cancel()
		logger.Get().Debugw("proxyRequestLlamaCpp: using per-model stream timeout",
			"backend", backendID, "model", modelFromCtx,
			"stream_timeout_sec", streamTimeout.Seconds())
	}

	req, err := http.NewRequestWithContext(reqCtx, r.Method, fullURL, bytes.NewReader(translatedBody))
	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for key, values := range r.Header {
		lowerKey := strings.ToLower(key)
		if lowerKey == "content-type" || lowerKey == "accept" || lowerKey == "content-length" || lowerKey == "host" {
			continue
		}
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	clientRealIP := p.getClientRealIP(r)
	if existingXFF := r.Header.Get("X-Forwarded-For"); existingXFF != "" {
		req.Header.Set("X-Forwarded-For", existingXFF)
	} else {
		req.Header.Set("X-Forwarded-For", clientRealIP)
	}
	req.Header.Set("X-Real-IP", clientRealIP)

	// PF-5 FIX (2026-06-27): применяем FirstByteTimeout на уровне HTTP-Transport
	// через per-request клонирование streamingTransport. По умолчанию
	// ResponseHeaderTimeout=0 (off), что позволяет зависшему upstream держать
	// соединение бесконечно. С FirstByteTimeout>0 upstream, не отправивший
	// HTTP-заголовки за указанное время, получает от Go HTTP-клиента ошибку
	// "net/http: timeout awaiting response headers" — balancer транслирует её
	// в понятный клиенту ответ.
	//
	// Применяется ТОЛЬКО для streaming + когда задан явный FirstByteTimeout.
	// Для non-streaming RequestTimeout уже покрывается контекстом выше.
	clientForReq := client
	if isStreaming {
		firstByte := p.getModelFirstByteTimeout(modelFromCtx)
		if firstByte > 0 {
			clientForReq = p.newStreamingClientWithResponseHeaderTimeout(firstByte)
		}
	}

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
	// После записи заголовков (строка ~213 ниже) retry невозможен — TCP уже начат.
	resp, err := clientForReq.Do(req)
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
				"backend", backendID,
				"model", modelFromCtx,
				"attempt", llamaAttempt,
				"retry_idx", retryIdx+1,
				"max_retries", maxConnectionRetries,
				"backoff_ms", backoff.Milliseconds(),
				"error", err)
			select {
			case <-time.After(backoff):
			case <-reqCtx.Done():
				durationMs := time.Since(llamaStartTime).Milliseconds()
				return fmt.Errorf("llama.cpp request failed [backend=%s, attempt=%d, duration_ms=%d, error_type=%s, cancelled_during_backoff]: %v",
					backendID, llamaAttempt, durationMs, determineErrorType(err, reqCtx), err)
			}
			llamaAttempt++
			resp, err = clientForReq.Do(req)
			if err == nil {
				break // success
			}
			if !isConnectionLevelError(err) {
				break // error type changed, exit retry loop
			}
		}
	}
	if err != nil && llamaEOFRetries < maxLlamaEOFRetries {
		errType := determineErrorType(err, reqCtx)
		if errType == "unexpected_eof" {
			logger.Get().Warnw("proxyRequestLlamaCpp: EOF from upstream, retrying once on same backend",
				"backend", backendID, "model", modelFromCtx,
				"attempt", llamaAttempt, "error", err)
			// Публикуем EOF event в EventPublisher (если подключён).
			p.publishTransportEOF(backendID, modelFromCtx, originalPath, err, time.Since(llamaStartTime))
			// Задержка 500ms перед retry — типичное время reload-операций cppworker.
			time.Sleep(p.getStreamingRetryDelay())
			llamaEOFRetries++
			llamaAttempt++
			resp, err = clientForReq.Do(req)
		}
	}
	if err != nil {
		durationMs := time.Since(llamaStartTime).Milliseconds()
		errType := determineErrorType(err, reqCtx)
		logger.Get().Errorw("proxyRequestLlamaCpp: upstream Do() failed",
			"backend", backendID,
			"model", modelFromCtx,
			"url", fullURL,
			"attempt", llamaAttempt,
			"duration_ms", durationMs,
			"error_type", errType,
			"is_streaming", isStreaming,
			"error", err)
		// Публикуем EOF event (если это EOF и мы ещё не публиковали).
		if errType == "unexpected_eof" {
			p.publishTransportEOF(backendID, modelFromCtx, originalPath, err, time.Duration(durationMs)*time.Millisecond)
		}
		// Round 8 (2026-07-10): persistent connection_refused → mark backend unhealthy.
		if errType == "connection_refused" {
			p.markBackendConnectionFailed(backendID)
		}
		return fmt.Errorf("llama.cpp request failed [backend=%s, attempt=%d, duration_ms=%d, error_type=%s]: %v",
			backendID, llamaAttempt, durationMs, errType, err)
	}
	defer resp.Body.Close()

	// 2026-07-01: transparent lazy-load retry (см. loading_retry.go).
	//
	// cppworker при первом inference-запросе к незагруженной модели отвечает
	// HTTP 503 с body {"error":"model is loading: <name>","loading":true,...}.
	// Без retry OpenWebUI/Cline видел оборванный стрим. Теперь:
	//   1. Делаем 503 response body readable (нужно для анализа сигнала).
	//   2. Если 503 + loading — polling /api/models/load/progress до 30 сек.
	//   3. После успешного polling повторяем clientForReq.Do(req) с тем же body.
	// Retry возможен ТОЛЬКО до записи заголовков клиенту — что и делаем здесь.
	if resp.StatusCode == http.StatusServiceUnavailable {
		loadingBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			loadingBody = []byte(fmt.Sprintf(`{"error":"upstream read error: %v"}`, readErr))
		}
		isLoading, loadingModelName, _ := loadingSignalFromBody(resp.StatusCode, loadingBody)
		if isLoading {
			// Если из body не извлекли имя модели — используем из контекста.
			if loadingModelName == "" {
				loadingModelName = modelFromCtx
			}
			logger.Get().Infow("proxyRequestLlamaCpp: 503 'model is loading', polling for load completion",
				"backend", backendID, "model", loadingModelName,
				"attempt", llamaAttempt, "duration_ms", time.Since(llamaStartTime).Milliseconds())

			loaded, waitErr := p.waitForBackendModelLoaded(reqCtx, state.Backend, loadingModelName, backendID)
			if waitErr != nil {
				logger.Get().Warnw("proxyRequestLlamaCpp: waitForBackendModelLoaded error",
					"backend", backendID, "model", loadingModelName, "error", waitErr)
			}
			if loaded {
				llamaAttempt++
				// Пересоздаём req: тело уже могло быть прочитано в io.ReadAll выше,
				// и Body указывает на закрытый reader. bytes.NewReader(translatedBody) снова.
				req2, reqErr := http.NewRequestWithContext(reqCtx, r.Method, fullURL, bytes.NewReader(translatedBody))
				if reqErr != nil {
					return fmt.Errorf("failed to create retry request: %v", reqErr)
				}
				req2.Header.Set("Content-Type", "application/json")
				req2.Header.Set("Accept", "application/json, text/event-stream")
				for key, values := range r.Header {
					lowerKey := strings.ToLower(key)
					if lowerKey == "content-type" || lowerKey == "accept" || lowerKey == "content-length" || lowerKey == "host" {
						continue
					}
					for _, value := range values {
						req2.Header.Add(key, value)
					}
				}
				req2.Header.Set("X-Forwarded-For", clientRealIP)
				req2.Header.Set("X-Real-IP", clientRealIP)
				resp, err = clientForReq.Do(req2)
				if err != nil {
					durationMs := time.Since(llamaStartTime).Milliseconds()
					errType := determineErrorType(err, reqCtx)
					logger.Get().Errorw("proxyRequestLlamaCpp: upstream Do() failed after loading retry",
						"backend", backendID, "model", modelFromCtx, "attempt", llamaAttempt,
						"duration_ms", durationMs, "error_type", errType, "error", err)
					return fmt.Errorf("llama.cpp request failed after load retry [backend=%s, attempt=%d, duration_ms=%d, error_type=%s]: %v",
						backendID, llamaAttempt, durationMs, errType, err)
				}
				logger.Get().Infow("proxyRequestLlamaCpp: retry after load successful",
					"backend", backendID, "model", modelFromCtx,
					"attempt", llamaAttempt, "duration_ms", time.Since(llamaStartTime).Milliseconds())
			} else {
				// Polling исчерпан — отдаём 503 клиенту с понятным сообщением.
				logger.Get().Warnw("proxyRequestLlamaCpp: load wait timeout, returning 503 to client",
					"backend", backendID, "model", loadingModelName)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "5")
				w.Header().Set("X-Model-Loading-Retry", "exhausted")
				errBody, _ := json.Marshal(map[string]interface{}{
					"error":      fmt.Sprintf("model is still loading after %ds: %s", loadingRetryMaxAttempts*int(loadingRetryInterval.Seconds()), loadingModelName),
					"loading":    true,
					"model":      loadingModelName,
					"retryAfterMs": 5000,
				})
				w.Header().Set("Content-Length", fmt.Sprintf("%d", len(errBody)))
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write(errBody)
				return nil
			}
		} else {
			// 503 без признака loading — восстанавливаем body и идём по обычному пути.
			// respBody был прочитан в loadingBody выше, проксируем как upstream error.
			resp = &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Body:       io.NopCloser(bytes.NewReader(loadingBody)),
				Header:     make(http.Header),
			}
		}
	}

	// Если upstream вернул не-2xx — не начинаем streaming, а возвращаем
	// структурированную ошибку в формате, который клиент сможет прочитать.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			respBody = []byte(fmt.Sprintf(`{"error":"upstream read error: %v"}`, readErr))
		}
		logger.Get().Warnw("proxyRequestLlamaCpp: upstream returned non-2xx",
			"backend", backendID, "status", resp.StatusCode,
			"body", string(respBody)[:min(500, len(respBody))])

		// 2026-06-24: n_ctx auto-reload — если upstream вернул HTTP 413 / 400
		// с structured NCtxError (code=2 или code=3 + bridge_info), пытаемся
		// auto-reload модель на больший n_ctx и retry запрос. БЕЗ этого стрим
		// от OpenWebUI/Cline обрывается с error в SSE-чанке — клиент видит
		// "Server Connection Error" вместо нормального ответа после reload.
		//
		// Применяем тот же паттерн, что и в proxyRequestLlamaCppNonStream.
		// bodyBuf уже буферизован (передаётся []byte в сигнатуре), используем
		// его для retry после reload.
		if resp.StatusCode >= 400 && p.nctxReload != nil {
			if nctxErr := ParseCppWorkerError(respBody, resp.StatusCode, backendID); nctxErr != nil {
				if errors.Is(nctxErr, bridge.ErrNCtxNeedsReload) || errors.Is(nctxErr, bridge.ErrPromptTooLong) {
					logger.Get().Infow("proxyRequestLlamaCpp: detected n_ctx error from cppworker, invoking auto-reload (streaming retry)",
						"backend", backendID, "model", modelFromCtx,
						"status", resp.StatusCode,
						"is_n_ctx", errors.Is(nctxErr, bridge.ErrNCtxNeedsReload),
						"is_prompt_too_long", errors.Is(nctxErr, bridge.ErrPromptTooLong))
					p.handleNCtxReload(r.Context(), w, r, backendID, modelFromCtx, nctxErr, bodyBuf)
					return nil
				}
			}
		}

		if originalPath == "/v1/chat/completions" {
			w.Header().Set("X-Accel-Buffering", "no")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Connection", "keep-alive")
			w.WriteHeader(resp.StatusCode)
			fmt.Fprintf(w, "data: {\"error\":\"upstream returned HTTP %d\",\"choices\":[{\"delta\":{},\"finish_reason\":\"error\"}]}\n\n", resp.StatusCode)
			fmt.Fprintf(w, "data: [DONE]\n\n")
			// Flush на error path обязателен — иначе клиент получит FIN до [DONE]
			// и aiohttp / httpx вернут TransferEncodingError.
			Flush(w)
			return nil
		}

		w.Header().Set("X-Accel-Buffering", "no")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Connection", "keep-alive")
		// Round 18 P0.1: capabilities headers (X-Model-*)
		p.addModelCapabilitiesHeaders(w, modelFromCtx)
		w.WriteHeader(resp.StatusCode)
		errNDJSON := buildDoneResponse(originalPath, modelFromCtx, 0)
		fmt.Fprintf(w, "%s\n", string(errNDJSON))
		Flush(w)
		return nil
	}

	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Cache-Control", "no-cache")
	if originalPath == "/v1/chat/completions" {
		w.Header().Set("Content-Type", "text/event-stream")
	} else {
		w.Header().Set("Content-Type", "application/x-ndjson")
	}
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	toolAccum := make(map[int]*accumulatedToolCall)

	// ==== Кросс-чанковое накопление content для JSON tool call детекции =====
	// Некоторые модели (Gemma-4) выводят JSON tool calls как обычный текст в
	// content, разбитый на множество SSE чанков. Мы аккумулируем content из
	// смежных чанков, детектируем полный JSON, и эмитим чистый NDJSON чанк.
	var contentBuffer string
	var contentToolCallsProcessed bool

	// ==== Накопление plain content для финального done-чанка ====
	// cppworker отдаёт полный ответ в финальном NDJSON-чанке (`done:true`), но
	// balancer его подавляет (см. ниже hasFinishReason) и формирует свой done-чанк.
	// Чтобы клиент (Cline) не получил пустой `content`, аккумулируем content
	// из streaming чанков и используем его в финальном done-чанке.
	var accumulatedPlainContent string
	// ==== Content, извлечённый из done-чанка upstream (если cppworker
	// уже сформировал content в финальном NDJSON; используем его приоритетно). ====
	var upstreamDoneContent string

	// ==== Stream-truncation detection (2026-06-26) ====
	// Корневая причина бага из env_log.txt: cppworker обрывал стрим (broken pipe,
	// context cancel клиента, OOM kill, и т.п.), балансер проксировал только то,
	// что пришло до обрыва, и закрывал соединение без финального [DONE]/done-чанка.
	// Клиент (Cline/OpenWebUI/Roo Code) при этом считал стрим "успешно завершённым"
	// (TCP FIN == нормальный конец), хотя фактически получил обрезанный ответ.
	//
	// После фикса: явно отслеживаем, дошёл ли финальный маркер завершения стрима,
	// и если нет — логируем truncation и эмитим клиенту финальный NDJSON/SSE-чанк
	// с error/done:true, чтобы клиент корректно отобразил сбой вместо "успеха".
	streamCompleted := false
	upstreamHadFinishReason := false
	var bytesForwarded int64
	var firstForwardErr error
	var lastForwardErr error

	// ==== Основной цикл SSE → NDJSON / SSE → SSE ==========
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0), 1024*1024)

	lastData := ""

	for scanner.Scan() {
		line := scanner.Text()

		if !strings.HasPrefix(line, "data:") {
			continue
		}

		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			streamCompleted = true
			// Передаём upstreamHadFinishReason — если cppworker уже прислал чанк с
			// finish_reason (внутри for-цикла), translate записал финальный NDJSON
			// с done:true и content. writeStreamingSSEDone должен пропустить запись
			// дублирующего финального чанка, чтобы клиент не увидел два done:true.
			errFwd := writeStreamingSSEDone(w, originalPath, modelFromCtx, toolAccum,
				accumulatedPlainContent, upstreamDoneContent, upstreamHadFinishReason)
			if errFwd != nil {
				logger.Get().Warnw("proxyRequestLlamaCpp: writeStreamingSSEDone error",
					"backend", backendID, "error", errFwd)
				lastForwardErr = errFwd
				if firstForwardErr == nil {
					firstForwardErr = errFwd
				}
			}
			// writeStreamingSSEDone flush'ит только для SSE→SSE passthrough.
			// Для /api/chat и /api/generate (NDJSON) делаем явный flush —
			// иначе на быстрых моделях (Qwen3.6-35B-A3B) финальный done-чанк
			// может остаться в Go HTTP буфере и клиент получит EOF без последнего чанка.
			// Видно как TransferEncodingError на стороне aiohttp / httpx.
			if originalPath != "/v1/chat/completions" {
				Flush(w)
			}
			break
		}

		lastData = data

		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			logger.Get().Debugw("proxyRequestLlamaCpp: skipping non-JSON SSE data",
				"data", data, "error", err)
			continue
		}

		if modelFromCtx == "" {
			if m, ok := chunk["model"].(string); ok && m != "" {
				modelFromCtx = m
			}
		}

		// Для SSE→SSE passthrough (/v1/chat/completions) — проксируем как есть,
		// дополнительно накапливая delta.content на случай если клиент захочет
		// полный контент после [DONE].
		if originalPath == "/v1/chat/completions" {
			// Накапливаем plain content для /v1/chat/completions (на случай будущего использования).
			if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
				if choice, ok := choices[0].(map[string]interface{}); ok {
					if delta, ok := choice["delta"].(map[string]interface{}); ok {
						if c, ok := delta["content"].(string); ok && c != "" {
							accumulatedPlainContent += c
						}
					}
				}
			}
			// SSE → SSE passthrough
			if filtered, shouldSkip := filterOpenAIStreamingLine([]byte(data)); shouldSkip {
				continue
			} else if filtered != nil {
				n, errFwd := fmt.Fprintf(w, "data: %s\n\n", string(filtered))
				bytesForwarded += int64(n)
				if errFwd != nil {
					lastForwardErr = errFwd
					if firstForwardErr == nil {
						firstForwardErr = errFwd
					}
					return fmt.Errorf("write filtered SSE: %v", errFwd)
				}
			} else {
				if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
					if choice, ok := choices[0].(map[string]interface{}); ok {
						if delta, ok := choice["delta"].(map[string]interface{}); ok {
							accumulateToolCallsFromDelta(delta, toolAccum)
						}
					}
				}
				if modified := extractToolCallsFromSSEContent([]byte(data)); modified != nil {
					n, errFwd := fmt.Fprintf(w, "data: %s\n\n", string(modified))
					bytesForwarded += int64(n)
					if errFwd != nil {
						lastForwardErr = errFwd
						if firstForwardErr == nil {
							firstForwardErr = errFwd
						}
						return fmt.Errorf("write modified SSE (tool calls): %v", errFwd)
					}
				} else {
					n, errFwd := fmt.Fprintf(w, "data: %s\n\n", data)
					bytesForwarded += int64(n)
					if errFwd != nil {
						lastForwardErr = errFwd
						if firstForwardErr == nil {
							firstForwardErr = errFwd
						}
						return fmt.Errorf("write SSE: %v", errFwd)
					}
				}
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		} else {
			// ==== SSE → NDJSON: для /api/generate и /api/chat =====
			var deltaContent string
			if originalPath == "/api/chat" {
				if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
					if choice, ok := choices[0].(map[string]interface{}); ok {
						if delta, ok := choice["delta"].(map[string]interface{}); ok {
							accumulateToolCallsFromDelta(delta, toolAccum)
							if c, ok := delta["content"].(string); ok {
								deltaContent = c
							}
						}
					}
				}

				// Если чанк содержит finish_reason (done:true) — извлекаем content и
				// done_reason из message, чтобы сохранить полный ответ cppworker'а.
				if upstreamHasDone, msgMap, _ := extractUpstreamDoneChunk(data); upstreamHasDone {
					if c, ok := msgMap["content"].(string); ok {
						upstreamDoneContent = c
					}
				}

				// Кросс-чанковое накопление content для детекции JSON tool calls.
				// Поддерживаемые триггеры накопления:
				//   - "[" — стандартный JSON-массив tool_calls
				//   - "<tool_call>" / "<|python_tag|>" — Hermes/Qwen/Llama-3 форматы
				//   - "{name" — начало одиночного JSON-объекта tool_call
				//   - contentBuffer не пуст — продолжаем накапливать уже начатый tool call
				if deltaContent != "" && !contentToolCallsProcessed {
					trimmed := strings.TrimSpace(deltaContent)
					startToolCallAccum := strings.HasPrefix(trimmed, "[") ||
						strings.HasPrefix(trimmed, "<tool_call>") ||
						strings.HasPrefix(trimmed, "<|python_tag|>") ||
						strings.HasPrefix(trimmed, "[TOOL_CALLS]") ||
						strings.HasPrefix(trimmed, "{name")
					if startToolCallAccum || contentBuffer != "" {
						contentBuffer += deltaContent

						// Кросс-чанковая склейка может оставить "лишний" символ '}' перед
						// закрывающим </tool_call> (когда JSON закрывается в одном чанке, а
						// '}'</tool_call>' приходит в следующем). Схлопываем дубликат.
						contentBuffer = collapseDuplicateClosingBrace(contentBuffer)

						// Проверяем, не накопился ли полный JSON tool call
						if detectedTC, remainingTC, found := detectAndExtractToolCallsFromContent(contentBuffer); found {
							contentToolCallsProcessed = true
							contentBuffer = ""
							logger.Get().Infow("proxyRequestLlamaCpp: cross-chunk tool_calls detected in content",
								"backend", backendID, "model", modelFromCtx,
								"tool_calls_count", len(detectedTC))

							cleanRemaining := stripServiceTokens(remainingTC)
							cleanRemaining = cleanContentAfterToolCallExtraction(cleanRemaining)

							msgMap := map[string]interface{}{
								"role":       "assistant",
								"content":    cleanRemaining,
								"tool_calls": detectedTC,
							}
							ollamaChunk := map[string]interface{}{
								"model":      modelFromCtx,
								"created_at": time.Now().UTC().Format(time.RFC3339),
								"done":       false,
								"message":    msgMap,
							}
							out, _ := json.Marshal(ollamaChunk)
							n, errFwd := fmt.Fprintf(w, "%s\n", string(out))
							bytesForwarded += int64(n)
							if errFwd != nil {
								lastForwardErr = errFwd
								if firstForwardErr == nil {
									firstForwardErr = errFwd
								}
								return fmt.Errorf("write NDJSON (cross-chunk tool calls): %v", errFwd)
							}
							if flusher, ok := w.(http.Flusher); ok {
								flusher.Flush()
							}
							continue
						}

						// Если JSON массив закрыт (найден ']' на правильной глубине),
						// но tool call не обнаружен — это не tool call JSON.
						// Сбрасываем буфер и даём чанку пройти через нормальный translate.
						if endPos := findMatchingClosingBracket(contentBuffer, 0); endPos >= 0 {
							contentBuffer = ""
						} else if len(contentBuffer) > 4096 {
							// Слишком большой буфер без закрывающей ']' — не tool call.
							contentBuffer = ""
						} else {
							// Всё ещё накапливаем — пропускаем этот чанк
							continue
						}
					}
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
				if deltaContent != "" && !contentToolCallsProcessed && !hasFinishReason(data) {
					accumulatedPlainContent += deltaContent
				}

				// Финальный чанк cppworker (с finish_reason) присылает ПОЛНЫЙ content
				// в message.content — используем его как upstreamDoneContent. Это
				// перезаписывает накопленный accumulatedPlainContent чтобы избежать
				// дубликата ("Hello from llama.cpp!" + "Hello from llama.cpp!").
				if upstreamHasDone, msgMap, _ := extractUpstreamDoneChunk(data); upstreamHasDone {
					if c, ok := msgMap["content"].(string); ok && c != "" {
						upstreamDoneContent = c
					}
				}
			} else if originalPath == "/api/generate" {
				// Для /api/generate content лежит в chunk["choices"][0].text или в done_chunk.response
				if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
					if choice, ok := choices[0].(map[string]interface{}); ok {
						if t, ok := choice["text"].(string); ok && t != "" {
							deltaContent = t
							// НЕ накапливаем чанки с finish_reason (аналогично /api/chat):
							// cppworker в финальном чанке шлёт ПОЛНЫЙ content=" llama.cpp!",
							// а accumulatedPlainContent уже содержит "Hello from". Без guard
							// финальный done-чанк получал дубликат ("Hello from llama.cpp! llama.cpp!").
							if !hasFinishReason(data) {
								accumulatedPlainContent += t
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
								upstreamDoneContent = accumulatedPlainContent + t
							}
						}
					}
					if upstreamDoneContent == "" {
						upstreamDoneContent = accumulatedPlainContent
					}
				}
			}

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
			if contentToolCallsProcessed || len(toolAccum) > 0 {
				// tool_calls уже были обработаны выше — writeStreamingSSEDone возьмёт
				// их из toolAccum и отправит финальный NDJSON с tool_calls.
				// upstreamHadFinishReason=false: даже если cppworker прислал finish_reason
				// в этом же чанке, tool_calls должны быть эмитированы (предыдущая ветка
				// кода обработала cross-chunk tool calls в отдельном NDJSON-чанке).
				if !streamCompleted {
					streamCompleted = true
					errFwd := writeStreamingSSEDone(w, originalPath, modelFromCtx, toolAccum,
						accumulatedPlainContent, upstreamDoneContent, false)
					if errFwd != nil {
						logger.Get().Warnw("proxyRequestLlamaCpp: writeStreamingSSEDone on tool_calls error",
							"backend", backendID, "error", errFwd)
						lastForwardErr = errFwd
						if firstForwardErr == nil {
							firstForwardErr = errFwd
						}
					}
					Flush(w)
				}
				continue
			}
			if hasFinishReason(data) && !streamCompleted {
				// cppworker закрывает TCP после этого чанка — отмечаем что стрим
				// завершён нормально, чтобы ниже не сработал stream-truncation detection.
				streamCompleted = true
				// PF-6 (2026-06-27) + dedup streaming fix (2026-06-30):
				// translateOpenAISSEDataToOllama сам формирует финальный NDJSON с done:true
				// и content из финального чанка. Если writeStreamingSSEDone тоже запишет
				// финальный done-чанк (например, через [DONE] маркер от SSE upstream)
				// клиент увидит ДУБЛИКАТ done-чанка с тем же content. Чтобы этого избежать
				// передаём флаг upstreamSentFinishReason — writeStreamingSSEDone пропустит
				// финальный NDJSON/SSE если upstream уже отправил finish_reason и нет tool_calls.
				upstreamHadFinishReason = true
			}
			ollamaChunk := translateOpenAISSEDataToOllama(originalPath, []byte(data), modelFromCtx)
			if len(ollamaChunk) == 0 {
				continue
			}
			n, errFwd := w.Write(ollamaChunk)
			bytesForwarded += int64(n)
			if errFwd != nil {
				lastForwardErr = errFwd
				if firstForwardErr == nil {
					firstForwardErr = errFwd
				}
				return fmt.Errorf("write NDJSON: %v", errFwd)
			}

			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	}

	// ==== Stream-truncation detection (2026-06-26) ====
	// Если мы вышли из цикла for без streamCompleted (т.е. cppworker оборвал
	// стрим до того, как прислал финальный [DONE] маркер), явно логируем это
	// и эмитим клиенту финальный чанк с error/done:true, чтобы клиент
	// (Cline/OpenWebUI/Roo Code) корректно отобразил сбой вместо "успешного
	// ответа" с обрезанным content.
	//
	// Сценарии:
	//   1. scanner.Err() != nil → broken pipe / context canceled / timeout
	//   2. scanner.Err() == nil && streamCompleted == false → cppworker
	//      закрыл TCP без [DONE] (редкий случай, обычно после ошибки модели).
	//
	// В обоих случаях клиент уже получил HTTP 200 + Content-Type, поэтому
	// поменять HTTP-статус нельзя. Лучшее, что мы можем — дописать в стрим
	// финальный маркер и закрыть соединение, чтобы клиент не висел.
	if !streamCompleted {
		scannerErr := scanner.Err()
		logger.Get().Warnw("proxyRequestLlamaCpp: SSE stream truncated before [DONE] marker",
			"backend", backendID, "model", modelFromCtx,
			"original_path", originalPath,
			"bytes_forwarded", bytesForwarded,
			"scanner_err", errString(scannerErr),
			"first_forward_err", errString(firstForwardErr),
			"last_forward_err", errString(lastForwardErr))

		// Эмитим финальный чанк с error, чтобы клиент точно завершил стрим.
		// Пробуем дописать; если клиент уже отвалился (broken pipe) — игнорируем ошибку.
		if originalPath == "/v1/chat/completions" {
			truncatedChunk := map[string]interface{}{
				"id":      fmt.Sprintf("trunc-%d", time.Now().UnixNano()),
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   modelFromCtx,
				"choices": []interface{}{
					map[string]interface{}{
						"index":         0,
						"delta":         map[string]interface{}{},
						"finish_reason": "truncated",
					},
				},
				"error": map[string]interface{}{
					"message": "stream truncated by upstream before completion",
					"type":    "stream_truncated",
				},
			}
			out, _ := json.Marshal(truncatedChunk)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", string(out))
			_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
			Flush(w)
		} else {
			// Для /api/chat и /api/generate (NDJSON): финальный done-чанк с error.
			truncatedChunk := map[string]interface{}{
				"model":      modelFromCtx,
				"created_at": time.Now().UTC().Format(time.RFC3339),
				"done":       true,
				"done_reason": "truncated",
				"error":      "stream truncated by upstream before completion",
			}
			out, _ := json.Marshal(truncatedChunk)
			_, _ = fmt.Fprintf(w, "%s\n", string(out))
			Flush(w)
		}
	}

	if err := scanner.Err(); err != nil {
		logger.Get().Errorw("proxyRequestLlamaCpp: scanner error", "error", err, "backend", backendID)
	}

	if p.nctxReload != nil {
		p.nctxReload.ResetCycleCounter(backendID)
	}

	if lastData == "" && originalPath != "/v1/chat/completions" {
		done := buildDoneResponse(originalPath, modelFromCtx, 0)
		_, errFwd := fmt.Fprintf(w, "%s\n", string(done))
		if errFwd != nil {
			return fmt.Errorf("write empty-done NDJSON: %v", errFwd)
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}

	return nil
}

// errString — безопасно возвращает string(err) даже если err == nil.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// hasFinishReason — проверяет, содержит ли SSE data-чанк finish_reason (done:true).
// Используется для подавления дублирующего done-чанка от translate.
func hasFinishReason(data string) bool {
	if len(data) == 0 {
		return false
	}
	var chunk map[string]interface{}
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return false
	}
	choices, ok := chunk["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return false
	}
	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		return false
	}
	fr, ok := choice["finish_reason"].(string)
	if !ok {
		return false
	}
	return fr != "" && fr != "null"
}

// extractUpstreamDoneChunk — извлекает content и done_reason из OpenAI SSE-чанка
// с finish_reason (например, финального чанка cppworker'а для /api/chat).
// Возвращает (true, msgMap, doneReason), если чанк содержит finish_reason.
func extractUpstreamDoneChunk(data string) (bool, map[string]interface{}, string) {
	var chunk map[string]interface{}
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return false, nil, ""
	}
	choices, ok := chunk["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return false, nil, ""
	}
	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		return false, nil, ""
	}
	fr, ok := choice["finish_reason"].(string)
	if !ok || fr == "" || fr == "null" {
		return false, nil, ""
	}
	msg, _ := choice["message"].(map[string]interface{})
	if msg == nil {
		// Иногда content лежит в delta
		if delta, ok := choice["delta"].(map[string]interface{}); ok {
			msg = map[string]interface{}{}
			if c, ok := delta["content"].(string); ok {
				msg["content"] = c
			}
		}
	}
	return true, msg, fr
}

// extractUpstreamGenerateDoneChunk — для /api/generate cppworker возвращает
// финальный чанк с finish_reason. Накапливает text (если есть) в plainContent.
// Возвращает true, если чанк содержит finish_reason.
func extractUpstreamGenerateDoneChunk(data string, plainContent *string) bool {
	if plainContent == nil {
		return false
	}
	var chunk map[string]interface{}
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return false
	}
	choices, ok := chunk["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return false
	}
	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		return false
	}
	fr, ok := choice["finish_reason"].(string)
	if !ok || fr == "" || fr == "null" {
		return false
	}
	if t, ok := choice["text"].(string); ok && t != "" {
		*plainContent += t
	}
	return true
}