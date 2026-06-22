package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// resetRamFallbackAttemptsForTest — очищает глобальный state для теста.
func resetRamFallbackAttemptsForTest() {
	ramFallbackAttempts.Range(func(key, value interface{}) bool {
		ramFallbackAttempts.Delete(key)
		return true
	})
}

func TestReloadAttempts_NoState(t *testing.T) {
	resetRamFallbackAttemptsForTest()
	defer resetRamFallbackAttemptsForTest()

	count, isLimit := getReloadAttempts("model-never-seen")
	if count != 0 || isLimit {
		t.Errorf("getReloadAttempts for new model: got count=%d, isLimit=%v, want 0, false", count, isLimit)
	}
}

func TestRecordReloadAttempt_FirstAttempt(t *testing.T) {
	resetRamFallbackAttemptsForTest()
	defer resetRamFallbackAttemptsForTest()

	recordReloadAttempt("model-A")
	count, isLimit := getReloadAttempts("model-A")
	if count != 1 {
		t.Errorf("after first record: count=%d, want 1", count)
	}
	if isLimit {
		t.Errorf("after first record: isLimit=true, want false")
	}
}

func TestRecordReloadAttempt_MultipleAttempts(t *testing.T) {
	resetRamFallbackAttemptsForTest()
	defer resetRamFallbackAttemptsForTest()

	recordReloadAttempt("model-B")
	recordReloadAttempt("model-B")
	recordReloadAttempt("model-B")

	count, isLimit := getReloadAttempts("model-B")
	if count != 3 {
		t.Errorf("after 3 records: count=%d, want 3", count)
	}
	if !isLimit {
		t.Errorf("after 3 records: isLimit=false, want true (limit reached)")
	}
}

func TestRecordReloadAttempt_ExceedsMaxLimit(t *testing.T) {
	resetRamFallbackAttemptsForTest()
	defer resetRamFallbackAttemptsForTest()

	// Делаем 5 попыток (больше ramFallbackMaxAttempts=3)
	for i := 0; i < 5; i++ {
		recordReloadAttempt("model-C")
	}

	count, isLimit := getReloadAttempts("model-C")
	if count != 5 {
		t.Errorf("after 5 records: count=%d, want 5", count)
	}
	if !isLimit {
		t.Errorf("after 5 records: isLimit=false, want true (limit exceeded)")
	}
}

func TestRecordReloadAttempt_DifferentModelsIndependent(t *testing.T) {
	resetRamFallbackAttemptsForTest()
	defer resetRamFallbackAttemptsForTest()

	recordReloadAttempt("model-X")
	recordReloadAttempt("model-X")
	recordReloadAttempt("model-Y")

	countX, isLimitX := getReloadAttempts("model-X")
	if countX != 2 || isLimitX {
		t.Errorf("model-X: count=%d, isLimit=%v, want 2, false", countX, isLimitX)
	}

	countY, isLimitY := getReloadAttempts("model-Y")
	if countY != 1 || isLimitY {
		t.Errorf("model-Y: count=%d, isLimit=%v, want 1, false", countY, isLimitY)
	}
}

func TestResetReloadAttempts(t *testing.T) {
	resetRamFallbackAttemptsForTest()
	defer resetRamFallbackAttemptsForTest()

	recordReloadAttempt("model-D")
	recordReloadAttempt("model-D")
	recordReloadAttempt("model-D")

	// До reset: лимит достигнут
	count, isLimit := getReloadAttempts("model-D")
	if !isLimit {
		t.Errorf("before reset: isLimit=false, want true")
	}

	resetReloadAttempts("model-D")

	// После reset: лимит снят
	count, isLimit = getReloadAttempts("model-D")
	if count != 0 || isLimit {
		t.Errorf("after reset: count=%d, isLimit=%v, want 0, false", count, isLimit)
	}
}

func TestResetReloadAttempts_NonExistent(t *testing.T) {
	resetRamFallbackAttemptsForTest()
	defer resetRamFallbackAttemptsForTest()

	// Reset для несуществующей модели не должен падать
	resetReloadAttempts("model-that-never-existed")
	// Если дошли сюда без panic — тест пройден
}

func TestReloadAttempts_AfterTimeWindowReset(t *testing.T) {
	resetRamFallbackAttemptsForTest()
	defer resetRamFallbackAttemptsForTest()

	// Ручная модификация: ставим firstAttemptTime в прошлое за пределами окна
	state := &ramFallbackAttemptState{
		count:            ramFallbackMaxAttempts, // = 3 (на пределе)
		firstAttemptTime: time.Now().Add(-2 * ramFallbackCycleResetInterval),
	}
	ramFallbackAttempts.Store("model-E", state)

	// При getReloadAttempts окно истекло — счётчик сбрасывается, лимит не достигнут
	count, isLimit := getReloadAttempts("model-E")
	if count != 0 {
		t.Errorf("after window expired: count=%d, want 0 (auto-reset)", count)
	}
	if isLimit {
		t.Errorf("after window expired: isLimit=true, want false (auto-reset)")
	}
}

