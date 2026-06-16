package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// Core handlers (health, WebSocket, admin, static, autoPull)
// ============================================================

// pingHandler — liveness probe: всегда 200 OK, пока HTTP-сервер жив.
// Не зависит от состояния бэкендов. Используется в Docker healthcheck
// (`/api/v1/health` может вернуть 503 в режиме degraded, что вызывает
// restart loop до того, как бэкенды успели зарегистрироваться).
func (s *Server) pingHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// healthHandler - глубокая проверка здоровья API (Docker healthcheck)
func (s *Server) healthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	state := s.proxy.GetClusterState()

	// Считаем бэкенды с агентом
	hasAgentCount := 0
	for _, b := range state.Backends {
		if b.HasAgent {
			hasAgentCount++
		}
	}

	// Статус HTTP: 503 если нет ни одного healthy бэкенда при наличии зарегистрированных
	httpStatus := http.StatusOK
	statusText := "healthy"

	if state.TotalBackends > 0 && state.HealthyBackends == 0 {
		httpStatus = http.StatusServiceUnavailable
		statusText = "degraded"
	}

	response := map[string]interface{}{
		"status":          statusText,
		"timestamp":       time.Now().UTC(),
		"version":         "1.0.0",
		"healthyBackends": state.HealthyBackends,
		"totalBackends":   state.TotalBackends,
		"hasAgents":       hasAgentCount,
		"totalRequests":   state.TotalRequests,
		"authEnabled":     false,
		"authHeader":      "X-API-Token",
		"wsEndpoint":      "/ws/metrics",
	}

	if s.authenticator != nil {
		response["authEnabled"] = s.authenticator.IsEnabled()
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	encoder.Encode(response)
}

// wsMetricsHandler - WebSocket для real-time метрик (event-driven + гибридный ping)
func (s *Server) wsMetricsHandler(w http.ResponseWriter, r *http.Request) {
	// Проверяем токен ДО WebSocket upgrade, но только если auth включена
	if s.authenticator != nil && s.authenticator.IsEnabled() {
		token := r.URL.Query().Get("token")
		if token == "" {
			http.Error(w, "Unauthorized: token is required", http.StatusUnauthorized)
			return
		}
		valid, _ := s.authenticator.Authenticate(r)
		if !valid {
			http.Error(w, "Unauthorized: invalid token", http.StatusUnauthorized)
			return
		}
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		logger.Get().Errorw("websocket upgrade error", "error", err)
		return
	}
	defer conn.Close()

	// Генерация уникального ID клиента
	clientID := fmt.Sprintf("client-%d", time.Now().UnixNano())

	// Канал для сигнала об отключении
	done := make(chan struct{})
	defer close(done)

	// Устанавливаем начальный write deadline
	conn.SetWriteDeadline(time.Now().Add(60 * time.Second))

	// Подписка на EventBus (event-driven)
	eventSubID, eventChan := s.proxy.SubscribeEvents()
	defer s.proxy.UnsubscribeEvents(eventSubID)

	// Подписка на periodic snapshot (fallback / heartbeat)
	metricsChan := s.metricsBroker.Subscribe(clientID, done)
	defer s.metricsBroker.Unsubscribe(clientID)

	// Канал для ошибок чтения
	errChan := make(chan error, 1)

	// Настраиваем read deadline и pong handler
	conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		return nil
	})

	// Goroutine для чтения сообщений от клиента (ping/pong/close)
	go func() {
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				errChan <- err
				return
			}
		}
	}()

	// Ping ticker (каждые 15 секунд для быстрого обнаружения разрывов)
	pingTicker := time.NewTicker(15 * time.Second)
	defer pingTicker.Stop()

	// Отправка начального состояния кластера
	initialState := s.metricsBroker.GetClusterState(s.proxy)
	if err := conn.WriteJSON(map[string]interface{}{
		"eventType": "clusterState",
		"timestamp": time.Now().UTC(),
		"data":      initialState,
	}); err != nil {
		logger.Get().Errorw("failed to send initial websocket state", "error", err)
		return
	}

	// Основной цикл: event-driven + periodic snapshot + ping
	for {
		select {
		case ev, ok := <-eventChan:
			if !ok {
				return
			}
			// Отправляем событие клиенту с eventType wrapper + write deadline
			conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
			wrapper := map[string]interface{}{
				"eventType": string(ev.Type),
				"timestamp": ev.Timestamp,
				"backendId": ev.BackendID,
				"data":      ev.Data,
			}
			if err := conn.WriteJSON(wrapper); err != nil {
				logger.Get().Errorw("failed to send websocket event", "error", err)
				return
			}

		case data, ok := <-metricsChan:
			if !ok {
				return
			}
			conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
			// Periodic snapshot — отправляем как clusterState
			var clusterState types.ClusterState
			if err := json.Unmarshal(data, &clusterState); err != nil {
				logger.Get().Errorw("failed to unmarshal cluster state", "error", err)
				continue
			}
			wrapper := map[string]interface{}{
				"eventType": "clusterState",
				"timestamp": time.Now().UTC(),
				"data":      clusterState,
			}
			if err := conn.WriteJSON(wrapper); err != nil {
				logger.Get().Errorw("failed to send websocket snapshot", "error", err)
				return
			}

		case <-pingTicker.C:
			// Ping для поддержания соединения с write deadline
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteJSON(map[string]interface{}{
				"eventType": "ping",
				"timestamp": time.Now().UTC(),
			}); err != nil {
				return
			}

		case err := <-errChan:
			logger.Get().Warnw("websocket read error", "error", err)
			return

		case <-done:
			return
		}
	}
}

