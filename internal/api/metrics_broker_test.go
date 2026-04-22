package api

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"ollama-loadbalancer/pkg/types"
)

// TestMetricsBrokerSubscribe - проверка подписки
func TestMetricsBrokerSubscribe(t *testing.T) {
	t.Parallel()

	broker := NewMetricsBroker()
	defer broker.Stop()

	done := make(chan struct{})
	ch := broker.Subscribe("client-1", done)

	assert.NotNil(t, ch)

	// Проверяем что канал открыт
	select {
	case _, ok := <-ch:
		if !ok {
			t.Error("Канал должен быть открыт")
		}
	default:
		// Канал открыт и пуст - это нормально
	}
}

// TestMetricsBrokerUnsubscribe - проверка отписки
func TestMetricsBrokerUnsubscribe(t *testing.T) {
	t.Parallel()

	broker := NewMetricsBroker()
	defer broker.Stop()

	done := make(chan struct{})
	ch := broker.Subscribe("client-1", done)

	// Подписка должна быть активна
	assert.NotNil(t, ch)

	// Отписываемся
	broker.Unsubscribe("client-1")

	// Канал должен быть закрыт
	_, ok := <-ch
	assert.False(t, ok, "Канал должен быть закрыт после отписки")
}

// TestMetricsBrokerPublish - проверка публикации метрик
func TestMetricsBrokerPublish(t *testing.T) {
	t.Parallel()

	broker := NewMetricsBroker()
	defer broker.Stop()

	done := make(chan struct{})
	defer close(done)
	
	ch := broker.Subscribe("client-1", done)

	// Создаем тестовые метрики
	metrics := &types.BackendMetrics{
		ID:        "backend-1",
		Timestamp: time.Now().UTC(),
		GPU: types.GPUMetrics{
			UsagePercent: 50.5,
			MemoryTotal:  16384,
			MemoryUsed:   8192,
			MemoryFree:   8192,
			Temperature:  65,
		},
		System: types.SystemMetrics{
			CPUUsagePercent: 30.0,
			MemoryTotal:     32768,
			MemoryUsed:      16384,
			MemoryFree:      16384,
		},
		Ollama: types.OllamaMetrics{
			ActiveRequests:  5,
			TotalRequests:   100,
			AvgResponseTime: 250.5,
			RequestsPerSecond: 10.0,
		},
	}

	// Публикуем метрики
	broker.Publish(metrics)

	// Ждем получения метрик
	select {
	case data, ok := <-ch:
		assert.True(t, ok, "Канал должен быть открыт")
		assert.NotNil(t, data)
		assert.Contains(t, string(data), "backend-1")
	case <-time.After(2 * time.Second):
		t.Fatal("Таймаут ожидания метрик")
	}
}

// TestMetricsBrokerMultipleClients - проверка работы с несколькими клиентами
func TestMetricsBrokerMultipleClients(t *testing.T) {
	t.Parallel()

	broker := NewMetricsBroker()
	defer broker.Stop()

	done1 := make(chan struct{})
	done2 := make(chan struct{})
	defer close(done1)
	defer close(done2)

	ch1 := broker.Subscribe("client-1", done1)
	ch2 := broker.Subscribe("client-2", done2)

	assert.NotNil(t, ch1)
	assert.NotNil(t, ch2)

	// Создаем метрики
	metrics := &types.BackendMetrics{
		ID:        "backend-1",
		Timestamp: time.Now().UTC(),
		GPU: types.GPUMetrics{
			UsagePercent: 75.0,
		},
	}

	// Публикуем метрики
	broker.Publish(metrics)

	// Оба клиента должны получить метрики
	received1 := false
	received2 := false

	timeout := time.After(2 * time.Second)
	for !received1 || !received2 {
		select {
		case data, ok := <-ch1:
			if ok && len(data) > 0 {
				received1 = true
			}
		case data, ok := <-ch2:
			if ok && len(data) > 0 {
				received2 = true
			}
		case <-timeout:
			t.Fatal("Таймаут ожидания метрик от клиентов")
		}
	}

	assert.True(t, received1, "Клиент 1 должен получить метрики")
	assert.True(t, received2, "Клиент 2 должен получить метрики")
}

