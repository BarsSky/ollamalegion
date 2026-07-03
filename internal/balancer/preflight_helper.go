// preflight_helper.go — высокоуровневая обёртка RunPreflight для роутеров.
//
// Используется llamacpp_handlers_inference.go перед проксированием запросов
// на cppworker. Что делает:
//  1. Парсит body запроса через ExtractRequestMeta (если распознан).
//  2. Получает состояние бэкенда: currentNCtx из NCtxReloadCoordinator
//     + modelMaxContext из per-model profile в config.
//  3. Вызывает RunPreflight (см. preflight_nctx.go).
//  4. Если PreflightReject — пишет HTTP 413 напрямую и возвращает (handled=true).
//  5. Если PreflightReload или NoOp — возвращает (handled=false), роутер
//     продолжает обычное проксирование.
//
// NB про MaxVRAMNCtx: на текущем этапе (2026-06-23) балансировщик не имеет
// прямого способа узнать max_vram_n_ctx бэкенда — cppworker сообщает это
// только в BRIDGE_ERR_PROMPT_TOO_LONG после ошибки инференса. В preflight
// мы используем только currentNCtx (из coordinator) и modelMaxContext (из
// профиля). Если они оба есть — preflight может Reject (по modelMaxContext)
// или Reload (по разнице currentNCtx и required). Безопасный fallback:
// если currentNCtx покрывает required → NoOp; иначе — пробуем Reload
// до required (cppworker вернёт 200 если хватило VRAM, 503 OOM если нет;
// в последнем случае существующий DecideReloadBackend отдаст 503).
package balancer

import (
	"net/http"

	"ollama-loadbalancer/pkg/logger"
)

// runInferencePreflightArgs — параметры для runInferencePreflight.
// Выделено в struct, чтобы вызывающий код (роутеры) не зависел от деталей
// internal proxy-методов.
type runInferencePreflightArgs struct {
	w          http.ResponseWriter
	r          *http.Request
	body       []byte
	model      string
	backendID  string
	backendURL string // полный http(s)://host:port для POST /api/models/reload
}

// runInferencePreflight — единая точка входа для роутеров. Возвращает:
//
//   - handled=false: preflight не сработал или успешен, роутер продолжает.
//   - handled=true:  preflight вернул Reject, ответ уже записан в w.
func (lr *LlamaCppRouter) runInferencePreflight(args runInferencePreflightArgs) bool {
	if lr == nil || lr.proxy == nil {
		return false
	}
	coord := lr.proxy.nctxReload
	if coord == nil {
		return false
	}
	cfg := coord.Config()
	if !cfg.PreflightEnabled {
		return false
	}
	if args.r == nil || args.backendURL == "" {
		return false
	}

	path := args.r.URL.Path
	meta := ExtractRequestMeta(args.body, path)
	if meta == nil {
		// Не смогли распарсить body (например, нестандартный формат).
		// Пропускаем preflight — пусть роутер проксирует как обычно.
		return false
	}

	state := lr.collectPreflightState(args.backendID, args.model)
	if state == nil {
		return false
	}

	backendURL := args.backendURL
	if backendURL == "" && lr.proxy != nil {
		backendURL = lr.proxy.backendHTTPAddrByID(args.backendID)
	}
	if backendURL == "" {
		return false
	}

	res, err := coord.RunPreflight(
		args.r.Context(),
		args.backendID,
		backendURL,
		meta,
		state,
		lr.proxy.newNCtxReloadHTTPClient(),
	)
	if err != nil {
		// Reload-инфраструктура упала (например, network error). Не блокируем
		// запрос — пусть роутер проксирует и вернёт code 3/2, на который
		// существующий DecideReloadBackend среагирует.
		logger.Get().Warnw("preflight helper: RunPreflight failed (continuing without preflight)",
			"backend", args.backendID, "error", err)
		return false
	}
	switch res.Decision {
	case PreflightReject:
		logger.Get().Warnw("preflight: HTTP 413 returned to client",
			"backend", args.backendID, "model", meta.ModelName,
			"target_n_ctx", res.TargetNCtx, "status", res.RejectStatus)
		args.w.Header().Set("Content-Type", "application/json")
		args.w.WriteHeader(res.RejectStatus)
		_, _ = args.w.Write([]byte(res.RejectBody))
		return true
	case PreflightReload:
		logger.Get().Infow("preflight: model reloaded before proxy (round-trip one)",
			"backend", args.backendID, "model", meta.ModelName,
			"new_n_ctx", res.TargetNCtx, "has_tools", meta.HasTools)
		return false
	default: // PreflightNoOp
		return false
	}
}

// collectPreflightState собирает NCtxBackendState для бэкенда:
// currentNCtx из NCtxReloadCoordinator; modelMaxContext из per-model
// profile в config. Возвращает nil, если currentNCtx неизвестен
// и preflight вырождается в NoOp.
func (lr *LlamaCppRouter) collectPreflightState(backendID, model string) *NCtxBackendState {
	if lr == nil || lr.proxy == nil {
		return nil
	}
	currentNCtx := lr.proxy.nctxReload.LastKnownNCtx(backendID)
	if currentNCtx <= 0 {
		// Fallback: LastKnownNCtx не установлен (первичный запрос).
		// Пытаемся получить n_ctx из metrics poller.
		currentNCtx = lr.proxy.getLoadedNCtxFromMetrics(backendID, model)
		if currentNCtx > 0 {
			lr.proxy.nctxReload.SetLastKnownNCtx(backendID, currentNCtx)
			logger.Get().Debugw("collectPreflightState: fallback to metrics",
				"backend", backendID, "model", model,
				"current_n_ctx", currentNCtx, "source", "metrics")
		}
	}
	if currentNCtx <= 0 {
		return nil
	}
	// modelMaxContext — из per-model profile в config (опционально).
	var modelMaxContext int
	if lr.proxy.config != nil {
		if mp, ok := lr.proxy.config.LlamaCppModelProfiles[model]; ok && mp.ContextLength > 0 {
			modelMaxContext = mp.ContextLength
		}
	}
	return &NCtxBackendState{
		BackendID:       backendID,
		CurrentNCtx:     currentNCtx,
		MaxVRAMNCtx:     lr.proxy.getMaxVRAMNCtxFromMetrics(backendID), // из /api/models poller (ранее было 0 — не доступен)
		ModelMaxContext: modelMaxContext,
	}
}
