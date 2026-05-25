//go:build !no_cppbackend

package tests

import (
	"testing"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/internal/cppbackend"
)

// ============================================================
// GPUManager tests
// ============================================================

func TestNewGPUManager(t *testing.T) {
	gm := cppbackend.NewGPUManager("vram-ratio")
	if gm == nil {
		t.Fatal("expected non-nil GPUManager")
	}
	if gm.GetDeviceCount() != 0 {
		t.Errorf("expected 0 devices, got %d", gm.GetDeviceCount())
	}
	if gm.GetTotalVRAM() != 0 {
		t.Errorf("expected 0 total VRAM, got %d", gm.GetTotalVRAM())
	}
}

func TestGPUManagerInitGPU(t *testing.T) {
	gm := cppbackend.NewGPUManager("vram-ratio")
	devices := []bridge.GPUDevice{
		{Index: 0, VRAMTotalMB: 24576, VRAMFreeMB: 24576, Name: "NVIDIA RTX 4090"},
		{Index: 1, VRAMTotalMB: 24576, VRAMFreeMB: 24576, Name: "NVIDIA RTX 4090"},
	}
	gm.InitGPU(devices)

	if gm.GetDeviceCount() != 2 {
		t.Errorf("expected 2 devices, got %d", gm.GetDeviceCount())
	}
	if gm.GetTotalVRAM() != 49152 {
		t.Errorf("expected 49152 total VRAM, got %d", gm.GetTotalVRAM())
	}
}

func TestGPUManagerCalculateTensorSplitNoGPU(t *testing.T) {
	gm := cppbackend.NewGPUManager("vram-ratio")
	ts := gm.CalculateTensorSplit(24000)
	if ts.GPUCount != 1 {
		t.Errorf("expected 1 GPU (CPU), got %d", ts.GPUCount)
	}
	if len(ts.Ratios) != 1 || ts.Ratios[0] != 1.0 {
		t.Errorf("expected ratio [1.0], got %v", ts.Ratios)
	}
	if ts.NGPULayers != -1 {
		t.Errorf("expected NGPULayers=-1 for CPU, got %d", ts.NGPULayers)
	}
}

func TestGPUManagerCalculateTensorSplitSingleGPU(t *testing.T) {
	gm := cppbackend.NewGPUManager("vram-ratio")
	devices := []bridge.GPUDevice{
		{Index: 0, VRAMTotalMB: 24576, VRAMFreeMB: 24576, Name: "NVIDIA RTX 4090"},
	}
	gm.InitGPU(devices)
	ts := gm.CalculateTensorSplit(24000)

	if ts.GPUCount != 1 {
		t.Errorf("expected 1 GPU, got %d", ts.GPUCount)
	}
	if len(ts.Ratios) != 1 || ts.Ratios[0] != 1.0 {
		t.Errorf("expected ratio [1.0], got %v", ts.Ratios)
	}
	if ts.MainGPU != 0 {
		t.Errorf("expected MainGPU=0, got %d", ts.MainGPU)
	}
}

func TestGPUManagerCalculateTensorSplitMultiGPU(t *testing.T) {
	gm := cppbackend.NewGPUManager("vram-ratio")
	devices := []bridge.GPUDevice{
		{Index: 0, VRAMTotalMB: 24576, VRAMFreeMB: 24576, Name: "NVIDIA RTX 4090"},
		{Index: 1, VRAMTotalMB: 12288, VRAMFreeMB: 12288, Name: "NVIDIA RTX 4080"},
	}
	gm.InitGPU(devices)
	ts := gm.CalculateTensorSplit(36000)

	if ts.GPUCount != 2 {
		t.Errorf("expected 2 GPUs, got %d", ts.GPUCount)
	}
	if len(ts.Ratios) != 2 {
		t.Errorf("expected 2 ratios, got %v", ts.Ratios)
	}

	// GPU 0 has 24GB, GPU 1 has 12GB → ratio ~ 0.66 / 0.33
	if ts.Ratios[0] < 0.6 || ts.Ratios[0] > 0.7 {
		t.Errorf("expected ratio[0] ~0.66, got %f", ts.Ratios[0])
	}
	if ts.Ratios[1] < 0.3 || ts.Ratios[1] > 0.4 {
		t.Errorf("expected ratio[1] ~0.33, got %f", ts.Ratios[1])
	}
}

