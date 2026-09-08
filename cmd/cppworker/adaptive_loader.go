// adaptive_loader.go ? ?????? ?????? ?????????? ???????? ???????.
//
// ??????????:
//   1. EnvironmentProfile ? ??????? ?????? (GPU, VRAM, RAM, CUDA)
//   2. ModelArchitecture ? ??????????? ?????? ?? GGUF (MoE, ????, head_dim)
//   3. DynamicOverhead ? ?????? overhead ?? GPU ??????
//   4. LoadStrategy ? ????? ?????????: exact_fit ? partial_offload ? cpu_only
//   5. kvCacheType auto-downgrade: f16 ? q8_0 ? q4_0
//   6. NaN-???????? ? ?????? ??????? ? auto-reload ??? NaN/??????
//   7. AdaptiveLoader API endpoint
//
// ??? ?????????? ?????????? ?????? ???????? ?????? (EnvironmentProfile),
// ??? ???????????? ???????? ????? lazy-load / reload / fallback.
package main

import (
	"context"
		"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/pkg/logger"
)

// ============================================================
// 1. EnvironmentProfile ? ?????? ??????? ??????
// ============================================================

// AdaptiveGPUDevice ??????? ?????????? ?? ????? GPU.
type AdaptiveGPUDevice struct {
	Index       int    `json:"index"`
	Name        string `json:"name"` // ???????? "NVIDIA A10", "NVIDIA GeForce RTX 3070"
	VRAMTotalMB uint64 `json:"vramTotalMb"`
	VRAMFreeMB  uint64 `json:"vramFreeMb"`
	CUDACompute string `json:"cudaCompute,omitempty"` // "8.6", "7.5" ? ?.?.
}

// EnvironmentProfile ? ??????? ?????, ??????????? ???? ??? ??? ?????? cppworker.
// ??????????? ??? reload (????? bridge ??? nvidia-smi ??? free VRAM).
type EnvironmentProfile struct {
	mu sync.RWMutex

	GPUCount           int                 `json:"gpuCount"`
	AdaptiveGPUDevices []AdaptiveGPUDevice `json:"gpuDevices"`
	TotalVRAM          int64               `json:"totalVramBytes"` // ? ??????
	FreeVRAM           int64               `json:"freeVramBytes"`  // ???????? ??????
	TotalRAM           int64               `json:"totalRamBytes"`
	FreeRAM            int64               `json:"freeRamBytes"`
	HasCUDA            bool                `json:"hasCuda"`
	NVMLReady          bool                `json:"nvmlReady"` // true=bridge, false=nvidia-smi

	// ???????????? ???????
	OverheadBytes int64 `json:"overheadBytes"` // ??????????? ?? GPU ??????
}

// GPUModelOverhead ?????????? overhead ? ?????? ??? ?????????? ?????? GPU.
// ?????? hardcoded 1.5GB ?????????? ?????????:
//
//	A100 80GB   ? 2.5 GB (ECC, ?????? CUDA ??????????)
//	A100 40GB   ? 2.0 GB
//	A10 24GB    ? 1.5 GB
//	RTX 4090 24GB ? 1.8 GB (??????? ?????????? ???????????, ?????? ???????)
//	RTX 4080 16GB ? 1.2 GB
//	RTX 3070 8GB  ? 1.0 GB
//	RTX 3060 12GB ? 1.0 GB
//	T4 16GB       ? 1.2 GB
//	L4 24GB       ? 1.5 GB
//	??????????    ? 1.5 GB (?????????????? default)
func GPUModelOverhead(gpuName string, totalVRAMMB uint64) int64 {
	name := strings.ToLower(gpuName)
	// ?? VRAM ? ??? ??????????? ???????
	switch {
	case totalVRAMMB >= 64000: // 64GB+
		return int64(2500) * 1024 * 1024 // 2.5 GB
	case totalVRAMMB >= 32000: // 32-63GB
		return int64(2000) * 1024 * 1024 // 2.0 GB
	case totalVRAMMB >= 20000: // 20-31GB
		// ????????? A10 vs RTX 4090 vs L4
		if strings.Contains(name, "a10") {
			return int64(1500) * 1024 * 1024 // 1.5 GB
		}
		if strings.Contains(name, "4090") || strings.Contains(name, "l40") || strings.Contains(name, "ada") {
			return int64(1800) * 1024 * 1024 // 1.8 GB
		}
		return int64(1500) * 1024 * 1024
	case totalVRAMMB >= 14000: // 14-19GB
		if strings.Contains(name, "t4") || strings.Contains(name, "a4000") {
			return int64(1200) * 1024 * 1024
		}
		return int64(1200) * 1024 * 1024
	default: // 8-13GB
		return int64(1000) * 1024 * 1024 // 1.0 GB
	}
}

// Refresh ????????? ????????? VRAM ? RAM (?????????? ????? reload).
func (ep *EnvironmentProfile) Refresh() {
	ep.mu.Lock()
	defer ep.mu.Unlock()

	// ????????? VRAM
	if ep.NVMLReady {
		ep.FreeVRAM = tryBridgeFreeVRAM()
	}
	if ep.FreeVRAM <= 0 {
		ep.FreeVRAM = tryNvidiaSMIFree()
	}
	// ????????? RAM
	ep.FreeRAM = availableRAMBytes()

	// ????????? per-device VRAMFreeMB ???? bridge ????????
	for i := range ep.AdaptiveGPUDevices {
		if ep.NVMLReady {
			info, err := bridge.GetGPUInfo(i)
			if err == nil && info != nil && info.VRAMFreeMB > 0 {
				ep.AdaptiveGPUDevices[i].VRAMFreeMB = info.VRAMFreeMB
			}
		}
	}
}

