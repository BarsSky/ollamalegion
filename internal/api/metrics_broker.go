package api

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// MetricsClient - представляет подключенного WebSocket клиента
type MetricsClient struct {
	ID   string
	Conn chan<- []byte
	Done <-chan struct{}
}

// ProxyInterface - интерфейс для получения состояния кластера
type ProxyInterface interface {
	GetClusterState() *types.ClusterState
}

// MetricsBroker - брокер для распределения метрик между WebSocket клиентами (pub/sub паттерн)
type MetricsBroker struct {
	mu          sync.RWMutex
	subscribers map[string]*MetricsClient
	metricsChan chan *types.BackendMetrics
	clusterChan chan *types.ClusterState
	stopChan    chan struct{}

	// R65d (2026-09-20): счётчики потерянных сообщений.
	//
	// Раньше оба `default:` в publish-путях молча отбрасывали данные: при
	// заполненном канале (медленный WebSocket-клиент, всплеск метрик) клиент
	// просто переставал получать обновления БЕЗ единого следа в логах и
	// метриках. Оператор видел «застывший» дашборд и не мог понять, что данные
	// теряются на стороне балансера.
	//
	// Теперь каждое отбрасывание считается, а факт дропа логируется
	// (с throttle, чтобы не залить лог при длительной перегрузке).
	brokerDrops   int64 // публикация в переполненный metricsChan/clusterChan
	clientDrops   int64 // доставка в переполненный канал подписчика
	lastDropLogNs int64 // для throttle логов
}

// brokerStatsSnapshot — снимок счётчиков потерь (для admin/метрик и тестов).
type brokerStatsSnapshot struct {
	BrokerDrops int64 `json:"broker_drops"`
	ClientDrops int64 `json:"client_drops"`
	Subscribers int   `json:"subscribers"`
	MetricsChan int   `json:"metrics_chan_len"`
	ClusterChan int   `json:"cluster_chan_len"`
}

// BrokerStats — R65d: экспорт счётчиков потерь.
func (mb *MetricsBroker) BrokerStats() brokerStatsSnapshot {
	if mb == nil {
		return brokerStatsSnapshot{}
	}
	mb.mu.RLock()
	subs := len(mb.subscribers)
	mb.mu.RUnlock()
	return brokerStatsSnapshot{
		BrokerDrops: atomic.LoadInt64(&mb.brokerDrops),
		ClientDrops: atomic.LoadInt64(&mb.clientDrops),
		Subscribers: subs,
		MetricsChan: len(mb.metricsChan),
		ClusterChan: len(mb.clusterChan),
	}
}

// noteBrokerDrop — инкремент счётчика + throttled лог.
func (mb *MetricsBroker) noteBrokerDrop(what string) {
	n := atomic.AddInt64(&mb.brokerDrops, 1)
	mb.logDropThrottled("MetricsBroker: %s отброшено — канал публикации переполнен", what, n)
}

// noteClientDrop — потеря при доставке конкретному подписчику.
func (mb *MetricsBroker) noteClientDrop(clientID string) {
	n := atomic.AddInt64(&mb.clientDrops, 1)
	mb.logDropThrottled("MetricsBroker: доставка клиенту "+clientID+
		" отброшена — канал подписчика переполнен", "client_drop", n)
}

// logDropThrottled логирует не чаще раза в 5 секунд, чтобы всплеск дропов не
// превратился в шторм записей в логе (сам счётчик при этом считает всё).
func (mb *MetricsBroker) logDropThrottled(format, what string, total int64) {
	const interval = int64(5 * time.Second)
	now := time.Now().UnixNano()
	last := atomic.LoadInt64(&mb.lastDropLogNs)
	if now-last < interval || !atomic.CompareAndSwapInt64(&mb.lastDropLogNs, last, now) {
		return
	}
	logger.Get().Warnw("metrics broker dropped data",
		"what", what, "format", format, "total_dropped", total)
}

