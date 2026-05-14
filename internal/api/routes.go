package api

// setupRoutes - настройка маршрутов API сервера
func (s *Server) setupRoutes() {
	// Health check (без аутентификации и rate limiting)
	s.mux.HandleFunc("/api/v1/health", s.healthHandler)

	// Auth endpoints (требуют токен, кроме health)
	s.mux.Handle("/api/v1/auth/status", AuthMiddleware(RateLimitMiddleware(AuthStatusHandler(s.authenticator), s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/auth/token", AuthMiddleware(RateLimitMiddleware(TokenManagementHandler(s.authenticator), s.rateLimiter), s.authenticator))

	// Rate limit status endpoint (публичный, без аутентификации)
	s.mux.HandleFunc("/api/v1/ratelimit/status", RateLimitStatusHandler(s.rateLimiter))

	// Backends (с аутентификацией и rate limiting)
	s.mux.Handle("/api/v1/backends", AuthMiddleware(RateLimitMiddleware(s.backendsHandler, s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/backends/", AuthMiddleware(RateLimitMiddleware(s.backendHandler, s.rateLimiter), s.authenticator))

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

	// Model Replication endpoints (Variant A) — с аутентификацией и rate limiting
	s.mux.Handle("/api/v1/replication/groups", AuthMiddleware(RateLimitMiddleware(s.replicationGroupsHandler, s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/replication/groups/", AuthMiddleware(RateLimitMiddleware(s.replicationGroupHandler, s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/replication/stats", AuthMiddleware(RateLimitMiddleware(s.replicationStatsHandler, s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/replication/reconcile", AuthMiddleware(RateLimitMiddleware(s.replicationReconcileHandler, s.rateLimiter), s.authenticator))

	// Virtual Model endpoints (Variant C) — с аутентификацией и rate limiting
	s.mux.Handle("/api/v1/virtualmodels", AuthMiddleware(RateLimitMiddleware(s.virtualModelsListHandler, s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/virtualmodels/", AuthMiddleware(RateLimitMiddleware(s.virtualModelsStatusHandler, s.rateLimiter), s.authenticator))

	// Proxy Logs endpoint (с аутентификацией и rate limiting)
	s.mux.Handle("/api/v1/proxy/logs", AuthMiddleware(RateLimitMiddleware(s.proxyLogsHandler, s.rateLimiter), s.authenticator))

	// Candidate Backends endpoint (с аутентификацией и rate limiting)
	// Возвращает группы бэкендов-кандидатов по приоритетам для всех моделей
	s.mux.Handle("/api/v1/candidates", AuthMiddleware(RateLimitMiddleware(s.candidatesHandler, s.rateLimiter), s.authenticator))

	// Favicon и статические ресурсы (без аутентификации, для браузеров)
	s.mux.HandleFunc("/favicon.ico", s.staticFileHandler)
	s.mux.HandleFunc("/favicon-16x16.png", s.staticFileHandler)
	s.mux.HandleFunc("/favicon-32x32.png", s.staticFileHandler)
	s.mux.HandleFunc("/logo.svg", s.staticFileHandler)
}
