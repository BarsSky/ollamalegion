package balancer

// ollama_router.go — Audit 2026-08-17 B3 refactor: после split содержит
// ТОЛЬКО OllamaRouter struct + Route dispatcher. Helpers вынесены в
// ollama_router_helpers.go, handlers — в ollama_router_tags.go и
// ollama_router_admin.go.
//
// Route — диспетчер Ollama API endpoint'ов. Возвращает true если запрос
// обработан (handled); false = fall through в main proxy flow.

import (
	"net/http"
)

// OllamaRouter — маршрутизатор для Ollama API endpoint'ов
type OllamaRouter struct {
	proxy *Proxy
}

// NewOllamaRouter — создание маршрутизатора
func NewOllamaRouter(proxy *Proxy) *OllamaRouter {
	return &OllamaRouter{proxy: proxy}
}

// Route — диспетчеризация запроса по URL.Path.
// Возвращает true если запрос был обработан.
//
// Категории:
//   - Read-only: /api/tags, /api/version, /api/show  → handlers в ollama_router_tags.go
//   - Admin:     /api/create, /api/pull, /api/delete, /api/copy, /api/push
//     → handlers в ollama_router_admin.go
//   - Passthrough (false): /api/embed, /api/ps
//     /api/embed (Ollama v0.1.14+) → cppworker native handler
//     /api/ps → LlamaCppRouter (reads from llamaMetrics[id].LoadedModels, Round 21)
func (or *OllamaRouter) Route(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	// === Read-only ===
	case "/api/tags":
		or.handleTags(w, r)
		return true
	case "/api/version":
		or.handleVersion(w, r)
		return true
	case "/api/show":
		or.handleShow(w, r)
		return true

	// === Admin ===
	case "/api/create":
		or.handleCreate(w, r)
		return true
	case "/api/pull":
		or.handlePull(w, r)
		return true
	case "/api/delete":
		or.handleDelete(w, r)
		return true
	case "/api/copy":
		or.handleCopy(w, r)
		return true
	case "/api/push":
		or.handlePush(w, r)
		return true

	// === Passthrough (handled by main proxy / LlamaCppRouter) ===
	case "/api/embed":
		// Round 21: Ollama v0.1.14+ new-style embeddings. Path passes through
		// to cppworker (which now natively supports /api/embed).
		return false
	case "/api/ps":
		// Round 21: don't proxy /api/ps to Ollama backend. Fall through to
		// LlamaCppRouter which reads from llamaMetrics[id].LoadedModels.
		return false
	}
	return false
}