// Get ?????????? ????? ??????? (??? ???????? ? ??????? ??? ??????????).
func (ep *EnvironmentProfile) Get() EnvironmentProfile {
	ep.mu.RLock()
	defer ep.mu.RUnlock()
	return EnvironmentProfile{
		GPUCount:           ep.GPUCount,
		AdaptiveGPUDevices: append([]AdaptiveGPUDevice{}, ep.AdaptiveGPUDevices...),
		TotalVRAM:          ep.TotalVRAM,
		FreeVRAM:           ep.FreeVRAM,
		TotalRAM:           ep.TotalRAM,
		FreeRAM:            ep.FreeRAM,
		HasCUDA:            ep.HasCUDA,
		NVMLReady:          ep.NVMLReady,
		OverheadBytes:      ep.OverheadBytes,
	}
}

// DetectEnvProfile ????????? EnvironmentProfile, ????????? ??????.
func DetectEnvProfile() *EnvironmentProfile {
	ep := &EnvironmentProfile{}

	// ???????? ????? bridge
	info0, err := bridge.GetGPUInfo(0)
	if err == nil && info0 != nil && info0.VRAMTotalMB > 0 {
		ep.NVMLReady = true
		ep.HasCUDA = true
		ep.GPUCount = backend.GetGPUCount()
		if ep.GPUCount <= 0 {
			ep.GPUCount = 1
		}
		ep.TotalVRAM = int64(info0.VRAMTotalMB) * 1024 * 1024
		ep.FreeVRAM = int64(info0.VRAMFreeMB) * 1024 * 1024

		// ????????? ??? ??????????
		for i := 0; i < ep.GPUCount; i++ {
			devInfo, devErr := bridge.GetGPUInfo(i)
			dev := AdaptiveGPUDevice{Index: i}
			if devErr == nil && devInfo != nil {
				dev.VRAMTotalMB = devInfo.VRAMTotalMB
				dev.VRAMFreeMB = devInfo.VRAMFreeMB
				dev.Name = devInfo.Name
				dev.CUDACompute = devInfo.Name
			}
			ep.AdaptiveGPUDevices = append(ep.AdaptiveGPUDevices, dev)
		}
		// Overhead ?? ??????? GPU
		if len(ep.AdaptiveGPUDevices) > 0 {
			ep.OverheadBytes = GPUModelOverhead(ep.AdaptiveGPUDevices[0].Name, ep.AdaptiveGPUDevices[0].VRAMTotalMB)
		}
	} else {
		// Fallback: nvidia-smi
		if v := tryNvidiaSMI(); v > 0 {
			ep.TotalVRAM = v
			ep.FreeVRAM = tryNvidiaSMIFree()
			ep.NVMLReady = false
			ep.HasCUDA = true
			ep.GPUCount = 1
			// ??? GPU ????? nvidia-smi
			if n := nvidiaSmiGPUName(); n != "" {
				ep.AdaptiveGPUDevices = append(ep.AdaptiveGPUDevices, AdaptiveGPUDevice{
					Index: 0, Name: n,
					VRAMTotalMB: uint64(v / (1024 * 1024)),
					VRAMFreeMB:  uint64(ep.FreeVRAM / (1024 * 1024)),
				})
				ep.OverheadBytes = GPUModelOverhead(n, uint64(v/(1024*1024)))
			} else {
				ep.OverheadBytes = int64(1536) * 1024 * 1024 // default 1.5GB
			}
		}
	}

	// RAM
	ep.TotalRAM = totalRAMBytes()
	ep.FreeRAM = availableRAMBytes()

	logger.Get().Infow("EnvironmentProfile detected",
		"gpu_count", ep.GPUCount,
		"total_vram_mb", ep.TotalVRAM/(1024*1024),
		"free_vram_mb", ep.FreeVRAM/(1024*1024),
		"gpu", len(ep.AdaptiveGPUDevices) > 0 && ep.AdaptiveGPUDevices[0].Name != "",
		"nvml_ready", ep.NVMLReady,
		"overhead_mb", ep.OverheadBytes/(1024*1024))

	return ep
}

// nvidiaSmiGPUName ?????????? ??? GPU ????? nvidia-smi.
func nvidiaSmiGPUName() string {
	out, err := exec.Command("nvidia-smi", "--query-gpu=name", "--format=csv,noheader").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.Split(string(out), "\n")[0])
}

