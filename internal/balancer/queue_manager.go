package balancer

import (
	"context"
	"net/http"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// CompletedRequest - выполненный запрос для истории
type CompletedRequest struct {
	Model       string    `json:"model"`
	Target      string    `json:"target"`
	Enqueued    time.Time `json:"enqueued"`
	CompletedAt time.Time `json:"completed_at"`
	WaitTimeMs  int64     `json:"wait_time_ms"`
}

// QueueManager - менеджер очереди с pool workers
type QueueManager struct {
	queue            chan *QueuedRequest
	mu               sync.Mutex
	maxSize          int
	numWorkers       int
	processed        int64
	timeout          time.Duration
	ctx              context.Context
	cancel           context.CancelFunc
	wg               sync.WaitGroup
	proxy            *Proxy
	pendingMu        sync.RWMutex
	pending          []*QueuedRequest
	processingMu     sync.RWMutex
	processing       []*QueuedRequest
	historyMu        sync.RWMutex
	completedHistory []*CompletedRequest

	// Dispatch counters — atomic
	dispatchAffinity int64 // Model Affinity (модель уже загружена)
	dispatchLoad     int64 // Resource-Aware (выбор по ресурсам)
	dispatchConfig   int64 // Config/Weight (выбор по весам/конфигурации)
}

// QueuedRequest - запрос в очереди
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
	CurrentSize        int   `json:"current_size"`
	MaxSize            int   `json:"max_size"`
	Processed          int64 `json:"processed_total"`
	WaitTimeAvgMs      int64 `json:"avg_wait_time_ms"`
	Workers            int   `json:"workers"`
	TimeoutSec         int   `json:"timeout_sec"`
	DispatchByAffinity int64 `json:"dispatch_by_affinity"`
	DispatchByLoad     int64 `json:"dispatch_by_load"`
	DispatchByConfig   int64 `json:"dispatch_by_config"`
}

// NewQueueManager - создание менеджера очереди с pool workers
func NewQueueManager(proxy *Proxy, maxSize int, numWorkers int, timeout time.Duration) *QueueManager {
	ctx, cancel := context.WithCancel(context.Background())
	qm := &QueueManager{
		queue:      make(chan *QueuedRequest, maxSize),
		maxSize:    maxSize,
		numWorkers: numWorkers,
		timeout:    timeout,
		ctx:        ctx,
		cancel:     cancel,
		proxy:      proxy,
	}
	// Запускаем pool workers
	for i := 0; i < qm.numWorkers; i++ {
		qm.wg.Add(1)
		go qm.worker(i)
	}
	return qm
}

// worker - обработчик запросов из очереди
func (qm *QueueManager) worker(id int) {
	defer qm.wg.Done()
	logger.Get().Infow("queue worker started", "worker_id", id)

	for {
		select {
		case <-qm.ctx.Done():
			logger.Get().Infow("queue worker stopped", "worker_id", id)
			return
		case req := <-qm.queue:
			qm.removePending(req)
			qm.addProcessing(req)
			qm.processRequest(req, id)
			qm.removeProcessing(req)
		}
	}
}

