package balancer

// R71 (2026-09-24): единое правило вместимости бэкенда.
//
// Живой дефект: балансер отдавал агенту в ответе на heartbeat
// runtimeMaxConcurrentRequests, агент применял значение локально и возвращал
// его же в следующем heartbeat — балансер писал «эхо» как вместимость. Узел с
// n_parallel=1 (cppworker шлёт maxConcurrentRequests=1 при саморегистрации)
// жил с порогом admission-очереди 4, а операторский PUT /limits откатывался
// за ~3 с (heartbeat раз в 3 с).
//
// Тесты:
//  1. EffectiveMaxConcurrentRequests: приоритет ноды над «эхом», оператора —
//     над нодой;
//  2. саморегистрация (метка auto-registered) помечает вместимость как
//     «от ноды» уже на AddBackend;
//  3. LoadState восстанавливает runtime-лимиты и признак «от ноды» (до R71 они
//     терялись при перезапуске балансера).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func TestEffectiveMaxConcurrentRequests_R71(t *testing.T) {
	cases := []struct {
		name    string
		backend types.Backend
		want    int
	}{
		{
			name:    "нода владеет вместимостью — n_parallel важнее эха",
			backend: types.Backend{MaxConcurrentReqs: 1, RuntimeMaxConcurrentRequests: 4, RuntimeCapacityFromNode: true},
			want:    1,
		},
		{
			name:    "без признака ноды runtime-лимит приоритетен",
			backend: types.Backend{MaxConcurrentReqs: 1, RuntimeMaxConcurrentRequests: 4},
			want:    4,
		},
		{
			name:    "нет runtime-лимита — статический max",
			backend: types.Backend{MaxConcurrentReqs: 6},
			want:    6,
		},
		{
			name:    "ничего не задано — 0 (лимит не задан)",
			backend: types.Backend{},
			want:    0,
		},
		{
			name:    "нода без валидного n_parallel не ломает fallback",
			backend: types.Backend{MaxConcurrentReqs: -1, RuntimeMaxConcurrentRequests: 3, RuntimeCapacityFromNode: true},
			want:    3,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.backend.EffectiveMaxConcurrentRequests(); got != tc.want {
				t.Errorf("EffectiveMaxConcurrentRequests() = %d, ожидалось %d", got, tc.want)
			}
		})
	}
}

func TestAddBackendR71_MarksCapacityFromNode(t *testing.T) {
	proxy := newProxyWithCleanup(t, createTestConfig())

	selfRegistered := types.Backend{
		ID:                "cppworker-self",
		Host:              "cppworker-gpu",
		CppWorkerPort:     18092,
		Type:              types.BackendTypeLlamaCpp,
		MaxConcurrentReqs: 1,
		Labels:            []string{"cppworker", "auto-registered"},
	}
	if err := proxy.AddBackend(selfRegistered); err != nil {
		t.Fatalf("AddBackend: %v", err)
	}

	got := proxy.GetBackend("cppworker-self")
	if !got.RuntimeCapacityFromNode {
		t.Error("RuntimeCapacityFromNode = false, ожидалось true для auto-registered бэкенда")
	}
	if got.RuntimeMaxConcurrentRequests != 1 {
		t.Errorf("RuntimeMaxConcurrentRequests = %d, ожидалось 1 (n_parallel ноды)", got.RuntimeMaxConcurrentRequests)
	}
	if eff := got.EffectiveMaxConcurrentRequests(); eff != 1 {
		t.Errorf("EffectiveMaxConcurrentRequests = %d, ожидалось 1", eff)
	}

	// Бэкенд, созданный оператором (без метки), не помечается.
	manual := types.Backend{
		ID:                "manual-cpp",
		Host:              "cppworker-manual",
		CppWorkerPort:     18093,
		Type:              types.BackendTypeLlamaCpp,
		MaxConcurrentReqs: 5,
		Labels:            []string{"manual"},
	}
	if err := proxy.AddBackend(manual); err != nil {
		t.Fatalf("AddBackend manual: %v", err)
	}
	if got := proxy.GetBackend("manual-cpp"); got.RuntimeCapacityFromNode {
		t.Error("RuntimeCapacityFromNode = true для бэкенда без метки auto-registered")
	}
}

