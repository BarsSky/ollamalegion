// autotune_defer_r283_test.go — R283 (2026-10-29): AutoTune не путает «сейчас
// занято» с «сломалось» и не долбится в воркер каждые ~70 секунд.
//
// ЖИВОЙ ФАКТ (стенд r282, живой лог за 20 минут): AutoTune срабатывал ≥4 раз
// (`kv_cache q4_0 → f16`), один раз упал с
//
//	unload failed: model 'gemma-4-E4B-it-Q4_K_M' is busy with active inference requests
//
// — то есть отказ пришёл ОТ ВОРКЕРА (модель занята), а не от нашего guard'а.
// После этого попытка повторялась каждые ~70 секунд и каждый раз писала ERROR
// «reload failed», хотя по смыслу это «сейчас нельзя».
//
// Тесты фиксируют три исхода:
//   - busy (воркер или наш guard) → deferred: НЕ error, НЕ circuit breaker,
//     НЕ событие «failed», с растущим backoff;
//   - успех → applied: backoff сброшен, circuit успешен;
//   - реальная ошибка → failed: circuit фиксирует ошибку.
package balancer

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestR283_BusyErrorClassification — «занято» отличается от «сломалось».
func TestR283_BusyErrorClassification(t *testing.T) {
	busy := []string{
		"unload failed: model 'gemma-4-E4B-it-Q4_K_M' is busy with active inference requests",
		"model is busy",
		"active inference requests pending",
		"reload already in progress for model",
	}
	for _, msg := range busy {
		if !isAutoTuneBusyError(msg) {
			t.Errorf("ожидали busy для %q", msg)
		}
	}

	failed := []string{
		"",
		"backend bk-1 not found",
		"load failed: model_file_not_found",
		"connection refused",
	}
	for _, msg := range failed {
		if isAutoTuneBusyError(msg) {
			t.Errorf("%q не должно считаться busy", msg)
		}
	}
}

// TestR283_ApplyErrorMapping — результат apply превращается в нужный класс.
func TestR283_ApplyErrorMapping(t *testing.T) {
	if err := autoTuneErrorFromApply(&AutoTuneApplyResult{Success: true}); err != nil {
		t.Fatalf("успех не должен давать ошибку, получили %v", err)
	}
	if err := autoTuneErrorFromApply(nil); err == nil {
		t.Fatal("nil-результат должен быть ошибкой")
	}

	busyErr := autoTuneErrorFromApply(&AutoTuneApplyResult{
		Error: "unload failed: model is busy with active inference requests",
	})
	if !errors.Is(busyErr, errAutoTuneReloadBusy) {
		t.Fatalf("busy должен давать errAutoTuneReloadBusy, получили %v", busyErr)
	}

	realErr := autoTuneErrorFromApply(&AutoTuneApplyResult{Error: "load failed: model_file_not_found"})
	if realErr == nil {
		t.Fatal("реальная ошибка не должна теряться")
	}
	if errors.Is(realErr, errAutoTuneReloadBusy) {
		t.Fatalf("реальная ошибка не должна классифицироваться как busy: %v", realErr)
	}
}

// TestR283_BackoffGrowsAndCaps — пауза растёт и упирается в потолок.
func TestR283_BackoffGrowsAndCaps(t *testing.T) {
	// Миллисекунды вместо минут: иначе тест шёл бы час.
	orig := autoTuneDeferSchedule
	autoTuneDeferSchedule = []time.Duration{time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond}
	defer func() { autoTuneDeferSchedule = orig }()

	tr := NewAutoTuneTracker(DefaultAutoTuneCircuitConfig())

	want := []time.Duration{time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond, 3 * time.Millisecond}
	for i, w := range want {
		if got := tr.DeferAutoTune("bk-1", "m"); got != w {
			t.Fatalf("попытка %d: пауза %v, ожидали %v", i+1, got, w)
		}
	}

	// Пока пауза не истекла — «отложено».
	if _, attempts, deferred := tr.AutoTuneDeferredUntil("bk-1", "m"); !deferred || attempts != 4 {
		t.Fatalf("ожидали deferred=true attempts=4, получили deferred=%v attempts=%d", deferred, attempts)
	}

	// По истечении — можно пробовать снова.
	time.Sleep(5 * time.Millisecond)
	if _, _, deferred := tr.AutoTuneDeferredUntil("bk-1", "m"); deferred {
		t.Fatal("после истечения паузы дефер должен сниматься")
	}

	// Явный сброс (успех) очищает состояние.
	tr.DeferAutoTune("bk-1", "m")
	tr.ClearAutoTuneDefer("bk-1", "m")
	if _, _, deferred := tr.AutoTuneDeferredUntil("bk-1", "m"); deferred {
		t.Fatal("ClearAutoTuneDefer должен снимать паузу")
	}

	// nil-трекер безопасен (вызывается из API-хендлеров и тестов).
	var nilTr *AutoTuneTracker
	if d := nilTr.DeferAutoTune("bk-1", "m"); d != 0 {
		t.Errorf("nil-трекер вернул паузу %v", d)
	}
	nilTr.ClearAutoTuneDefer("bk-1", "m")
	if _, _, deferred := nilTr.AutoTuneDeferredUntil("bk-1", "m"); deferred {
		t.Error("nil-трекер не должен сообщать о паузе")
	}
}

