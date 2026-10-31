// state_host_persist_r351_test.go — R351 (2026-10-31).
//
// ЖИВОЙ ДЕФЕКТ (двухузловой стенд, docs/plans/2026-10-31-two-node-donor-check.md).
// Оператор исправил адрес донора через PUT /api/v1/backends/{id}
// (192.168.13.34 → 100.64.0.7). В памяти запись обновилась и попала в state.json,
// НО после пересоздания контейнера балансер поднял СТАРЫЙ адрес: config.json в
// bundled-развёртывании read-only и содержит прежний host, а LoadState переносил
// поля по списку — host и портов в этом списке не было.
//
// Последствия были не косметические: health давал провал (ollama_unavailable),
// раскладка уходила в пустоту («узлы раскладки недоступны 192.168.13.34:50052»),
// и загрузка молча шла локально при rpc_enabled=false — то есть «донор доступен, а
// раскладка не поднимается».
//
// Проверяется:
//  1. host/порты/имя из state.json переживают LoadState (config со старыми — нет);
//  2. пустой host и нулевые порты в state.json НЕ затирают config (запись могла
//     быть создана старой сборкой без этих полей);
//  3. перенос не ломает соседние поля (status/runtime-лимиты по-прежнему из state).
package balancer

import (
	"path/filepath"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR351_LoadStateRestoresOperatorAddress — главный сценарий: адрес оператора
// побеждает config.json, иначе правка через WebUI/API живёт до первого рестарта.
func TestR351_LoadStateRestoresOperatorAddress(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	stateWithBackend(t, statePath, types.Backend{
		ID:            "donor-1",
		Name:          "donor (renamed)",
		Host:          "100.64.0.7",
		OllamaPort:    11434,
		CppWorkerPort: 18092,
		AgentPort:     18034,
		Type:          types.BackendTypeLlamaCpp,
		Role:          types.BackendRoleDonor,
	})

	// config.json (источник истины при старте) — со СТАРЫМ адресом.
	p := loadStateProxy(t, statePath, []types.Backend{{
		ID:            "donor-1",
		Name:          "donor",
		Host:          "192.168.13.34",
		OllamaPort:    11434,
		CppWorkerPort: 18092,
		AgentPort:     18034,
		Type:          types.BackendTypeLlamaCpp,
		Role:          types.BackendRoleDonor,
	}})

	got := p.GetBackend("donor-1")
	if got == nil {
		t.Fatal("бэкенд donor-1 не найден после LoadState")
	}
	if got.Host != "100.64.0.7" {
		t.Fatalf("host после LoadState = %q, want 100.64.0.7 — иначе правка адреса "+
			"(PUT /api/v1/backends/{id}) не переживает рестарт: балансер снова зовёт мёртвый "+
			"узел, раскладка уходит в пустоту, загрузка идёт локально", got.Host)
	}
	if got.Name != "donor (renamed)" {
		t.Errorf("name = %q, want «donor (renamed)» (имя тоже операторское)", got.Name)
	}
	// Соседние поля не должны пострадать от переноса.
	if got.Role != types.BackendRoleDonor {
		t.Errorf("role = %q, want donor (перенос адреса не должен менять роль)", got.Role)
	}
	if got.CppWorkerPort != 18092 || got.AgentPort != 18034 {
		t.Errorf("порты после LoadState = %d/%d, want 18092/18034", got.CppWorkerPort, got.AgentPort)
	}
}

// TestR351_LoadStateKeepsConfigWhenStateFieldsEmpty — старая запись без адреса не
// должна обнулять рабочий конфиг.
func TestR351_LoadStateKeepsConfigWhenStateFieldsEmpty(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	stateWithBackend(t, statePath, types.Backend{
		ID:   "b1",
		Type: types.BackendTypeLlamaCpp,
		// Host, Name и порты пустые — запись создана сборкой без этих полей.
	})

	p := loadStateProxy(t, statePath, []types.Backend{{
		ID:            "b1",
		Name:          "worker",
		Host:          "10.0.0.5",
		CppWorkerPort: 18092,
		AgentPort:     18034,
		Type:          types.BackendTypeLlamaCpp,
	}})

	got := p.GetBackend("b1")
	if got == nil {
		t.Fatal("бэкенд b1 не найден после LoadState")
	}
	if got.Host != "10.0.0.5" {
		t.Errorf("host = %q, want 10.0.0.5: пустой host в state.json не должен затирать config", got.Host)
	}
	if got.CppWorkerPort != 18092 || got.AgentPort != 18034 {
		t.Errorf("порты = %d/%d, want 18092/18034: нули в state.json не должны затирать config",
			got.CppWorkerPort, got.AgentPort)
	}
	if got.Name != "worker" {
		t.Errorf("name = %q, want worker", got.Name)
	}
}

// TestR351_LoadStateStillRestoresRuntimeAndStatus — перенос адреса не должен
// сломать прежние переносы (status, runtime-лимиты из R71/R121).
func TestR351_LoadStateStillRestoresRuntimeAndStatus(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	stateWithBackend(t, statePath, types.Backend{
		ID:                           "b2",
		Host:                         "100.64.0.7",
		CppWorkerPort:                18092,
		Type:                         types.BackendTypeLlamaCpp,
		Status:                       types.StatusOllamaUnavailable,
		MaxConcurrentReqs:            2,
		RuntimeMaxConcurrentRequests: 1, // ниже вместимости — операторский троттлинг (R71)
		MaxModels:                    3,
		RuntimeMaxModels:             2,
	})

	p := loadStateProxy(t, statePath, []types.Backend{{
		ID:                "b2",
		Host:              "192.168.13.34",
		CppWorkerPort:     18092,
		Type:              types.BackendTypeLlamaCpp,
		Status:            types.StatusHealthy,
		MaxConcurrentReqs: 2,
		MaxModels:         3,
	}})

	got := p.GetBackend("b2")
	if got == nil {
		t.Fatal("бэкенд b2 не найден после LoadState")
	}
	if got.RuntimeMaxConcurrentRequests != 1 || got.RuntimeMaxModels != 2 {
		t.Errorf("runtime-лимиты = %d/%d, want 1/2 (R71/R121 не должны сломаться)",
			got.RuntimeMaxConcurrentRequests, got.RuntimeMaxModels)
	}
	if got.Host != "100.64.0.7" {
		t.Errorf("host = %q, want 100.64.0.7", got.Host)
	}
}
