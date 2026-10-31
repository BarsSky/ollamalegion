// metrics_outcome_r358_test.go — R358 (2026-10-31): счётчики отмен.
//
// ЖИВОЙ ДЕФЕКТ (аудит abort, пункт A8 — «ни одного счётчика отмены»). Отмена не
// считалась нигде: isBackendFailure() исключает bridge.ErrAborted из ОШИБОК, а другого
// места для неё не было. Поэтому генерация, остановленная клиентом (кнопка Stop, обрыв,
// отмена балансером), попадала в метрики как УСПЕХ: TotalRequests +1, TotalErrors +0.
//
// Следствие для оператора: /metrics и /api/infer/metrics показывали «всё хорошо», даже
// когда половина запросов обрывалась клиентом, и отличить «модель медленная, клиент не
// дождался» от «модель отвечает нормально» было нечем.
//
// R358 вводит третий исход (OutcomeCancelled) и отдельные счётчики:
//   - общий TotalCancelled и per-model Cancelled;
//   - в /api/infer/metrics — totals.cancelled / cancel_rate и models[].cancelled;
//   - в /metrics — cppworker_generation_cancelled_total{model} и cppworker_cancelled_total.
//
// Проверяется:
//  1. отмена считается ОТДЕЛЬНО и не попадает ни в ошибки, ни в «успехи»;
//  2. обычная ошибка по-прежнему увеличивает только Errors;
//  3. успех не увеличивает ни Errors, ни Cancelled;
//  4. per-model счётчик отмен считается по своей модели;
//  5. отображение исхода для метрик: ErrAborted → cancelled, прочие ошибки → error.
package cppbackend

import (
	"errors"
	"testing"
	"time"

	"ollama-loadbalancer/c/bridge"
)

// TestR358_CancelledCountedSeparately — отмена не смешивается с ошибками и успехом.
func TestR358_CancelledCountedSeparately(t *testing.T) {
	m := NewMetrics()

	m.RecordRequestOutcome("gemma", 10, time.Second, OutcomeSuccess)
	m.RecordRequestOutcome("gemma", 5, time.Second, OutcomeCancelled)
	m.RecordRequestOutcome("gemma", 0, time.Second, OutcomeError)

	if got := m.TotalRequests.Load(); got != 3 {
		t.Errorf("TotalRequests = %d, want 3 (отмена — тоже запрос)", got)
	}
	if got := m.TotalCancelled.Load(); got != 1 {
		t.Errorf("TotalCancelled = %d, want 1: без этого счётчика Stop клиента невидим "+
			"ни в /metrics, ни в /api/infer/metrics (аудит A8)", got)
	}
	if got := m.TotalErrors.Load(); got != 1 {
		t.Errorf("TotalErrors = %d, want 1: отмена не должна раздувать error_rate", got)
	}

	snap := m.GetModelMetricsSnapshot()["gemma"]
	if snap.Cancelled != 1 {
		t.Errorf("models[gemma].cancelled = %d, want 1", snap.Cancelled)
	}
	if snap.Errors != 1 {
		t.Errorf("models[gemma].errors = %d, want 1", snap.Errors)
	}
	if snap.Requests != 3 {
		t.Errorf("models[gemma].requests = %d, want 3", snap.Requests)
	}
}

// TestR358_SuccessDoesNotCountCancelOrError — успех не задевает новые счётчики.
func TestR358_SuccessDoesNotCountCancelOrError(t *testing.T) {
	m := NewMetrics()
	m.RecordRequest("gemma", 10, time.Second, true) // совместимый вход

	if m.TotalCancelled.Load() != 0 || m.TotalErrors.Load() != 0 {
		t.Errorf("успех дал errors=%d cancelled=%d, want 0/0",
			m.TotalErrors.Load(), m.TotalCancelled.Load())
	}
	if m.TotalRequests.Load() != 1 {
		t.Errorf("TotalRequests = %d, want 1", m.TotalRequests.Load())
	}
}

// TestR358_CancelledIsPerModel — отмена считается по своей модели.
func TestR358_CancelledIsPerModel(t *testing.T) {
	m := NewMetrics()
	m.RecordRequestOutcome("a", 1, time.Second, OutcomeCancelled)
	m.RecordRequestOutcome("b", 1, time.Second, OutcomeSuccess)

	snap := m.GetModelMetricsSnapshot()
	if snap["a"].Cancelled != 1 {
		t.Errorf("модель a: cancelled = %d, want 1", snap["a"].Cancelled)
	}
	if snap["b"].Cancelled != 0 {
		t.Errorf("модель b: cancelled = %d, want 0 (чужая отмена не должна прилипать)", snap["b"].Cancelled)
	}
}

// TestR358_OutcomeMapping — как ошибка отображения превращается в исход метрики.
func TestR358_OutcomeMapping(t *testing.T) {
	if got := metricsOutcomeR358(nil); got != OutcomeSuccess {
		t.Errorf("nil → %v, want success", got)
	}
	if got := metricsOutcomeR358(bridge.ErrAborted); got != OutcomeCancelled {
		t.Errorf("ErrAborted → %v, want cancelled: именно этот случай раньше "+
			"приклеивался к успеху", got)
	}
	if got := metricsOutcomeR358(errors.New("backend unreachable")); got != OutcomeError {
		t.Errorf("обычная ошибка → %v, want error", got)
	}
	if got := metricsOutcomeR358(OutcomeWrapError{}); got != OutcomeError {
		t.Errorf("обёрнутая ошибка → %v, want error", got)
	}
	// Обёрнутый ErrAborted тоже должен распознаваться (errors.Is).
	if got := metricsOutcomeR358(wrapErr(bridge.ErrAborted)); got != OutcomeCancelled {
		t.Errorf("обёрнутый ErrAborted → %v, want cancelled", got)
	}
}

// OutcomeWrapError — простая ошибка для проверки ветки «иное».
type OutcomeWrapError struct{}

func (OutcomeWrapError) Error() string { return "some failure" }

// wrapErr — обёртка с Unwrap (аналог fmt.Errorf("%w")).
func wrapErr(err error) error { return wrappedErr{err: err} }

type wrappedErr struct{ err error }

func (w wrappedErr) Error() string { return "wrapped: " + w.err.Error() }
func (w wrappedErr) Unwrap() error { return w.err }
