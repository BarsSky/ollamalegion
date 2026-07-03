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
	"encoding/json"
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
	Index      int    `json:"index"`
	Name       string `json:"name"`       // ???????? "NVIDIA A10", "NVIDIA GeForce RTX 3070"
	VRAMTotalMB uint64 `json:"vramTotalMb"`
	VRAMFreeMB  uint64 `json:"vramFreeMb"`
	CUDACompute string `json:"cudaCompute,omitempty"` // "8.6", "7.5" ? ?.?.
}

// EnvironmentProfile ? ??????? ?????, ??????????? ???? ??? ??? ?????? cppworker.
// ??????????? ??? reload (????? bridge ??? nvidia-smi ??? free VRAM).
type EnvironmentProfile struct {
	mu sync.RWMutex

	GPUCount   int          `json:"gpuCount"`
	AdaptiveGPUDevices []AdaptiveGPUDevice  `json:"gpuDevices"`
	TotalVRAM  int64        `json:"totalVramBytes"`  // ? ??????
	FreeVRAM   int64        `json:"freeVramBytes"`   // ???????? ??????
	TotalRAM   int64        `json:"totalRamBytes"`
	FreeRAM    int64        `json:"freeRamBytes"`
	HasCUDA    bool         `json:"hasCuda"`
	NVMLReady  bool         `json:"nvmlReady"` // true=bridge, false=nvidia-smi

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
		GPUCount:      ep.GPUCount,
		AdaptiveGPUDevices:    append([]AdaptiveGPUDevice{}, ep.AdaptiveGPUDevices...),
		TotalVRAM:     ep.TotalVRAM,
		FreeVRAM:      ep.FreeVRAM,
		TotalRAM:      ep.TotalRAM,
		FreeRAM:       ep.FreeRAM,
		HasCUDA:       ep.HasCUDA,
		NVMLReady:     ep.NVMLReady,
		OverheadBytes: ep.OverheadBytes,
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
	if nKvHeads > 0 && nHeads > 0 && nKvHeads < nHeads/4 {
		return "moe"
	}
	// ?? ????? ???????????
	archLower := strings.ToLower(arch)
	if strings.Contains(archLower, "moe") || strings.Contains(archLower, "mixtral") || strings.Contains(archLower, "qwen2") && strings.Contains(archLower, "a3b") {
		return "moe"
	}
	return "dense"
}

// MoEWeightRatio ?????????? ???? ?????, ??????? ??????? ???? ?? GPU ??? MoE.
// ??? MoE: ?????? attention ???? (gate+up+down ???????? ????????? ? RAM).
// ??????: Qwen3.6 35B-A3B ????? 80 ?????, 64 ????????, topK=8.
// ?? GPU ???? ?????? KV-cache (attention), ???????? ????? mmap ? RAM.
// ?????????? 1.0 ??? dense ???????.
func MoEWeightRatio(archType string) float64 {
	if archType == "moe" {
		return 0.3 // ~30% ????? ? attention, 70% ? ???????? ? RAM
	}
	return 1.0
}

// ============================================================
// 3. LoadStrategy ? ?????? ????????? ????????
// ============================================================

// LoadStrategyResult ? ????????? ?????? ?????????.
type LoadStrategyResult struct {
	GPULayers    int    `json:"gpuLayers"`    // -1 = ???
	NCtx         int    `json:"nCtx"`
	KVCacheType  string `json:"kvCacheType"`  // "f16", "q8_0", "q4_0"
	UseMmap      bool   `json:"useMmap"`
	Stage        string `json:"stage"`        // "exact_fit", "partial_offload", "cpu_only", "moe_offload", "auto_retry"
	KVReduced    bool   `json:"kvReduced"`    // ??? ?? downgrade kvCacheType
	GPUReduced   bool   `json:"gpuReduced"`   // ???? ?? ????????? gpu_layers
	NCtxReduced  bool   `json:"nCtxReduced"`  // ??? ?? ???????? n_ctx
	MaxViableNCtx int   `json:"maxViableNCtx"` // ????. n_ctx ??? cpu-only
	Explanation  string `json:"explanation"`  // ???????????????? ????????
}

// KVCacheTypeOrder ? ??????? ????: ?? ??????? ? ???????.
var KVCacheTypeOrder = []string{"f16", "q8_0", "q4_0"}

