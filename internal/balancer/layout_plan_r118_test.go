// layout_plan_r118_test.go — R118 (2026-10-14): раскладка слоёв по устройствам
// для WebUI и регрессия декодирования tensor_split.
//
// ЖАЛОБА: «не хватает пометок в webui, что осуществлена раскладка на донора и по
// какой модели и по каким слоям». Данные для этого уже есть в cppworker
// (n_layers, gpu_layers, tensor_split), но:
//
//  1. агрегатор балансера декодировал tensor_split в map[string]interface{},
//     а cppworker отдаёт МАССИВ → декодирование всего ответа падало, и runtime-
//     строка в WebUI оставалась пустой именно тогда, когда модель загружена с
//     раскладкой;
//  2. раскладки «какие слои куда» в API не было вовсе.
//
// Правила вывода плана сверены с ДВУМЯ живыми прогонами стенда:
//
//	42 слоя, tensor_split [0.537, 0.463], все слои на GPU →
//	    CUDA0 23 (1–23), RPC0 19 (24–42), CPU 1        ← в логе: CUDA0=23 RPC0=19 CPU=1
//	42 слоя, tensor_split [0.537, 0.463], gpu_layers=10 →
//	    CUDA0 6 (33–38), RPC0 4 (39–42), CPU 33        ← в логе: CUDA0=6 RPC1=4 CPU=33
package balancer

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestR118_RuntimeModelDecodesTensorSplitArray — регрессия: tensor_split приходит
// массивом и обязан декодироваться (иначе теряется весь список моделей).
func TestR118_RuntimeModelDecodesTensorSplitArray(t *testing.T) {
	payload := `{"loaded_models":[{
		"name":"gemma-4-E4B-it-Q4_K_M","state":"loaded","n_layers":42,"gpu_layers":10,
		"gpu_count":1,"tensor_split":[0.53704435,0.46295568],
		"context_size":4096,"kv_cache_type":"q4_0","parallel":2
	}]}`

	var parsed struct {
		LoadedModels []runtimeModelR118 `json:"loaded_models"`
	}
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		t.Fatalf("декодирование ответа cppworker упало: %v — именно так терялась runtime-строка "+
			"в WebUI, когда модель загружена с раскладкой (tensor_split приходит массивом)", err)
	}
	if len(parsed.LoadedModels) != 1 {
		t.Fatalf("моделей = %d, want 1", len(parsed.LoadedModels))
	}
	m := parsed.LoadedModels[0]
	if len(m.TensorSplit) != 2 || m.TensorSplit[0] != 0.53704435 {
		t.Errorf("tensor_split = %v, want [0.53704435 0.46295568]", m.TensorSplit)
	}
	if m.GPULayers != 10 || m.NLayers != 42 {
		t.Errorf("gpu_layers/n_layers = %d/%d, want 10/42", m.GPULayers, m.NLayers)
	}
}

