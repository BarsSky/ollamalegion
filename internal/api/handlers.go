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
	// eventBus — общая EventBus с балансировщиком (для нотификаций, F.α).
	// Инициализируется через SetEventBus из main.go (после создания Proxy).
	eventBus      EventBusLike
	// eventsHub — локальный ring buffer + подписки на eventBus (F.α SSE endpoint).
	eventsHub     *eventsHub
	stopCh        chan struct{} // graceful shutdown for metricsPublishLoop
	configSaver   func() error  // функция сохранения конфига на диск (устанавливается из main)
	// overridesStore — persistent runtime overrides для read-only config.json
	// (Session 17, 2026-07-27). Хранит llamaCpp-секцию в /app/data/runtime-overrides/
	// (writable named volume `balancer_data`). Опционально — если nil, PUT
	// llamaCpp пишет в in-memory только, без персистенции.
	overridesStore OverridesStore
	// baseLlamaCpp — snapshot LlamaCppConfig из config.json ПРИ СТАРТЕ (до
	// применения override). Используется для "Reset to bundled defaults":
	// после DELETE override мы восстанавливаем s.config.LlamaCpp из этого
	// snapshot, иначе пользователь увидит reset в UI, но runtime не изменится.
	baseLlamaCpp *types.LlamaCppConfig
}

// EventBusLike — интерфейс EventBus из balancer.EventBus для тестирования.
// (Реальная реализация передаётся из main через SetEventBus.)
type EventBusLike interface {
	Subscribe() (string, <-chan types.Event)
	Unsubscribe(id string)
}

// SetConfigSaver — устанавливает функцию для сохранения конфигурации на диск
func (s *Server) SetConfigSaver(saver func() error) {
	s.configSaver = saver
}

// OverridesStore — интерфейс для persistent runtime-оверрайдов.
// Реализация по умолчанию — *runtimeoverrides.Store (Session 17).
// Абстракция нужна, чтобы handlers.go не зависел от runtimeoverrides напрямую
// (избегаем циклических импортов и упрощаем тестирование с mock-реализацией).
//
// Контракт: LoadLlamaCpp возвращает:
//   - (config, true, nil)  — override есть и загружен
//   - (nil, false, nil)    — override отсутствует (нормальная ситуация)
//   - (nil, false, err)    — ошибка ввода-вывода / парсинга
type OverridesStore interface {
	IsEnabled() bool
	LoadLlamaCpp() (*types.LlamaCppConfig, bool, error)
	SaveLlamaCpp(*types.LlamaCppConfig) error
	ClearLlamaCpp() error
	HasLlamaCppOverride() bool
	ApplyLlamaCppToConfig(target *types.LoadBalancerConfig) error
}

// SetOverridesStore — устанавливает persistent runtime overrides store.
// nil = оверрайды отключены (по умолчанию для dev-режима).
// baseSnapshot — snapshot LlamaCppConfig из config.json ДО применения override.
// Используется для restore при DELETE override.
func (s *Server) SetOverridesStore(store OverridesStore, baseSnapshot *types.LlamaCppConfig) {
	s.overridesStore = store
	s.baseLlamaCpp = baseSnapshot
}

// GetConfig — возвращает текущую конфигурацию (для тестов и отладки)
func (s *Server) GetConfig() *types.LoadBalancerConfig {
	return s.config
}

// GetResetHandler возвращает обработчик сброса конфигурации (для тестов)
func (s *Server) GetResetHandler() http.HandlerFunc {
	return s.configResetHandler
}

// GetExportHandler возвращает обработчик экспорта конфигурации (для тестов)
func (s *Server) GetExportHandler() http.HandlerFunc {
	return s.configExportHandler
}

// GetImportHandler возвращает обработчик импорта конфигурации (для тестов)
func (s *Server) GetImportHandler() http.HandlerFunc {
	return s.configImportHandler
}

// SetEventBus — подключает общую EventBus от балансировщика для SSE-нотификаций.
// Должна быть вызвана из main.go после NewServer (после создания Proxy).
func (s *Server) SetEventBus(bus EventBusLike) {
	s.eventBus = bus
	s.eventsHub = newEventsHub()
	logger.Get().Infow("Server.SetEventBus: SSE notifications endpoint enabled")
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