// round16_reasoning_env_test.go — Round 16 (2026-07-30) tests для
// reasoning/thinking env vars.
//
// Покрывает:
//   1. DefaultEnableReasoning env var (CPPWORKER_DEFAULT_ENABLE_REASONING)
//   2. DefaultReasoningBudget env var (CPPWORKER_DEFAULT_REASONING_BUDGET)
//   3. EnableReasoning env var (CPPWORKER_ENABLE_REASONING)
//   4. ReasoningBudget env var (CPPWORKER_REASONING_BUDGET)
//   5. Default values (off, budget=0) когда env vars НЕ заданы
//
// Round 16 motivation: до Round 16 reasoning/thinking можно было
// включить только через JSON config файл или per-model load request.
// Теперь можно глобально через env — критично для docker-compose
// deployments, где хочется globally задать policy (например, OFF для
// tool use workloads с Cline/Roo/Aider) без per-model overrides.

package cppbackend

import (
	"os"
	"testing"
)

// TestConfig_DefaultEnableReasoning_Default проверяет что default = false
// (reasoning OFF — backward compat, важно для tool use workloads где
// thinking мешает парсеру tool calls в Qwen3).
func TestConfig_DefaultEnableReasoning_Default(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.DefaultEnableReasoning {
		t.Errorf("DefaultConfig().DefaultEnableReasoning = true, want false (backward compat)")
	}
	if cfg.DefaultReasoningBudget != 0 {
		t.Errorf("DefaultConfig().DefaultReasoningBudget = %d, want 0 (no limit)", cfg.DefaultReasoningBudget)
	}
}

// TestConfig_DefaultEnableReasoning_EnvOverride проверяет что
// CPPWORKER_DEFAULT_ENABLE_REASONING env var корректно выставляет флаг.
func TestConfig_DefaultEnableReasoning_EnvOverride(t *testing.T) {
	cases := []struct {
		envVal string
		want   bool
	}{
		{"", false},   // unset (должен вернуться default)
		{"0", false},
		{"false", false},
		{"1", true},
		{"true", true},
	}

	for _, tc := range cases {
		t.Run("env="+tc.envVal, func(t *testing.T) {
			oldVal, hadOld := os.LookupEnv("CPPWORKER_DEFAULT_ENABLE_REASONING")
			defer func() {
				if hadOld {
					os.Setenv("CPPWORKER_DEFAULT_ENABLE_REASONING", oldVal)
				} else {
					os.Unsetenv("CPPWORKER_DEFAULT_ENABLE_REASONING")
				}
			}()

			if tc.envVal == "" {
				os.Unsetenv("CPPWORKER_DEFAULT_ENABLE_REASONING")
			} else {
				os.Setenv("CPPWORKER_DEFAULT_ENABLE_REASONING", tc.envVal)
			}

			cfg := LoadConfigFromEnv()
			if cfg.DefaultEnableReasoning != tc.want {
				t.Errorf("DefaultEnableReasoning = %v, want %v (env=%q)",
					cfg.DefaultEnableReasoning, tc.want, tc.envVal)
			}
		})
	}
}

// TestConfig_DefaultReasoningBudget_EnvOverride проверяет что
// CPPWORKER_DEFAULT_REASONING_BUDGET env var задаёт budget.
func TestConfig_DefaultReasoningBudget_EnvOverride(t *testing.T) {
	oldVal, hadOld := os.LookupEnv("CPPWORKER_DEFAULT_REASONING_BUDGET")
	defer func() {
		if hadOld {
			os.Setenv("CPPWORKER_DEFAULT_REASONING_BUDGET", oldVal)
		} else {
			os.Unsetenv("CPPWORKER_DEFAULT_REASONING_BUDGET")
		}
	}()

	os.Setenv("CPPWORKER_DEFAULT_REASONING_BUDGET", "4096")
	cfg := LoadConfigFromEnv()
	if cfg.DefaultReasoningBudget != 4096 {
		t.Errorf("DefaultReasoningBudget = %d, want 4096", cfg.DefaultReasoningBudget)
	}

	os.Setenv("CPPWORKER_DEFAULT_REASONING_BUDGET", "0")
	cfg = LoadConfigFromEnv()
	if cfg.DefaultReasoningBudget != 0 {
		t.Errorf("DefaultReasoningBudget = %d, want 0 (unlimited)", cfg.DefaultReasoningBudget)
	}
}

// TestConfig_EnableReasoning_EnvOverride проверяет что
// CPPWORKER_ENABLE_REASONING env var (legacy field) работает.
func TestConfig_EnableReasoning_EnvOverride(t *testing.T) {
	oldVal, hadOld := os.LookupEnv("CPPWORKER_ENABLE_REASONING")
	defer func() {
		if hadOld {
			os.Setenv("CPPWORKER_ENABLE_REASONING", oldVal)
		} else {
			os.Unsetenv("CPPWORKER_ENABLE_REASONING")
		}
	}()

	os.Setenv("CPPWORKER_ENABLE_REASONING", "true")
	cfg := LoadConfigFromEnv()
	if !cfg.EnableReasoning {
		t.Errorf("EnableReasoning = false, want true (env=true)")
	}

	os.Setenv("CPPWORKER_ENABLE_REASONING", "0")
	cfg = LoadConfigFromEnv()
	if cfg.EnableReasoning {
		t.Errorf("EnableReasoning = true, want false (env=0)")
	}
}

// TestConfig_ReasoningBudget_EnvOverride проверяет что
// CPPWORKER_REASONING_BUDGET env var работает.
func TestConfig_ReasoningBudget_EnvOverride(t *testing.T) {
	oldVal, hadOld := os.LookupEnv("CPPWORKER_REASONING_BUDGET")
	defer func() {
		if hadOld {
			os.Setenv("CPPWORKER_REASONING_BUDGET", oldVal)
		} else {
			os.Unsetenv("CPPWORKER_REASONING_BUDGET")
		}
	}()

	os.Setenv("CPPWORKER_REASONING_BUDGET", "2048")
	cfg := LoadConfigFromEnv()
	if cfg.ReasoningBudget != 2048 {
		t.Errorf("ReasoningBudget = %d, want 2048", cfg.ReasoningBudget)
	}
}

// TestConfig_EnableBatchedParallel_EnvOverride_Round16 — Round 15.1 +
// Round 16: opt-in флаг для batched parallel inference через env var.
// Default = false (Round 13 multi-slot path — backward compat).
func TestConfig_EnableBatchedParallel_EnvOverride_Round16(t *testing.T) {
	oldVal, hadOld := os.LookupEnv("CPPWORKER_ENABLE_BATCHED_PARALLEL")
	defer func() {
		if hadOld {
			os.Setenv("CPPWORKER_ENABLE_BATCHED_PARALLEL", oldVal)
		} else {
			os.Unsetenv("CPPWORKER_ENABLE_BATCHED_PARALLEL")
		}
	}()

	os.Setenv("CPPWORKER_ENABLE_BATCHED_PARALLEL", "1")
	cfg := LoadConfigFromEnv()
	if !cfg.EnableBatchedParallel {
		t.Errorf("EnableBatchedParallel = false, want true (env=1)")
	}

	os.Unsetenv("CPPWORKER_ENABLE_BATCHED_PARALLEL")
	cfg = LoadConfigFromEnv()
	if cfg.EnableBatchedParallel {
		t.Errorf("EnableBatchedParallel = true, want false (default after unset)")
	}
}
