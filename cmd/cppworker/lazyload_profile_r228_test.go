//go:build llama_stub

// lazyload_profile_r228_test.go — R228 (2026-10-22): пустой кэш профилей — это НЕ
// «у модели нет профиля».
//
// Живой дефект. После пересоздания воркера pull-синхронизация профилей ещё не
// успела отработать, и ленивая загрузка молча поднимала модель на env-дефолтах
// (живой случай: n_ctx=32768 вместо профильных 12288, kvCacheType=f16 вместо
// q4_0). Теперь load-путь, не найдя профиль в ПУСТОМ кэше, тянет профили один
// раз по месту и повторяет поиск.
package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"go.uber.org/zap"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/pkg/types"
)

// newEmptyCacheSyncerR228 — синхронизатор с ПУСТЫМ кэшем и живым httptest-
// сервером вместо балансера. start() не вызывается намеренно: тест проверяет
// именно путь «кэш пуст → fetchNow по требованию загрузки».
func newEmptyCacheSyncerR228(srv *httptest.Server) *profileSyncerT {
	return &profileSyncerT{
		cache:       map[string]types.LlamaCppModelProfile{},
		balancerURL: srv.URL,
		client:      srv.Client(),
		logger:      zap.NewNop().Sugar(),
	}
}

func profilesServerR228(t *testing.T, hits *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/cppworker/model-profiles" {
			t.Errorf("запрос профилей ушёл не туда: %s", r.URL.Path)
		}
		atomic.AddInt32(hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":{"gemma-4-E4B-it-Q4_K_M":{"contextLength":12288,"batchSize":512,"numGpuLayers":-1,"kvCacheType":"q4_0","parallel":1}}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestR228_LazyLoadFetchesProfilesWhenCacheEmpty — главный регресс: пустой кэш не
// отменяет профиль. После fetchNow профиль применяется (ctx 12288, kv q4_0),
// а не остаётся env-дефолт (32768).
func TestR228_LazyLoadFetchesProfilesWhenCacheEmpty(t *testing.T) {
	old := profileSyncer
	defer func() { profileSyncer = old }()

	var hits int32
	ps := newEmptyCacheSyncerR228(profilesServerR228(t, &hits))
	profileSyncer = ps

	// План без окна: как в живом случае «загрузка с env-дефолтом» (32768).
	opts := cppbackend.LoadModelOpts{}
	if err := applySyncedProfileToLazyLoadOpts("gemma-4-E4B-it-Q4_K_M", &opts); err != nil {
		t.Fatalf("applySyncedProfileToLazyLoadOpts: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("профили подтянуты %d раз, want 1 (пустой кэш → fetchNow)", got)
	}
	if opts.ContextSize != 12288 {
		t.Errorf("ContextSize = %d, want 12288 (профиль модели); 32768 — env-дефолт, "+
			"из-за которого живой стенд поднимал модель не с тем окном", opts.ContextSize)
	}
	if opts.KVCacheType != "q4_0" {
		t.Errorf("KVCacheType = %q, want q4_0 (из профиля)", opts.KVCacheType)
	}
	if opts.GPULayers != -1 {
		t.Errorf("GPULayers = %d, want -1 (из профиля)", opts.GPULayers)
	}
}

// TestR228_LazyLoadFetchFailureKeepsPlan — балансер недоступен: загрузка не
// падает, поля плана остаются как были (никакого «подставили что попало»).
func TestR228_LazyLoadFetchFailureKeepsPlan(t *testing.T) {
	old := profileSyncer
	defer func() { profileSyncer = old }()

	var hits int32
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, "balancer down", http.StatusInternalServerError)
	}))
	defer failing.Close()

	profileSyncer = &profileSyncerT{
		cache:       map[string]types.LlamaCppModelProfile{},
		balancerURL: failing.URL,
		client:      failing.Client(),
		logger:      zap.NewNop().Sugar(),
	}

	opts := cppbackend.LoadModelOpts{ContextSize: 32768}
	if err := applySyncedProfileToLazyLoadOpts("gemma-4-E4B-it-Q4_K_M", &opts); err != nil {
		t.Fatalf("недоступный балансер не должен ронять загрузку: %v", err)
	}
	if hits == 0 {
		t.Error("попытки подтянуть профили не было")
	}
	if opts.ContextSize != 32768 || opts.KVCacheType != "" {
		t.Errorf("план изменён без профиля: ctx=%d kv=%q", opts.ContextSize, opts.KVCacheType)
	}
}

// TestR228_LazyLoadDoesNotRefetchWhenCacheHasEntries — если кэш НЕ пуст и у этой
// модели профиля нет, лишних запросов к балансеру быть не должно: fetchNow —
// только реанимация пустого кэша, а не фоновая синхронизация на каждой загрузке.
func TestR228_LazyLoadDoesNotRefetchWhenCacheHasEntries(t *testing.T) {
	old := profileSyncer
	defer func() { profileSyncer = old }()

	var hits int32
	ps := newEmptyCacheSyncerR228(profilesServerR228(t, &hits))
	ps.cache["other-model"] = types.LlamaCppModelProfile{ContextLength: 8192}
	profileSyncer = ps

	opts := cppbackend.LoadModelOpts{ContextSize: 32768}
	if err := applySyncedProfileToLazyLoadOpts("gemma-4-E4B-it-Q4_K_M", &opts); err != nil {
		t.Fatalf("applySyncedProfileToLazyLoadOpts: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("к балансеру сходили %d раз(а), want 0 (кэш не пуст)", got)
	}
	if opts.ContextSize != 32768 {
		t.Errorf("ContextSize = %d, want 32768 (профиля у модели нет)", opts.ContextSize)
	}
}
