// reload_kv_cache_override_test.go — Round 40 #2 regression test.
//
// Проверяет контракт applyStrategyKVCacheOverride (см. одноимённую функцию
// в reload_kv_cache_override.go). Регрессия: до Round 40 #2 в
// handlers_model.go:1304 стояла безусловная перезапись
//
//	opts.KVCacheType = strategy.KVCacheType
//
// которая молча отбрасывала kvCacheType из тела reload-запроса. Этот тест
// пинит 4-веточный контракт merge-логики, чтобы при будущих правках
// стратегии (или LLM-помощника, который "упростит" код) регрессия была
// поймана CI.
//
// 4 ветки (документированы в reload_kv_cache_override.go):
//   - opts == ""  && strategy != "" → strategy wins
//   - opts != ""  && strategy == "" → user wins
//   - opts != ""  && strategy != "" && different → user wins + log
//   - both equal / both empty → без изменений, user wins
//
//go:build llama_stub

package main

import (
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

func TestApplyStrategyKVCacheOverride(t *testing.T) {
	type tc struct {
		name           string
		optsKV         string
		strategyKV     string
		strategyStage  string
		wantOptsKV     string
		wantFromUser   bool
	}
	cases := []tc{
		// === REGRESSION: user q4_0 не должен быть перезаписан strategy f16 ===
		{
			name:         "user_q4_0_strategy_f16_keeps_q4_0",
			optsKV:       "q4_0",
			strategyKV:   "f16",
			strategyStage: "exact_fit",
			wantOptsKV:   "q4_0",
			wantFromUser: true,
		},
		{
			name:         "user_q8_0_strategy_q4_0_keeps_q8_0",
			optsKV:       "q8_0",
			strategyKV:   "q4_0",
			strategyStage: "partial_offload",
			wantOptsKV:   "q8_0",
			wantFromUser: true,
		},
		{
			name:         "user_f16_strategy_q4_0_keeps_f16",
			optsKV:       "f16",
			strategyKV:   "q4_0",
			strategyStage: "auto_retry",
			wantOptsKV:   "f16",
			wantFromUser: true,
		},
		// === negative tests: не сломали нормальный путь ===
		{
			name:         "no_user_strategy_q8_0_applies_q8_0",
			optsKV:       "",
			strategyKV:   "q8_0",
			strategyStage: "auto_retry",
			wantOptsKV:   "q8_0",
			wantFromUser: false,
		},
		{
			name:         "no_user_no_strategy_stays_empty",
			optsKV:       "",
			strategyKV:   "",
			strategyStage: "fallback_no_meta",
			wantOptsKV:   "",
			wantFromUser: false,
		},
		// === idempotency: оба согласны ===
		{
			name:         "user_q8_0_strategy_q8_0_unchanged",
			optsKV:       "q8_0",
			strategyKV:   "q8_0",
			strategyStage: "exact_fit",
			wantOptsKV:   "q8_0",
			wantFromUser: true,
		},
		// === strategy без мнения ===
		{
			name:         "user_q4_0_strategy_empty_keeps_user",
			optsKV:       "q4_0",
			strategyKV:   "",
			strategyStage: "fallback_no_meta",
			wantOptsKV:   "q4_0",
			wantFromUser: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts := &cppbackend.LoadModelOpts{KVCacheType: c.optsKV}
			strategy := LoadStrategyResult{
				KVCacheType: c.strategyKV,
				Stage:       c.strategyStage,
			}
			fromUser := applyStrategyKVCacheOverride(opts, strategy, "test-model")

			if opts.KVCacheType != c.wantOptsKV {
				t.Errorf("opts.KVCacheType: want %q, got %q", c.wantOptsKV, opts.KVCacheType)
			}
			if fromUser != c.wantFromUser {
				t.Errorf("fromUser: want %v, got %v", c.wantFromUser, fromUser)
			}
		})
	}
}

// TestApplyStrategyKVCacheOverride_DoesNotTouchOtherFields — sanity check:
// helper не должен модифицировать поля кроме KVCacheType.
func TestApplyStrategyKVCacheOverride_DoesNotTouchOtherFields(t *testing.T) {
	opts := &cppbackend.LoadModelOpts{
		KVCacheType:   "q4_0",
		ContextSize:   65536,
		BatchSize:     512,
		GPULayers:     12,
		Parallel:      4,
		FlashAttnType: 1,
	}
	snapshot := *opts
	strategy := LoadStrategyResult{
		GPULayers:   99,  // should not propagate
		NCtx:        1024,
		KVCacheType: "f16",
		UseMmap:     true,
		Stage:       "exact_fit",
	}

	_ = applyStrategyKVCacheOverride(opts, strategy, "test-model")

	if opts.ContextSize != snapshot.ContextSize {
		t.Errorf("ContextSize mutated: %d -> %d", snapshot.ContextSize, opts.ContextSize)
	}
	if opts.BatchSize != snapshot.BatchSize {
		t.Errorf("BatchSize mutated: %d -> %d", snapshot.BatchSize, opts.BatchSize)
	}
	if opts.GPULayers != snapshot.GPULayers {
		t.Errorf("GPULayers mutated: %d -> %d", snapshot.GPULayers, opts.GPULayers)
	}
	if opts.Parallel != snapshot.Parallel {
		t.Errorf("Parallel mutated: %d -> %d", snapshot.Parallel, opts.Parallel)
	}
	if opts.FlashAttnType != snapshot.FlashAttnType {
		t.Errorf("FlashAttnType mutated: %d -> %d", snapshot.FlashAttnType, opts.FlashAttnType)
	}
}
