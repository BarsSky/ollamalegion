package balancer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/pkg/logger"
)

// ---------- Inference endpoints ----------

// handleOpenAIChatCompletions — проксирует /v1/chat/completions к llama.cpp бэкенду.
// Этот endpoint вызывают OpenAI-совместимые клиенты: Roo Code, Cline, Continue.dev,
// и иногда OpenWebUI при выборе OpenAI-совместимого режима.
func (lr *LlamaCppRouter) handleOpenAIChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var bodyBuf []byte
	if r.Body != nil {
		var err error
		bodyBuf, err = io.ReadAll(r.Body)
		if err != nil {
			logger.Get().Errorw("handleOpenAIChatCompletions: failed to read body", "error", err)
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(bodyBuf))
	}

	var req map[string]interface{}
	if err := json.Unmarshal(bodyBuf, &req); err != nil {
		logger.Get().Errorw("handleOpenAIChatCompletions: failed to parse body", "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	model, _ := req["model"].(string)

	if normalized := normalizeOpenAIBody(bodyBuf); len(normalized) > 0 {
		bodyBuf = normalized
	}

	backendID := lr.findModelOnLlamaCppBackend(model)
	if backendID == "" {
		backendID = lr.selectAnyLlamaCppHealthy()
	}
	if backendID == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no llama.cpp backend available"})
		return
	}

	state, ok := lr.proxy.backends[backendID]
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "backend state not found"})
		return
	}

	// Preflight n_ctx check.
	if lr.runInferencePreflight(runInferencePreflightArgs{
		w:          w,
		r:          r,
		body:       bodyBuf,
		model:      model,
		backendID:  backendID,
		backendURL: lr.proxy.backendHTTPAddrByID(backendID),
	}) {
		return
	}

	// Auto-load.
	if model != "" {
		loadOpts := warmupOptions{}
		resolved := lr.proxy.ResolveNumCtx(model, bodyBuf, backendID)
		if resolved.Value > 0 {
			loadedNCtx := lr.proxy.getLoadedNCtxFromMetrics(backendID, model)
			if loadedNCtx > resolved.Value {
				logger.Get().Infow("handleChat: keeping loaded n_ctx (not lowering)",
					"model", model, "backend", backendID,
					"loaded_n_ctx", loadedNCtx, "requested_n_ctx", resolved.Value,
					"source", resolved.Source)
				loadOpts.NumCtx = loadedNCtx
			} else {
				loadOpts.NumCtx = resolved.Value
			}
		}
		if _, loadErr := lr.ensureModelLoadedOnBackend(backendID, model, loadOpts); loadErr != nil {
			logger.Get().Errorw("handleOpenAIChatCompletions: auto-load failed",
				"backend", backendID, "model", model, "error", loadErr)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"error": fmt.Sprintf("model '%s' is not loaded and auto-load failed: %v", model, loadErr),
			})
			return
		}
	}

	r.Body = io.NopCloser(bytes.NewReader(bodyBuf))

	resolved := lr.proxy.ApplyCppCtxHeader(r, model, bodyBuf, backendID)
	if resolved.Value > 0 {
		logger.Get().Debugw("handleOpenAIChatCompletions: 3-tier resolver applied num_ctx override",
			"model", model, "backend", backendID,
			"resolved_n_ctx", resolved.Value, "source", resolved.Source)
	}

	targetURL := fmt.Sprintf("http://%s:%d/v1/chat/completions", state.Backend.Host, lr.proxy.getBackendPort(state.Backend))

	logger.Get().Infow("handleOpenAIChatCompletions: proxying to cppworker",
		"backend", backendID, "url", targetURL, "model", model)

	// PF-5 fix (2026-06-27): используем per-request client с ResponseHeaderTimeout = FirstByteTimeout.
	// Иначе upstream, не отправивший HTTP-заголовки за указанное время, держит соединение бесконечно.
	firstByte := lr.proxy.getModelFirstByteTimeout(model)
	clientForReq := lr.proxy.streamingClient
	if firstByte > 0 {
		clientForReq = lr.proxy.newStreamingClientWithResponseHeaderTimeout(firstByte)
	}

	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, targetURL, bytes.NewReader(bodyBuf))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	upstreamReq.Header.Set("Content-Type", "application/json")
	upstreamReq.Header.Set("Accept", "application/json, text/event-stream")

	clientRealIP := lr.proxy.getClientRealIP(r)
	if existingXFF := r.Header.Get("X-Forwarded-For"); existingXFF != "" {
		upstreamReq.Header.Set("X-Forwarded-For", existingXFF)
	} else {
		upstreamReq.Header.Set("X-Forwarded-For", clientRealIP)
	}
	upstreamReq.Header.Set("X-Real-IP", clientRealIP)

	upstreamResp, err := clientForReq.Do(upstreamReq)
	if err != nil {
		errType := determineErrorType(err, r.Context())
		logger.Get().Errorw("handleOpenAIChatCompletions: upstream request failed",
			"backend", backendID, "error", err, "error_type", errType)
		if errType == "timeout" || errType == "unexpected_eof" || errType == "connection_reset_by_peer" || errType == "broken_pipe" {
			logger.Get().Infow("handleOpenAIChatCompletions: retrying once after network/header error",
				"backend", backendID, "model", model, "error_type", errType)
			time.Sleep(lr.proxy.getStreamingRetryDelay())
			r.Body = io.NopCloser(bytes.NewReader(bodyBuf))
			retryReq, retryErr := http.NewRequestWithContext(r.Context(), http.MethodPost, targetURL, bytes.NewReader(bodyBuf))
			if retryErr == nil {
				retryReq.Header.Set("Content-Type", "application/json")
				retryReq.Header.Set("Accept", "application/json, text/event-stream")
				if existingXFF := r.Header.Get("X-Forwarded-For"); existingXFF != "" {
					retryReq.Header.Set("X-Forwarded-For", existingXFF)
				} else {
					retryReq.Header.Set("X-Forwarded-For", lr.proxy.getClientRealIP(r))
				}
				retryReq.Header.Set("X-Real-IP", lr.proxy.getClientRealIP(r))
				upstreamResp, err = clientForReq.Do(retryReq)
				if err == nil {
					logger.Get().Infow("handleOpenAIChatCompletions: retry succeeded",
						"backend", backendID, "model", model)
				}
			}
		}
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
	}

	// n_ctx auto-reload: перехват структурированной ошибки от cppworker.
	if upstreamResp.StatusCode >= 400 {
		if peekBody, peekErr := io.ReadAll(upstreamResp.Body); peekErr == nil {
			_ = upstreamResp.Body.Close()
			if nctxErr := ParseCppWorkerError(peekBody, upstreamResp.StatusCode, backendID); nctxErr != nil {
				if errors.Is(nctxErr, bridge.ErrNCtxNeedsReload) || errors.Is(nctxErr, bridge.ErrPromptTooLong) {
					logger.Get().Infow("handleOpenAIChatCompletions: detected n_ctx error from cppworker, invoking auto-reload",
						"backend", backendID, "model", model,
						"status", upstreamResp.StatusCode)
					lr.proxy.handleNCtxReload(r.Context(), w, r, backendID, model, nctxErr, bodyBuf)
					return
				}
			}
			upstreamResp.Body = io.NopCloser(bytes.NewReader(peekBody))
		}
	}

	_ = lr.proxy.proxyRequestOpenAIStreaming(w, r, upstreamResp, backendID)
}

