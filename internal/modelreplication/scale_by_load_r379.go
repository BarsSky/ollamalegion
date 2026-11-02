// scale_by_load_r379.go — R379 (2026-11-01): масштабирование группы по
// ЗАНЯТОСТИ, а не только по minInstances (P2-2 из аудита регрессий).
//
// ЗАЧЕМ. Контроллер группы умел только «дотянуть до minInstances» и «срезать
// выше maxInstances». Между ними он не реагировал ни на что: группа из двух
// копий при полной занятости обеих стояла и ждала, а оператор, задавший
// maxInstances=4, видел, что «запас есть, но он не используется».
//
// ПРАВИЛО. Если загруженных копий уже достаточно для minInstances, но меньше
// maxInstances И средняя загрузка загруженных копий не ниже порога — просим ОДНУ
// дополнительную копию. Загрузка приходит из балансера (SetBackendLoadFn) и
// считается как ActiveReqs / EffectiveMaxConcurrentRequests.
//
// ПОЧЕМУ БЕЗОПАСНО. Масштабирование включается ТОЛЬКО когда оператор сам задал
// maxInstances больше minInstances: в дефолтной конфигурации max == min и
// поведение не меняется вовсе. Инстанс сразу помечается Loading
// (scaleUpGroup), поэтому следующий тик не запросит вторую копию на тот же
// дефицит: в расчёте участвует loaded+inFlight.
package modelreplication

import "ollama-loadbalancer/pkg/types"

// groupSaturationLoadR379 — средняя загрузка загруженных копий, после которой
// группа считается насыщенной. 0.85: при одном слоте это «слот занят», при
// четырёх — «заняты три с лишним».
const groupSaturationLoadR379 = 0.85

// groupSaturatedR379 — насыщена ли группа по средней загрузке её загруженных
// копий. Пустая группа или отсутствие callback'а загрузки — не насыщена
// (масштабировать не по чему; minInstances-путь работает отдельно).
func (mgr *ModelGroupManager) groupSaturatedR379(g *ModelGroup) bool {
	if mgr == nil || g == nil {
		return false
	}
	mgr.mu.RLock()
	loadFn := mgr.loadFn
	mgr.mu.RUnlock()
	if loadFn == nil {
		return false
	}

	g.mu.RLock()
	ids := make([]string, 0, len(g.Instances))
	for id, inst := range g.Instances {
		if inst == nil || inst.Status != types.ModelStateLoaded {
			continue
		}
		ids = append(ids, id)
	}
	g.mu.RUnlock()
	if len(ids) == 0 {
		return false
	}

	sum := 0.0
	for _, id := range ids {
		sum += loadFn(id)
	}
	return sum/float64(len(ids)) >= groupSaturationLoadR379
}
