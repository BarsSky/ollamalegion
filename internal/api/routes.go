package api

import "net/http"

// setupRoutes - настройка маршрутов API сервера
func (s *Server) setupRoutes() {
	// Health check (без аутентификации и rate limiting)
	s.mux.HandleFunc("/api/v1/health", s.healthHandler)

	// Liveness probe — всегда 200 OK, пока HTTP-сервер жив.
	// Используется Docker healthcheck, чтобы не падать в restart loop,
	// когда бэкенды ещё не зарегистрированы (healthHandler возвращает 503 в degraded).
	s.mux.HandleFunc("/api/v1/ping", s.pingHandler)

	// Auth endpoints (требуют токен, кроме health)
	s.mux.Handle("/api/v1/auth/status", AuthMiddleware(RateLimitMiddleware(AuthStatusHandler(s.authenticator), s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/auth/token", AuthMiddleware(RateLimitMiddleware(TokenManagementHandler(s.authenticator), s.rateLimiter), s.authenticator))

	// Rate limit status endpoint (публичный, без аутентификации)
	s.mux.HandleFunc("/api/v1/ratelimit/status", RateLimitStatusHandler(s.rateLimiter))

	// Backends (с аутентификацией и rate limiting)
	s.mux.Handle("/api/v1/backends", AuthMiddleware(RateLimitMiddleware(s.backendsHandler, s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/backends/", AuthMiddleware(RateLimitMiddleware(s.backendHandler, s.rateLimiter), s.authenticator))

	// Backend types info (публичный, без аутентификации)
	s.mux.HandleFunc("/api/v1/backends/types", s.handleBackendTypeInfo)

	// Operating Mode switch (с аутентификацией и rate limiting)
	s.mux.Handle("/api/v1/config/mode", AuthMiddleware(RateLimitMiddleware(http.HandlerFunc(s.handleModeSwitch), s.rateLimiter), s.authenticator))

	// Config reset to defaults (с аутентификацией и rate limiting)
	s.mux.Handle("/api/v1/config/reset", AuthMiddleware(RateLimitMiddleware(http.HandlerFunc(s.configResetHandler), s.rateLimiter), s.authenticator))

	// Config export/import (с аутентификацией и rate limiting)
	s.mux.Handle("/api/v1/config/export", AuthMiddleware(RateLimitMiddleware(http.HandlerFunc(s.configExportHandler), s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/config/import", AuthMiddleware(RateLimitMiddleware(http.HandlerFunc(s.configImportHandler), s.rateLimiter), s.authenticator))

	// Models capacity (global)
	s.mux.Handle("/api/v1/models/capacity", AuthMiddleware(RateLimitMiddleware(s.modelsCapacityHandler, s.rateLimiter), s.authenticator))

	// Model operations status endpoint
	s.mux.Handle("/api/v1/models/operations", AuthMiddleware(RateLimitMiddleware(s.modelOpsStatusHandler, s.rateLimiter), s.authenticator))

	// Metrics (с аутентификацией и rate limiting)
	s.mux.Handle("/api/v1/metrics", AuthMiddleware(RateLimitMiddleware(s.metricsHandler, s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/metrics/", AuthMiddleware(RateLimitMiddleware(s.metricHandler, s.rateLimiter), s.authenticator))

	// Sessions (с аутентификацией и rate limiting)
	s.mux.Handle("/api/v1/sessions", AuthMiddleware(RateLimitMiddleware(s.sessionsHandler, s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/sessions/", AuthMiddleware(RateLimitMiddleware(s.sessionHandler, s.rateLimiter), s.authenticator))

	// Models (с аутентификацией и rate limiting)
	s.mux.Handle("/api/v1/models", AuthMiddleware(RateLimitMiddleware(s.modelsHandler, s.rateLimiter), s.authenticator))

	// Cluster state (с аутентификацией и rate limiting)
	s.mux.Handle("/api/v1/cluster", AuthMiddleware(RateLimitMiddleware(s.clusterHandler, s.rateLimiter), s.authenticator))

	// Cluster-level model management (с аутентификацией и rate limiting).
	// Позволяет управлять моделями через балансировщик (порт 18081) без прямого
	// обращения к cppworker (порт 18092). Полезно для UI и пользовательских скриптов.
	s.mux.Handle("/api/v1/cluster/models/loaded", AuthMiddleware(RateLimitMiddleware(http.HandlerFunc(s.clusterLoadedModelsHandler), s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/cluster/models/loading", AuthMiddleware(RateLimitMiddleware(http.HandlerFunc(s.clusterLoadingModelsHandler), s.rateLimiter), s.authenticator))
	// Cluster-level proxy для cppworker debug endpoint /api/v1/cppworker/debug/last-prompt.
	// Используется для диагностики случаев "Cline получил 413 prompt_exceeds_context"
	// без прямого доступа к cppworker (защита сети, единая точка запроса).
	s.mux.Handle("/api/v1/cluster/cppworker/debug/last-prompt", AuthMiddleware(RateLimitMiddleware(http.HandlerFunc(s.clusterDebugLastPromptHandler), s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/cluster/models/", AuthMiddleware(RateLimitMiddleware(http.HandlerFunc(s.clusterReloadModelHandler), s.rateLimiter), s.authenticator))

	// Queue stats (с аутентификацией и rate limiting)
	s.mux.Handle("/api/v1/queue/stats", AuthMiddleware(RateLimitMiddleware(s.queueStatsHandler, s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/queue/details", AuthMiddleware(RateLimitMiddleware(s.queueDetailsHandler, s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/queue/history", AuthMiddleware(RateLimitMiddleware(s.queueHistoryHandler, s.rateLimiter), s.authenticator))

	// Cluster config (runtime-смена алгоритма)
	s.mux.Handle("/api/v1/cluster/config", AuthMiddleware(RateLimitMiddleware(s.clusterConfigHandler, s.rateLimiter), s.authenticator))

	// Predictions (с аутентификацией и rate limiting)
	s.mux.Handle("/api/v1/predictions", AuthMiddleware(RateLimitMiddleware(s.predictionsHandler, s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/predictions/", AuthMiddleware(RateLimitMiddleware(s.predictionHandler, s.rateLimiter), s.authenticator))

	// Auto-pull endpoints (с аутентификацией и rate limiting)
	s.mux.Handle("/api/v1/autopull", AuthMiddleware(RateLimitMiddleware(s.autoPullHandler, s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/autopull/status", AuthMiddleware(RateLimitMiddleware(s.autoPullStatusHandler, s.rateLimiter), s.authenticator))

	// Agents endpoints (с аутентификацией и rate limiting)
	s.mux.Handle("/api/v1/agents/register", AuthMiddleware(RateLimitMiddleware(s.agentRegisterHandler, s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/agents/metrics", AuthMiddleware(RateLimitMiddleware(s.agentMetricsHandler, s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/agents/heartbeat", AuthMiddleware(RateLimitMiddleware(s.agentHeartbeatHandler, s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/agents/stats", AuthMiddleware(RateLimitMiddleware(s.agentStatsHandler, s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/agents/", AuthMiddleware(RateLimitMiddleware(s.agentInfoHandler, s.rateLimiter), s.authenticator))

	// WebSocket (с rate limiting, аутентификация внутри handler после Upgrade)
	s.mux.Handle("/ws/metrics", RateLimitMiddleware(s.wsMetricsHandler, s.wsRateLimiter))

	// Monitor HTML page (без аутентификации)
	s.mux.HandleFunc("/monitor", s.monitorHandler)

	// Restart endpoint (c аутентификацией и rate limiting, только от webui)
	s.mux.Handle("/api/v1/admin/restart", AuthMiddleware(RateLimitMiddleware(s.restartHandler, s.rateLimiter), s.authenticator))

	// Internal callbacks от cppworker (Шаг «отображение загрузки в мониторе»).
	// POST /api/v1/internal/llama-model-loaded — callback при успешной загрузке модели.
	// Endpoint требует X-API-Token (если в config задан API_TOKEN). Не публичный.
	s.mux.Handle("/api/v1/internal/llama-model-loaded", AuthMiddleware(RateLimitMiddleware(http.HandlerFunc(s.handleLlamaModelLoaded), s.rateLimiter), s.authenticator))

	// Model Replication endpoints (Variant A) — с аутентификацией и rate limiting
	s.mux.Handle("/api/v1/replication/groups", AuthMiddleware(RateLimitMiddleware(s.replicationGroupsHandler, s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/replication/groups/", AuthMiddleware(RateLimitMiddleware(s.replicationGroupHandler, s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/replication/stats", AuthMiddleware(RateLimitMiddleware(s.replicationStatsHandler, s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/replication/reconcile", AuthMiddleware(RateLimitMiddleware(s.replicationReconcileHandler, s.rateLimiter), s.authenticator))

	// Virtual Model endpoints (Variant C) — с аутентификацией и rate limiting
	s.mux.Handle("/api/v1/virtualmodels", AuthMiddleware(RateLimitMiddleware(s.virtualModelsListHandler, s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/virtualmodels/", AuthMiddleware(RateLimitMiddleware(s.virtualModelsStatusHandler, s.rateLimiter), s.authenticator))

	// GGUF backends info for WebUI (публичный, без аутентификации — используется страницей GGUF)
	s.mux.HandleFunc("/api/v1/gguf/backends", s.handleGgufBackends)

	// GGUF backend proxy — проксирует запросы WebUI к CppWorker конкретного
	// llama.cpp бэкенда. Используется страницей GGUF для скачивания моделей,
	// списка файлов, прогресса загрузки, загрузки/выгрузки. Префикс /api/v1/gguf/
	// уже отрезается стандартным mux (см. handleGgufBackendProxy для деталей).
	s.mux.HandleFunc("/api/v1/gguf/backends/", s.handleGgufBackendProxy)

	// Proxy Logs endpoint (с аутентификацией и rate limiting)
	s.mux.Handle("/api/v1/proxy/logs", AuthMiddleware(RateLimitMiddleware(s.proxyLogsHandler, s.rateLimiter), s.authenticator))

	// Per-model profiles для cppworker (Шаг 5 cppworker-preflight-nctx-session).
	// GET    /api/v1/cppworker/model-profiles         — список всех профилей
	// GET/PUT/DELETE /api/v1/cppworker/model-profiles/{name}
	// POST   /api/v1/cppworker/model-profiles/{name}/apply — save + reload на бэкендах
	s.mux.Handle("/api/v1/cppworker/model-profiles", AuthMiddleware(RateLimitMiddleware(http.HandlerFunc(s.handleListModelProfiles), s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/cppworker/model-profiles/", AuthMiddleware(RateLimitMiddleware(http.HandlerFunc(s.handleModelProfile), s.rateLimiter), s.authenticator))

	// 2026-06-24: proxy to cppworker reset-reload-counter endpoint.
	// Сбрасывает ramFallbackAttempts на cppworker (cycle counter блокирует reload
	// после превышения лимита). Без этого нужен `docker restart`.
	s.mux.Handle("/api/v1/cppworker/reset-reload-counter", AuthMiddleware(RateLimitMiddleware(http.HandlerFunc(s.handleResetCppWorkerReloadCounter), s.rateLimiter), s.authenticator))

	// RPC Coordinator management endpoints (B2 — Session 5, 2026-06-26).
	// Управление worker'ами и distributed моделями через балансировщик,
	// без прямого доступа к worker'ам.
	// /api/v1/rpc/workers — GET (list) или POST (register через тот же роут)
	s.mux.Handle("/api/v1/rpc/workers", AuthMiddleware(RateLimitMiddleware(http.HandlerFunc(s.handleRpcWorkersListOrRegister), s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/rpc/workers/register", AuthMiddleware(RateLimitMiddleware(http.HandlerFunc(s.handleRpcWorkersRegister_POST), s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/rpc/workers/", AuthMiddleware(RateLimitMiddleware(http.HandlerFunc(s.handleRpcWorkerItem), s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/rpc/models", AuthMiddleware(RateLimitMiddleware(http.HandlerFunc(s.handleRpcModelsRouter), s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/rpc/models/", AuthMiddleware(RateLimitMiddleware(http.HandlerFunc(s.handleRpcModelsRouter), s.rateLimiter), s.authenticator))

	// /api/v1/rpc/metrics — Prometheus exposition для RPC Coordinator (B7).
	// Возвращает text/plain; version=0.0.4 — scrape-совместимый output.
	s.mux.Handle("/api/v1/rpc/metrics", AuthMiddleware(RateLimitMiddleware(http.HandlerFunc(s.handleRpcMetrics), s.rateLimiter), s.authenticator))

	// B8 — TP (tensor parallelism) endpoints.
	s.mux.Handle("/api/v1/rpc/tp/infer", AuthMiddleware(RateLimitMiddleware(http.HandlerFunc(s.handleRPCModelTPInfer), s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/rpc/tp/status", AuthMiddleware(RateLimitMiddleware(http.HandlerFunc(s.handleRPCModelTPStatus), s.rateLimiter), s.authenticator))

	// Candidate Backends endpoint (с аутентификацией и rate limiting)
	// Возвращает группы бэкендов-кандидатов по приоритетам для всех моделей
	s.mux.Handle("/api/v1/candidates", AuthMiddleware(RateLimitMiddleware(s.candidatesHandler, s.rateLimiter), s.authenticator))

	// Favicon и статические ресурсы (без аутентификации, для браузеров)
	s.mux.HandleFunc("/favicon.ico", s.staticFileHandler)
	s.mux.HandleFunc("/favicon-16x16.png", s.staticFileHandler)
	s.mux.HandleFunc("/favicon-32x32.png", s.staticFileHandler)
	s.mux.HandleFunc("/logo.svg", s.staticFileHandler)
}