// handleOpenAICompletion — проксирует /v1/completions (legacy text completion) к llama.cpp.
//
// Round 15.1 follow-up (2026-07-29): до этого /v1/completions шёл через main
// proxy flow → queue manager → headroom check блокировал при VRAM > 85%.
// Структура идентична handleOpenAIChatCompletions, отличие только в upstream
// URL (вместо /v1/chat/completions → /v1/completions). Делаем refactor позже
// если появится третий endpoint с тем же flow — пока проще держать две функции
// чтобы grep'ать handler-vs-call-site связи.
func (lr *LlamaCppRouter) handleOpenAICompletion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var bodyBuf []byte
	if r.Body != nil {
		var err error
		bodyBuf, err = io.ReadAll(r.Body)
		if err != nil {
			logger.Get().Errorw("handleOpenAICompletion: failed to read body", "error", err)
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(bodyBuf))
	}

	var req map[string]interface{}
	if err := json.Unmarshal(bodyBuf, &req); err != nil {
		logger.Get().Errorw("handleOpenAICompletion: failed to parse body", "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	model, _ := req["model"].(string)

	backendID := lr.findModelOnLlamaCppBackend(model)
	if backendID == "" {
		backendID = lr.selectAnyLlamaCppHealthy()
	}
	if backendID == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no llama.cpp backend available"})
		return
	}

	state, ok := lr.proxy.backends[backendID]
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "backend state not found"})
		return
	}

	// Preflight n_ctx check.
	if lr.runInferencePreflight(runInferencePreflightArgs{
		w:          w,
		r:          r,
		body:       bodyBuf,
		model:      model,
		backendID:  backendID,
		backendURL: lr.proxy.backendHTTPAddrByID(backendID),
	}) {
		return
	}

	// Auto-load.
	if model != "" {
		loadOpts := warmupOptions{}
		resolved := lr.proxy.ResolveNumCtx(model, bodyBuf, backendID)
		if resolved.Value > 0 {
			loadedNCtx := lr.proxy.getLoadedNCtxFromMetrics(backendID, model)
			if loadedNCtx > resolved.Value {
				logger.Get().Infow("handleOpenAICompletion: keeping loaded n_ctx (not lowering)",
					"model", model, "backend", backendID,
					"loaded_n_ctx", loadedNCtx, "requested_n_ctx", resolved.Value,
					"source", resolved.Source)
				loadOpts.NumCtx = loadedNCtx
			} else {
				loadOpts.NumCtx = resolved.Value
			}
		}
		if _, loadErr := lr.ensureModelLoadedOnBackend(backendID, model, loadOpts); loadErr != nil {
			logger.Get().Errorw("handleOpenAICompletion: auto-load failed",
				"backend", backendID, "model", model, "error", loadErr)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"error": fmt.Sprintf("model '%s' is not loaded and auto-load failed: %v", model, loadErr),
			})
			return
		}
	}

	r.Body = io.NopCloser(bytes.NewReader(bodyBuf))

	resolved := lr.proxy.ApplyCppCtxHeader(r, model, bodyBuf, backendID)
	if resolved.Value > 0 {
		logger.Get().Debugw("handleOpenAICompletion: 3-tier resolver applied num_ctx override",
			"model", model, "backend", backendID,
			"resolved_n_ctx", resolved.Value, "source", resolved.Source)
	}

	targetURL := fmt.Sprintf("http://%s:%d/v1/completions", state.Backend.Host, lr.proxy.getBackendPort(state.Backend))

	logger.Get().Infow("handleOpenAICompletion: proxying to cppworker",
		"backend", backendID, "url", targetURL, "model", model)

	// PF-5 fix (2026-06-27): per-request client с ResponseHeaderTimeout = FirstByteTimeout.
	firstByte := lr.proxy.getModelFirstByteTimeout(model)
	clientForReq := lr.proxy.streamingClient
	if firstByte > 0 {
		clientForReq = lr.proxy.newStreamingClientWithResponseHeaderTimeout(firstByte)
	}

	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, targetURL, bytes.NewReader(bodyBuf))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	upstreamReq.Header.Set("Content-Type", "application/json")
	upstreamReq.Header.Set("Accept", "application/json, text/event-stream")

	clientRealIP := lr.proxy.getClientRealIP(r)
	if existingXFF := r.Header.Get("X-Forwarded-For"); existingXFF != "" {
		upstreamReq.Header.Set("X-Forwarded-For", existingXFF)
	} else {
		upstreamReq.Header.Set("X-Forwarded-For", clientRealIP)
	}
	upstreamReq.Header.Set("X-Real-IP", clientRealIP)

	upstreamResp, err := clientForReq.Do(upstreamReq)
	if err != nil {
		errType := determineErrorType(err, r.Context())
		logger.Get().Errorw("handleOpenAICompletion: upstream request failed",
			"backend", backendID, "error", err, "error_type", errType)
		if errType == "timeout" || errType == "unexpected_eof" || errType == "connection_reset_by_peer" || errType == "broken_pipe" {
			logger.Get().Infow("handleOpenAICompletion: retrying once after network/header error",
				"backend", backendID, "model", model, "error_type", errType)
			time.Sleep(lr.proxy.getStreamingRetryDelay())
			r.Body = io.NopCloser(bytes.NewReader(bodyBuf))
			retryReq, retryErr := http.NewRequestWithContext(r.Context(), http.MethodPost, targetURL, bytes.NewReader(bodyBuf))
			if retryErr == nil {
				retryReq.Header.Set("Content-Type", "application/json")
				retryReq.Header.Set("Accept", "application/json, text/event-stream")
				if existingXFF := r.Header.Get("X-Forwarded-For"); existingXFF != "" {
					retryReq.Header.Set("X-Forwarded-For", existingXFF)
				} else {
					retryReq.Header.Set("X-Forwarded-For", lr.proxy.getClientRealIP(r))
				}
				retryReq.Header.Set("X-Real-IP", lr.proxy.getClientRealIP(r))
				upstreamResp, err = clientForReq.Do(retryReq)
				if err == nil {
					logger.Get().Infow("handleOpenAICompletion: retry succeeded",
						"backend", backendID, "model", model)
				}
			}
		}
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
	}

	// n_ctx auto-reload: перехват структурированной ошибки от cppworker.
	if upstreamResp.StatusCode >= 400 {
		if peekBody, peekErr := io.ReadAll(upstreamResp.Body); peekErr == nil {
			_ = upstreamResp.Body.Close()
			if nctxErr := ParseCppWorkerError(peekBody, upstreamResp.StatusCode, backendID); nctxErr != nil {
				if errors.Is(nctxErr, bridge.ErrNCtxNeedsReload) || errors.Is(nctxErr, bridge.ErrPromptTooLong) {
					logger.Get().Infow("handleOpenAICompletion: detected n_ctx error from cppworker, invoking auto-reload",
						"backend", backendID, "model", model,
						"status", upstreamResp.StatusCode)
					lr.proxy.handleNCtxReload(r.Context(), w, r, backendID, model, nctxErr, bodyBuf)
					return
				}
			}
			upstreamResp.Body = io.NopCloser(bytes.NewReader(peekBody))
		}
	}

	_ = lr.proxy.proxyRequestOpenAIStreaming(w, r, upstreamResp, backendID)
}

