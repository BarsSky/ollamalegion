// autotune_defer_r283.go — R283 (2026-10-29): AutoTune не путает «сейчас занято»
// с «сломалось» и не долбится в воркер каждые 70 секунд.
//
// ЖИВОЙ ФАКТ (стенд r282, живой лог за 20 минут). AutoTune срабатывал ≥4 раз
// (`R54.4: kv_cache q4_0 → f16`), и один раз УПАЛ:
//
//	autotune: async reload failed … unload failed: model 'gemma-4-E4B-it-Q4_K_M'
//	is busy with active inference requests
//
// То есть отказ пришёл от cppworker (модель занята живым инференсом), а не от
// нашего guard'а R280. После этого AutoTune повторял попытку каждые ~70 секунд
// и каждый раз писал ERROR «reload failed» — хотя по смыслу это «сейчас нельзя,
// модель занята».
//
// ПОЧЕМУ ТАК. Горутина reload'а различала ровно два исхода: `nil` (успех) и
// `err != nil` (сбой → RecordError + событие «failed» + cool-down). Третий,
// самый частый на живом стенде исход — «воркер занят» — попадал в «сбой».
// Наш собственный guard (waitStreamsIdleR271) этого не ловит и не должен:
// он знает только про стримы, которые видит САМ балансер, а воркер в момент
// нашей попытки может ещё доигрывать чужой запрос (или освобождать слот после
// завершённого стрима) — для нас стримов уже нет, для воркера модель занята.
//
// РЕШЕНИЕ. Три исхода вместо двух:
//
//	applied  — перезагрузка прошла (RecordSuccess, backoff сброшен);
//	deferred — занято (воркер busy или наш guard): НЕ ошибка, НЕ в circuit
//	           breaker, без события «failed», с экспоненциальным backoff;
//	failed   — реальная ошибка конфигурации/воркера: RecordError + событие.
//
// Backoff отличает «дело не в модели, а в моменте»: 2 → 5 → 10 → 30 → 60 минут
// (потолок), и сбрасывается первым же успехом. Петля «каждые 70 с вечно»
// превращалась в спам error-логов и в растущий cool-down там, где всё в порядке.
package balancer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// errAutoTuneReloadBusy — перезагрузка отложена: модель занята (воркер ответил
// «busy with active inference requests»). Отдельный сентинел, а не текст: по
// нему горутина отличает «отложили» от «сломалось».
var errAutoTuneReloadBusy = errors.New("autotune reload deferred: model is busy with active inference")

// autoTuneDeferSchedule — пауза между попытками после «занято».
//
// Переменная, а не константа: тесты подменяют её миллисекундами, иначе проверка
// backoff'а занимала бы час. В production не меняется.
var autoTuneDeferSchedule = []time.Duration{
	2 * time.Minute,
	5 * time.Minute,
	10 * time.Minute,
	30 * time.Minute,
	60 * time.Minute,
}

// autoTuneDeferState — состояние backoff'а для пары (backend, model).
type autoTuneDeferState struct {
	until    time.Time
	attempts int // сколько раз подряд отложили (индекс в расписании)
}

// busyMarkers — подстроки, которыми воркер и мы сами сообщаем «занято».
//
// Список короткий и явный: «model is busy with active inference requests» —
// текст cppworker (cmd/cppworker/handlers_model.go:1068, :2442);
// «reload already in progress» — наш собственный маркер dedup-регистра
// (R230): это тоже «момент», а не поломка.
var busyMarkers = []string{
	"busy with active inference",
	"model is busy",
	"active inference requests",
	"reload already in progress",
}

