// Package balancer — R98 (S8 плана 2026-10-07): видимость image-запросов в
// `/api/v1/queue/stats`.
//
// ПРОБЛЕМА (зафиксирована в плане §2 как известный пробел). Счётчики
// image-запросов (image_request_metrics.go) существуют с R-Image Phase 8 и
// отдаются в `/api/v1/metrics` в блоке `image`. Но страница очереди
// (`/api/v1/queue/stats`) их не показывала вовсе: оператор открывал «Очередь»,
// видел нули текстового пути и не понимал, что GPU занят двумя долгими
// генерациями. Это не косметика: `queue/stats` — то место, куда смотрят при
// жалобе «всё встало».
//
// ЧТО ДОБАВЛЯЕТСЯ. Блок `image` в ответе очереди:
//   - агрегаты пула (in_flight/total/ok/failed/rejected/rps/длительности) —
//     те же ключи, что в `/api/v1/metrics` (одна функция
//     `imageRequestStore.aggregateMap`, чтобы подписи не разъезжались);
//   - `backends_in_flight` — по какому узлу сколько генераций идёт: без этого
//     видно только суммарное «в полёте» и непонятно, кого ждать;
//   - `recent` — общая лента последних запросов (у каждого свой id, статус и
//     модель), то есть «каждое видно в статистике».
//
// КОГДА БЛОКА НЕТ. Если image-бэкендов не зарегистрировано и трафика не было,
// блок не добавляется: нулевой image-блок на текстовом стенде — шум, который
// заставляет думать, что image-путь вообще задействован.
package balancer

import (
	"sort"

	"ollama-loadbalancer/pkg/types"
)

// imageQueueStatsR98 — срез image-нагрузки для /api/v1/queue/stats (nil = нечего показывать).
func (p *Proxy) imageQueueStatsR98() map[string]interface{} {
	if p == nil || p.imageRes == nil {
		return nil
	}
	store := p.imageRes.imageRequests()
	if store == nil {
		return nil
	}

	ids := p.imageBackendIDsR98()
	agg := store.aggregate()
	out := store.aggregateMap(imageRecentLimitGlobal)
	if out == nil || agg == nil {
		return nil
	}
	// Решение «показывать ли блок» принимаем по ТИПИЗИРОВАННОМУ агрегату, а не по
	// значениям map: в map `in_flight` лежит как int, а `total` — как int64
	// (см. imageRequestMetricsMap), и type-assertion на int64 молча дал бы 0.
	if len(ids) == 0 && agg.Total == 0 && agg.InFlight == 0 {
		// Image-путь на этом стенде не используется — не засоряем очередь.
		return nil
	}

	if len(ids) > 0 {
		out["backends"] = ids
		perBackend := map[string]int64{}
		for _, id := range ids {
			if m := store.snapshot(id); m != nil && m.InFlight > 0 {
				perBackend[id] = int64(m.InFlight)
			}
		}
		if len(perBackend) > 0 {
			out["backends_in_flight"] = perBackend
		}
	}
	return out
}

// imageBackendIDsR98 — id зарегистрированных image-бэкендов (детерминированный
// порядок: страница очереди не должна «прыгать» между обновлениями).
func (p *Proxy) imageBackendIDsR98() []string {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	ids := make([]string, 0, 4)
	for id, state := range p.backends {
		if state == nil || state.Backend == nil {
			continue
		}
		if normalizeBackendType(state.Backend.Type) != types.BackendTypeImage {
			continue
		}
		ids = append(ids, id)
	}
	p.mu.RUnlock()
	sort.Strings(ids)
	return ids
}
