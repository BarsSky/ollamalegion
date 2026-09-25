package balancer

// Package balancer — хранилище статистики очереди (R78).
//
// ИСТОРИЯ: до R73 у Ollama-пути была своя очередь — канал `queue` + пул
// worker'ов, которые забирали запросы и обслуживали их через `dispatchRequest`
// (со своими лимитами и без per-session справедливости). R73 свёл ожидание
// слотов в admission-очередь (`admission_queue.go`), после чего канал и worker'ы
// стали мёртвым кодом: запросы в них больше никто не клал.
//
// R78 удалил канал, пул worker'ов и `dispatchRequest`. `QueueManager` остался
// как **хранилище статистики и истории** для контракта API:
//
//   - `processed` — сколько запросов обслужено (пополняется `RecordUnified`);
//   - `completedHistory` — история `/api/v1/queue/history`;
//   - dispatch-счётчики (`dispatchAffinity/Load/Config`) — их инкрементирует
//     выбор бэкенда, читает `/api/v1/queue/stats` и страница /monitor;
//   - `maxSize` — порог backpressure для единой очереди
//     (`admissionOverloaded`);
//   - `pending`/`processing` — legacy-поля ответа `/api/v1/queue/details`
//     (всегда пусты: реальные ожидающие живут в admission-очереди, их видно в
//     `/api/v1/queue/stats` → `admission`).
//
// `numWorkers`/`timeout` остаются только для совместимости API-ответов
// (`workers`, `timeout_sec`) — пула worker'ов больше нет.

