package api

import (
	"encoding/json"
	"sync"
	"time"

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
	stopChan    chan struct{}
}

// NewMetricsBroker - создание нового брокера метрик
func NewMetricsBroker() *MetricsBroker {
	mb := &MetricsBroker{
		subscribers: make(map[string]*MetricsClient),
		metricsChan: make(chan *types.BackendMetrics, 100),
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
			mb.publish(metrics)
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

// Publish - публикация метрик всем подписчикам
func (mb *MetricsBroker) Publish(metrics *types.BackendMetrics) {
	select {
	case mb.metricsChan <- metrics:
	default:
		// Канал переполнен, пропускаем метрику
	}
}

// publish - рассылка метрик всем активным подписчикам
func (mb *MetricsBroker) publish(metrics *types.BackendMetrics) {
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
			// Канал клиента переполнен, пропускаем
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
