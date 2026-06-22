// router.go — HTTP router setup for all CppWorker endpoints.
package main

import (
	"net/http"
)

// setupRouter создаёт ServeMux с регистрацией всех эндпоинтов CppWorker.
func setupRouter() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/api/health", handleHealth)
	mux.HandleFunc("/info", handleInfo)
	mux.HandleFunc("/api/info", handleInfo)
	mux.HandleFunc("/api/gpu", handleGPUInfo)
	mux.HandleFunc("/api/models/load", handleLoadModel)
	mux.HandleFunc("/load", handleLoadModel)                        // alias for balancer warmup
	mux.HandleFunc("/api/models/load/progress", handleLoadProgress) // loading state polling
	mux.HandleFunc("/api/models/unload", handleUnloadModel)
	mux.HandleFunc("/api/models/reload", authMiddleware(handleReloadModel))
	mux.HandleFunc("/api/models", handleListModels)
	mux.HandleFunc("/api/model", handleGetModel)
	mux.HandleFunc("/api/models/files", handleListModelsDir)
	mux.HandleFunc("/api/models/delete", handleDeleteModel)
	mux.HandleFunc("/api/delete", handleDeleteModel) // Ollama-compatible alias
	mux.HandleFunc("/api/generate", handleGenerate)
	mux.HandleFunc("/api/chat", handleChat)
	mux.HandleFunc("/api/embeddings", handleOllamaEmbeddings)
	mux.HandleFunc("/api/ollama/generate", handleOllamaGenerate)
	mux.HandleFunc("/api/ollama/tags", handleOllamaTags)
	mux.HandleFunc("/api/tags", handleOllamaTags)
	mux.HandleFunc("/api/show", handleOllamaShow)
	mux.HandleFunc("/api/copy", handleOllamaCopy)
	mux.HandleFunc("/api/create", handleOllamaCreate)
	mux.HandleFunc("/api/push", handleOllamaPush)
	mux.HandleFunc("/api/version", handleCppWorkerVersion)
	mux.HandleFunc("/api/hf/search", handleHFSearch)
	mux.HandleFunc("/api/hf/files", handleHFFiles)
	mux.HandleFunc("/api/hf/download", handleHFDownload)
	mux.HandleFunc("/api/hf/progress", handleHFDownloadProgress)
	mux.HandleFunc("/api/hf/downloads", handleHFDownloads)
	mux.HandleFunc("/api/hf/cancel", handleHFCancel)
	mux.HandleFunc("/api/pull", handlePull)
	mux.HandleFunc("/api/v1/cppworker/config", handleCppWorkerGetConfig)
	mux.HandleFunc("/api/v1/cppworker/config/update", authMiddleware(handleCppWorkerUpdateConfig))
	mux.HandleFunc("/api/v1/cppworker/config/reload", authMiddleware(handleCppWorkerReloadConfig))
	mux.HandleFunc("/api/v1/cppworker/health", handleHealth)
	mux.HandleFunc("/api/v1/cppworker/metrics", handleInfo)
	// Diagnostics endpoints — помогают диагностировать проблемы с загрузкой моделей
	// через runtime JSON-ответ без чтения логов.
	mux.HandleFunc("/api/diagnostics", handleDiagnostics)
	mux.HandleFunc("/api/diagnostics/models", handleDiagnosticsModels)
	mux.HandleFunc("/api/diagnostics/load", handleDiagnosticsLoad)
	mux.HandleFunc("/api/diagnostics/clear", handleDiagnosticsClear)
	// OpenAI-совместимые /v1/ endpoints
	mux.HandleFunc("/v1/chat/completions", handleV1ChatCompletions)
	mux.HandleFunc("/v1/completions", handleV1Completions)
	mux.HandleFunc("/v1/embeddings", handleV1Embeddings)
	mux.HandleFunc("/v1/models", handleV1Models)
	return corsMiddleware(loggingMiddleware(mux))
}