// TestR283_DeferredIsNotFailure — главное: «занято» не попадает в circuit
// breaker и не порождает событие «failed», зато выставляет backoff.
func TestR283_DeferredIsNotFailure(t *testing.T) {
	orig := autoTuneDeferSchedule
	autoTuneDeferSchedule = []time.Duration{time.Millisecond}
	defer func() { autoTuneDeferSchedule = orig }()

	p := &Proxy{autoTuneTracker: NewAutoTuneTracker(DefaultAutoTuneCircuitConfig())}
	circuit := &AutoTuneCircuit{}
	circuit.RecordAttempt()

	p.finalizeAutoTuneReload("bk-1", "m", &AutoTuneReloadPlan{Reason: "kv_cache q4_0 → f16"}, circuit, errAutoTuneReloadBusy)

	if circuit.LastError != "" {
		t.Fatalf("«занято» не должно записываться как ошибка circuit'а: %q", circuit.LastError)
	}
	if !circuit.LastSuccess.IsZero() {
		t.Fatal("«занято» не должно считаться успехом")
	}
	if _, _, deferred := p.autoTuneTracker.AutoTuneDeferredUntil("bk-1", "m"); !deferred {
		t.Fatal("после «занято» должен быть выставлен backoff")
	}

	// Наш собственный guard (R280) — тот же класс: deferred, не failure.
	circuit2 := &AutoTuneCircuit{}
	circuit2.RecordAttempt()
	p.finalizeAutoTuneReload("bk-2", "m", nil, circuit2, errAutoTuneReloadDeferred)
	if circuit2.LastError != "" {
		t.Fatalf("guard-отложение не должно быть ошибкой: %q", circuit2.LastError)
	}
}

// TestR283_SuccessClearsBackoffAndRealErrorRecorded — успех снимает паузу,
// реальная ошибка фиксируется.
func TestR283_SuccessClearsBackoffAndRealErrorRecorded(t *testing.T) {
	orig := autoTuneDeferSchedule
	autoTuneDeferSchedule = []time.Duration{time.Hour}
	defer func() { autoTuneDeferSchedule = orig }()

	p := &Proxy{autoTuneTracker: NewAutoTuneTracker(DefaultAutoTuneCircuitConfig())}
	p.autoTuneTracker.DeferAutoTune("bk-1", "m")

	circuit := &AutoTuneCircuit{}
	p.finalizeAutoTuneReload("bk-1", "m", &AutoTuneReloadPlan{Reason: "ok"}, circuit, nil)

	if circuit.LastSuccess.IsZero() {
		t.Fatal("успех должен фиксироваться в circuit")
	}
	if _, _, deferred := p.autoTuneTracker.AutoTuneDeferredUntil("bk-1", "m"); deferred {
		t.Fatal("успех должен снимать backoff")
	}

	circuitErr := &AutoTuneCircuit{}
	p.finalizeAutoTuneReload("bk-1", "m", nil, circuitErr, errors.New("load failed: no such file"))
	if circuitErr.LastError == "" {
		t.Fatal("реальная ошибка должна записываться в circuit")
	}
}