// NewMetricsBroker - создание нового брокера метрик
func NewMetricsBroker() *MetricsBroker {
	mb := &MetricsBroker{
		subscribers: make(map[string]*MetricsClient),
		metricsChan: make(chan *types.BackendMetrics, 100),
		clusterChan: make(chan *types.ClusterState, 100),
		stopChan:    make(chan struct{}),
	}

	// Запуск goroutine для рассылки метрик
	go mb.run()

	return mb
}

// run - основной цикл рассылки метрик подписчикам
func (mb *MetricsBroker) run() {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case metrics := <-mb.metricsChan:
			mb.publishMetrics(metrics)
		case clusterState := <-mb.clusterChan:
			mb.publishClusterState(clusterState)
		case <-ticker.C:
			// Периодическая отправка для поддержания соединения
		case <-mb.stopChan:
			return
		}
	}
}

// Subscribe - подписка на обновления метрик
func (mb *MetricsBroker) Subscribe(clientID string, done <-chan struct{}) <-chan []byte {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	conn := make(chan []byte, 100)
	mb.subscribers[clientID] = &MetricsClient{
		ID:   clientID,
		Conn: conn,
		Done: done,
	}

	return conn
}

// Unsubscribe - отписка от обновлений метрик
func (mb *MetricsBroker) Unsubscribe(clientID string) {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	if client, exists := mb.subscribers[clientID]; exists {
		close(client.Conn)
		delete(mb.subscribers, clientID)
	}
}

// Publish - публикация метрик бэкенда всем подписчикам
func (mb *MetricsBroker) Publish(metrics *types.BackendMetrics) {
	select {
	case mb.metricsChan <- metrics:
	default:
		// R65d: раньше пропуск был полностью молчаливым.
		mb.noteBrokerDrop("метрики бэкенда")
	}
}

// PublishClusterState - публикация полного состояния кластера всем подписчикам
func (mb *MetricsBroker) PublishClusterState(state *types.ClusterState) {
	select {
	case mb.clusterChan <- state:
	default:
		// R65d: раньше пропуск был полностью молчаливым.
		mb.noteBrokerDrop("состояние кластера")
	}
}

// publishMetrics - рассылка метрик одного бэкенда всем активным подписчикам
func (mb *MetricsBroker) publishMetrics(metrics *types.BackendMetrics) {
	mb.mu.RLock()
	defer mb.mu.RUnlock()

	data, err := json.Marshal(metrics)
	if err != nil {
		return
	}

	for id, client := range mb.subscribers {
		select {
		case client.Conn <- data:
		case <-client.Done:
			// Клиент отключился, помечаем для удаления
			go mb.Unsubscribe(id)
		default:
			// R65d: раньше пропуск был молчаливым — клиент «застывал» без
			// диагностики. Теперь считаем и логируем (throttled).
			mb.noteClientDrop(id)
		}
	}
}

// publishClusterState - рассылка полного состояния кластера всем активным подписчикам
func (mb *MetricsBroker) publishClusterState(state *types.ClusterState) {
	mb.mu.RLock()
	defer mb.mu.RUnlock()

	data, err := json.Marshal(state)
	if err != nil {
		return
	}

	for id, client := range mb.subscribers {
		select {
		case client.Conn <- data:
		case <-client.Done:
			// Клиент отключился, помечаем для удаления
			go mb.Unsubscribe(id)
		default:
			// R65d: раньше пропуск был молчаливым — клиент «застывал» без
			// диагностики. Теперь считаем и логируем (throttled).
			mb.noteClientDrop(id)
		}
	}
}

// Stop - остановка брокера
func (mb *MetricsBroker) Stop() {
	close(mb.stopChan)

	mb.mu.Lock()
	defer mb.mu.Unlock()

	for _, client := range mb.subscribers {
		close(client.Conn)
	}
	mb.subscribers = make(map[string]*MetricsClient)
}

// GetClusterState - получение состояния кластера для отправки клиентам
func (mb *MetricsBroker) GetClusterState(proxy ProxyInterface) *types.ClusterState {
	if proxy == nil {
		return &types.ClusterState{
			Timestamp:       time.Now().UTC(),
			TotalBackends:   0,
			HealthyBackends: 0,
			Backends:        []types.BackendMetrics{},
		}
	}

	return proxy.GetClusterState()
}