// handleChat — проксирует /api/chat запросы к llama.cpp бэкендам.
func (lr *LlamaCppRouter) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var bodyBuf []byte
	if r.Body != nil {
		var err error
		bodyBuf, err = io.ReadAll(r.Body)
		if err != nil {
			logger.Get().Errorw("handleChat: failed to read body", "error", err)
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(bodyBuf))
	}

	var reqMap map[string]interface{}
	if err := json.Unmarshal(bodyBuf, &reqMap); err != nil {
		logger.Get().Errorw("handleChat: failed to parse body JSON", "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	model, _ := reqMap["model"].(string)
	if model == "" {
		logger.Get().Warnw("handleChat: empty model in request body")
	}

	if normalized := normalizeOpenAIBody(bodyBuf); len(normalized) > 0 {
		bodyBuf = normalized
	}

	backendID := lr.findModelOnLlamaCppBackend(model)
	if backendID == "" {
		backendID = lr.selectAnyLlamaCppHealthy()
	}
	if backendID == "" {
		logger.Get().Errorw("handleChat: no llama.cpp backend available",
			"model", model)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no llama.cpp backend available"})
		return
	}
	logger.Get().Debugw("handleChat: selected backend",
		"model", model, "backend", backendID)
	r.Body = io.NopCloser(bytes.NewReader(bodyBuf))

	// Preflight n_ctx check.
	if lr.runInferencePreflight(runInferencePreflightArgs{
		w:          w,
		r:          r,
		body:       bodyBuf,
		model:      model,
		backendID:  backendID,
		backendURL: lr.proxy.backendHTTPAddrByID(backendID),
	}) {
		return
	}

	// Auto-load.
	loadOpts := warmupOptions{}
	resolvedLoad := lr.proxy.ResolveNumCtx(model, bodyBuf, backendID)
	if resolvedLoad.Value > 0 {
		loadOpts.NumCtx = resolvedLoad.Value
	}
	if _, loadErr := lr.ensureModelLoadedOnBackend(backendID, model, loadOpts); loadErr != nil {
		logger.Get().Errorw("handleChat: auto-load failed",
			"backend", backendID, "model", model, "error", loadErr)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": fmt.Sprintf("model '%s' is not loaded and auto-load failed: %v", model, loadErr),
		})
		return
	}

	// Update LastKnownNCtx after model load.
	if loadedNCtx := lr.proxy.getLoadedNCtxFromMetrics(backendID, model); loadedNCtx > 0 {
		lr.proxy.nctxReload.SetLastKnownNCtx(backendID, loadedNCtx)
		logger.Get().Debugw("handleChat: updated LastKnownNCtx",
			"backend", backendID, "model", model, "loaded_n_ctx", loadedNCtx)
	}

	resolved := lr.proxy.ApplyCppCtxHeader(r, model, bodyBuf, backendID)
	if resolved.Value > 0 {
		logger.Get().Debugw("handleChat: 3-tier resolver applied num_ctx override",
			"model", model, "backend", backendID,
			"resolved_n_ctx", resolved.Value, "source", resolved.Source)
	}

	var err error
	if isStreamingFromBody(r.URL.Path, bodyBuf) {
		logger.Get().Debugw("handleChat: streaming mode", "backend", backendID)
		err = lr.proxy.proxyRequestLlamaCpp(w, r, backendID, bodyBuf)
	} else {
		logger.Get().Debugw("handleChat: non-streaming mode", "backend", backendID)
		err = lr.proxy.proxyRequestLlamaCppNonStream(w, r, backendID, bodyBuf)
	}
	if err != nil {
		logger.Get().Errorw("handleChat: proxy failed", "backend", backendID, "error", err)
		if !isHeadersSent(w) {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		}
	}
}

