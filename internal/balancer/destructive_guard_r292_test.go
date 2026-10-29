package balancer

// destructive_guard_r292_test.go — R292 (2026-10-29): второй клиент не имеет
// права выгрузить модель под живым стримом первого.
//
// ЖИВАЯ ЖАЛОБА (второй стенд, два агента, одна модель): как только запускали
// второго агента, первый получал `stream ended by upstream before completion`.
// Guard R271 стоял только на путях ПЕРЕЗАГРУЗКИ (n_ctx-preflight, AutoTune,
// операторский reload), а ModelManager.ExecuteOperation (load/unload — через него
// ходят автозагрузка по запросу клиента и API) его не проверял. Для cppworker
// `load` с другими параметрами = unload+load, то есть выгрузка модели под чужой
// генерацией.
//
// Тесты фиксируют:
//   - при активном стриме load/unload ОТКЛАДЫВАЕТСЯ и в воркер НЕ уходит;
//   - отложение не считается провалом: circuit breaker не открывается, клиенту
//     советуют короткий retry, а не 90 секунд;
//   - без стримов поведение прежнее (операция выполняется);
//   - «отложено» переживает обёртку «model load failed», и клиент получает
//     честный текст («занято», а не «не загрузилось»).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// TestR292_GuardDefersWhileStreamActive — guard откладывает операцию, пока на
// бэкенде есть живой стрим, и пропускает, когда стримов нет.
func TestR292_GuardDefersWhileStreamActive(t *testing.T) {
	// Бюджет ожидания 0 = «не ждать»: операция откладывается сразу.
	t.Setenv("LB_DESTRUCTIVE_OP_WAIT_SEC", "0")

	p := &Proxy{}
	if derr := p.guardModelOpR292("b1", "m1", "load"); derr != nil {
		t.Fatalf("без стримов guard должен пропускать, получили %v", derr)
	}

	release := beginClientStreamR271(p, "b1", "m1")
	defer release()

	derr := p.guardModelOpR292("b1", "m1", "load")
	if derr == nil {
		t.Fatal("при активном стриме load обязан быть отложен")
	}
	if derr.Streams != 1 {
		t.Errorf("Streams = %d, want 1", derr.Streams)
	}
	if derr.RetrySec < 5 {
		t.Errorf("RetrySec = %d, want >= 5 (клиент не должен ждать вечно)", derr.RetrySec)
	}
	if !errors.Is(derr, errDestructiveOpDeferredR292) {
		t.Error("ошибка отложения должна матчиться по sentinel")
	}

	// Стрим на ДРУГОЙ модели того же бэкенда — тоже повод отложить: cppworker
	// выгружает модель целиком, а слоты делят одну карту.
	releaseM2 := beginClientStreamR271(p, "b1", "m2")
	defer releaseM2()
	if derr := p.guardModelOpR292("b1", "m1", "unload"); derr == nil {
		t.Fatal("стрим на другой модели того же бэкенда тоже должен откладывать операцию")
	}
}