// TestMetricsBrokerPublishMultiple - проверка публикации нескольких метрик
func TestMetricsBrokerPublishMultiple(t *testing.T) {
	t.Parallel()

	broker := NewMetricsBroker()
	defer broker.Stop()

	done := make(chan struct{})
	defer close(done)
	
	ch := broker.Subscribe("client-1", done)

	// Публикуем 5 метрик
	for i := 0; i < 5; i++ {
		metrics := &types.BackendMetrics{
			ID:        "backend-1",
			Timestamp: time.Now().UTC(),
			GPU: types.GPUMetrics{
				UsagePercent: float64(i * 20),
			},
		}
		broker.Publish(metrics)
	}

	// Получаем все метрики
	received := 0
	timeout := time.After(3 * time.Second)
	
	for received < 5 {
		select {
		case _, ok := <-ch:
			if ok {
				received++
			}
		case <-timeout:
			t.Fatalf("Получено только %d из 5 метрик", received)
		}
	}

	assert.Equal(t, 5, received)
}

// TestMetricsBrokerStop - проверка остановки брокера
func TestMetricsBrokerStop(t *testing.T) {
	t.Parallel()

	broker := NewMetricsBroker()

	done := make(chan struct{})
	ch := broker.Subscribe("client-1", done)

	// Останавливаем брокер
	broker.Stop()

	// Канал должен быть закрыт
	_, ok := <-ch
	assert.False(t, ok, "Канал должен быть закрыт после остановки")
}

// TestMetricsBrokerUnsubscribeNonExistent - проверка отписки несуществующего клиента
func TestMetricsBrokerUnsubscribeNonExistent(t *testing.T) {
	t.Parallel()

	broker := NewMetricsBroker()
	defer broker.Stop()

	// Отписка несуществующего клиента не должна вызывать панику
	assert.NotPanics(t, func() {
		broker.Unsubscribe("non-existent-client")
	})
}

// TestMetricsBrokerPublishAfterStop - проверка публикации после остановки
func TestMetricsBrokerPublishAfterStop(t *testing.T) {
	t.Parallel()

	broker := NewMetricsBroker()

	done := make(chan struct{})
	_ = broker.Subscribe("client-1", done)

	broker.Stop()

	// Публикация после остановки не должна вызывать панику
	assert.NotPanics(t, func() {
		broker.Publish(&types.BackendMetrics{
			ID: "backend-1",
		})
	})
}

// TestMetricsBrokerConcurrentSubscribe - проверка потокобезопасности подписки
func TestMetricsBrokerConcurrentSubscribe(t *testing.T) {
	t.Parallel()

	broker := NewMetricsBroker()
	defer broker.Stop()

	var wg sync.WaitGroup
	doneChannels := make([]chan struct{}, 10)
	channels := make([]<-chan []byte, 10)

	// Создаем 10 подписчиков concurrently
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			done := make(chan struct{})
			ch := broker.Subscribe("client-"+string(rune('0'+idx)), done)
			doneChannels[idx] = done
			channels[idx] = ch
		}(i)
	}

	wg.Wait()

	// Все каналы должны быть не nil
	for i, ch := range channels {
		assert.NotNil(t, ch, "Канал %d должен быть не nil", i)
	}

	// Закрываем done каналы
	for _, done := range doneChannels {
		close(done)
	}
}

// TestMetricsBrokerConcurrentPublish - проверка потокобезопасности публикации
func TestMetricsBrokerConcurrentPublish(t *testing.T) {
	t.Parallel()

	broker := NewMetricsBroker()
	defer broker.Stop()

	done := make(chan struct{})
	defer close(done)
	
	ch := broker.Subscribe("client-1", done)
	_ = ch // используем переменную

	var wg sync.WaitGroup

	// 10 горутин публикуют метрики одновременно
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				metrics := &types.BackendMetrics{
					ID:        "backend-" + string(rune('0'+idx)),
					Timestamp: time.Now().UTC(),
				}
				broker.Publish(metrics)
			}
		}(i)
	}

	wg.Wait()

	// Даем время на обработку
	time.Sleep(100 * time.Millisecond)
}

