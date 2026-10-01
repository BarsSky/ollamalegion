//go:build llama_stub

// lazyload_profile_r83_test.go — R83/v52 (2026-10-01): ленивая загрузка cppworker
// обязана применять профиль модели, иначе прямой запрос (WebUI, диагностика,
// клиент мимо балансера) поднимал модель на env-дефолтах контейнера.
//
// Живой замер стенда: инстанс, загруженный балансером (профиль → q4_0), работал
// с tokenLatencyMs 125-145; инстанс, поднятый ленивой загрузкой cppworker
// (env-дефолт → f16), — 1035 мс, 19 слоёв из 42 на CPU, генерация ~1 tok/s.
package main

import (
	"strings"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/pkg/types"
)

// TestR83v52_LazyLoadTakesKVCacheTypeFromProfile — главный регресс: пустой
// kvCacheType в плане загрузки обязан заполниться значением профиля, а окно,
// выбранное под клиента, остаться прежним.
func TestR83v52_LazyLoadTakesKVCacheTypeFromProfile(t *testing.T) {
	old := profileSyncer
	defer func() { profileSyncer = old }()
	ps := &profileSyncerT{cache: map[string]types.LlamaCppModelProfile{}}
	ps.cache["gemma-4-E4B-it-Q4_K_M"] = types.LlamaCppModelProfile{
		ContextLength: 65536,
		BatchSize:     512,
		NumGPULayers:  -1,
		KVCacheType:   "q4_0",
	}
	profileSyncer = ps

	// Как в плане ленивой загрузки: окно от клиента, kvCacheType не задан
	// (до фикса так и уходило в llama.cpp → env-дефолт f16).
	opts := cppbackend.LoadModelOpts{ContextSize: 32768}
	if err := applySyncedProfileToLazyLoadOpts("gemma-4-E4B-it-Q4_K_M", &opts); err != nil {
		t.Fatalf("applySyncedProfileToLazyLoadOpts: %v", err)
	}
	if opts.KVCacheType != "q4_0" {
		t.Errorf("KVCacheType = %q, want q4_0 (из профиля модели); env-дефолт f16 "+
			"даёт KV вчетверо больше и оставляет слои на CPU", opts.KVCacheType)
	}
	if opts.ContextSize != 32768 {
		t.Errorf("ContextSize = %d, want 32768: профиль не должен перезаписывать "+
			"окно, выбранное под клиента", opts.ContextSize)
	}
	if opts.GPULayers != -1 {
		t.Errorf("GPULayers = %d, want -1 (из профиля, план был пуст)", opts.GPULayers)
	}
}

// TestR83v52_LazyLoadKeepsExplicitPlan — явные значения плана (выбранные под
// клиента/железо) профиль не перезаписывает: он только дополняет.
func TestR83v52_LazyLoadKeepsExplicitPlan(t *testing.T) {
	old := profileSyncer
	defer func() { profileSyncer = old }()
	ps := &profileSyncerT{cache: map[string]types.LlamaCppModelProfile{}}
	ps.cache["gemma-4-E4B-it-Q4_K_M"] = types.LlamaCppModelProfile{
		ContextLength: 65536,
		BatchSize:     256,
		NumGPULayers:  -1,
		KVCacheType:   "f16",
	}
	profileSyncer = ps

	opts := cppbackend.LoadModelOpts{
		ContextSize: 32768,
		BatchSize:   512,
		GPULayers:   35,
		KVCacheType: "q4_0",
	}
	if err := applySyncedProfileToLazyLoadOpts("gemma-4-E4B-it-Q4_K_M", &opts); err != nil {
		t.Fatalf("applySyncedProfileToLazyLoadOpts: %v", err)
	}
	if opts.ContextSize != 32768 || opts.BatchSize != 512 || opts.GPULayers != 35 || opts.KVCacheType != "q4_0" {
		t.Errorf("план перезаписан профилем: ctx=%d batch=%d layers=%d kv=%q",
			opts.ContextSize, opts.BatchSize, opts.GPULayers, opts.KVCacheType)
	}
}

// TestR83v52_LazyLoadDisabledProfileIsRefusal — disabled-профиль это отказ, а не
// молчаливая загрузка «как получится».
func TestR83v52_LazyLoadDisabledProfileIsRefusal(t *testing.T) {
	old := profileSyncer
	defer func() { profileSyncer = old }()
	ps := &profileSyncerT{cache: map[string]types.LlamaCppModelProfile{}}
	ps.cache["broken-model"] = types.LlamaCppModelProfile{Disabled: true}
	profileSyncer = ps

	opts := cppbackend.LoadModelOpts{ContextSize: 32768}
	err := applySyncedProfileToLazyLoadOpts("broken-model", &opts)
	if err == nil {
		t.Fatal("ожидался отказ для disabled-профиля")
	}
	if !strings.Contains(err.Error(), "disabled") {
		t.Errorf("ошибка должна объяснять причину: %v", err)
	}
}

// TestR83v52_LazyLoadNoProfileIsNoop — модель без профиля грузится как раньше.
func TestR83v52_LazyLoadNoProfileIsNoop(t *testing.T) {
	old := profileSyncer
	defer func() { profileSyncer = old }()
	profileSyncer = &profileSyncerT{cache: map[string]types.LlamaCppModelProfile{}}

	opts := cppbackend.LoadModelOpts{ContextSize: 32768}
	if err := applySyncedProfileToLazyLoadOpts("unknown-model", &opts); err != nil {
		t.Fatalf("без профиля ошибки быть не должно: %v", err)
	}
	if opts.ContextSize != 32768 || opts.KVCacheType != "" {
		t.Errorf("план изменён без профиля: ctx=%d kv=%q", opts.ContextSize, opts.KVCacheType)
	}
}
