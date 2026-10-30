//go:build llama_stub

// heartbeat_buildinfo_r311_test.go — R311 (2026-10-30): версия сборки llama.cpp
// едет в heartbeat, а не только в регистрацию.
//
// ЖИВОЙ ФАКТ. Агент регистрируется ОДИН раз при своём старте. Балансер
// перезапускается независимо и берёт запись из state.json — там остаётся прежнее
// buildInfo. На стенде это выглядело так: после рестарта балансера у главного
// узла снова buildInfo="0eadefebd" (без даты) и relation в панели снова unknown,
// хотя образ и env уже несли дату; восстанавливалось только перезапуском
// ВОРКЕРА. Тот же класс уже лечили для заявки о вместимости (R121) — там
// heartbeat стал носителем факта.
package api

import (
	"net/http"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func TestR311_HeartbeatUpdatesBuildInfo(t *testing.T) {
	t.Setenv("LB_CAPACITY_FROM_MODEL_SLOTS", "false")
	s := newAgentCapacityServer(t)
	const id = "r311-buildinfo"

	// Регистрация создаёт запись, затем приводим buildInfo к «значению из
	// state.json» — то, что видит оператор сразу после рестарта балансера.
	rec := doRegisterAgent(t, s, registerAgentPayload(id, string(types.BackendTypeLlamaCpp), 1))
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("регистрация вернула %d: %s", rec.Code, rec.Body.String())
	}
	if err := s.proxy.MutateBackend(id, func(b *types.Backend) {
		b.BuildInfo = "0eadefebd"
	}); err != nil {
		t.Fatalf("MutateBackend: %v", err)
	}

	// Агент в каждом heartbeat присылает версию с датой — запись обязана
	// самовосстановиться без его перезапуска.
	doBackendHeartbeat(t, s, id, id, heartbeatPayload(map[string]interface{}{
		"nodeMaxConcurrentRequests": 1,
		"buildInfo":                 "0eadefebd 2026-09-01",
	}))

	got := s.proxy.GetBackend(id)
	if got == nil {
		t.Fatal("бэкенд пропал после heartbeat")
	}
	if got.BuildInfo != "0eadefebd 2026-09-01" {
		t.Fatalf("BuildInfo = %q, want \"0eadefebd 2026-09-01\": после рестарта балансера "+
			"версия осталась прежней (relation снова unknown до перезапуска воркера)", got.BuildInfo)
	}
}

func TestR311_EmptyHeartbeatBuildInfoKeepsKnown(t *testing.T) {
	t.Setenv("LB_CAPACITY_FROM_MODEL_SLOTS", "false")
	s := newAgentCapacityServer(t)
	const id = "r311-buildinfo-keep"

	rec := doRegisterAgent(t, s, registerAgentPayload(id, string(types.BackendTypeLlamaCpp), 1))
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("регистрация вернула %d: %s", rec.Code, rec.Body.String())
	}
	if err := s.proxy.MutateBackend(id, func(b *types.Backend) {
		b.BuildInfo = "0eadefebd 2026-09-01"
	}); err != nil {
		t.Fatalf("MutateBackend: %v", err)
	}

	// Старый агент поля не шлёт вовсе: известная версия не должна обнуляться.
	doBackendHeartbeat(t, s, id, id, heartbeatPayload(map[string]interface{}{
		"nodeMaxConcurrentRequests": 1,
	}))

	got := s.proxy.GetBackend(id)
	if got == nil {
		t.Fatal("бэкенд пропал после heartbeat")
	}
	if got.BuildInfo != "0eadefebd 2026-09-01" {
		t.Fatalf("BuildInfo = %q, want прежнее значение: пустое поле не должно стирать версию", got.BuildInfo)
	}
}