// monitorHandler - отдаёт HTML страницу монитора
func (s *Server) monitorHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Пути для поиска monitor.html (сначала runtime, потом dev)
	paths := []string{
		"/app/monitor.html",
		"cmd/monitor/monitor.html",
		"../cmd/monitor/monitor.html",
	}

	var data []byte
	var monitorPath string
	var err error
	for _, p := range paths {
		data, err = os.ReadFile(p)
		if err == nil {
			monitorPath = p
			break
		}
	}

	if err != nil {
		logger.Get().Errorw("monitor.html not found", "error", err)
		http.Error(w, "Monitor page not found", http.StatusNotFound)
		return
	}

	htmlStr := string(data)

	// Встраиваем WEBUI_CONFIG и config.js inline.
	// WEBUI_CONFIG должен содержать те же поля, что и webui/js/modules/config.js:
	// API_BASE, API_BASE_URL, API_TOKEN, CPPWORKER_URL, REFRESH_INTERVAL и т.д.
	// Для обратной совместимости с monitor.html/state.js также сохраняем apiBase/dashboardUrl.
	//
	// apiBase вычисляем из текущего запроса: relative path ('') корректно работает
	// как при прямом доступе, так и за nginx-прокси. Если явно задан X-Forwarded-Prefix,
	// используем его.
	apiBase := strings.TrimSuffix(r.Header.Get("X-Forwarded-Prefix"), "/")
	if apiBase == "" {
		// Для Docker/nginx relative path всегда безопасен.
		apiBase = ""
	}

	configObj := fmt.Sprintf(
		`window.WEBUI_CONFIG={apiBase:"%s",dashboardUrl:"/",API_BASE:"%s",API_BASE_URL:"%s",API_TOKEN:"",CPPWORKER_URL:"http://localhost:18092",REFRESH_INTERVAL:5000,MAX_RECONNECT_ATTEMPTS:10,RECONNECT_INTERVAL_BASE:3000,WS_URL:null};`,
		apiBase, apiBase, apiBase,
	)

	configPath := filepath.Join(filepath.Dir(monitorPath), "config.js")
	if configData, configErr := os.ReadFile(configPath); configErr == nil {
		inline := fmt.Sprintf(
			`<script>%s</script>`+"\n"+`<script>%s</script>`,
			configObj,
			string(configData),
		)
		// Заменяем <script src="config.js"></script> на inline-скрипты
		if strings.Contains(htmlStr, `<script src="config.js"></script>`) {
			htmlStr = strings.Replace(htmlStr, `<script src="config.js"></script>`, inline, 1)
		} else {
			// Fallback: вставляем перед </head>
			htmlStr = strings.Replace(htmlStr, "</head>", inline+"\n</head>", 1)
		}
	} else {
		// config.js не найден — встраиваем только WEBUI_CONFIG
		inline := `<script>` + configObj + `</script>`
		if strings.Contains(htmlStr, `<script src="config.js"></script>`) {
			htmlStr = strings.Replace(htmlStr, `<script src="config.js"></script>`, inline, 1)
		} else {
			htmlStr = strings.Replace(htmlStr, "</head>", inline+"\n</head>", 1)
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(htmlStr))
}

// restartHandler - перезапуск балансера (только для webui)
func (s *Server) restartHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	logger.Get().Infow("balancer restart requested via API")

	// Запускаем перезапуск в goroutine, чтобы ответить клиенту до остановки
	go func() {
		time.Sleep(500 * time.Millisecond) // Даём время на отправку ответа
		if err := s.proxy.Restart(); err != nil {
			logger.Get().Errorw("balancer restart failed", "error", err)
		}
	}()

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": "Balancer restart initiated. The process will exit and be restarted by the supervisor.",
	})
}

