// memfit_breakdown_r115.go — R115: отказ по памяти обязан показывать раскладку.
//
// ЧТО БЫЛО. Гейт n_ctx отказывал числом: «physically feasible maximum 0,
// max_vram_n_ctx=0, max_ram_n_ctx=0». Оператор не мог понять, ЧЕГО не хватило:
// весов или KV, локальной VRAM или памяти RPC-узла, RAM или лимита cgroup.
// Диагностика упиралась в чтение логов и пересчёт в уме.
//
// ЧТО ТЕПЕРЬ. Тот же вердикт memfit (никакой второй формулы — это требование
// владельца проекта: «не заплатка, должно быть определяемо по правилам»)
// раскладывается по статьям расхода и уходит в 422 вместе с текстом.
//
// ПРАВИЛО, ПО КОТОРОМУ СЧИТАЕТСЯ РАСКЛАДКА (её и печатаем):
//   - VRAM: свободно локально + свободно на RPC-узлах против весов и KV
//     GPU-слоёв (budget.VRAMFree уже включает RPC — см. MemfitBudget);
//   - RAM: доступно (env → /proc/meminfo → cgroup) минус резерв против весов и
//     KV CPU-слоёв; при use_mmap веса CPU вытесняемы, KV — нет;
//   - потолки n_ctx: MaxExactFitCtx = всё в VRAM, MaxHardCtx = с выгрузкой.
//
// Никакие значения здесь не досчитываются «примерно»: если memfit чего-то не
// знает (VRAM/RAM/cgroup-лимит), поле остаётся нулевым, а Known — false. Ноль в
// отчёте означает «неизвестно», а не «нет памяти», и текст это проговаривает.
package main

import (
	"fmt"
	"strings"

	"ollama-loadbalancer/internal/memfit"
)

// memoryBreakdown — вердикт memfit, разложенный по статьям расхода.
type memoryBreakdown struct {
	VRAMFreeMB    int64
	VRAMTotalMB   int64
	VRAMKnown     bool
	RPCDevices    int
	RAMAvailMB    int64
	RAMTotalMB    int64
	RAMLimitMB    int64
	RAMLimitKnown bool
	RAMSource     string

	WeightsMB    int64
	GPUWeightsMB int64
	CPUWeightsMB int64
	GPUKVMB      int64
	CPUKVMB      int64
	UsableVRAMMB int64
	UsableRAMMB  int64

	GPULayers   int
	TotalLayers int
	KVPerToken  int
	Stage       string
}

// buildMemoryBreakdown переносит числа вердикта в отчёт. Ничего не вычисляет
// заново: любое расхождение между решением и отчётом было бы дефектом, поэтому
// здесь только присваивания.
func buildMemoryBreakdown(budget memfit.Budget, v memfit.Verdict, rpcDevices int) memoryBreakdown {
	return memoryBreakdown{
		VRAMFreeMB:    budget.VRAMFree.MiB(),
		VRAMTotalMB:   budget.VRAMTotal.MiB(),
		VRAMKnown:     budget.VRAMKnown,
		RPCDevices:    rpcDevices,
		RAMAvailMB:    budget.RAMAvail.MiB(),
		RAMTotalMB:    budget.RAMTotal.MiB(),
		RAMLimitMB:    budget.RAMLimit.MiB(),
		RAMLimitKnown: budget.RAMLimitKnown,
		RAMSource:     budget.Source,
		WeightsMB:     v.Weights.MiB(),
		GPUWeightsMB:  v.GPUWeights.MiB(),
		CPUWeightsMB:  v.CPUWeights.MiB(),
		GPUKVMB:       v.GPUKV.MiB(),
		CPUKVMB:       v.CPUKV.MiB(),
		UsableVRAMMB:  v.UsableVRAM.MiB(),
		UsableRAMMB:   v.UsableRAM.MiB(),
		GPULayers:     v.GPULayers,
		TotalLayers:   v.TotalLayers,
		KVPerToken:    int(v.KVPerToken),
		Stage:         string(v.Stage),
	}
}

