// proxy_helpers.go — package-level HTTP proxy helpers shared by the Ollama Router,
// LlamaCpp Router, and all llamacpp_handlers_*.go files.
//
// R51.1 (2026-08-20): extracted from internal/balancer/ollama_router.go.
//
// History:
//   - R40-R48: writeJSON + copyResponse lived in ollama_router.go (lines 258-279).
//     This was a code smell: helpers in a router file. The placement was
//     fragile — R49 audit (commit e66c6da) tried to remove them as "dead code"
//     and broke `go build ./...`. R49b (commit 2a06f70) restored them via
//     `git checkout d3f7003 -- internal/balancer/ollama_router.go`.
//   - R50 design doc (docs/superpowers/specs/2026-08-19-balancer-api-routing-design.md,
//     §4.1) flagged writeJSON + copyResponse as a known-fragile design.
//   - R51.1: this file. Helpers moved to dedicated proxy_helpers.go.
//     Same package (balancer), same signatures, same semantics.
//     No caller needs to change — they're package-level functions.
//
// Design intent (per R50 design doc §4.1):
//   - Keep the package-level scope (NOT unexported) so llamacpp_handlers_*.go
//     and ollama_router_*.go can call them without import gymnastics.
//   - Document the API here (single source of truth for "how balancer writes
//     JSON responses" and "how balancer copies upstream responses").
//   - Future R51.x refactors can add tests, sub-types, or alternative formats
//     here without touching router files.
//
// Anti-pattern to AVOID:
//   - DO NOT inline writeJSON/copyResponse into a single call site. The 50+
//     call sites across llamacpp_handlers_*.go would duplicate the logic
//     again. Keep these helpers here.
//   - DO NOT add new package-level helpers to router files. If you need a
//     helper, add it to this file (or a sibling like proxy_helpers_test.go).

package balancer

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// writeJSON — write a JSON response with the given HTTP status and payload.
// Sets Content-Type to application/json before writing. Body is `data` encoded
// via encoding/json (no trailing newline added by caller).
//
// Used by 50+ call sites across:
//   - llamacpp_handlers_admin.go
//   - llamacpp_handlers_inference.go
//   - llamacpp_handlers_readonly.go
//   - llamacpp_runtime_config.go
//   - ollama_router_tags.go
//   - ollama_router_admin.go
//   - session_handler.go
//   - slot_handler.go
//
// Pre-R51.1: defined in ollama_router.go:269.
// R51.1: moved here. No signature change.
func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

// writeServiceUnavailable — write a 503 response with a Retry-After
// header (in seconds). Use this for any 503 that tells the client to
// retry later (e.g. auto-load in progress, async reload pending).
//
// R60.16 (2026-09-08): prior to this, the auto-load-failed paths in
// llamacpp_handlers_inference.go (5 sites) returned 503 with NO
// Retry-After header, so clients (Cline/Roo/openai-python) saw an
// empty Retry-After and either retried immediately (busy-loop) or
// gave up. The fix: always set Retry-After on 503 for the auto-load
// path. 30s is a reasonable default — matches the async-reload
// Retry-After used elsewhere in R60.6.
//
// retryAfterSec: seconds the client should wait before retrying.
// Pass 0 to use the default (30s).
//
// Used by:
//   - llamacpp_handlers_inference.go (5 sites: chat/completions,
//     completions, embeddings, /api/chat, /api/generate)
func writeServiceUnavailable(w http.ResponseWriter, errMsg string, retryAfterSec int) {
	if retryAfterSec <= 0 {
		retryAfterSec = 30
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterSec))
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": errMsg})
}

// ServiceUnavailableDiagnostic — структурированные данные для ответа 503/502.
// R60.40 (2026-09-11): actionable diagnostics для клиента.
//
// OpenWebUI раньше получал `{"error": "..."}` без какой-либо информации
// о том:
//   - Что произошло (model loading? n_ctx overflow? prompt too long?)
//   - Сколько ждать (Retry-After мог быть hardcoded 30s даже для reload
//     который займёт 95s+)
//   - Какие оптимальные настройки для этой модели на этом железе
//
// R60.40 заполняет все эти поля из:
//   - cppworker's last bridge_info (current_n_ctx, required_n_ctx, etc.)
//   - cppworker /api/models metrics (feasible_max_context, gguf_max_context)
//   - preflight decision (target_n_ctx, estimated_load_time_ms)
//   - heuristic suggestion text
type ServiceUnavailableDiagnostic struct {
	// Error — короткое человекочитаемое сообщение (обязательно).
	Error string
	// RetryAfterSec — клиент должен подождать столько секунд (0 = default 30).
	RetryAfterSec int

	// Optional diagnostics — все поля опциональны. Пустые не попадают в JSON.
	Model              string // имя модели
	BackendID          string // для multi-backend debug
	TargetNCtx         int    // n_ctx который мы загружаем / перезагружаем
	EstimatedLoadMs    int    // estimated_load_time_ms от cppworker load response
	FeasibleMaxContext int    // max_vram_n_ctx из cppworker /api/models
	GGUFMaxContext     int    // gguf_max_context из cppworker /api/models
	CurrentNCtx        int    // загруженный сейчас (для overflow errors)
	RequiredNCtx       int    // требуемый (для overflow errors)
	Suggestion         string // actionable совет ("Reduce num_predict to 1024 OR...")
}

