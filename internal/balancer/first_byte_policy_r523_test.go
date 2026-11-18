//go:build llama_stub

// first_byte_policy_r523_test.go — R523 (2026-11-19, §7 плана, S1/T-1):
// граница «нет первого байта» ОДНА на всех путях и считается по размеру модели.
//
// ЖИВОЙ ФАКТ ИЗ АУДИТА: плоские 180 с против живых холодных загрузок 140–485 с.
// R498 ввёл оценку по размеру модели, но применил её только в
// llamacpp_transport.go; четыре других пути (llamacpp_handlers_inference.go:347,615,
// proxy_request_openai_auto_stream.go:105, llamacpp_transport.go:373) продолжали
// возвращать 0 = «ждать бесконечно». Один и тот же запрос обрывался или не
// обрывался в зависимости от поверхности — ровно то расхождение, которое собирает
// аудит S1.
package balancer

import (
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

func r523Proxy() *Proxy {
	return &Proxy{config: &types.LoadBalancerConfig{}}
}

// TestR523_UnknownSizeIsBoundedNotZero — размер неизвестен: граница есть (оценка
// 900 с), а не ноль.
func TestR523_UnknownSizeIsBoundedNotZero(t *testing.T) {
	t.Setenv("LB_STREAMING_NEVER_TIMEOUT", "")
	t.Setenv("LB_FIRST_BYTE_TIMEOUT_SEC", "")
	t.Setenv("LB_FIRST_BYTE_TIMEOUT_DEFAULT_SEC", "")
	restore := setModelSizeBytesForTest(func(string) int64 { return 0 })
	defer restore()

	got := r523Proxy().getModelFirstByteTimeout("unknown-model")
	if got != EstimateFirstByteTimeoutFromModelSize(0) {
		t.Fatalf("граница = %v, ожидалась оценка эстиматора %v", got, EstimateFirstByteTimeoutFromModelSize(0))
	}
	if got <= 0 {
		t.Fatalf("граница %v: ноль означает «ждать бесконечно» — это и было расхождение с R498", got)
	}
}

// TestR523_KnownSizeOutlivesColdLoad — 16 ГБ: 1800 с, загрузка 140–485 с влезает.
func TestR523_KnownSizeOutlivesColdLoad(t *testing.T) {
	t.Setenv("LB_STREAMING_NEVER_TIMEOUT", "")
	t.Setenv("LB_FIRST_BYTE_TIMEOUT_SEC", "")
	t.Setenv("LB_FIRST_BYTE_TIMEOUT_DEFAULT_SEC", "")
	restore := setModelSizeBytesForTest(func(string) int64 { return 16 * 1024 * 1024 * 1024 })
	defer restore()

	got := r523Proxy().getModelFirstByteTimeout("qwen3.8-27b-q4")
	if got != 1800*time.Second {
		t.Fatalf("граница = %v, ожидалось 30m по оценке 16 ГБ", got)
	}
	if got < 485*time.Second {
		t.Fatalf("граница %v меньше наблюдаемой холодной загрузки 485 с", got)
	}
}

// TestR523_SameAsColdLoadPath — обе функции обязаны вернуть одно значение:
// одна политика, второй копии нет.
func TestR523_SameAsColdLoadPath(t *testing.T) {
	t.Setenv("LB_STREAMING_NEVER_TIMEOUT", "")
	t.Setenv("LB_FIRST_BYTE_TIMEOUT_SEC", "")
	t.Setenv("LB_FIRST_BYTE_TIMEOUT_DEFAULT_SEC", "")
	restore := setModelSizeBytesForTest(func(string) int64 { return 4 * 1024 * 1024 * 1024 })
	defer restore()

	p := r523Proxy()
	if a, b := p.getModelFirstByteTimeout("m"), p.defaultFirstByteTimeoutForModelR498("m"); a != b {
		t.Fatalf("политики разошлись: getModelFirstByteTimeout=%v, defaultFirstByteTimeoutForModelR498=%v", a, b)
	}
}

// TestR523_ProfileAndEnvWin — осознанные настройки сильнее оценки.
func TestR523_ProfileAndEnvWin(t *testing.T) {
	t.Setenv("LB_STREAMING_NEVER_TIMEOUT", "")
	t.Setenv("LB_FIRST_BYTE_TIMEOUT_SEC", "")
	t.Setenv("LB_FIRST_BYTE_TIMEOUT_DEFAULT_SEC", "")
	restore := setModelSizeBytesForTest(func(string) int64 { return 16 * 1024 * 1024 * 1024 })
	defer restore()

	p := r523Proxy()
	p.ReplaceModelProfiles(map[string]types.LlamaCppModelProfile{
		"fast-model": {FirstByteTimeoutSec: 55},
	})
	// Профильные таймауты — осознанный opt-in (LB_ALLOW_PROFILE_TIMEOUTS=1):
	// без него значение профиля игнорируется, и это тоже проверяем.
	if got := p.getModelFirstByteTimeout("fast-model"); got == 55*time.Second {
		t.Fatalf("профиль применился без LB_ALLOW_PROFILE_TIMEOUTS — политика opt-in нарушена")
	}
	t.Setenv("LB_ALLOW_PROFILE_TIMEOUTS", "1")
	if got := p.getModelFirstByteTimeout("fast-model"); got != 55*time.Second {
		t.Fatalf("профиль не применён при opt-in: %v, ожидалось 55s", got)
	}
	t.Setenv("LB_FIRST_BYTE_TIMEOUT_SEC", "42")
	if got := p.getModelFirstByteTimeout("fast-model"); got != 42*time.Second {
		t.Fatalf("env не сильнее профиля: %v, ожидалось 42s", got)
	}
	t.Setenv("LB_STREAMING_NEVER_TIMEOUT", "1")
	if got := p.getModelFirstByteTimeout("fast-model"); got != 0 {
		t.Fatalf("осознанное «никогда» не применено: %v, ожидалось 0", got)
	}
}
