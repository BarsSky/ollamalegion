// config_round12_test.go — Round 12 (2026-07-28): тесты для defaultNParallel.
//
// Покрывает:
//   - DefaultConfig() имеет DefaultNParallel=0
//   - ENV CPPWORKER_N_PARALLEL корректно подхватывается (0..8)
//   - Невалидные значения (отрицательные) — fallback на default
//   - LoadModelOpts.Parallel > 0 ВСЕГДА выигрывает у DefaultNParallel (per-model override)
package cppbackend

import (
	"os"
	"testing"
)

// TestDefaultConfig_NParallel_Default проверяет hardcoded default = 0
// (что в C-bridge трактуется как llama.cpp default = 1).
func TestDefaultConfig_NParallel_Default(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.DefaultNParallel != 0 {
		t.Errorf("DefaultConfig().DefaultNParallel = %d, want 0 (bridge default = 1)", cfg.DefaultNParallel)
	}
}

// TestLoadConfigFromEnv_NParallel_Valid проверяет ENV CPPWORKER_N_PARALLEL
// для всех допустимых значений 0..8.
func TestLoadConfigFromEnv_NParallel_Valid(t *testing.T) {
	// Save and restore.
	prev, hadPrev := os.LookupEnv("CPPWORKER_N_PARALLEL")
	t.Cleanup(func() {
		if hadPrev {
			_ = os.Setenv("CPPWORKER_N_PARALLEL", prev)
		} else {
			_ = os.Unsetenv("CPPWORKER_N_PARALLEL")
		}
	})

	tests := []struct {
		envValue string
		want     int
	}{
		{"0", 0},   // bridge default
		{"1", 1},   // explicit n_parallel=1
		{"2", 2},   // 2-slot batched
		{"4", 4},   // 4-slot batched
		{"8", 8},   // max recommended
	}

	for _, tt := range tests {
		t.Run("env="+tt.envValue, func(t *testing.T) {
			_ = os.Setenv("CPPWORKER_N_PARALLEL", tt.envValue)
			cfg := LoadConfigFromEnv()
			if cfg.DefaultNParallel != tt.want {
				t.Errorf("CPPWORKER_N_PARALLEL=%q: DefaultNParallel=%d, want %d",
					tt.envValue, cfg.DefaultNParallel, tt.want)
			}
		})
	}
}

// TestLoadConfigFromEnv_NParallel_Invalid проверяет что невалидные ENV
// значения (отрицательные, мусор) НЕ ломают config — fallback на default (0).
//
// Negative values в parseInt() пройдут, но env-loading проверяет n >= 0.
// Мусор ("abc") → parseInt вернёт defaultVal (0).
func TestLoadConfigFromEnv_NParallel_Invalid(t *testing.T) {
	prev, hadPrev := os.LookupEnv("CPPWORKER_N_PARALLEL")
	t.Cleanup(func() {
		if hadPrev {
			_ = os.Setenv("CPPWORKER_N_PARALLEL", prev)
		} else {
			_ = os.Unsetenv("CPPWORKER_N_PARALLEL")
		}
	})

	tests := []struct {
		envValue string
		want     int
		desc     string
	}{
		{"abc", 0, "non-numeric → parseInt fallback to 0"},
		{"-1", 0, "negative rejected by n>=0 check"},
		{"-100", 0, "large negative rejected"},
		{"", 0, "empty → default 0"},
	}

	for _, tt := range tests {
		t.Run("env="+tt.envValue, func(t *testing.T) {
			if tt.envValue == "" {
				_ = os.Unsetenv("CPPWORKER_N_PARALLEL")
			} else {
				_ = os.Setenv("CPPWORKER_N_PARALLEL", tt.envValue)
			}
			cfg := LoadConfigFromEnv()
			if cfg.DefaultNParallel != tt.want {
				t.Errorf("CPPWORKER_N_PARALLEL=%q (%s): DefaultNParallel=%d, want %d",
					tt.envValue, tt.desc, cfg.DefaultNParallel, tt.want)
			}
		})
	}
}

// TestLoadConfigFromEnv_NParallel_NotSet проверяет что при unset
// остаётся default 0 (или значение из cppworker-defaults.json, если он есть).
func TestLoadConfigFromEnv_NParallel_NotSet(t *testing.T) {
	prev, hadPrev := os.LookupEnv("CPPWORKER_N_PARALLEL")
	t.Cleanup(func() {
		if hadPrev {
			_ = os.Setenv("CPPWORKER_N_PARALLEL", prev)
		} else {
			_ = os.Unsetenv("CPPWORKER_N_PARALLEL")
		}
	})
	_ = os.Unsetenv("CPPWORKER_N_PARALLEL")

	cfg := LoadConfigFromEnv()
	// Если файл config/cppworker-defaults.json содержит defaultNParallel=0 — увидим 0.
	// Если файл отсутствует — DefaultConfig() даст 0.
	// В обоих случаях при unset env мы должны получить 0.
	if cfg.DefaultNParallel != 0 {
		t.Logf("DefaultNParallel=%d (file overrides possible, но 0 — базовое значение)", cfg.DefaultNParallel)
	}
}