func TestGPUManagerSelectGPUForModel(t *testing.T) {
	gm := cppbackend.NewGPUManager("vram-ratio")
	devices := []bridge.GPUDevice{
		{Index: 0, VRAMTotalMB: 8192, VRAMFreeMB: 8192, Name: "GPU Low"},
		{Index: 1, VRAMTotalMB: 24576, VRAMFreeMB: 24576, Name: "GPU High"},
	}
	gm.InitGPU(devices)

	// Модель требует 12GB → GPU 1 (High) должен быть выбран
	gpuIdx := gm.SelectGPUForModel(12000)
	if gpuIdx != 1 {
		t.Errorf("expected GPU 1 (High) for 12GB model, got %d", gpuIdx)
	}
}

func TestGPUManagerSelectGPUForModelSmall(t *testing.T) {
	gm := cppbackend.NewGPUManager("vram-ratio")
	devices := []bridge.GPUDevice{
		{Index: 0, VRAMTotalMB: 8192, VRAMFreeMB: 8192, Name: "GPU Low"},
		{Index: 1, VRAMTotalMB: 24576, VRAMFreeMB: 24576, Name: "GPU High"},
	}
	gm.InitGPU(devices)

	// Модель требует 4GB → любой GPU подходит, выбираем с большим score
	gpuIdx := gm.SelectGPUForModel(4000)
	if gpuIdx != 1 {
		t.Errorf("expected GPU 1 (more free VRAM), got %d", gpuIdx)
	}
}

func TestGPUManagerReserveReleaseMemory(t *testing.T) {
	gm := cppbackend.NewGPUManager("vram-ratio")
	devices := []bridge.GPUDevice{
		{Index: 0, VRAMTotalMB: 24576, VRAMFreeMB: 24576, Name: "NVIDIA RTX 4090"},
	}
	gm.InitGPU(devices)

	// Резервируем 10GB
	gm.ReserveGPUMemory(0, 10240)
	usage := gm.GetGPUUsage()
	if usage[0] != 10240 {
		t.Errorf("expected usage[0]=10240, got %d", usage[0])
	}

	free := gm.GetFreeVRAM()
	if free[0] != 24576-10240 {
		t.Errorf("expected free[0]=14336, got %d", free[0])
	}

	// Освобождаем 4GB
	gm.ReleaseGPUMemory(0, 4096)
	usage = gm.GetGPUUsage()
	if usage[0] != 6144 {
		t.Errorf("expected usage[0]=6144, got %d", usage[0])
	}
}

func TestGPUManagerReleaseUnderflow(t *testing.T) {
	gm := cppbackend.NewGPUManager("vram-ratio")
	devices := []bridge.GPUDevice{
		{Index: 0, VRAMTotalMB: 24576, VRAMFreeMB: 24576, Name: "NVIDIA RTX 4090"},
	}
	gm.InitGPU(devices)

	// Освобождаем больше чем зарезервировано → должно стать 0
	gm.ReserveGPUMemory(0, 100)
	gm.ReleaseGPUMemory(0, 1000)
	usage := gm.GetGPUUsage()
	if usage[0] != 0 {
		t.Errorf("expected usage[0]=0 after underflow, got %d", usage[0])
	}
}

func TestGPUManagerRoundRobinStrategy(t *testing.T) {
	gm := cppbackend.NewGPUManager("round-robin")
	devices := []bridge.GPUDevice{
		{Index: 0, VRAMTotalMB: 24576, VRAMFreeMB: 24576, Name: "GPU A"},
		{Index: 1, VRAMTotalMB: 12288, VRAMFreeMB: 12288, Name: "GPU B"},
		{Index: 2, VRAMTotalMB: 8192, VRAMFreeMB: 8192, Name: "GPU C"},
	}
	gm.InitGPU(devices)

	ts := gm.CalculateTensorSplit(40000)
	if ts.GPUCount != 3 {
		t.Errorf("expected 3 GPUs, got %d", ts.GPUCount)
	}
	if len(ts.Ratios) != 3 {
		t.Errorf("expected 3 ratios, got %v", ts.Ratios)
	}

	// Round-robin должен выбрать GPU 0 как main (наименьшая usage - все нули)
	if ts.MainGPU != 0 {
		t.Errorf("expected MainGPU=0 for round-robin, got %d", ts.MainGPU)
	}

	// Все ratios должны быть равны ~0.33
	for i, r := range ts.Ratios {
		if r < 0.3 || r > 0.37 {
			t.Errorf("expected ratio[%d] ~0.33, got %f", i, r)
		}
	}
}

