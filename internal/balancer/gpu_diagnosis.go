// gpu_diagnosis.go — R495 (2026-11-18), шаг S1: «почему карта медленная».
//
// ЗАЧЕМ. Балансер судил о занятости GPU по `utilization.gpu` (SM busy). На декоде
// каждый токен читает все веса модели + KV-cache, поэтому предел скорости —
// полоса памяти, а занятость SM при этом падает до 40–70 % (тензорные ядра ждут
// данные). Memory-bound карта выглядела свободной: `gpuFree = 100 - SM` давал
// 30–60 %, score высокий, и на уже насыщенную карту уходили новые запросы.
//
// S1 НИЧЕГО НЕ РЕШАЕТ: здесь только публикация измеренных чисел
// (`/api/metrics` → `gpu_diagnosis`) и правил чтения. Вердикт и его влияние на
// score — S2/S3, отдельными коммитами и под флагом LB_GPU_BOTTLENECK.
package balancer

import (
	"ollama-loadbalancer/pkg/types"
)

// gpuDiagnosisSnapshot — снимок измеренных признаков «почему медленно» по
// бэкендам. Ключ — ID бэкенда.
//
// Правило R493 «числа только измеренные»: поле попадает в снимок, ТОЛЬКО если
// оно действительно измерено. Ноль не публикуется, потому что 0 у
// `mem_busy_percent` означает «драйвер не отдал» (N/A), а не «память свободна»;
// `pstate = -1` — «неизвестно»; `throttle_known = false` — «троттлинг не
// измерялся». Публикация нулей вернула бы оператора ровно к исходному дефекту,
// только теперь в панели.
func (p *Proxy) gpuDiagnosisSnapshot() map[string]map[string]interface{} {
	if p == nil || p.metricsMgr == nil {
		return nil
	}
	p.metricsMgr.mu.RLock()
	defer p.metricsMgr.mu.RUnlock()

	out := make(map[string]map[string]interface{}, len(p.metricsMgr.metrics))
	for backendID, m := range p.metricsMgr.metrics {
		if m == nil {
			continue
		}
		entry := gpuDiagnosisEntry(m.GPU)
		if len(entry) == 0 {
			continue // ни одного измеренного признака — молчим, а не печатаем нули
		}
		out[backendID] = entry
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// gpuDiagnosisEntry — измеренные поля одной карты в виде, готовом для JSON.
// Пустая map = «нечего показывать» (старый агент / нет данных NVML).
func gpuDiagnosisEntry(gpu types.GPUMetrics) map[string]interface{} {
	// Есть ли вообще R495-сигнал от этой карты. Только тогда публикуем блок:
	// у старого агента полей нет, и любой ноль в ответе был бы выдумкой.
	//
	// PState проверяется ЗДЕСЬ ЖЕ, а не отдельно: у старого агента поля нет, и
	// нулевое значение Go означает 0 = «P0», то есть карта выглядела бы
	// работающей на максимуме. Поэтому PState считаем измеренным только если
	// пришёл хоть один «новый» признак.
	measured := gpu.MemBusyPercent > 0 || gpu.ThrottleKnown ||
		gpu.PCIELinkGen > 0 || gpu.PCIELinkWidth > 0
	if !measured {
		return nil
	}

	entry := make(map[string]interface{}, 8)

	// Занятость SM — публикуем всегда: это старое, измеренное поле. Но именно
	// её одной недостаточно (см. §1.1 плана), поэтому рядом идут остальные.
	entry["usage_percent"] = gpu.UsagePercent

	if gpu.MemBusyPercent > 0 {
		entry["mem_busy_percent"] = gpu.MemBusyPercent
	}
	if gpu.PowerLimit > 0 {
		// Отношение — то, по чему S2 отличает power-bound от compute-bound.
		ratio := float64(gpu.PowerUsage) / float64(gpu.PowerLimit)
		if ratio < 0 {
			ratio = 0
		}
		entry["power_ratio"] = ratio
	}
	if gpu.PState >= 0 {
		entry["pstate"] = gpu.PState
	}
	if gpu.ThrottleKnown {
		entry["throttle_reasons"] = gpu.ThrottleReasons
		entry["throttle_known"] = true
		if len(gpu.ThrottleReasonsList) > 0 {
			entry["throttle_reasons_list"] = gpu.ThrottleReasonsList
		}
	}
	if gpu.PCIELinkGen > 0 {
		entry["pcie_link_gen"] = gpu.PCIELinkGen
	}
	if gpu.PCIELinkWidth > 0 {
		entry["pcie_link_width"] = gpu.PCIELinkWidth
	}

	// Явный признак того, измерена ли полоса памяти: без него «нет поля» и
	// «ноль» снова смешались бы у потребителя.
	entry["mem_busy_measured"] = gpu.MemBusyPercent > 0
	return entry
}