// isAutoTuneBusyError — «занято» ли это (а не сбой).
func isAutoTuneBusyError(msg string) bool {
	if msg == "" {
		return false
	}
	low := strings.ToLower(msg)
	for _, m := range busyMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// autoTuneErrorFromApply — превратить неуспешный ApplyAutoTunePlan в ошибку
// нужного класса: busy → сентинел (deferred), иначе обычная ошибка (failed).
//
// Вынесено отдельной функцией, потому что именно здесь раньше терялась разница
// между «занято» и «сломалось»: результат apply проверялся на Success и всё
// остальное сваливалось в fmt.Errorf.
func autoTuneErrorFromApply(res *AutoTuneApplyResult) error {
	if res == nil {
		return fmt.Errorf("autotune: apply plan returned nil result")
	}
	if res.Success {
		return nil
	}
	if isAutoTuneBusyError(res.Error) {
		return errAutoTuneReloadBusy
	}
	return fmt.Errorf("autotune: apply plan failed: %s", res.Error)
}

// --- backoff на AutoTuneTracker -------------------------------------------------
//
// Живёт в трекере, а не в Proxy: трекер уже владеет per-(backend,model) состоянием
// и мьютексом, и его создаёт тот же код, что и circuit breaker. Отдельная карта
// в Proxy означала бы ещё одно место, где состояние AutoTune живёт по-своему.

// deferBackoffFor — задержка для указанного числа подряд отложенных попыток.
func deferBackoffFor(attempts int) time.Duration {
	if len(autoTuneDeferSchedule) == 0 {
		return 0
	}
	if attempts <= 0 {
		return autoTuneDeferSchedule[0]
	}
	if attempts >= len(autoTuneDeferSchedule) {
		return autoTuneDeferSchedule[len(autoTuneDeferSchedule)-1]
	}
	return autoTuneDeferSchedule[attempts]
}

// DeferAutoTune — отложить AutoTune для пары (backend, model) на очередную
// паузу backoff'а. Возвращает срок паузы (для лога/ответа).
func (t *AutoTuneTracker) DeferAutoTune(backendID, modelName string) time.Duration {
	if t == nil {
		return 0
	}
	key := backendID + "|" + modelName
	t.mu.Lock()
	if t.defers == nil {
		t.defers = make(map[string]*autoTuneDeferState)
	}
	st := t.defers[key]
	if st == nil {
		st = &autoTuneDeferState{}
		t.defers[key] = st
	}
	delay := deferBackoffFor(st.attempts)
	st.attempts++
	st.until = time.Now().Add(delay)
	t.mu.Unlock()
	return delay
}

// AutoTuneDeferredUntil — сколько ещё ждать и сколько раз подряд откладывали.
// ok=false означает «паузы нет, можно пробовать».
func (t *AutoTuneTracker) AutoTuneDeferredUntil(backendID, modelName string) (wait time.Duration, attempts int, ok bool) {
	if t == nil {
		return 0, 0, false
	}
	key := backendID + "|" + modelName
	t.mu.RLock()
	st := t.defers[key]
	t.mu.RUnlock()
	if st == nil {
		return 0, 0, false
	}
	if wait = time.Until(st.until); wait > 0 {
		return wait, st.attempts, true
	}
	return 0, st.attempts, false
}

// ClearAutoTuneDefer — сбросить backoff (успех или ручное применение оператором).
func (t *AutoTuneTracker) ClearAutoTuneDefer(backendID, modelName string) {
	if t == nil {
		return
	}
	key := backendID + "|" + modelName
	t.mu.Lock()
	delete(t.defers, key)
	t.mu.Unlock()
}

// finalizeAutoTuneReload — единая обработка исхода перезагрузки.
//
// Раньше эта логика была тремя ветками внутри горутины, и третий исход
// (занято) просто не существовал. Теперь исход один из трёх, и все решения
// (breaker, событие, backoff, лог) принимаются ЗДЕСЬ — чтобы новый путь не
// смог однажды снова записать «занято» как «failed».
func (p *Proxy) finalizeAutoTuneReload(backendID, modelName string, plan *AutoTuneReloadPlan, circuit *AutoTuneCircuit, err error) {
	if p == nil {
		return
	}
	reason := ""
	if plan != nil {
		reason = plan.Reason
	}

	switch {
	case err == nil:
		if circuit != nil {
			circuit.RecordSuccess()
		}
		if p.autoTuneTracker != nil {
			p.autoTuneTracker.ResetCircuit(backendID, modelName)
			p.autoTuneTracker.ClearAutoTuneDefer(backendID, modelName)
		}
		logger.Get().Infow("autotune: async reload succeeded",
			"backend", backendID, "model", modelName, "reason", reason)
		p.publishAutoTuneEvent(autoTuneSucceededEvent(backendID, modelName, plan))

	case errors.Is(err, errAutoTuneReloadBusy) || errors.Is(err, errAutoTuneReloadDeferred):
		// Занято/отложено: НЕ сбой. Ни RecordError, ни события «failed»,
		// иначе оператор видит растущий cool-down там, где всё в порядке.
		var delay time.Duration
		if p.autoTuneTracker != nil {
			delay = p.autoTuneTracker.DeferAutoTune(backendID, modelName)
		}
		logger.Get().Infow("autotune reload deferred (busy|streams): retry in %s",
			"backend", backendID, "model", modelName,
			"reason", reason, "retry_in", delay.String(),
			"cause", err.Error())

	default:
		if circuit != nil {
			circuit.RecordError(err.Error())
		}
		logger.Get().Warnw("autotune reload failed",
			"backend", backendID, "model", modelName, "error", err)
		p.publishAutoTuneEvent(autoTuneFailedEvent(backendID, modelName, circuit, err))
	}
}

// --- экспорт для API-слоя ---------------------------------------------------
//
// Ручное применение плана (`POST /api/v1/admin/autotune/{id}/apply`) шло МИМО
// guard'а R271: обработчик вызывал ApplyAutoTunePlan напрямую. То есть
// операторская кнопка могла выгрузить модель под живым стримом (клиент получал
// обрыв без пометки причины — «upstream ended», хотя upstream жив), а ответ
// воркера «busy» записывался в circuit breaker как ошибка.
//
// Экспортируем ровно один вход, чтобы API не собирал ту же последовательность
// сам (иначе появится четвёртое место, где guard забыт).

// ApplyAutoTunePlanGuarded — применить план с guard'ом R271/R280.
//
// deferred=true означает: на бэкенде идут стримы, операция НЕ начиналась
// (ни unload, ни load). Это не ошибка — вызывающий должен ответить 503 с
// причиной и предложением повторить, а не записывать это в breaker.
func (p *Proxy) ApplyAutoTunePlanGuarded(backendID string, plan *AutoTuneReloadPlan) (res *AutoTuneApplyResult, deferred bool) {
	if p == nil || plan == nil {
		return &AutoTuneApplyResult{Error: "nil plan"}, false
	}
	// R291: ручной «Применить план» из WebUI не должен думать две минуты:
	// при активных стримах отвечаем deferred сразу (решение уже известно).
	if ok, _ := p.deferDestructiveOpR291(context.Background(), backendID, plan.ModelName, "autotune-apply"); !ok {
		return nil, true
	}
	p.noteDestructiveOpR271(backendID, plan.ModelName, destructiveAutotuneR271)
	res = p.ApplyAutoTunePlan(backendID, plan)
	if res != nil && res.Unloaded {
		// R296: модель РЕАЛЬНО сняли — только теперь обрыв чужого стрима можно
		// объяснять нашей автоперезагрузкой (а не самой пометкой «начали»).
		p.confirmDestructiveOpUnloadedR271(backendID, plan.ModelName)
	}
	if res == nil || (!res.Success && !res.Unloaded) {
		// Ничего разрушительного не произошло — пометку снимаем, иначе
		// следующий оборванный стрим объявят «нашей» автоперезагрузкой.
		p.clearDestructiveOpR271(backendID, plan.ModelName)
	}
	return res, false
}

// IsAutoTuneBusyError — «занято» ли это (экспорт той же классификации, что
// использует авто-путь). Нужен API-слою, чтобы не писать busy в breaker.
func IsAutoTuneBusyError(msg string) bool { return isAutoTuneBusyError(msg) }

// autoTuneSucceededEvent — событие успеха (тот же набор полей, что был в
// горутине triggerAutoTuneReload: причина, окно, kv-cache, слои).
func autoTuneSucceededEvent(backendID, modelName string, plan *AutoTuneReloadPlan) types.Event {
	ev := types.Event{
		Type:      types.EventAutoTuneReloadSucceeded,
		Timestamp: time.Now(),
		BackendID: backendID,
		Model:     modelName,
		Severity:  types.SeverityInfo,
		Source:    "autotune",
		Message:   "AutoTune reload succeeded",
	}
	if plan != nil {
		ev.Message = "AutoTune reload succeeded: " + plan.Reason
		ev.Data = map[string]interface{}{
			"contextSize":  plan.ContextSize,
			"kvCacheType":  plan.KVCacheType,
			"numGpuLayers": plan.NumGPULayers,
			"reason":       plan.Reason,
		}
	}
	return ev
}

// autoTuneFailedEvent — событие реального сбоя (только для исхода failed;
// «занято» событий не порождает, чтобы история AutoTune не врала).
func autoTuneFailedEvent(backendID, modelName string, circuit *AutoTuneCircuit, err error) types.Event {
	msg := "AutoTune reload failed"
	data := map[string]interface{}{}
	if err != nil {
		msg += ": " + err.Error()
		data["error"] = err.Error()
	}
	if circuit != nil {
		data["circuitErrors"] = circuit.LastError
	}
	return types.Event{
		Type:      types.EventAutoTuneReloadFailed,
		Timestamp: time.Now(),
		BackendID: backendID,
		Model:     modelName,
		Severity:  types.SeverityWarning,
		Source:    "autotune",
		Message:   msg,
		Data:      data,
	}
}