// processRequest - обработка одного запроса из очереди через централизованный dispatch
func (qm *QueueManager) processRequest(req *QueuedRequest, workerID int) {
	var targetBackend string
	// Защита от паники: гарантируем освобождение слота, processing и уведомление клиента
	defer func() {
		if r := recover(); r != nil {
			logger.Get().Errorw("panic in processRequest, recovered",
				"worker_id", workerID,
				"model", req.Model,
				"panic", r,
			)
			// Освобождаем захваченный слот чтобы избежать перманентной утечки
			if targetBackend != "" {
				qm.proxy.releaseSlot(targetBackend)
			}
			select {
			case req.Done <- false:
			default:
			}
		}
	}()

	logger.Get().Debugw("processing queued request",
		"worker_id", workerID,
		"model", req.Model,
		"requeue_count", req.RequeueCount,
	)

	var dispatchType string

	// Если запрос requeue'ится более 3 раз — принудительно выбираем любой
	// свободный бэкенд без учёта model affinity.
	const maxRequeues = 3
	if req.RequeueCount >= maxRequeues {
		logger.Get().Warnw("max requeues exceeded, forcing rebalance to any free backend",
			"worker_id", workerID,
			"requeue_count", req.RequeueCount,
			"model", req.Model,
		)
		targetBackend = qm.proxy.selectFreeBackendAny()
		if targetBackend != "" {
			// Защита от гонки: проверяем что бэкенд всё ещё существует
			if _, exists := qm.proxy.backends[targetBackend]; !exists {
				targetBackend = ""
			} else if !qm.proxy.tryAcquireSlot(targetBackend) {
				targetBackend = ""
			} else {
				dispatchType = "force_rebalance"
				logger.Get().Infow("force rebalanced to free backend",
					"worker_id", workerID,
					"backend", targetBackend,
					"model", req.Model,
				)
			}
		}
	}

	// Централизованный dispatch через новую логику
	if targetBackend == "" {
		// Проверяем дедлайн: если запрос ждёт дольше queue timeout — отдаём 503
		queueTimeout := qm.timeout
		if isStream, _ := req.Request.Context().Value(streamContextKey).(bool); isStream {
			// Для streaming запросов уменьшаем timeout вдвое
			streamTimeout := qm.proxy.config.Balancing.QueueTimeout / 2
			if streamTimeout < 5 {
				streamTimeout = 5 // минимум 5 секунд
			}
			if streamTimeout > 0 {
				queueTimeout = time.Duration(streamTimeout) * time.Second
			}
		}
		if time.Since(req.Enqueued) > queueTimeout {
			logger.Get().Warnw("request exceeded queue timeout, returning 503",
				"worker_id", workerID,
				"model", req.Model,
				"wait_sec", time.Since(req.Enqueued).Seconds(),
				"requeue_count", req.RequeueCount,
			)
			req.Writer.WriteHeader(http.StatusServiceUnavailable)
			req.Writer.Write([]byte(`{"error":"queue timeout exceeded"}`))
			select {
			case req.Done <- false:
			default:
			}
			return
		}

		result := qm.proxy.dispatchRequest(req)
		if result.Error != nil {
			req.RequeueCount++
			// После N ретраев принудительно отдаём 503 вместо бесконечного re-queue
			const maxRequeueAttempts = 10
			if req.RequeueCount >= maxRequeueAttempts {
				logger.Get().Errorw("max requeue attempts exceeded, returning 503",
					"worker_id", workerID,
					"requeue_count", req.RequeueCount,
					"model", req.Model,
					"error", result.Error,
				)
				req.Writer.WriteHeader(http.StatusServiceUnavailable)
				req.Writer.Write([]byte(`{"error":"all backends busy, max retries exceeded"}`))
				select {
				case req.Done <- false:
				default:
				}
				return
			}
			logger.Get().Warnw("dispatch failed, re-queueing request",
				"worker_id", workerID,
				"requeue_count", req.RequeueCount,
				"error", result.Error,
			)
			time.AfterFunc(100*time.Millisecond, func() {
				select {
				case qm.queue <- req:
				case <-qm.ctx.Done():
					select {
					case req.Done <- false:
					default:
					}
				}
			})
			return
		}
		targetBackend = result.BackendID
		dispatchType = result.DispatchType
	}

	// На этом этапе слот уже захвачен внутри dispatchRequest (или force_rebalance).
	// Гарантируем освобождение слота при любом выходе (включая panic — см. defer выше).
	// Двойное освобождение предотвращается проверкой targetBackend != "" в panic-recover и
	// atomic захватом/освобождением в releaseSlot.
	defer func() {
		if targetBackend != "" {
			qm.proxy.releaseSlot(targetBackend)
			targetBackend = ""
		}
	}()

	req.Target = targetBackend
	logger.Get().Debugw("proxying queued request to backend",
		"worker_id", workerID,
		"backend", targetBackend,
		"dispatch_type", dispatchType,
	)

	// Проксируем запрос — с обработкой ошибок и fallback на другие бэкенды
	err := qm.proxy.proxyRequest(req.Writer, req.Request, targetBackend)
	if err == nil {
		select {
		case req.Done <- true:
		default:
		}
		qm.recordCompleted(req, dispatchType, workerID)
		return
	}

	// Ошибка на целевом бэкенде — освобождаем слот и пробуем fallback
	logger.Get().Warnw("queue request failed on target backend, trying fallback",
		"worker_id", workerID, "backend", targetBackend, "error", err)
	failedBackend := targetBackend
	qm.proxy.releaseSlot(failedBackend)
	targetBackend = "" // предотвращаем повторное освобождение через defer

	attemptedBackends := map[string]bool{failedBackend: true}
	const maxFallbackAttempts = 3
	for attempt := 0; attempt < maxFallbackAttempts; attempt++ {
		altBackend := qm.proxy.selectBackendExcluding(req.Model, attemptedBackends)
		if altBackend == "" {
			break
		}
		if !qm.proxy.tryAcquireSlot(altBackend) {
			attemptedBackends[altBackend] = true
			continue
		}

		req.Target = altBackend
		err = qm.proxy.proxyRequest(req.Writer, req.Request, altBackend)
		if err == nil {
			qm.proxy.releaseSlot(altBackend)
			select {
			case req.Done <- true:
			default:
			}
			qm.recordCompleted(req, "fallback_"+dispatchType, workerID)
			return
		}

		logger.Get().Warnw("fallback backend also failed",
			"worker_id", workerID, "backend", altBackend, "attempt", attempt+1, "error", err)
		qm.proxy.releaseSlot(altBackend)
		attemptedBackends[altBackend] = true
	}

	// Все бэкенды не сработали — пробуем requeue или отдаём 503
	const maxRequeueAttempts = 10
	if req.RequeueCount < maxRequeueAttempts {
		req.RequeueCount++
		logger.Get().Warnw("all fallbacks exhausted, re-queueing request",
			"worker_id", workerID,
			"requeue_count", req.RequeueCount,
			"model", req.Model,
		)
		time.AfterFunc(100*time.Millisecond, func() {
			select {
			case qm.queue <- req:
			case <-qm.ctx.Done():
				select {
				case req.Done <- false:
				default:
				}
			}
		})
		return
	}

	logger.Get().Errorw("max requeue attempts exceeded after fallback, returning 503",
		"worker_id", workerID,
		"requeue_count", req.RequeueCount,
		"model", req.Model,
	)
	req.Writer.WriteHeader(http.StatusServiceUnavailable)
	req.Writer.Write([]byte(`{"error":"all backends failed, max retries exceeded"}`))
	select {
	case req.Done <- false:
	default:
	}
}

