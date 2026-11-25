// model_management_llamacpp.go — R632 (2026-11-19): путь llama.cpp: executeLlamaCppLoadOnce (главный цикл загрузки), unload/delete, запрос к воркеру.
//
// ЗАЧЕМ ВЫНЕСЕНО. `model_management.go` держал 2172 строки: конструктор,
// операции, ожидание загрузки, отдельный 447-строчный цикл загрузки llama.cpp и
// общий транспорт. Здесь — один из слоёв; поведение не менялось, код перенесён как есть.
package balancer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"strings"
	"time"
)

func (mm *ModelManager) executeLlamaCppLoadOnce(host string, port int, backendID string, req ModelOpRequest) *ModelOpResult {
	// Round 27 follow-up (v0.5.14 follow-up #2): если у модели стоит профиль
	// с disabled=true (например, gemma-4 с upstream GGML_ASSERT на любом n_ctx >= 8192)
	// — отказываем в авто-загрузке с понятной ошибкой. Без этого клиент (Cline/OpenWebUI)
	// уходит в crash-loop: cppworker SIGABRT → Docker restart → балансер снова
	// пытается загрузить → опять SIGABRT.
	if prof, ok := mm.proxy.GetModelProfile(req.ModelName); ok && prof.Disabled {
		msg := fmt.Sprintf("model %q is marked as disabled in profile (broken: see profile.notes). "+
			"Auto-load refused. Use a different model or remove the disabled flag from the profile.",
			req.ModelName)
		logger.Get().Warnw("executeLlamaCppLoad: model disabled in profile, refusing auto-load",
			"model", req.ModelName, "backend", backendID, "profileNotes", prof.Notes)
		return &ModelOpResult{
			Success:   false,
			Operation: req.Operation,
			ModelName: req.ModelName,
			BackendID: backendID,
			Error:     msg,
		}
	}

	// R118.2/R118.3: гард раскладки и локальный повтор после её отказа живут в
	// обёртке executeLlamaCppLoad (rpc_layout_failure.go) — здесь остаётся
	// только сама загрузка.

	// R66d (2026-09-23): профиль модели — источник параметров загрузки, если
	// запрос их не задал.
	//
	// БЫЛО (баг): профиль использовался ТОЛЬКО для override-tensors, а
	// contextLength / batchSize / numGpuLayers / flashAttn / useMmap /
	// kvCacheType из него при загрузке игнорировались. При этом n_ctx-resolver
	// (GetModelProfileNumCtx, num_ctx_resolver.go:99) уже считает
	// profile.ContextLength истиной и отдаёт его в UI и в preflight — то есть
	// UI показывал n_ctx=8192 (профиль gemma-4-E4B-it-Q4_K_M), а cppworker
	// грузил модель со своим дефолтом. Отсюда две жалобы сразу:
	//   * «в WebUI не видно реальной конфигурации загруженной модели»;
	//   * «профиль не применяется при загрузке (preload)» — профиль действовал
	//     только через POST .../apply, который делает reload.
	//
	// Приоритет: явные поля запроса > профиль > дефолт cppworker.
	// Явные поля важнее, потому что WebUI/клиент видят их в форме и ждут
	// ровно этих значений.
	//
	// NumGPULayers: 0 трактуем как «не задано» (в типе профиля нет признака
	// «поле отсутствует», а 0 = CPU-only на практике не использовался;
	// ValidateProfile допускает 0).
	// R119 (2026-10-15, пункт 4): происхождение параметров считается ПО ХОДУ
	// применения слоёв (сравнением снимков), поэтому не может разойтись с тем,
	// что реально уехало в cppworker. См. load_provenance.go.
	provenance := newLoadProvenanceBuilder(&req)
	if prof, ok := mm.profileForLoad(req.ModelName); ok {
		provenance.observe(LoadSourceProfile, func() { applyProfileLoadParams(&req, prof) })
	}
	// R83 (2026-09-30): «настройки по умолчанию для новых моделей».
	//
	// Если у модели НЕТ своего профиля (или у профиля ignoreDefaults=true),
	// параметры загрузки берутся из config.defaultModelProfile — того самого
	// конфига, который оператор редактирует из WebUI. Раньше этот раздел config
	// влиял только на потолок n_ctx, а загрузка уходила в env-дефолты контейнера
	// cppworker: оператор выставлял значения в WebUI, а новая модель поднималась
	// «как настроен контейнер» — увидеть и поменять это из WebUI было нельзя.
	//
	// Вызов идёт ПОСЛЕ профиля модели и заполняет только пустые поля, поэтому
	// приоритет остаётся: явный запрос > профиль модели > дефолт > env cppworker.
	// R83/v62 (2026-10-02): ignoreDefaults=true в профиле МОДЕЛИ означает «не
	// подставлять значения из настроек по умолчанию» — и только это.
	//
	// БЫЛО (дефект, живая жалоба оператора): профиль модели с ignoreDefaults
	// сам не применялся (см. ранний return в applyProfileLoadParams), НО сразу
	// после него применялся defaultModelProfile — и заполнял всё: параллельность,
	// KV-cache, слои, окно. То есть «игнорировать умолчания» на практике значило
	// «подставить умолчания вместо настроек оператора».
	//
	// ТЕПЕРЬ: при ignoreDefaults дефолтный профиль для этой модели не
	// применяется вовсе — действуют env/конфиг контейнера. Явный запрос и
	// профиль модели (когда ignoreDefaults=false) работают как раньше; приоритет:
	// явный запрос > профиль модели > дефолт > env cppworker.
	ignoreDefaultsSet := false
	if modelProf, ok := mm.profileForLoad(req.ModelName); ok {
		ignoreDefaultsSet = modelProf.IgnoreDefaults
	}
	if dp, ok := mm.proxy.GetDefaultModelProfile(); ok && !ignoreDefaultsSet {
		provenance.observe(LoadSourceDefaultProfile, func() { applyProfileLoadParams(&req, dp) })
	} else if ignoreDefaultsSet && logger.Get() != nil {
		logger.Get().Infow(i18n.T("balancer.model_management.default_profile_skipped_info"),
			"model", req.ModelName, "backend", backendID)
	}

	// R119 (пункт 4): сохраняем provenance для API/UI — оператор видит, какой слой
	// дал каждое значение (request / profile / default_profile / backend).
	if mm.proxy != nil {
		mm.proxy.RecordLoadProvenance(backendID, req.ModelName, provenance.finalize())
	}

	// R83-fix (2026-09-30): явная загрузка с окном от оператора (WebUI,
	// load-with-params, apply профиля) ЗАКРЕПЛЯЕТ окно.
	//
	// ЗАЧЕМ. AutoTune («over-allocation fix») умеет уменьшать n_ctx, и раньше его
	// останавливал только «желаемый n_ctx» от КЛИЕНТСКИХ запросов (R69, TTL 30
	// мин). Подготовленная оператором модель без трафика такой защиты не имела:
	// первый же запрос с меньшим окном мог привести к перезагрузке в меньшее окно
	// («подготовленная модель с 131k выгружена в пользу 128k от клиента»).
	// Теперь операторский выбор окна фиксируется тем же механизмом.
	if req.ContextSize != nil && *req.ContextSize > 0 && req.ModelName != "" {
		mm.proxy.recordDesiredNCtx(backendID, req.ModelName, *req.ContextSize)
	}

	// Round 7: resolve override-tensors (req override > profile > none).
	overrideTensors, overrideTensorBufts := mm.resolveOverrideTensors(req)
	useLoadWithParams := len(overrideTensors) > 0 && len(overrideTensors) == len(overrideTensorBufts)

	// R185 (2026-10-20): ОТКАЗ ДО ЗАГРУЗКИ, если образ воркера собран не под эту карту.
	//
	// ДЕФЕКТ (живой разбор). Образ собран с CUDA_ARCH=86;120, карта sm_75: веса
	// мапятся, загрузка «успешна», а первое GPU-ядро убивает процесс по SIGABRT
	// (no kernel image is available). Клиент и UI видели бесконечные повторы без
	// причины. Теперь спрашиваем /api/info и отказываем терминально, с текстом.
	if refuse, reason := mm.proxy.cudaArchRefusal(backendID, host, port); refuse {
		if logger.Get() != nil {
			logger.Get().Errorw(i18n.T("balancer.model_management.cuda_arch_refused_error"),
				"backend", backendID, "model", req.ModelName, "reason", reason)
		}
		// R185: запоминаем отказ, чтобы клиент получил ТЕРМИНАЛЬНЫЙ 400 (а не 503
		// «повтори») на первом же запросе, без ожидания повторной загрузки.
		mm.proxy.recordModelArchUnsupported(backendID, req.ModelName, reason, nil)
		return &ModelOpResult{
			Success:   false,
			Operation: req.Operation,
			ModelName: req.ModelName,
			BackendID: backendID,
			Error:     reason,
		}
	}

	// R188 (2026-10-20): ОТКАЗ ДО ЗАГРУЗКИ, если модель заведомо не влезает в
	// свободную память бэкенда. Живой разбор: балансер ставил тяжёлую модель на
	// узел, где уже жила другая, и запускал загрузку, которая не могла
	// поместиться — слот и очередь занимались на десятки минут, клиент терял ход.
	if mm.proxy != nil {
		token := ""
		if b := mm.proxy.GetBackend(backendID); b != nil {
			token = b.CppWorkerApiToken
		}
		if refuse, reason := mm.proxy.loadFitRefusal(backendID, req.ModelName, host, port, token); refuse {
			if lg := logger.Get(); lg != nil {
				lg.Errorw(i18n.T("balancer.model_management.load_fit_refused_error"),
					"backend", backendID, "model", req.ModelName, "reason", reason)
			}
			return &ModelOpResult{
				Success:   false,
				Operation: req.Operation,
				ModelName: req.ModelName,
				BackendID: backendID,
				Error:     reason,
			}
		}
	}

	url := fmt.Sprintf("http://%s:%d/api/models/load", host, port)
	if useLoadWithParams {
		url = fmt.Sprintf("http://%s:%d/api/models/load-with-params", host, port)
	}
	body := buildLlamaCppLoadBody(req, overrideTensors, overrideTensorBufts, useLoadWithParams)
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return &ModelOpResult{
			Success:   false,
			Operation: req.Operation,
			ModelName: req.ModelName,
			BackendID: backendID,
			Error:     fmt.Sprintf("failed to marshal request body: %v", err),
		}
	}

	// R83/v67 (2026-10-02): без капа по умолчанию.
	//
	// БЫЛО: отдельный клиент с Timeout = Balancing.ModelLoadTimeout (default
	// 120s, позже 600s). Загрузка qwen3.8 15.7 ГБ на A10 идёт дольше, и запрос
	// обрывался на таймере, хотя cppworker продолжал грузить модель — в логе
	// «auto-load failed: context deadline exceeded», в UI «модель не грузится».
	//
	// СТАЛО: ждём ответа бэкенда (успех/ошибка/недоступность). Кап — только
	// opt-in (LB_ALLOW_MODEL_LOAD_TIMEOUT_SEC), с громким WARN.
	loadTimeout := loadRequestTimeout()
	loadClient := &http.Client{
		Timeout: loadTimeout,
	}
	if loadTimeout > 0 {
		logger.Get().Warnw("load request timeout armed (operator opt-in)",
			"load_timeout", loadTimeout.String(), "source", "LB_ALLOW_MODEL_LOAD_TIMEOUT_SEC")
	}

	for attempt := 0; attempt < llamaCppLoadMaxRetries; attempt++ {
		httpReq, reqErr := http.NewRequest("POST", url, bytes.NewReader(jsonBody))
		if reqErr != nil {
			return &ModelOpResult{
				Success:   false,
				Operation: req.Operation,
				ModelName: req.ModelName,
				BackendID: backendID,
				Error:     fmt.Sprintf("failed to create request: %v", reqErr),
			}
		}
		httpReq.Header.Set("Content-Type", "application/json")
		// R307: административная ручка воркера закрыта токеном — предъявляем.
		applyWorkerAdminTokenR307(httpReq.Header, "/api/models/load", mm.workerAdminToken(backendID))
		if rid := RequestIDFromContext(httpReq.Context()); rid != "" {
			httpReq.Header.Set(requestIDHeader, rid)
		}

		start := time.Now()
		resp, err := loadClient.Do(httpReq)
		durationMs := time.Since(start).Milliseconds()
		if err != nil {
			// Сетевая ошибка или таймаут. Раньше сразу возвращали ошибку —
			// но для 21GB+ моделей load занимает 5-10 минут, что может превысить
			// даже увеличенный timeout. Поэтому проверяем, может модель уже загружена
			// на cppworker (load продолжается в фоне) — poll /api/models/load/progress
			// и /api/models пока не получим state="loaded" или timeout на poll.
			//
			// Round 8 (2026-07-10): graceful load timeout — если наш HTTP client
			// отвалился по таймауту, но cppworker всё ещё грузит, ждём завершения
			// через polling. Это решает проблему "model loaded but client got timeout".
			if isTimeoutError(err) {
				logger.Get().Warnw("executeLlamaCppLoad: HTTP timeout, polling for load completion",
					"backend", backendID, "model", req.ModelName,
					"timeout", loadTimeout, "duration_ms", durationMs)
				if pollResult := mm.pollLoadCompletionUntilLoaded(
					host, port, backendID, req.ModelName,
					loadTimeout); pollResult != nil {
					return pollResult
				}
				// R262: poll вернул nil только когда судьба модели НЕИЗВЕСТНА
				// (её нет в списке воркера или воркер недоступен). Если она ещё
				// грузится, poll уже вернул StillLoading (HTTP 202), а не nil —
				// поэтому «мы не дождались» больше не превращается в 5xx.
			}
			logger.Get().Debugw("executeLlamaCppLoad: HTTP error (no retry)",
				"backend", backendID, "model", req.ModelName,
				"attempt", attempt+1, "duration_ms", durationMs, "error", err)
			return &ModelOpResult{
				Success:   false,
				Operation: req.Operation,
				ModelName: req.ModelName,
				BackendID: backendID,
				Error:     fmt.Sprintf("request failed: %v", err),
			}
		}

		respBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return &ModelOpResult{
				Success:   false,
				Operation: req.Operation,
				ModelName: req.ModelName,
				BackendID: backendID,
				Error:     fmt.Sprintf("failed to read response: %v", readErr),
			}
		}

		// Успех.
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			// Round 24 (2026-08-04): detect 202 Accepted from cppworker.
			// cppworker теперь по умолчанию возвращает 202 + Location сразу,
			// реальная загрузка идёт в background goroutine. Без polling balancer
			// считал бы модель загруженной, но инференс-вызов получил бы 503
			// "model is loading". Поэтому при 202 — polling /api/models пока
			// state не станет "loaded" или maxWait не истечёт.
			//
			//   - status=loading      → ещё грузится
			//   - status=already_loaded → готово
			//   - status=loaded       → готово (sync ?wait=true path)
			if resp.StatusCode == http.StatusAccepted {
				var loadResp struct {
					Status              string `json:"status"`
					EstimatedLoadTimeMs int64  `json:"estimatedLoadTimeMs"`
				}
				_ = json.Unmarshal(respBody, &loadResp)
				if loadResp.Status == "loading" || loadResp.Status == "loading_after_timeout" {
					// Round 32 #15 (2026-08-11): user report "model not ready after
					// 1m26.4475s" — balancer сдался слишком рано, модель
					// догрузилась через несколько секунд после timeout.
					// Root cause: maxWait = est*1.5 + 10s — слишком оптимистично
					// для gemma-4 (5GB) на RTX 3070. Реальная загрузка занимает
					// 1m30s+ (VRAM alloc + llama.cpp init + warmup), а cppworker
					// estimated = ~50s → maxWait = 85s = 1m25s. Балансер polling
					// истёк за 3-5 секунд ДО завершения load.
					//
					// Fix: 2x estimated + 60s buffer, min 3min, max 15min.
					// Это даёт 1m40s + 60s = 2m40s для 50s estimated
					// (раньше было 85s). Достаточно для RTX 3070.
					//
					// На медленных GPU (ноутбук, A10) gemma-4 может грузиться
					// 3-5 минут. maxWait = 15min покрывает.
					//
					// Round 35c (2026-08-13): user report "model not ready after
					// 8m7.182s" для Qwen3.6-35B-A3B (22GB) на 3070 — `2*est+60s`
					// дало 8m6s, но фактическая загрузка заняла 12+ мин из-за
					// CUDA_Host pinned memory allocation для 19GB+ весов.
					// Multiplier/cap/buffer теперь env-конфигурируемые через
					// LB_NCTX_PREFLIGHT_MAX_WAIT_SEC / _WAIT_MULTIPLIER /
					// _WAIT_BUFFER_SEC (см. NCtxReloadConfig + applyNCtxReloadEnvOverrides).
					capWait, multiplier, bufDur := resolvePreflightWaitTuning(mm.proxy)
					maxWait := capWait
					if loadResp.EstimatedLoadTimeMs > 0 {
						est := time.Duration(loadResp.EstimatedLoadTimeMs) * time.Millisecond
						maxWait = time.Duration(multiplier)*est + bufDur
						// Round 35c: убрал hardcoded 3-min floor, заменил на dynamic
						// min(1s, capWait/10). Для capWait=900s → min=90s (1.5 min).
						// Для capWait=3s (тесты) → min=300ms. Это позволяет unit-тестам
						// использовать маленький cap через ENV override.
						if maxWait < capWait/10 {
							maxWait = capWait / 10
						}
						if maxWait < time.Second {
							maxWait = time.Second
						}
						if maxWait > capWait {
							maxWait = capWait
						}
					}
					logger.Get().Infow("executeLlamaCppLoad: 202 Accepted (async load), polling for completion",
						"backend", backendID, "model", req.ModelName,
						"estimated_ms", loadResp.EstimatedLoadTimeMs,
						"max_wait", maxWait, "cap_wait", capWait,
						"multiplier", multiplier, "buffer", bufDur)
					// R191 (2026-10-20): РАСКЛАДКА РАСШИРЯЕТ ОКНО ОЖИДАНИЯ.
					// Живой случай: gemma с узлом-донором (RPC0 = 19 слоёв) грузилась дольше
					// окна, балансер отдал 500 «model not ready after 2m26.408s», хотя модель
					// догрузилась и работала (layout_degraded=false): оценка загрузки от воркера
					// не знает про сетевой узел (передача весов + инициализация RPC).
					if mm.proxy != nil {
						token := ""
						if b := mm.proxy.GetBackend(backendID); b != nil {
							token = b.CppWorkerApiToken
						}
						if mm.proxy.rpcLayoutActive(host, port, token) {
							extended := maxWait * 2
							if extended > capWait {
								extended = capWait
							}
							if extended > maxWait {
								logger.Get().Infow(i18n.T("balancer.model_management.layout_wait_extended_info"),
									"backend", backendID, "model", req.ModelName,
									"was", maxWait, "now", extended)
								maxWait = extended
							}
						}
					}

					if pollResult := mm.pollLoadCompletionUntilLoaded(
						host, port, backendID, req.ModelName, maxWait); pollResult != nil {
						return pollResult
					}
					// R262: nil здесь означает «модель не появилась у воркера или
					// воркер недоступен» — вот это и есть провал. Случай «ещё
					// грузится» poll отдаёт как StillLoading (HTTP 202 Accepted),
					// чтобы клиент не считал живую загрузку ошибкой.
					return &ModelOpResult{
						Success:   false,
						Operation: req.Operation,
						ModelName: req.ModelName,
						BackendID: backendID,
						Error: fmt.Sprintf("async load (202) on cppworker: model not found at the worker after %v "+
							"(it never appeared in /api/models — the load did not start)", maxWait),
					}
				}
				// status=already_loaded / loaded / loaded_by_other / unknown → fall through to success.
			}
			return &ModelOpResult{
				Success:   true,
				Operation: req.Operation,
				ModelName: req.ModelName,
				BackendID: backendID,
				Message:   fmt.Sprintf("Model '%s' loaded into memory on backend '%s'", req.ModelName, backendID),
			}
		}

		// Извлекаем сообщение об ошибке из JSON-ответа cppworker.
		errMsg := string(respBody)
		var cwErr struct {
			Error   string `json:"error"`
			Loading bool   `json:"loading"`
		}
		if json.Unmarshal(respBody, &cwErr) == nil && cwErr.Error != "" {
			errMsg = cwErr.Error
		}

		// Специальный случай: 503 с признаком параллельной загрузки — retry.
		if resp.StatusCode == http.StatusServiceUnavailable &&
			(cwErr.Loading || strings.Contains(strings.ToLower(errMsg), "model is loading")) {
			logger.Get().Infow("executeLlamaCppLoad: model is loading on cppworker, retrying",
				"backend", backendID, "model", req.ModelName,
				"attempt", attempt+1, "max_attempts", llamaCppLoadMaxRetries,
				"retry_in_sec", llamaCppLoadRetryInterval.Seconds(),
				"status_code", resp.StatusCode)
			if attempt < llamaCppLoadMaxRetries-1 {
				time.Sleep(llamaCppLoadRetryInterval)
				continue
			}
			// R299 (2026-10-29): «ещё грузится» — НЕ ошибка.
			//
			// Живой факт (стенд 793db393, 148 с): оператор получал HTTP 500 с
			// текстом «модель ещё загружается», то есть собственный текст
			// балансера противоречил коду ответа: 5xx для клиента значит «сломалось,
			// повтори», и оператор нажимал «загрузить» заново поверх живой
			// загрузки. Формат «ещё грузится» уже существует (R262) и отвечает
			// 202 — второй формат для той же ситуации заводить нельзя.
			logger.Get().Infow("executeLlamaCppLoad: cppworker still loading model, answering 202 (not an error)",
				"backend", backendID, "model", req.ModelName,
				"attempts", llamaCppLoadMaxRetries)
			return stillLoadingAfterLoadRetriesR299(req, backendID)
		}

		// Другая ошибка — не повторяем.
		return &ModelOpResult{
			Success:      false,
			Operation:    req.Operation,
			ModelName:    req.ModelName,
			BackendID:    backendID,
			WorkerStatus: resp.StatusCode,
			Error:        fmt.Sprintf("cppworker error (HTTP %d): %s", resp.StatusCode, errMsg),
		}
	}

	// Сюда не должны попасть (цикл всегда выходит через return), но на всякий случай:
	return &ModelOpResult{
		Success:   false,
		Operation: req.Operation,
		ModelName: req.ModelName,
		BackendID: backendID,
		Error:     fmt.Sprintf("cppworker load retries exhausted for model '%s'", req.ModelName),
	}
}

