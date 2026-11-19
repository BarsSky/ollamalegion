package balancer

// selection_order.go — R493 (2026-11-17): шов для порядка выбора бэкенда.
//
// КОНТЕКСТ. Долг 12n закрыт удалением поля `algorithm` (и `LB_ALGORITHM`): выбор
// бэкенда один — многоступенчатый `expandCandidates` (LOADED → WARMING → FREE →
// FALLBACK) с учётом окна и нагрузки. Решение владельца записано в
// types.BalancingSettings: «вычистить поле, совместимость не нужна, на будущее
// заложить реализацию алгоритма, которую рекомендуешь».
//
// ЧТО ЗДЕСЬ ЗАЛОЖЕНО. Рекомендованный вариант — `leastconn`: среди УЖЕ ДОПУЩЕННЫХ
// кандидатов первым идёт узел с меньшим числом активных запросов. Это не новый
// селектор и не замена фильтров: порядок применяется ПОСЛЕ всех проверок
// (здоровье, тип, окно, каталог модели, image-gate, fit по VRAM), поэтому
// инварианты R447/R480/R250/R304 и правила очереди остаются в силе — см. план
// `docs/plans/2026-11-17-selection-order-plan.md`.
//
// ПОЧЕМУ ФЛАГ, А НЕ «ПРОСТО ВКЛЮЧИТЬ». Изменение порядка меняет распределение
// трафика на живом стенде, а проверок на реальном профиле у нас пока нет:
// честнее дать явный переключатель с дефолтом «как было», чем тихо поменять
// поведение. Значение по умолчанию — `unified`.
//
// ФЛАГ: `LB_SELECTION_ORDER`
//
//	unified  (по умолчанию) — порядок задаёт fit по VRAM (текущее поведение);
//	leastconn               — первым идёт узел с меньшим числом активных запросов,
//	                          при равенстве сохраняется порядок fit по VRAM.
//
// Неизвестное значение = `unified` + одно предупреждение в лог (не молча: иначе
// оператор будет искать, «почему не работает»).

import (
	"os"
	"sort"
	"strings"
	"sync"

	"ollama-loadbalancer/pkg/logger"
)

// Режимы LB_SELECTION_ORDER.
const (
	selectionOrderUnifiedR493   = "unified"   // порядок = fit по VRAM (по умолчанию)
	selectionOrderLeastConnR493 = "leastconn" // + меньше активных запросов вперёд
)

var (
	selectionOrderWarnMuR493 sync.Mutex
	selectionOrderWarnedR493 = map[string]bool{}
	selectionOrderEnvKeyR493 = "LB_SELECTION_ORDER"
)

// selectionOrderModeR493 — действующий режим порядка выбора ("" → unified).
func selectionOrderModeR493() string {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(selectionOrderEnvKeyR493)))
	switch raw {
	case "", selectionOrderUnifiedR493:
		return selectionOrderUnifiedR493
	case selectionOrderLeastConnR493:
		return selectionOrderLeastConnR493
	default:
		selectionOrderWarnOnceR493(raw)
		return selectionOrderUnifiedR493
	}
}

// selectionOrderWarnOnceR493 — одно предупреждение на значение за процесс.
func selectionOrderWarnOnceR493(value string) {
	selectionOrderWarnMuR493.Lock()
	seen := selectionOrderWarnedR493[value]
	selectionOrderWarnedR493[value] = true
	selectionOrderWarnMuR493.Unlock()
	if seen {
		return
	}
	logger.Get().Warnw("unknown LB_SELECTION_ORDER value, using unified order",
		"value", value,
		"valid", selectionOrderUnifiedR493+"|"+selectionOrderLeastConnR493,
		"hint", "см. docs/plans/2026-11-17-selection-order-plan.md")
}

// resetSelectionOrderWarningsR493 — для тестов.
func resetSelectionOrderWarningsR493() {
	selectionOrderWarnMuR493.Lock()
	selectionOrderWarnedR493 = map[string]bool{}
	selectionOrderWarnMuR493.Unlock()
}

// orderCandidatesForSelectionR493 — порядок обхода кандидатов под загрузку модели.
//
// Применяется в `pickLoadableFreeBackendR493` ПОСЛЕ того, как кандидаты уже
// отфильтрованы по группам P3/P4 (тип, здоровье, выключенность, донор, каталог),
// и до проверок слота/image-gate. Поэтому функция МОЖЕТ только переставить
// элементы: длина и состав обязаны совпадать с входом (это проверяется тестом).
func (p *Proxy) orderCandidatesForSelectionR493(candidates []string, model string) []string {
	base := p.orderCandidatesByVramFit(candidates, model)
	if selectionOrderModeR493() != selectionOrderLeastConnR493 {
		return base
	}
	if len(base) < 2 {
		return base
	}
	type rank struct {
		id      string
		active  int
		origIdx int
	}
	ranks := make([]rank, 0, len(base))
	for i, id := range base {
		ranks = append(ranks, rank{id: id, active: p.activeRequestsR493(id), origIdx: i})
	}
	sort.SliceStable(ranks, func(i, j int) bool {
		if ranks[i].active != ranks[j].active {
			return ranks[i].active < ranks[j].active
		}
		return ranks[i].origIdx < ranks[j].origIdx
	})
	out := make([]string, 0, len(ranks))
	for _, r := range ranks {
		out = append(out, r.id)
	}
	if changedOrder(base, out) {
		logger.Get().Infow("selection order: leastconn (fewer active requests first)",
			"model", model,
			"order", out,
			"note", "порядок меняется только внутри уже допущенных кандидатов")
	}
	return out
}

// activeRequestsR493 — сколько запросов узел обслуживает прямо сейчас.
// nil-safe: неизвестный бэкенд считается свободным (он и так не попадёт в обход).
func (p *Proxy) activeRequestsR493(backendID string) int {
	if p == nil {
		return 0
	}
	p.mu.RLock()
	state := p.backends[backendID]
	p.mu.RUnlock()
	if state == nil {
		return 0
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.ActiveReqs
}
