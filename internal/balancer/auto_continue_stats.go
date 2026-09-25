// auto_continue_stats.go — R83 (2026-09-25): счётчики авто-продолжения (D4).
//
// ЗАЧЕМ. До этой правки единственным следом работы защиты от дубликата
// (авто-продолжение + подавление перегенерации) были СТРОКИ В ЛОГЕ: «suppressing
// duplicate» или «auto-continue succeeded». Оператор не мог ответить на вопрос
// «сколько раз ответ дублировался/был подавлен», а именно это нужно, чтобы
// понять, работает ли фикс на живом трафике.
//
// Счётчики атомарные и неблокирующие (на горячем пути только Add(1)), отдаются
// сводкой в GET /api/v1/metrics рядом с блоком placement — по тому же образцу
// (PlacementMetricsSummary).
package balancer

import (
	"sync/atomic"
)

// autoContinueStats — процесс-глобальные счётчики. Отдельный объект, а не поля
// Proxy: значения одинаковы для всех прокси и не участвуют в его блокировках.
type autoContinueCounters struct {
	// suppressed — перегенерация распознана и НЕ отправлена клиенту (это и есть
	// «спасённый» дубль).
	suppressed atomic.Int64
	// emitted — продолжение реально отправлено клиенту.
	emitted atomic.Int64
	// failed — continue-запрос не удался (клиент получит обрезанный ответ как есть).
	failed atomic.Int64
	// policyDisabled — политика выключила подавление (LB_AUTO_CONTINUE_CHAT_POLICY
	// = always/never), поэтому перегенерации не отсекаются. Отдельный счётчик,
	// потому что это конфигурационный выбор оператора, а не сбой.
	policyDisabled atomic.Int64
}

var autoContinueStats autoContinueCounters

func recordAutoContinueSuppressed()     { autoContinueStats.suppressed.Add(1) }
func recordAutoContinueEmitted()        { autoContinueStats.emitted.Add(1) }
func recordAutoContinueFailed()         { autoContinueStats.failed.Add(1) }
func recordAutoContinuePolicyDisabled() { autoContinueStats.policyDisabled.Add(1) }

// AutoContinueMetricsSummary — сводка для GET /api/v1/metrics.
//
// `duplicateRisk` — прямая подсказка оператору: сколько раз продолжение ушло
// клиенту, хотя похоже на перегенерацию (не ноль при policy=always означает, что
// дубли возможны и это следствие конфигурации).
func (p *Proxy) AutoContinueMetricsSummary() map[string]interface{} {
	suppressed := autoContinueStats.suppressed.Load()
	emitted := autoContinueStats.emitted.Load()
	failed := autoContinueStats.failed.Load()
	policyDisabled := autoContinueStats.policyDisabled.Load()

	summary := map[string]interface{}{
		"enabled":        IsAutoContinueOnTruncationEnabled(),
		"policy":         GetAutoContinueChatPolicy(),
		"suppressed":     suppressed,
		"emitted":        emitted,
		"failed":         failed,
		"policyDisabled": policyDisabled,
	}
	// Доля подавлений среди распознанных случаев — понятнее, чем два счётчика.
	if total := suppressed + emitted; total > 0 {
		summary["suppressedRatio"] = float64(suppressed) / float64(total)
	} else {
		summary["suppressedRatio"] = 0.0
	}
	// Риск дубля: продолжения уходили клиенту при выключенном подавлении.
	summary["duplicateRisk"] = policyDisabled > 0
	return summary
}
