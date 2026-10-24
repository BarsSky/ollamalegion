package api

// R100 (S5/S6 плана 2026-10-07): PUT /api/v1/placement — применение политики.
//
// Это «запись balancing.placement», которой не хватало, чтобы AC S5 («пустой
// стенд → рабочая политика») и S6 («объединить две машины») были выполнимы из UI.
//
// Проверяем главное: (1) применение доходит до ЖИВОГО балансера, а не только до
// файла; (2) заведомо сломанный конфиг не применяется вовсе; (3) неудачная
// персистенция не выдаётся за успех.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func placementApplyServerR100(t *testing.T) (*Server, *int) {
	t.Helper()
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{
		{ID: "a-1", Host: "localhost", CppWorkerPort: 18092, Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy},
		{ID: "b-1", Host: "localhost", CppWorkerPort: 18092, Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy},
	})
	t.Cleanup(cleanup)
	saves := 0
	s.SetConfigSaver(func() error {
		saves++
		return nil
	})
	return s, &saves
}

func putPlacementR100(t *testing.T, s *Server, body map[string]interface{}) (int, map[string]interface{}) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPut, "/api/v1/placement", bytes.NewReader(raw))
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

func validPlacementBodyR100() map[string]interface{} {
	return map[string]interface{}{
		"placement": map[string]interface{}{
			"enabled":  true,
			"fallback": "error",
			"models": []map[string]interface{}{
				{"model": "qwen3-14b*", "strategy": "sharded", "reason": "R100: раскладка на две машины"},
			},
		},
		"backendGroups": []map[string]interface{}{{
			"id": "g1", "model": "qwen3-14b*", "strategy": "layer",
			"members": []map[string]interface{}{
				{"backendId": "a-1", "role": "master"},
				{"backendId": "b-1", "role": "worker", "rpcEndpoint": "192.0.2.11:50052"},
			},
		}},
	}
}

func TestPlacementApply_AppliesToLiveBalancer_R100(t *testing.T) {
	s, saves := placementApplyServerR100(t)

	code, resp := putPlacementR100(t, s, validPlacementBodyR100())
	if code != http.StatusOK {
		t.Fatalf("HTTP %d: %v", code, resp)
	}
	if resp["applied"] != true || resp["persisted"] != true {
		t.Errorf("applied/persisted = %v/%v, ожидалось true/true", resp["applied"], resp["persisted"])
	}
	if *saves != 1 {
		t.Errorf("configSaver вызван %d раз, ожидался 1", *saves)
	}

	// Политика действует на ЖИВОМ балансере, а не только записана в файл.
	live := s.proxy.PlacementSettings()
	if !live.Enabled || len(live.Models) != 1 {
		t.Fatalf("политика не применена к балансеру: %+v", live)
	}
	if len(s.proxy.BackendGroupStatuses()) != 1 {
		t.Fatalf("группа не зарегистрирована: %+v", s.proxy.BackendGroupStatuses())
	}

	// GET отражает применённое состояние (и группы видны оператору).
	got := getPlacementJSON(t, s, "")
	if got["enabled"] != true {
		t.Errorf("GET enabled = %v", got["enabled"])
	}
	if groups, ok := got["groups"].([]interface{}); !ok || len(groups) != 1 {
		t.Errorf("GET groups = %v", got["groups"])
	}

	// Ответ несёт состояние ПОСЛЕ применения — UI не должен ходить за ним вторым запросом.
	if _, ok := resp["groups"].([]interface{}); !ok {
		t.Errorf("в ответе должны быть состояния групп: %T", resp["groups"])
	}
}

func TestPlacementApply_DryRunChangesNothing_R100(t *testing.T) {
	s, saves := placementApplyServerR100(t)

	body := validPlacementBodyR100()
	body["dryRun"] = true
	code, resp := putPlacementR100(t, s, body)
	if code != http.StatusOK {
		t.Fatalf("HTTP %d: %v", code, resp)
	}
	if resp["dryRun"] != true || resp["valid"] != true || resp["applied"] != false {
		t.Errorf("dry-run ответ неверен: %v", resp)
	}
	if *saves != 0 {
		t.Errorf("dry-run не должен сохранять конфиг (вызовов: %d)", *saves)
	}
	if s.proxy.PlacementSettings().Enabled {
		t.Error("dry-run не должен менять живую политику")
	}
}