func TestGPUManagerManualStrategy(t *testing.T) {
	gm := cppbackend.NewGPUManager("manual")
	devices := []bridge.GPUDevice{
		{Index: 0, VRAMTotalMB: 24576, VRAMFreeMB: 24576, Name: "GPU A"},
		{Index: 1, VRAMTotalMB: 12288, VRAMFreeMB: 12288, Name: "GPU B"},
	}
	gm.InitGPU(devices)

	ts := gm.CalculateTensorSplit(36000)
	if ts.GPUCount != 2 {
		t.Errorf("expected 2 GPUs, got %d", ts.GPUCount)
	}

	// Manual = равномерное распределение
	if ts.Ratios[0] < 0.49 || ts.Ratios[0] > 0.51 {
		t.Errorf("expected ratio[0] ~0.5, got %f", ts.Ratios[0])
	}
	if ts.Ratios[1] < 0.49 || ts.Ratios[1] > 0.51 {
		t.Errorf("expected ratio[1] ~0.5, got %f", ts.Ratios[1])
	}
}

func TestGPUManagerVRAMSummary(t *testing.T) {
	gm := cppbackend.NewGPUManager("vram-ratio")
	devices := []bridge.GPUDevice{
		{Index: 0, VRAMTotalMB: 24576, VRAMFreeMB: 24576, Name: "NVIDIA RTX 4090"},
		{Index: 1, VRAMTotalMB: 24576, VRAMFreeMB: 24576, Name: "NVIDIA RTX 4090"},
	}
	gm.InitGPU(devices)
	gm.ReserveGPUMemory(0, 10240)

	summary := gm.GetVRAMSummary()
	if summary["gpuCount"].(int) != 2 {
		t.Errorf("expected 2 GPUs in summary, got %d", summary["gpuCount"])
	}
	if summary["vramUsedMB"].(uint64) != 10240 {
		t.Errorf("expected 10240 MB used, got %d", summary["vramUsedMB"])
	}
	if summary["vramFreeMB"].(uint64) != 49152-10240 {
		t.Errorf("expected 38912 MB free, got %d", summary["vramFreeMB"])
	}

	devicesList := summary["devices"].([]map[string]interface{})
	if len(devicesList) != 2 {
		t.Errorf("expected 2 devices in list, got %d", len(devicesList))
	}
	if devicesList[0]["vramUsedMB"].(uint64) != 10240 {
		t.Errorf("expected device[0] used=10240, got %d", devicesList[0]["vramUsedMB"])
	}
	if devicesList[1]["vramUsedMB"].(uint64) != 0 {
		t.Errorf("expected device[1] used=0, got %d", devicesList[1]["vramUsedMB"])
	}
}

func TestGPUManagerGetDevices(t *testing.T) {
	gm := cppbackend.NewGPUManager("vram-ratio")
	devices := []bridge.GPUDevice{
		{Index: 0, VRAMTotalMB: 24576, VRAMFreeMB: 10000, Name: "NVIDIA RTX 4090"},
	}
	gm.InitGPU(devices)

	result := gm.GetDevices()
	if len(result) != 1 {
		t.Errorf("expected 1 device, got %d", len(result))
	}
	if result[0].Name != "NVIDIA RTX 4090" {
		t.Errorf("expected name 'NVIDIA RTX 4090', got '%s'", result[0].Name)
	}
	if result[0].VRAMFreeMB != 10000 {
		t.Errorf("expected VRAMFreeMB=10000, got %d", result[0].VRAMFreeMB)
	}
}

// ============================================================
// EstimateModelVRAM tests
// ============================================================

func TestEstimateModelVRAM(t *testing.T) {
	// 10GB модель, 40 layers, все на GPU, контекст 4096
	mem := cppbackend.EstimateModelVRAM(10*1024*1024*1024, -1, 40, 4096)
	if mem == 0 {
		t.Error("expected non-zero VRAM estimate")
	}

	// Должна быть хотя бы 5000 MB для 10GB модели
	if mem < 5000 {
		t.Errorf("expected VRAM >= 5000 MB, got %d", mem)
	}

	// Контекст больше → больше VRAM
	memBigCtx := cppbackend.EstimateModelVRAM(10*1024*1024*1024, -1, 40, 8192)
	if memBigCtx <= mem {
		t.Errorf("expected BigCtx > SmallCtx, got BigCtx=%d <= SmallCtx=%d", memBigCtx, mem)
	}
}

func TestEstimateModelVRAMNoGPU(t *testing.T) {
	mem := cppbackend.EstimateModelVRAM(10*1024*1024*1024, 0, 40, 4096)
	if mem != 0 {
		t.Errorf("expected 0 for CPU-only, got %d", mem)
	}
}

// ============================================================
// TensorSplit String test
// ============================================================

func TestTensorSplitString(t *testing.T) {
	ts := cppbackend.TensorSplit{
		Ratios:     []float32{0.7, 0.3},
		MainGPU:    0,
		GPUCount:   2,
		NGPULayers: -1,
	}
	str := ts.String()
	if str == "" {
		t.Error("expected non-empty string")
	}
}