// TestR118_DeriveLayerPlanMatchesLiveRuns — вывод плана совпадает с тем, что
// llama.cpp напечатал в лог на живом стенде.
func TestR118_DeriveLayerPlanMatchesLiveRuns(t *testing.T) {
	donor := []string{"192.0.2.10:50052"}
	split := []float64{0.53704435, 0.46295568}

	cases := []struct {
		name       string
		nLayers    int
		gpuLayers  int
		split      []float64
		localGPUs  int
		rpc        []string
		wantDevs   []deviceLayerRange
		wantCPU    int
		wantGPUTot int
	}{
		{
			name: "живой прогон: все 42 слоя, локально+донор (23/19/1)",
			nLayers: 42, gpuLayers: 42, split: split, localGPUs: 1, rpc: donor,
			wantDevs: []deviceLayerRange{
				{Device: "CUDA0", Kind: "local", Count: 23, From: 1, To: 23},
				{Device: "RPC0", Kind: "rpc", Node: donor[0], Count: 19, From: 24, To: 42},
			},
			wantCPU: 1, wantGPUTot: 42,
		},
		{
			name: "живой прогон: gpu_layers=10, локально+донор (6/4/33)",
			nLayers: 42, gpuLayers: 10, split: split, localGPUs: 1, rpc: donor,
			wantDevs: []deviceLayerRange{
				{Device: "CUDA0", Kind: "local", Count: 6, From: 33, To: 38},
				{Device: "RPC0", Kind: "rpc", Node: donor[0], Count: 4, From: 39, To: 42},
			},
			wantCPU: 33, wantGPUTot: 10,
		},
		{
			name: "только локально: все слои на GPU",
			nLayers: 42, gpuLayers: 42, split: nil, localGPUs: 1, rpc: nil,
			wantDevs: []deviceLayerRange{
				{Device: "CUDA0", Kind: "local", Count: 42, From: 1, To: 42},
			},
			wantCPU: 1, wantGPUTot: 42,
		},
		{
			name: "все слои на CPU",
			nLayers: 42, gpuLayers: 0, split: nil, localGPUs: 1, rpc: nil,
			wantDevs: nil, wantCPU: 42, wantGPUTot: 0,
		},
		{
			name: "-1 = все слои на GPU (конвенция llama.cpp)",
			nLayers: 42, gpuLayers: -1, split: nil, localGPUs: 1, rpc: nil,
			wantDevs: []deviceLayerRange{
				{Device: "CUDA0", Kind: "local", Count: 42, From: 1, To: 42},
			},
			wantCPU: 1, wantGPUTot: 42,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := deriveLayerPlanR118(tc.nLayers, tc.gpuLayers, tc.split, tc.localGPUs, tc.rpc)
			if plan.Total != tc.nLayers {
				t.Errorf("total = %d, want %d", plan.Total, tc.nLayers)
			}
			if plan.GPU != tc.wantGPUTot {
				t.Errorf("gpu = %d, want %d", plan.GPU, tc.wantGPUTot)
			}
			if plan.CPU != tc.wantCPU {
				t.Errorf("cpu = %d, want %d (n_layers - gpu_layers + выходной слой)", plan.CPU, tc.wantCPU)
			}
			if !plan.Estimated || plan.Note == "" {
				t.Error("план обязан помечаться как расчёт (estimated + note), иначе оператор " +
					"примет его за точный список из лога")
			}
			if len(plan.Devices) != len(tc.wantDevs) {
				t.Fatalf("устройств = %d, want %d (%v)", len(plan.Devices), len(tc.wantDevs), plan.Devices)
			}
			for i, want := range tc.wantDevs {
				got := plan.Devices[i]
				if got.Device != want.Device || got.Kind != want.Kind || got.Count != want.Count ||
					got.From != want.From || got.To != want.To || got.Node != want.Node {
					t.Errorf("устройство %d = %+v, want %+v", i, got, want)
				}
			}
		})
	}
}

// TestR118_LayerPlanDistributesAllGPULayers — сумма по устройствам всегда равна
// gpu_layers: иначе UI покажет «слои не сходятся».
func TestR118_LayerPlanDistributesAllGPULayers(t *testing.T) {
	rpc := []string{"10.0.0.2:50052", "10.0.0.3:50052"}
	for _, gpu := range []int{1, 2, 3, 7, 16, 33, 42} {
		plan := deriveLayerPlanR118(42, gpu, []float64{0.5, 0.3, 0.2}, 1, rpc)
		sum := 0
		for _, d := range plan.Devices {
			sum += d.Count
		}
		if sum != gpu {
			t.Errorf("gpu_layers=%d: сумма по устройствам = %d, want %d (%v)", gpu, sum, gpu, plan.Devices)
		}
		if len(plan.Devices) == 0 {
			t.Errorf("gpu_layers=%d: пустой список устройств", gpu)
		}
	}
}

// TestR118_16_LayerPlanFromExact — план из ФАКТА: ровно те слои, которые llama.cpp
// напечатал в лог на живом прогоне стенда (загрузка gemma-4-E4B с раскладкой).
//
// Живой лог (docker logs ol-stack-cppworker-gpu, 43 назначения = n_layer+1):
//
//	layer   0 assigned to device CPU     → CPU   (1 слой, 0–0)
//	layer   1..23 assigned to CUDA0      → CUDA0 (23 слоя, 1–23)
//	layer  24..42 assigned to RPC0       → RPC0  (19 слоёв, 24–42)
//	load_tensors: RPC0[192.0.2.10:50052] model buffer size = 1251.07 MiB
func TestR118_16_LayerPlanFromExact(t *testing.T) {
	live := []exactDeviceRangeR118{
		{Device: "CPU", From: 0, To: 0, Count: 1},
		{Device: "CUDA0", From: 1, To: 23, Count: 23},
		{Device: "RPC0", From: 24, To: 42, Count: 19, Node: "192.0.2.10:50052"},
	}

	plan, ok := layerPlanFromExactR118(live, 42)
	if !ok {
		t.Fatal("точный план не принят, хотя сумма сходится (n_layer+1 = 43)")
	}
	if plan.Estimated {
		t.Error("план из лога обязан иметь estimated=false — иначе оператор считает его расчётом")
	}
	if plan.Note == "" || !strings.Contains(plan.Note, "точный список") {
		t.Errorf("в подсказке нет указания на источник факта: %q", plan.Note)
	}
	if plan.Total != 43 || plan.GPU != 42 || plan.CPU != 1 {
		t.Errorf("total/gpu/cpu = %d/%d/%d, want 43/42/1", plan.Total, plan.GPU, plan.CPU)
	}
	if len(plan.Devices) != 2 {
		t.Fatalf("устройств = %d, want 2 (CPU идёт в cpu_layers, а не в список устройств): %+v",
			len(plan.Devices), plan.Devices)
	}
	if d := plan.Devices[0]; d.Device != "CUDA0" || d.Kind != "local" || d.Node != "" ||
		d.From != 1 || d.To != 23 || d.Count != 23 {
		t.Errorf("CUDA0 = %+v", d)
	}
	if d := plan.Devices[1]; d.Device != "RPC0" || d.Kind != "rpc" ||
		d.Node != "192.0.2.10:50052" || d.From != 24 || d.To != 42 || d.Count != 19 {
		t.Errorf("RPC0 = %+v", d)
	}
	// Сумма по GPU-устройствам и CPU обязана дать Total: это то, что показывает UI.
	if plan.Devices[0].Count+plan.Devices[1].Count+plan.CPU != plan.Total {
		t.Errorf("слои не сходятся: %d+%d+%d != %d", plan.Devices[0].Count, plan.Devices[1].Count,
			plan.CPU, plan.Total)
	}
}

