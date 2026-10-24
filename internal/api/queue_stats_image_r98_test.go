package api

// R98 (S8 плана 2026-10-07): image-нагрузка видна в GET /api/v1/queue/stats.
//
// Проверяем именно HTTP-поверхность: раньше счётчики image-запросов жили только в
// /api/v1/metrics, и страница «Очередь» показывала нули текстового пути, пока GPU
// был занят генерациями.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func queueStatsR98(t *testing.T, s *Server) map[string]interface{} {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/queue/stats", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/queue/stats: HTTP %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("ответ не JSON: %v (%s)", err, rec.Body.String())
	}
	return resp
}

func TestQueueStats_ImageBlockForImageBackend_R98(t *testing.T) {
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{
		{ID: "q-txt", Host: "localhost", CppWorkerPort: 18092, Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy},
		{ID: "q-img", Host: "localhost", ImagePort: 18093, Type: types.BackendTypeImage, Status: types.StatusHealthy},
	})
	defer cleanup()

	resp := queueStatsR98(t, s)
	image, ok := resp["image"].(map[string]interface{})
	if !ok {
		t.Fatalf("на стенде с image-бэкендом блок image обязателен: %v", resp)
	}
	backends, ok := image["backends"].([]interface{})
	if !ok || len(backends) != 1 || backends[0] != "q-img" {
		t.Fatalf("image.backends = %v, ожидался [q-img]", image["backends"])
	}
	// Текстовый узел в image-блок не попадает.
	for _, b := range backends {
		if b == "q-txt" {
			t.Error("текстовый узел не должен числиться в image-бэкендах")
		}
	}
	// Базовые ключи текстовой очереди остались на месте (обратная совместимость).
	for _, key := range []string{"current_size", "processed_total", "admission"} {
		if _, ok := resp[key]; !ok {
			t.Errorf("в ответе пропал ключ %q: %v", key, resp)
		}
	}
}

func TestQueueStats_NoImageBlockOnTextStand_R98(t *testing.T) {
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{
		{ID: "only-txt", Host: "localhost", CppWorkerPort: 18092, Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy},
	})
	defer cleanup()

	resp := queueStatsR98(t, s)
	if _, ok := resp["image"]; ok {
		t.Errorf("на текстовом стенде блок image появляться не должен: %v", resp["image"])
	}
}
