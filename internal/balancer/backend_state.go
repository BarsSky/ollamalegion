package balancer

import (
	"sync"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// contextKey - типизированный ключ для context.Value
type contextKey string

const modelContextKey contextKey = "model"
const streamContextKey contextKey = "stream"

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
	ErrorCount      int                     // Счётчик ошибок
	TotalAttempts   int                     // Всего попыток
	mu              sync.Mutex
}