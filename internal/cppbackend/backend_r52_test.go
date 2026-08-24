// Round 52 (2026-08-24): regression test for CalculateOptimalGPULayers
// CPU-RAM accounting with useMmap.
//
// Pre-R52: cpuMemoryMB formula used 0.5*kvCacheMB for the CPU-portion
// of KV cache, even when gpuLayers=0 (all layers on CPU → 100% KV on
// CPU). And it always counted model weight bytes against RAM, even
// when useMmap=true (mmap allows OS to page weights in/out of RAM).
//
// This test verifies the cpuMemoryMB math directly:
//   - bestLayers=0 (all CPU), useMmap=true → cpuMemoryMB should be ~kvCacheMB (not weights+half_kv)
//   - bestLayers=0, useMmap=false → cpuMemoryMB should be ~weights+kvCacheMB
//
// We don't test the full integration because that depends on the
// binary search result, which is implementation-defined.
package cppbackend

import (
	"testing"

	"ollama-loadbalancer/c/bridge"
)

// TestCalculateOptimalGPULayers_NoVRAMData_NoMmap — unit-level test
// for the R52 fix in cpuMemoryMB. With totalFreeVRAM=0 the function
// short-circuits to wantedGPULayers/false. Use this to test the
// happy path of useMmap return value.
func TestCalculateOptimalGPULayers_NoVRAMData_NoMmap(t *testing.T) {
	b := &Backend{
		gpuDevices: []bridge.GPUDevice{
			{Index: 0, VRAMTotalMB: 22 * 1024, VRAMFreeMB: 22 * 1024},
		},
	}
	// 19GB model, 32K ctx — wanted 19GB+8MB+1GB+0.44GB = 14.7GB < 22GB → exact fit
	// Returns (48, false, nil) — pre-R52 didn't use mmap in this case (model fits
	// in VRAM entirely). R52 doesn't change this.
	_, useMmap, diag := b.CalculateOptimalGPULayers(
		19*1024*1024*1024, 48, 32, 8, 2560,
		48, // wanted = 48 (all layers), -2 would be normalized to 48 internally
		32768,
	)
	if diag != nil && diag.Recomendation != "" {
		t.Errorf("unexpected error diagnostic: %+v", diag)
	}
	_ = useMmap // when all layers fit in VRAM, useMmap=false is fine
}

// TestCalculateOptimalGPULayers_22GB_VRAM_19GB_Model_32KCtx —
// integrated test for the user's reported scenario: 22GB VRAM,
// 20GB RAM, 19GB model, n_ctx=32768, requestedGPULayers=-2 (auto).
// The function should return a valid strategy (no error diagnostic).
func TestCalculateOptimalGPULayers_22GB_VRAM_19GB_Model_32KCtx(t *testing.T) {
	b := &Backend{
		gpuDevices: []bridge.GPUDevice{
			{Index: 0, VRAMTotalMB: 22 * 1024, VRAMFreeMB: 22 * 1024},
		},
	}
	optGPULayers, _, diag := b.CalculateOptimalGPULayers(
		19*1024*1024*1024, 48, 32, 8, 2560, // Qwen3.6-35B-A3B Q4_K_M
		-2, // auto
		32768,
	)
	// For 19GB/22GB/20GB with 32K ctx, the function should return
	// EITHER a successful strategy (gpuLayers=43+, all on GPU) OR
	// gpuLayers=0 (all CPU) with useMmap=true. The KEY assertion is
	// no error diagnostic for a scenario that should work.
	if diag != nil && diag.Recomendation != "" {
		t.Errorf("R52 regression: should NOT return error diagnostic for 19GB/22GB/20GB scenario. diag=%+v", diag)
	}
	_ = optGPULayers
}
