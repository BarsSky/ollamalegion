package cppbackend

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestUserTracker_BasicTryAcquireRelease — в пределах лимита TryAcquire=true,
// Release→TryAcquire=true снова.
func TestUserTracker_BasicTryAcquireRelease(t *testing.T) {
	t.Parallel()
	tr := NewUserTracker()
	if !tr.TryAcquire("alice", 3) {
		t.Fatal("1st acquire should succeed")
	}
	if tr.Current("alice") != 1 {
		t.Fatalf("expected current=1, got %d", tr.Current("alice"))
	}
	tr.Release("alice")
	if tr.Current("alice") != 0 {
		t.Fatalf("expected current=0 after release, got %d", tr.Current("alice"))
	}
	if !tr.TryAcquire("alice", 3) {
		t.Fatal("re-acquire after release should succeed")
	}
}

// TestUserTracker_ExceedsLimit — на 4-м TryAcquire с max=3 → false,
// после Release → true снова.
func TestUserTracker_ExceedsLimit(t *testing.T) {
	t.Parallel()
	tr := NewUserTracker()
	for i := 0; i < 3; i++ {
		if !tr.TryAcquire("alice", 3) {
			t.Fatalf("acquire #%d should succeed (limit=3)", i+1)
		}
	}
	if tr.TryAcquire("alice", 3) {
		t.Fatal("4th acquire should fail (limit=3)")
	}
	if tr.Current("alice") != 3 {
		t.Fatalf("expected current=3, got %d", tr.Current("alice"))
	}
	tr.Release("alice")
	if !tr.TryAcquire("alice", 3) {
		t.Fatal("acquire after release should succeed")
	}
}

// TestUserTracker_DifferentUsersIndependent — alice's limit не влияет на bob.
func TestUserTracker_DifferentUsersIndependent(t *testing.T) {
	t.Parallel()
	tr := NewUserTracker()
	if !tr.TryAcquire("alice", 1) {
		t.Fatal("alice 1st acquire should succeed")
	}
	if tr.TryAcquire("alice", 1) {
		t.Fatal("alice 2nd acquire should fail")
	}
	if !tr.TryAcquire("bob", 1) {
		t.Fatal("bob 1st acquire should succeed (different bucket)")
	}
}

// TestUserTracker_UnlimitedMax — max=0 → всегда true, счётчик НЕ растёт.
func TestUserTracker_UnlimitedMax(t *testing.T) {
	t.Parallel()
	tr := NewUserTracker()
	for i := 0; i < 100; i++ {
		if !tr.TryAcquire("alice", 0) {
			t.Fatalf("unlimited acquire #%d should succeed", i+1)
		}
	}
	// Счётчик НЕ должен расти (max<=0 не инкрементирует).
	if tr.Current("alice") != 0 {
		t.Fatalf("unlimited mode should not increment counter, got %d", tr.Current("alice"))
	}
	if tr.Snapshot()["alice"] != 0 {
		t.Fatal("Snapshot should not show alice with 0")
	}
}

// TestUserTracker_NilSafe — все методы на nil receiver не паникуют.
func TestUserTracker_NilSafe(t *testing.T) {
	t.Parallel()
	var tr *UserTracker
	// TryAcquire на nil: возвращаем false (defensive: admission запрещён).
	if tr.TryAcquire("alice", 3) {
		t.Fatal("nil TryAcquire should return false")
	}
	// остальные — no-op
	tr.Release("alice")
	tr.Reset("alice")
	if got := tr.Current("alice"); got != 0 {
		t.Fatalf("nil Current should return 0, got %d", got)
	}
	snap := tr.Snapshot()
	if snap == nil {
		t.Fatal("nil Snapshot should return empty map, not nil")
	}
	if len(snap) != 0 {
		t.Fatalf("nil Snapshot should be empty, got %v", snap)
	}
}

// TestUserTracker_EmptyUserIDBucketAnonymous — userID="" → "anonymous" bucket.
func TestUserTracker_EmptyUserIDBucketAnonymous(t *testing.T) {
	t.Parallel()
	tr := NewUserTracker()
	if !tr.TryAcquire("", 1) {
		t.Fatal("empty userID acquire should succeed")
	}
	if tr.Current("") != 1 {
		t.Fatalf("expected empty=1, got %d", tr.Current(""))
	}
	// anonymous bucket — то же значение
	if tr.Current("anonymous") != 1 {
		t.Fatalf("expected anonymous=1, got %d", tr.Current("anonymous"))
	}
	// 2-й acquire с пустым userID → fail (тот же bucket)
	if tr.TryAcquire("", 1) {
		t.Fatal("2nd empty userID acquire should fail (anonymous bucket full)")
	}
	// Release → ok
	tr.Release("")
	if tr.Current("anonymous") != 0 {
		t.Fatal("release with empty userID should decrement anonymous")
	}
}

// TestUserTracker_SnapshotOnlyNonZero — после Release все нули вычищаются.
func TestUserTracker_SnapshotOnlyNonZero(t *testing.T) {
	t.Parallel()
	tr := NewUserTracker()
	tr.TryAcquire("alice", 10)
	tr.TryAcquire("bob", 10)
	tr.Release("bob") // bob=0, должен быть удалён
	snap := tr.Snapshot()
	if _, has := snap["bob"]; has {
		t.Fatal("Snapshot should not include bob (counter=0)")
	}
	if snap["alice"] != 1 {
		t.Fatalf("expected alice=1, got %d", snap["alice"])
	}
}

// TestUserTracker_Concurrent — стресс-тест: 100 горутин, max=4,
// ровно 4 acquire=true должны быть успешными в любой момент времени.
func TestUserTracker_Concurrent(t *testing.T) {
	t.Parallel()
	tr := NewUserTracker()
	const max = 4
	const goroutines = 100

	var successCount, failCount int64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if tr.TryAcquire("alice", max) {
				atomic.AddInt64(&successCount, 1)
				// Имитация работы — release через горутину
				tr.Release("alice")
			} else {
				atomic.AddInt64(&failCount, 1)
			}
		}()
	}
	wg.Wait()

	// В идеальном случае: 4 success (слоты 1-4) + 96 fail (слоты 5+).
	// Реально из-за concurrent: success может быть от 0 до 4 (зависит от race),
	// но суммарно successes <= max всегда, и counter в конце = 0.
	success := atomic.LoadInt64(&successCount)
	fail := atomic.LoadInt64(&failCount)
	if success+fail != goroutines {
		t.Fatalf("expected sum=%d, got success=%d + fail=%d", goroutines, success, fail)
	}
	if success > max {
		t.Fatalf("concurrent success (%d) exceeded max (%d)", success, max)
	}
	// После всех Release counter должен быть 0
	if tr.Current("alice") != 0 {
		t.Fatalf("counter should be 0 after all releases, got %d", tr.Current("alice"))
	}
}

// TestUserTracker_DoubleReleaseSafe — Release больше раз чем Acquire не уходит ниже 0.
func TestUserTracker_DoubleReleaseSafe(t *testing.T) {
	t.Parallel()
	tr := NewUserTracker()
	tr.TryAcquire("alice", 5)
	tr.Release("alice")
	tr.Release("alice") // double release — counter не должен уйти в -1
	tr.Release("alice")
	if tr.Current("alice") != 0 {
		t.Fatalf("double release should keep counter at 0, got %d", tr.Current("alice"))
	}
}