// Fields — машинночитаемая раскладка для тела 422 (и для load_failures).
// Ключи с суффиксом _mb всегда в мебибайтах; zero означает «неизвестно», и это
// явно передаётся парными *_known.
func (m memoryBreakdown) Fields() map[string]interface{} {
	return map[string]interface{}{
		"vram_free_mb":    m.VRAMFreeMB,
		"vram_total_mb":   m.VRAMTotalMB,
		"vram_known":      m.VRAMKnown,
		"rpc_devices":     m.RPCDevices,
		"ram_avail_mb":    m.RAMAvailMB,
		"ram_total_mb":    m.RAMTotalMB,
		"ram_limit_mb":    m.RAMLimitMB,
		"ram_limit_known": m.RAMLimitKnown,
		"ram_source":      m.RAMSource,
		"weights_mb":      m.WeightsMB,
		"gpu_weights_mb":  m.GPUWeightsMB,
		"cpu_weights_mb":  m.CPUWeightsMB,
		"gpu_kv_mb":       m.GPUKVMB,
		"cpu_kv_mb":       m.CPUKVMB,
		"usable_vram_mb":  m.UsableVRAMMB,
		"usable_ram_mb":   m.UsableRAMMB,
		"gpu_layers":      m.GPULayers,
		"total_layers":    m.TotalLayers,
		"kv_per_token":    m.KVPerToken,
		"stage":           m.Stage,
	}
}

// breakdownFieldsOrNil — раскладка для карт логов/провалов. nil-безопасна: у
// вердикта без бюджета раскладки нет, и подставлять туда нули нельзя (ноль в
// отчёте означает «неизвестно», а не «нет памяти»).
func breakdownFieldsOrNil(b *memoryBreakdown) interface{} {
	if b == nil {
		return nil
	}
	return b.Fields()
}

// Text — та же раскладка одной строкой для "error" и лога. Числа идут в том же
// порядке, что и статьи расхода, чтобы строка читалась как счёт, а не как
// перечисление.
func (m memoryBreakdown) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "memory breakdown: VRAM free %d MB of %d MB", m.VRAMFreeMB, m.VRAMTotalMB)
	if !m.VRAMKnown {
		b.WriteString(" (unknown)")
	}
	if m.RPCDevices > 0 {
		fmt.Fprintf(&b, " including %d RPC node(s)", m.RPCDevices)
	}
	fmt.Fprintf(&b, "; RAM available %d MB of %d MB", m.RAMAvailMB, m.RAMTotalMB)
	if m.RAMLimitKnown {
		fmt.Fprintf(&b, " (cgroup headroom %d MB)", m.RAMLimitMB)
	} else {
		b.WriteString(" (cgroup limit unknown)")
	}
	if m.RAMSource != "" {
		fmt.Fprintf(&b, " via %s", m.RAMSource)
	}
	// R218: kv_per_token приходит в БАЙТАХ, а не в мегабайтах. В живом отказе
	// печаталось «at 65536 MB/token», что читается как абсурд и мешает оператору
	// считать бюджет (65536 байт/токен = 64 KB/токен). Показываем обе единицы.
	fmt.Fprintf(&b, "; weights %d MB (GPU %d + CPU %d), KV %d MB (GPU %d + CPU %d) at %d bytes/token (%.0f KB/token)",
		m.WeightsMB, m.GPUWeightsMB, m.CPUWeightsMB,
		m.GPUKVMB+m.CPUKVMB, m.GPUKVMB, m.CPUKVMB, m.KVPerToken, float64(m.KVPerToken)/1024.0)
	fmt.Fprintf(&b, "; usable for KV: VRAM %d MB, RAM %d MB; plan gpu_layers %d/%d, stage %s",
		m.UsableVRAMMB, m.UsableRAMMB, m.GPULayers, m.TotalLayers, m.Stage)
	return b.String()
}
