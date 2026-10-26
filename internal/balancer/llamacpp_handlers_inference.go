package balancer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/internal/i18n"
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
		bodyBuf, err = readRequestBodyLimited(r)
		if errors.Is(err, errBodyTooLarge) {
			sendBodyTooLarge(w, r)
			return
		}
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

	// R247 (2026-10-26), этап D: изображения не отбрасываем молча.
	// Модель сейчас обслуживается текстовым мостом (mmproj не подключён):
	// честный 415 с объяснением лучше 200, где модель «не видела» картинку.
	if images := countImagePartsInBody(bodyBuf); images > 0 {
		logger.Get().Warnw("vision parts rejected: backend is text-only",
			"model", model, "image_parts", images, "path", r.URL.Path)
		writeVisionUnsupported(w, model, images)
		return
	}

	if normalized := normalizeOpenAIBody(bodyBuf); len(normalized) > 0 {
		bodyBuf = normalized
	}

	// R186 (2026-10-20): схемы инструментов — в контекст запроса, чтобы привести
	// аргументы вызовов к объявленным типам на ЛЮБОМ пути отдачи SSE.
	// Живой случай: Cline получал `{"commands":"git status"}` при схеме
	// `commands: array` → «✖ Invalid input» и повтор до остановки по циклу.
	r = r.WithContext(WithToolSchemas(r.Context(), toolSchemasFromBody(bodyBuf)))

	// R84 (2026-10-03): инструмент генерации изображений для текстовой модели.
	//
	// Объявляем его ТОЛЬКО когда есть кому исполнять: здоровый image_cpp и либо
	// загруженная модель, либо (R85, при LB_IMAGE_TOOL_ALLOW_LOAD=on) модели,
	// которые вызов поднимет сам (imageToolTargetFor). Иначе тело запроса
	// остаётся байт-в-байт прежним, и путь ничем не отличается от обычного.
	//
	// Исполняет вызов сам балансер: инструмент объявил прокси, клиент про него не
	// знает и вызвать бы не смог (см. image_tool.go).
	//
	// R86: решение логируется одной INFO-строкой (путь, что прислал клиент, что
	// решил балансер и почему) — без неё вопрос «почему модель не видит
	// инструменты» требовал чтения исходников.
	clientTools := countClientTools(bodyBuf)
	var imageTool *imageToolTarget
	if tgt, skip := lr.proxy.imageToolDecision(r.Context()); tgt != nil {
		tools := imageToolOpenAITools(tgt)
		// R141: в режиме auto объявляем только тем, кто прислал свои tools[].
		if !imageToolShouldInject(lr.proxy.imageToolSettings(), clientTools) {
			logImageToolDecision(r.URL.Path, model, clientTools, false, nil,
				imageToolSkipReason(imageToolNoInjectReason(lr.proxy.imageToolSettings())))
		} else if injected, ok, injErr := injectImageTool(bodyBuf, tools); injErr != nil {
			logger.Get().Warnw(i18n.T("balancer.llamacpp_handlers_inference.image_tool_inject_failed_warn"),
				"error", injErr)
		} else if ok {
			bodyBuf = injected
			imageTool = tgt
			// Наблюдаемость: клиент/лог видит, что инструменты были объявлены.
			// При автозагрузке их два — список имён честнее одного заголовка.
			w.Header().Set("X-Image-Tool", imageToolNamesHeader(tools))
			logImageToolDecision(r.URL.Path, model, clientTools, true, imageToolNames(tools), "")
		} else {
			logImageToolDecision(r.URL.Path, model, clientTools, false, nil,
				imageToolSkipReason(imageToolSkipClientOwns))
		}
	} else {
		logImageToolDecision(r.URL.Path, model, clientTools, false, nil, imageToolSkipReason(skip))
	}

	// R81: сначала группа репликации (строгий обход копий), затем — как раньше.
	backendID := lr.selectLlamaCppBackendForModelWithNCtx(model, ExtractNumCtxFromBody(bodyBuf))
	// R193 (2026-10-20): НЕИЗВЕСТНУЮ МОДЕЛЬ НЕ ПОДМЕНЯЕМ ЗАГРУЖЕННОЙ.
	// Живой дефект: напрямую воркер отвечает 404 model_file_not_found, а через
	// балансер клиент получал 200 и ответ ДРУГОЙ (загруженной) модели. Для агента
	// (Cline) это тихая ошибка: он уверен, что говорит с выбранной моделью.
	if ok, available := lr.proxy.modelAnswerable(backendID, model); !ok {
		logger.Get().Warnw(i18n.T("balancer.llamacpp_handlers_inference.model_missing_catalog_warn"),
			"backend", backendID, "model", model, "available", len(available))
		writeModelMissingResponseFor(w, r.URL.Path, model, i18n.T("balancer.llamacpp_handlers_inference.model_missing_reason"), available)
		return
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

	// R55.8 (2026-08-25): bind session для OpenAI-совместимых клиентов.
	// Pre-R55.8: sessions создавались только в proxyRequest.go для /api/generate + /api/chat.
	// OpenAI пути шли через proxyRequestOpenAIStreamAsNonStream/proxyRequestLlamaCpp
	// и НЕ создавали sessions -> WebUI показывал "0 сессий" под нагрузкой.
	{
		clientName := lr.proxy.getClientName(r)
		sessionID := lr.proxy.getSessionIDWithModel(r, clientName, model)
		if sessionID != "" {
			lr.proxy.sessionMgr.SetWithProtocol(sessionID, backendID, model, clientName, lr.proxy.getClientRealIP(r), r.UserAgent(), RequestProtocol(r.URL.Path))
			w.Header().Set("X-Session-ID", sessionID)
		}
		w.Header().Set("X-Backend-ID", backendID)
	}

	// Preflight n_ctx check.
	if handled, _ := lr.runInferencePreflight(runInferencePreflightArgs{
		w:          w,
		r:          r,
		body:       bodyBuf,
		model:      model,
		backendID:  backendID,
		backendURL: lr.proxy.backendHTTPAddrByID(backendID),
	}); handled {
		return
	}

	// Auto-load.
	if model != "" {
		loadOpts := warmupOptions{}
		resolved := lr.proxy.ResolveNumCtx(model, bodyBuf, backendID)
		// R127 (2026-10-16, пункт 7 разбора стенда): окно для АВТОЗАГРУЗКИ берём
		// из резолвера, без отдельной «подгонки под загруженное».
		//
		// ЖИВОЙ ДЕФЕКТ. Снимок метрик может ещё несколько секунд считать модель
		// загруженной, когда она уже выгружена. Тогда ветка «не понижаем
		// загруженное» подменяла ПАРАМЕТР ЗАПРОСА чужим числом, и автозагрузка
		// уходила без num_ctx: в логе
		//   ResolveNumCtx: R60.37 — body num_ctx < loaded, upgrading (4096 -> 32768)
		//   ensureModelLoadedOnBackend: auto-loading model ... numCtx=null
		// а модель поднималась с профильными 8192 — запрос 4096 не влиял ни на что.
		//
		// ПОЧЕМУ ВЕТКА БЫЛА ЛИШНЕЙ. Резолвер УЖЕ поднимает клиентское значение до
		// загруженного окна (R60.37 внутри ResolveNumCtx). А этот код исполняется
		// только когда модель НЕ готова (иначе ensureModelLoadedWithFailover
		// выходит сразу), то есть «загруженное» здесь — либо то же число, либо
		// устаревшее. Правильный источник в момент загрузки — запрос → профиль →
		// дефолт (то, что и вернул резолвер).
		if resolved.Value > 0 {
			loadOpts.NumCtx = resolved.Value
		}
		if newBackend, loadErr := lr.ensureModelLoadedWithFailover(backendID, model, warmupOptions{NumCtx: loadOpts.NumCtx, GPULayers: loadOpts.GPULayers, Ctx: r.Context(), SessionKey: RequestSessionKey(r, bodyBuf)}); loadErr != nil {
			logger.Get().Errorw("handleOpenAIChatCompletions: auto-load failed",
				"backend", backendID, "model", model, "error", loadErr)
			// R60.16: include Retry-After so clients (Cline/Roo/openai-python)
			// back off instead of busy-looping. Default 30s matches R60.6
			// async-reload Retry-After.
			// R119 (2026-10-15): для ОТСУТСТВУЮЩЕЙ модели — терминальный 404, а не
			// 503 «повтори» (см. model_missing_r119.go): модели от повторения не
			// появится, а клиент уходил в retry-цикл на каждый запрос.
			if lr.proxy.writeModelMissingIfKnownFor(w, r.URL.Path, backendID, model, loadErr) {
				return
			}
			// R185 (2026-10-20): карта не поддерживается образом воркера —
			// терминальный 400 с причиной и советом. 503 здесь означал бы
			// «повтори, станет лучше», и клиент крутил бы перезагрузки вечно,
			// пока воркер падал по SIGABRT на первом GPU-ядре.
			if lr.proxy.writeModelArchUnsupportedIfKnownFor(w, r.URL.Path, backendID, model, loadErr) {
				return
			}
			writeServiceUnavailable(w,
				fmt.Sprintf("model '%s' is not loaded and auto-load failed: %v", model, loadErr),
				writeAutoLoadRetryAfter(loadErr))
			return
		} else if newBackend != backendID {
			// R82: исходный бэкенд выпал — обслуживаем с живой копии. Обновляем и
			// снапшот state: targetURL ниже строится из state.Backend.Host.
			backendID = newBackend
			w.Header().Set("X-Backend-ID", backendID)
			if st, ok := lr.proxy.backends[backendID]; ok {
				state = st
			}
			logger.Get().Infow(i18n.T("balancer.llamacpp_handlers_inference.r82_migrate_chat_completions_info"),
				"backend", backendID, "model", model)
		}
	}

	r.Body = io.NopCloser(bytes.NewReader(bodyBuf))

	// Round 34 follow-up: preflight n_ctx check для OpenAI path.
	// Раньше (Round 34 Phase 0) preflight был добавлен ТОЛЬКО для Ollama путей
	// (handleChat, handleGenerate). Cline использует OpenAI /v1/chat/completions,
	// поэтому preflight skip'ался → запрос напрямую шёл в cppworker → cppworker
	// возвращал 400 "n_ctx too large" → балансер конвертировал в 413 → Cline
	// получал 413 sync вместо 503+Retry-After.
	// Fix: добавляем preflight ДО ApplyCppCtxHeader и proxying.
	if handled, _ := lr.runInferencePreflight(runInferencePreflightArgs{
		w:          w,
		r:          r,
		body:       bodyBuf,
		model:      model,
		backendID:  backendID,
		backendURL: lr.proxy.backendHTTPAddrByID(backendID),
	}); handled {
		return
	}

	resolved := lr.proxy.ApplyCppCtxHeader(r, model, bodyBuf, backendID)
	if resolved.Value > 0 {
		logger.Get().Debugw("handleOpenAIChatCompletions: 3-tier resolver applied num_ctx override",
			"model", model, "backend", backendID,
			"resolved_n_ctx", resolved.Value, "source", resolved.Source)
	}

	// R67b (2026-09-23): admission-очередь. Если все слоты бэкенда заняты
	// (maxConcurrentReqs), запрос ждёт освобождения вместо немедленного 503 —
	// жалоба «резко отбивает повторный запрос выдавая 503, а не поставляет в
	// очередь». Освобождается в defer после завершения генерации.
	if release, admitted := lr.acquireInferenceAdmission(w, r, backendID, model, bodyBuf); !admitted {
		return
	} else if release != nil {
		defer release()
	}

	targetURL := fmt.Sprintf("http://%s:%d/v1/chat/completions", state.Backend.Host, lr.proxy.getBackendPort(state.Backend))
	logger.Get().Infow("handleOpenAIChatCompletions: proxying to cppworker",
		"backend", backendID, "url", targetURL, "model", model)

	// R84: запрос с объявленным image-инструментом обслуживает цикл
	// «вызов → генерация → финальный ответ» (image_tool_loop.go). Он возвращает
	// true, если ответ клиенту уже записан; false — если не смог даже начать, и
	// тогда работает обычный путь ниже.
	if imageTool != nil {
		if lr.runImageToolLoop(w, r, imageToolLoopArgs{
			bodyBuf:   bodyBuf,
			model:     model,
			backendID: backendID,
			targetURL: targetURL,
			target:    imageTool,
		}) {
			return
		}
	}

	// Round 31 #1 (2026-08-09): auto-stream workaround для non-stream клиентов.
	// cppworker буферизирует non-stream (ждёт headers до конца генерации) → 120-300s timeout.
	// Workaround: шлём upstream как stream=true, накапливаем чанки, формируем non-stream JSON.
	// Клиент получает 200 ОК сразу (chunked transfer) → нет блокировки.
	// Default disabled (LB_OPENAI_AUTO_STREAM=false) для backward compat.
	if !isStreamingFromBody(r.URL.Path, bodyBuf) && IsOpenAIAutoStreamEnabled() {
		logger.Get().Infow("handleOpenAIChatCompletions: using Round 31 #1 auto-stream workaround",
			"backend", backendID, "model", model)
		if err := lr.proxy.proxyRequestOpenAIStreamAsNonStream(w, r, bodyBuf, targetURL, backendID); err != nil {
			logger.Get().Errorw("handleOpenAIChatCompletions: auto-stream workaround failed",
				"backend", backendID, "error", err)
			// R53.5 (2026-08-24): mid-stream error → emit final SSE done-чанк
			if isHeadersSent(w) {
				writeStreamErrorChunk(w, "/v1/chat/completions", model, err.Error())
			} else {
				// R83-fix (2026-09-30): не отдаём сырой dial-текст — Cline принимал
				// его за переполнение контекста. 503 + Retry-After для «узел недоступен».
				writeUpstreamError(w, backendID, model, err)
			}
		}
		return
	}

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
			// R83-fix (2026-09-30): не отдаём сырой dial-текст — Cline принимал
			// его за переполнение контекста. 503 + Retry-After для «узел недоступен».
			writeUpstreamError(w, backendID, model, err)
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

	// Round 18 P0.1: X-Model-* capability headers.
	lr.proxy.addModelCapabilitiesHeaders(w, model)
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
		bodyBuf, err = readRequestBodyLimited(r)
		if errors.Is(err, errBodyTooLarge) {
			sendBodyTooLarge(w, r)
			return
		}
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

	// R81: сначала группа репликации (строгий обход копий), затем — как раньше.
	backendID := lr.selectLlamaCppBackendForModelWithNCtx(model, ExtractNumCtxFromBody(bodyBuf))
	// R193 (2026-10-20): неизвестное имя модели не подменяем загруженной — 404.
	if ok, available := lr.proxy.modelAnswerable(backendID, model); !ok {
		logger.Get().Warnw(i18n.T("balancer.llamacpp_handlers_inference.model_missing_catalog_llamacpp_warn"),
			"backend", backendID, "model", model, "available", len(available))
		writeModelMissingResponseFor(w, r.URL.Path, model, i18n.T("balancer.llamacpp_handlers_inference.model_missing_reason"), available)
		return
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

	// R55.8 (2026-08-25): bind session для OpenAI-совместимых клиентов.
	// Pre-R55.8: sessions создавались только в proxyRequest.go для /api/generate + /api/chat.
	// OpenAI пути шли через proxyRequestOpenAIStreamAsNonStream/proxyRequestLlamaCpp
	// и НЕ создавали sessions -> WebUI показывал "0 сессий" под нагрузкой.
	{
		clientName := lr.proxy.getClientName(r)
		sessionID := lr.proxy.getSessionIDWithModel(r, clientName, model)
		if sessionID != "" {
			lr.proxy.sessionMgr.SetWithProtocol(sessionID, backendID, model, clientName, lr.proxy.getClientRealIP(r), r.UserAgent(), RequestProtocol(r.URL.Path))
			w.Header().Set("X-Session-ID", sessionID)
		}
		w.Header().Set("X-Backend-ID", backendID)
	}

	// Preflight n_ctx check.
	if handled, _ := lr.runInferencePreflight(runInferencePreflightArgs{
		w:          w,
		r:          r,
		body:       bodyBuf,
		model:      model,
		backendID:  backendID,
		backendURL: lr.proxy.backendHTTPAddrByID(backendID),
	}); handled {
		return
	}

	// Auto-load.
	if model != "" {
		loadOpts := warmupOptions{}
		resolved := lr.proxy.ResolveNumCtx(model, bodyBuf, backendID)
		// R127 (2026-10-16, пункт 7): см. подробный комментарий в handleOpenAIChatCompletions: окно для АВТОЗАГРУЗКИ берём
		// из резолвера, без отдельной «подгонки под загруженное».
		//
		// ЖИВОЙ ДЕФЕКТ. Снимок метрик может ещё несколько секунд считать модель
		// загруженной, когда она уже выгружена. Тогда ветка «не понижаем
		// загруженное» подменяла ПАРАМЕТР ЗАПРОСА чужим числом, и автозагрузка
		// уходила без num_ctx: в логе
		//   ResolveNumCtx: R60.37 — body num_ctx < loaded, upgrading (4096 -> 32768)
		//   ensureModelLoadedOnBackend: auto-loading model ... numCtx=null
		// а модель поднималась с профильными 8192 — запрос 4096 не влиял ни на что.
		//
		// ПОЧЕМУ ВЕТКА БЫЛА ЛИШНЕЙ. Резолвер УЖЕ поднимает клиентское значение до
		// загруженного окна (R60.37 внутри ResolveNumCtx). А этот код исполняется
		// только когда модель НЕ готова (иначе ensureModelLoadedWithFailover
		// выходит сразу), то есть «загруженное» здесь — либо то же число, либо
		// устаревшее. Правильный источник в момент загрузки — запрос → профиль →
		// дефолт (то, что и вернул резолвер).
		if resolved.Value > 0 {
			loadOpts.NumCtx = resolved.Value
		}
		if newBackend, loadErr := lr.ensureModelLoadedWithFailover(backendID, model, warmupOptions{NumCtx: loadOpts.NumCtx, GPULayers: loadOpts.GPULayers, Ctx: r.Context(), SessionKey: RequestSessionKey(r, bodyBuf)}); loadErr != nil {
			logger.Get().Errorw("handleOpenAICompletion: auto-load failed",
				"backend", backendID, "model", model, "error", loadErr)
			// R60.16: include Retry-After so clients back off (see chat completions).
			// R119: отсутствующая модель даёт терминальный 404, а не 503 «повтори».
			if lr.proxy.writeModelMissingIfKnownFor(w, r.URL.Path, backendID, model, loadErr) {
				return
			}
			// R185 (2026-10-20): карта не поддерживается образом воркера —
			// терминальный 400 с причиной и советом. 503 здесь означал бы
			// «повтори, станет лучше», и клиент крутил бы перезагрузки вечно,
			// пока воркер падал по SIGABRT на первом GPU-ядре.
			if lr.proxy.writeModelArchUnsupportedIfKnownFor(w, r.URL.Path, backendID, model, loadErr) {
				return
			}
			writeServiceUnavailable(w,
				fmt.Sprintf("model '%s' is not loaded and auto-load failed: %v", model, loadErr),
				0)
			return
		} else if newBackend != backendID {
			// R82: переезд — обновляем и backendID, и снапшот state (ниже из него
			// берётся URL апстрима).
			backendID = newBackend
			w.Header().Set("X-Backend-ID", backendID)
			if st, ok := lr.proxy.backends[backendID]; ok {
				state = st
			}
			logger.Get().Infow(i18n.T("balancer.llamacpp_handlers_inference.r82_migrate_completion_info"),
				"backend", backendID, "model", model)
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
			// R83-fix (2026-09-30): не отдаём сырой dial-текст — Cline принимал
			// его за переполнение контекста. 503 + Retry-After для «узел недоступен».
			writeUpstreamError(w, backendID, model, err)
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

	// Round 18 P0.1: X-Model-* capability headers.
	lr.proxy.addModelCapabilitiesHeaders(w, model)
	_ = lr.proxy.proxyRequestOpenAIStreaming(w, r, upstreamResp, backendID)
}

// handleOpenAIEmbeddings — OpenAI /v1/embeddings через dedicated dispatch.
//
// Round 22 (2026-08-03): вместо main proxy flow (который идёт в queue_manager
// → slot manager → блокируется на VRAM headroom 15%) делаем direct dispatch
// на cppworker, как handleOpenAICompletion. Без этого: при VRAM > 85%
// (например после /v1/chat/completions загрузившего модель) embeddings
// получают 30s timeout 503.
//
// Использует ensureModelLoadedOnBackend для автозагрузки модели.
func (lr *LlamaCppRouter) handleOpenAIEmbeddings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var bodyBuf []byte
	if r.Body != nil {
		var err error
		bodyBuf, err = readRequestBodyLimited(r)
		if errors.Is(err, errBodyTooLarge) {
			sendBodyTooLarge(w, r)
			return
		}
		if err != nil {
			logger.Get().Errorw("handleOpenAIEmbeddings: failed to read body", "error", err)
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(bodyBuf))
	}

	var req map[string]interface{}
	if err := json.Unmarshal(bodyBuf, &req); err != nil {
		logger.Get().Errorw("handleOpenAIEmbeddings: failed to parse body", "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	model, _ := req["model"].(string)

	// R81: сначала группа репликации (строгий обход копий), затем — как раньше.
	backendID := lr.selectLlamaCppBackendForModelWithNCtx(model, ExtractNumCtxFromBody(bodyBuf))
	// R193 (2026-10-20): неизвестное имя модели не подменяем загруженной — 404.
	if ok, available := lr.proxy.modelAnswerable(backendID, model); !ok {
		logger.Get().Warnw(i18n.T("balancer.llamacpp_handlers_inference.model_missing_catalog_llamacpp_warn"),
			"backend", backendID, "model", model, "available", len(available))
		writeModelMissingResponseFor(w, r.URL.Path, model, i18n.T("balancer.llamacpp_handlers_inference.model_missing_reason"), available)
		return
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

	// Auto-load (если модель ещё не загружена).
	if model != "" {
		if newBackend, loadErr := lr.ensureModelLoadedWithFailover(backendID, model, warmupOptions{Ctx: r.Context(), SessionKey: RequestSessionKey(r, bodyBuf)}); loadErr != nil {
			logger.Get().Errorw("handleOpenAIEmbeddings: auto-load failed",
				"backend", backendID, "model", model, "error", loadErr)
			// R60.16: include Retry-After so clients back off (see chat completions).
			// R119: отсутствующая модель даёт терминальный 404, а не 503 «повтори».
			if lr.proxy.writeModelMissingIfKnownFor(w, r.URL.Path, backendID, model, loadErr) {
				return
			}
			// R185 (2026-10-20): карта не поддерживается образом воркера —
			// терминальный 400 с причиной и советом. 503 здесь означал бы
			// «повтори, станет лучше», и клиент крутил бы перезагрузки вечно,
			// пока воркер падал по SIGABRT на первом GPU-ядре.
			if lr.proxy.writeModelArchUnsupportedIfKnownFor(w, r.URL.Path, backendID, model, loadErr) {
				return
			}
			writeServiceUnavailable(w,
				fmt.Sprintf("model '%s' is not loaded and auto-load failed: %v", model, loadErr),
				0)
			return
		} else if newBackend != backendID {
			// R82: переезд на живую копию (URL ниже берётся из state).
			backendID = newBackend
			w.Header().Set("X-Backend-ID", backendID)
			if st, ok := lr.proxy.backends[backendID]; ok {
				state = st
			}
			logger.Get().Infow(i18n.T("balancer.llamacpp_handlers_inference.r82_migrate_embeddings_info"),
				"backend", backendID, "model", model)
		}
	}

	r.Body = io.NopCloser(bytes.NewReader(bodyBuf))

	targetURL := fmt.Sprintf("http://%s:%d/v1/embeddings", state.Backend.Host, lr.proxy.getBackendPort(state.Backend))

	logger.Get().Infow("handleOpenAIEmbeddings: proxying to cppworker",
		"backend", backendID, "url", targetURL, "model", model)

	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, targetURL, bytes.NewReader(bodyBuf))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	upstreamReq.Header.Set("Content-Type", "application/json")
	upstreamReq.Header.Set("Accept", "application/json")

	upstreamResp, err := lr.proxy.client.Do(upstreamReq)
	if err != nil {
		logger.Get().Errorw("handleOpenAIEmbeddings: upstream request failed",
			"backend", backendID, "model", model, "error", err)
		// R83-fix (2026-09-30): та же понятная ошибка, что и на chat-путях
		// (503 + Retry-After при недоступном узле вместо сырого dial-текста).
		writeUpstreamError(w, backendID, model, err)
		return
	}
	defer upstreamResp.Body.Close()

	// Round 18 P0.1: X-Model-* capability headers.
	lr.proxy.addModelCapabilitiesHeaders(w, model)

	// Пробрасываем ответ клиенту.
	for k, v := range upstreamResp.Header {
		for _, vv := range v {
			w.Header().Add(k, vv)
		}
	}
	w.WriteHeader(upstreamResp.StatusCode)
	_, _ = io.Copy(w, upstreamResp.Body)
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
		bodyBuf, err = readRequestBodyLimited(r)
		if errors.Is(err, errBodyTooLarge) {
			sendBodyTooLarge(w, r)
			return
		}
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

	// R86 (2026-10-06): инструмент генерации изображений на OLLAMA-поверхности.
	//
	// ЗАЧЕМ: Open WebUI по умолчанию подключается как Ollama-сервер и ходит на
	// POST /api/chat — там инструменты не объявлялись вовсе, поэтому «модель не
	// видит инструменты» при работе через Open WebUI. Теперь объявляем здесь так
	// же, как на /v1/chat/completions, и исполняем вызов своим циклом.
	//
	// Тело меняется ТОЛЬКО когда инструмент реально объявлен: без image-бэкенда
	// (или при LB_IMAGE_TOOL=off) путь остаётся байт-в-байт прежним.
	//
	// Инструменты КЛИЕНТА сохраняются (в Open WebUI у пользователя обычно есть
	// свои), а tool_choice="none" уважается — см. injectImageToolsOllama.
	clientTools := countClientTools(bodyBuf)
	var imageToolOllama *imageToolTarget
	if tgt, skip := lr.proxy.imageToolDecision(r.Context()); tgt != nil {
		// R141: в режиме auto объявляем только тем, кто прислал свои tools[].
		if !imageToolShouldInject(lr.proxy.imageToolSettings(), clientTools) {
			logImageToolDecision(r.URL.Path, model, clientTools, false, nil,
				imageToolSkipReason(imageToolNoInjectReason(lr.proxy.imageToolSettings())))
		} else if injected, added := injectImageToolOllama(bodyBuf, tgt); len(added) > 0 {
			bodyBuf = injected
			imageToolOllama = tgt
			w.Header().Set("X-Image-Tool", strings.Join(added, ","))
			logImageToolDecision(r.URL.Path, model, clientTools, true, added, "")
		} else {
			logImageToolDecision(r.URL.Path, model, clientTools, false, nil,
				imageToolSkipReason(imageToolSkipClientOwns))
		}
	} else {
		logImageToolDecision(r.URL.Path, model, clientTools, false, nil, imageToolSkipReason(skip))
	}

	// R81: сначала группа репликации (строгий обход копий), затем — как раньше.
	backendID := lr.selectLlamaCppBackendForModelWithNCtx(model, ExtractNumCtxFromBody(bodyBuf))
	if backendID == "" {
		logger.Get().Errorw("handleChat: no llama.cpp backend available",
			"model", model)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no llama.cpp backend available"})
		return
	}
	logger.Get().Debugw("handleChat: selected backend",
		"model", model, "backend", backendID)
	r.Body = io.NopCloser(bytes.NewReader(bodyBuf))

	// R55.8 (2026-08-25): bind session для Ollama native /api/chat клиентов.
	{
		clientName := lr.proxy.getClientName(r)
		sessionID := lr.proxy.getSessionIDWithModel(r, clientName, model)
		if sessionID != "" {
			lr.proxy.sessionMgr.SetWithProtocol(sessionID, backendID, model, clientName, lr.proxy.getClientRealIP(r), r.UserAgent(), RequestProtocol(r.URL.Path))
			w.Header().Set("X-Session-ID", sessionID)
		}
		w.Header().Set("X-Backend-ID", backendID)
	}

	// Preflight n_ctx check.
	if handled, _ := lr.runInferencePreflight(runInferencePreflightArgs{
		w:          w,
		r:          r,
		body:       bodyBuf,
		model:      model,
		backendID:  backendID,
		backendURL: lr.proxy.backendHTTPAddrByID(backendID),
	}); handled {
		return
	}

	// Auto-load.
	loadOpts := warmupOptions{}
	resolvedLoad := lr.proxy.ResolveNumCtx(model, bodyBuf, backendID)
	if resolvedLoad.Value > 0 {
		loadOpts.NumCtx = resolvedLoad.Value
	}
	if newBackend, loadErr := lr.ensureModelLoadedWithFailover(backendID, model, warmupOptions{NumCtx: loadOpts.NumCtx, GPULayers: loadOpts.GPULayers, Ctx: r.Context(), SessionKey: RequestSessionKey(r, bodyBuf)}); loadErr != nil {
		// R60.41: downgraded to Info — это не "error" в обычном смысле, это
		// просто "load стартовал async, клиенту надо подождать". Реальная
		// ошибка была бы только если load полностью провалился (sync path,
		// circuit breaker open и т.д.). Async path в R60.33+ всегда
		// возвращает loadErr даже при успешном kick'е.
		logger.Get().Infow("handleChat: model load started (R60.33 async, R60.41 message)",
			"backend", backendID, "model", model, "loadErr", loadErr)
		// R60.16: include Retry-After so clients back off.
		// R60.40: include actionable diagnostics — OpenWebUI can show the
		// user what to fix instead of just "auto-load failed".
		// R60.41: error message itself is more accurate (no "failed" when
		// load was actually kicked off successfully).
		diag := buildAutoLoadDiagnosticWithProxy(lr.proxy, backendID, model, resolvedLoad.Value, loadErr)
		writeServiceUnavailableWithDiagnostics(w, diag)
		return
	} else if newBackend != backendID {
		// R82: исходный бэкенд выпал — обслуживаем с живой копии.
		backendID = newBackend
		w.Header().Set("X-Backend-ID", backendID)
		logger.Get().Infow(i18n.T("balancer.llamacpp_handlers_inference.r82_migrate_chat_info"),
			"backend", backendID, "model", model)
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

	// R67b (2026-09-23): admission-очередь — ждём свободный слот вместо 503.
	if release, admitted := lr.acquireInferenceAdmission(w, r, backendID, model, bodyBuf); !admitted {
		return
	} else if release != nil {
		defer release()
	}

	// R86: инструмент объявлен → сперва пробуем цикл «вызов → исполнение →
	// финальный ответ». Он сам решает, нужен ли второй turn, и возвращает false
	// только если ПЕРВЫЙ шаг не удался (клиенту ещё ничего не отправлено) —
	// тогда запрос идёт обычным путём.
	if imageToolOllama != nil {
		if lr.runImageToolLoopOllama(w, r, imageToolLoopArgsOllama{
			bodyBuf:    bodyBuf,
			model:      model,
			backendID:  backendID,
			backendURL: lr.proxy.backendHTTPAddrByID(backendID),
			target:     imageToolOllama,
		}) {
			return
		}
		logger.Get().Warnw(i18n.T("balancer.llamacpp_handlers_inference.image_tool_loop_fallback_warn"),
			"model", model, "backend", backendID)
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
		// R53.5 (2026-08-24): mid-stream error path. If headers were already sent
		// (streaming started), MUST emit final done-чанк with error info,
		// otherwise client (Cline/OpenWebUI/Flowise) gets connection drop without
		// done:true → "Did not receive done or success response in stream" error.
		if isHeadersSent(w) {
			writeStreamErrorChunk(w, "/api/chat", model, err.Error())
		} else {
			// R83-fix (2026-09-30): не отдаём сырой dial-текст — Cline принимал
			// его за переполнение контекста. 503 + Retry-After для «узел недоступен».
			writeUpstreamError(w, backendID, model, err)
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
		bodyBuf, err = readRequestBodyLimited(r)
		if errors.Is(err, errBodyTooLarge) {
			sendBodyTooLarge(w, r)
			return
		}
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

	// R81: сначала группа репликации (строгий обход копий), затем — как раньше.
	backendID := lr.selectLlamaCppBackendForModelWithNCtx(model, ExtractNumCtxFromBody(bodyBuf))
	// R193 (2026-10-20): неизвестное имя модели не подменяем загруженной — 404.
	if ok, available := lr.proxy.modelAnswerable(backendID, model); !ok {
		logger.Get().Warnw(i18n.T("balancer.llamacpp_handlers_inference.model_missing_catalog_llamacpp_warn"),
			"backend", backendID, "model", model, "available", len(available))
		writeModelMissingResponseFor(w, r.URL.Path, model, i18n.T("balancer.llamacpp_handlers_inference.model_missing_reason"), available)
		return
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

	// R55.8 (2026-08-25): bind session для Ollama native /api/generate клиентов.
	{
		clientName := lr.proxy.getClientName(r)
		sessionID := lr.proxy.getSessionIDWithModel(r, clientName, model)
		if sessionID != "" {
			lr.proxy.sessionMgr.SetWithProtocol(sessionID, backendID, model, clientName, lr.proxy.getClientRealIP(r), r.UserAgent(), RequestProtocol(r.URL.Path))
			w.Header().Set("X-Session-ID", sessionID)
		}
		w.Header().Set("X-Backend-ID", backendID)
	}

	// Preflight n_ctx check.
	if handled, _ := lr.runInferencePreflight(runInferencePreflightArgs{
		w:          w,
		r:          r,
		body:       bodyBuf,
		model:      model,
		backendID:  backendID,
		backendURL: lr.proxy.backendHTTPAddrByID(backendID),
	}); handled {
		return
	}

	// Auto-load.
	loadOpts := warmupOptions{}
	resolvedLoad := lr.proxy.ResolveNumCtx(model, bodyBuf, backendID)
	if resolvedLoad.Value > 0 {
		loadOpts.NumCtx = resolvedLoad.Value
	}
	if newBackend, loadErr := lr.ensureModelLoadedWithFailover(backendID, model, warmupOptions{NumCtx: loadOpts.NumCtx, GPULayers: loadOpts.GPULayers, Ctx: r.Context(), SessionKey: RequestSessionKey(r, bodyBuf)}); loadErr != nil {
		logger.Get().Errorw("handleGenerate: auto-load failed",
			"backend", backendID, "model", model, "error", loadErr)
		// R60.16: include Retry-After so clients back off (see chat completions).
		// R185: карта не поддерживается образом — терминальный 400, не 503.
		if lr.proxy.writeModelArchUnsupportedIfKnownFor(w, r.URL.Path, backendID, model, loadErr) {
			return
		}
		writeServiceUnavailable(w,
			fmt.Sprintf("model '%s' is not loaded and auto-load failed: %v", model, loadErr),
			0)
		return
	} else if newBackend != backendID {
		// R82: исходный бэкенд выпал — обслуживаем с живой копии.
		backendID = newBackend
		w.Header().Set("X-Backend-ID", backendID)
		logger.Get().Infow(i18n.T("balancer.llamacpp_handlers_inference.r82_migrate_generate_info"),
			"backend", backendID, "model", model)
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

	// R67b (2026-09-23): admission-очередь — ждём свободный слот вместо 503.
	if release, admitted := lr.acquireInferenceAdmission(w, r, backendID, model, bodyBuf); !admitted {
		return
	} else if release != nil {
		defer release()
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
		// R53.5 (2026-08-24): см. handleChat — mid-stream error path
		// должен эмитить final done-чанк клиенту, иначе Cline/OpenWebUI
		// валятся с "Did not receive done or success response in stream".
		if isHeadersSent(w) {
			writeStreamErrorChunk(w, "/api/generate", model, err.Error())
		} else {
			// R83-fix (2026-09-30): не отдаём сырой dial-текст — Cline принимал
			// его за переполнение контекста. 503 + Retry-After для «узел недоступен».
			writeUpstreamError(w, backendID, model, err)
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

// writeStreamErrorChunk — Round 53.5 (2026-08-24) HOTFIX.
//
// При ошибке proxyRequest*_Stream() mid-stream (cppworker timeout / hang / network blip)
// заголовки клиенту уже отправлены (200 OK, Content-Type, chunked transfer).
// Без этого фикса: balancer возвращает управление, connection закрывается,
// клиент (Cline, OpenWebUI, Flowise) никогда не получает done:true чанк →
// "Did not receive done or success response in stream" ошибка.
//
// Функция эмитит финальный Ollama NDJSON / OpenAI SSE чанк с done:true +
// done_reason:"error" + error message, чтобы клиент корректно завершил стрим.
//
// Параметры:
//   - w: ResponseWriter
//   - originalPath: "/api/chat" | "/api/generate" | "/v1/chat/completions" | "/v1/completions"
//   - modelFromCtx: имя модели (для Ollama-чанка; OpenAI получает из chunk)
//   - errorMsg: текст ошибки
func writeStreamErrorChunk(w http.ResponseWriter, originalPath, modelFromCtx, errorMsg string) {
	switch originalPath {
	case "/v1/chat/completions":
		// OpenAI SSE: финальный чанк с finish_reason="error" + data: [DONE]
		errChunk := map[string]interface{}{
			"id":      fmt.Sprintf("err-%d", time.Now().UnixNano()),
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   modelFromCtx,
			"choices": []interface{}{
				map[string]interface{}{
					"index":         0,
					"delta":         map[string]interface{}{},
					"finish_reason": "error",
				},
			},
			"error": map[string]interface{}{
				"message": errorMsg,
				"type":    "stream_proxy_error",
			},
		}
		out, _ := json.Marshal(errChunk)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", string(out))
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
		Flush(w)
	case "/v1/completions":
		// OpenAI /v1/completions: тот же формат
		errChunk := map[string]interface{}{
			"id":      fmt.Sprintf("err-%d", time.Now().UnixNano()),
			"object":  "text_completion",
			"created": time.Now().Unix(),
			"model":   modelFromCtx,
			"choices": []interface{}{
				map[string]interface{}{
					"index":         0,
					"text":          "",
					"finish_reason": "error",
				},
			},
			"error": map[string]interface{}{
				"message": errorMsg,
				"type":    "stream_proxy_error",
			},
		}
		out, _ := json.Marshal(errChunk)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", string(out))
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
		Flush(w)
	case "/api/generate":
		// Ollama /api/generate: NDJSON done-чанк
		msg := map[string]interface{}{
			"model":       modelFromCtx,
			"created_at":  time.Now().UTC().Format(time.RFC3339),
			"done":        true,
			"done_reason": "error",
			"error":       errorMsg,
			"response":    "",
		}
		out, _ := json.Marshal(msg)
		_, _ = fmt.Fprintf(w, "%s\n", string(out))
		Flush(w)
	default:
		// /api/chat + fallback: Ollama NDJSON done-чанк
		msg := map[string]interface{}{
			"model":       modelFromCtx,
			"created_at":  time.Now().UTC().Format(time.RFC3339),
			"done":        true,
			"done_reason": "error",
			"error":       errorMsg,
			"message":     map[string]interface{}{"role": "assistant", "content": ""},
		}
		out, _ := json.Marshal(msg)
		_, _ = fmt.Fprintf(w, "%s\n", string(out))
		Flush(w)
	}
}
