package balancer

import (
	"sync"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// ProxyLogEntry — запись лога прокси-запроса
type ProxyLogEntry struct {
	Timestamp  time.Time `json:"timestamp"`
	Method     string    `json:"method"`
	Path       string    `json:"path"`
	Model      string    `json:"model,omitempty"`
	ClientIP   string    `json:"clientIP,omitempty"`
	UserAgent  string    `json:"userAgent,omitempty"`
	BackendID  string    `json:"backendID,omitempty"`
	SessionID  string    `json:"sessionID,omitempty"`
	StatusCode int       `json:"statusCode"`
	DurationMs int64     `json:"durationMs"`
	Error      string    `json:"error,omitempty"`
	Stream     bool      `json:"stream"`
	ClientName string    `json:"clientName,omitempty"`
}

// ProxyLogger — кольцевой буфер для хранения логов прокси-запросов
type ProxyLogger struct {
	mu       sync.RWMutex
	entries  []ProxyLogEntry
	maxSize  int
	position int // следующая позиция для записи
	count    int // всего записей сделано (для определения реального количества)
	eventBus *EventBus
}

// NewProxyLogger — создание нового ProxyLogger
func NewProxyLogger(maxSize int, eventBus *EventBus) *ProxyLogger {
	if maxSize <= 0 {
		maxSize = 1000 // значение по умолчанию
	}
	return &ProxyLogger{
		entries:  make([]ProxyLogEntry, maxSize),
		maxSize:  maxSize,
		position: 0,
		count:    0,
		eventBus: eventBus,
	}
}

// Append — добавление записи в кольцевой буфер
func (pl *ProxyLogger) Append(entry ProxyLogEntry) {
	pl.mu.Lock()
	pl.entries[pl.position] = entry
	pl.position = (pl.position + 1) % pl.maxSize
	pl.count++
	pl.mu.Unlock()

	// Публикуем событие через EventBus для WebSocket клиентов
	if pl.eventBus != nil {
		pl.eventBus.Publish(types.Event{
			Type:      types.EventProxyLog,
			Timestamp: entry.Timestamp,
			Data: map[string]interface{}{
				"entry": entry,
			},
		})
	}
}

// GetLast — получение последних N записей (или всех, если n <= 0)
func (pl *ProxyLogger) GetLast(n int) []ProxyLogEntry {
	pl.mu.RLock()
	defer pl.mu.RUnlock()

	if pl.count == 0 {
		return nil
	}

	actualCount := pl.count
	if actualCount > pl.maxSize {
		actualCount = pl.maxSize
	}
	if n <= 0 || n > actualCount {
		n = actualCount
	}

	result := make([]ProxyLogEntry, n)
	start := (pl.position - n + pl.maxSize) % pl.maxSize
	for i := 0; i < n; i++ {
		idx := (start + i) % pl.maxSize
		result[i] = pl.entries[idx]
	}
	return result
}

// Clear — очистка всех записей
func (pl *ProxyLogger) Clear() {
	pl.mu.Lock()
	defer pl.mu.Unlock()

	pl.entries = make([]ProxyLogEntry, pl.maxSize)
	pl.position = 0
	pl.count = 0
}

// Count — количество сохранённых записей
func (pl *ProxyLogger) Count() int {
	pl.mu.RLock()
	defer pl.mu.RUnlock()

	if pl.count > pl.maxSize {
		return pl.maxSize
	}
	return pl.count
}
