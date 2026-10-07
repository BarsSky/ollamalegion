package api

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// R-MultiHost (2026-10-07), третья итерация.
//
// На живой паре из двух машин `imageworker` (машина 1) и `IMAGEWORKER-34`
// (машина 2) объявили ОДИН host — "imageworker" (имя контейнера; на машине 2
// SDWORKER_ADVERTISE_HOST не доехал до воркера). Ключ де-дупликации (host, port)
// у них совпал, и в списке оставалась только одна запись: карточка «мигала», а
// половина кластера пропадала из WebUI. Различает машины только адрес узла —
// NodeAddr (он же виден в логе guard'а как ownerNode).
//
// Эти тесты фиксируют: два узла с одинаковым host НЕ схлопываются, а настоящие
// дубли одного узла — по-прежнему схлопываются.

func be(id, host, nodeAddr string, port int, hasAgent bool) types.Backend {
	return types.Backend{
		ID:            id,
		Host:          host,
		NodeAddr:      nodeAddr,
		ImagePort:     port,
		Type:          types.BackendTypeImage,
		HasAgent:      hasAgent,
		OllamaPort:    11434,
		CppWorkerPort: 0,
	}
}

func dedupByEndpoint(list []types.Backend) []types.Backend {
	return dedupBackendsByEndpoint(
		list,
		func(b types.Backend) endpointKey {
			return endpointKey{Host: b.Host, Port: backendDedupPort(b), NodeAddr: b.NodeAddr}
		},
		func(b types.Backend) bool { return b.HasAgent },
		true,
	)
}

// TestDedupEndpoint_SameHostDifferentNodesBothVisible — две машины, одинаковый
// host и порт, разные узлы → обе записи должны остаться.
func TestDedupEndpoint_SameHostDifferentNodesBothVisible(t *testing.T) {
	list := []types.Backend{
		be("imageworker", "imageworker", "172.23.0.5", 18093, true),
		be("IMAGEWORKER-34", "imageworker", "172.23.0.1", 18093, true),
	}
	result := dedupByEndpoint(list)
	if len(result) != 2 {
		ids := make([]string, 0, len(result))
		for _, b := range result {
			ids = append(ids, b.ID)
		}
		t.Fatalf("после де-дупа осталось %d записей (%v); две РАЗНЫЕ машины с одинаковым host нельзя схлопывать — одна из них пропадёт из WebUI", len(result), ids)
	}
}

// TestDedupEndpoint_NoNodeAddrStaysSeparate — запись без NodeAddr (созданная до
// этой ревизии или вручную из WebUI) считается отдельным ключом: показать лишний
// дубль на переходный период безопаснее, чем спрятать машину.
func TestDedupEndpoint_NoNodeAddrStaysSeparate(t *testing.T) {
	list := []types.Backend{
		be("imageworker", "imageworker", "", 18093, true),
		be("IMAGEWORKER-34", "imageworker", "172.23.0.1", 18093, true),
	}
	result := dedupByEndpoint(list)
	if len(result) != 2 {
		t.Fatalf("после де-дупа осталось %d записей, ожидалось 2", len(result))
	}
}

// TestDedupEndpoint_RealDuplicateStillCollapsed — настоящий дубль (тот же узел,
// host и порт) по-прежнему схлопывается, и предпочитается запись с агентом.
func TestDedupEndpoint_RealDuplicateStillCollapsed(t *testing.T) {
	list := []types.Backend{
		be("cppworker-gpu", "cppworker-gpu", "172.23.0.4", 18092, false),
		be("cppworker-gpu-bundled-agent", "cppworker-gpu", "172.23.0.4", 18092, true),
	}
	result := dedupByEndpoint(list)
	if len(result) != 1 {
		t.Fatalf("после де-дупа осталось %d записей, ожидалась 1", len(result))
	}
	if !result[0].HasAgent {
		t.Fatalf("оставлена запись без агента (%q) — предпочтение должно быть у записи с агентом", result[0].ID)
	}
}

// TestDedupEndpoint_HostPortWrapperUnchanged — старая функция (host, port) без
// узла сохраняет прежнее поведение: она остаётся для вызовов без NodeAddr.
func TestDedupEndpoint_HostPortWrapperUnchanged(t *testing.T) {
	list := []types.Backend{
		be("a", "imageworker", "172.23.0.5", 18093, true),
		be("b", "imageworker", "172.23.0.1", 18093, true),
	}
	result := dedupBackendsByHostPort(
		list,
		func(b types.Backend) string { return b.Host },
		backendDedupPort,
		func(b types.Backend) bool { return b.HasAgent },
		true,
	)
	if len(result) != 1 {
		t.Fatalf("обёртка (host, port) должна схлопывать такие записи, осталось %d", len(result))
	}
}
