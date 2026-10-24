package api

// R102 (S6 плана 2026-10-07): GET /api/v1/placement отдаёт СЫРЫЕ секции конфига.
//
// Зачем это отдельный тест. PUT /api/v1/placement заменяет `placement` и
// `backendGroups` ЦЕЛИКОМ (иначе не выразить удаление правила или группы), а
// редактирующий UI обязан перенести чужие правила и группы как есть. Значит он
// должен их откуда-то прочитать: `decisions` — это РЕЗУЛЬТАТ резолва (по нему
// нельзя восстановить, что оператор написал), и без сырых полей окно объединения
// затирало бы всё, чего не касается.

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func TestPlacementEndpoint_RawConfigForEditing_R102(t *testing.T) {
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{
		{ID: "r102-a", Host: "localhost", CppWorkerPort: 18092, Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy},
		{ID: "r102-b", Host: "localhost", CppWorkerPort: 18092, Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy},
	})
	defer cleanup()

	rule := types.PlacementModelRule{
		Model:    "чужая-модель",
		Strategy: string(types.PlacementSingle),
		Reason:   "правило оператора",
	}
	group := types.BackendGroupConfig{
		ID: "group-qwen3-14b", Model: "qwen3-14b", Strategy: "layer",
		Members: []types.BackendGroupMemberConfig{
			{BackendID: "r102-a", Role: "master"},
			{BackendID: "r102-b", Role: "worker", RPCEndpoint: "192.0.2.11:50052"},
		},
	}
	s.proxy.SetPlacementSettings(types.PlacementSettings{
		Enabled: true, Fallback: types.PlacementFallbackError, Models: []types.PlacementModelRule{rule},
	})
	s.proxy.SetBackendGroupsR92(types.BalancingSettings{BackendGroups: []types.BackendGroupConfig{group}})

	resp := getPlacementJSON(t, s, "")

	// 1. Сырые правила: их можно прочитать, перенести и записать обратно.
	placement, ok := resp["placement"].(map[string]interface{})
	if !ok {
		t.Fatalf("GET не отдаёт сырую политику: %T (%v)", resp["placement"], resp["placement"])
	}
	rules, _ := placement["models"].([]interface{})
	if len(rules) != 1 {
		t.Fatalf("правил = %d, ожидалось 1: %v", len(rules), placement["models"])
	}
	gotRule, _ := rules[0].(map[string]interface{})
	if gotRule["model"] != "чужая-модель" || gotRule["strategy"] != "single" {
		t.Errorf("правило искажено: %v", gotRule)
	}

	// 2. Сырые группы: id и состав (у состояния `groups` другая задача — health).
	groups, ok := resp["backendGroups"].([]interface{})
	if !ok || len(groups) != 1 {
		t.Fatalf("GET не отдаёт конфиг групп: %T (%v)", resp["backendGroups"], resp["backendGroups"])
	}
	gotGroup, _ := groups[0].(map[string]interface{})
	if gotGroup["id"] != "group-qwen3-14b" || gotGroup["strategy"] != "layer" {
		t.Errorf("группа искажена: %v", gotGroup)
	}
	members, _ := gotGroup["members"].([]interface{})
	if len(members) != 2 {
		t.Errorf("участников = %d, ожидалось 2", len(members))
	}

	// 3. Круг замкнулся: то, что отдал GET, принимает PUT без потерь.
	code, applyResp := putPlacementR100(t, s, map[string]interface{}{
		"placement":     placement,
		"backendGroups": groups,
	})
	if code != 200 {
		t.Fatalf("повторное применение прочитанного конфига: HTTP %d: %v", code, applyResp)
	}
	if applyResp["applied"] != true {
		t.Errorf("applied = %v", applyResp["applied"])
	}
	if live := s.proxy.PlacementSettings(); len(live.Models) != 1 || live.Models[0].Model != "чужая-модель" {
		t.Errorf("правило потерялось при круге GET → PUT: %+v", live.Models)
	}
	if got := s.proxy.RawBackendGroupsR102(); len(got) != 1 || got[0].ID != "group-qwen3-14b" {
		t.Errorf("группа потерялась при круге GET → PUT: %+v", got)
	}
}

func TestPlacementEndpoint_RawConfigEmptyIsArray_R102(t *testing.T) {
	// Пустой конфиг должен отдаваться пустым МАССИВОМ, а не null: иначе UI
	// пришлось бы отдельно проверять тип перед тем, как дописать правило.
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{{
		ID: "r102-c", Host: "localhost", CppWorkerPort: 18092,
		Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
	}})
	defer cleanup()

	resp := getPlacementJSON(t, s, "")
	if _, ok := resp["backendGroups"].([]interface{}); !ok {
		t.Errorf("backendGroups = %T (%v), ожидался пустой массив", resp["backendGroups"], resp["backendGroups"])
	}
	if _, ok := resp["placement"].(map[string]interface{}); !ok {
		t.Errorf("placement = %T, ожидался объект", resp["placement"])
	}
}