// totalRAMBytes ?????????? ????? RAM ? ??????.
func totalRAMBytes() int64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "MemTotal:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil && kb > 0 {
						return kb * 1024
					}
				}
			}
		}
	}
	// macOS
	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err == nil {
		if n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// ============================================================
// 2. ModelArchitecture ? ??????????? ?????? ?? GGUF
// ============================================================

// detectArchType ?????????? ??? ???????????: "moe", "dense", "unknown"
func detectArchType(arch string, nHeads, nKvHeads int) string {
	// MoE ????????: n_kv_heads << n_heads (????????, 8 vs 64 ??? Qwen2 MoE)
	if nKvHeads > 0 && nHeads > 0 && nKvHeads*4 < nHeads {
		return "moe"
	}
	// ?? ????? ???????????
	archLower := strings.ToLower(arch)
	if strings.Contains(archLower, "moe") ||
		strings.Contains(archLower, "mixtral") ||
		(strings.Contains(archLower, "qwen2") && strings.Contains(archLower, "a3b")) ||
		(strings.Contains(archLower, "qwen3") && strings.Contains(archLower, "a3b")) ||
		strings.Contains(archLower, "qwen3moe") ||
		strings.Contains(archLower, "qwen3next") ||
		strings.Contains(archLower, "qwen35moe") {
		return "moe"
	}
	return "dense"
}

// MoEWeightRatio — backward-compat wrapper (returns 0.30 generic, use MoEWeightRatioForSize for size-aware ratio).
func MoEWeightRatio(archType string) float64 {
	return MoEWeightRatioForSize(archType, 0)
}

// MoEWeightRatioForSize — variant with explicit model size (in bytes).
//
// Qwen3.6 35B-A3B Q4_K_M = 22 GB → 0.10 (attention+embed ≈ 2 GB of 22 GB).
// Mixtral 8x7B Q4_K_M ≈ 24 GB → 0.30 (active part is denser).
//
// Возвращает 1.0 для dense архитектур.
func MoEWeightRatioForSize(archType string, sizeBytes int64) float64 {
	if archType != "moe" {
		return 1.0
	}
	if sizeBytes >= 20*1024*1024*1024 {
		return 0.10 // A3B-class
	}
	return 0.30 // generic MoE fallback
}

// ============================================================
// 3. LoadStrategy ? ?????? ????????? ????????
// ============================================================

// LoadStrategyResult ? ????????? ?????? ?????????.
type LoadStrategyResult struct {
	GPULayers     int    `json:"gpuLayers"` // -1 = ???
	NCtx          int    `json:"nCtx"`
	KVCacheType   string `json:"kvCacheType"` // "f16", "q8_0", "q4_0"
	UseMmap       bool   `json:"useMmap"`
	Stage         string `json:"stage"`         // "exact_fit", "partial_offload", "cpu_only", "moe_offload", "auto_retry"
	KVReduced     bool   `json:"kvReduced"`     // ??? ?? downgrade kvCacheType
	GPUReduced    bool   `json:"gpuReduced"`    // ???? ?? ????????? gpu_layers
	NCtxReduced   bool   `json:"nCtxReduced"`   // ??? ?? ???????? n_ctx
	MaxViableNCtx int    `json:"maxViableNCtx"` // ????. n_ctx ??? cpu-only
	Explanation   string `json:"explanation"`   // ???????????????? ????????
	// Round 7: per-tensor override (parallel arrays).
	// For MoE: leave attention on GPU, route expert tensors to CPU.
	OverrideTensors     []string `json:"overrideTensors,omitempty"`
	OverrideTensorBufts []string `json:"overrideTensorBufts,omitempty"`
}

// KVCacheTypeOrder ? ??????? ????: ?? ??????? ? ???????.
var KVCacheTypeOrder = []string{"f16", "q8_0", "q4_0"}

// autoKVCacheEnabled controls whether fallback_no_meta auto-selects kvCacheType
// based on available VRAM. Default true (auto). Set CPPWORKER_AUTO_KV_CACHE=false
// to disable and always use f16.
var autoKVCacheEnabled = true

// init читает CPPWORKER_AUTO_KV_CACHE из окружения (P0 fix: ранее env-var был
// задокументирован, но не читался — переменная оставалась hardcoded=true).
// Принимаемые значения: true/1/yes/on → enable, false/0/no/off → disable.
// Пустая строка или нераспознанное значение → дефолт (true, backward-compatible).
func init() {
	if v := os.Getenv("CPPWORKER_AUTO_KV_CACHE"); v != "" {
		autoKVCacheEnabled = parseBoolEnv(v)
	}
}

// bytesPerKVCacheType ? ????????? ??? ??????? ????.
var bytesPerKVCacheType = map[string]int64{
	"f16":  4,
	"q8_0": 2,
	"q4_0": 1,
}

// SelectStrategy ? ?????? ???????? ????????? ????????.
//
// ????????:
//   1. ?????????? ??????????? (MoE vs dense)
//   2. ??? MoE: ???????? ? RAM (mmap), ?????? attention ?? GPU
//   3. ??? ??????? kvCacheType (f16 ? q8_0 ? q4_0):
//      a. ????????? exact_fit: weights + KV + overhead <= safeVRAM
//      b. ???? ??? ? partial_offload: ????????? gpu_layers
//      c. ???? ??? ? cpu_only: gpu_layers=0, ????????? n_ctx
//   4. ??????? ????????? ????????? ?????????
func SelectStrategy(
	env *EnvironmentProfile,
	modelName string,
	meta cppbackend.GGUFModelMeta,
	requestedNCtx int,
	requestedGPULayers int,
	defaults *cppbackend.Config,
) LoadStrategyResult {
	// Round 7: MoE override-tensors (parallel arrays). When isMOE, route
	// expert tensors to CPU and keep attention on GPU. Pattern matches Qwen3-A3B /
	// Mixtral routed-experts (GGUF: blk.N.ffn_*.exps.weight/bias). Declared
	// here at function top so all branches (including fallback_no_meta) can
	// reference them safely (nil if not MoE).
	var moeOverridePatterns, moeOverrideBufts []string

	if meta.NLayers == 0 || meta.NEmbd == 0 || meta.NHeads == 0 {
		// Auto-select optimal kvCacheType based on available VRAM.
		// With f16 at 65K context on 8GB GPU, KV-cache alone is ~6.7GB → guaranteed OOM.
		// q4_0 reduces this to ~1.7GB, freeing VRAM for more GPU layers.
		kvType := "f16"
		if autoKVCacheEnabled && env != nil && env.FreeVRAM > 0 && requestedNCtx > 32768 {
			// Estimate KV-cache for each type and pick the best that leaves room for model weights.
			// Conservative model estimate: ~5GB for 7-9B Q4_K_M models.
			const conservativeModelMB = 5120
			modelBytes := int64(conservativeModelMB) * 1024 * 1024
			overheadBytes := int64(1000) * 1024 * 1024 // 1GB overhead
			if env.OverheadBytes > 0 {
				overheadBytes = env.OverheadBytes
			}
			for _, candidate := range []string{"q4_0", "q8_0", "f16"} {
				bytesPerToken := bytesPerKVCacheType[candidate]
				if bytesPerToken <= 0 {
					bytesPerToken = 4
				}
				// Conservative KV-cache estimate: 42 layers, n_kv_heads=2, head_dim=128
				kvBytes := int64(2) * int64(42) * int64(requestedNCtx) * int64(2) * int64(128) * bytesPerToken
				totalNeeded := modelBytes + kvBytes + overheadBytes
				if totalNeeded <= int64(env.FreeVRAM) {
					kvType = candidate
					break
				}
			}
		}
		// Calculate max viable n_ctx from available VRAM + RAM.
		// Strategy: offload most layers to RAM (partial_offload), keeping
		// minimal layers on GPU. This frees VRAM for larger KV-cache.
		// Use actual model size from GGUF if available, otherwise conservative estimate.
		const defaultModelMB = 5120
		// Round 6 #11: arch-aware fallback. meta.Architecture is set even when
		// NLayers/NEmbd/NHeads are zero (the trigger for this branch), so we
		// can narrow numLayers per architecture family. Without this, a Qwen3.6
		// 35B-A3B (48 layers, 22 GB) was being estimated as 60 layers (8-15 GB
		// size class), causing per-layer weight math to be off by 25% and
		// partial_offload to mis-target GPU layer count.
		const defaultLayers = 42
		modelBytes := int64(defaultModelMB) * 1024 * 1024
		numLayers := defaultLayers
		if meta.SizeBytes > 0 {
			modelBytes = int64(meta.SizeBytes)
			// Layer estimate: arch first (more accurate), fall back to size heuristic.
			arch := strings.ToLower(meta.Architecture)
			switch {
			case strings.Contains(arch, "qwen3"), strings.Contains(arch, "qwen35"):
				// Qwen3.5 72B is 80 layers, Qwen3.6-35B-A3B is 48, Qwen3-32B is 64.
				if modelBytes > 30*1024*1024*1024 {
					numLayers = 80 // 72B+ class
				} else if modelBytes > 15*1024*1024*1024 {
					numLayers = 48 // 30-35B class (Qwen3.6 35B-A3B)
				} else {
					numLayers = 64 // small Qwen3 default
				}
			case strings.Contains(arch, "gemma"), strings.Contains(arch, "gemma2"):
				numLayers = 42 // Gemma2/3/4 default
			case strings.Contains(arch, "llama"):
				// Llama 3.x: 32 (8B), 40 (70B), 80 (405B).
				if modelBytes > 250*1024*1024*1024 {
					numLayers = 80 // Llama-3.1 405B
				} else if modelBytes > 50*1024*1024*1024 {
					numLayers = 80 // Llama-3 70B
				} else {
					numLayers = 32 // Llama-3 8B
				}
			default:
				// Size-based heuristic as fallback (was the only path before).
				if modelBytes > 15*1024*1024*1024 {
					numLayers = 80 // 70B+ class
				} else if modelBytes > 8*1024*1024*1024 {
					numLayers = 60 // 30-34B class
				}
			}
		}
		const minGPULayers = 4 // keep attention layers on GPU
		perLayer := modelBytes / int64(numLayers)
		overheadBytes := int64(1000) * 1024 * 1024
		if env != nil && env.OverheadBytes > 0 {
			overheadBytes = env.OverheadBytes
		}

		// VRAM used by minimal GPU layers
		gpuWeightsBytes := int64(minGPULayers) * perLayer
		// RAM needed for offloaded layers
		ramNeededBytes := int64(numLayers-minGPULayers) * perLayer

		// Check if we have enough RAM for offload
		canOffload := true
		if env != nil && env.FreeRAM > 0 && ramNeededBytes > int64(env.FreeRAM) {
			canOffload = false
		}

		freeForKV := int64(0)
		if canOffload && env != nil && int64(env.FreeVRAM) > gpuWeightsBytes+overheadBytes {
			// With partial offload: VRAM freed = total - gpu_weights - overhead
			freeForKV = int64(env.FreeVRAM) - gpuWeightsBytes - overheadBytes
		} else if env != nil && int64(env.FreeVRAM) > modelBytes+overheadBytes {
			// Fallback: all layers on GPU (less KV-cache room)
			freeForKV = int64(env.FreeVRAM) - modelBytes - overheadBytes
		}

		// KV-cache per token: 2 (K+V) * N layers * N kv_heads * 128 head_dim * bytes_per_elem
		// Estimate n_kv_heads from model size (conservative):
		//   <10GB → 2 (7-9B class: gemma, mistral)
		//   10-20GB → 4 (13-34B class)
		//   >20GB → 8 (70B+ class)
		nKvHeads := 2
		if modelBytes > 20*1024*1024*1024 {
			nKvHeads = 8
		} else if modelBytes > 10*1024*1024*1024 {
			nKvHeads = 4
		}
		bytesPerToken := bytesPerKVCacheType[kvType]
		if bytesPerToken <= 0 {
			bytesPerToken = 4
		}
		kvPerToken := int64(2) * int64(numLayers) * int64(nKvHeads) * int64(128) * bytesPerToken
		maxViableNCtx := 512
		if kvPerToken > 0 && freeForKV > 0 {
			maxViableNCtx = int(freeForKV / kvPerToken)
			if maxViableNCtx < 512 {
				maxViableNCtx = 512
			}
		}

		return LoadStrategyResult{
			GPULayers:     requestedGPULayers,
			NCtx:          requestedNCtx,
			KVCacheType:   kvType,
			UseMmap:       true,
			Stage:         "fallback_no_meta",
			MaxViableNCtx: maxViableNCtx,
			Explanation: fmt.Sprintf("GGUF header has no architecture data, auto-selected kvCacheType=%s (auto_kv=%v, free_vram=%dMB, free_ram=%dMB, offload=%v, n_ctx=%d, max_viable=%d)",
				kvType, autoKVCacheEnabled, env.FreeVRAM/(1024*1024), env.FreeRAM/(1024*1024), canOffload, requestedNCtx, maxViableNCtx),
			OverrideTensors:     moeOverridePatterns,
			OverrideTensorBufts: moeOverrideBufts,
		}
	}

	// ?????????? ???????????
	archType := detectArchType(meta.Architecture, meta.NHeads, meta.NKvHeads)
	isMOE := archType == "moe"

	// ??? ?????? ? overhead
	weightsPerLayer := int64(meta.SizeBytes) / int64(meta.NLayers)
	moefrac := MoEWeightRatioForSize(archType, meta.SizeBytes)

	// Round 7: When isMOE, populate MoE override-tensors (parallel arrays)
	// to route expert tensors to CPU and keep attention on GPU. Pattern
	// matches Qwen3-A3B / Mixtral routed-experts
	// (GGUF: blk.N.ffn_*.exps.weight/bias).
	if isMOE {
		moeOverridePatterns = []string{
			`blk\.\d+\.ffn_.*_exps\.weight`,
			`blk\.\d+\.ffn_.*_exps\.bias`,
		}
		moeOverrideBufts = []string{"CPU", "CPU"}
	}

	// ?????????? VRAM
	safeVRAM := int64(float64(env.FreeVRAM) * 0.85)
	if safeVRAM <= 0 {
		safeVRAM = int64(float64(env.TotalVRAM) * 0.85)
	}
	// ????????? RAM ?????????? ??? cpu_only branch (?????????, ??????????
	// weights ? KV-cache ???????????? ? RAM/mmap). 0.5 = ?????????????
	// ??? ??????? OS ???????????.
	safeRAM := int64(0)
	if env != nil && env.FreeRAM > 0 {
		safeRAM = int64(float64(env.FreeRAM) * 0.5)
	}
	overhead := env.OverheadBytes

	// ??????? ?????? kvCacheType ?? ??????? ? ???????
	for _, kvType := range KVCacheTypeOrder {
		bytesPerToken := int64(4) // f16 default
		if b, ok := bytesPerKVCacheType[kvType]; ok {
			bytesPerToken = b
		}

		// KV-cache ??? requestedNCtx
		kvCacheBytes := estimateKVCacheBytes(
			requestedNCtx, meta.NLayers, meta.NEmbd,
			meta.NHeads, meta.NKvHeads, kvType)

		// ??? MoE: ?????? attention ?? GPU, ???????? ????? mmap
		gpuWeightFraction := moefrac
		gpuWeightsBytes := int64(float64(meta.SizeBytes) * gpuWeightFraction)
		ramWeightsBytes := int64(meta.SizeBytes) - gpuWeightsBytes
		totalVRAMNeeded := gpuWeightsBytes + kvCacheBytes + overhead

		logger.Get().Debugw("adaptive: trying kvCacheType",
			"model", modelName,
			"kv_type", kvType,
			"arch", archType,
			"free_vram_mb", env.FreeVRAM/(1024*1024),
			"kv_cache_mb", kvCacheBytes/(1024*1024),
			"gpu_weights_mb", gpuWeightsBytes/(1024*1024),
			"total_vram_needed_mb", totalVRAMNeeded/(1024*1024),
			"safe_vram_mb", safeVRAM/(1024*1024))

		if totalVRAMNeeded <= safeVRAM {
			// exact_fit ? ??? ???????
			gpuL := meta.NLayers
			if isMOE {
				// ??? MoE: attention ?? GPU, ???????? ? RAM (????? mmap)
				gpuL = meta.NLayers
				// ????????? ??? attention + KV-cache ???????
				weightsOnly := gpuWeightsBytes + overhead
				if weightsOnly+kvCacheBytes > safeVRAM {
					availForMoE := safeVRAM - overhead - kvCacheBytes
					if availForMoE > 0 {
						perLayer := int64(float64(meta.SizeBytes)*moefrac) / int64(meta.NLayers)
						if perLayer <= 0 {
							perLayer = weightsPerLayer
						}
						reduced := int(availForMoE / perLayer)
						if reduced < 1 {
							reduced = 1
						}
						gpuL = reduced
					} else {
						continue
					}
				}
			}
			if requestedGPULayers > 0 && requestedGPULayers < gpuL {
				gpuL = requestedGPULayers
			}
			return LoadStrategyResult{
				GPULayers:     gpuL,
				NCtx:          requestedNCtx,
				KVCacheType:   kvType,
				UseMmap:       ramWeightsBytes > 0,
				Stage:         "exact_fit",
				KVReduced:     kvType != "f16",
				MaxViableNCtx: requestedNCtx,
				Explanation: fmt.Sprintf("exact_fit: kvType=%s, gpuLayers=%d/%d, vram=%dMB < safe=%dMB",
					kvType, gpuL, meta.NLayers, totalVRAMNeeded/(1024*1024), safeVRAM/(1024*1024)),
				OverrideTensors:     moeOverridePatterns,
				OverrideTensorBufts: moeOverrideBufts,
			}
		}

		// partial_offload: ????????? gpu_layers
		availForWeights := safeVRAM - overhead - kvCacheBytes
		if availForWeights > 0 {
			// Bug fix (Round 5 Fix 4): for MoE, attention-only per-layer
			// is much smaller than full per-layer (Qwen3.6-A3B: 60 MB attn vs
			// 470 MB full with experts). Using effectivePerLayer gives a more
			// honest count of GPU layers, so partial_offload doesn't load
			// too few layers and leave everything in RAM.
			//
			// Estimate: 3 * NEmbd^2 bytes (q + out + small kv ~ 3 * d^2 ~ 60 MB at d=5120 f16).
			effectivePerLayer := weightsPerLayer
			if isMOE && meta.NEmbd > 0 {
				attnEstimate := int64(3) * int64(meta.NEmbd) * int64(meta.NEmbd)
				if attnEstimate > 0 && attnEstimate < weightsPerLayer {
					effectivePerLayer = attnEstimate
				}
			}
			reducedGPULayers := int(availForWeights / effectivePerLayer)
			if reducedGPULayers > meta.NLayers {
				reducedGPULayers = meta.NLayers
			}
			if reducedGPULayers >= 1 {
				// ????????? RAM ??? offloaded ?????
				ramNeeded := int64(meta.NLayers-reducedGPULayers) * weightsPerLayer
				if env.FreeRAM <= 0 || ramNeeded <= env.FreeRAM {
					return LoadStrategyResult{
						GPULayers:     reducedGPULayers,
						NCtx:          requestedNCtx,
						KVCacheType:   kvType,
						UseMmap:       true,
						Stage:         "partial_offload",
						KVReduced:     kvType != "f16",
						GPUReduced:    true,
						MaxViableNCtx: requestedNCtx,
						Explanation: fmt.Sprintf("partial_offload: kvType=%s, gpuLayers=%d/%d, attn_per_layer=%dMB, ram_needed=%dMB, free_ram=%dMB",
							kvType, reducedGPULayers, meta.NLayers, effectivePerLayer/(1024*1024), ramNeeded/(1024*1024), env.FreeRAM/(1024*1024)),
						OverrideTensors:     moeOverridePatterns,
						OverrideTensorBufts: moeOverrideBufts,
					}
				}
			}
		}

		// cpu_only: gpu_layers=0 + reduced n_ctx
		if !isMOE || kvType == "q4_0" {
			// ??? MoE cpu-only ?? ???? ???????????? ? ???????? ??? ? RAM
			// ??????? max n_ctx ??? cpu-only
			kvPerToken := int64(meta.NLayers) * int64(meta.NKvHeads) * (int64(meta.NEmbd) / int64(meta.NHeads)) * bytesPerToken
			if kvPerToken <= 0 {
				kvPerToken = 4096
			}
			// Bug fix (Round 2): cpu_only maxNCtx теперь учитывает не только VRAM,
			// но и безопасную RAM (mmap для KV-cache при gpu_layers=0).
			// Без этого на A10 (24GB) + 30GB RAM с Qwen3.6-A3B мы получали ~103k вместо
			// желаемых 128k при f16 KV (или ~200k при q4_0).
			kvBudget := safeVRAM - overhead
			if safeRAM > 0 {
				kvBudget = kvBudget + safeRAM
			}
			maxNCtx := int(kvBudget / kvPerToken)
			if maxNCtx < 512 {
				maxNCtx = 512
			}
			finalNCtx := requestedNCtx
			if finalNCtx > maxNCtx {
				finalNCtx = maxNCtx
			}
			// R60.13 (2026-09-07): smart-skip safety clamp for cpu_only.
			//
			// Pre-R60.13: the theoretical maxNCtx from safeVRAM + safeRAM could be
			// 200K+ on 8GB VRAM + 16GB RAM. The model would load with cpu_only
			// strategy (KV-cache in RAM) but take 60-180s because touching
			// 48GB of RAM takes time. Client sees 503 + Retry-After: 90-120s
			// and retries — triggering cascading reloads.
			//
			// R60.13: cap cpu_only n_ctx at a practical limit that loads in
			// < 60s on typical hardware. Default 65536 (64K) — for 5GB Qwen3-4B
			// this means ~30-60s reload time vs 60-180s at 131K.
			//
			// Override via env: CPPWORKER_CPU_ONLY_MAX_NCTX (operator tunable).
			// Set to 0 to disable clamp (legacy behavior).
			cpuOnlyMaxNCtx := int64(65536)
			if envMax := os.Getenv("CPPWORKER_CPU_ONLY_MAX_NCTX"); envMax != "" {
				if v, err := strconv.ParseInt(envMax, 10, 64); err == nil {
					cpuOnlyMaxNCtx = v
				}
			}
			wasClamped := false
			if cpuOnlyMaxNCtx > 0 && int64(finalNCtx) > cpuOnlyMaxNCtx {
				logger.Get().Warnw("SelectStrategy: cpu_only n_ctx clamped (R60.13)",
					"model", modelName,
					"requested_n_ctx", requestedNCtx,
					"theoretical_max", maxNCtx,
					"clamped_to", cpuOnlyMaxNCtx,
					"reason", "prevent slow cpu_only reload (5+ min at high n_ctx)",
					"override_env", "CPPWORKER_CPU_ONLY_MAX_NCTX")
				finalNCtx = int(cpuOnlyMaxNCtx)
				wasClamped = true
			}
			explanation := fmt.Sprintf("cpu_only: kvType=%s, n_ctx=%d/%d, max_viable=%d",
				kvType, finalNCtx, requestedNCtx, maxNCtx)
			if wasClamped {
				explanation += fmt.Sprintf(", cpu_only_clamped_to=%d (R60.13)", cpuOnlyMaxNCtx)
			}
			return LoadStrategyResult{
				GPULayers:     0,
				NCtx:          finalNCtx,
				KVCacheType:   kvType,
				UseMmap:       true,
				Stage:         "cpu_only",
				KVReduced:     kvType != "f16",
				GPUReduced:    true,
				NCtxReduced:   finalNCtx < requestedNCtx,
				MaxViableNCtx: maxNCtx,
				Explanation: explanation,
				OverrideTensors:     moeOverridePatterns,
				OverrideTensorBufts: moeOverrideBufts,
			}
		}
	}

	// ?????? ?? ??????? ? fallback ?? ??????????? ?????????
	return LoadStrategyResult{
		GPULayers:           requestedGPULayers,
		NCtx:                requestedNCtx,
		KVCacheType:         "f16",
		UseMmap:             true,
		Stage:               "fallback_no_fit",
		Explanation:         "no strategy fits available VRAM/RAM at any kvCacheType",
		OverrideTensors:     moeOverridePatterns,
		OverrideTensorBufts: moeOverrideBufts,
	}
}

// ============================================================
// 4. NaN-????????
// ============================================================

// NaNHealingConfig ? ???????????? ?????????????????? ??? NaN/???????.
type NaNHealingConfig struct {
	// MaxBrokenPerPeriod ? ????. ??????? ?? ?????? ?? auto-reload
	MaxBrokenPerPeriod int `json:"maxBrokenPerPeriod"`
	// PeriodMinutes ? ?????? ?????????? ? ???????
	PeriodMinutes int `json:"periodMinutes"`
	// GPULayersReductionPct ? ?? ??????? % ????????? gpu_layers ??? NaN
	GPULayersReductionPct float64 `json:"gpuLayersReductionPct"`
	// EnableAutoHeal ? ???????? auto-reload ??? NaN
	EnableAutoHeal bool `json:"enableAutoHeal"`
}

// DefaultNaNHealingConfig ? defaults.
func DefaultNaNHealingConfig() NaNHealingConfig {
	return NaNHealingConfig{
		MaxBrokenPerPeriod:    2,
		PeriodMinutes:         10,
		GPULayersReductionPct: 20.0,
		EnableAutoHeal:        true,
	}
}

// NaNHealer ??????????? ?????? ??????? ? ?????????? auto-reload ??? NaN.
type NaNHealer struct {
	mu       sync.Mutex
	config   NaNHealingConfig
	breaks   map[string][]time.Time // modelName ? []time (????? ???????)
	reloadFn func(modelName string, reductionPct float64) error
}

// NewNaNHealer ??????? healer.
func NewNaNHealer(cfg NaNHealingConfig, fn func(string, float64) error) *NaNHealer {
	return &NaNHealer{
		config:   cfg,
		breaks:   make(map[string][]time.Time),
		reloadFn: fn,
	}
}

// RecordBreak ???????????? ????? ?????? ??? ??????.
// ?????????? true ???? ???????? ????? ? ????? ????????? auto-heal.
func (h *NaNHealer) RecordBreak(modelName string, reason string, lastWriteErr string) bool {
	if !h.config.EnableAutoHeal {
		return false
	}
	// Различаем, что лом идёт от реальной поломки (NaN/OOM/timeout),
	// а не от обычного disconnect (отвалился клиент или прокси).
	//
	// Round 25 (cppworker-gpu reload bug): чистый context.Canceled
	// на ctx_done_on_write = клиентский аборт (Cline default 120s timeout,
	// balancer RequestTimeout, user cancel) — НЕ вина модели. Если это считать,
	// любой длинный стрим вызывает false-positive auto-reload (модель в порядке,
	// просто клиент не дождался). context.DeadlineExceeded / i/o timeout /
	// неизвестные ошибки продолжаем считать — это может быть реальный
	// model/network инцидент.
	switch reason {
	case "write_error":
		// write_error с broken pipe / connection reset = клиент отвалился
		if strings.Contains(lastWriteErr, "broken pipe") || strings.Contains(lastWriteErr, "connection reset") {
			return false
		}
		// другие write_error (например "i/o timeout") = реальная сеть/модель
	case "ctx_done_on_write":
		// Чистый context.Canceled (Cline timeout / balancer close / user cancel)
		// = клиентский аборт, НЕ вина модели.
		if lastWriteErr == "" || lastWriteErr == context.Canceled.Error() {
			return false
		}
		// broken pipe / connection reset — тоже клиент отвалился
		if strings.Contains(lastWriteErr, "broken pipe") || strings.Contains(lastWriteErr, "connection reset") {
			return false
		}
		// context.DeadlineExceeded / i/o timeout / неизвестное — возможный
		// server-side timeout или model hang, считаем.
	default:
		return false // неизвестный тип — не трогаем auto-heal
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	now := time.Now()
	period := time.Duration(h.config.PeriodMinutes) * time.Minute

	// ??????? ?????? ??????
	recent := h.breaks[modelName][:0]
	for _, t := range h.breaks[modelName] {
		if now.Sub(t) < period {
			recent = append(recent, t)
		}
	}
	recent = append(recent, now)
	h.breaks[modelName] = recent

	if len(recent) >= h.config.MaxBrokenPerPeriod {
		return true
	}
	return false
}

// Reset ?????????? ??????? ??????? ??? ??????.
func (h *NaNHealer) Reset(modelName string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.breaks, modelName)
}

// ============================================================
// 5. AdaptiveLoader API endpoint
// ============================================================

var (
	globalEnv     *EnvironmentProfile
	globalNaNHeal *NaNHealer
	envDetectOnce sync.Once
)

// initAdaptiveLoader ?????????????? ?????????? ??????????.
func initAdaptiveLoader(reloadFn func(string, float64) error) {
	envDetectOnce.Do(func() {
		globalEnv = DetectEnvProfile()
		globalNaNHeal = NewNaNHealer(DefaultNaNHealingConfig(), reloadFn)
		logger.Get().Infow("adaptive loader initialized",
			"total_vram_mb", globalEnv.TotalVRAM/(1024*1024),
			"overhead_mb", globalEnv.OverheadBytes/(1024*1024),
			"gpu_count", globalEnv.GPUCount)
	})
}

// handleAdaptiveStrategy ? GET /api/v1/cppworker/adaptive/strategy
// ?????????? ????????? ????????? ??? ?????? ??? ????????.
func handleAdaptiveStrategy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use GET or POST")
		return
	}

	modelName := r.URL.Query().Get("name")
	if modelName == "" {
		writeError(w, http.StatusBadRequest, "name query parameter is required")
		return
	}

	nCtxStr := r.URL.Query().Get("n_ctx")
	nCtx := 32768
	if nCtxStr != "" {
		if v, err := strconv.Atoi(nCtxStr); err == nil && v > 0 {
			nCtx = v
		}
	}

	gpuLayersStr := r.URL.Query().Get("gpu_layers")
	gpuLayers := -2 // AUTO
	if gpuLayersStr != "" {
		if v, err := strconv.Atoi(gpuLayersStr); err == nil {
			gpuLayers = v
		}
	}

	// ???????? ?????????? GGUF
	mm := backend.ModelManager()
	if mm == nil {
		writeError(w, http.StatusInternalServerError, "model manager not initialized")
		return
	}
	meta, err := mm.GetModelMeta(modelName + ".gguf")
	if err != nil {
		meta, err = mm.GetModelMeta(modelName)
		if err != nil {
			writeError(w, http.StatusNotFound, "model metadata not found: "+err.Error())
			return
		}
	}

	env := globalEnv.Get()
	strategy := SelectStrategy(
		&env,
		modelName,
		*meta,
		nCtx,
		gpuLayers,
		currentConfig,
	)

	writeJSON(w, http.StatusOK, strategy)
}

