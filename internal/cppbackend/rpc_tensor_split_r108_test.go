//go:build llama_stub

// rpc_tensor_split_r108_test.go — S1 (2026-10-13): раскладка слоёв на удалённые
// устройства.
//
// Живой дефект: мост честно передавал llama.cpp два устройства («RPC: устройств в
// раскладке 2»), но дальше раскладка не доходила. Две причины, обе закрыты:
//  1. Режим раскладки не доезжал до моста (bridge.ModelConfig.SplitMode оставался
//     0 = LLAMA_SPLIT_MODE_NONE), и llama.cpp (src/llama.cpp:284) оставлял из
//     списка только main_gpu — проверяется в cmd/cppworker (source-guard тест);
//  2. tensor_split считался только по локальным картам (gpuCount=1).
//
// Здесь проверяется (2): пропорции обязаны учитывать свободную память удалённых
// устройств, а мастер — остаться на локальной карте (порядок моста: локальные
// устройства первыми).
package cppbackend

import (
	"math"
	"testing"

	"ollama-loadbalancer/c/bridge"
)

func newTestGPUManager(localVRAMTotalMB uint64) *GPUManager {
	m := NewGPUManager("vram-ratio")
	m.InitGPU([]bridge.GPUDevice{{Index: 0, Name: "RTX 3070", VRAMTotalMB: localVRAMTotalMB, VRAMFreeMB: localVRAMTotalMB}})
	return m
}

// TestR108_TensorSplitIncludesRemoteDevices — локальные + удалённые устройства
// дают пропорции по свободной памяти, мастер остаётся локальным (mainGPU=0).
func TestR108_TensorSplitIncludesRemoteDevices(t *testing.T) {
	m := newTestGPUManager(4096) // 4 ГБ локально
	// Локальная занята на 1 ГБ → свободно 3 ГБ; узел отдаёт 6 ГБ.
	m.ReserveGPUMemory(0, 1024)

	remote := []bridge.DeviceInfo{
		{Name: "RPC0", Type: 1, IsRemote: true, MemoryFree: 6 * 1024 * 1024 * 1024, MemoryTotal: 8 * 1024 * 1024 * 1024},
	}
	ts := m.CalculateTensorSplitWithRPC(remote)

	if ts.GPUCount != 2 {
		t.Fatalf("GPUCount = %d, ожидалось 2 (локальная + узел)", ts.GPUCount)
	}
	if ts.MainGPU != 0 {
		t.Errorf("MainGPU = %d, ожидался 0: мастер обязан остаться на локальной карте, "+
			"иначе его активации пойдут по сети", ts.MainGPU)
	}
	if len(ts.Ratios) != 2 {
		t.Fatalf("Ratios = %v, ожидалось 2 элемента", ts.Ratios)
	}
	// 3 ГБ локально против 6 ГБ на узле → 1/3 и 2/3.
	if math.Abs(float64(ts.Ratios[0])-1.0/3.0) > 0.02 {
		t.Errorf("доля локальной карты = %.3f, ожидалось ~0.333", ts.Ratios[0])
	}
	if math.Abs(float64(ts.Ratios[1])-2.0/3.0) > 0.02 {
		t.Errorf("доля узла = %.3f, ожидалось ~0.667", ts.Ratios[1])
	}
	sum := float32(0)
	for _, r := range ts.Ratios {
		sum += r
	}
	if math.Abs(float64(sum)-1.0) > 0.001 {
		t.Errorf("сумма пропорций = %.4f, ожидалась 1.0", sum)
	}
}

// TestR108_TensorSplitNoRemoteKeepsLocalBehaviour — без подключённых узлов
// поведение прежнее: раскладка только по локальным картам.
func TestR108_TensorSplitNoRemoteKeepsLocalBehaviour(t *testing.T) {
	m := newTestGPUManager(8192)
	ts := m.CalculateTensorSplitWithRPC(nil)
	if ts.GPUCount != 1 {
		t.Errorf("GPUCount = %d, ожидалось 1 (только локальная карта)", ts.GPUCount)
	}
}

// TestR108_RemoteDevicesFilter — в tensor_split уходят только удалённые
// устройства: локальные уже посчитаны GPUManager'ом, дублировать их нельзя
// (иначе пропорций станет больше, чем устройств в раскладке).
func TestR108_RemoteDevicesFilter(t *testing.T) {
	list := []bridge.DeviceInfo{
		{Name: "CUDA0", IsRemote: false, MemoryFree: 100},
		{Name: "RPC0", IsRemote: true, MemoryFree: 200},
	}
	remote := remoteDevices(list)
	if len(remote) != 1 || remote[0].Name != "RPC0" {
		t.Fatalf("remoteDevices = %+v, ожидалось только RPC0", remote)
	}
}

