// backend_selector_vram_r248.go — R248 (2026-10-27): выбор бэкенда для ЗАГРУЗКИ
// тяжёлой модели по свободной VRAM.
//
// ЖИВОЙ ДЕФЕКТ. Qwen3.8-27B (≈15 ГБ) уходил на узел с 8 ГБ, хотя рядом был
// свободный узел на 20 ГБ. Причина: этап P3 (Sync Model Load,
// backend_selector.go) брал ПЕРВЫЙ кандидат, у которого есть слот
// (canAcceptRequest), и вообще не смотрел ни на размер модели, ни на свободную
// VRAM узла. Дальше cppworker сам «спасался» RAM-fallback'ом (часть слоёв на
// CPU), и оператор видел тяжёлую модель на маленькой карте.
//
// РЕШЕНИЕ. Порядок кандидатов на загрузку сортируется по пригодности:
//   1) узлы, где свободная VRAM >= размер модели (с запасом 5%);
//   2) остальные — по убыванию свободной VRAM (наименее плохой вариант).
// Узлы без метрик VRAM (0/неизвестно) НЕ поднимаются наверх и не топят
// известные: при полном отсутствии данных сохраняется исходный порядок
// (честная деградация вместо выдумки).
package balancer

import (
	"sort"

	"ollama-loadbalancer/pkg/logger"
)

// vramFitSlack — запас над размером файла модели: в VRAM уезжают не только
// веса (KV-cache, буферы контекста), поэтому «ровно впритык» считать пригодным
// нельзя. 5% — оценка без гадания о конкретных n_ctx/квантизации KV.
const vramFitSlack = 1.05

// backendVramFreeMB — свободная VRAM узла по последним метрикам (0 = нет данных).
//
// Источник — LlamaCppMetrics.AvailableVRAMMB (R115: это ёмкость, доступная
// модели, включая узлы раскладки) вместе с флагом VramKnown: без него 0
// означал бы «неизвестно» и «не влезает» одновременно — ровно та
// неоднозначность, из-за которой нельзя честно сортировать.
func (p *Proxy) backendVramFreeMB(backendID string) (freeMB int, known bool) {
	if p.metricsMgr == nil {
		return 0, false
	}
	p.metricsMgr.mu.RLock()
	defer p.metricsMgr.mu.RUnlock()
	lm := p.metricsMgr.llamaMetrics[backendID]
	if lm == nil || !lm.VramKnown || lm.AvailableVRAMMB == 0 {
		return 0, false
	}
	return int(lm.AvailableVRAMMB), true
}

// orderCandidatesByVramFit сортирует кандидатов на загрузку модели.
//
// Порядок:
//   - пригодные (free >= size*1.05): между собой — по убыванию свободной VRAM;
//   - непригодные с известной VRAM: по убыванию свободной VRAM;
//   - узлы без метрик: в конце, сохраняя исходный относительный порядок.
//
// Если размер модели неизвестен (0) — возвращается исходный порядок: без
// размера сортировка была бы догадкой.
func (p *Proxy) orderCandidatesByVramFit(backendIDs []string, model string) []string {
	if len(backendIDs) < 2 {
		return backendIDs
	}
	sizeBytes := p.getModelSizeBytes(model)
	if sizeBytes <= 0 {
		return backendIDs
	}
	needMB := float64(sizeBytes) / (1024 * 1024) * vramFitSlack

	type rank struct {
		id      string
		known   bool
		fits    bool
		freeMB  int
		origIdx int
	}
	ranks := make([]rank, 0, len(backendIDs))
	for i, id := range backendIDs {
		freeMB, known := p.backendVramFreeMB(id)
		ranks = append(ranks, rank{
			id:      id,
			known:   known,
			fits:    known && float64(freeMB) >= needMB,
			freeMB:  freeMB,
			origIdx: i,
		})
	}
	sort.SliceStable(ranks, func(i, j int) bool {
		a, b := ranks[i], ranks[j]
		if a.fits != b.fits {
			return a.fits
		}
		if a.known != b.known {
			return a.known
		}
		if a.known && b.known && a.freeMB != b.freeMB {
			return a.freeMB > b.freeMB
		}
		return a.origIdx < b.origIdx
	})

	out := make([]string, 0, len(ranks))
	for _, r := range ranks {
		out = append(out, r.id)
	}
	// Логируем ТОЛЬКО когда порядок реально изменился: иначе лог превращается
	// в шум на каждый запрос.
	if changedOrder(backendIDs, out) {
		logger.Get().Infow("model load: candidates ordered by VRAM fit",
			"model", model,
			"model_size_mb", int(float64(sizeBytes)/(1024*1024)),
			"need_mb", int(needMB),
			"order", out)
	}
	return out
}

// changedOrder — true, если порядок отличается от исходного.
func changedOrder(orig, sorted []string) bool {
	if len(orig) != len(sorted) {
		return true
	}
	for i := range orig {
		if orig[i] != sorted[i] {
			return true
		}
	}
	return false
}
