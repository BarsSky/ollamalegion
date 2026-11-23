// autotune_budget.go — Round 230 (2026-10-24): корректный бюджет памяти
// для AutoTune.
//
// Два дефекта R54-логики, найденные на живом стенде (A10 24GB, n_ctx=131072,
// KV q8_0, num_gpu_layers=16):
//
//  1. Единицы измерения. types.GPUMetrics.MemoryFree/MemoryTotal/MemoryUsed,
//     types.System.MemoryFree и types.LlamaCppModel.VRAMUsage/RAMUsage отдаются
//     в МБ, а ModelProfileInfo.FreeVRAMBytes/FreeRAMBytes/TotalVRAMBytes — в
//     байтах. Вызовы AnalyzeBackend/triggerAutoTuneReload передавали МБ как
//     байты: 20000 MB превращались в 20000 B ≈ 0.0 GB. Отсюда «свободно 0 GB»,
//     рекомендация q4_0 вместо q8_0 и num_gpu_layers=-2 вместо числа.
//
//  2. Бюджет reload. free VRAM измеряется при ЗАГРУЖЕННОЙ модели, хотя reload
//     её выгружает: перед загрузкой другой конфигурации эта VRAM освободится.
//     Без «+ usage текущей модели» анализ видит остаток (например 2GB вместо
//     24GB) и снова советует q4_0/-2. Бюджет = free + usage модели, ≤ total.
package balancer

import (
	"strings"

	"ollama-loadbalancer/internal/memfit"
)

const (
	bytesPerMB                = 1024 * 1024
	autotuneMinReserveBytes   = 256 * 1024 * 1024 // минимум под activations/compute buffers
	autotuneReservePercent    = 20                // 5% от total VRAM (делитель)
	autotuneLayerSafetyFactor = 0.95              // запас на фрагментацию при подсчёте слоёв
)

// mbToBytes переводит МБ (как их отдают метрики cppworker) в байты.
func mbToBytes(mb uint64) uint64 {
	if mb == 0 {
		return 0
	}
	return mulSat(mb, bytesPerMB)
}

// MBToBytes — экспорт mbToBytes для API-слоя (internal/api передаёт метрики
// GPU/System, которые измеряются в МБ, в AnalyzeBackend, который ждёт байты).
func MBToBytes(mb uint64) uint64 {
	return mbToBytes(mb)
}

// mulSat — умножение с насыщением (защита от overflow на мусорных метриках).
func mulSat(a, b uint64) uint64 {
	if a == 0 || b == 0 {
		return 0
	}
	if a > ^uint64(0)/b {
		return ^uint64(0)
	}
	return a * b
}

// addSat — сложение с насыщением.
func addSat(a, b uint64) uint64 {
	if a > ^uint64(0)-b {
		return ^uint64(0)
	}
	return a + b
}

// reloadBudgetBytes — сколько памяти будет доступно модели ПОСЛЕ выгрузки её
// текущей копии, т.е. free + usage самой модели, но не больше total.
//
// usageMB — ModelProfileInfo.VRAMUsage/RAMUsage (МБ). totalBytes == 0 —
// ограничение неизвестно (для RAM), бюджет не клипуется.
func reloadBudgetBytes(freeBytes, totalBytes, usageMB uint64) uint64 {
	budget := addSat(freeBytes, mbToBytes(usageMB))
	if totalBytes > 0 && budget > totalBytes {
		budget = totalBytes
	}
	return budget
}

// autotuneReserveBytes — запас на activations/compute buffers: max(256MB, 5% total).
func autotuneReserveBytes(totalVRAMBytes uint64) uint64 {
	reserve := uint64(autotuneMinReserveBytes)
	if totalVRAMBytes > 0 {
		if five := totalVRAMBytes / autotuneReservePercent; five > reserve {
			reserve = five
		}
	}
	return reserve
}

// bytesToGB — байты в гибибайтах (для человекочитаемых reason-строк).
func bytesToGB(b uint64) float64 {
	return float64(b) / (1024 * 1024 * 1024)
}

// normalizeKVType приводит тип KV cache к каноническому виду.
func normalizeKVType(kvType string) string {
	normalized := strings.ToLower(strings.TrimSpace(kvType))
	switch normalized {
	case "f16", "fp16", "float16":
		return "f16"
	case "q8_0", "q8":
		return "q8_0"
	case "q4_0", "q4":
		return "q4_0"
	default:
		return normalized
	}
}

