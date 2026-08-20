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
	"io"
	"net/http"
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
func copyResponse(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