// writeServiceUnavailableWithDiagnostics — R60.40 enhanced version.
// Возвращает 503 с Retry-After header + JSON body с actionable diagnostics.
//
// Используется вместо writeServiceUnavailable когда:
//   - auto-load triggered и мы знаем target_n_ctx
//   - cppworker вернул n_ctx overflow и мы знаем feasible/required
//   - есть estimated_load_time_ms от cppworker load API
//
// Response JSON пример:
//
//	{
//	  "error": "model 'X' auto-load in progress, retry in 30s",
//	  "model": "X",
//	  "target_n_ctx": 8192,
//	  "estimated_load_time_ms": 95000,
//	  "feasible_max_context": 25884,
//	  "gguf_max_context": 262144,
//	  "current_n_ctx": 2048,
//	  "required_n_ctx": 8500,
//	  "retry_after": 95,
//	  "suggestion": "Reduce num_predict to 1024 OR save a model profile with contextLength=8192 and reload"
//	}
func writeServiceUnavailableWithDiagnostics(w http.ResponseWriter, diag ServiceUnavailableDiagnostic) {
	// Вычисляем Retry-After:
	//   1. diag.RetryAfterSec > 0 → use it
	//   2. diag.EstimatedLoadMs > 0 → use EstimatedLoadMs / 1000 (capped)
	//   3. default → 90 (R60.42: bumped from 30s based on real load times)
	retryAfterSec := diag.RetryAfterSec
	if retryAfterSec <= 0 && diag.EstimatedLoadMs > 0 {
		retryAfterSec = clampEstimatedMsToRetryAfter(diag.EstimatedLoadMs)
	}
	if retryAfterSec <= 0 {
		retryAfterSec = 90
	}

	// Build JSON body with omitempty (только заполненные поля попадают в ответ).
	body := map[string]interface{}{
		"error": diag.Error,
	}
	if diag.Model != "" {
		body["model"] = diag.Model
	}
	if diag.BackendID != "" {
		body["backend_id"] = diag.BackendID
	}
	if diag.TargetNCtx > 0 {
		body["target_n_ctx"] = diag.TargetNCtx
	}
	if diag.EstimatedLoadMs > 0 {
		body["estimated_load_time_ms"] = diag.EstimatedLoadMs
	}
	if diag.FeasibleMaxContext > 0 {
		body["feasible_max_context"] = diag.FeasibleMaxContext
	}
	if diag.GGUFMaxContext > 0 {
		body["gguf_max_context"] = diag.GGUFMaxContext
	}
	if diag.CurrentNCtx > 0 {
		body["current_n_ctx"] = diag.CurrentNCtx
	}
	if diag.RequiredNCtx > 0 {
		body["required_n_ctx"] = diag.RequiredNCtx
	}
	if diag.Suggestion != "" {
		body["suggestion"] = diag.Suggestion
	}
	body["retry_after"] = retryAfterSec

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterSec))
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(body)
}

// clampEstimatedMsToRetryAfter — конвертирует estimated_load_time_ms в
// Retry-After секунды с разумными границами:
//   - floor: 5s (чтобы клиент не retry-ил слишком часто)
//   - ceiling: 600s (10 мин — чтоб не висели вечно)
//
// Округление: ceil(ms / 1000) — минимум 1 секунда сверх floor.
// Например: 95000ms → 95s, 1000ms → 5s (floor), 700000ms → 600s (cap).
func clampEstimatedMsToRetryAfter(ms int) int {
	if ms <= 0 {
		return 0
	}
	sec := (ms + 999) / 1000 // ceil division
	if sec < 5 {
		sec = 5
	}
	if sec > 600 {
		sec = 600
	}
	return sec
}

