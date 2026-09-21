//go:build llama_stub

// broker_stats_endpoint_r66_test.go — R66 (2026-09-20): admin-эндпоинт
// GET /api/v1/admin/metrics-broker/stats.
//
// Проверяем ровно то, что важно контрактно:
//  1. эндпоинт требует токен (как остальные admin-ручки после R65d);
//  2. считает и отдаёт broker_drops/client_drops, которые нельзя получить
//     иначе как из лога с throttle;
//  3. total_drops = broker + client (клиент мониторинга не должен складывать);
//  4. ошибка метода и отсутствие брокера не выглядят как «0 потерь».
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// getBrokerStats дёргает эндпоинт и возвращает код + распарсенное тело.
func getBrokerStats(t *testing.T, s *Server, token string) (int, map[string]interface{}) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/metrics-broker/stats", nil)
	if token != "" {
		req.Header.Set("X-API-Token", token)
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	var resp map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec.Code, resp
}

// TestR66_BrokerStatsEndpoint_RequiresAuth — без токена 401.
//
// Эндпоинт раскрывает число подключённых WS-клиентов (топология), поэтому
// должен быть закрыт так же, как /api/v1/admin/autotune/config.
func TestR66_BrokerStatsEndpoint_RequiresAuth(t *testing.T) {
	s := authEnabledServer(t)

	if code, _ := getBrokerStats(t, s, ""); code != http.StatusUnauthorized {
		t.Errorf("GET без токена = %d, want 401", code)
	}
	if code, _ := getBrokerStats(t, s, "r65d-secret-token"); code != http.StatusOK {
		t.Errorf("GET с валидным токеном = %d, want 200", code)
	}
}

// TestR66_BrokerStatsEndpoint_ReportsDrops — счётчики потерь видны снаружи.
func TestR66_BrokerStatsEndpoint_ReportsDrops(t *testing.T) {
	s := authEnabledServer(t)

	// Нулевое состояние: поля присутствуют и равны нулю (а не отсутствуют —
	// иначе мониторинг не отличит «нет потерь» от «поле переименовали»).
	code, resp := getBrokerStats(t, s, "r65d-secret-token")
	if code != http.StatusOK {
		t.Fatalf("код %d, want 200 (тело %v)", code, resp)
	}
	for _, key := range []string{"broker_drops", "client_drops", "subscribers", "total_drops"} {
		if _, ok := resp[key]; !ok {
			t.Errorf("в ответе нет поля %q — мониторинг не сможет отличить "+
				"нулевые потери от переименования поля", key)
		}
	}

	// Создаём потери и проверяем, что они доехали до HTTP-ответа.
	mb := s.metricsBroker
	if mb == nil {
		t.Fatal("s.metricsBroker == nil — эндпоинт не сможет ничего отдать")
	}
	for i := 0; i < 1000; i++ {
		mb.Publish(&types.BackendMetrics{ID: "b1"})
	}

	code, resp = getBrokerStats(t, s, "r65d-secret-token")
	if code != http.StatusOK {
		t.Fatalf("код %d, want 200", code)
	}

	brokerDrops := jsonInt(t, resp, "broker_drops")
	clientDrops := jsonInt(t, resp, "client_drops")
	totalDrops := jsonInt(t, resp, "total_drops")

	if brokerDrops == 0 {
		t.Errorf("broker_drops = 0 после 1000 публикаций — потери не доехали "+
			"до эндпоинта (тело %v)", resp)
	}
	if totalDrops != brokerDrops+clientDrops {
		t.Errorf("total_drops = %d, want %d (broker %d + client %d)",
			totalDrops, brokerDrops+clientDrops, brokerDrops, clientDrops)
	}

	// Значения из HTTP должны совпадать с внутренними (иначе эндпоинт
	// показывает не то, что реально считает брокер).
	st := mb.BrokerStats()
	if brokerDrops != st.BrokerDrops || clientDrops != st.ClientDrops {
		t.Errorf("HTTP отдал broker=%d client=%d, а брокер считает broker=%d client=%d",
			brokerDrops, clientDrops, st.BrokerDrops, st.ClientDrops)
	}
}

// TestR66_BrokerStatsEndpoint_MethodAndNilGuard — POST запрещён; при
// отсутствующем брокере ответ не выглядит как «нет потерь».
func TestR66_BrokerStatsEndpoint_MethodAndNilGuard(t *testing.T) {
	s := authEnabledServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/metrics-broker/stats", nil)
	req.Header.Set("X-API-Token", "r65d-secret-token")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d, want 405", rec.Code)
	}

	// Прямой вызов handler'а с nil-брокером: 503, а не 200 с нулями.
	bare := &Server{}
	rec2 := httptest.NewRecorder()
	bare.handleAdminBrokerStats(rec2, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec2.Code != http.StatusServiceUnavailable {
		t.Errorf("с nil брокером код = %d, want 503 (200 с нулями ввёл бы "+
			"мониторинг в заблуждение)", rec2.Code)
	}
}

// jsonInt достаёт int64-совместимое значение из распарсенного JSON.
func jsonInt(t *testing.T, m map[string]interface{}, key string) int64 {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("в ответе нет поля %q", key)
	}
	f, ok := v.(float64) // encoding/json парсит все числа как float64
	if !ok {
		t.Fatalf("поле %q = %T (%v), want число", key, v, v)
	}
	return int64(f)
}
