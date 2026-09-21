// handlers_broker_admin.go — R66 (2026-09-20): admin-эндпоинт для счётчиков
// потерь MetricsBroker.
//
// ЗАЧЕМ. В R65d брокер перестал молча терять метрики: появились счётчики
// brokerDrops/clientDrops и throttled WARN. Но счётчики были доступны только
// из Go-кода (`BrokerStats()`), наружу не выведены — оператор видел в логе
// «доставка отброшена», однако не мог ответить на вопрос «сколько всего
// потеряно и растёт ли это прямо сейчас». По логу с throttle раз в 5 секунд
// это не видно; по дашборду тем более — WebUI показывает только последнее
// известное состояние, а не факт потери кадров.
//
// ПУБЛИЧНЫЙ КОНТРАКТ.
//
//	GET /api/v1/admin/metrics-broker/stats
//	→ 200 {
//	      "broker_drops":  <int64>,  // публикация в переполненный канал брокера
//	      "client_drops":  <int64>,  // доставка в переполненный канал подписчика
//	      "subscribers":   <int>,    // текущее число WS-подписчиков
//	      "metrics_chan_len": <int>, // текущая загруженность очередей
//	      "cluster_chan_len": <int>
//	  }
//
// Эндпоинт read-only, но идёт через AuthMiddleware: он раскрывает топологию
// (сколько клиентов подключено) и не должен быть доступен без токена — по той
// же причине, по которой в R65d были закрыты /api/v1/admin/autotune/{history,
// config}. Побочный эффект: эндпоинт ничего не мутирует, поэтому его безопасно
// дёргать из мониторинга с любым интервалом.
package api

import (
	"net/http"
	"time"
)

// brokerStatsResponse — JSON-ответ эндпоинта. Дублирует поля
// brokerStatsSnapshot явно (а не встраивает тип), чтобы HTTP-контракт не
// менялся молча при рефакторинге внутренней структуры.
type brokerStatsResponse struct {
	BrokerDrops    int64  `json:"broker_drops"`
	ClientDrops    int64  `json:"client_drops"`
	Subscribers    int    `json:"subscribers"`
	MetricsChanLen int    `json:"metrics_chan_len"`
	ClusterChanLen int    `json:"cluster_chan_len"`
	// TotalDrops — brokerage + client, чтобы мониторингу не считать самому.
	TotalDrops int64  `json:"total_drops"`
	Timestamp  string `json:"timestamp"`
}

// handleAdminBrokerStats — GET /api/v1/admin/metrics-broker/stats.
func (s *Server) handleAdminBrokerStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.metricsBroker == nil {
		// Не 500: брокер может быть не инициализирован в минимальной сборке,
		// и «нет данных» — честный ответ. Клиент отличает его от «0 потерь».
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "metrics broker not initialized",
		})
		return
	}

	st := s.metricsBroker.BrokerStats()
	s.writeJSON(w, http.StatusOK, brokerStatsResponse{
		BrokerDrops:    st.BrokerDrops,
		ClientDrops:    st.ClientDrops,
		Subscribers:    st.Subscribers,
		MetricsChanLen: st.MetricsChan,
		ClusterChanLen: st.ClusterChan,
		TotalDrops:     st.BrokerDrops + st.ClientDrops,
		Timestamp:      time.Now().UTC().Format(time.RFC3339),
	})
}
