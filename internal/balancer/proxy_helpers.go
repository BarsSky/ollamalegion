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

// writeAutoLoadRetryAfter — R60.33 (2026-09-10): вычисляет правильный
// Retry-After в зависимости от причины auto-load failure. Если load
// идёт async (R60.33) — 30s (модель загрузится). Если failed
// (circuit breaker и т.д.) — больше.
//
// ВСЕГДА возвращает >=1 (writeServiceUnavailable default 30s, но
// мы explicit ставим правильное значение для ясности).
func writeAutoLoadRetryAfter(loadErr error) int {
	if loadErr == nil {
		return 30
	}
	errStr := loadErr.Error()
	// R60.33 async mode: модель в процессе загрузки. Клиент retry
	// через 30s (типичное время load Qwen3-7B).
	if strings.Contains(errStr, "auto-load in progress") {
		return 30
	}
	// R60.6 async-reload: n_ctx reload. 30s.
	if strings.Contains(errStr, "n_ctx reload") {
		return 30
	}
	// R60.26 circuit breaker: 60s (backoff).
	if strings.Contains(errStr, "circuit breaker open") {
		return 60
	}
	// Default 30s.
	return 30
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
