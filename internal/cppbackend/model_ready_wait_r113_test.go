//go:build llama_stub

// model_ready_wait_r113_test.go — R113 (2026-10-13): ожидание ГОТОВНОСТИ модели.
//
// ЖИВОЙ ДЕФЕКТ (воспроизведён на стенде). Запрос к модели, которая в этот момент
// грузилась/перезагружалась (n_ctx другого клиента), получал ответ за 0.15 с:
//
//	{"done":true,"done_reason":"error",
//	 "error":"model gemma-4-E4B-it-Q4_K_M is not loaded (state: loading)"}
//
// То есть клиент (Cline) видел ЗАВЕРШЁННЫЙ ход без единого токена — «COMPLETE
// прилетает до того, как вообще происходит потоковая генерация ответа».
//
// Корень: GetModel() отдаёт запись модели в ЛЮБОМ состоянии, а вызывающий считал
// это признаком готовности. Эти тесты фиксируют разницу между «запись есть» и
// «модель готова», и что ожидание заканчивается ровно тогда, когда модель готова.
package cppbackend

import (
	"context"
	"errors"
	"testing"
	"time"
)

func backendWithModelR113(t *testing.T, name string, state LoadState) *Backend {
	t.Helper()
	b := NewBackend(Config{ModelsDir: t.TempDir()})
	inst := &modelInstance{info: ModelInfo{Name: name, State: state, GPULayers: 19}}
	b.mu.Lock()
	b.models[name] = inst
	b.mu.Unlock()
	return b
}

func setStateR113(b *Backend, name string, state LoadState) {
	b.mu.RLock()
	inst := b.models[name]
	b.mu.RUnlock()
	if inst == nil {
		return
	}
	inst.mu.Lock()
	inst.info.State = state
	inst.mu.Unlock()
}

// TestR113_GetModelDoesNotMeanReady — GetModel отдаёт запись и в состоянии
// loading; именно на этом ломался прежний код («есть запись» ≠ «готова»).
func TestR113_GetModelDoesNotMeanReady(t *testing.T) {
	b := backendWithModelR113(t, "m", StateLoading)
	if _, err := b.GetModel("m"); err != nil {
		t.Fatalf("GetModel в состоянии loading вернул ошибку: %v (тест потерял смысл)", err)
	}
	state, exists := b.ModelState("m")
	if !exists || state != StateLoading {
		t.Fatalf("ModelState = (%v, %v), ожидалось (loading, true)", state, exists)
	}
}

// TestR113_WaitForModelReady_WaitsUntilLoaded — ожидание заканчивается, когда
// модель стала готовой, и не раньше.
func TestR113_WaitForModelReady_WaitsUntilLoaded(t *testing.T) {
	b := backendWithModelR113(t, "m", StateLoading)

	go func() {
		time.Sleep(150 * time.Millisecond)
		setStateR113(b, "m", StateLoaded)
	}()

	start := time.Now()
	err := b.WaitForModelReady(context.Background(), "m", 10*time.Millisecond, 5*time.Second)
	if err != nil {
		t.Fatalf("ожидание готовности вернуло ошибку: %v", err)
	}
	if waited := time.Since(start); waited < 100*time.Millisecond {
		t.Fatalf("вернулось слишком рано (%v) — состояние loading не учтено", waited)
	}
}

// TestR113_WaitForModelReady_TimeoutMeansLoading — истёк предел → ErrModelIsLoading
// (caller отдаёт 503 + Retry-After, а не done:true).
func TestR113_WaitForModelReady_TimeoutMeansLoading(t *testing.T) {
	b := backendWithModelR113(t, "m", StateLoading)
	err := b.WaitForModelReady(context.Background(), "m", 10*time.Millisecond, 200*time.Millisecond)
	if !errors.Is(err, ErrModelIsLoading) {
		t.Fatalf("по истечении предела получено %v, ожидалось ErrModelIsLoading", err)
	}
}

// TestR113_WaitForModelReady_ContextCancel — отмена клиентом тоже loading-исход.
func TestR113_WaitForModelReady_ContextCancel(t *testing.T) {
	b := backendWithModelR113(t, "m", StateLoading)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(80 * time.Millisecond)
		cancel()
	}()
	err := b.WaitForModelReady(ctx, "m", 10*time.Millisecond, 10*time.Second)
	if !errors.Is(err, ErrModelIsLoading) {
		t.Fatalf("при отмене контекста получено %v, ожидалось ErrModelIsLoading", err)
	}
}

// TestR113_WaitForModelReady_StateError — провал загрузки отдаётся ошибкой
// загрузки (а не «повтори позже»: повторять бессмысленно).
func TestR113_WaitForModelReady_StateError(t *testing.T) {
	b := backendWithModelR113(t, "m", StateError)
	err := b.WaitForModelReady(context.Background(), "m", 10*time.Millisecond, time.Second)
	if err == nil || errors.Is(err, ErrModelIsLoading) {
		t.Fatalf("состояние error: получено %v, ожидалась ошибка загрузки", err)
	}
}

// TestR113_WaitForModelReady_NotFound — записи нет: ждать нечего.
func TestR113_WaitForModelReady_NotFound(t *testing.T) {
	b := NewBackend(Config{ModelsDir: t.TempDir()})
	err := b.WaitForModelReady(context.Background(), "nope", 10*time.Millisecond, 200*time.Millisecond)
	if err == nil || errors.Is(err, ErrModelIsLoading) {
		t.Fatalf("отсутствующая модель: получено %v, ожидалась ошибка «not found»", err)
	}
}