// handleAdaptiveEnvironment ? GET /api/v1/cppworker/adaptive/environment
func handleAdaptiveEnvironment(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	env := globalEnv.Get()
	// Pass by pointer to avoid copying the embedded sync.RWMutex inside
	// EnvironmentProfile (vet's copylocks check). json.Marshal handles the
	// mu field via reflection (it gets a zero value, which is harmless).
	writeJSON(w, http.StatusOK, &env)
}

// handleAdaptiveHealConfig ? GET/POST /api/v1/cppworker/adaptive/heal-config
func handleAdaptiveHealConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := DefaultNaNHealingConfig()
		if globalNaNHeal != nil {
			globalNaNHeal.mu.Lock()
			cfg = globalNaNHeal.config
			globalNaNHeal.mu.Unlock()
		}
		writeJSON(w, http.StatusOK, cfg)
	case http.MethodPost:
		var cfg NaNHealingConfig
		if err := decodeJSONRequest(r, &cfg, 0); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if globalNaNHeal != nil {
			globalNaNHeal.mu.Lock()
			globalNaNHeal.config = cfg
			globalNaNHeal.mu.Unlock()
			logger.Get().Infow("adaptive: heal config updated",
				"max_broken", cfg.MaxBrokenPerPeriod,
				"reduction_pct", cfg.GPULayersReductionPct,
				"auto_heal", cfg.EnableAutoHeal)
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"status": "updated"})
	}
}

// RecordStreamBreak ? ?????????? ?? safeStreamWriter.markBroken ??? NaN-????????.
func RecordStreamBreak(modelName, reason, lastWriteErr string) {
	if globalNaNHeal == nil {
		return
	}
	if globalNaNHeal.RecordBreak(modelName, reason, lastWriteErr) {
		logger.Get().Warnw("adaptive NaN-heal: threshold exceeded, initiating auto-reload",
			"model", modelName,
			"reason", reason,
			"last_error", lastWriteErr)
		// Auto-reload ? ??????????? gpu_layers ?? 20%
		go func() {
			if err := backend.UnloadModel(modelName); err != nil {
				logger.Get().Errorw("adaptive NaN-heal: unload failed", "model", modelName, "error", err)
				return
			}
			// ????????????? ? ???????????? gpu_layers ? ???????? ???????
			// reload ? ?????? ????? ?????? /api/models/reload ? gpuLayers=-2
			globalNaNHeal.Reset(modelName)
		}()
	}
}