import (
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// CompletedRequest - выполненный запрос для истории
type CompletedRequest struct {
	Model       string    `json:"model"`
	Target      string    `json:"target"`
	Enqueued    time.Time `json:"enqueued"`
	CompletedAt time.Time `json:"completed_at"`
	WaitTimeMs  int64     `json:"wait_time_ms"`
}

// QueueManager - счётчики и история очереди (без канала и worker'ов, см. шапку
// файла). Порядок полей — под fieldalignment.
type QueueManager struct {
	pending          []*QueuedRequest
	processing       []*QueuedRequest
	completedHistory []*CompletedRequest

	// Счётчики (атомарные): processed — обслужено всего, dispatch* — каким
	// правилом выбран бэкенд.
	processed        int64
	dispatchAffinity int64
	dispatchLoad     int64
	dispatchConfig   int64
	// timeout — исторический queueTimeout (в API как timeout_sec).
	timeout time.Duration
	// maxSize — порог backpressure единой очереди; numWorkers — legacy-значение
	// для ответа API (`workers`).
	maxSize    int
	numWorkers int

	pendingMu    sync.RWMutex
	processingMu sync.RWMutex
	historyMu    sync.RWMutex
}

// QueuedRequest - запрос в очереди (legacy-структура: используется только
// тестами и DTO истории; в обслуживании не участвует с R73).
type QueuedRequest struct {
	Request      *http.Request
	Writer       http.ResponseWriter
	Model        string
	Enqueued     time.Time
	Done         chan bool
	Target       string
	RequeueCount int       // Счётчик повторных постановок в очередь
	LoadDeadline time.Time // Дедлайн ожидания загрузки модели (если инициирована)
}

// QueueStats - статистика очереди
type QueueStats struct {
	Admission          queueAdmissionStats `json:"admission"`
	CurrentSize        int                 `json:"current_size"`
	MaxSize            int                 `json:"max_size"`
	Processed          int64               `json:"processed_total"`
	WaitTimeAvgMs      int64               `json:"avg_wait_time_ms"`
	Workers            int                 `json:"workers"`
	TimeoutSec         int                 `json:"timeout_sec"`
	DispatchByAffinity int64               `json:"dispatch_by_affinity"`
	DispatchByLoad     int64               `json:"dispatch_by_load"`
	DispatchByConfig   int64               `json:"dispatch_by_config"`
}

// NewQueueManager - создание хранилища статистики очереди.
//
// Сигнатура сохранена ради конфига (`queueMaxSize`, `queueWorkers`,
// `queueTimeout`) и существующих вызовов/тестов; пул worker'ов не создаётся —
// ожидание слотов обеспечивает admission-очередь (R73).
func NewQueueManager(_ *Proxy, maxSize int, numWorkers int, timeout time.Duration) *QueueManager {
	return &QueueManager{
		maxSize:    maxSize,
		numWorkers: numWorkers,
		timeout:    timeout,
	}
}

// Stop - сохранён для совместимости (Shutdown и тестовые cleanup): останавливать
// больше нечего, вызов идемпотентен.
func (qm *QueueManager) Stop() {}

// addDispatchAffinity/addDispatchLoad/addDispatchConfig — инкременты счётчиков
// выбора бэкенда (заменяют прямые atomic.AddInt64 в backend_selector/queue_dispatch).
func (qm *QueueManager) addDispatchAffinity() { atomic.AddInt64(&qm.dispatchAffinity, 1) }
func (qm *QueueManager) addDispatchLoad()     { atomic.AddInt64(&qm.dispatchLoad, 1) }
func (qm *QueueManager) addDispatchConfig()   { atomic.AddInt64(&qm.dispatchConfig, 1) }

// RecordUnified — запись запроса, обслуженного ЕДИНОЙ (admission-)очередью.
//
// Счётчики и история остаются частью API (`/api/v1/queue/stats`,
// `/api/v1/queue/history`, панель WebUI), поэтому Ollama-путь отмечает здесь
// каждый обслуженный запрос.
func (qm *QueueManager) RecordUnified(model, target string, enqueued time.Time) {
	if qm == nil {
		return
	}
	now := time.Now()
	// R66d: processed — атомарный (читается из cluster_state без блокировки).
	atomic.AddInt64(&qm.processed, 1)

	qm.historyMu.Lock()
	qm.completedHistory = append(qm.completedHistory, &CompletedRequest{
		Model:       model,
		Target:      target,
		Enqueued:    enqueued,
		CompletedAt: now,
		WaitTimeMs:  now.Sub(enqueued).Milliseconds(),
	})
	if len(qm.completedHistory) > queueHistoryLimit {
		qm.completedHistory = qm.completedHistory[len(qm.completedHistory)-queueHistoryLimit:]
	}
	qm.historyMu.Unlock()
}

// queueHistoryLimit — сколько завершённых запросов держим в истории.
const queueHistoryLimit = 100

// getProcessingDTOs - получение DTO processing запросов для API.
// R78: список всегда пуст (реальные запросы ждут в admission-очереди), но поле
// ответа сохранено для совместимости.
func (qm *QueueManager) getProcessingDTOs() []map[string]interface{} {
	qm.processingMu.RLock()
	defer qm.processingMu.RUnlock()

	result := make([]map[string]interface{}, 0, len(qm.processing))
	now := time.Now()
	for _, req := range qm.processing {
		result = append(result, map[string]interface{}{
			"model":      req.Model,
			"enqueued":   req.Enqueued.UTC().Format(time.RFC3339),
			"waitTimeMs": now.Sub(req.Enqueued).Milliseconds(),
			"target":     req.Target,
			"status":     "processing",
		})
	}
	return result
}

// getPendingDTOs - получение DTO pending запросов для API (см. выше).
func (qm *QueueManager) getPendingDTOs() []map[string]interface{} {
	qm.pendingMu.RLock()
	defer qm.pendingMu.RUnlock()

	result := make([]map[string]interface{}, 0, len(qm.pending))
	now := time.Now()
	for _, req := range qm.pending {
		result = append(result, map[string]interface{}{
			"model":      req.Model,
			"enqueued":   req.Enqueued.UTC().Format(time.RFC3339),
			"waitTimeMs": now.Sub(req.Enqueued).Milliseconds(),
			"target":     req.Target,
			"status":     "pending",
		})
	}
	return result
}
