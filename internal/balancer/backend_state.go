package balancer

import (
	"fmt"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// contextKey - типизированный ключ для context.Value
type contextKey string

const modelContextKey contextKey = "model"
const streamContextKey contextKey = "stream"

// LatencyRecord — запись о времени ответа бэкенда для адаптивного расчёта таймаута.
type LatencyRecord struct {
	Timestamp time.Time `json:"timestamp"`
	LatencyMs int64     `json:"latencyMs"`
	Model     string    `json:"model"`
	Success   bool      `json:"success"`
}

// BackendState - состояние бэкенда
type BackendState struct {
	Backend         *types.Backend
	ActiveReqs      int
	TotalRequests   int64                   // Atomic: всего запросов на этот бэкенд
	LastUsed        time.Time
	MetricsHistory  []types.MetricsSnapshot // История метрик для прогнозирования
	Prediction      types.Prediction        // Последний прогноз
	RequestHistory  []time.Time             // Таймстемпы запросов для расчёта RPS (окно 60с)
	CalculatedRPS   float64                 // Вычисленный RPS
	WarmingUpModels map[string]*types.WarmupState // Модели в превентивной загрузке
	AgentID         string                     // ID агента v2, прикреплённого к этому бэкенду
	ErrorCount      int                     // Счётчик ошибок
	TotalAttempts   int                     // Всего попыток

	// Адаптивный таймаут
	LatencyHistory  []LatencyRecord `json:"latencyHistory"`  // История задержек (до 100 записей)
	AdaptiveTimeout int             `json:"adaptiveTimeout"` // Текущий адаптивный таймаут (сек), 0 = использовать глобальный

	mu sync.Mutex
}

// resolveBackendEngine — определяет эффективный движок конкретного бэкенда (ollama_api / llama_cpp).
func (p *Proxy) resolveBackendEngine(backend *types.Backend) types.BackendEngine {
	return types.ResolveEngine(backend.Engine, backend.Type)
}

// getBackendPort — возвращает порт инференса в зависимости от движка бэкенда.
func (p *Proxy) getBackendPort(backend *types.Backend) int {
	engine := p.resolveBackendEngine(backend)
	switch engine {
	case types.EngineLlamaCPP:
		if backend.CppWorkerPort > 0 {
			return backend.CppWorkerPort
		}
		// Modern llama.cpp CppWorker installations listen on 18092.
		// 18091 is legacy; kept for backwards compatibility with old deployments.
		logger.Get().Warnw("llama.cpp backend has no CppWorkerPort, using default",
			"backend", backend.ID, "default_port", 18092)
		return 18092 // default cppworker port (modern)
	default:
		return backend.OllamaPort
	}
}

// getBackendBaseURL — возвращает базовый URL бэкенда для инференса.
func (p *Proxy) getBackendBaseURL(backend *types.Backend) string {
	return fmt.Sprintf("http://%s:%d", backend.Host, p.getBackendPort(backend))
}

// backendHTTPAddrByID — возвращает базовый URL бэкенда по его ID
// (например, "http://cppworker:18092"). Используется preflight-helper'ом
// (см. preflight_helper.go) для POST /api/models/reload.
//
// Безопасен при unknown backendID: возвращает пустую строку, и caller
// может пропустить preflight (graceful degradation).
func (p *Proxy) backendHTTPAddrByID(backendID string) string {
	if p == nil {
		return ""
	}
	p.mu.RLock()
	state, exists := p.backends[backendID]
	p.mu.RUnlock()
	if !exists || state == nil || state.Backend == nil {
		return ""
	}
	return p.getBackendBaseURL(state.Backend)
}

// isLlamaCppBackend — проверяет, является ли конкретный бэкенд llama.cpp.
// Проверяем по BackendType (не по Engine), чтобы бэкенды с EngineAuto тоже корректно
// обрабатывались через proxyRequestLlamaCpp с трансляцией форматов.
func (p *Proxy) isLlamaCppBackend(backend *types.Backend) bool {
	return normalizeBackendType(backend.Type) == types.BackendTypeLlamaCpp
}
