// autotune_budget_r230.go — Round 230 (2026-10-24): корректный бюджет памяти
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

import "strings"

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
// Формула: 2 (K+V) * NLayers * NKvHeads * HeadDimK * bytesPerElement, где
// f16 = 2 байта, q8_0 = 1 байт, q4_0 = 0.5 байта (block-quantized).
// Возвращает 0, если данных/контекста нет (тогда KV не резервируем).
func kvCacheBytesForContext(p *ModelProfileInfo, kvType string, nCtx int) uint64 {
	if p == nil || nCtx <= 0 || p.NLayers <= 0 || p.NKvHeads <= 0 || p.HeadDimK <= 0 {
		return 0
	}
	typ := normalizeKVType(kvType)
	if typ == "" {
		return 0
	}
	perTokenF16 := mulSat(4, mulSat(uint64(p.NLayers), mulSat(uint64(p.NKvHeads), uint64(p.HeadDimK))))
	var perToken uint64
	switch typ {
	case "f16":
		perToken = perTokenF16
	case "q4_0":
		perToken = perTokenF16 / 4
	default: // q8_0 и прочие 8-битные
		perToken = perTokenF16 / 2
	}
	return mulSat(perToken, uint64(nCtx))
}
