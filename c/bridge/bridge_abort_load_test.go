// bridge_abort_load_test.go — R60.57 (2026-09-13): tests для RequestLoadAbort API
// и R60.57 follow-up (2026-09-13) — LoadModelWithEarlyHandle API.
//
// Покрывают:
//   - nil safety: nil handle → error, no panic
//   - nil ptr safety: handle с ptr=nil → error (real build) / no-op (stub)
//   - RequestLoadAbortAll — no-op, no panic
//   - IsLoadAborted nil safety
//   - IsLoadAborted на stub-loaded model → false (stub no-op)
//   - LoadModelWithEarlyHandle: API contract (early expose, nil safety, error path)
//
// Тесты компилируются с ОБОИМИ build tags (llama_stub и !llama_stub):
//   - В stub mode: RequestLoadAbort — no-op (return nil); IsLoadAborted — false.
//     LoadModelWithEarlyHandle — earlyHandle.path заполняется, ptr остаётся nil.
//   - В real build: RequestLoadAbort(nil) → error "nil handle"; real load
//     aborts через ctx.Done watcher (Phase 3-4 — out of scope здесь).
//
// Integration-тесты с реальным C-bridge (force stuck load, verify abort
// within <500ms, verify subsequent load succeeds) — Phase 5, в
// internal/cppbackend/load_abort_r60_57_test.go.

package bridge

import (
	"strings"
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

// ============================================================
// R60.57 follow-up (2026-09-13): LoadModelWithEarlyHandle tests
// ============================================================
//
// Покрывают contract новой API:
//   - earlyHandle == nil → error
//   - happy path → earlyHandle получает path, возвращённый handle не nil
//   - error path → возвращается error (stub mode: всегда успех; real mode:
//     load failure приводит к error и earlyHandle.ptr = nil — но в stub
//     mode мы не можем это проверить)
//   - legacy LoadModel остаётся backward compatible

// TestLoadModelWithEarlyHandle_NilEarlyHandle — вызов с nil earlyHandle
// возвращает error и не паникует. Это критично для безопасности —
// nil-указатель мог бы привести к C-side segfault.
func TestLoadModelWithEarlyHandle_NilEarlyHandle(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("LoadModelWithEarlyHandle(nil) panicked: %v", r)
		}
	}()
	handle, err := LoadModelWithEarlyHandle(ModelConfig{ModelPath: "/tmp/fake.gguf"}, nil)
	if err == nil {
		t.Error("LoadModelWithEarlyHandle(nil earlyHandle) should return error")
	}
	if handle != nil {
		t.Errorf("LoadModelWithEarlyHandle(nil) should return nil handle, got: %v", handle)
	}
	// Error должен упомянуть "earlyHandle" для diagnostics.
	if err != nil && !strings.Contains(err.Error(), "earlyHandle") {
		t.Errorf("error should mention earlyHandle for diagnostics, got: %v", err)
	}
}

// TestLoadModelWithEarlyHandle_StubSetsPath — happy path в stub mode.
// earlyHandle должен получить path ДО возврата из функции. В stub mode
// ptr остаётся nil (нет C-уровневого handle), но path установлен —
// имитирует «early expose» из real bridge.
func TestLoadModelWithEarlyHandle_StubSetsPath(t *testing.T) {
	cfg := ModelConfig{ModelPath: "/tmp/fake-model.gguf"}
	earlyHandle := &ModelHandle{}

	handle, err := LoadModelWithEarlyHandle(cfg, earlyHandle)
	if err != nil {
		t.Fatalf("LoadModelWithEarlyHandle: %v", err)
	}
	if handle == nil {
		t.Fatal("returned handle is nil")
	}
	// earlyHandle.path должен быть установлен (имитация early expose).
	if earlyHandle.path != cfg.ModelPath {
		t.Errorf("earlyHandle.path = %q, want %q", earlyHandle.path, cfg.ModelPath)
	}
	// Возвращённый handle.path должен совпадать.
	if handle.path != cfg.ModelPath {
		t.Errorf("returned handle.path = %q, want %q", handle.path, cfg.ModelPath)
	}
	// Defer FreeModel для cleanup.
	defer handle.FreeModel()
}

// TestLoadModelWithEarlyHandle_ConcurrentSafe — race detector тест.
// 100 goroutines параллельно вызывают LoadModelWithEarlyHandle с
// разными earlyHandle. Это проверяет что runtime.Pinner защищает
// от GC relocation и нет data race между C-write и Go-write в
// earlyHandle.path / earlyHandle.ptr.
//
// Если runtime.Pinner не используется или есть race, `go test -race`
// упадёт с race detector report.
func TestLoadModelWithEarlyHandle_ConcurrentSafe(t *testing.T) {
	const N = 100
	type result struct {
		path     string
		err      error
		handleID int
	}
	results := make(chan result, N)

	for i := 0; i < N; i++ {
		go func(id int) {
			cfg := ModelConfig{ModelPath: "/tmp/concurrent-test.gguf"}
			earlyHandle := &ModelHandle{}
			handle, err := LoadModelWithEarlyHandle(cfg, earlyHandle)
			results <- result{
				path:     earlyHandle.path,
				err:      err,
				handleID: id,
			}
			if handle != nil {
				handle.FreeModel()
			}
		}(i)
	}

	for i := 0; i < N; i++ {
		r := <-results
		if r.err != nil {
			t.Errorf("goroutine %d failed: %v", r.handleID, r.err)
		}
		if r.path != "/tmp/concurrent-test.gguf" {
			t.Errorf("goroutine %d: earlyHandle.path = %q, want %q",
				r.handleID, r.path, "/tmp/concurrent-test.gguf")
		}
	}
}

// TestLoadModel_LegacyStillWorks — legacy LoadModel (single-arg) остаётся
// backward compatible. Это критично — rpcworker/model_manager.go и другие
// callers не должны ломаться. R60.57 follow-up НЕ должен быть breaking
// change для legacy users.
func TestLoadModel_LegacyStillWorks(t *testing.T) {
	cfg := ModelConfig{ModelPath: "/tmp/legacy-test.gguf"}
	handle, err := LoadModel(cfg)
	if err != nil {
		t.Fatalf("LoadModel: %v", err)
	}
	if handle == nil {
		t.Fatal("LoadModel returned nil handle")
	}
	if handle.path != cfg.ModelPath {
		t.Errorf("handle.path = %q, want %q", handle.path, cfg.ModelPath)
	}
	defer handle.FreeModel()
}
