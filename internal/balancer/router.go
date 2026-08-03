package balancer

import (
	"net/http"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// routeRequest — маршрутизация входящего HTTP запроса.
// Возвращает true если запрос обработан (health или Ollama/llama.cpp API), false для основного flow.
func (p *Proxy) routeRequest(w http.ResponseWriter, r *http.Request) bool {
	// Health check
	if r.URL.Path == "/health" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("{\"status\":\"healthy\"}"))
		return true
	}

	// Ollama/llama.cpp API routing
	bt := p.determineRequestBackendType(r)
	logger.Get().Infow("routeRequest: backend type resolved",
		"path", r.URL.Path,
		"backend_type", bt,
		"method", r.Method)

	// Инференс-запросы (/api/chat, /api/generate) в смешанных режимах
	// (bt == "") должны идти через основной flow ServeHTTP, где selectBackend
	// выбирает конкретный бэкенд по модели/ресурсам. Специализированные роутеры
	// перехватывают эти пути только когда режим жёстко привязан к llama.cpp.
	if bt == "" && isChatOrGenerateRequest(r.URL.Path) {
		logger.Get().Infow("routeRequest: mixed mode, falling through to selectBackend",
			"path", r.URL.Path)
		return false
	}

	// Смешанный режим (bt == ""): read-only/админ endpoint'ы /api/*.
	// Выбираем порядок роутеров на основе фактических типов бэкендов:
	// если Ollama-бэкендов нет, а llama.cpp есть — пробуем LlamaCppRouter
	// первым, чтобы не получить пустой ответ от OllamaRouter.
	if bt == "" {
		counts := p.countBackendsByType()
		hasOllama := counts[types.BackendTypeOllama] > 0
		hasLlama := counts[types.BackendTypeLlamaCpp] > 0

		if hasOllama {
			if p.ollamaRouter != nil && p.ollamaRouter.Route(w, r) {
				return true
			}
			if p.llamaCppRouter != nil && p.llamaCppRouter.Route(w, r) {
				return true
			}
		} else if hasLlama {
			if p.llamaCppRouter != nil && p.llamaCppRouter.Route(w, r) {
				return true
			}
			if p.ollamaRouter != nil && p.ollamaRouter.Route(w, r) {
				return true
			}
		} else {
			// Нет бэкендов — стандартный fallback: OllamaRouter, затем llama.cpp
			if p.ollamaRouter != nil && p.ollamaRouter.Route(w, r) {
				return true
			}
			if p.llamaCppRouter != nil && p.llamaCppRouter.Route(w, r) {
				return true
			}
		}
		return false
	}

	// Режим жёстко привязан к llama.cpp
	if bt == types.BackendTypeLlamaCpp && p.llamaCppRouter != nil {
		if p.llamaCppRouter.Route(w, r) {
			return true
		}
	}

	// Режим жёстко привязан к Ollama
	if bt == types.BackendTypeOllama && p.ollamaRouter != nil {
		if p.ollamaRouter.Route(w, r) {
			return true
		}
	}

	return false
}

// isEmbeddingsRequest — проверяет, является ли запрос embeddings (skip stickiness)
func isEmbeddingsRequest(path string) bool {
	return path == "/api/embeddings" || path == "/api/embed"
}

// Round 22 (2026-08-03): endpoints которые НЕ требуют загруженной модели в VRAM.
// Для них warmup/sync-load ПРОПУСКАЕТСЯ — иначе balancer зависает на 10-30s
// ожидая load модели, которая этому endpoint'у не нужна.
//
// Read-only (информация о моделях/бэкендах):
//   - /api/version      (Ollama version)
//   - /api/tags         (список моделей)
//   - /api/ps           (running processes)
//   - /v1/models        (OpenAI models)
//   - /api/models       (cppworker state)
//   - /api/models/files (cppworker filesystem scan)
//   - /api/show         (model info — НЕ требует loaded модели, файл на диске)
//
// Model management (Ollama registry-style):
//   - /api/pull         (скачать модель)
//   - /api/push         (залить модель в registry, cppworker отдаёт 501)
//   - /api/copy         (скопировать)
//   - /api/delete       (удалить)
//   - /api/create       (создать из Modelfile)
//
// Blob upload/download:
//   - /api/blobs/*      (digest-based file storage)
//
// Embeddings — отдельная категория: требует загруженную модель, но НЕ должна
// триггерить sync-warmup (т.к. embeddings — короткие операции, лучше 503
// чем 30s wait). Поэтому их тут НЕТ — они идут через обычный flow.
func isReadOnlyOrMgmtEndpoint(path string) bool {
	switch path {
	case "/api/version", "/api/tags", "/api/ps", "/v1/models",
		"/api/models", "/api/models/files", "/api/show",
		"/api/pull", "/api/push", "/api/copy", "/api/delete", "/api/create":
		return true
	}
	// /api/blobs/<digest> — upload/download
	if len(path) >= len("/api/blobs/") && path[:len("/api/blobs/")] == "/api/blobs/" {
		return true
	}
	return false
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