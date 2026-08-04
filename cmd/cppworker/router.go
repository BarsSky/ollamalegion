// router.go ? HTTP router setup for all CppWorker endpoints.
package main

import (
	"net/http"
)

// setupRouter ??????? ServeMux ? ???????????? ???? ?????????? CppWorker.
func setupRouter() http.Handler {
	mux := http.NewServeMux()
	// Initialize adaptive loader (EnvironmentProfile, NaN-healer, strategies)
	EnsureAdaptiveLoaderInit()
	// Register adaptive routes from adaptive_integration.go
	for _, r := range additionalRoutes {
		mux.HandleFunc(r.pattern, r.handler)
	}
	// ????????????? ??????????? ?????????? (EnvironmentProfile, NaN-healer, ?????????)
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/api/health", handleHealth)
	mux.HandleFunc("/info", handleInfo)
	mux.HandleFunc("/api/info", handleInfo)
	mux.HandleFunc("/api/gpu", handleGPUInfo)
	mux.HandleFunc("/api/models/load", handleLoadModel)
	mux.HandleFunc("/api/models/load-with-params", handleLoadWithParams) // ??????????? ????????? (Session 4 P-1)
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
	mux.HandleFunc("/api/embed", handleOllamaEmbed) // Round 21: Ollama v0.1.14+ new-style embeddings (OpenWebUI 0.4+)
	// /v1/embeddings — registered below as handleV1Embeddings (cmd/cppworker/handlers_openai.go:1553)
	mux.HandleFunc("/api/ollama/generate", handleOllamaGenerate)
	mux.HandleFunc("/api/ollama/tags", handleOllamaTags)
	mux.HandleFunc("/api/tags", handleOllamaTags)
	mux.HandleFunc("/api/ps", handleOllamaPS) // Round 21: running models in memory
	mux.HandleFunc("/api/show", handleOllamaShow)
	mux.HandleFunc("/api/copy", handleOllamaCopy)
	mux.HandleFunc("/api/create", handleOllamaCreate)
	mux.HandleFunc("/api/push", handleOllamaPush)
	mux.HandleFunc("/api/version", handleCppWorkerVersion)
	mux.HandleFunc("/api/cancel", handleCancel) // Round 18 P0.2: cancel active generation
	mux.HandleFunc("/api/infer/active", handleInferActive) // Round 18 P1.4: list active generations
	mux.HandleFunc("/api/infer/users", handleInferUsers) // Round 18 P0.3: per-user parallel counters
	mux.HandleFunc("/api/infer/metrics", handleInferMetrics) // Round 18 P1.4: per-model metrics with percentiles
	mux.HandleFunc("/api/hf/search", handleHFSearch)
	mux.HandleFunc("/api/hf/files", handleHFFiles)
	mux.HandleFunc("/api/hf/download", handleHFDownload)
	mux.HandleFunc("/api/hf/progress", handleHFDownloadProgress)
	mux.HandleFunc("/api/hf/downloads", handleHFDownloads)
	mux.HandleFunc("/api/hf/cancel", handleHFCancel)
	// Round 17.3 (2026-08-03): cleanup endpoint — удаляет скачанный/частичный
	// файл из контейнера. Поддерживает DELETE (с query params) и POST (с body).
	mux.HandleFunc("/api/hf/cleanup", handleHFCleanup)
	mux.HandleFunc("/api/pull", handlePull)
	mux.HandleFunc("/api/v1/cppworker/config", handleCppWorkerGetConfig)
	mux.HandleFunc("/api/v1/cppworker/config/update", authMiddleware(handleCppWorkerUpdateConfig))
	mux.HandleFunc("/api/v1/cppworker/config/reload", authMiddleware(handleCppWorkerReloadConfig))
	mux.HandleFunc("/api/v1/cppworker/config/runtime", handleCppWorkerRuntimeConfig)
	mux.HandleFunc("/api/v1/cppworker/reset-reload-counter", authMiddleware(handleResetReloadCounter))
	mux.HandleFunc("/api/v1/cppworker/health", handleHealth)
	mux.HandleFunc("/api/v1/cppworker/metrics", handleInfo)
	// ??????????? prompt-too-long: ?????????? ?????????? inference-???????
	// (??? runbook ???????? B ? ??? ??????????? ??????? "prompt_exceeds_context" ? Cline).
	mux.HandleFunc("/api/v1/cppworker/debug/last-prompt", authMiddleware(handleDebugLastPrompt))
	// ??????????? stream disconnect: ????????? snapshot ?????? ??????
	// (Issue ?????? ?????? ??? ?????-???? ???????). ???????? ??????, ??? ??
	// ??? ctx.Done() (?????? ?????????) ??? write error (broken pipe).
	mux.HandleFunc("/api/v1/cppworker/debug/last-stream", authMiddleware(handleDebugLastStream))
	mux.HandleFunc("/api/v1/cppworker/debug/last-stream/clear", authMiddleware(handleDebugLastStreamClear))
	// Adaptive loader API (?????????????? ????? init() ? adaptive_integration.go)
	// Diagnostics endpoints ? ???????? ??????????????? ???????? ? ????????? ???????
	// ????? runtime JSON-????? ??? ?????? ?????.
	mux.HandleFunc("/api/diagnostics", handleDiagnostics)
	mux.HandleFunc("/api/diagnostics/models", handleDiagnosticsModels)
	mux.HandleFunc("/api/diagnostics/load", handleDiagnosticsLoad)
	mux.HandleFunc("/api/diagnostics/clear", handleDiagnosticsClear)
	// OpenAI-??????????? /v1/ endpoints
	mux.HandleFunc("/v1/chat/completions", handleV1ChatCompletions)
	mux.HandleFunc("/v1/completions", handleV1Completions)
	mux.HandleFunc("/v1/embeddings", handleV1Embeddings)
	mux.HandleFunc("/v1/models", handleV1Models)
	// F.0b (2026-06-28): session F ? recoverMiddleware ??? ????? ??????? ????,
	// catch'?? panic ?? corsMiddleware / loggingMiddleware / ?????? handler'?.
	// ??? ????? panic ???????? ? EOF ??? ??????????? ??? ???????.
	return recoverMiddleware(corsMiddleware(loggingMiddleware(mux)))
}

