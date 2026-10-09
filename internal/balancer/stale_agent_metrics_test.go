package balancer

import (
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// R-MultiHost (2026-10-07). Машину 192.0.2.11 выключили, и в панели осталась
// живая запись её image-воркера: статус healthy, метрики (1.4% VRAM, 52 °C,
// 405 MHz) — числа последнего опроса, выданные за текущие.
//
// Две независимые причины:
//  1. health-check бьёт по URL из Host, а Host записи — «imageworker», то есть имя
//     контейнера ПЕРВОЙ машины: проверка всегда успешна, сколько бы вторая машина
//     ни была выключена (HasAmbiguousHost);
//  2. agent-timeout чекер снимал только флаг HasAgent, а последние присланные
//     метрики оставались в MetricsManager навсегда (ClearBackendMetrics).
//
// Тесты фиксируют и то, и другое.

func backendWithHost(id, host, nodeAddr string, hasAgent bool, lastContact time.Time) types.Backend {
	return types.Backend{
		ID:               id,
		Host:             host,
		NodeAddr:         nodeAddr,
		Type:             types.BackendTypeImage,
		ImagePort:        18093,
		HasAgent:         hasAgent,
		LastAgentContact: lastContact,
		Status:           types.StatusHealthy,
	}
}

func newMultiHostProxy(t *testing.T, backends ...types.Backend) *Proxy {
	t.Helper()
	p := newProxyWithCleanup(t, &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 8080, APIPort: 8081},
	})
	for _, b := range backends {
		if err := p.AddBackend(b); err != nil {
			t.Fatalf("AddBackend(%s): %v", b.ID, err)
		}
	}
	return p
}

// TestHasAmbiguousHost_DetectsTwoNodesSameHost — две машины объявили один host.
func TestHasAmbiguousHost_DetectsTwoNodesSameHost(t *testing.T) {
	p := newMultiHostProxy(t,
		backendWithHost("imageworker", "imageworker", "172.23.0.5", true, time.Now()),
		backendWithHost("IMAGEWORKER-34", "imageworker", "172.23.0.1", true, time.Now()),
	)
	if !p.HasAmbiguousHost("IMAGEWORKER-34") {
		t.Fatal("IMAGEWORKER-34 должен считаться неоднозначным: такой же host у другой машины")
	}
	if !p.HasAmbiguousHost("imageworker") {
		t.Fatal("imageworker должен считаться неоднозначным: такой же host у другой машины")
	}
}

// TestHasAmbiguousHost_SameNodeIsNotAmbiguous — один узел, две записи: обычный
// дубль, проверять по URL можно.
func TestHasAmbiguousHost_SameNodeIsNotAmbiguous(t *testing.T) {
	p := newMultiHostProxy(t,
		backendWithHost("cppworker-gpu", "cppworker-gpu", "172.23.0.4", false, time.Time{}),
		backendWithHost("cppworker-gpu-bundled-agent", "cppworker-gpu", "172.23.0.4", true, time.Now()),
	)
	if p.HasAmbiguousHost("cppworker-gpu-bundled-agent") {
		t.Fatal("две записи ОДНОГО узла не должны считаться неоднозначными")
	}
}

// TestHasAmbiguousHost_UnknownNodeAddrIsNotAmbiguous — запись без узла (создана
// до появления NodeAddr или руками из WebUI) не защищается: прежнее поведение.
func TestHasAmbiguousHost_UnknownNodeAddrIsNotAmbiguous(t *testing.T) {
	p := newMultiHostProxy(t,
		backendWithHost("imageworker", "imageworker", "", true, time.Now()),
		backendWithHost("IMAGEWORKER-34", "imageworker", "172.23.0.1", true, time.Now()),
	)
	if p.HasAmbiguousHost("imageworker") {
		t.Fatal("запись без NodeAddr не должна считаться неоднозначной")
	}
}

// TestHasAmbiguousHost_DifferentHosts — обычный случай двух машин с разными
// адресами: неоднозначности нет.
func TestHasAmbiguousHost_DifferentHosts(t *testing.T) {
	p := newMultiHostProxy(t,
		backendWithHost("imageworker", "imageworker", "172.23.0.5", true, time.Now()),
		backendWithHost("IMAGEWORKER-34", "192.0.2.11", "172.23.0.1", true, time.Now()),
	)
	if p.HasAmbiguousHost("IMAGEWORKER-34") {
		t.Fatal("разные host'ы — неоднозначности нет")
	}
}

// TestClearAgentMetrics_RemovesStaleTelemetry — после молчания агента метрики не
// должны отдаваться как текущие.
func TestClearAgentMetrics_RemovesStaleTelemetry(t *testing.T) {
	p := newMultiHostProxy(t,
		backendWithHost("IMAGEWORKER-34", "imageworker", "172.23.0.1", true, time.Now()),
	)

	// Агент прислал метрики (как это делает POST /api/v1/agents/metrics).
	p.UpdateMetrics("IMAGEWORKER-34", &types.BackendMetrics{
		ID: "IMAGEWORKER-34",
		GPU: types.GPUMetrics{
			MemoryTotal:  8192,
			MemoryFree:   7097,
			UsagePercent: 1.4,
			Temperature:  52,
		},
	})
	if m, ok := p.metricsMgr.SnapshotBackendMetrics("IMAGEWORKER-34"); !ok || m.GPU.Temperature != 52 {
		t.Fatalf("метрики агента не сохранились: ok=%v", ok)
	}

	p.ClearAgentMetrics("IMAGEWORKER-34")

	if _, ok := p.metricsMgr.SnapshotBackendMetrics("IMAGEWORKER-34"); ok {
		t.Fatal("после ClearAgentMetrics метрики должны исчезнуть, иначе UI покажет замороженные значения")
	}

	// GetClusterState после очистки не должен отдавать старую температуру.
	for _, b := range p.GetClusterState().Backends {
		if b.ID == "IMAGEWORKER-34" && b.GPU.Temperature != 0 {
			t.Fatalf("в cluster state осталась температура %d — метрики выключенной машины выдаются за живые", b.GPU.Temperature)
		}
	}
}

// TestClearAgentMetrics_SafeOnUnknownBackend — очистка несуществующего бэкенда
// не паникует (агент мог отвалиться сразу после удаления записи).
func TestClearAgentMetrics_SafeOnUnknownBackend(t *testing.T) {
	p := newMultiHostProxy(t)
	p.ClearAgentMetrics("nope")
	p.ClearAgentMetrics("")
}