// staticFileHandler — обработчик статических файлов (favicon, logo, etc.)
func (s *Server) staticFileHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	fileName := filepath.Base(r.URL.Path)
	if fileName == "." || fileName == "/" {
		http.NotFound(w, r)
		return
	}

	// Пути для поиска (сначала runtime Docker, потом локальная разработка)
	paths := []string{
		filepath.Join("/app", fileName),
		filepath.Join("webui", fileName),
		filepath.Join("../webui", fileName),
	}

	var data []byte
	var err error
	for _, p := range paths {
		data, err = os.ReadFile(p)
		if err == nil {
			break
		}
	}

	if err != nil {
		http.NotFound(w, r)
		return
	}

	// Определяем Content-Type
	contentType := "application/octet-stream"
	switch {
	case strings.HasSuffix(fileName, ".png"):
		contentType = "image/png"
	case strings.HasSuffix(fileName, ".ico"):
		contentType = "image/x-icon"
	case strings.HasSuffix(fileName, ".svg"):
		contentType = "image/svg+xml"
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// autoPullHandler — управление конфигурацией автоматической загрузки моделей (Pull-on-Demand).
func (s *Server) autoPullHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// Возвращаем конфигурацию и активные загрузки
		activePulls := []map[string]interface{}{}
		if s.proxy.AutoPull != nil {
			activePulls = s.proxy.AutoPull.GetActivePulls()
		}

		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"enabled":       s.config.Balancing.AutoPull.Enabled,
			"maxConcurrent": s.config.Balancing.AutoPull.MaxConcurrent,
			"pullTimeout":   s.config.Balancing.AutoPull.PullTimeout,
			"retryCount":    s.config.Balancing.AutoPull.RetryCount,
			"activePulls":   activePulls,
		})

	case http.MethodPut:
		var req struct {
			Enabled       *bool   `json:"enabled"`
			MaxConcurrent *int    `json:"maxConcurrent"`
			PullTimeout   *string `json:"pullTimeout"`
			RetryCount    *int    `json:"retryCount"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
				"success": false,
				"error":   "Invalid request body",
			})
			return
		}

		// Валидация
		if req.PullTimeout != nil {
			if _, err := time.ParseDuration(*req.PullTimeout); err != nil {
				s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
					"success": false,
					"error":   fmt.Sprintf("Invalid pullTimeout: %v. Use Go duration format (e.g. '5m', '10m')", err),
				})
				return
			}
		}
		if req.MaxConcurrent != nil && *req.MaxConcurrent < 0 {
			s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
				"success": false,
				"error":   "maxConcurrent must be >= 0",
			})
			return
		}
		if req.RetryCount != nil && *req.RetryCount < 0 {
			s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
				"success": false,
				"error":   "retryCount must be >= 0",
			})
			return
		}

		// Применяем изменения
		if req.Enabled != nil {
			s.config.Balancing.AutoPull.Enabled = *req.Enabled
		}
		if req.MaxConcurrent != nil {
			s.config.Balancing.AutoPull.MaxConcurrent = *req.MaxConcurrent
		}
		if req.PullTimeout != nil {
			s.config.Balancing.AutoPull.PullTimeout = *req.PullTimeout
		}
		if req.RetryCount != nil {
			s.config.Balancing.AutoPull.RetryCount = *req.RetryCount
		}

		// Обновляем конфигурацию в AutoPullManager если он инициализирован
		if s.proxy.AutoPull != nil {
			s.proxy.AutoPull.SetConfig(s.config.Balancing.AutoPull)
		} else if s.config.Balancing.AutoPull.Enabled {
			// Если менеджер ещё не создан, но мы включили — создаём
			s.proxy.AutoPull = balancer.NewAutoPullManager(s.proxy, s.config.Balancing.AutoPull)
		}

		// Сохраняем конфигурацию на диск
		if s.configSaver != nil {
			if err := s.configSaver(); err != nil {
				logger.Get().Warnw("failed to save auto-pull config to disk",
					"error", err)
			}
		}

		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"config": map[string]interface{}{
				"enabled":       s.config.Balancing.AutoPull.Enabled,
				"maxConcurrent": s.config.Balancing.AutoPull.MaxConcurrent,
				"pullTimeout":   s.config.Balancing.AutoPull.PullTimeout,
				"retryCount":    s.config.Balancing.AutoPull.RetryCount,
			},
			"message": "Auto-pull configuration updated successfully",
		})

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// autoPullStatusHandler — получение статуса активных и завершённых загрузок моделей
func (s *Server) autoPullStatusHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	activePulls := []map[string]interface{}{}
	if s.proxy.AutoPull != nil {
		activePulls = s.proxy.AutoPull.GetActivePulls()
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled":     s.config.Balancing.AutoPull.Enabled,
		"activePulls": activePulls,
		"totalActive": len(activePulls),
	})
}