// kvCacheBytesForContext — размер KV cache в байтах для n_ctx при типе kvType.
//
// R498 / аудит N-5 (2026-11-19): ОДНА формула KV на весь стек — memfit.
//
// БЫЛО (четвёртая копия формулы): f16 = 2 байта на элемент «как есть»,
// q8_0 — ровно вдвое меньше, q4_0 — вчетверо. В llama.cpp квантованный KV — это
// значение ПЛЮС масштаб на блок: q8_0 = 34/32 байта на элемент, q4_0 = 18/32,
// то есть не /2 и /4, а /1.88 и /3.56. Из-за этого AutoTune считал KV по q8_0
// меньше реального и советовал «урезать окно» там, где окно помещалось.
//
// Константы берутся из memfit.KVBytesPerElement — тот же источник, что у
// C-моста и memfit, поэтому правка в одном месте меняет все.
//
// R707 (2026-11-25): формула теперь считает ПОСЛОЙНО (memfit.KVShape). Если
// профиль несёт раскладку из /api/models (kv_plan_source=plan), оконные слои
// ограничены окном, а shared_kv не считаются вовсе: для gemma-4-E4B на окне
// 65536 это 302 MiB вместо 3.17 GiB (q4_0). Без раскладки (старый cppworker)
// работает прежняя плоская формула «все слои × HeadDimK» — но уже с шириной V
// из профиля (R705), а не «2 × K».
// Возвращает 0, если данных/контекста нет (тогда KV не резервируем).
func kvCacheBytesForContext(p *ModelProfileInfo, kvType string, nCtx int) uint64 {
	if p == nil || nCtx <= 0 {
		return 0
	}
	shape, ok := kvShapeForProfileR707(p)
	if !ok {
		return 0
	}
	return uint64(shape.BytesAtCtx(memfitKVTypeR707(kvType), nCtx))
}

// kvShapeForProfileR707 — раскладка KV профиля в терминах единой формулы.
//
// Порядок:
//  1. раскладка из /api/models ("plan") — классы слоёв с ширинами и окном;
//  2. иначе плоская (все слои, ширина HeadDimK/HeadDimV) — прежнее поведение.
//
// ok=false — геометрии нет вовсе (NLayers/NKvHeads/HeadDimK пустые): KV не
// считаем, вызывающий не должен выдумывать число.
func kvShapeForProfileR707(p *ModelProfileInfo) (memfit.KVShape, bool) {
	if p == nil || p.NKvHeads <= 0 {
		return memfit.KVShape{}, false
	}
	if p.KVPlanSource == "plan" && p.KVGlobalLayers+p.KVSWALayers > 0 {
		global := memfit.KVLayerClass{
			Layers:   p.KVGlobalLayers,
			HeadDimK: p.HeadDimK,
			HeadDimV: p.HeadDimV,
			KVHeads:  p.NKvHeads,
		}
		swaHeads := p.NKvHeadsSWA
		if swaHeads <= 0 {
			swaHeads = p.NKvHeads
		}
		swa := memfit.KVLayerClass{
			Layers:   p.KVSWALayers,
			HeadDimK: p.HeadDimSWA,
			HeadDimV: p.HeadDimSWAV,
			KVHeads:  swaHeads,
		}
		return memfit.KVShape{Global: global, SWA: swa, SWAWindow: p.KVSWAWindow}, true
	}
	if p.NLayers <= 0 || p.HeadDimK <= 0 {
		return memfit.KVShape{}, false
	}
	return memfit.KVShape{
		Global: memfit.KVLayerClass{
			Layers:   p.NLayers,
			HeadDimK: p.HeadDimK,
			HeadDimV: p.HeadDimV,
			KVHeads:  p.NKvHeads,
		},
	}, true
}

// memfitKVTypeR707 — тип KV-cache для memfit по строке профиля.
func memfitKVTypeR707(kvType string) memfit.KVType {
	switch normalizeKVType(kvType) {
	case "q8_0":
		return memfit.KVQ8
	case "q4_0":
		return memfit.KVQ4
	default:
		return memfit.KVF16
	}
}