// executeLlamaCppUnload — выгрузка модели из памяти на cppworker-бэкенде.
// cppworker принимает POST /api/models/unload с ?name=... в query string.
//
// R60.32 (2026-09-10): после успешного unload ОБЯЗАТЕЛЬНО инвалидировать
// state в balancer (NCtxReloadCoordinator.lastKnownNCtx=0 +
// MetricsManager.UpdateLlamaCppModelUnloaded). Иначе R60.31 stickiness
// (preflight_nctx.go:335-360) думает что модель ещё загружена с
// n_ctx=4096, не триггерит reload → следующий запрос получает 502
// от cppworker (модель не загружена). cppworker callback
// /api/v1/internal/llama-model-unloaded НЕ вызывается автоматически
// при unload — balancer должен явно инвалидировать state.
func (mm *ModelManager) executeLlamaCppUnload(host string, port int, backendID string, req ModelOpRequest) *ModelOpResult {
	safeName := urlPathEscape(req.ModelName)
	unloadURL := fmt.Sprintf("http://%s:%d/api/models/unload?name=%s", host, port, safeName)

	// R66c (2026-09-22): прокидываем ?force=true, если оператор явно попросил
	// выгрузить ЗАНЯТУЮ модель. Без этого cppworker отвечает 409 и модель
	// остаётся в памяти, а у пользователя нет способа её выгрузить.
	if req.Force != nil && *req.Force {
		unloadURL += "&force=true"
	}

	resp, err := mm.sendRawRequestWithTokenR307("POST", unloadURL, nil, mm.workerAdminToken(backendID))
	if err != nil {
		return &ModelOpResult{
			Success:   false,
			Operation: req.Operation,
			ModelName: req.ModelName,
			BackendID: backendID,
			Error:     fmt.Sprintf("failed to unload model: %v", err),
		}
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	// R66c (2026-09-22): 409 Conflict = cppworker отказал, потому что по модели
	// есть активные inference-запросы (cmd/cppworker/handlers_model.go:902-929).
	// Отдаём это структурно, а не только текстом: WebUI по полю retryWithForce
	// предлагает выгрузить модель с force=true, не разбирая строку ошибки.
	if resp.StatusCode == http.StatusConflict {
		return &ModelOpResult{
			Success:        false,
			Operation:      req.Operation,
			ModelName:      req.ModelName,
			BackendID:      backendID,
			Error:          fmt.Sprintf("model '%s' is busy with active inference requests", req.ModelName),
			Busy:           true,
			RetryWithForce: true,
		}
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// R60.32: invalidate balancer state so R60.31 stickiness knows
		// the model is no longer loaded.
		if mm.proxy != nil {
			if coord := mm.proxy.GetNCtxReloadCoordinator(); coord != nil {
				coord.SetLastKnownNCtx(backendID, 0)
			}
			if mmgr := mm.proxy.GetMetricsManager(); mmgr != nil {
				mmgr.UpdateLlamaCppModelUnloaded(backendID, req.ModelName)
			}
		}
		return &ModelOpResult{
			Success:   true,
			Operation: req.Operation,
			ModelName: req.ModelName,
			BackendID: backendID,
			Message:   fmt.Sprintf("Model '%s' unloaded from memory on backend '%s'", req.ModelName, backendID),
		}
	}
	return &ModelOpResult{
		Success:      false,
		Operation:    req.Operation,
		ModelName:    req.ModelName,
		BackendID:    backendID,
		WorkerStatus: resp.StatusCode,
		Error:        fmt.Sprintf("cppworker error (HTTP %d): %s", resp.StatusCode, string(respBody)),
	}
}

// executeLlamaCppDelete — удаление модели с диска на cppworker-бэкенде.
// cppworker принимает POST /api/models/delete (или DELETE) с JSON {"name": "..."} или ?name=...
func (mm *ModelManager) executeLlamaCppDelete(host string, port int, backendID string, req ModelOpRequest) *ModelOpResult {
	url := fmt.Sprintf("http://%s:%d/api/models/delete", host, port)
	body := map[string]interface{}{
		"name": req.ModelName,
	}
	return mm.sendCppWorkerRequest("POST", url, backendID, req, body)
}

// sendCppWorkerRequest — отправка запроса к cppworker (delete и пр.).
//
// R758 (2026-11-25): ТОКЕН АДМИНСКОЙ РУЧКИ ОБЯЗАТЕЛЕН.
//
// ЖИВОЙ ДЕФЕКТ (первый стенд): удаление модели из WebUI («GGUF-модели» →
// «Удалить») всегда падало с `Delete failed: cppworker error (HTTP 401):
// invalid or missing API token`. Причина не в кнопке и не в правах оператора:
// `/api/models/delete` у воркера закрыт authMiddleware, а этот путь шёл через
// `sendRawRequest` — БЕЗ токена. R307 закрыл токеном load (applyWorkerAdminTokenR307)
// и unload (sendRawRequestWithTokenR307), а delete остался без него: файл на
// диске удалить из панели было нельзя вообще, только вручную через docker exec.
//
// Пустая строка токена означает «у бэкенда токена нет» — заголовок не ставится,
// и воркер без авторизации такой запрос пропускает (та же семантика, что у
// unload: поведение обеих разрушительных операций обязано совпадать).
func (mm *ModelManager) sendCppWorkerRequest(method, url, backendID string, req ModelOpRequest, body map[string]interface{}) *ModelOpResult {
	resp, err := mm.sendRawRequestWithTokenR307(method, url, body, mm.workerAdminToken(backendID))
	if err != nil {
		return &ModelOpResult{
			Success:   false,
			Operation: req.Operation,
			ModelName: req.ModelName,
			BackendID: backendID,
			Error:     fmt.Sprintf("request failed: %v", err),
		}
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		opName := operationDisplayName(req.Operation)
		return &ModelOpResult{
			Success:   true,
			Operation: req.Operation,
			ModelName: req.ModelName,
			BackendID: backendID,
			Message:   fmt.Sprintf("Model '%s' %s on backend '%s'", req.ModelName, opName, backendID),
		}
	}

	errMsg := string(respBody)
	var cwErr struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(respBody, &cwErr) == nil && cwErr.Error != "" {
		errMsg = cwErr.Error
	}
	return &ModelOpResult{
		Success:      false,
		Operation:    req.Operation,
		ModelName:    req.ModelName,
		BackendID:    backendID,
		WorkerStatus: resp.StatusCode,
		Error:        fmt.Sprintf("cppworker error (HTTP %d): %s", resp.StatusCode, errMsg),
	}
}

// urlPathEscape — экранирование пути для подстановки в URL-сегмент (а не в query).
func urlPathEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '-' || r == '_' || r == '.' || r == '~' {
			b.WriteRune(r)
		} else {
			fmt.Fprintf(&b, "%%%02X", r)
		}
	}
	return b.String()
}
