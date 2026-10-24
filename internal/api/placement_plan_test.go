package api

// R96 (S5 плана 2026-10-07): POST /api/v1/placement/plan — предлагаемый план.
//
// Мастер показывает план, а не переключает режим: эндпоинт возвращает числа и
// ГОТОВЫЙ фрагмент конфига, но ничего не применяет.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func r96APIServer(t *testing.T) *Server {
	t.Helper()
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{
		{ID: "r96-a", Host: "localhost", CppWorkerPort: 18092, Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy},
	})
	t.Cleanup(cleanup)
	return s
}

func postPlanR96(t *testing.T, s *Server, raw []byte) (int, map[string]interface{}) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/placement/plan", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	var resp map[string]interface{}
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("ответ не JSON: %v (%s)", err, rec.Body.String())
		}
	}
	return rec.Code, resp
}

func TestPlacementPlan_ReturnsProposalNotChange_R96(t *testing.T) {
	s := r96APIServer(t)

	body := []byte(`{
		"models": [{"name":"m-8b","sizeBytes":8589934592,"hiddenSize":4096,"layers":32}],
		"lang": "ru",
		"tokensPerSecond": 30
	}`)
	code, resp := postPlanR96(t, s, body)
	if code != http.StatusOK {
		t.Fatalf("HTTP %d: %v", code, resp)
	}
	if resp["success"] != true || resp["dryRun"] != true {
		t.Errorf("ответ должен быть успешным dry-run: %v", resp)
	}
	plans, ok := resp["plans"].([]interface{})
	if !ok || len(plans) != 1 {
		t.Fatalf("plans = %v", resp["plans"])
	}
	item, _ := plans[0].(map[string]interface{})
	for _, key := range []string{"model", "strategy", "reason", "rule", "notes", "knownSize"} {
		if _, ok := item[key]; !ok {
			t.Errorf("в предложении нет ключа %q: %v", key, item)
		}
	}
	if item["model"] != "m-8b" {
		t.Errorf("model = %v", item["model"])
	}
	// Размер задан, а метрик VRAM у тестового стенда нет → честный отказ
	// (не гадаем), а не выдуманный «влезает».
	if item["knownSize"] != true {
		t.Errorf("размер передан в запросе — knownSize=true: %v", item)
	}
	if item["strategy"] != "single" || item["degraded"] != true {
		t.Errorf("без метрик VRAM раскладку предлагать нельзя: %v", item)
	}

	// Готовый (но не применённый) фрагмент конфига.
	placement, ok := resp["placement"].(map[string]interface{})
	if !ok {
		t.Fatalf("placement отсутствует: %T", resp["placement"])
	}
	if placement["enabled"] != true {
		t.Errorf("политика в предложении должна быть включена: %v", placement)
	}
	rules, _ := placement["models"].([]interface{})
	if len(rules) != 1 {
		t.Fatalf("правил = %d, ожидалось 1", len(rules))
	}
	rule, _ := rules[0].(map[string]interface{})
	if rule["strategy"] != "auto" {
		t.Errorf("политика пишется как auto (адаптивность), получено %v", rule["strategy"])
	}
	if _, ok := resp["backendGroups"].([]interface{}); !ok {
		t.Errorf("backendGroups должен быть списком: %T", resp["backendGroups"])
	}
	if resp["hint"] == "" || resp["hint"] == nil {
		t.Error("мастеру нужна подсказка «это предложение, а не применение»")
	}

	// Ничего не применено.
	if got := len(s.proxy.BackendGroupStatuses()); got != 0 {
		t.Errorf("план не должен создавать группы: %d", got)
	}
	if got := len(s.proxy.PlacementSettings().Models); got != 0 {
		t.Errorf("план не должен менять политику: %d", got)
	}
}

func TestPlacementPlan_EmptyBodyDiscoversModels_R96(t *testing.T) {
	s := r96APIServer(t)

	// Пустое тело допустимо: планируем всё, что знает стенд (может быть 0).
	code, resp := postPlanR96(t, s, []byte(`{}`))
	if code != http.StatusOK {
		t.Fatalf("HTTP %d: %v", code, resp)
	}
	if _, ok := resp["discoveredModels"]; !ok {
		t.Errorf("мастеру нужно знать, сколько моделей найдено: %v", resp)
	}
	if _, ok := resp["plans"].([]interface{}); !ok {
		t.Errorf("plans должен быть (возможно пустым) списком: %T", resp["plans"])
	}
}

func TestPlacementPlan_RejectsBadRequests_R96(t *testing.T) {
	s := r96APIServer(t)

	if code, _ := postPlanR96(t, s, []byte("{не json")); code != http.StatusBadRequest {
		t.Errorf("битый JSON: HTTP %d, ожидался 400", code)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/placement/plan", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: HTTP %d, ожидался 405", rec.Code)
	}
}
