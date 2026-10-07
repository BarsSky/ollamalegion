package balancer

import (
	"path/filepath"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// R-MultiHost (2026-10-07).
//
// NodeAddr — узел, за которым закреплена запись бэкенда. Это единственный
// признак, различающий машины в multi-host развёртывании (compose на обеих
// одинаков, поэтому совпадают host, backendID и порты).
//
// Регрессия, найденная на живой паре: guard работал до первого перезапуска
// балансера, а после него записи снова оказывались «ничьими», и метрики второй
// машины опять подменяли метрики первой (CPU/GPU переключались между машинами
// от замера к замеру). Причина — LoadState переносил из state.json только
// перечисленные поля, и NodeAddr не восстанавливался.
func TestLoadStateRMultiHost_RestoresNodeAddr(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	stateWithBackend(t, statePath, types.Backend{
		ID:                "cppworker-gpu-bundled-agent",
		Host:              "cppworker-gpu",
		CppWorkerPort:     18092,
		Type:              types.BackendTypeLlamaCpp,
		AgentID:           "cppworker-gpu-bundled-agent",
		HasAgent:          true,
		NodeAddr:          "172.23.0.4",
		MaxConcurrentReqs: 2,
	})

	p := loadStateProxy(t, statePath, []types.Backend{{
		ID:            "cppworker-gpu-bundled-agent",
		Host:          "cppworker-gpu",
		CppWorkerPort: 18092,
		Type:          types.BackendTypeLlamaCpp,
	}})

	got := p.GetBackend("cppworker-gpu-bundled-agent")
	if got == nil {
		t.Fatal("бэкенд не найден после LoadState")
	}
	if got.NodeAddr != "172.23.0.4" {
		t.Fatalf("NodeAddr после LoadState = %q, ожидался 172.23.0.4 — иначе защита от захвата записи второй машиной исчезает после перезапуска", got.NodeAddr)
	}
}

// Пустое значение в state.json не должно затирать уже известный узел: запись
// могла быть сохранена сборкой, которая поля ещё не знала.
func TestLoadStateRMultiHost_EmptySavedNodeAddrKeepsKnown(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	stateWithBackend(t, statePath, types.Backend{
		ID:            "b1",
		Host:          "localhost",
		CppWorkerPort: 18092,
		Type:          types.BackendTypeLlamaCpp,
	})

	p := loadStateProxy(t, statePath, []types.Backend{{
		ID:            "b1",
		Host:          "localhost",
		CppWorkerPort: 18092,
		Type:          types.BackendTypeLlamaCpp,
		NodeAddr:      "172.23.0.6",
	}})

	got := p.GetBackend("b1")
	if got == nil {
		t.Fatal("бэкенд b1 не найден после LoadState")
	}
	if got.NodeAddr != "172.23.0.6" {
		t.Fatalf("NodeAddr = %q, ожидался 172.23.0.6 (пустое значение в файле не должно затирать известный узел)", got.NodeAddr)
	}
}

// SaveState должен сохранять узел — иначе он не переживёт перезапуск, даже если
// LoadState его восстанавливает.
func TestSaveStateRMultiHost_PersistsNodeAddr(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	p := newProxyWithCleanup(t, &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 8080, APIPort: 8081, StatePath: statePath},
	})

	if err := p.AddBackend(types.Backend{
		ID:            "b1",
		Host:          "localhost",
		CppWorkerPort: 18092,
		Type:          types.BackendTypeLlamaCpp,
		NodeAddr:      "192.168.13.34",
	}); err != nil {
		t.Fatalf("AddBackend: %v", err)
	}
	if err := p.SaveState(); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	p2 := newProxyWithCleanup(t, &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 8080, APIPort: 8081, StatePath: statePath},
	})
	if err := p2.LoadState(); err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	got := p2.GetBackend("b1")
	if got == nil {
		t.Fatal("бэкенд b1 не найден после перезагрузки состояния")
	}
	if got.NodeAddr != "192.168.13.34" {
		t.Fatalf("NodeAddr после SaveState+LoadState = %q, ожидался 192.168.13.34", got.NodeAddr)
	}
}