// Stop - остановка всех workers
func (qm *QueueManager) Stop() {
	qm.cancel()
	qm.wg.Wait()
	close(qm.queue)
	for req := range qm.queue {
		select {
		case req.Done <- false:
		default:
		}
	}
}

// addPending - добавление запроса в pending список
func (qm *QueueManager) addPending(req *QueuedRequest) {
	qm.pendingMu.Lock()
	qm.pending = append(qm.pending, req)
	qm.pendingMu.Unlock()
}

// removePending - удаление запроса из pending списка
func (qm *QueueManager) removePending(req *QueuedRequest) {
	qm.pendingMu.Lock()
	for i, r := range qm.pending {
		if r == req {
			qm.pending = append(qm.pending[:i], qm.pending[i+1:]...)
			break
		}
	}
	qm.pendingMu.Unlock()
}

// addProcessing - добавление запроса в processing список
func (qm *QueueManager) addProcessing(req *QueuedRequest) {
	qm.processingMu.Lock()
	qm.processing = append(qm.processing, req)
	qm.processingMu.Unlock()
}

// removeProcessing - удаление запроса из processing списка
func (qm *QueueManager) removeProcessing(req *QueuedRequest) {
	qm.processingMu.Lock()
	for i, r := range qm.processing {
		if r == req {
			qm.processing = append(qm.processing[:i], qm.processing[i+1:]...)
			break
		}
	}
	qm.processingMu.Unlock()
}

// recordCompleted - запись завершённого запроса в историю и обновление счётчиков
func (qm *QueueManager) recordCompleted(req *QueuedRequest, dispatchType string, workerID int) {
	now := time.Now()
	waitTimeMs := now.Sub(req.Enqueued).Milliseconds()
	qm.mu.Lock()
	qm.processed++
	processed := qm.processed
	qm.mu.Unlock()

	qm.historyMu.Lock()
	qm.completedHistory = append(qm.completedHistory, &CompletedRequest{
		Model:       req.Model,
		Target:      req.Target,
		Enqueued:    req.Enqueued,
		CompletedAt: now,
		WaitTimeMs:  waitTimeMs,
	})
	const maxHistory = 100
	if len(qm.completedHistory) > maxHistory {
		qm.completedHistory = qm.completedHistory[len(qm.completedHistory)-maxHistory:]
	}
	qm.historyMu.Unlock()

	logger.Get().Debugw("queued request completed",
		"worker_id", workerID,
		"processed_total", processed,
		"wait_time_ms", waitTimeMs,
		"dispatch_type", dispatchType,
	)
}

// getProcessingDTOs - получение DTO processing запросов для API
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

// getPendingDTOs - получение DTO pending запросов для API
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

