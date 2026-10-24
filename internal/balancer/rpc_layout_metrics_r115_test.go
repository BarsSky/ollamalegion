// rpc_layout_metrics_r115_test.go — R115, пункт «балансер балансирует и на
// бэкенды в режиме раскладки».
//
// ЧТО ПРОВЕРЯЕТСЯ. Итоговая ёмкость (`available_vram_mb`) уже включает узлы
// раскладки — её считает memfit в cppworker. Но по одному числу балансер не может
// отличить «8 ГБ на карте» от «8 ГБ карты + 7 ГБ удалённого узла», а разница
// существенна: во втором случае часть слоёв считается по сети, и узел может
// отвалиться. Тест фиксирует, что состав ёмкости доезжает до метрик балансера
// (и, значит, до UI), а не теряется при разборе /api/models.
package balancer

import (
	"testing"
)

const r115LayoutModelsJSON = `{
  "available_ram_mb": 20480,
  "available_vram_mb": 14114,
  "total_vram_mb": 16314,
  "count": 0,
  "gpu_count": 1,
  "rpc_enabled": true,
  "rpc_servers_active": ["192.0.2.10:50052"],
  "vram_local_free_mb": 7097,
  "vram_local_total_mb": 8191,
  "vram_remote_free_mb": 7017,
  "vram_remote_total_mb": 8123,
  "vram_layout_free_mb": 14114,
  "vram_layout_total_mb": 16314,
  "vram_remote_nodes": 1,
  "vram_known": true,
  "models": []
}`

func TestR115_PollerKeepsLayoutComposition(t *testing.T) {
	fake := &r45FakeCppWorker{modelsJSON: r115LayoutModelsJSON, progressJSON: `{}`}
	proxy, _ := r45BuildProxyWithFakeCppWorker(t, fake)

	proxy.llamaCppMetricsPoller.pollAll()

	lm, ok := r45LlamaMetrics(proxy, "fake-1")
	if !ok || lm == nil {
		t.Fatalf("R115: метрики бэкенда не собраны")
	}
	if lm.AvailableVRAMMB != 14114 {
		t.Errorf("R115: итоговая ёмкость = %d MB, ожидалось 14114 (карта + узел)", lm.AvailableVRAMMB)
	}
	if !lm.RPCEnabled {
		t.Errorf("R115: режим раскладки не доехал до балансера")
	}
	if len(lm.RPCServersActive) != 1 || lm.RPCServersActive[0] != "192.0.2.10:50052" {
		t.Errorf("R115: активные узлы = %v, ожидался один 192.0.2.10:50052", lm.RPCServersActive)
	}
	if lm.VramLocalFreeMB != 7097 {
		t.Errorf("R115: локальная свободная память = %d, ожидалось 7097", lm.VramLocalFreeMB)
	}
	if lm.VramRemoteFreeMB != 7017 {
		t.Errorf("R115: память узлов = %d, ожидалось 7017", lm.VramRemoteFreeMB)
	}
	if lm.VramRemoteNodes != 1 {
		t.Errorf("R115: число узлов = %d, ожидалось 1", lm.VramRemoteNodes)
	}
	// Итог обязан быть суммой состава: иначе UI показал бы «8 ГБ» рядом с итогом 14 ГБ.
	if lm.VramLocalFreeMB+lm.VramRemoteFreeMB != lm.AvailableVRAMMB {
		t.Errorf("R115: состав (%d + %d) не сходится с итогом %d",
			lm.VramLocalFreeMB, lm.VramRemoteFreeMB, lm.AvailableVRAMMB)
	}
}

func TestR115_PollerBackwardCompatNoRPCFields(t *testing.T) {
	// Старый cppworker (без R115) не отдаёт полей раскладки: балансер обязан
	// работать как раньше, а не считать бэкенд «раскладкой без узлов».
	fake := &r45FakeCppWorker{modelsJSON: r45RealisticCppModelsJSON, progressJSON: `{}`}
	proxy, _ := r45BuildProxyWithFakeCppWorker(t, fake)

	proxy.llamaCppMetricsPoller.pollAll()

	lm, ok := r45LlamaMetrics(proxy, "fake-1")
	if !ok || lm == nil {
		t.Fatalf("R115: метрики бэкенда не собраны")
	}
	if lm.RPCEnabled {
		t.Errorf("R115: без полей раскладки режим обязан считаться выключенным")
	}
	if len(lm.RPCServersActive) != 0 {
		t.Errorf("R115: без полей раскладки список узлов пуст, получено %v", lm.RPCServersActive)
	}
	// Прежние поля читаются как раньше.
	if lm.AvailableVRAMMB != 8191 {
		t.Errorf("R115: available_vram_mb = %d, ожидалось 8191 (совместимость)", lm.AvailableVRAMMB)
	}
}
