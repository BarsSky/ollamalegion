package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"

	"github.com/gorilla/websocket"
)

// Server - API сервер
type Server struct {
	proxy         *balancer.Proxy
	config        *types.LoadBalancerConfig
	mux           *http.ServeMux
	healthChecker *balancer.HealthChecker
	metricsBroker *MetricsBroker
	rateLimiter   *RateLimiter
	wsRateLimiter *RateLimiter
	authenticator *TokenAuthenticator
	stopCh        chan struct{} // graceful shutdown for metricsPublishLoop
	configSaver   func() error  // функция сохранения конфига на диск (устанавливается из main)
}

// SetConfigSaver — устанавливает функцию для сохранения конфигурации на диск
func (s *Server) SetConfigSaver(saver func() error) {
	s.configSaver = saver
}

// upgrader - апгрейдер HTTP до WebSocket с CORS whitelist
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true // same-origin запросы (no Origin header)
		}
		// Разрешаем localhost и 127.0.0.1 на любых портах (Docker, dev-серверы, nginx)
		if strings.HasPrefix(origin, "http://localhost") ||
			strings.HasPrefix(origin, "https://localhost") ||
			strings.HasPrefix(origin, "http://127.0.0.1") ||
			strings.HasPrefix(origin, "https://127.0.0.1") {
			return true
		}
		// Разрешаем запросы с любых приватных IP (Docker networks, LAN)
		// Шаблон: http(s)://192.168.x.x, 10.x.x.x, 172.16-31.x.x
		for _, prefix := range []string{"http://192.168.", "https://192.168.", "http://10.", "https://10.", "http://172.", "https://172."} {
			if strings.HasPrefix(origin, prefix) {
				return true
			}
		}
		logger.Get().Warnw("websocket origin rejected", "origin", origin)
		return false
	},
}

// NewServer - создание API сервера
func NewServer(proxy *balancer.Proxy, config *types.LoadBalancerConfig, healthChecker *balancer.HealthChecker) *Server {
	// Инициализация rate limiter с параметрами из конфига
	rateLimiter := NewRateLimiter(config.API.RateBurst, config.API.RateLimit)

	// Отдельный rate limiter для WebSocket с более высоким лимитом
	wsRateLimiter := NewRateLimiter(config.API.RateBurst*2, config.API.RateLimit*2)

	// Инициализация аутентификатора
	authenticator := NewTokenAuthenticator(
		config.Auth.Tokens,
		config.Auth.HeaderName,
		config.Auth.Enabled,
	)

	s := &Server{
		proxy:         proxy,
		config:        config,
		mux:           http.NewServeMux(),
		healthChecker: healthChecker,
		metricsBroker: NewMetricsBroker(),
		rateLimiter:   rateLimiter,
		wsRateLimiter: wsRateLimiter,
		authenticator: authenticator,
		stopCh:        make(chan struct{}),
	}

	s.setupRoutes()

	// Запуск goroutine для периодической отправки метрик
	go s.metricsPublishLoop()

	return s
}

// metricsPublishLoop - периодическая публикация метрик в брокер
func (s *Server) metricsPublishLoop() {
	ticker := time.NewTicker(2000 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			state := s.proxy.GetClusterState()
			if state == nil {
				continue
			}

			// Публикуем полное состояние кластера для WebSocket клиентов
			stateCopy := *state
			s.metricsBroker.PublishClusterState(&stateCopy)
		case <-s.stopCh:
			return
		}
	}
}

// StopMetricsLoop - остановка metrics publish loop
func (s *Server) StopMetricsLoop() {
	close(s.stopCh)
}

// ServeHTTP - обработка HTTP запросов
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// CORS headers
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Agent-ID, X-API-Token")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}

	s.mux.ServeHTTP(w, r)
}

// writeJSON - запись JSON ответа
func (s *Server) writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(data)
}