// TestR115_TensorSplitThreeDevices — R115 (пункт «больше двух машин»):
// мастер + ДВА узла. Проверяется, что раскладка не двухместная по построению:
// пропорции считаются по всем устройствам, сумма равна 1, мастер остаётся
// локальным (main_gpu=0), а порядок — локальное устройство первым.
func TestR115_TensorSplitThreeDevices(t *testing.T) {
	m := newTestGPUManager(8192)
	m.ReserveGPUMemory(0, 2048) // локально свободно 6 ГБ

	remote := []bridge.DeviceInfo{
		{Name: "RPC0", Type: 1, IsRemote: true,
			MemoryFree: 3 * 1024 * 1024 * 1024, MemoryTotal: 8 * 1024 * 1024 * 1024},
		{Name: "RPC1", Type: 1, IsRemote: true,
			MemoryFree: 3 * 1024 * 1024 * 1024, MemoryTotal: 8 * 1024 * 1024 * 1024},
	}
	ts := m.CalculateTensorSplitWithRPC(remote)

	if ts.GPUCount != 3 {
		t.Fatalf("GPUCount = %d, ожидалось 3 (локальная + два узла)", ts.GPUCount)
	}
	if len(ts.Ratios) != 3 {
		t.Fatalf("Ratios = %v, ожидалось 3 элемента: по одному на устройство", ts.Ratios)
	}
	if ts.MainGPU != 0 {
		t.Errorf("MainGPU = %d, ожидался 0 (мастер локальный)", ts.MainGPU)
	}
	// 6 ГБ локально против 3 + 3 ГБ на узлах → половина локально, по четверти узлам.
	if math.Abs(float64(ts.Ratios[0])-0.5) > 0.02 {
		t.Errorf("доля локальной карты = %.3f, ожидалось ~0.5", ts.Ratios[0])
	}
	for i, r := range ts.Ratios[1:] {
		if math.Abs(float64(r)-0.25) > 0.02 {
			t.Errorf("доля узла %d = %.3f, ожидалось ~0.25", i, r)
		}
	}
	sum := float32(0)
	for _, r := range ts.Ratios {
		sum += r
	}
	if math.Abs(float64(sum)-1.0) > 0.001 {
		t.Errorf("сумма пропорций = %.4f, ожидалась 1.0", sum)
	}
}

// TestR115_TensorSplitSkipsDeadNode — узел с нулевой свободной памятью не
// получает долю: иначе устройство попало бы в tensor_split, но не смогло
// принять слои (живой случай — узел, чья память уже занята).
//
// Проверяется ТА ЖЕ композиция, что и в бою (backend.go:1179):
// remoteDevices(снимок) → CalculateTensorSplitWithRPC. Отсев делается в
// remoteDevices, потому что список устройств для llama.cpp строится там же —
// выкинуть устройство из пропорций, оставив его в списке, значило бы сдвинуть
// соответствие «пропорция ↔ устройство».
func TestR115_TensorSplitSkipsDeadNode(t *testing.T) {
	m := newTestGPUManager(8192)
	snapshot := []bridge.DeviceInfo{
		{Name: "CUDA0", IsRemote: false, MemoryFree: 8 * 1024 * 1024 * 1024},
		{Name: "RPC0", Type: 1, IsRemote: true, MemoryFree: 4 * 1024 * 1024 * 1024, MemoryTotal: 8 * 1024 * 1024 * 1024},
		{Name: "RPC1", Type: 1, IsRemote: true, MemoryFree: 0, MemoryTotal: 8 * 1024 * 1024 * 1024},
	}
	remote := remoteDevices(snapshot)
	if len(remote) != 1 || remote[0].Name != "RPC0" {
		t.Fatalf("remoteDevices = %+v, ожидался только живой узел RPC0", remote)
	}

	ts := m.CalculateTensorSplitWithRPC(remote)

	if ts.GPUCount != 2 {
		t.Errorf("GPUCount = %d, ожидалось 2: узел без свободной памяти в раскладку не идёт", ts.GPUCount)
	}
	sum := float32(0)
	for _, r := range ts.Ratios {
		sum += r
	}
	if math.Abs(float64(sum)-1.0) > 0.001 {
		t.Errorf("сумма пропорций = %.4f, ожидалась 1.0", sum)
	}
}
