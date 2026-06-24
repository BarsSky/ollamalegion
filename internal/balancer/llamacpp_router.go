package balancer

import (
	"net/http"
	"strings"
)

// LlamaCppRouter — маршрутизатор для llama.cpp backend endpoint'ов.
// Аналогичен OllamaRouter, но использует BackendTypeLlamaCpp для выбора бэкендов
// и ищет модели в метриках llama.cpp вместо Ollama.
type LlamaCppRouter struct {
	proxy *Proxy
}

// NewLlamaCppRouter — создание маршрутизатора для llama.cpp
func NewLlamaCppRouter(proxy *Proxy) *LlamaCppRouter {
	return &LlamaCppRouter{proxy: proxy}
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
	}
	return false
}