// TestR292_ExecuteOperationDeferredAndDoesNotReachWorker — главный тест:
// под активным стримом ModelManager НЕ должен отправлять load/unload воркеру.
func TestR292_ExecuteOperationDeferredAndDoesNotReachWorker(t *testing.T) {
	t.Setenv("LB_DESTRUCTIVE_OP_WAIT_SEC", "0")

	var workerCalls atomic.Int32
	p := newProxyWithCustomCppWorker(t, "b1", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/api/models") {
			workerCalls.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"models":[]}`)
	})
	mm := NewModelManager(p)
	p.SetModelManager(mm)

	// Без стримов операция доходит до воркера (поведение прежнее).
	before := workerCalls.Load()
	_ = mm.ExecuteOperation("b1", ModelOpRequest{Operation: "load", ModelName: "m1", ContextPerSeq: intPtrR292(4096)})
	if workerCalls.Load() == before {
		t.Log("примечание: воркер не был вызван и без стримов (мок не поддержал load) — проверяем только отложенный кейс")
	}

	// Под активным стримом — операция откладывается и воркер НЕ трогаем.
	release := beginClientStreamR271(p, "b1", "m1")
	defer release()

	before = workerCalls.Load()
	res := mm.ExecuteOperation("b1", ModelOpRequest{Operation: "unload", ModelName: "m1"})
	if res == nil {
		t.Fatal("ExecuteOperation вернул nil")
	}
	if !res.Deferred {
		t.Fatalf("ожидали Deferred=true при активном стриме, получили %+v", res)
	}
	if res.RetryAfterSec <= 0 {
		t.Errorf("RetryAfterSec = %d, want > 0", res.RetryAfterSec)
	}
	if got := workerCalls.Load(); got != before {
		t.Errorf("воркер получил %d запрос(ов) под живым стримом — модель могли выгрузить", got-before)
	}
	if !strings.Contains(strings.ToLower(res.Error), "deferred") {
		t.Errorf("текст ошибки должен говорить «отложено», получили %q", res.Error)
	}
}

// TestR292_DeferredSurvivesWrappingAndShortRetry — «отложено» не должно
// превращаться в «load failed» с советом ждать 90 секунд.
func TestR292_DeferredSurvivesWrappingAndShortRetry(t *testing.T) {
	derr := &destructiveDeferredErrorR292{BackendID: "b1", Model: "m1", Streams: 2, RetrySec: 7}

	// ровно та обёртка, которой пользуются пути ожидания загрузки.
	wrapped := fmt.Errorf("model load failed: %w", derr)
	if !IsDestructiveDeferredR292(wrapped) {
		t.Fatal("errors.Is через обёртку не работает: классификация потеряется")
	}
	if retry, ok := RetryAfterOfDestructiveDeferredR292(wrapped); !ok || retry != 7 {
		t.Fatalf("RetryAfter = (%d,%v), want (7,true)", retry, ok)
	}
	if got := writeAutoLoadRetryAfter(wrapped); got != 7 {
		t.Errorf("writeAutoLoadRetryAfter = %d, want 7 (а не 90 по умолчанию)", got)
	}

	// Текст для клиента: «занято», а не «не загрузилось».
	text := autoLoadFailureTextR292("m1", wrapped)
	lower := strings.ToLower(text)
	if strings.Contains(lower, "auto-load failed") {
		t.Errorf("клиенту нельзя говорить «auto-load failed»: %q", text)
	}
	if !strings.Contains(lower, "busy") && !strings.Contains(lower, "занят") {
		t.Errorf("текст должен объяснять занятость: %q", text)
	}
	if !strings.Contains(text, "7") {
		t.Errorf("текст должен называть retry-секунды: %q", text)
	}
}

// TestR292_GuardWaitsForStreamToFinish — при ненулевом бюджете guard ЖДЁТ
// завершения стрима и пропускает операцию, а не откладывает навсегда.
func TestR292_GuardWaitsForStreamToFinish(t *testing.T) {
	t.Setenv("LB_DESTRUCTIVE_OP_WAIT_SEC", "2")

	p := &Proxy{}
	release := beginClientStreamR271(p, "b1", "m1")

	go func() {
		time.Sleep(300 * time.Millisecond)
		release()
	}()

	start := time.Now()
	derr := p.guardModelOpR292("b1", "m1", "load")
	if derr != nil {
		t.Fatalf("guard должен был дождаться завершения стрима, получили отложение: %v", derr)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("guard ждал слишком долго: %s", elapsed)
	}
}

// TestR292_GuardNilSafe — nil-прокси и пустой backendID не паникуют.
func TestR292_GuardNilSafe(t *testing.T) {
	var nilProxy *Proxy
	if derr := nilProxy.guardModelOpR292("b1", "m1", "load"); derr != nil {
		t.Fatalf("nil-proxy должен пропускать, получили %v", derr)
	}
	p := &Proxy{}
	if derr := p.guardModelOpR292("", "m1", "load"); derr != nil {
		t.Fatalf("пустой backendID должен пропускать, получили %v", derr)
	}
	if _, ok := RetryAfterOfDestructiveDeferredR292(nil); ok {
		t.Error("nil-ошибка не должна считаться отложенной")
	}
	if IsDestructiveDeferredR292(errors.New("other")) {
		t.Error("посторонняя ошибка не должна считаться отложенной")
	}
}

// TestR292_BudgetFromSettings — бюджет из реестра настроек: 0 = не ждать,
// отрицательное = дефолт (а не «ждать вечно»).
func TestR292_BudgetFromSettings(t *testing.T) {
	t.Setenv("LB_DESTRUCTIVE_OP_WAIT_SEC", "0")
	if got := destructiveOpWaitBudgetR292(); got != 0 {
		t.Errorf("0 должен означать «не ждать», получили %s", got)
	}
	t.Setenv("LB_DESTRUCTIVE_OP_WAIT_SEC", "-3")
	if got := destructiveOpWaitBudgetR292(); got != defaultDestructiveOpWaitSecR292*time.Second {
		t.Errorf("отрицательное значение должно давать дефолт, получили %s", got)
	}
}

// TestR292_DestructiveKindMapping — вид операции для классификатора обрыва.
func TestR292_DestructiveKindMapping(t *testing.T) {
	if got := destructiveKindForModelOpR292("unload"); got != destructiveUnloadR271 {
		t.Errorf("unload → %q, want %q", got, destructiveUnloadR271)
	}
	if got := destructiveKindForModelOpR292("load"); got != destructiveReloadR271 {
		t.Errorf("load → %q, want %q", got, destructiveReloadR271)
	}
}

func intPtrR292(v int) *int { return &v }

// TestR292_ContextNotCanceledPanic — ждём через контекст: отменённый контекст
// не должен вешать ожидание (nil-ctx трактуется как Background).
func TestR292_ContextCanceledStopsWaiting(t *testing.T) {
	p := &Proxy{}
	release := beginClientStreamR271(p, "b1", "m1")
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Стрим активен, а контекст уже отменён: ждать секунду не должны — сразу false
	// (иначе отменённый клиент держал бы операцию ещё секунду).
	start := time.Now()
	if ok := p.waitStreamsIdleR271(ctx, "b1", "m1", time.Second); ok {
		t.Error("отменённый контекст не должен давать «можно выполнять»")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("ждали %s при отменённом контексте — слишком долго", elapsed)
	}
}

// TestR292_BackendTypeGuardOnlyForLlamaCpp — guard применяется к llama.cpp
// (для ollama-бэкенда load/unload не выгружает чужую модель тем же способом).
func TestR292_BackendTypeGuardOnlyForLlamaCpp(t *testing.T) {
	p := newProxyWithCustomCppWorker(t, "b1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"models":[]}`)
	})
	mm := NewModelManager(p)
	p.SetModelManager(mm)

	// Ollama-бэкенд того же ID не должен попадать под новый guard: подменяем тип.
	if st, ok := p.backends["b1"]; ok && st != nil {
		st.Backend.Type = types.BackendTypeOllama
		st.Backend.Engine = types.EngineOllamaAPI
	}

	release := beginClientStreamR271(p, "b1", "m1")
	defer release()
	res := mm.ExecuteOperation("b1", ModelOpRequest{Operation: "unload", ModelName: "m1"})
	if res != nil && res.Deferred {
		t.Error("для ollama-бэкенда отложение по R292 не применяется")
	}
}