func TestPlacementApply_RejectsInvalidPlacement_R100(t *testing.T) {
	s, saves := placementApplyServerR100(t)

	body := validPlacementBodyR100()
	body["placement"] = map[string]interface{}{
		"enabled": true,
		"models":  []map[string]interface{}{{"model": "m", "strategy": "spread"}},
	}
	code, resp := putPlacementR100(t, s, body)
	if code != http.StatusBadRequest {
		t.Fatalf("HTTP %d, ожидался 400: %v", code, resp)
	}
	if resp["applied"] != false {
		t.Error("сломанный конфиг не должен применяться")
	}
	if *saves != 0 {
		t.Error("при отказе конфиг сохранять нельзя")
	}
	if s.proxy.PlacementSettings().Enabled {
		t.Error("живая политика не должна измениться при отказе валидации")
	}
	warnings, _ := resp["warnings"].([]interface{})
	if len(warnings) == 0 {
		t.Error("оператор должен увидеть, что именно не так")
	}
}

func TestPlacementApply_RejectsBrokenGroup_R100(t *testing.T) {
	s, _ := placementApplyServerR100(t)

	body := validPlacementBodyR100()
	// Два воркера и ни одного мастера — группу собрать нельзя.
	body["backendGroups"] = []map[string]interface{}{{
		"id": "broken", "model": "m", "strategy": "layer",
		"members": []map[string]interface{}{
			{"backendId": "a-1", "role": "worker", "rpcEndpoint": "h:1"},
			{"backendId": "b-1", "role": "worker", "rpcEndpoint": "h:2"},
		},
	}}
	code, resp := putPlacementR100(t, s, body)
	if code != http.StatusBadRequest {
		t.Fatalf("HTTP %d, ожидался 400: %v", code, resp)
	}
	if s.proxy.PlacementSettings().Enabled {
		t.Error("политика не должна применяться вместе со сломанной группой")
	}
}

func TestPlacementApply_AllowWarningsApplies_R100(t *testing.T) {
	s, _ := placementApplyServerR100(t)

	body := validPlacementBodyR100()
	// `rpc` — legacy-стратегия: валидатор про неё предупреждает.
	body["placement"] = map[string]interface{}{
		"enabled": true,
		"models":  []map[string]interface{}{{"model": "m", "strategy": "rpc"}},
	}
	body["allowWarnings"] = true

	code, resp := putPlacementR100(t, s, body)
	if code != http.StatusOK || resp["applied"] != true {
		t.Fatalf("с allowWarnings применение разрешено: HTTP %d, %v", code, resp)
	}
	warnings, _ := resp["warnings"].([]interface{})
	if len(warnings) == 0 {
		t.Error("подтверждённые предупреждения всё равно должны быть перечислены")
	}
	if !s.proxy.PlacementSettings().Enabled {
		t.Error("политика должна быть применена")
	}
}

func TestPlacementApply_NoSaverIsHonest_R100(t *testing.T) {
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{
		{ID: "a-1", Host: "localhost", CppWorkerPort: 18092, Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy},
	})
	defer cleanup()
	// configSaver НЕ установлен (типично для конфига из env).

	code, resp := putPlacementR100(t, s, validPlacementBodyR100())
	if code != http.StatusOK {
		t.Fatalf("HTTP %d: %v", code, resp)
	}
	if resp["applied"] != true {
		t.Error("политика применяется к живому балансеру и без сохранения файла")
	}
	if resp["persisted"] != false {
		t.Error("нельзя утверждать, что конфиг сохранён, если сохранять нечем")
	}
	if resp["persistError"] == "" || resp["persistError"] == nil {
		t.Error("нужна причина, почему не сохранено")
	}
}

func TestPlacementApply_SaverFailureIsNotSuccess_R100(t *testing.T) {
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{
		{ID: "a-1", Host: "localhost", CppWorkerPort: 18092, Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy},
	})
	defer cleanup()
	// bundled-стек монтирует config.json только для чтения — сохранение падает.
	s.SetConfigSaver(func() error { return errTestSaverR100{} })

	code, resp := putPlacementR100(t, s, validPlacementBodyR100())
	if code != http.StatusOK {
		t.Fatalf("HTTP %d: %v", code, resp)
	}
	if resp["persisted"] != false {
		t.Error("провал сохранения не должен выглядеть успехом")
	}
	// При этом политика УЖЕ действует: об этом ответ тоже обязан сказать.
	if resp["applied"] != true || !s.proxy.PlacementSettings().Enabled {
		t.Error("политика применяется к живому балансеру даже при провале записи файла")
	}
}

func TestPlacementApply_GetUnchanged_R100(t *testing.T) {
	s, _ := placementApplyServerR100(t)

	// Метод-бранч не должен ломать наблюдаемость: GET по-прежнему работает.
	if code, _ := putPlacementR100(t, s, map[string]interface{}{}); code == http.StatusMethodNotAllowed {
		t.Error("PUT должен обрабатываться, а не отвергаться как метод")
	}
	_ = getPlacementJSON(t, s, "")

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/placement", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE: HTTP %d, ожидался 405", rec.Code)
	}
}

type errTestSaverR100 struct{}

func (errTestSaverR100) Error() string { return "read-only file system: config/config.json" }
