// autotune_apply_verify_r230_test.go — Round 230 (2026-10-24): AutoTune apply
// обязан либо реально применить план, либо вернуть ошибку.
//
// Живой дефект: apply логировал success, но параллельная ленивая загрузка
// успевала раньше и грузила модель по профилю (q4_0/40 вместо f16/-1) — план
// «применялся» повторно без эффекта.
package balancer

import (
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// TestR230_BeginExternalReload — маркер «reload идёт» виден ожидающим,
// второй begin отклоняется, finish идемпотентен и снимает маркер.
func TestR230_BeginExternalReload(t *testing.T) {
	r := newReloadDedupRegistry()

	finish, ok := r.BeginExternalReload("b1", "m1")
	if !ok || finish == nil {
		t.Fatal("первый BeginExternalReload должен пройти")
	}
	if !r.IsReloadPending("b1", "m1") {
		t.Fatal("после begin reload должен быть pending")
	}
	if _, ok2 := r.BeginExternalReload("b1", "m1"); ok2 {
		t.Fatal("второй begin при идущем reload должен отклоняться")
	}
	if r.IsReloadPending("b1", "other") {
		t.Fatal("другая модель не должна быть pending")
	}

	waited := make(chan error, 1)
	go func() { waited <- r.WaitReloadDone("b1", "m1", 2*time.Second) }()
	select {
	case err := <-waited:
		t.Fatalf("WaitReloadDone вернулся до finish: %v", err)
	case <-time.After(60 * time.Millisecond):
	}

	finish()
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("WaitReloadDone после finish: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitReloadDone не проснулся после finish")
	}
	if r.IsReloadPending("b1", "m1") {
		t.Fatal("pending остался после finish")
	}
	finish() // идемпотентность: повторный вызов не паникует

	// После finish маркер можно взять снова.
	finish2, ok2 := r.BeginExternalReload("b1", "m1")
	if !ok2 || finish2 == nil {
		t.Fatal("после finish begin должен снова работать")
	}
	finish2()
}

// TestR230_AutoTuneAppliedMismatch — сверка плана с фактически загруженной моделью.
func TestR230_AutoTuneAppliedMismatch(t *testing.T) {
	plan := &AutoTuneReloadPlan{
		ModelName:    "gemma-4-E4B-it-Q4_K_M",
		ContextSize:  12288,
		KVCacheType:  "f16",
		NumGPULayers: -1,
	}

	t.Run("совпало", func(t *testing.T) {
		m := types.LlamaCppModel{ContextLength: 12288, KvCacheType: "f16", NumGPULayers: 42, NLayers: 42}
		if diff := autoTuneAppliedMismatch(plan, m); diff != "" {
			t.Fatalf("ожидалось совпадение, got %q", diff)
		}
	})
	t.Run("-1 в плане = фактически все слои", func(t *testing.T) {
		m := types.LlamaCppModel{ContextLength: 12288, KvCacheType: "f16", NumGPULayers: 40, NLayers: 42}
		if diff := autoTuneAppliedMismatch(plan, m); diff != "" {
			t.Fatalf("-1 не должен требовать точного числа, got %q", diff)
		}
	})
	t.Run("kv не применился", func(t *testing.T) {
		m := types.LlamaCppModel{ContextLength: 12288, KvCacheType: "q4_0", NumGPULayers: 42, NLayers: 42}
		diff := autoTuneAppliedMismatch(plan, m)
		if !strings.Contains(diff, "kv_cache") {
			t.Fatalf("ожидалось расхождение kv_cache, got %q", diff)
		}
	})
	t.Run("контекст не применился", func(t *testing.T) {
		m := types.LlamaCppModel{ContextLength: 8192, KvCacheType: "f16", NumGPULayers: 42, NLayers: 42}
		if diff := autoTuneAppliedMismatch(plan, m); !strings.Contains(diff, "n_ctx") {
			t.Fatalf("ожидалось расхождение n_ctx, got %q", diff)
		}
	})
	t.Run("положительное число слоёв сверяется", func(t *testing.T) {
		p := &AutoTuneReloadPlan{ContextSize: 12288, KVCacheType: "q4_0", NumGPULayers: 32}
		m := types.LlamaCppModel{ContextLength: 12288, KvCacheType: "q4_0", NumGPULayers: 40, NLayers: 42}
		if diff := autoTuneAppliedMismatch(p, m); !strings.Contains(diff, "num_gpu_layers") {
			t.Fatalf("ожидалось расхождение слоёв, got %q", diff)
		}
	})
	t.Run("воркер не сообщает параметры — не повод для провала", func(t *testing.T) {
		m := types.LlamaCppModel{Name: "gemma-4-E4B-it-Q4_K_M", State: "loaded"}
		if diff := autoTuneAppliedMismatch(plan, m); diff != "" {
			t.Fatalf("без отчётных параметров сверять нечего, got %q", diff)
		}
	})
	t.Run("nil план", func(t *testing.T) {
		if diff := autoTuneAppliedMismatch(nil, types.LlamaCppModel{}); diff != "" {
			t.Fatalf("nil план не должен давать diff, got %q", diff)
		}
	})
}
