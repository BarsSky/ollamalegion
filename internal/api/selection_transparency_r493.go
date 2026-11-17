package api

// selection_transparency_r493.go — долг 12n: «алгоритм» больше не настройка.
//
// ИСТОРИЯ. Поле `balancing.algorithm` (и `LB_ALGORITHM`) принималось, сохранялось
// в конфиге, отдавалось API и копировалось при импорте. Но НИ ОДИН продакшн-файл
// пакета internal/balancer это поле не читал: выбор бэкенда один —
// многоступенчатый `expandCandidates` (LOADED → WARMING → FREE → FALLBACK) с
// учётом окна и текущей нагрузки. Настройка была «принимается и молча ничего не
// делает»; цена — красный CI на тесте, который ждал равномерности round-robin
// (24 вызова из 400 при пороге 30), и вопросы операторов «почему не работает».
//
// СНАЧАЛА (R493, 2026-11-16) сделали прозрачность: одно предупреждение в лог на
// значение + `effectiveAlgorithm` в API. Этого оказалось недостаточно: WebUI
// предлагал ПЯТЬ вариантов, из которых три (`least-connections`, `round-robin`,
// `weighted`) сервер отвергал с 400, а два принимаемых имени в списке
// отсутствовали — то есть интерфейс обещал выбор, которого нет.
//
// РЕШЕНИЕ ВЛАДЕЛЬЦА (2026-11-17): поле удалить полностью, совместимость со
// старыми конфигами не нужна (обновляется всё сразу). Сделано:
//   - поля `Balancing.Algorithm` в types больше нет, `LB_ALGORITHM` не читается;
//   - `PUT /api/v1/cluster/config` с полем → 400 `algorithm_removed`;
//   - из GET-ответов, экспорта/импорта, диагностики, WebUI и доков поле убрано;
//   - в API остаётся `effectiveAlgorithm` — ФАКТ выбора, а не намерение.
//
// Что осталось на будущее (сознательно НЕ включено): реализация порядка выбора
// (`selection order`) с рекомендуемым `leastconn`. План и инварианты —
// `docs/plans/2026-11-17-selection-order-plan.md`; шов для него уже есть
// (`pickBetterCandidateR493`, применяется как тай-брейк при равном счёте).

import (
	"sync"

	"ollama-loadbalancer/pkg/logger"
)

// effectiveSelectionAlgorithmR493 — как селектор выбирает бэкенд НА САМОМ ДЕЛЕ.
// Одно значение на весь бинарь: второй «эффективный алгоритм» появиться не
// должен, иначе API снова начнёт обещать несуществующее.
const effectiveSelectionAlgorithmR493 = "unified-multistage"

// algorithmWarnSeenR493 — какие значения уже предупреждены (остатки
// `LB_ALGORITHM` в окружении). Карта нужна, чтобы не писать warn на каждый
// запуск одного и того же стенда с одним и тем же значением.
var (
	algorithmWarnMuR493   sync.Mutex
	algorithmWarnSeenR493 = map[string]bool{}
)

// warnAlgorithmFieldRemovedR493 — сказать в лог, что настройка удалена.
//
// ЗАЧЕМ ВООБЩЕ ПРЕДУПРЕЖДАТЬ, ЕСЛИ СОВМЕСТИМОСТЬ НЕ НУЖНА. Удаление — это
// отсутствие эффекта, а не отсутствие факта: у оператора в `.env` может остаться
// строка, и он будет искать, «почему не переключается». Одно сообщение закрывает
// этот вопрос, не притворяясь работающей настройкой.
func warnAlgorithmFieldRemovedR493(where, value string) {
	if value == "" {
		return
	}
	algorithmWarnMuR493.Lock()
	seen := algorithmWarnSeenR493[where+":"+value]
	if !seen {
		algorithmWarnSeenR493[where+":"+value] = true
	}
	algorithmWarnMuR493.Unlock()
	if seen {
		return
	}
	logger.Get().Warnw("algorithm setting is removed and has no effect: the selector is single (unified-multistage)",
		"where", where,
		"value", value,
		"effective_selection", effectiveSelectionAlgorithmR493,
		"why", "выбор бэкенда один: expandCandidates (loaded/warming/free/fallback) + нагрузка и окно; "+
			"поля balancing.algorithm и LB_ALGORITHM больше нет",
		"hint", "удалите переменную/ключ; см. docs/architecture/balancing-guide.md §2.1 и debts.ru.md 12n")
}

// resetAlgorithmWarnSeenR493 — для тестов: сбросить «уже предупредили».
func resetAlgorithmWarnSeenR493() {
	algorithmWarnMuR493.Lock()
	algorithmWarnSeenR493 = map[string]bool{}
	algorithmWarnMuR493.Unlock()
}
