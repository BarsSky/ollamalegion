// memfit_adapter.go — адаптеры между cppbackend и internal/memfit.
//
// R83, шаги 2–3 миграции. Сначала это был мост для shadow-сравнения (считаем и
// старым кодом, и memfit, решение принимает старый). После переключения решений
// (шаг 3) shadow-машинерия удалена: и потолки n_ctx, и гейт загрузки, и раскладка
// слоёв на GPU считаются memfit, поэтому сравнивать больше не с чем.
//
// Здесь остаётся только сбор ВХОДНЫХ данных: снимок ресурсов (Budget) и описание
// модели (ModelSpec). Формулы и вердикт — в internal/memfit.
//
// Подробности: plans/2026-09-25-memory-fit-subsystem.md.
package cppbackend

import (
	"strings"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/internal/memfit"
)

// defaultRAMReserveMBForFit — системный резерв под ОС и runtime (как в
// CalculateResourceLimits). Задан здесь явно, а не четвёртым числом в коде.
const defaultRAMReserveMBForFit = 4096

// MemfitPolicy — политика решения о памяти.
//
// Одна скидка на ресурс: VRAM − 2 GiB (CUDA-контекст и буферы), RAM − 4 GiB
// (система и runtime). Дополнительная доля RAMUtil не применяется — см.
// memfit.Policy: сочетание 0.8 и резерва 4 GiB давало двойную скидку и отказывало
// в загрузке 27B на машине, где она физически грузится и работает.
func MemfitPolicy() memfit.Policy {
	return memfit.Policy{VRAMUtil: 1.0, RAMUtil: 1.0, UnknownIsFatal: false}
}

// EffectiveKVCacheType — какой тип KV-cache реально пойдёт в расчёт (per-request,
// иначе дефолт конфига). Экспортирован, чтобы решения принимались на тех же
// входных данных, что и прежний код: иначе f16 против q4_0 даёт разницу вчетверо.
func (b *Backend) EffectiveKVCacheType(perRequest string) string {
	if b == nil {
		return "f16"
	}
	return effectiveKVCacheType(perRequest, b.cfg.DefaultKVCacheType)
}

// MemfitKVType переводит строковый тип KV-cache в тип пакета memfit.
// Пустая строка и неизвестные значения → f16 (безопасный верх: наибольший KV).
func MemfitKVType(s string) memfit.KVType {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "q4_0":
		return memfit.KVQ4
	case "q8_0":
		return memfit.KVQ8
	default:
		return memfit.KVF16
	}
}

// MemfitBudget собирает снимок ресурсов из живого состояния backend'а и системы.
// VRAM обновляется из C-bridge (как раньше в CalculateResourceLimits), RAM — из
// memfit.ProbeRAM (env → /proc/meminfo → cgroup), см. ram_available.go.
func (b *Backend) MemfitBudget() memfit.Budget {
	budget := memfit.Budget{
		VRAMReserve: memfit.MiBOf(int64(vramOverheadMBConfig())),
		RAMReserve:  memfit.MiBOf(defaultRAMReserveMBForFit),
	}

	if b != nil {
		b.mu.Lock()
		for i := 0; i < b.gpuCount; i++ {
			if dev, err := bridge.GetGPUInfo(i); err == nil && dev != nil {
				if i < len(b.gpuDevices) {
					b.gpuDevices[i].VRAMFreeMB = dev.VRAMFreeMB
					b.gpuDevices[i].VRAMTotalMB = dev.VRAMTotalMB
				}
			}
		}
		var free, total uint64
		for _, dev := range b.gpuDevices {
			total += uint64(dev.VRAMTotalMB)
			free += uint64(dev.VRAMFreeMB)
		}
		b.mu.Unlock()

		if total > 0 {
			budget.VRAMTotal = memfit.MiBOf(int64(total))
			budget.VRAMFree = memfit.MiBOf(int64(free))
			budget.VRAMKnown = true
		}
	}

	pr := memfit.ProbeRAM()
	budget.RAMAvail = pr.Available
	budget.RAMTotal = pr.Total
	budget.RAMLimit = pr.Limit
	budget.RAMLimitKnown = pr.LimitKnown
	budget.RAMKnown = pr.Known
	budget.Source = pr.Source
	return budget
}