func TestReloadAttempts_WindowNotExpired(t *testing.T) {
	resetRamFallbackAttemptsForTest()
	defer resetRamFallbackAttemptsForTest()

	// Свежая запись — окно не истекло
	recordReloadAttempt("model-F")
	recordReloadAttempt("model-F")

	stateRaw, _ := ramFallbackAttempts.Load("model-F")
	state := stateRaw.(*ramFallbackAttemptState)
	state.mu.Lock()
	state.firstAttemptTime = time.Now().Add(-10 * time.Second) // Недавняя
	state.mu.Unlock()

	count, isLimit := getReloadAttempts("model-F")
	if count != 2 {
		t.Errorf("within window: count=%d, want 2", count)
	}
	if isLimit {
		t.Errorf("within window: isLimit=true, want false (count=2 < max=3)")
	}
}

func TestReloadLoopLimitError_Error(t *testing.T) {
	err := &ReloadLoopLimitError{
		Model:   "test-model",
		Count:   3,
		Elapsed: 30 * time.Second,
	}
	msg := err.Error()
	if !strings.Contains(msg, "test-model") {
		t.Errorf("error message missing model name: %s", msg)
	}
	if !strings.Contains(msg, "3 reloads") {
		t.Errorf("error message missing attempts count: %s", msg)
	}
	if !strings.Contains(msg, "Reduce tools") {
		t.Errorf("error message missing suggestion: %s", msg)
	}
}

func TestIsReloadLoopLimitError(t *testing.T) {
	// nil → false
	if isReloadLoopLimitError(nil) {
		t.Errorf("isReloadLoopLimitError(nil) = true, want false")
	}

	// ReloadLoopLimitError → true
	rllErr := &ReloadLoopLimitError{Model: "x", Count: 3}
	if !isReloadLoopLimitError(rllErr) {
		t.Errorf("isReloadLoopLimitError(ReloadLoopLimitError) = false, want true")
	}

	// Обычная ошибка → false
	plainErr := errors.New("some other error")
	if isReloadLoopLimitError(plainErr) {
		t.Errorf("isReloadLoopLimitError(plain error) = true, want false")
	}

	// Завёрнутая ошибка НЕ считается (только direct type match)
	wrappedErr := &wrappedError{msg: "wrapped", inner: rllErr}
	if isReloadLoopLimitError(wrappedErr) {
		t.Errorf("isReloadLoopLimitError(wrapped) = true, want false (только direct match)")
	}
}

// wrappedError — простой wrapper для теста type assertion.
type wrappedError struct {
	msg   string
inner error
}

func (e *wrappedError) Error() string { return e.msg + ": " + e.inner.Error() }

func TestReloadAttempts_ConcurrentSafe(t *testing.T) {
	resetRamFallbackAttemptsForTest()
	defer resetRamFallbackAttemptsForTest()

	// Запускаем 100 горутин, каждая делает recordReloadAttempt
	// Должны получить count >= 1 (без race conditions)
	done := make(chan struct{})
	for i := 0; i < 100; i++ {
		go func() {
			recordReloadAttempt("concurrent-model")
			done <- struct{}{}
		}()
	}
	for i := 0; i < 100; i++ {
		<-done
	}

	count, _ := getReloadAttempts("concurrent-model")
	if count < 1 {
		t.Errorf("after 100 concurrent records: count=%d, want >= 1", count)
	}
	if count > 100 {
		t.Errorf("after 100 concurrent records: count=%d, want <= 100 (no race over-count)", count)
	}
}

func TestRamFallbackMaxAttempts_LimitConstant(t *testing.T) {
	// Smoke test: убедимся что константа в разумных пределах.
	// Если кто-то по ошибке поменяет на 0 или 10000 — тест укажет.
	if ramFallbackMaxAttempts < 1 || ramFallbackMaxAttempts > 10 {
		t.Errorf("ramFallbackMaxAttempts=%d outside sane range [1, 10]", ramFallbackMaxAttempts)
	}
}

func TestRamFallbackCycleResetInterval_Reasonable(t *testing.T) {
	// Window должен быть >= 10 сек (иначе false positives на медленных бэкендах)
	// и <= 10 минут (иначе reload-loop может продолжаться слишком долго).
	if ramFallbackCycleResetInterval < 10*time.Second {
		t.Errorf("ramFallbackCycleResetInterval=%s < 10s (too aggressive)", ramFallbackCycleResetInterval)
	}
	if ramFallbackCycleResetInterval > 10*time.Minute {
		t.Errorf("ramFallbackCycleResetInterval=%s > 10m (too lenient)", ramFallbackCycleResetInterval)
	}
}