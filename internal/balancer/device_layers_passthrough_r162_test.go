//go:build llama_stub

// device_layers_passthrough_r162_test.go — R162 (2026-10-17): точная раскладка
// слоёв доезжает от воркера до WebUI.
//
// ЖИВОЙ ДЕФЕКТ. cppworker НЕ отдаёт `gpu_layers` в /api/models (поле пустое), а
// отдаёт `device_layers_exact` (`load_tensors: layer N assigned to device X`).
// Балансер раскладку декодировал, но в types.LlamaCppModel НЕ переносил — и
// WebUI, складывая numGpuLayers=0 с nLayers=42, рисовал
//
//	gpu_layers=0/42 — «Модель целиком на CPU — генерация будет очень медленной»
//
// при фактических 42/42 слоях на CUDA0. Ложное предупреждение отправляет
// оператора искать несуществующую проблему.
//
// КОНТРФАКТУАЛЬНОСТЬ: без переноса (старый код) LoadedModels[0].DeviceLayersExact
// пуст — тест падает на первой же проверке.
package balancer

import (
	"testing"
)

// TestR162DeviceLayersReachTheUIModel — раскладка из /api/models видна потребителю.
func TestR162DeviceLayersReachTheUIModel(t *testing.T) {
	fake := &r45FakeCppWorker{
		modelsJSON: `{"count":1,"models":[{
			"name":"gemma-4-E4B-it-Q4_K_M",
			"path":"/app/models/gemma-4-E4B-it-Q4_K_M.gguf",
			"state":"loaded",
			"n_layers":42,
			"context_size":8192,
			"context_per_seq":4096,
			"device_layers_exact":[
				{"device":"CPU","from":0,"to":0,"count":1},
				{"device":"CUDA0","from":1,"to":42,"count":42}
			]
		}],"state":"loaded","current_model":"gemma-4-E4B-it-Q4_K_M"}`,
		progressJSON: `{}`,
	}
	proxy, _ := r45BuildProxyWithFakeCppWorker(t, fake)
	proxy.llamaCppMetricsPoller.pollAll()

	lm, ok := r45LlamaMetrics(proxy, "fake-1")
	if !ok || len(lm.LoadedModels) == 0 {
		t.Fatalf("поллер не собрал модели: ok=%v", ok)
	}
	m := lm.LoadedModels[0]
	if len(m.DeviceLayersExact) != 2 {
		t.Fatalf("DeviceLayersExact не перенесён в модель (len=%d): WebUI покажет «0/42 целиком на CPU»",
			len(m.DeviceLayersExact))
	}
	gpu := 0
	for _, d := range m.DeviceLayersExact {
		if d.Device != "CPU" {
			gpu += d.Count
		}
	}
	if gpu != 42 {
		t.Errorf("слоёв на GPU по раскладке = %d, want 42", gpu)
	}
	if m.NLayers != 42 {
		t.Errorf("nLayers = %d, want 42", m.NLayers)
	}
	// R213 (2026-10-21): поле gpu_layers воркер не отдаёт, поэтому число слоёв
	// на GPU берётся из АВТОРИТЕТНОЙ раскладки (42 на CUDA0), а не остаётся
	// нулём. Раньше тест фиксировал ноль — и /api/v1/backends показывал
	// «0 слоёв на GPU» при фактических 42/42 (жалоба оператора «в настройках
	// одно, в загруженных параметрах другое»).
	if m.NumGPULayers != 42 {
		t.Errorf("NumGPULayers = %d, want 42 (R213: производное от DeviceLayersExact)", m.NumGPULayers)
	}
}

// TestR213GpuLayersExplicitWins — если воркер всё же сообщил gpu_layers, число
// из раскладки НЕ перетирает его (факт воркера главнее производной).
func TestR213GpuLayersExplicitWins(t *testing.T) {
	fake := &r45FakeCppWorker{
		modelsJSON: `{"count":1,"models":[{
			"name":"m","state":"loaded","n_layers":42,"context_size":8192,
			"gpu_layers":17,
			"device_layers_exact":[
				{"device":"CPU","from":0,"to":24,"count":25},
				{"device":"CUDA0","from":25,"to":41,"count":17}
			]
		}]}`,
		progressJSON: `{}`,
	}
	proxy, _ := r45BuildProxyWithFakeCppWorker(t, fake)
	proxy.llamaCppMetricsPoller.pollAll()

	lm, ok := r45LlamaMetrics(proxy, "fake-1")
	if !ok || len(lm.LoadedModels) == 0 {
		t.Fatalf("поллер не собрал модели: ok=%v", ok)
	}
	if got := lm.LoadedModels[0].NumGPULayers; got != 17 {
		t.Errorf("NumGPULayers = %d, want 17 (явное поле воркера)", got)
	}
}

// TestR162NoDeviceLayersStaysEmpty — обратная сторона: без раскладки поле пустое,
// и потребитель обязан отличать «не сообщил» от «все слои на CPU».
func TestR162NoDeviceLayersStaysEmpty(t *testing.T) {
	fake := &r45FakeCppWorker{
		modelsJSON: `{"count":1,"models":[{"name":"m","state":"loaded","n_layers":42}],
			"state":"loaded","current_model":"m"}`,
		progressJSON: `{}`,
	}
	proxy, _ := r45BuildProxyWithFakeCppWorker(t, fake)
	proxy.llamaCppMetricsPoller.pollAll()

	lm, ok := r45LlamaMetrics(proxy, "fake-1")
	if !ok || len(lm.LoadedModels) == 0 {
		t.Fatalf("поллер не собрал модели")
	}
	if len(lm.LoadedModels[0].DeviceLayersExact) != 0 {
		t.Errorf("раскладки нет, а поле непустое: %+v", lm.LoadedModels[0].DeviceLayersExact)
	}
}