// WeightsOnGPUBytes — сколько байт весов модели уедет на GPU при gpuLayers слоях
// из totalLayers.
//
// R83 §9.4 (2026-09-26): это та же пропорция, которой пользуется
// memfit.computeSplit (`ModelSpec.SizeBytes.Scale(g, n_layers)`), вынесенная для
// вызывающих, которым нужна ОЦЕНКА ЧИСЛА, а не вердикт: выбор бэкенда, warmup и
// scoring сравнивают байты, чтобы принять решение о маршрутизации.
//
// Заменяет legacy EstimateGPUMemoryForModel в части весов: та формула брала
// «70% модели» (×0.7) — занижение, из-за которого на 8 GB карте планировалось
// меньше слоёв, чем помещается, и модель уходила в CPU-only.
//
// gpuLayers: 0 = CPU-only (весов на GPU нет), -1 = «все слои» (как в llama.cpp),
// >= totalLayers = тоже все. Важно: 0 и -1 — РАЗНЫЕ случаи, и legacy-функция,
// которую эта замена закрывает, различала их так же.
func WeightsOnGPUBytes(sizeBytes int64, gpuLayers, totalLayers int) uint64 {
	if sizeBytes <= 0 || totalLayers <= 0 {
		return 0
	}
	if gpuLayers == 0 {
		return 0
	}
	if gpuLayers < 0 || gpuLayers >= totalLayers {
		return uint64(sizeBytes)
	}
	return uint64(int64(sizeBytes) * int64(gpuLayers) / int64(totalLayers))
}

// MemfitSpec собирает описание модели по внешнему имени (включая «qwen3.8:latest»).
// Резолв — тем же путём, что и сама загрузка.
func (b *Backend) MemfitSpec(name string) (memfit.ModelSpec, bool) {
	if b == nil || strings.TrimSpace(name) == "" {
		return memfit.ModelSpec{}, false
	}
	mm := b.ModelManager()
	if mm == nil {
		return memfit.ModelSpec{}, false
	}
	meta, err := mm.GetModelMeta(name)
	if err != nil || meta == nil {
		if _, canonical, ok := mm.FindModelByVariants(name); ok {
			meta, err = mm.GetModelMeta(canonical)
		}
	}
	if err != nil || meta == nil {
		return memfit.ModelSpec{}, false
	}
	return MemfitSpecFromMeta(name, meta), true
}

// MemfitSpecFromMeta — сборка ModelSpec из метаданных GGUF, включая параметры
// реального KV-кэша (число слоёв с KV и head_dim KV). Без них memfit считал бы KV
// по block_count и n_embd/n_heads, то есть завышал бы его в разы (kv_layers.go).
func MemfitSpecFromMeta(name string, meta *GGUFModelMeta) memfit.ModelSpec {
	if meta == nil {
		return memfit.ModelSpec{}
	}
	kvLayers, _ := meta.KVLayers()
	return MemfitSpecFromValues(name, meta.SizeBytes, meta.NLayers, meta.NHeads, meta.NKvHeads,
		meta.NEmbd, meta.ContextLength, kvLayers, meta.KVHeadDim())
}

// MemfitSpecFromValues — сборка ModelSpec из уже известных чисел (например, из
// GGUF-заголовка, прочитанного в checkVRAMForModel: там файл повторно не читается).
// kvLayers/kvHeadDim = 0 означают «неизвестно» → memfit берёт верхнюю оценку.
func MemfitSpecFromValues(name string, sizeBytes int64, nLayers, nHeads, nKvHeads, nEmbd, trainCtx, kvLayers, kvHeadDim int) memfit.ModelSpec {
	return memfit.ModelSpec{
		Name:      name,
		SizeBytes: memfit.Bytes(sizeBytes),
		NLayers:   nLayers,
		KVLayers:  kvLayers,
		NHeads:    nHeads,
		NKvHeads:  nKvHeads,
		NEmbd:     nEmbd,
		KVHeadDim: kvHeadDim,
		TrainCtx:  trainCtx,
	}
}
