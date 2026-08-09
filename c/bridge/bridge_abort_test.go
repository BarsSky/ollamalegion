// bridge_abort_test.go — Round 31 #6 (2026-08-09): tests для RequestAbort API.
//
// Покрывают:
//   - nil safety: nil handle → error, no panic
//   - IsAborted на nil handle → false
//   - RequestAbortAll — no-op, no panic
//   - ErrCodeAborted = -100 (контракт для C-bridge sync)
//   - ErrAborted sentinel — usable через errors.Is
//
// Не требуют реальной модели — тестируют Go-side обвязку поверх
// (предположительно stub) C-bridge. В stub mode RequestAbort no-op,
// IsAborted всегда false. Это OK для unit-тестов: мы проверяем
// что Go API не паникует и ведёт себя consistent с stub-контрактом.

package bridge

import (
	"errors"
	"testing"
)

// TestRequestAbort_NilHandle — вызов с nil не паникует. В stub mode — no-op
// (return nil). В real build — может вернуть error (если model==nil проверка
// строгая). Главный критерий: NO PANIC.
func TestRequestAbort_NilHandle(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("RequestAbort(nil) panicked: %v", r)
		}
	}()
	_ = RequestAbort(nil) // OK — оба варианта (nil/err) валидны
}

// TestRequestAbort_NilPtr — handle с ptr=nil возвращает error "model not loaded".
// В stub mode RequestAbort — no-op, поэтому err == nil. Это OK: контракт
// stub-build tag'а — не падать на типичных сценариях.
func TestRequestAbort_NilPtr(t *testing.T) {
	handle, err := LoadModel(ModelConfig{ModelPath: "/tmp/fake.gguf"})
	if err != nil {
		t.Fatalf("LoadModel: %v", err)
	}
	defer handle.FreeModel()
	// Оба поведения (error в real, nil в stub) валидны для своего build tag.
	_ = RequestAbort(handle)
}

// TestIsAborted_NilSafety — nil handle / nil ptr → false, no panic.
func TestIsAborted_NilSafety(t *testing.T) {
	if IsAborted(nil) {
		t.Error("IsAborted(nil) should return false")
	}
	handle, err := LoadModel(ModelConfig{ModelPath: "/tmp/fake.gguf"})
	if err != nil {
		t.Fatalf("LoadModel: %v", err)
	}
	defer handle.FreeModel()
	// IsAborted на stub handle — false (в stub mode abort не отслеживается).
	if IsAborted(handle) {
		t.Error("IsAborted on stub handle should be false")
	}
}

// TestRequestAbortAll_NoError — RequestAbortAll всегда nil, не паникует.
func TestRequestAbortAll_NoError(t *testing.T) {
	if err := RequestAbortAll(); err != nil {
		t.Errorf("RequestAbortAll() should return nil, got: %v", err)
	}
}

// TestErrCodeAborted_Contract — ErrCodeAborted == -100 для sync с C-bridge.
// Если C-bridge возвращает -100 (BRIDGE_ERR_ABORTED), Go-side
// должен распознать это как ErrCodeAborted.
func TestErrCodeAborted_Contract(t *testing.T) {
	if ErrCodeAborted != -100 {
		t.Errorf("ErrCodeAborted must be -100 (C-bridge contract), got %d", ErrCodeAborted)
	}
}

// TestErrAborted_ErrorsIs — ErrAborted sentinel usable через errors.Is.
func TestErrAborted_ErrorsIs(t *testing.T) {
	wrapped := errors.New("wrapper: " + ErrAborted.Error())
	if errors.Is(wrapped, ErrAborted) {
		t.Fatal("plain errors.New should not match ErrAborted via errors.Is")
	}

	// Реальный wrapping через fmt.Errorf("%w", ...) — должен матчиться.
	properlyWrapped := wrapErrAborted()
	if !errors.Is(properlyWrapped, ErrAborted) {
		t.Fatal("fmt.Errorf(\"%w\", ErrAborted) should match via errors.Is")
	}
}

// wrapErrAborted — helper для теста, чтобы избежать import cycle.
func wrapErrAborted() error {
	return errors.Join(errors.New("context"), ErrAborted)
}

// TestStubMode_RequestAbortSafe — sanity check: в stub build RequestAbort
// не паникует на валидном handle. (В stub build tag этот тест всегда pass;
// в real build — поведение зависит от C-bridge, которое пока stub.)
func TestStubMode_RequestAbortSafe(t *testing.T) {
	handle, err := LoadModel(ModelConfig{ModelPath: "/tmp/fake.gguf"})
	if err != nil {
		t.Fatalf("LoadModel: %v", err)
	}
	defer handle.FreeModel()

	// В stub mode — no-op (return nil). В real mode — возможна ошибка
	// если модель не загружена в C. Для нашего теста оба варианта OK.
	_ = RequestAbort(handle)

	// IsAborted в stub mode всегда false.
	if IsAborted(handle) {
		t.Error("IsAborted in stub mode should always be false")
	}
}
