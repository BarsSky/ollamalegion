package balancer

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// LlamaCppRouter — маршрутизатор для llama.cpp backend endpoint'ов.
// Аналогичен OllamaRouter, но использует BackendTypeLlamaCpp для выбора бэкендов
// и ищет модели в метриках llama.cpp вместо Ollama.
type LlamaCppRouter struct {
	proxy *Proxy
	// lastKnownModels + lastKnownModelsAt — кэш последнего успешного ответа
	// /api/models per backend. Используется в queryCppWorkerModels при EOF
	// во время reload cppworker (см. cppWorkerLastKnown комментарий).
	lastKnownModelsMu sync.RWMutex
	lastKnownModels   map[string][]cppWorkerModelState
	lastKnownModelsAt map[string]time.Time
	// loadBackoff — circuit breaker для failed auto-load (Round 26 v0.5.14).
	// После maxConsecutiveFailures подряд failures, breaker открывается
	// на breakerOpenDuration — balancer возвращает ошибку сразу без retry,
	// предотвращая infinite retry loop при upstream bug (например,
	// gemma-4 GGML_ASSERT при n_ctx>32768).
	loadBackoff *loadBackoff
}

// NewLlamaCppRouter — создание маршрутизатора для llama.cpp
func NewLlamaCppRouter(proxy *Proxy) *LlamaCppRouter {
	return &LlamaCppRouter{
		proxy:             proxy,
		lastKnownModels:   make(map[string][]cppWorkerModelState),
		lastKnownModelsAt: make(map[string]time.Time),
		loadBackoff:       newLoadBackoff(),
	}
}

// Route — диспетчеризация запроса по URL.Path.
// Возвращает true если запрос был обработан.
//
// Поддерживает префиксы /ollama/* и /openai/* (используются OpenWebUI):
//   - /ollama/api/version  → strip → /api/version   → handleVersion
//   - /ollama/api/chat     → strip → /api/chat      → handleChat
//   - /ollama/api/tags     → strip → /api/tags      → handleTags
//   - /openai/v1/models    → strip → /v1/models     → handleOpenAIModels
//   - /openai/v1/chat/completions → strip → /v1/chat/completions → handleOpenAIChatCompletions
//
// Без этой нормализации OpenWebUI получает 500 на любой запрос к балансеру
// (потому что в switch нет case'ов /ollama/* и /openai/*, и proxy flow падает
// с пустым model / неизвестным путём).
func (lr *LlamaCppRouter) Route(w http.ResponseWriter, r *http.Request) bool {
	// Нормализация префиксов OpenWebUI: /ollama/* и /openai/*
	// делаем r.URL.Path копию чтобы не мутировать входящий *http.Request
	path := r.URL.Path
	switch {
	case strings.HasPrefix(path, "/ollama/"):
		path = strings.TrimPrefix(path, "/ollama")
		r2 := r.Clone(r.Context())
		r2.URL.Path = path
		r = r2
	case strings.HasPrefix(path, "/openai/"):
		path = strings.TrimPrefix(path, "/openai")
		r2 := r.Clone(r.Context())
		r2.URL.Path = path
		r = r2
	}

	switch r.URL.Path {
	case "/api/tags":
		lr.handleTags(w, r)
		return true
	case "/api/version":
		lr.handleVersion(w, r)
		return true
	case "/api/ps":
		lr.handlePS(w, r)
		return true
	case "/api/show":
		lr.handleShow(w, r)
		return true
	case "/api/create":
		lr.handleCreate(w, r)
		return true
	case "/api/pull":
		lr.handlePull(w, r)
		return true
	case "/api/delete":
		lr.handleDelete(w, r)
		return true
	case "/api/copy":
		lr.handleCopy(w, r)
		return true
	case "/api/push":
		lr.handlePush(w, r)
		return true
	case "/api/chat":
		lr.handleChat(w, r)
		return true
	case "/api/generate":
		lr.handleGenerate(w, r)
		return true
	case "/api/models":
		// Нативный cppworker endpoint: {count, models:[{name, path, state, sizeBytes, ...}]}.
		// Агрегируем по всем llama.cpp бэкендам.
		lr.handleModels(w, r)
		return true
	case "/api/v1/cppworker/config/runtime":
		// Агрегированные runtime-параметры загруженных моделей (n_ctx,
		// gpu_layers, batch_size, flash_attn, n_layers и т.д.) со всех
		// llama.cpp бэкендов. Используется WebUI на вкладке GGUF Models.
		lr.handleRuntimeConfig(w, r)
		return true
	// OpenAI-совместимые пути (для OpenWebUI, который вызывает /openai/v1/*)
	case "/v1/models":
		lr.handleOpenAIModels(w, r)
		return true
	case "/v1/chat/completions":
		lr.handleOpenAIChatCompletions(w, r)
		return true
	case "/v1/completions":
		// Round 15.1 follow-up (2026-07-29): /v1/completions (legacy text completion)
		// теперь идёт через dedicated handler вместо main proxy flow → queue manager.
		// Без этого: balancer блокирует запрос с "all backends busy" при высокой
		// VRAM загрузке (>85%) потому что headroom check в queue_manager.go
		// не пропускает запросы пока gpuHeadroomPercent (15%) не освободится.
		// /v1/chat/completions (handleOpenAIChatCompletions выше) делает direct
		// dispatch и работает; /v1/completions шёл в общий flow и блокировался.
		lr.handleOpenAICompletion(w, r)
		return true
	case "/v1/embeddings":
		// Round 22 (2026-08-03): /v1/embeddings тоже direct dispatch.
		// Без этого: после загрузки модели (VRAM > 85%) embeddings застревают
		// в queue_manager с 503 (slot blocked из-за headroom 15%).
		lr.handleOpenAIEmbeddings(w, r)
		return true
	}
	return false
}
