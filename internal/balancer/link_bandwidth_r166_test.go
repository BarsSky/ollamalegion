//go:build llama_stub

// link_bandwidth_r166_test.go — R166 (2026-10-17): откуда берётся пропускная
// способность канала в оценке трафика раскладки.
//
// ЖИВОЙ КОНТЕКСТ. Число бралось только из тела запроса, иначе — 112 МБ/с
// (1 Гбит/с). На стенде с каналом 10 Гбит/с оценка трафика была в 10 раз
// пессимистичнее, и рабочая композиция могла быть отклонена «из-за сети».
// Теперь работает лестница: запрос → автозамер балансера → LB_RPC_LINK_MBPS →
// дефолт, и в ответе ВСЕГДА видно, какой источник сработал.
//
// КОНТРФАКТУАЛЬНОСТЬ: на старом коде avto-замер и env вообще не участвовали —
// первые три теста падали бы, возвращая дефолт 112 МБ/с.
package balancer

import (
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// bwProxy — прокси с двумя участниками раскладки (main + donor) и без замеров.
func bwProxy() *Proxy {
	p := &Proxy{
		config:     &types.LoadBalancerConfig{},
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
	}
	p.backends["main-1"] = &BackendState{Backend: &types.Backend{
		ID: "main-1", Host: "10.0.0.1", CppWorkerPort: 18092,
		Type: types.BackendTypeLlamaCpp, Role: types.BackendRoleMain,
	}}
	p.backends["donor-1"] = &BackendState{Backend: &types.Backend{
		ID: "donor-1", Host: "10.0.0.2", CppWorkerPort: 18092,
		Type: types.BackendTypeLlamaCpp, Role: types.BackendRoleDonor,
	}}
	return p
}

// seedLink — записать «свежий» авто-замер.
func seedLink(p *Proxy, backendID string, mbps int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rpcLinkMeasured == nil {
		p.rpcLinkMeasured = map[string]rpcLinkMeasurement{}
	}
	p.rpcLinkMeasured[backendID] = rpcLinkMeasurement{
		At: time.Now().UTC(), Node: backendID, Mbps: mbps,
	}
}

// TestR166DefaultWhenNothingMeasured — ни замера, ни env: честный дефолт 1 Гбит/с
// с пометкой source=default (а не «измерено»).
func TestR166DefaultWhenNothingMeasured(t *testing.T) {
	t.Setenv("LB_RPC_LINK_MBPS", "")
	p := bwProxy()
	info := p.LinkBandwidthInfo(0)
	if info.Source != LinkSourceDefault {
		t.Fatalf("source = %q, want %q", info.Source, LinkSourceDefault)
	}
	if info.MBps != defaultBandwidthMBpsR95 {
		t.Errorf("МБ/с = %v, want %v", info.MBps, defaultBandwidthMBpsR95)
	}
}

// TestR166AutoMeasuredBeatsEnv — автозамер балансера главнее операторского .env,
// потому что он меряет ТОТ ЖЕ путь (cppworker ↔ cppworker), которым идут слои.
func TestR166AutoMeasuredBeatsEnv(t *testing.T) {
	t.Setenv("LB_RPC_LINK_MBPS", "2000") // 2 Гбит/с от оператора
	p := bwProxy()
	seedLink(p, "donor-1", 8000) // 8 Гбит/с автозамер

	info := p.LinkBandwidthInfo(0)
	if info.Source != LinkSourceAuto {
		t.Fatalf("source = %q, want %q (автозамер главнее env)", info.Source, LinkSourceAuto)
	}
	if info.Mbps != 8000 {
		t.Errorf("Мбит/с = %d, want 8000", info.Mbps)
	}
	if info.MBps != 1000 {
		t.Errorf("МБ/с = %v, want 1000 (8000 Мбит/с)", info.MBps)
	}
	if info.BackendID != "donor-1" {
		t.Errorf("BackendID = %q, want donor-1", info.BackendID)
	}
}

// TestR166EnvWhenNoMeasurement — замеров нет, но есть LB_RPC_LINK_MBPS: берём его.
func TestR166EnvWhenNoMeasurement(t *testing.T) {
	t.Setenv("LB_RPC_LINK_MBPS", "2000")
	p := bwProxy()
	info := p.LinkBandwidthInfo(0)
	if info.Source != LinkSourceEnv {
		t.Fatalf("source = %q, want %q", info.Source, LinkSourceEnv)
	}
	if info.MBps != 250 {
		t.Errorf("МБ/с = %v, want 250 (2000 Мбит/с)", info.MBps)
	}
}

// TestR166RequestWins — явное значение оператора главнее любого замера.
func TestR166RequestWins(t *testing.T) {
	t.Setenv("LB_RPC_LINK_MBPS", "2000")
	p := bwProxy()
	seedLink(p, "donor-1", 8000)
	info := p.LinkBandwidthInfo(500) // 500 МБ/с = 4 Гбит/с
	if info.Source != LinkSourceRequest {
		t.Fatalf("source = %q, want %q", info.Source, LinkSourceRequest)
	}
	if info.MBps != 500 {
		t.Errorf("МБ/с = %v, want 500", info.MBps)
	}
}

// TestR166SlowestParticipantWins — при нескольких узлах берётся САМЫЙ МЕДЛЕННЫЙ:
// раскладка синхронна, общий темп задаёт медленный канал.
func TestR166SlowestParticipantWins(t *testing.T) {
	t.Setenv("LB_RPC_LINK_MBPS", "")
	p := bwProxy()
	seedLink(p, "main-1", 8000)
	seedLink(p, "donor-1", 2000)
	info := p.LinkBandwidthInfo(0)
	if info.Source != LinkSourceAuto || info.Mbps != 2000 {
		t.Fatalf("ожидали самый медленный канал 2000 Мбит/с, получили %d (%s)", info.Mbps, info.Source)
	}
	if info.BackendID != "donor-1" {
		t.Errorf("BackendID = %q, want donor-1 (медленный)", info.BackendID)
	}
}

// TestR166StaleMeasurementIgnored — протухший замер не используется: лучше env
// или честный дефолт, чем устаревшая цифра, выданная за замер.
func TestR166StaleMeasurementIgnored(t *testing.T) {
	t.Setenv("LB_RPC_LINK_MBPS", "2000")
	p := bwProxy()
	p.mu.Lock()
	p.rpcLinkMeasured = map[string]rpcLinkMeasurement{
		"donor-1": {
			At: time.Now().Add(-10 * rpcLinkMeasureDefaultSec * time.Second),
			Node: "donor-1", Mbps: 8000,
		},
	}
	p.mu.Unlock()

	info := p.LinkBandwidthInfo(0)
	if info.Source != LinkSourceEnv {
		t.Fatalf("протухший замер должен уступить env, source = %q", info.Source)
	}
}

// TestR166OnlyParticipantsCount — канал узлов без роли main/donor к раскладке не
// относится: их замеры не должны попадать в оценку.
func TestR166OnlyParticipantsCount(t *testing.T) {
	t.Setenv("LB_RPC_LINK_MBPS", "")
	p := bwProxy()
	p.backends["idle-1"] = &BackendState{Backend: &types.Backend{
		ID: "idle-1", Host: "10.0.0.3", CppWorkerPort: 18092,
		Type: types.BackendTypeLlamaCpp, Role: types.BackendRoleStandalone,
	}}
	seedLink(p, "idle-1", 1000)

	if info := p.LinkBandwidthInfo(0); info.Source != LinkSourceDefault {
		t.Fatalf("замер standalone-узла не должен участвовать, source = %q", info.Source)
	}
}
