// router.go ? HTTP router setup for all CppWorker endpoints.
package main

import (
	"net/http"

	"ollama-loadbalancer/pkg/observability"
)

// setupRouter создаёт ServeMux с регистрацией всех эндпоинтов CppWorker.
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
	// R118.15 (2026-10-14): замер канала до узла раскладки средствами продукта —
	// отдаём N байт известного объёма, время скачивания засекает балансер.
	mux.HandleFunc("/api/rpc-link-probe", handleLinkProbe)
	mux.HandleFunc("/api/models/load", authMiddleware(handleLoadModel))
	mux.HandleFunc("/api/models/load-with-params", authMiddleware(handleLoadWithParams))         // ??????????? ????????? (Session 4 P-1)
	mux.HandleFunc("/load", authMiddleware(handleLoadModel))                                     // alias for balancer warmup
	mux.HandleFunc("/api/models/load/progress", handleLoadProgress)              // loading state polling
	mux.HandleFunc("/api/models/load/progress/stream", handleLoadProgressStream) // Round 25: SSE stream
	mux.HandleFunc("/api/models/unload", authMiddleware(handleUnloadModel))
	mux.HandleFunc("/api/models/reload", authMiddleware(handleReloadModel))
	mux.HandleFunc("/api/models/active-queries", handleGetActiveQueries) // Round 26 v0.5.13: WebUI busy badge
	mux.HandleFunc("/api/models", handleListModels)
	mux.HandleFunc("/api/model", handleGetModel)
	// R116: эффективные параметры модели — «что применится и почему» (профиль
	// против дефолта бэкенда, плюс вердикт памяти тем же расчётом, что загрузка).
	mux.HandleFunc("/api/models/effective", handleModelEffective)
	mux.HandleFunc("/api/models/files", handleListModelsDir)
	// R93 (2026-10-09): принудительная проверка каталога моделей на диске
	// (кнопка «Обновить» в WebUI и force-refresh каталога балансером).
	mux.HandleFunc("/api/models/refresh", handleRefreshModelsDir)
	mux.HandleFunc("/api/models/delete", authMiddleware(handleDeleteModel))
	mux.HandleFunc("/api/delete", authMiddleware(handleDeleteModel)) // Ollama-compatible alias
	// R89: перенос модели между бэкендами (источник отдаёт файл, приёмник
	// сохраняет). Под authMiddleware — как /api/models/reload: это чтение файлов
	// и запись в каталог моделей, а не публичный трафик генерации. Если токен в
	// воркере не сконфигурирован, middleware пропускает (legacy-поведение).
	mux.HandleFunc("/api/models/export", authMiddleware(handleExportModel))
	mux.HandleFunc("/api/models/import", authMiddleware(handleImportModel))
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
	mux.HandleFunc("/api/cancel", handleCancel)              // Round 18 P0.2: cancel active generation
	mux.HandleFunc("/api/infer/active", handleInferActive)   // Round 18 P1.4: list active generations
	mux.HandleFunc("/api/infer/users", handleInferUsers)     // Round 18 P0.3: per-user parallel counters
	mux.HandleFunc("/api/infer/metrics", handleInferMetrics) // Round 18 P1.4: per-model metrics with percentiles
	mux.HandleFunc("/metrics", handlePrometheusMetrics)      // Round 22 deferred: Prometheus exposition format
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
	// R125 (2026-10-16, пункт 3): какие узлы раскладки РЕАЛЬНО отвечают.
	// Отдельная ручка, потому что проверка — сетевой I/O с таймаутом, а конфиг
	// читают и в фоне (балансер, WebUI).
	mux.HandleFunc("/api/v1/cppworker/rpc-peers", handleCppWorkerRPCPeers)
	// R172 (2026-10-18): вывод ВСТРОЕННОГО узла раскладки (дочерний rpc-server)
	// и причина его смерти. Закрыт токеном: в строках видны пути моделей и
	// параметры буферов, а ручка нужна оператору, а не клиенту.
	mux.HandleFunc("/api/v1/cppworker/rpc-server/log", authMiddleware(handleRPCServerLog))
	// R129 (2026-10-16, пункт 3): приёмник замера пропускной способности между
	// узлами. Дорогая ручка (десятки МБ трафика) — вызывается только по явному
	// запросу мастера/оператора, не поллером.
	mux.HandleFunc("/api/v1/cppworker/link-bench", handleCppWorkerLinkBench)
	mux.HandleFunc("/api/v1/cppworker/reset-reload-counter", authMiddleware(handleResetReloadCounter))
	mux.HandleFunc("/api/v1/cppworker/health", handleHealth)
	mux.HandleFunc("/api/v1/cppworker/metrics", handleInfo)
	// Диагностика prompt-too-long: метаданные последнего inference-запроса
	// (для runbook сценария B и для диагностики случаев "prompt_exceeds_context" в Cline).
	mux.HandleFunc("/api/v1/cppworker/debug/last-prompt", authMiddleware(handleDebugLastPrompt))
	// Диагностика stream disconnect: последний snapshot обрыва стрима
	// (Issue «обрыв ответа без каких-либо ошибок»). Помогает понять, был ли
	// это ctx.Done() (клиент отвалился) или write error (broken pipe).
	mux.HandleFunc("/api/v1/cppworker/debug/last-stream", authMiddleware(handleDebugLastStream))
	mux.HandleFunc("/api/v1/cppworker/debug/last-stream/clear", authMiddleware(handleDebugLastStreamClear))
	// Adaptive loader API (?????????????? ????? init() ? adaptive_integration.go)
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
	// R60.8 (2026-09-07): /v1/models/{model_id} per OpenAI spec. Trailing slash
	// для subtree match; handler парсит r.URL.Path внутри.
	// НЕ конфликтует с /v1/models (выше) — Go ServeMux делает exact match.
	mux.HandleFunc("/v1/models/", handleV1ModelByID)
	// F.0b (2026-06-28): session F — recoverMiddleware как самый внешний слой,
	// catch'ит panic от corsMiddleware / loggingMiddleware / любого handler'а.
	// Без этого panic приводит к EOF без диагностики для клиента.
	// Round 36 Phase 4: RequestIDMiddleware first, so all subsequent
	// middleware and handlers can read the request ID from context.
	return recoverMiddleware(
		corsMiddleware(
			observability.RequestIDMiddleware(
				loggingMiddleware(mux))))
}
