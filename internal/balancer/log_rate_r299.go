package balancer

// log_rate_r299.go — R299 (2026-10-29): не спамить одинаковыми предупреждениями.
//
// ЖИВОЙ ФАКТ (стенд, прогон 793db393). При недостижимом доноре балансер писал
// `EnsureRPCLayout: не удалось применить раскладку` каждые 20 секунд (тик гарда),
// а селектор — сотни строк `no available backends with exclusions` за один прогон
// (299 строк). Оба сообщения полезны, но в таком количестве лог перестаёт
// читаться: оператор прокручивает страницы одинаковых строк и не видит того
// единственного события, ради которого лог открывают.
//
// ЧТО ЗДЕСЬ. Простое ограничение частоты: одно сообщение на ключ не чаще, чем
// раз в период. Ключ — то, что делает сообщение ОДНИМ И ТЕМ ЖЕ событием
// (например «бэкенд X не применил раскладку»). Смена состояния даёт новое
// сообщение, потому что меняется ключ или истекает окно.
//
// ЧЕГО ЗДЕСЬ НЕТ И БЫТЬ НЕ ДОЛЖНО. Подавления ошибок: первое сообщение всегда
// проходит, а подавленные считаются (см. dropped), чтобы при следующем
// сообщении было видно «а ещё было N таких же».

import (
	"sync"
	"time"
)

// logRateLimiter — ограничитель частоты лог-сообщений по ключу.
//
// zero value не пригоден: период и карта создаются конструктором
// newLogRateLimiter. nil-получатель безопасен (allow всегда true) — так
// вызывающему коду не нужно проверять, инициализирован ли ограничитель.
type logRateLimiter struct {
	every time.Duration // минимальный интервал между сообщениями с одним ключом
	now   func() time.Time

	mu      sync.Mutex
	last    map[string]time.Time
	dropped map[string]int
}

// newLogRateLimiter — ограничитель с периодом every (<= 0 → 1 минута).
func newLogRateLimiter(every time.Duration) *logRateLimiter {
	if every <= 0 {
		every = time.Minute
	}
	return &logRateLimiter{
		every:   every,
		now:     time.Now,
		last:    map[string]time.Time{},
		dropped: map[string]int{},
	}
}

// allow — можно ли писать сообщение с этим ключом сейчас.
//
// Возвращает (true, dropped): сообщение писать можно, а dropped — сколько
// таких же было подавлено с прошлого раза (0, если нечего показывать).
func (l *logRateLimiter) allow(key string) (bool, int) {
	if l == nil {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if l.last == nil {
		l.last = map[string]time.Time{}
	}
	if l.dropped == nil {
		l.dropped = map[string]int{}
	}
	if prev, ok := l.last[key]; ok && now.Sub(prev) < l.every {
		l.dropped[key]++
		return false, 0
	}
	dropped := l.dropped[key]
	delete(l.dropped, key)
	l.last[key] = now
	return true, dropped
}

// reset — снять отметку по ключу (состояние изменилось: следующее сообщение
// снова интересно). Нужен там, где «пропуск» сменился успехом.
func (l *logRateLimiter) reset(key string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.last, key)
	delete(l.dropped, key)
}

// Пакетные ограничители: одни и те же на весь процесс, чтобы разные пути
// (гард раскладки, селектор) не заводили каждый свой счётчик.
var (
	// Раскладка на недоступный узел: тик 20 с, поэтому 5 минут — это «одно
	// сообщение на эпизод», а не на тик.
	layoutFailLogR299 = newLogRateLimiter(5 * time.Minute)
	// «Нет кандидатов с исключениями»: бывает серией в пределах одного запроса
	// (retry-цикл), поэтому минута.
	noBackendLogR299 = newLogRateLimiter(time.Minute)
)
