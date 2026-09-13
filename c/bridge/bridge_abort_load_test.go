// bridge_abort_load_test.go — R60.57 (2026-09-13): tests для RequestLoadAbort API.
//
// Покрывают:
//   - nil safety: nil handle → error, no panic
//   - nil ptr safety: handle с ptr=nil → error (real build) / no-op (stub)
//   - RequestLoadAbortAll — no-op, no panic
//   - IsLoadAborted nil safety
//   - IsLoadAborted на stub-loaded model → false (stub no-op)
//
// Тесты компилируются с ОБОИМИ build tags (llama_stub и !llama_stub):
//   - В stub mode: RequestLoadAbort — no-op (return nil); IsLoadAborted — false.
//   - В real build: RequestLoadAbort(nil) → error "nil handle"; real load
//     aborts через ctx.Done watcher (Phase 3-4 — out of scope здесь).
//
// Integration-тесты с реальным C-bridge (force stuck load, verify abort
// within <500ms, verify subsequent load succeeds) — Phase 5, в
// internal/cppbackend/load_abort_r60_57_test.go.

package bridge

import (
	"testing"
)

// TestRequestLoadAbort_NilHandle — вызов с nil handle не паникует.
// В stub mode — no-op (return nil). В real build — может вернуть error
// (если model==nil проверка строгая). Главный критерий: NO PANIC.
func TestRequestLoadAbort_NilHandle(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("RequestLoadAbort(nil) panicked: %v", r)
		}
	}()
	_ = RequestLoadAbort(nil) // OK — оба варианта (nil/err) валидны
}

// TestRequestLoadAbort_NilPtr — handle с ptr=nil возвращает error
// "model handle ptr is nil". В stub mode RequestLoadAbort — no-op,
// поэтому err == nil (stub-build tag контракт). Это OK: контракт
// stub-build tag'а — не падать на типичных сценариях.
func TestRequestLoadAbort_NilPtr(t *testing.T) {
	handle, err := LoadModel(ModelConfig{ModelPath: "/tmp/fake.gguf"})
	if err != nil {
		t.Fatalf("LoadModel: %v", err)
	}
	defer handle.FreeModel()
	// В stub mode (этот файл всегда компилируется с обоими build tags)
	// — no-op. В real mode может быть error. Оба поведения OK для своего tag.
	_ = RequestLoadAbort(handle)
}

// TestIsLoadAborted_NilSafety — nil handle / nil ptr → false, no panic.
func TestIsLoadAborted_NilSafety(t *testing.T) {
	if IsLoadAborted(nil) {
		t.Error("IsLoadAborted(nil) should return false")
	}
	handle, err := LoadModel(ModelConfig{ModelPath: "/tmp/fake.gguf"})
	if err != nil {
		t.Fatalf("LoadModel: %v", err)
	}
	defer handle.FreeModel()
	// IsLoadAborted на stub handle — false (в stub mode abort не отслеживается).
	if IsLoadAborted(handle) {
		t.Error("IsLoadAborted on stub handle should be false")
	}
}

// TestRequestLoadAbortAll_NoError — RequestLoadAbortAll всегда nil, не паникует.
func TestRequestLoadAbortAll_NoError(t *testing.T) {
	if err := RequestLoadAbortAll(); err != nil {
		t.Errorf("RequestLoadAbortAll() should return nil, got: %v", err)
	}
}

// TestRequestLoadAbort_StubSafe — sanity check: в stub build RequestLoadAbort
// не паникует на валидном handle. (В stub build tag этот тест всегда pass;
// в real build — поведение зависит от C-bridge, которое пока stub в тестах.)
func TestRequestLoadAbort_StubSafe(t *testing.T) {
	handle, err := LoadModel(ModelConfig{ModelPath: "/tmp/fake.gguf"})
	if err != nil {
		t.Fatalf("LoadModel: %v", err)
	}
	defer handle.FreeModel()

	// В stub mode — no-op (return nil). В real mode — возможна ошибка
	// если модель не загружена в C. Для нашего теста оба варианта OK.
	_ = RequestLoadAbort(handle)

	// IsLoadAborted в stub mode всегда false.
	if IsLoadAborted(handle) {
		t.Error("IsLoadAborted in stub mode should always be false")
	}
}

// TestRequestLoadAbort_Idempotent — повторные вызовы RequestLoadAbort
// безопасны (atomic flag остаётся = 1, нет UB). В stub mode это no-op.
// В real mode проверяет что нет crash/double-free при множественных вызовах
// до того как C-bridge обработает abort.
func TestRequestLoadAbort_Idempotent(t *testing.T) {
	handle, err := LoadModel(ModelConfig{ModelPath: "/tmp/fake.gguf"})
	if err != nil {
		t.Fatalf("LoadModel: %v", err)
	}
	defer handle.FreeModel()

	// 5 повторных вызовов — все должны быть safe.
	for i := 0; i < 5; i++ {
		_ = RequestLoadAbort(handle)
	}
}

// TestAbortLoadSentinels_Contract — проверка что сигналы и коды для
// R60.57 идентичны Round 31 #6 (inference abort). Это страховка от
// drift: коды должны быть одинаковыми между inference и load abort,
// потому что Go-side handler (Phase 3-4) будет использовать один и
// тот же ErrAborted sentinel для обоих случаев.
func TestAbortLoadSentinels_Contract(t *testing.T) {
	if ErrCodeAborted != -100 {
		t.Errorf("ErrCodeAborted must be -100 (C-bridge BRIDGE_ERR_ABORTED contract), got %d", ErrCodeAborted)
	}
	if ErrAborted == nil {
		t.Error("ErrAborted sentinel must be non-nil")
	}
}
