package balancer

import (
	"testing"
	"time"
)

// R547 (T-9 аудита docs/plans/2026-11-19-consistency-audit.md §1.4).
//
// ЖИВОЙ ДЕФЕКТ. Бюджет ожидания lazy-загрузки был КОНСТАНТОЙ:
// `loadingRetryMaxAttempts = 60` × `loadingRetryInterval = 500ms` = 30 секунд, а
// комментарий рядом утверждал «достаточно для cold-start 22GB». Живые числа того
// же репозитория (autoload_wait.go, docs/architecture/debts.ru.md): загрузка
// 16–27B занимает 140–485 с. Итог: клиент получал
//   {"error":"model is still loading after 30s: <model>"}
// ЗАДОЛГО до того, как модель успевала подняться, и повторял запрос вслепую.
//
// После правки бюджет берётся из того же эстиматора по размеру модели, что и
// граница первого байта (R498/R523), а явная настройка
// `LB_LOADING_RETRY_MAX_SEC` сильнее оценки.

// TestR547_BudgetFromModelSize — 16 ГБ модель: 1800 с, а не 30.
func TestR547_BudgetFromModelSize(t *testing.T) {
	t.Setenv("LB_LOADING_RETRY_MAX_SEC", "")
	restore := setModelSizeBytesForTest(func(string) int64 { return 16 * 1024 * 1024 * 1024 })
	defer restore()

	p := &Proxy{}
	got, source := p.loadingRetryBudget("qwen3.8-27b-q4")

	want := EstimateFirstByteTimeoutFromModelSize(16 * 1024 * 1024 * 1024)
	if got != want {
		t.Fatalf("бюджет = %v, ожидалось %v (оценка по 16 ГБ), источник %q", got, want, source)
	}
	if got <= 30*time.Second {
		t.Fatalf("бюджет %v не больше прежних 30 с — загрузка 16 ГБ (140–485 с) снова не успеет", got)
	}
	if sec := p.loadingRetryBudgetSeconds("qwen3.8-27b-q4"); sec <= 30 {
		t.Fatalf("текст отказа назвал бы %d с — это и был дефект T-9", sec)
	}
}

// TestR547_UnknownSizeIsNotFast — размер неизвестен: 900 с (оценка для
// неизвестного размера), а не 30 и не ноль.
func TestR547_UnknownSizeIsNotFast(t *testing.T) {
	t.Setenv("LB_LOADING_RETRY_MAX_SEC", "")
	restore := setModelSizeBytesForTest(func(string) int64 { return 0 })
	defer restore()

	p := &Proxy{}
	got, _ := p.loadingRetryBudget("неизвестная-модель")
	if want := EstimateFirstByteTimeoutFromModelSize(0); got != want {
		t.Fatalf("бюджет = %v, ожидалось %v (оценка для неизвестного размера)", got, want)
	}
	if got <= time.Minute {
		t.Fatalf("бюджет %v слишком мал: «размер неизвестен» не значит «модель маленькая»", got)
	}
}

// TestR547_EnvWins — явная настройка сильнее оценки.
func TestR547_EnvWins(t *testing.T) {
	t.Setenv("LB_LOADING_RETRY_MAX_SEC", "600")
	restore := setModelSizeBytesForTest(func(string) int64 { return 16 * 1024 * 1024 * 1024 })
	defer restore()

	p := &Proxy{}
	got, source := p.loadingRetryBudget("qwen3.8-27b-q4")
	if got != 600*time.Second {
		t.Fatalf("бюджет = %v, ожидалось 600s из LB_LOADING_RETRY_MAX_SEC (источник %q)", got, source)
	}
	if source != "env LB_LOADING_RETRY_MAX_SEC" {
		t.Fatalf("источник = %q, ожидался env (оператор должен видеть, что число его)", source)
	}
}

// TestR547_EnvBelowMinimumIsClamped — мусорные и слишком малые значения не
// обрывают ожидание раньше 30 с.
func TestR547_EnvBelowMinimumIsClamped(t *testing.T) {
	restore := setModelSizeBytesForTest(func(string) int64 { return 0 })
	defer restore()
	p := &Proxy{}

	for _, raw := range []string{"5", "-1", "abc"} {
		t.Setenv("LB_LOADING_RETRY_MAX_SEC", raw)
		got, source := p.loadingRetryBudget("m")
		if raw == "5" {
			if got != loadingRetryMinBudget {
				t.Fatalf("LB_LOADING_RETRY_MAX_SEC=5 → бюджет %v, ожидалась нижняя граница %v", got, loadingRetryMinBudget)
			}
			if source != "env LB_LOADING_RETRY_MAX_SEC" {
				t.Fatalf("источник = %q, ожидался env", source)
			}
			continue
		}
		// Невалидное значение = настройки нет → работает оценка.
		if want := EstimateFirstByteTimeoutFromModelSize(0); got != want {
			t.Fatalf("LB_LOADING_RETRY_MAX_SEC=%q → бюджет %v, ожидалась оценка %v", raw, got, want)
		}
	}
}

// TestR547_StreamingNeverTimeoutDoesNotDisableLoadWait — мастер-выключатель
// таймаутов СТРИМА не должен отключать границу ожидания загрузки (у них разные
// предметы; раньше соблазн был велик — общий хелпер envSeconds).
func TestR547_StreamingNeverTimeoutDoesNotDisableLoadWait(t *testing.T) {
	t.Setenv("LB_STREAMING_NEVER_TIMEOUT", "1")
	t.Setenv("LB_LOADING_RETRY_MAX_SEC", "")
	restore := setModelSizeBytesForTest(func(string) int64 { return 8 * 1024 * 1024 * 1024 })
	defer restore()

	p := &Proxy{}
	got, _ := p.loadingRetryBudget("m")
	if got <= 0 {
		t.Fatalf("LB_STREAMING_NEVER_TIMEOUT=1 обнулил бюджет ожидания загрузки — это разные величины")
	}
	if want := EstimateFirstByteTimeoutFromModelSize(8 * 1024 * 1024 * 1024); got != want {
		t.Fatalf("бюджет = %v, ожидалось %v", got, want)
	}
}
