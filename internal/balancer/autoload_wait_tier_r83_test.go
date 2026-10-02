//go:build llama_stub

// autoload_wait_tier_r83_test.go — R83 §9.1 (2026-09-26).
//
// Живой инцидент: 16.4 GB модель (Qwen3.8-27B) на bind-mount грузится 7-9 минут.
// Жёсткие 180 с обрывали ожидание, клиент получал «auto-load failed», повторял
// запрос и запускал ВТОРУЮ загрузку. «Датчик бездействия» лечит это, пока
// прогресс доступен; тест фиксирует резервный тир по размеру модели.
package balancer

import (
	"testing"
	"time"
)

// setModelSize подменяет источник размера модели для теста (пакетный шов).
// Возвращает функцию восстановления — вызывающий обязан её вызвать (defer),
// потому что вложенные t.Cleanup выполняются только после всех сабтестов.
func setModelSize(size int64) func() {
	prev := modelSizeBytesForTest
	modelSizeBytesForTest = func(string) int64 { return size }
	return func() { modelSizeBytesForTest = prev }
}

// TestR83_AutoLoadWaitTier_ByModelSize — бюджет растёт с размером модели.
func TestR83_AutoLoadWaitTier_ByModelSize(t *testing.T) {
	t.Setenv("LB_AUTO_LOAD_WAIT_SEC", "")
	p := &Proxy{}

	cases := []struct {
		name     string
		bytes    int64
		wantMin  time.Duration
		wantMax  time.Duration
		rational string
	}{
		{"4 GB", 4 << 30, 3 * time.Minute, 4 * time.Minute, "4 GB на 20 MB/s ≈ 3.4 мин"},
		{"8 GB", 8 << 30, 6 * time.Minute, 8 * time.Minute, "8 GB ≈ 6.8 мин"},
		{"16.4 GB", 16464440224, 10 * time.Minute, 15 * time.Minute, "27B Q4_K_M ≈ 13 мин"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			restore := setModelSize(tc.bytes)
			defer restore()
			got := p.autoLoadWaitTimeoutForModel("qwen3.8:latest")
			if got < tc.wantMin || got > tc.wantMax {
				t.Errorf("бюджет = %s, want в диапазоне %s…%s (%s)",
					got, tc.wantMin, tc.wantMax, tc.rational)
			}
		})
	}
}

// TestR83_AutoLoadWaitTier_ExplicitEnvWins — операторский LB_AUTO_LOAD_WAIT_SEC
// уважается и не перебивается тиром.
func TestR83_AutoLoadWaitTier_ExplicitEnvWins(t *testing.T) {
	p := &Proxy{}
	restore := setModelSize(16 * 1024 * 1024 * 1024)
	defer restore()

	t.Setenv("LB_AUTO_LOAD_WAIT_SEC", "42")
	if got := p.autoLoadWaitTimeoutForModel("m"); got != 42*time.Second {
		t.Errorf("явный env: %s, want 42s", got)
	}
	t.Setenv("LB_AUTO_LOAD_WAIT_SEC", "0")
	if got := p.autoLoadWaitTimeoutForModel("m"); got != 0 {
		t.Errorf("0 = не ждать: %s, want 0", got)
	}
	t.Setenv("LB_AUTO_LOAD_WAIT_SEC", "-1")
	if got := p.autoLoadWaitTimeoutForModel("m"); got != autoLoadHardCeiling {
		t.Errorf("отрицательное = ждать до предела: %s, want %s", got, autoLoadHardCeiling)
	}
	t.Setenv("LB_AUTO_LOAD_WAIT_SEC", "abc")
	if got := p.autoLoadWaitTimeoutForModel("m"); got < 10*time.Minute {
		t.Errorf("мусор в env → тир по размеру: %s, want ≥ 10 мин для 16 GB", got)
	}
}

// TestR83_AutoLoadWaitTier_HardCeiling — бюджет не может превысить hard ceiling
// (30 мин): соединение не держим вечно.
func TestR83_AutoLoadWaitTier_HardCeiling(t *testing.T) {
	t.Setenv("LB_AUTO_LOAD_WAIT_SEC", "")
	p := &Proxy{}
	restore := setModelSize(200 * 1024 * 1024 * 1024)
	defer restore() // 200 GB — заведомо больше потолка
	if got := p.autoLoadWaitTimeoutForModel("m"); got != autoLoadHardCeiling {
		t.Errorf("бюджет = %s, want %s (hard ceiling)", got, autoLoadHardCeiling)
	}
}

// TestR83_AutoLoadWaitTier_UnknownSizeKeepsDefault — размер неизвестен → прежние 180 с.
func TestR83_AutoLoadWaitTier_UnknownSizeKeepsDefault(t *testing.T) {
	t.Setenv("LB_AUTO_LOAD_WAIT_SEC", "")
	p := &Proxy{}
	restore := setModelSize(0)
	defer restore()
	if got := p.autoLoadWaitTimeoutForModel("unknown-model"); got != autoLoadWaitDefaultSec*time.Second {
		t.Errorf("бюджет = %s, want %s (дефолт при неизвестном размере)",
			got, autoLoadWaitDefaultSec*time.Second)
	}
}
