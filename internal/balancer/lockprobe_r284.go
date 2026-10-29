package balancer

// lockprobe_r284.go — R284 (2026-10-29): честный ответ «жив ли балансер».
//
// ЖИВОЙ ДЕФЕКТ (стенд r284). Под нагрузкой агентских метрик балансер уходил в
// дедлок на p.mu (рекурсивный RLock, см. lock_deadlock_r284_test.go), но
// `docker ps` продолжал показывать `healthy`: healthcheck ходит на
// /api/v1/ping, а тот p.mu не трогает и отвечает 200 даже когда ни один
// HTTP-обработчик, которому нужен кластер, не может получить лок. Для оператора
// это худший вид отказа — «контейнер здоров, панель мертва».
//
// РЕШЕНИЕ: проба «лок берётся за отведённое время». Один и тот же приём
// используется в /api/v1/ping (liveness, docker healthcheck) и /api/v1/health
// (readiness): если лок не взят — 503 с reason=lock_stuck.
//
// ПОЧЕМУ НЕ TryRLock. В дедлоке писатель ЖДЁТ, а читатель держит RLock:
// sync.RWMutex.TryRLock в такой ситуации может выдать блокировку (в Go 1.21 он
// проверяет только «не держит ли писатель»), то есть проба соврала бы «всё
// хорошо». Поэтому берём настоящий RLock в отдельной горутине и ждём по таймауту:
// если лок реально не выдаётся — узнаем это.
//
// ПРО УТЕЧКУ. При зависании горутина-проба останется ждать лок (отменить RLock
// нельзя). Поэтому пробы СЕРИАЛИЗОВАНЫ: пока предыдущая не завершилась, новые
// не порождаются и сразу получают «занято» — иначе healthcheck раз в 10 секунд
// плодил бы горутины. Когда дедлок разрешится (или процесс перезапустят),
// зависшая проба завершится сама.

import (
	"sync/atomic"
	"time"
)

// lockProbeInFlight — проба уже ждёт лок; новые пробы не порождаем.
var lockProbeInFlight atomic.Bool

// ProbeLocksHealthy — берётся ли p.mu за budget.
//
// Возвращает (true, ожидание) если лок взят; (false, ожидание) если за budget не
// взят (дедлок или предельно долгая операция под локом).
func (p *Proxy) ProbeLocksHealthy(budget time.Duration) (bool, time.Duration) {
	if p == nil {
		return true, 0
	}
	if budget <= 0 {
		budget = time.Second
	}

	// Уже есть незавершённая проба — не плодим горутины: считаем, что лок не
	// берётся (именно это предыдущая проба и выясняет).
	if !lockProbeInFlight.CompareAndSwap(false, true) {
		return false, 0
	}

	start := time.Now()
	done := make(chan struct{})
	go func() {
		defer lockProbeInFlight.Store(false)
		p.mu.RLock()
		p.mu.RUnlock()
		close(done)
	}()

	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case <-done:
		return true, time.Since(start)
	case <-timer.C:
		return false, time.Since(start)
	}
}
