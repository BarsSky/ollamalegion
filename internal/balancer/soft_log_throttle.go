// soft_log_throttle.go — R488 (2026-11-11): мягкие пороги не должны
// вытеснять из кольца логов то, ради чего архив открывают.
//
// ЖИВОЙ ФАКТ (архив второго стенда 2026-11-11): balancer-logs.txt — 2000 строк
// кольца, из них 733 — «backend CPU занят (soft, не блокирует): … запрос встанет
// в очередь» и столько же «VRAM usage above threshold». Настоящих строк
// `request trace` в окне — ОДНА. То есть разбор «почему запрос встал в очередь и
// кто кого отменил» упирался в отсутствие данных, а не в их сложность.
//
// Правило: мягкое наблюдение («занят, но не блокирует») — не событие хода. Оно
// печатается не чаще одного раза в softLogEveryR488 на (бэкенд, вид), а не на
// каждый проход планировщика.
package balancer

import (
	"sync"
	"time"
)

// softLogEveryR488 — период, чаще которого одно и то же мягкое наблюдение по
// одному бэкенду в кольцо не пишется. 60 с: достаточно, чтобы оператор увидел
// устойчивое состояние, и мало, чтобы оно съело кольцо.
const softLogEveryR488 = 60 * time.Second

// softLogEveryVramR493 — отдельный период для «VRAM выше порога».
//
// ЖИВОЙ ФАКТ (второй стенд, 2026-11-16): при рабочем режиме 83–89 % VRAM и
// пороге 85 % строка «backend VRAM usage above threshold (degraded, not
// blocked)» заняла 276 из 2000 строк кольца за 2 ч 54 мин — треть всего лога.
// Это не ошибка, а устойчивое состояние двух A10 с загруженной 27B-моделью;
// при 60 с на бэкенд шум вытесняет настоящие предупреждения. 10 минут
// достаточно, чтобы устойчивое состояние было видно, и мало, чтобы съесть кольцо
// (≈35 строк за те же 2 ч 54 мин).
const softLogEveryVramR493 = 10 * time.Minute

// softLogThrottleR488 — состояние троттлинга (ключ "backend|kind").
var softLogThrottleR488 = struct {
	last map[string]time.Time
	mu   sync.Mutex
}{last: make(map[string]time.Time)}

// softLogAllowedR488 — можно ли сейчас печатать это мягкое наблюдение.
func softLogAllowedR488(backend, kind string, now time.Time, every time.Duration) bool {
	if every <= 0 {
		return true
	}
	key := backend + "|" + kind
	softLogThrottleR488.mu.Lock()
	defer softLogThrottleR488.mu.Unlock()
	if ts, ok := softLogThrottleR488.last[key]; ok && now.Sub(ts) < every {
		return false
	}
	softLogThrottleR488.last[key] = now
	return true
}

// resetSoftLogThrottleR488 — только для тестов.
func resetSoftLogThrottleR488() {
	softLogThrottleR488.mu.Lock()
	defer softLogThrottleR488.mu.Unlock()
	softLogThrottleR488.last = make(map[string]time.Time)
}
