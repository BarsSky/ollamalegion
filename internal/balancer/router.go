package balancer

import (
	"net/http"

	"ollama-loadbalancer/pkg/logger"
)

// routeRequest — маршрутизация входящего HTTP запроса.
// Возвращает true если запрос обработан (health или Ollama API), false для основного flow.
func (p *Proxy) routeRequest(w http.ResponseWriter, r *http.Request) bool {
	// Health check
	if r.URL.Path == "/health" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("{\"status\":\"healthy\"}"))
		return true
	}

	// Ollama API routing (не /api/generate и не /api/chat)
	path := r.URL.Path
	isChatOrGenerate := (path == "/api/generate" || path == "/api/chat")
	if !isChatOrGenerate && p.ollamaRouter != nil && p.ollamaRouter.Route(w, r) {
		return true
	}

	return false
}

// isEmbeddingsRequest — проверяет, является ли запрос embeddings (skip stickiness)
func isEmbeddingsRequest(path string) bool {
	return path == "/api/embeddings" || path == "/api/embed"
}

// isChatOrGenerateRequest — проверяет, является ли запрос основным LLM-вызовом
func isChatOrGenerateRequest(path string) bool {
	return path == "/api/generate" || path == "/api/chat"
}

// logRoutingDecision — логирование решения о маршрутизации
func (p *Proxy) logRoutingDecision(path string, targetBackend string, sessionID string, reason string) {
	logger.Get().Debugw("routing decision",
		"path", path,
		"target_backend", targetBackend,
		"session_id", sessionID,
		"reason", reason,
	)
}