// TestR283_DestructiveMarkerKeptOnlyIfUnloaded — пометка «разрушительная
// операция» обвиняет балансер только если модель РЕАЛЬНО выгружена.
//
// Иначе клиенту сообщают неправду о причине обрыва: пометка ставится перед
// apply, apply падает на «busy», модель никто не выгружал — а следующий
// оборванный стрим получил бы `balancer_autotune`.
func TestR283_DestructiveMarkerKeptOnlyIfUnloaded(t *testing.T) {
	orig := autoTuneDeferSchedule
	autoTuneDeferSchedule = []time.Duration{time.Millisecond}
	defer func() { autoTuneDeferSchedule = orig }()

	p := &Proxy{autoTuneTracker: NewAutoTuneTracker(DefaultAutoTuneCircuitConfig())}

	// R296: БЫЛО — `noteDestructiveOpR271` сама по себе уже давала причину
	// обрыва («пометка должна ставиться перед apply»). СТАЛО — начало операции
	// причиной не является: нужен подтверждённый unload.
	p.noteDestructiveOpR271("bk-1", "m", destructiveAutotuneR271)
	if kind, ok := p.recentDestructiveOpR271("bk-1", "m", time.Minute); ok {
		t.Fatalf("начало операции не должно обвинять балансер (kind=%q)", kind)
	}
	// Модель подтверждённо сняли — вот теперь причина наша.
	p.confirmDestructiveOpUnloadedR271("bk-1", "m")
	if kind, ok := p.recentDestructiveOpR271("bk-1", "m", time.Minute); !ok || kind != destructiveAutotuneR271 {
		t.Fatalf("после подтверждённой выгрузки пометка должна объяснять обрыв (kind=%q ok=%v)", kind, ok)
	}

	// Применение не состоялось: выгрузки не было → пометки нет.
	p.clearDestructiveOpR271("bk-1", "m")
	if _, ok := p.recentDestructiveOpR271("bk-1", "m", time.Minute); ok {
		t.Fatal("после неудавшейся выгрузки пометка обязана сниматься")
	}

	// Применение состоялось (выгрузка была) → пометку не снимаем.
	p.noteDestructiveOpR271("bk-2", "m", destructiveAutotuneR271)
	p.confirmDestructiveOpUnloadedR271("bk-2", "m")
	if kind, ok := p.recentDestructiveOpR271("bk-2", "m", time.Minute); !ok || kind != destructiveAutotuneR271 {
		t.Fatalf("после реальной выгрузки пометка должна оставаться (kind=%q ok=%v)", kind, ok)
	}
}

// TestR283_GuardedApplyForAPISide — ручной путь (API) тоже под guard'ом.
//
// БЫЛО: обработчик /api/v1/admin/autotune/{id}/apply вызывал ApplyAutoTunePlan
// напрямую — кнопка оператора могла выгрузить модель под живым стримом, а
// «busy» от воркера записывался в circuit breaker как ошибка.
func TestR283_GuardedApplyForAPISide(t *testing.T) {
	p := autotuneProxyR280(t)
	t.Setenv("LB_RELOAD_DEFER_MAX_SEC", "0")

	plan := &AutoTuneReloadPlan{ModelName: "qwen3.8", KVCacheType: "f16", Reason: "manual"}

	// Идёт стрим → операция даже не начинается.
	endStream := beginClientStreamR271(p, "bk-1", "qwen3.8")
	res, deferred := p.ApplyAutoTunePlanGuarded("bk-1", plan)
	if !deferred || res != nil {
		endStream()
		t.Fatalf("под активным стримом ожидали deferred=true res=nil, получили deferred=%v res=%+v", deferred, res)
	}
	endStream()

	// Стримов нет → попытка идёт (воркер недоступен, но это уже не «deferred»).
	res2, deferred2 := p.ApplyAutoTunePlanGuarded("bk-1", plan)
	if deferred2 {
		t.Fatalf("без стримов не должно быть deferred: %+v", res2)
	}
	if res2 == nil {
		t.Fatal("ожидали результат применения")
	}
	// Выгрузка не состоялась → пометка не должна «висеть» и портить причины
	// обрывов следующих стримов.
	if kind, ok := p.recentDestructiveOpR271("bk-1", "qwen3.8", time.Minute); ok {
		t.Fatalf("при неудавшейся выгрузке пометка не должна оставаться (kind=%q)", kind)
	}
}

// TestR283_TriggerSkippedDuringBackoff — пока идёт пауза, повторного анализа и
// попытки apply нет вовсе (именно это убирает петлю «каждые 70 секунд»).
func TestR283_TriggerSkippedDuringBackoff(t *testing.T) {
	p := &Proxy{autoTuneTracker: NewAutoTuneTracker(DefaultAutoTuneCircuitConfig())}
	p.autoTuneTracker.DeferAutoTune("bk-1", "gemma-4-E4B-it-Q4_K_M")

	res := p.triggerAutoTuneReload("bk-1", "gemma-4-E4B-it-Q4_K_M", 0, 0, 0)
	if res == nil {
		t.Fatal("ожидали результат проверки")
	}
	if res.Triggered {
		t.Fatal("во время паузы перезагрузка не должна запускаться")
	}
	if !strings.Contains(res.SkippedReason, "deferred") {
		t.Fatalf("причина должна объяснять паузу, получили %q", res.SkippedReason)
	}
}
