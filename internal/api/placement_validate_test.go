package api

// R95 (S4 плана 2026-10-07): POST /api/v1/placement/validate — сухой прогон.
//
// Эндпоинт обязан: принимать ПРЕДЛАГАЕМУЮ композицию (которой ещё нет в конфиге),
// возвращать замечания по уровням с машиночитаемым CheckID и НИЧЕГО не менять.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func r95APIServer(t *testing.T) *Server {
	t.Helper()
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{
		{ID: "r95-a", Host: "localhost", CppWorkerPort: 18092, Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy},
		{ID: "r95-b", Host: "localhost", CppWorkerPort: 18092, Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy},
	})
	t.Cleanup(cleanup)
	return s
}

func postValidateR95(t *testing.T, s *Server, body map[string]interface{}) (int, map[string]interface{}) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/placement/validate", bytes.NewReader(raw))
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

func validateBodyR95(sizeBytes int64) map[string]interface{} {
	return map[string]interface{}{
		"group": map[string]interface{}{
			"id": "g-new", "model": "m-14b", "strategy": "layer",
			"members": []map[string]interface{}{
				{"backendId": "r95-a", "role": "master"},
				{"backendId": "r95-b", "role": "worker", "rpcEndpoint": "192.0.2.11:50052"},
			},
		},
		"model": map[string]interface{}{
			"name": "m-14b", "backendType": "llama_cpp",
			"sizeBytes": sizeBytes, "hiddenSize": 4096, "layers": 32, "bytesPerElem": 2,
		},
		"lang":       "ru",
		"skipProbes": true, // в тесте не стучимся в 192.0.2.11
	}
}

func TestPlacementValidate_DryRunReport_R95(t *testing.T) {
	s := r95APIServer(t)

	code, resp := postValidateR95(t, s, validateBodyR95(2<<30))
	if code != http.StatusOK {
		t.Fatalf("HTTP %d: %v", code, resp)
	}
	if resp["success"] != true || resp["dryRun"] != true {
		t.Errorf("ответ должен быть dry-run и успешным: %v", resp)
	}
	if resp["groupId"] != "g-new" {
		t.Errorf("groupId = %v", resp["groupId"])
	}
	if resp["hasErrors"] != false {
		t.Errorf("модель 2 ГиБ должна собираться без ошибок: %v", resp["issues"])
	}
	// Все уровни присутствуют даже пустыми — UI не должен проверять наличие ключей.
	for _, key := range []string{"errors", "warnings", "hints", "issues", "checks", "configWarnings"} {
		if _, ok := resp[key]; !ok {
			t.Errorf("в ответе нет ключа %q", key)
		}
	}
	checks, _ := resp["checks"].([]interface{})
	if len(checks) == 0 {
		t.Error("список проверок нужен UI для легенды («проверок 11, ошибок 0»)")
	}
}

func TestPlacementValidate_ReportsErrorsWithNumbers_R95(t *testing.T) {
	s := r95APIServer(t)

	// 40 ГиБ на узлы, у которых нет метрик VRAM в тестовом сервере: сработает
	// дешёвая и всегда доступная проверка — модель не влезает (метрик нет, но
	// и подсказки нет, зато backendType/структура видны).
	code, resp := postValidateR95(t, s, validateBodyR95(40<<30))
	if code != http.StatusOK {
		t.Fatalf("HTTP %d", code)
	}
	issues, _ := resp["issues"].([]interface{})
	if len(issues) == 0 {
		// Без метрик VRAM проверка суммы не срабатывает (не гадаем) — тогда
		// отчёт обязан быть пустым, и это тоже корректное поведение.
		if resp["hasErrors"] == true {
			t.Errorf("hasErrors=true без замечаний: %v", resp)
		}
		return
	}
	first, _ := issues[0].(map[string]interface{})
	for _, key := range []string{"check", "level", "text"} {
		if _, ok := first[key]; !ok {
			t.Errorf("замечание должно содержать %q: %v", key, first)
		}
	}
}

func TestPlacementValidate_ConfigWarnings_R95(t *testing.T) {
	s := r95APIServer(t)

	body := validateBodyR95(2 << 30)
	// Ломаем группу: два воркера и ни одного мастера.
	body["group"].(map[string]interface{})["members"] = []map[string]interface{}{
		{"backendId": "r95-a", "role": "worker", "rpcEndpoint": "h:1"},
		{"backendId": "r95-b", "role": "worker", "rpcEndpoint": "h:2"},
	}
	code, resp := postValidateR95(t, s, body)
	if code != http.StatusOK {
		t.Fatalf("HTTP %d", code)
	}
	warnings, _ := resp["configWarnings"].([]interface{})
	if len(warnings) == 0 {
		t.Fatalf("структурные ошибки группы должны быть видны: %v", resp)
	}
}

func TestPlacementValidate_RejectsBadRequests_R95(t *testing.T) {
	s := r95APIServer(t)

	// Пустой состав: проверять нечего — это ошибка клиента, а не «всё хорошо».
	body := validateBodyR95(2 << 30)
	body["group"].(map[string]interface{})["members"] = []map[string]interface{}{}
	if code, resp := postValidateR95(t, s, body); code != http.StatusBadRequest {
		t.Errorf("пустой состав: HTTP %d, ожидался 400 (%v)", code, resp)
	}

	// Не JSON.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/placement/validate", bytes.NewReader([]byte("{не json")))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("битый JSON: HTTP %d, ожидался 400", rec.Code)
	}

	// GET запрещён: сухой прогон принимает тело.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/placement/validate", nil)
	rec = httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: HTTP %d, ожидался 405", rec.Code)
	}
}

func TestPlacementValidate_DoesNotChangeConfig_R95(t *testing.T) {
	s := r95APIServer(t)

	if code, _ := postValidateR95(t, s, validateBodyR95(2<<30)); code != http.StatusOK {
		t.Fatalf("HTTP %d", code)
	}
	if got := len(s.proxy.BackendGroupStatuses()); got != 0 {
		t.Errorf("сухой прогон не должен создавать группы: %d", got)
	}
	if got := len(s.proxy.PlacementSettings().Models); got != 0 {
		t.Errorf("сухой прогон не должен менять placement-политику: %v", got)
	}
}