// writeAutoLoadRetryAfter — R60.33 (2026-09-10): вычисляет правильный
// Retry-After в зависимости от причины auto-load failure. Если load
// идёт async (R60.33) — возвращает 30s (модель загрузится). Если failed
// (circuit breaker и т.д.) — больше.
//
// R60.42 (2026-09-11): **Bumped default to 90s** based on real-world data:
// 2.5GB Q4_K_M модель на RTX-3070 с reload n_ctx=2048→4096 занимает ~180s
// (см. комментарии R60.42). Hardcoded 30s вызывал cascade: client retry-ил
// 6 раз подряд (каждые 30s = 3 минуты), а load ещё не завершился.
// 90s — conservative middle: большинство loads укладываются, worst case
// = 2 retries = 3 минуты вместо 6.
//
// ВСЕГДА возвращает >=1 (writeServiceUnavailable default 30s, но
// мы explicit ставим правильное значение для ясности).
//
// R60.41 (2026-09-11): renamed "auto-load in progress" → "load started, waiting
// for cppworker" в error message. Функция всё ещё находит по substring.
func writeAutoLoadRetryAfter(loadErr error) int {
	if loadErr == nil {
		return 90
	}
	errStr := loadErr.Error()
	// R60.33 async mode: модель в процессе загрузки. Клиент retry
	// через 90s (realistic для 2-3GB моделей).
	// R60.41: matches both old "auto-load in progress" и new "load started".
	if strings.Contains(errStr, "auto-load in progress") || strings.Contains(errStr, "load started") {
		return 90
	}
	// R60.6 async-reload: n_ctx reload. 90s.
	if strings.Contains(errStr, "n_ctx reload") {
		return 90
	}
	// R60.26 circuit breaker: 60s (backoff).
	if strings.Contains(errStr, "circuit breaker open") {
		return 60
	}
	// Default 90s.
	return 90
}

// buildAutoLoadDiagnostic — R60.40 (2026-09-11) helper: формирует
// ServiceUnavailableDiagnostic для auto-load failure cases с actionable
// info для клиента.
//
// Что кладём в диагностику:
//   - error: human-readable message
//   - retry_after_sec: from writeAutoLoadRetryAfter() (R60.33 logic)
//   - target_n_ctx: что пытаемся загрузить (resolved.Value из ResolveNumCtx)
//   - feasible_max_context: max_vram_n_ctx из cppworker /api/models metrics
//   - gguf_max_context: gguf_max_context из metrics (модель's hard upper bound)
//   - suggestion: человекочитаемый совет ("Reduce num_predict" или "wait")
//
// Что НЕ кладём (нет инфы):
//   - estimated_load_time_ms — cppworker не возвращает в async mode
//     (только в initial /api/models/load response). Для async load
//     retry_after_sec остаётся 30s (default).
//
// Caller: handleChat (R60.40).
func buildAutoLoadDiagnostic(
	backendID, model string,
	targetNCtx int,
	loadErr error,
) ServiceUnavailableDiagnostic {
	diag := ServiceUnavailableDiagnostic{
		Error: fmt.Sprintf("model '%s' is not loaded and auto-load failed: %v", model, loadErr),
		Model: model,
		BackendID: backendID,
		RetryAfterSec: writeAutoLoadRetryAfter(loadErr),
	}
	if targetNCtx > 0 {
		diag.TargetNCtx = targetNCtx
	}
	// Fetch metrics если Proxy доступен — через p.getMaxVRAMNCtxFromMetrics +
	// getGGUFMaxContext (вызывающий передаёт *Proxy или мы делаем lookup).
	// Чтобы не плодить циклы импорта, оставляем вызов через p proxy — см.
	// buildAutoLoadDiagnosticWithProxy ниже.
	return diag
}

// buildAutoLoadDiagnosticWithProxy — R60.40 (2026-09-11) full version
// с доступом к Proxy для получения feasible_max_context / gguf_max_context.
//
// Используется в handler'ах где есть доступ к *Proxy.
func buildAutoLoadDiagnosticWithProxy(
	p *Proxy,
	backendID, model string,
	targetNCtx int,
	loadErr error,
) ServiceUnavailableDiagnostic {
	diag := buildAutoLoadDiagnostic(backendID, model, targetNCtx, loadErr)
	if p == nil {
		return diag
	}
	// Fetch cppworker /api/models metrics.
	diag.FeasibleMaxContext = p.getMaxVRAMNCtxFromMetrics(backendID)
	// GGUFMaxContext — из llamaMetrics[backendID].GGUFMaxContext.
	if mm := p.GetMetricsManager(); mm != nil {
		if lm := mm.GetLlamaCppMetrics(backendID); lm != nil {
			diag.GGUFMaxContext = lm.GGUFMaxContext
		}
	}
	// Suggestion: actionable text based on what we know.
	diag.Suggestion = buildAutoLoadSuggestion(diag, loadErr)
	return diag
}