// handleGenerate — проксирует /api/generate запросы к llama.cpp бэкендам.
func (lr *LlamaCppRouter) handleGenerate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var bodyBuf []byte
	if r.Body != nil {
		var err error
		bodyBuf, err = io.ReadAll(r.Body)
		if err != nil {
			logger.Get().Errorw("handleGenerate: failed to read body", "error", err)
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(bodyBuf))
	}

	var reqMap map[string]interface{}
	if err := json.Unmarshal(bodyBuf, &reqMap); err != nil {
		logger.Get().Errorw("handleGenerate: failed to parse body JSON", "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	model, _ := reqMap["model"].(string)
	logger.Get().Infow("handleGenerate: parsed request",
		"model", model, "body_len", len(bodyBuf))

	if normalized := normalizeOpenAIBody(bodyBuf); len(normalized) > 0 {
		bodyBuf = normalized
	}

	backendID := lr.findModelOnLlamaCppBackend(model)
	if backendID == "" {
		backendID = lr.selectAnyLlamaCppHealthy()
	}
	if backendID == "" {
		logger.Get().Errorw("handleGenerate: no llama.cpp backend available",
			"model", model)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no llama.cpp backend available"})
		return
	}
	logger.Get().Debugw("handleGenerate: selected backend",
		"model", model, "backend", backendID)
	r.Body = io.NopCloser(bytes.NewReader(bodyBuf))

	// Preflight n_ctx check.
	if lr.runInferencePreflight(runInferencePreflightArgs{
		w:          w,
		r:          r,
		body:       bodyBuf,
		model:      model,
		backendID:  backendID,
		backendURL: lr.proxy.backendHTTPAddrByID(backendID),
	}) {
		return
	}

	// Auto-load.
	loadOpts := warmupOptions{}
	resolvedLoad := lr.proxy.ResolveNumCtx(model, bodyBuf, backendID)
	if resolvedLoad.Value > 0 {
		loadOpts.NumCtx = resolvedLoad.Value
	}
	if _, loadErr := lr.ensureModelLoadedOnBackend(backendID, model, loadOpts); loadErr != nil {
		logger.Get().Errorw("handleGenerate: auto-load failed",
			"backend", backendID, "model", model, "error", loadErr)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": fmt.Sprintf("model '%s' is not loaded and auto-load failed: %v", model, loadErr),
		})
		return
	}

	// Update LastKnownNCtx after model load.
	if loadedNCtx := lr.proxy.getLoadedNCtxFromMetrics(backendID, model); loadedNCtx > 0 {
		lr.proxy.nctxReload.SetLastKnownNCtx(backendID, loadedNCtx)
		logger.Get().Debugw("handleGenerate: updated LastKnownNCtx",
			"backend", backendID, "model", model, "loaded_n_ctx", loadedNCtx)
	}

	resolved := lr.proxy.ApplyCppCtxHeader(r, model, bodyBuf, backendID)
	if resolved.Value > 0 {
		logger.Get().Debugw("handleGenerate: 3-tier resolver applied num_ctx override",
			"model", model, "backend", backendID,
			"resolved_n_ctx", resolved.Value, "source", resolved.Source)
	}

	var err error
	if isStreamingFromBody(r.URL.Path, bodyBuf) {
		logger.Get().Debugw("handleGenerate: streaming mode", "backend", backendID)
		err = lr.proxy.proxyRequestLlamaCpp(w, r, backendID, bodyBuf)
	} else {
		logger.Get().Debugw("handleGenerate: non-streaming mode", "backend", backendID)
		err = lr.proxy.proxyRequestLlamaCppNonStream(w, r, backendID, bodyBuf)
	}
	if err != nil {
		logger.Get().Errorw("handleGenerate: proxy failed", "backend", backendID, "error", err)
		if !isHeadersSent(w) {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		}
	}
}

// isHeadersSent — проверяет, были ли уже отправлены HTTP-заголовки.
func isHeadersSent(w http.ResponseWriter) bool {
	if flusher, ok := w.(http.Flusher); ok {
		_ = flusher
	}
	return w.Header().Get("Content-Type") != ""
}