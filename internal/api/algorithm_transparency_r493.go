package api

// algorithm_transparency_r493.go — долг 12n: поле `algorithm` не переключает
// селектор, и это должно быть видно оператору, а не выясняться по логам.
//
// ФАКТ (проверено 2026-11-15/16). Значения roundrobin/leastconn/resource-aware/
// model-affinity проходят whitelist, сохраняются в конфиге, отдаются API и
// копируются при импорте конфига. Но НИ ОДИН продакшн-файл пакета
// internal/balancer не читает Balancing.Algorithm: выбор бэкенда один —
// многоступенчатый expandCandidates (LOADED → WARMING → FREE → FALLBACK) с
// учётом окна и текущей нагрузки. Настройка была «принимается и молча ничего не
// делает»; цена — красный CI на тесте, который ждал равномерности round-robin
// (24 вызова из 400 при пороге 30, docs/architecture/debts.ru.md 12n).
//
// РЕШЕНИЕ (этот файл): не менять поведение и не ломать конфиги — оставить
// значение принимаемым, но (а) один раз предупредить в лог при старте/смене и
// (б) отдавать в API явное `effectiveAlgorithm`, чтобы было видно, что реально
// работает. Реализация разных алгоритмов или отказ в приёме значения — отдельное
// продуктовое решение, записанное в долге.

import (
	"sync"

	"ollama-loadbalancer/pkg/logger"
)

// effectiveSelectionAlgorithmR493 — как селектор выбирает бэкенд НА САМОМ ДЕЛЕ.
// Одно значение на весь бинарь: второй «эффективный алгоритм» появиться не
// должен, иначе API снова начнёт обещать несуществующее.
const effectiveSelectionAlgorithmR493 = "unified-multistage"

// algorithmWarnSeenR493 — какие значения уже предупреждены. Карта нужна, чтобы
// не писать warn на каждый PUT конфига: оператор правит настройки пачкой, а
// сообщение всегда одно и то же.
var (
	algorithmWarnMuR493   sync.Mutex
	algorithmWarnSeenR493 = map[string]bool{}
)

// logAlgorithmIgnoredR493 — сказать в лог, что значение принято, но выбор не
// переключает. Один warn на значение за жизнь процесса.
func logAlgorithmIgnoredR493(where, value string) {
	if value == "" {
		return
	}
	algorithmWarnMuR493.Lock()
	seen := algorithmWarnSeenR493[value]
	if !seen {
		algorithmWarnSeenR493[value] = true
	}
	algorithmWarnMuR493.Unlock()
	if seen {
		return
	}
	logger.Get().Warnw("balancing algorithm accepted for compatibility, selection is unchanged",
		"where", where,
		"algorithm", value,
		"effective_algorithm", effectiveSelectionAlgorithmR493,
		"why", "selector is a single multi-stage pipeline (expandCandidates: loaded/warming/free/fallback + load and window); "+
			"the field does not switch algorithms — see docs/architecture/debts.ru.md 12n",
		"hint", "remove the field or implement per-algorithm selection; until then it only describes intent")
}

// resetAlgorithmWarnSeenR493 — для тестов: сбросить «уже предупредили».
func resetAlgorithmWarnSeenR493() {
	algorithmWarnMuR493.Lock()
	algorithmWarnSeenR493 = map[string]bool{}
	algorithmWarnMuR493.Unlock()
}