// autoKVCacheEnabled controls whether fallback_no_meta auto-selects kvCacheType
// based on available VRAM. Default true (auto). Set CPPWORKER_AUTO_KV_CACHE=false
// to disable and always use f16.
var autoKVCacheEnabled = true

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
		const defaultLayers = 42
		modelBytes := int64(defaultModelMB) * 1024 * 1024
		numLayers := defaultLayers
		if meta.SizeBytes > 0 {
			modelBytes = int64(meta.SizeBytes)
			// Guess layers from file size: ~119 MB/layer for Q4_K_M 7-9B,
			// ~250 MB/layer for larger models.
			if modelBytes > 15*1024*1024*1024 {
				numLayers = 80 // 70B+ class
			} else if modelBytes > 8*1024*1024*1024 {
				numLayers = 60 // 30-34B class
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
		}
	}

	// ?????????? ???????????
	archType := detectArchType(meta.Architecture, meta.NHeads, meta.NKvHeads)
	isMOE := archType == "moe"

	// ??? ?????? ? overhead
	weightsPerLayer := int64(meta.SizeBytes) / int64(meta.NLayers)
	moefrac := MoEWeightRatio(archType)

	// ?????????? VRAM
	safeVRAM := int64(float64(env.FreeVRAM) * 0.85)
	if safeVRAM <= 0 {
		safeVRAM = int64(float64(env.TotalVRAM) * 0.85)
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
						perLayer := int64(float64(meta.SizeBytes) * moefrac) / int64(meta.NLayers)
						if perLayer <= 0 { perLayer = weightsPerLayer }
						reduced := int(availForMoE / perLayer)
						if reduced < 1 { reduced = 1 }
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
				GPULayers:    gpuL,
				NCtx:         requestedNCtx,
				KVCacheType:  kvType,
				UseMmap:      ramWeightsBytes > 0,
				Stage:        "exact_fit",
				KVReduced:    kvType != "f16",
				MaxViableNCtx: requestedNCtx,
				Explanation:  fmt.Sprintf("exact_fit: kvType=%s, gpuLayers=%d/%d, vram=%dMB < safe=%dMB",
					kvType, gpuL, meta.NLayers, totalVRAMNeeded/(1024*1024), safeVRAM/(1024*1024)),
			}
		}

		// partial_offload: ????????? gpu_layers
		availForWeights := safeVRAM - overhead - kvCacheBytes
		if availForWeights > 0 {
			reducedGPULayers := int(availForWeights / weightsPerLayer)
			if reducedGPULayers > meta.NLayers {
				reducedGPULayers = meta.NLayers
			}
			if reducedGPULayers >= 1 {
				// ????????? RAM ??? offloaded ?????
				ramNeeded := int64(meta.NLayers-reducedGPULayers) * weightsPerLayer
				if env.FreeRAM <= 0 || ramNeeded <= env.FreeRAM {
					return LoadStrategyResult{
						GPULayers:    reducedGPULayers,
						NCtx:         requestedNCtx,
						KVCacheType:  kvType,
						UseMmap:      true,
						Stage:        "partial_offload",
						KVReduced:    kvType != "f16",
						GPUReduced:   true,
						MaxViableNCtx: requestedNCtx,
						Explanation:  fmt.Sprintf("partial_offload: kvType=%s, gpuLayers=%d/%d, ram_needed=%dMB, free_ram=%dMB",
							kvType, reducedGPULayers, meta.NLayers, ramNeeded/(1024*1024), env.FreeRAM/(1024*1024)),
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
			maxNCtx := int((safeVRAM - overhead) / kvPerToken)
			if maxNCtx < 512 {
				maxNCtx = 512
			}
			finalNCtx := requestedNCtx
			if finalNCtx > maxNCtx {
				finalNCtx = maxNCtx
			}
			return LoadStrategyResult{
				GPULayers:    0,
				NCtx:         finalNCtx,
				KVCacheType:  kvType,
				UseMmap:      true,
				Stage:        "cpu_only",
				KVReduced:    kvType != "f16",
				GPUReduced:   true,
				NCtxReduced:  finalNCtx < requestedNCtx,
				MaxViableNCtx: maxNCtx,
				Explanation:  fmt.Sprintf("cpu_only: kvType=%s, n_ctx=%d/%d, max_viable=%d",
					kvType, finalNCtx, requestedNCtx, maxNCtx),
			}
		}
	}

	// ?????? ?? ??????? ? fallback ?? ??????????? ?????????
	return LoadStrategyResult{
		GPULayers:   requestedGPULayers,
		NCtx:        requestedNCtx,
		KVCacheType: "f16",
		UseMmap:     true,
		Stage:       "fallback_no_fit",
		Explanation: "no strategy fits available VRAM/RAM at any kvCacheType",
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
	// ?????????, ??? ????? ????? ?? ???????? ?????? (NaN/OOM/timeout),
	// ? ?? ?? ?????????? disconnect (???????????? ?????? ???????).
	switch reason {
	case "write_error":
		// write_error ? broken pipe / connection reset = ?????? ?????????
		if strings.Contains(lastWriteErr, "broken pipe") || strings.Contains(lastWriteErr, "connection reset") {
			return false
		}
		// ?????? write_error (???????? "i/o timeout") ? ???????? ????/???????
	case "ctx_done_on_write":
		// context canceled ??? broken pipe = ?????? ???????? ?????? (timeout/OOM)
		// ?? ??????? ?????????? disconnect ? ??? ?????? ?? ??????/?? ?????? ????????
		if strings.Contains(lastWriteErr, "broken pipe") || strings.Contains(lastWriteErr, "connection reset") {
			return false // ?????? ?????????
		}
	default:
		return false // ?????? ??????? ? ?? ???? ????????
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
	globalEnv      *EnvironmentProfile
	globalNaNHeal  *NaNHealer
	envDetectOnce  sync.Once
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
	writeJSON(w, http.StatusOK, env)
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
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
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

