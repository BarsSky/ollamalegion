package balancer

// R528 (2026-11-18): сторож контракта WaitStopped для фоновой идентичности.
//
// ЗАЧЕМ. `pollAll` переспрашивает идентичность воркера ОТДЕЛЬНОЙ горутиной
// (`refreshWorkerIdentityR493`) и не ждал её ни в цикле, ни в `WaitStopped`.
// Итог: после `Stop()+WaitStopped()` поздняя запись ещё шла — и читатель,
// бравший снимок бэкенда без `state.mu` (image_lock_followup_test.go:191),
// получал data race. CI поймал это на -race:
//
//	Read at ... TestGPUIndexLock_ExplicitGPUZeroConflictsWithItself
//	Previous write at ... setNonEmptyR493 <- refreshWorkerIdentityR493
//
// Здесь проверяется именно контракт: WaitStopped НЕ возвращает true, пока
// фоновая запись идентичности жива, и возвращает true сразу после её конца.

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestR528_WaitStoppedWaitsIdentityRefresh — ожидание идентичности входит в
// контракт «после Stop() состояние неподвижно».
func TestR528_WaitStoppedWaitsIdentityRefresh(t *testing.T) {
	p := &llamaCppMetricsPoller{stopCh: make(chan struct{})}
	release := make(chan struct{})
	p.identityWg.Add(1)
	go func() {
		defer p.identityWg.Done()
		<-release
	}()

	if p.WaitStopped(30 * time.Millisecond) {
		t.Fatal("WaitStopped вернул true, хотя запись идентичности ещё идёт: " +
			"поздний setNonEmptyR493 снова гоняется с читателями снимка (R528)")
	}
	close(release)
	if !p.WaitStopped(2 * time.Second) {
		t.Fatal("WaitStopped не дождался завершения фоновой идентичности (R528)")
	}
}

// TestR528_PollAllTracksIdentityGoroutine — сторож проводки: горутина
// идентичности обязана учитываться в identityWg, иначе WaitStopped снова
// перестанет быть гарантией (поведенчески это ловится только -race).
func TestR528_PollAllTracksIdentityGoroutine(t *testing.T) {
	src, err := os.ReadFile("llamacpp_metrics_poller.go")
	if err != nil {
		t.Skipf("llamacpp_metrics_poller.go не найден: %v", err)
	}
	text := string(src)
	for _, want := range []string{
		"p.identityWg.Add(1)",
		"defer p.identityWg.Done()",
		"waitGroupWithin(&p.identityWg, deadline)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("в llamacpp_metrics_poller.go нет %q — ожидание идентичности "+
				"снова выпало из WaitStopped (R528)", want)
		}
	}
}
