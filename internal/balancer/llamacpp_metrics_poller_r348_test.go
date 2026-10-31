// llamacpp_metrics_poller_r348_test.go — R348 (2026-10-31).
//
// ЗАЧЕМ. Двухузловой стенд (docs/plans/2026-10-31-two-node-donor-check.md) показал:
// после того как снимок узлов раскладки протухает (rpcDeviceTTL = 30 с), cppworker
// честно обнуляет `vram_remote_*` (R132: протухшее не выдаём за текущую ёмкость), и
// балансер получал 0/0 — «0 МБ узлов» неотличимо от «7 ГБ донора, измеренные
// 10 минут назад».
//
// R348 отдаёт на воркере ОТДЕЛЬНЫЕ поля истории (vram_remote_last_known_*), а поллер
// обязан их декодировать и передать в метрики. Тест кормит поллер реальным
// snake_case ответом воркера и проверяет, что история доехала до кэша:
// если тег разъедется со snake_case (класс дефектов R45/F-15), история молча станет
// нулевой и панель снова начнёт врать «узлов нет».
package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/logger"
)

// r348CppModelsWithRemoteHistory — ответ воркера с протухшим текущим вкладом узлов
// и сохранённой историей последнего замера.
const r348CppModelsWithRemoteHistory = `{
  "available_ram_mb": 20480,
  "available_vram_mb": 7073,
  "count": 0,
  "gpu_count": 1,
  "max_vram_n_ctx": 0,
  "model_max_context": 0,
  "models": [],
  "rpc_enabled": true,
  "rpc_servers_active": ["100.64.0.7:50052"],
  "rpc_remote_free_mb": 0,
  "vram_local_free_mb": 7073,
  "vram_layout_free_mb": 7073,
  "vram_layout_total_mb": 8191,
  "vram_remote_free_mb": 0,
  "vram_remote_nodes": 0,
  "vram_remote_source": "none",
  "vram_remote_last_known_age_sec": 732,
  "vram_remote_last_known_free_mb": 7019,
  "vram_remote_last_known_nodes": 1,
  "vram_remote_last_known_total_mb": 8123,
  "total_ram_mb": 24576,
  "total_vram_mb": 8191
}`

// TestR348_Poller_CarriesRemoteHistory — история последнего замера доезжает до
// метрик балансера (иначе панель показывает «узлов нет» вместо «измерено 12 мин назад»).
func TestR348_Poller_CarriesRemoteHistory(t *testing.T) {
	logger.Init("error")
	fake := &r45FakeCppWorker{
		modelsJSON:   r348CppModelsWithRemoteHistory,
		progressJSON: `{"count":0,"models":[]}`,
	}
	proxy, _ := r45BuildProxyWithFakeCppWorker(t, fake)

	poller := newLlamaCppMetricsPoller(proxy)
	poller.interval = 1
	poller.fastInterval = 1
	poller.pollAll()

	lm, ok := r45LlamaMetrics(proxy, "fake-1")
	if !ok || lm == nil {
		t.Fatalf("expected metrics for fake-1, got ok=%v lm=%v", ok, lm)
	}

	// Текущий вклад остаётся нулевым: R132 — протухшее не выдаём за текущее.
	if lm.VramRemoteFreeMB != 0 || lm.VramRemoteNodes != 0 {
		t.Errorf("текущий вклад узлов = %d МБ / %d узлов, want 0/0: "+
			"протухший снимок не должен попадать в текущую ёмкость",
			lm.VramRemoteFreeMB, lm.VramRemoteNodes)
	}
	if lm.VramRemoteSource != "none" {
		t.Errorf("источник вклада узлов %q, want none", lm.VramRemoteSource)
	}

	// История — отдельные поля и отдельные значения.
	if lm.VramRemoteLastKnownFreeMB != 7019 {
		t.Errorf("VramRemoteLastKnownFreeMB = %d, want 7019 (тег vram_remote_last_known_free_mb "+
			"разъехался со snake_case — ровно класс дефектов R45/F-15)",
			lm.VramRemoteLastKnownFreeMB)
	}
	if lm.VramRemoteLastKnownTotalMB != 8123 {
		t.Errorf("VramRemoteLastKnownTotalMB = %d, want 8123", lm.VramRemoteLastKnownTotalMB)
	}
	if lm.VramRemoteLastKnownNodes != 1 {
		t.Errorf("VramRemoteLastKnownNodes = %d, want 1", lm.VramRemoteLastKnownNodes)
	}
	if lm.VramRemoteLastKnownAgeSec != 732 {
		t.Errorf("VramRemoteLastKnownAgeSec = %d, want 732 (возраст обязателен: "+
			"без него старые данные принимаются за свежие)", lm.VramRemoteLastKnownAgeSec)
	}

	// И режим раскладки виден: без него история не показывается.
	if !lm.RPCEnabled {
		t.Error("RPCEnabled = false, want true (rpc_enabled в ответе воркера)")
	}
}
