package api

import (
	"net/http"
	"strings"
)

// ggufBackendProxyPath — общий диспетчер для проксирования запросов
// от WebUI к CppWorker конкретного llama_cpp бэкенда.
//
// Дополнительный endpoint (Session 4 — P-1):
//
//	POST /api/v1/gguf/backends/{id}/proxy/api/models/load-with-params
//
// Поддерживает расширенные llama.cpp-параметры (nThreads, parallel,
// kvCacheType, splitMode, overrideTensor) для тонкой настройки загрузки
// модели. Пример body:
//
//	{
//	  "name": "gemma-4-E4B-it-Q4_K_M",
//	  "contextSize": 32768,
//	  "gpuLayers": -2,           // -2 = AUTO
//	  "kvCacheType": 1,           // 1 = Q8_0 (50% VRAM savings)
//	  "parallel": 2,              // batched generation
//	  "nThreads": 16,
//	  "overrideTensor": "blk\\..*\\.ffn_.*_exps=CPU"
//	}
//
// Базовые поля (name, path, contextSize, batchSize, gpuLayers, flashAttnType,
// numa, useMmap, tensorSplit) полностью совместимы с /api/models/load.
//
// URL-формат:
//
//	GET  /api/v1/gguf/backends/{id}/proxy/info
//	GET  /api/v1/gguf/backends/{id}/proxy/api/gpu
//	GET  /api/v1/gguf/backends/{id}/proxy/api/models
//	GET  /api/v1/gguf/backends/{id}/proxy/api/models/files
//	GET  /api/v1/gguf/backends/{id}/proxy/api/models/load
//	POST /api/v1/gguf/backends/{id}/proxy/api/models/load
//	POST /api/v1/gguf/backends/{id}/proxy/api/models/unload
//	POST /api/v1/gguf/backends/{id}/proxy/api/models/delete
//	GET  /api/v1/gguf/backends/{id}/proxy/api/hf/search
//	GET  /api/v1/gguf/backends/{id}/proxy/api/hf/files
//	POST /api/v1/gguf/backends/{id}/proxy/api/hf/download
//	GET  /api/v1/gguf/backends/{id}/proxy/api/hf/progress
//	GET  /api/v1/gguf/backends/{id}/proxy/api/hf/downloads
//	POST /api/v1/gguf/backends/{id}/proxy/api/hf/cancel
//
// Префикс /api/v1/gguf/backends/{id}/proxy/ проглатывается диспетчером, остаток
// передаётся в CppWorker как есть.
//
// Это позволяет WebUI общаться с CppWorker через балансер, минуя прямые
// fetch-запросы из браузера (которые ломаются в Docker-окружении из-за CORS
// и недоступности host.docker.internal).
func (s *Server) handleGgufBackendProxy(w http.ResponseWriter, r *http.Request) {
	// Извлекаем backendID и относительный путь.
	// Полный путь: /api/v1/gguf/backends/{id}/proxy/...
	id, relPath, ok := splitGgufBackendProxyPath(r.URL.Path)
	if !ok {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid proxy path",
		})
		return
	}

	// Метод ограничиваем только теми, что имеет смысл проксировать.
	switch r.Method {
	case http.MethodGet, http.MethodPost, http.MethodDelete, http.MethodPut:
		// OK
	default:
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error": "method not allowed",
		})
		return
	}

	s.proxyToCppWorker(w, r, id, relPath)
}

// splitGgufBackendProxyPath — парсит /api/v1/gguf/backends/{id}/proxy/... и
// возвращает backendID и путь относительно CppWorker (с ведущим /).
func splitGgufBackendProxyPath(fullPath string) (backendID, relPath string, ok bool) {
	const prefix = "/api/v1/gguf/backends/"
	if !strings.HasPrefix(fullPath, prefix) {
		return "", "", false
	}
	rest := fullPath[len(prefix):]
	// rest = "{id}/proxy/..."
	slash := strings.Index(rest, "/")
	if slash < 0 {
		return "", "", false
	}
	backendID = rest[:slash]
	rest = rest[slash+1:] // после первого слэша
	// Должно начинаться с "proxy/"
	if !strings.HasPrefix(rest, "proxy/") {
		return "", "", false
	}
	relPath = "/" + rest[len("proxy/"):]
	if relPath == "/" {
		relPath = "/"
	}
	return backendID, relPath, true
}