func TestUpdateBackendLimitsR71_ClearsNodeOwnership(t *testing.T) {
	proxy := newProxyWithCleanup(t, createTestConfig())

	selfRegistered := types.Backend{
		ID:                "cppworker-self",
		Host:              "cppworker-gpu",
		CppWorkerPort:     18092,
		Type:              types.BackendTypeLlamaCpp,
		MaxConcurrentReqs: 1,
		Labels:            []string{"auto-registered"},
	}
	if err := proxy.AddBackend(selfRegistered); err != nil {
		t.Fatalf("AddBackend: %v", err)
	}

	// Оператор ставит лимит 3 (больше n_parallel) — лимит должен победить.
	if err := proxy.UpdateBackendLimits("cppworker-self", 2, 3); err != nil {
		t.Fatalf("UpdateBackendLimits: %v", err)
	}

	got := proxy.GetBackend("cppworker-self")
	if got.RuntimeCapacityFromNode {
		t.Error("RuntimeCapacityFromNode = true после операторского лимита — лимит не сработает")
	}
	if eff := got.EffectiveMaxConcurrentRequests(); eff != 3 {
		t.Errorf("EffectiveMaxConcurrentRequests = %d, ожидалось 3 (лимит оператора)", eff)
	}
}

// stateWithBackend — записать в state.json один бэкенд.
func stateWithBackend(t *testing.T, statePath string, backend types.Backend) {
	t.Helper()
	state := types.StateFile{
		Version:  types.StateVersion,
		Backends: []types.Backend{backend},
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	if err := os.WriteFile(statePath, data, 0o644); err != nil {
		t.Fatalf("write state: %v", err)
	}
}

func loadStateProxy(t *testing.T, statePath string, backends []types.Backend) *Proxy {
	t.Helper()
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 8080, APIPort: 8081, StatePath: statePath},
		Backends:     backends,
	}
	p := newProxyWithCleanup(t, cfg)
	if err := p.LoadState(); err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	return p
}

// R71: операторский runtime-лимит переживает перезапуск балансера.
func TestLoadStateR71_RestoresRuntimeLimits(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	stateWithBackend(t, statePath, types.Backend{
		ID:                           "b1",
		Host:                         "localhost",
		CppWorkerPort:                18092,
		Type:                         types.BackendTypeLlamaCpp,
		MaxConcurrentReqs:            8,
		RuntimeMaxModels:             2,
		RuntimeMaxConcurrentRequests: 5, // троттлинг ниже вместимости ноды
	})

	p := loadStateProxy(t, statePath, []types.Backend{{
		ID:                "b1",
		Host:              "localhost",
		CppWorkerPort:     18092,
		Type:              types.BackendTypeLlamaCpp,
		MaxConcurrentReqs: 8,
	}})

	got := p.GetBackend("b1")
	if got == nil {
		t.Fatal("бэкенд b1 не найден после LoadState")
	}
	if got.RuntimeMaxConcurrentRequests != 5 {
		t.Errorf("RuntimeMaxConcurrentRequests = %d, ожидалось 5 (восстановление из state.json)",
			got.RuntimeMaxConcurrentRequests)
	}
	if got.RuntimeMaxModels != 2 {
		t.Errorf("RuntimeMaxModels = %d, ожидалось 2", got.RuntimeMaxModels)
	}
}

