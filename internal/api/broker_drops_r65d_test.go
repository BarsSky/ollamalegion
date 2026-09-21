//go:build llama_stub

// broker_drops_r65d_test.go — R65d (2026-09-20): счётчики потерь в MetricsBroker.
//
// Раньше оба `default:` в publish-путях молча отбрасывали данные:
//   - Publish / PublishClusterState: переполнение metricsChan/clusterChan (100);
//   - publishMetrics / publishClusterState: переполнение канала подписчика (100).
//
// Клиент «застывал», и в логах/метриках не было ни следа. Теперь потери
// считаются, а факт дропа логируется с throttle.
package api

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR65d_Broker_CountsPublishDrops — переполнение канала публикации считается.
func TestR65d_Broker_CountsPublishDrops(t *testing.T) {
	mb := NewMetricsBroker()
	defer mb.Stop()

	before := mb.BrokerStats()
	if before.BrokerDrops != 0 {
		t.Fatalf("начальное значение broker_drops = %d, want 0", before.BrokerDrops)
	}

	// Заливаем канал сверх capacity (100), не давая run() разгрести.
	// run() читает конкурентно, поэтому шлём заметно больше capacity.
	total := 1000
	for i := 0; i < total; i++ {
		mb.Publish(&types.BackendMetrics{ID: "b1"})
	}

	after := mb.BrokerStats()
	if after.BrokerDrops == 0 {
		t.Errorf("broker_drops = 0 после %d публикаций — потери не считаются "+
			"(раньше это было молчаливое отбрасывание)", total)
	}
}

// TestR65d_Broker_CountsClientDrops — переполнение канала ПОДПИСЧИКА считается
// отдельным счётчиком: он указывает на конкретного медленного клиента.
//
// Тест вызывает publishMetrics НАПРЯМУЮ, минуя промежуточный metricsChan.
// Это делает проверку детерминированной: при обычной публикации фоновый run()
// успевает разгребать очередь, и до подписчика доходит слишком мало сообщений,
// чтобы его канал (capacity 100) переполнился.
func TestR65d_Broker_CountsClientDrops(t *testing.T) {
	mb := NewMetricsBroker()
	defer mb.Stop()

	// Подписчик, который НЕ читает свой канал → он переполнится.
	done := make(chan struct{})
	defer close(done)
	mb.Subscribe("slow-client", done)

	if stats := mb.BrokerStats(); stats.Subscribers != 1 {
		t.Fatalf("subscribers = %d, want 1", stats.Subscribers)
	}

	// 100 сообщений заполнят канал подписчика (capacity 100), следующие — дроп.
	const total = 200
	for i := 0; i < total; i++ {
		mb.publishMetrics(&types.BackendMetrics{ID: "b1"})
	}

	stats := mb.BrokerStats()
	wantDrops := int64(total - 100)
	if stats.ClientDrops == 0 {
		t.Errorf("client_drops = 0 после %d доставок при нечитающем подписчике — "+
			"потеря доставки не считается (клиент «застывает» без диагностики); stats=%+v",
			total, stats)
	} else if stats.ClientDrops != wantDrops {
		t.Errorf("client_drops = %d, want %d (канал подписчика вмещает 100 из %d)",
			stats.ClientDrops, wantDrops, total)
	}
}

// TestR65d_Broker_StatsShape — снимок содержит ожидаемые поля (контракт для
// admin-эндпоинта/дашборда).
func TestR65d_Broker_StatsShape(t *testing.T) {
	mb := NewMetricsBroker()
	defer mb.Stop()

	stats := mb.BrokerStats()
	if stats.Subscribers != 0 {
		t.Errorf("subscribers = %d, want 0", stats.Subscribers)
	}
	// Доступные буферы — каналы с capacity 100.
	if stats.MetricsChan != 0 || stats.ClusterChan != 0 {
		t.Errorf("буферы должны быть пустыми: metrics=%d cluster=%d",
			stats.MetricsChan, stats.ClusterChan)
	}
}

// TestR65d_Broker_StatsNilSafe — nil-брокер не паникует (защита от вызовов
// до инициализации).
func TestR65d_Broker_StatsNilSafe(t *testing.T) {
	var mb *MetricsBroker
	if got := mb.BrokerStats(); got != (brokerStatsSnapshot{}) {
		t.Errorf("nil-брокер вернул %+v, want zero value", got)
	}
}