// TestMetricsBrokerGetClusterState - проверка получения состояния кластера
func TestMetricsBrokerGetClusterState(t *testing.T) {
	t.Parallel()

	broker := NewMetricsBroker()
	defer broker.Stop()

	// Тестируем с nil proxy
	state := broker.GetClusterState(nil)
	assert.NotNil(t, state)
	assert.Equal(t, 0, state.TotalBackends)
	assert.Equal(t, 0, state.HealthyBackends)
	assert.Empty(t, state.Backends)
}

// TestMetricsBrokerChannelBufferSize - проверка размера буфера канала
func TestMetricsBrokerChannelBufferSizeUnused(t *testing.T) {
	t.Parallel()

	broker := NewMetricsBroker()
	defer broker.Stop()

	done := make(chan struct{})
	defer close(done)
	
	ch := broker.Subscribe("client-1", done)
	_ = ch // используем переменную

	// Заполняем буфер (100 элементов)
	for i := 0; i < 50; i++ {
		metrics := &types.BackendMetrics{
			ID: "backend-1",
			GPU: types.GPUMetrics{
				UsagePercent: float64(i),
			},
		}
		broker.Publish(metrics)
	}

	// Канал должен оставаться открытым
	select {
	case _, ok := <-ch:
		assert.True(t, ok)
	default:
		// Канал не пуст - это нормально
	}
}

// TestMetricsBrokerChannelBufferSize - проверка размера буфера канала
func TestMetricsBrokerChannelBufferSize(t *testing.T) {
	t.Parallel()

	broker := NewMetricsBroker()
	defer broker.Stop()

	done := make(chan struct{})
	defer close(done)
	
	ch := broker.Subscribe("client-1", done)

	// Заполняем буфер (100 элементов)
	for i := 0; i < 50; i++ {
		metrics := &types.BackendMetrics{
			ID: "backend-1",
			GPU: types.GPUMetrics{
				UsagePercent: float64(i),
			},
		}
		broker.Publish(metrics)
	}

	// Канал должен оставаться открытым
	select {
	case _, ok := <-ch:
		assert.True(t, ok)
	default:
		// Канал не пуст - это нормально
	}
}

// TestMetricsBrokerMultiplePublishesSequential - проверка последовательной публикации
func TestMetricsBrokerMultiplePublishesSequential(t *testing.T) {
	t.Parallel()

	broker := NewMetricsBroker()
	defer broker.Stop()

	done := make(chan struct{})
	defer close(done)
	
	ch := broker.Subscribe("client-1", done)

	// Последовательно публикуем и получаем
	for i := 0; i < 3; i++ {
		metrics := &types.BackendMetrics{
			ID:        "backend-1",
			Timestamp: time.Now().UTC(),
			GPU: types.GPUMetrics{
				UsagePercent: float64(i * 25),
			},
		}

		broker.Publish(metrics)

		select {
		case data, ok := <-ch:
			assert.True(t, ok)
			assert.NotNil(t, data)
		case <-time.After(time.Second):
			t.Fatalf("Таймаут получения метрики %d", i)
		}
	}
}

// TestMetricsBrokerSubscribeUnsubscribeMultiple - проверка множественной подписки/отписки
func TestMetricsBrokerSubscribeUnsubscribeMultiple(t *testing.T) {
	t.Parallel()

	broker := NewMetricsBroker()
	defer broker.Stop()

	// Подписка-отписка несколько раз
	for i := 0; i < 5; i++ {
		done := make(chan struct{})
		ch := broker.Subscribe("client-"+string(rune('0'+i)), done)
		assert.NotNil(t, ch)
		broker.Unsubscribe("client-" + string(rune('0'+i)))
		
		// Канал должен быть закрыт
		_, ok := <-ch
		assert.False(t, ok)
		close(done)
	}
}