// buildAutoLoadSuggestion — формирует человекочитаемый совет на основе
// причины auto-load failure.
func buildAutoLoadSuggestion(diag ServiceUnavailableDiagnostic, loadErr error) string {
	if loadErr == nil {
		return "Model is loading. Please retry in 60-120 seconds."
	}
	errStr := loadErr.Error()
	// R60.41: matches both R60.33 "auto-load in progress" (legacy) и
	// "load started, waiting for cppworker" (new). R60.41 message
	// change is to clarify что load был УСПЕШНО kickнутый, не failed.
	if strings.Contains(errStr, "auto-load in progress") || strings.Contains(errStr, "load started") {
		return fmt.Sprintf(
			"Model '%s' load has been triggered on cppworker (target n_ctx=%d). "+
				"cppworker is loading the model into VRAM — this typically takes 30-180 seconds for 2-3GB models "+
				"(observed on RTX-3070: 180s for 2048→4096 n_ctx reload, 90-120s for first load). "+
				"Please wait %d seconds and retry. "+
				"If you see this error repeatedly, reduce num_predict in your client "+
				"to avoid the wait on future requests.",
			diag.Model, diag.TargetNCtx, diag.RetryAfterSec)
	}
	// n_ctx reload (target n_ctx different from current)
	if strings.Contains(errStr, "n_ctx reload") {
		if diag.FeasibleMaxContext > 0 && diag.TargetNCtx > diag.FeasibleMaxContext {
			return fmt.Sprintf(
				"Requested n_ctx=%d exceeds feasible_max_context=%d for this model on this hardware. "+
					"Reduce num_predict to fit within feasible_max_context, "+
					"or save a model profile with smaller contextLength.",
				diag.TargetNCtx, diag.FeasibleMaxContext)
		}
		return fmt.Sprintf(
			"Model is being reloaded to n_ctx=%d. "+
				"cppworker needs to reload model with new context size — "+
				"this typically takes 30-180 seconds. Please retry in %d seconds.",
			diag.TargetNCtx, diag.RetryAfterSec)
	}
	// Circuit breaker open
	if strings.Contains(errStr, "circuit breaker open") {
		return "Backend is temporarily unavailable due to repeated failures. " +
			"Please wait 60 seconds and retry."
	}
	// Generic fallback (R60.41: "Auto-load failed" → "Model load failed")
	return fmt.Sprintf(
		"Model load failed: %v. Please retry in %d seconds.",
		loadErr, diag.RetryAfterSec)
}

// copyResponse — copy headers + status + body from an upstream http.Response
// to the client ResponseWriter. Closes the upstream body when done. Used for
// proxying raw responses from cppworker / llama.cpp backends when no body
// transformation is needed.
//
// Used by:
//   - llamacpp_handlers_admin.go (6 sites, for /api/show, /api/create, etc.
//     when backend response is passed through unchanged)
//   - ollama_router_admin.go (1 site, for handleShow)
//
// Pre-R51.1: defined in ollama_router.go:258.
// R51.1: moved here. No signature change.
//
// R60.15 (2026-09-07): preserve upstream X-Request-Id as X-Upstream-Request-Id
// (balancer already set its own X-Request-Id at proxy.go:524). Avoids
// duplicate X-Request-Id header in response.
func copyResponse(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()
	for key, values := range resp.Header {
		keyLower := strings.ToLower(key)
		// R60.15: rename upstream X-Request-Id to X-Upstream-Request-Id
		// to avoid duplicate headers.
		if keyLower == "x-request-id" {
			for _, value := range values {
				if existing := w.Header().Get("X-Upstream-Request-Id"); existing == "" {
					w.Header().Set("X-Upstream-Request-Id", value)
				}
			}
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// proxyRequestToBackend — R59.15c (2026-09-03): shared HTTP proxy helper
// used by both OllamaRouter.proxyHTTP and LlamaCppRouter.proxyHTTP.
//
// Replaces two near-identical implementations that differed only in HTTP
// client timeout:
//   - OllamaRouter: 30s (Ollama is fast, no long-poll endpoints)
//   - LlamaCppRouter: 120s (Round 21 — /api/show, /api/pull, /api/create
//     trigger cppworker lazy-load which can take 50-70s for 5GB models)
//
// Each router's proxyHTTP is now a thin wrapper that calls this with the
// appropriate timeout. The caller (handler) is unchanged.
//
// Returns the raw upstream http.Response. The caller is responsible for
// closing resp.Body.
func (p *Proxy) proxyRequestToBackend(r *http.Request, backendID string, timeout time.Duration) (*http.Response, error) {
	backend := p.GetBackend(backendID)
	if backend == nil {
		return nil, fmt.Errorf("backend not found: %s", backendID)
	}

	port := p.getBackendPort(backend)
	targetURL := fmt.Sprintf("http://%s:%d%s", backend.Host, port, r.URL.String())
	client := &http.Client{Timeout: timeout}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
	if err != nil {
		return nil, err
	}
	for key, values := range r.Header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	return client.Do(req)
}
