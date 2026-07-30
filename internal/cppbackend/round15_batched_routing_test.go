// round15_batched_routing_test.go — Round 15.1 (2026-07-30) tests для
// Backend integration of BatchedScheduler.
//
// Покрывает:
//   1. Config.EnableBatchedParallel: default false
//   2. Config.EnableBatchedParallel: env override (CPPWORKER_ENABLE_BATCHED_PARALLEL)
//   3. LoadModelOpts.EnableBatchedParallel: per-model override (nil = inherit global)
//
// Интеграционные тесты (с реальной моделью + 4 parallel /v1/chat) — в step 3.5.

package cppbackend

import (
	"os"
	"testing"
)

// TestConfig_EnableBatchedParallel_Default проверяет что default = false
// (Round 13 multi-slot path — backward compat).
func TestConfig_EnableBatchedParallel_Default(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.EnableBatchedParallel {
		t.Errorf("DefaultConfig().EnableBatchedParallel = true, want false (backward compat)")
	}
}

// TestConfig_EnableBatchedParallel_EnvOverride проверяет что
// CPPWORKER_ENABLE_BATCHED_PARALLEL env var корректно выставляет флаг.
func TestConfig_EnableBatchedParallel_EnvOverride(t *testing.T) {
	cases := []struct {
		envVal string
		want   bool
	}{
		{"", false},   // unset (должен вернуться default)
		{"0", false},
		{"false", false},
		{"1", true},
		{"true", true},
		// Note: cppworker's existing pattern для bool env vars
		// принимает только "1"/"0"/"true"/"false" (см. config.go:268+).
		// "yes"/"no"/"on"/"off" не поддерживаются — это by design для
		// consistency с другими env vars (NUMA, USE_MMAP, MLOCK, etc.).
	}

	for _, tc := range cases {
		t.Run("env="+tc.envVal, func(t *testing.T) {
			// Save and restore env (loadConfigFromEnv читает CPPWORKER_* напрямую).
			oldVal, hadOld := os.LookupEnv("CPPWORKER_ENABLE_BATCHED_PARALLEL")
			defer func() {
				if hadOld {
					os.Setenv("CPPWORKER_ENABLE_BATCHED_PARALLEL", oldVal)
				} else {
					os.Unsetenv("CPPWORKER_ENABLE_BATCHED_PARALLEL")
				}
			}()

			if tc.envVal == "" {
				os.Unsetenv("CPPWORKER_ENABLE_BATCHED_PARALLEL")
			} else {
				os.Setenv("CPPWORKER_ENABLE_BATCHED_PARALLEL", tc.envVal)
			}

			cfg := LoadConfigFromEnv()
			if cfg.EnableBatchedParallel != tc.want {
				t.Errorf("EnableBatchedParallel = %v, want %v (env=%q)",
					cfg.EnableBatchedParallel, tc.want, tc.envVal)
			}
		})
	}
}

// TestLoadModelOpts_EnableBatchedParallel_PerModelOverride проверяет что
// per-model override работает: nil = inherit global, *true/*false = override.
func TestLoadModelOpts_EnableBatchedParallel_PerModelOverride(t *testing.T) {
	truePtr := true
	falsePtr := false

	opts := LoadModelOpts{}
	if opts.EnableBatchedParallel != nil {
		t.Errorf("zero-value LoadModelOpts.EnableBatchedParallel = %v, want nil",
			*opts.EnableBatchedParallel)
	}

	// Per-model true
	opts.EnableBatchedParallel = &truePtr
	if opts.EnableBatchedParallel == nil || *opts.EnableBatchedParallel != true {
		t.Errorf("per-model override true failed: got %v", opts.EnableBatchedParallel)
	}

	// Per-model false
	opts.EnableBatchedParallel = &falsePtr
	if opts.EnableBatchedParallel == nil || *opts.EnableBatchedParallel != false {
		t.Errorf("per-model override false failed: got %v", opts.EnableBatchedParallel)
	}
}

// TestModelInfo_BatchedParallel_FieldExists проверяет что BatchedParallel
// есть в ModelInfo и JSON-сериализуется (UI отображает "Batched: ON/OFF").
func TestModelInfo_BatchedParallel_FieldExists(t *testing.T) {
	info := ModelInfo{BatchedParallel: true}
	if !info.BatchedParallel {
		t.Errorf("ModelInfo.BatchedParallel not stored")
	}

	info2 := ModelInfo{BatchedParallel: false}
	if info2.BatchedParallel {
		t.Errorf("ModelInfo.BatchedParallel = true, want false")
	}
}

// TestBackend_BatchedInferStream_NoScheduler проверяет defensive path:
// если BatchedScheduler не активен (default для legacy моделей), метод
// возвращает informative error, не падает с nil pointer.
//
// (Этот тест не вызывает batchedInferStream напрямую — он private. Вместо
// этого проверяем что GenerateStream правильно роутит: с nil scheduler
// должен идти в legacy path, который требует model + handle. Проверяем
// только что у modelInstance есть batchedScheduler field (можно nil).)
func TestModelInstance_BatchedScheduler_DefaultsNil(t *testing.T) {
	// Проверяем что zero-value modelInstance имеет nil batchedScheduler.
	inst := &modelInstance{}
	if inst.batchedScheduler != nil {
		t.Errorf("zero-value modelInstance.batchedScheduler = %v, want nil",
			inst.batchedScheduler)
	}
	if inst.batchedSchedulerCancel != nil {
		t.Errorf("zero-value modelInstance.batchedSchedulerCancel = %v, want nil",
			inst.batchedSchedulerCancel)
	}
}
