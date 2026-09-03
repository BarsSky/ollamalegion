package balancer

import (
	"net/http"
	"strings"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// isAPIv1Path — Round 40 #3 (2026-08-18): проверяет, является ли путь
// admin / management endpoint'ом, обслуживаемым API-сервером (порт 18081).
// Все пути под /api/v1/ (cluster/*, cppworker/*, events SSE, rpc/tp/*, gguf/*,
// llama-cpp/overrides, и т.д.) живут на API сервере, не на proxy flow.
//
// Без этой проверки balancer проксирует /api/v1/* на backend → 404 (cppworker
// не знает /api/v1/cluster/* и т.п.) → 30s timeout (Round 22 BUG #7).
func isAPIv1Path(path string) bool {
	return strings.HasPrefix(path, "/api/v1/")
}

// serveAPIv1Request — Round 40 #3 (2026-08-18): forward /api/v1/* запросы
// на локальный API-сервер. Использует cached httputil.ReverseProxy
// (initialized в NewProxy) — не делает DNS lookup на каждый запрос.
//
// Авторизация: API сервер применяет свой AuthMiddleware к /api/v1/* routes
// (см. internal/api/routes.go). Мы только пробрасываем оригинальные
// заголовки (X-API-Token / Authorization: Bearer) без изменений —
// default Director в httputil.NewSingleHostReverseProxy это делает.
func (p *Proxy) serveAPIv1Request(w http.ResponseWriter, r *http.Request) bool {
	if p.apiReverseProxy == nil {
		// Defensive: NewProxy должен был инициализировать. Не должно случаться
		// в production, но если API port не настроен — лучше 503 чем panic.
		logger.Get().Errorw("apiReverseProxy not initialized, returning 503",
			"path", r.URL.Path)
		http.Error(w, `{"error":"api_server_not_configured","message":"API server not configured in balancer"}`,
			http.StatusServiceUnavailable)
		return true
	}
	logger.Get().Debugw("forwarding /api/v1/* to API server",
		"path", r.URL.Path, "method", r.Method)
	p.apiReverseProxy.ServeHTTP(w, r)
	return true
}

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

	// Round 40 #3 (2026-08-18): /api/v1/* пути — admin / management endpoints
	// обслуживаемые API-сервером на порту 18081, а не backend'ами.
	// Forward'им напрямую — иначе balancer проксирует на cppworker →
	// 404 → 30s timeout (Round 22 BUG #7).
	if isAPIv1Path(r.URL.Path) {
		return p.serveAPIv1Request(w, r)
	}

	// Round 22 (2026-08-03): early 404 для OpenAI endpoints, которые мы НЕ
	// поддерживаем (audio/images). Без этого balancer проксирует на
	// backend → 404 → ждёт 30s timeout (Round 22 BUG #7). Клиент сразу
	// получает 404 + понятное сообщение.
	if isUnsupportedOpenAIEndpoint(r.URL.Path) || isUnsupportedOllamaEndpoint(r.URL.Path) {
		round22Early404Total.Add(1)
		logger.Get().Debugw("routeRequest: unsupported endpoint, returning 404",
			"path", r.URL.Path, "method", r.Method)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"not_supported","message":"this endpoint is not implemented in OllamaLegion balancer","path":"` + r.URL.Path + `"}`))
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

	// R59.15b (2026-09-03): build priority-ordered list of routers and
	// dispatch through a single helper. Replaces three repeated `if X != nil
	// && X.Route(w, r)` ladders. The order is what matters:
	//   • mixed mode with Ollama backends → Ollama first, then llama.cpp
	//     (Ollama has the most native /api/* support)
	//   • mixed mode with only llama.cpp → llama.cpp first, then Ollama
	//     fallback (avoid empty aggregate responses)
	//   • no backends registered → Ollama first as default
	//   • locked llama.cpp mode → only llama.cpp router
	//   • locked Ollama mode → only Ollama router
	routers := p.buildRoutersForDispatch(bt)
	if p.dispatchRouters(routers, w, r) {
		return true
	}
	return false
}

// buildRoutersForDispatch — R59.15b: returns the priority-ordered list of
// BackendRouter instances to try for the given request backend type. nil
// routers are preserved in the list so dispatchRouters can skip them
// uniformly (instead of inlining nil checks at each call site).
func (p *Proxy) buildRoutersForDispatch(bt types.BackendType) []BackendRouter {
	// Locked modes: only one router is relevant.
	switch bt {
	case types.BackendTypeLlamaCpp:
		return []BackendRouter{p.llamaCppRouter}
	case types.BackendTypeOllama:
		return []BackendRouter{p.ollamaRouter}
	}

	// Mixed mode (bt == ""): order by which backend types are registered.
	counts := p.countBackendsByType()
	hasOllama := counts[types.BackendTypeOllama] > 0
	hasLlama := counts[types.BackendTypeLlamaCpp] > 0

	switch {
	case hasOllama:
		// Ollama-приоритет: большинство /api/* endpoint'ов нативные.
		return []BackendRouter{p.ollamaRouter, p.llamaCppRouter}
	case hasLlama:
		// Только llama.cpp: пробуем его первым, иначе OllamaRouter вернёт
		// пустой aggregate (нельзя делать aggregation без Ollama backends).
		return []BackendRouter{p.llamaCppRouter, p.ollamaRouter}
	default:
		// Нет зарегистрированных бэкендов — стандартный fallback: Ollama,
		// затем llama.cpp (для случая когда router инициализирован, но
		// backends ещё не подключились).
		return []BackendRouter{p.ollamaRouter, p.llamaCppRouter}
	}
}

// dispatchRouters — R59.15b: единая диспетчеризация по приоритетному списку
// BackendRouter. Возвращает true если любой router в списке обработал запрос.
// nil-элементы пропускаются.
func (p *Proxy) dispatchRouters(routers []BackendRouter, w http.ResponseWriter, r *http.Request) bool {
	for _, router := range routers {
		if router == nil {
			continue
		}
		if router.Route(w, r) {
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

// isUnsupportedOpenAIEndpoint — OpenAI endpoints, которые OllamaLegion НЕ
// реализует (audio/images/etc). Возвращаем early 404 вместо проксирования
// на backend → 30s timeout (Round 22 BUG #7).
func isUnsupportedOpenAIEndpoint(path string) bool {
	switch {
	case strings.HasPrefix(path, "/v1/audio/"):
		return true
	case strings.HasPrefix(path, "/v1/images/"):
		return true
	case path == "/v1/realtime":
		return true
	case path == "/v1/fine_tuning/jobs" || strings.HasPrefix(path, "/v1/fine_tuning/"):
		return true
	case path == "/v1/batches" || strings.HasPrefix(path, "/v1/batches/"):
		return true
	case path == "/v1/assistants" || strings.HasPrefix(path, "/v1/assistants/"):
		return true
	case path == "/v1/threads" || strings.HasPrefix(path, "/v1/threads/"):
		return true
	}
	return false
}

// isUnsupportedOllamaEndpoint — Ollama endpoints, которые OllamaLegion НЕ
// реализует (signin/logout/web UI). Возвращаем early 404 чтобы не висеть
// 30s на проксировании (Round 22 BUG #11).
func isUnsupportedOllamaEndpoint(path string) bool {
	switch {
	case path == "/api/signin":
		return true
	case path == "/api/logout":
		return true
	case strings.HasPrefix(path, "/api/web/"):
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