// TestR118_16_LayerPlanFromExactRejectsIncomplete — неполный или чужой факт
// отбрасывается: показать расчёт честнее, чем чужую раскладку.
func TestR118_16_LayerPlanFromExactRejectsIncomplete(t *testing.T) {
	live := []exactDeviceRangeR118{
		{Device: "CPU", From: 0, To: 0, Count: 1},
		{Device: "CUDA0", From: 1, To: 23, Count: 23},
		{Device: "RPC0", From: 24, To: 42, Count: 19},
	}
	cases := []struct {
		name    string
		ranges  []exactDeviceRangeR118
		nLayers int
	}{
		{"пустой список (cppworker не сообщил)", nil, 42},
		{"n_layers неизвестно", live, 0},
		{"не хватает слоя (раскладка другой модели)", live[:2], 42},
		{"лишний слой", append(append([]exactDeviceRangeR118{}, live...),
			exactDeviceRangeR118{Device: "CUDA0", From: 43, To: 43, Count: 1}), 42},
		{"устройство без имени", []exactDeviceRangeR118{{From: 0, To: 42, Count: 43}}, 42},
		{"нулевой диапазон", []exactDeviceRangeR118{{Device: "CUDA0", Count: 0}}, 42},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if plan, ok := layerPlanFromExactR118(c.ranges, c.nLayers); ok {
				t.Errorf("принят неполный факт: %+v", plan)
			}
		})
	}
}

// TestR118_16_RuntimeConfigPrefersExactOverEstimate — агрегатор обязан показать
// ФАКТ, когда он есть: точный список из лога важнее расчёта по tensor_split.
func TestR118_16_RuntimeConfigPrefersExactOverEstimate(t *testing.T) {
	// Ответ cppworker: тот же формат, что отдаёт /config/runtime.
	payload := `{"loaded_models":[{
		"name":"gemma-4-E4B-it-Q4_K_M","state":"loaded","n_layers":42,"gpu_layers":10,
		"gpu_count":1,"tensor_split":[0.53704435,0.46295568],
		"device_layers_exact":[
			{"device":"CPU","from":0,"to":0,"count":1},
			{"device":"CUDA0","from":1,"to":23,"count":23},
			{"device":"RPC0","from":24,"to":42,"count":19,"node":"192.0.2.10:50052"}
		]}]}`
	var parsed struct {
		LoadedModels []runtimeModelR118 `json:"loaded_models"`
	}
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		t.Fatalf("декодирование device_layers_exact упало: %v", err)
	}
	if len(parsed.LoadedModels) != 1 {
		t.Fatalf("моделей = %d, want 1", len(parsed.LoadedModels))
	}
	m := parsed.LoadedModels[0]
	plan, ok := layerPlanFromExactR118(m.ExactDeviceLayers, m.NLayers)
	if !ok {
		t.Fatal("device_layers_exact не декодировался в план — WebUI покажет расчёт вместо факта")
	}
	if plan.Estimated {
		t.Error("план из device_layers_exact помечен как расчёт")
	}
	// Расчёт по тем же числам дал бы ДРУГИЕ диапазоны (gpu_layers=10) — это и есть
	// причина, по которой факт обязан побеждать.
	derived := deriveLayerPlanR118(m.NLayers, m.GPULayers, m.TensorSplit, m.GPUCount, nil)
	if derived.Estimated != true {
		t.Fatal("расчётный план обязан быть помечен estimated=true")
	}
	if derived.CPU == plan.CPU && len(derived.Devices) == len(plan.Devices) &&
		derived.Devices[0].From == plan.Devices[0].From {
		t.Error("живой факт и расчёт совпали — тест перестал различать их (проверьте данные)")
	}
}