// R71: признак «вместимость от ноды» переживает перезапуск, иначе heartbeat
// агента снова перекрывает n_parallel.
func TestLoadStateR71_RestoresNodeCapacityOwnership(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	stateWithBackend(t, statePath, types.Backend{
		ID:                           "b1",
		Host:                         "localhost",
		CppWorkerPort:                18092,
		Type:                         types.BackendTypeLlamaCpp,
		MaxConcurrentReqs:            1,
		Labels:                       []string{"auto-registered"},
		RuntimeMaxConcurrentRequests: 1,
		RuntimeCapacityFromNode:      true,
	})

	p := loadStateProxy(t, statePath, []types.Backend{{
		ID:                "b1",
		Host:              "localhost",
		CppWorkerPort:     18092,
		Type:              types.BackendTypeLlamaCpp,
		MaxConcurrentReqs: 1,
	}})

	got := p.GetBackend("b1")
	if !got.RuntimeCapacityFromNode {
		t.Error("RuntimeCapacityFromNode = false после LoadState — признак не сериализуется/не восстанавливается")
	}
	if eff := got.EffectiveMaxConcurrentRequests(); eff != 1 {
		t.Errorf("EffectiveMaxConcurrentRequests = %d, ожидалось 1", eff)
	}
}

// R71: самоисцеление state.json, записанного до R71 (признака «от ноды» в
// файле нет): у cppworker с n_parallel=1 стояло «эхо» агента 4 — вместимость
// приводится к вместимости ноды, иначе после обновления балансера очередь
// снова пускала бы 4 запроса на узел, обслуживающий 1.
func TestLoadStateR71_SelfHealsLegacyState(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	stateWithBackend(t, statePath, types.Backend{
		ID:                           "b1",
		Host:                         "localhost",
		CppWorkerPort:                18092,
		Type:                         types.BackendTypeLlamaCpp,
		MaxConcurrentReqs:            1,
		Labels:                       []string{"linux", "llamacpp", "gpu", "sm_86"},
		RuntimeMaxConcurrentRequests: 4, // устаревшее «эхо» агента
	})

	p := loadStateProxy(t, statePath, []types.Backend{{
		ID:                "b1",
		Host:              "localhost",
		CppWorkerPort:     18092,
		Type:              types.BackendTypeLlamaCpp,
		MaxConcurrentReqs: 1,
	}})

	got := p.GetBackend("b1")
	if !got.RuntimeCapacityFromNode {
		t.Error("RuntimeCapacityFromNode = false — самоисцеление вместимости не сработало")
	}
	if got.RuntimeMaxConcurrentRequests != 1 {
		t.Errorf("RuntimeMaxConcurrentRequests = %d, ожидалось 1 (реальный n_parallel вместо эха 4)",
			got.RuntimeMaxConcurrentRequests)
	}
	if eff := got.EffectiveMaxConcurrentRequests(); eff != 1 {
		t.Errorf("EffectiveMaxConcurrentRequests = %d, ожидалось 1", eff)
	}
}

// R71: лимит оператора НИЖЕ вместимости ноды сохраняется (троттлинг) и не
// подменяется n_parallel.
func TestLoadStateR71_KeepsOperatorThrottle(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	stateWithBackend(t, statePath, types.Backend{
		ID:                           "b1",
		Host:                         "localhost",
		CppWorkerPort:                18092,
		Type:                         types.BackendTypeLlamaCpp,
		MaxConcurrentReqs:            8,
		RuntimeMaxConcurrentRequests: 2, // операторский троттлинг
	})

	p := loadStateProxy(t, statePath, []types.Backend{{
		ID:                "b1",
		Host:              "localhost",
		CppWorkerPort:     18092,
		Type:              types.BackendTypeLlamaCpp,
		MaxConcurrentReqs: 8,
	}})

	got := p.GetBackend("b1")
	if got.RuntimeCapacityFromNode {
		t.Error("RuntimeCapacityFromNode = true — признак ноды не должен появляться при троттлинге оператора")
	}
	if eff := got.EffectiveMaxConcurrentRequests(); eff != 2 {
		t.Errorf("EffectiveMaxConcurrentRequests = %d, ожидалось 2 (лимит оператора)", eff)
	}
}
