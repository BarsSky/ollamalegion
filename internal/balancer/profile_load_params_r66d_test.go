//go:build llama_stub

// profile_load_params_r66d_test.go — R66d (2026-09-23).
//
// РЕГРЕСС: per-model профиль должен применяться уже при ПЕРВОЙ загрузке модели,
// а не только через POST /api/v1/cppworker/model-profiles/{name}/apply.
//
// БЫЛО: executeLlamaCppLoad брал из профиля только override-tensors, а
// contextLength / batchSize / numGpuLayers / flashAttn / useMmap / kvCacheType
// игнорировал. При этом n_ctx-resolver (GetModelProfileNumCtx) и preflight уже
// считают profile.ContextLength истиной и показывают его в UI — то есть WebUI
// показывал n_ctx из профиля, а cppworker грузил модель со своим дефолтом.
// Отсюда жалобы «в WebUI видно не то, что реально загружено» и «профиль не
// применяется при загрузке/предзагрузке».
//
// Приоритет, который проверяют тесты: явные поля запроса > профиль > cppworker.
package balancer

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// newProfileLoadCaptureServer — мок cppworker, который запоминает тело
// /api/models/load и отвечает успехом.
func newProfileLoadCaptureServer(t *testing.T) (*httptest.Server, func() map[string]interface{}) {
	t.Helper()
	var mu sync.Mutex
	var raw []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models/load" || r.URL.Path == "/api/models/load-with-params" {
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			raw = body
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"loaded","name":"gemma-4-E4B-it-Q4_K_M"}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	get := func() map[string]interface{} {
		mu.Lock()
		defer mu.Unlock()
		if raw == nil {
			return nil
		}
		var out map[string]interface{}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("load body is not JSON: %v (%s)", err, string(raw))
		}
		return out
	}
	return srv, get
}

// newModelManagerWithProfiles — ModelManager с прокси, у которого заданы
// per-model профили (как в config.json).
func newModelManagerWithProfiles(profiles map[string]types.LlamaCppModelProfile) *ModelManager {
	p := &Proxy{
		config: &types.LoadBalancerConfig{
			LlamaCppModelProfiles: profiles,
		},
	}
	mm := NewModelManager(nil)
	mm.proxy = p
	return mm
}

func boolPtrProfile(b bool) *bool { return &b }

// TestExecuteLlamaCppLoad_AppliesProfileParamsWhenRequestOmitted — запрос без
// параметров (типичный auto-load по запросу клиента / кнопка Load без настроек)
// должен получить параметры профиля.
func TestExecuteLlamaCppLoad_AppliesProfileParamsWhenRequestOmitted(t *testing.T) {
	srv, body := newProfileLoadCaptureServer(t)
	host, port := parseTestHostPort(t, srv.URL)

	mm := newModelManagerWithProfiles(map[string]types.LlamaCppModelProfile{
		"gemma-4-E4B-it-Q4_K_M": {
			ContextLength: 8192,
			BatchSize:     512,
			NumGPULayers:  -1,
			FlashAttn:     boolPtrProfile(true),
			UseMmap:       boolPtrProfile(false),
			KVCacheType:   "q4_0",
		},
	})

	res := mm.executeLlamaCppLoad(host, port, "test-backend", ModelOpRequest{
		Operation: "load",
		ModelName: "gemma-4-E4B-it-Q4_K_M",
	})
	if res == nil || !res.Success {
		t.Fatalf("load should succeed, got %+v", res)
	}

	got := body()
	if got == nil {
		t.Fatal("cppworker did not receive a load request")
	}
	if v, _ := got["contextSize"].(float64); int(v) != 8192 {
		t.Errorf("contextSize = %v, want 8192 (profile.contextLength)", got["contextSize"])
	}
	if v, _ := got["batchSize"].(float64); int(v) != 512 {
		t.Errorf("batchSize = %v, want 512 (profile.batchSize)", got["batchSize"])
	}
	if v, _ := got["gpuLayers"].(float64); int(v) != -1 {
		t.Errorf("gpuLayers = %v, want -1 (profile.numGpuLayers)", got["gpuLayers"])
	}
	// profile.flashAttn — *bool, cppworker ждёт *int (-1/0/1).
	if v, _ := got["flashAttn"].(float64); int(v) != 1 {
		t.Errorf("flashAttn = %v, want 1 (profile.flashAttn=true)", got["flashAttn"])
	}
	if v, ok := got["useMmap"].(bool); !ok || v {
		t.Errorf("useMmap = %v, want false (profile.useMmap=false)", got["useMmap"])
	}
	if v, _ := got["kvCacheType"].(string); v != "q4_0" {
		t.Errorf("kvCacheType = %v, want q4_0 (profile.kvCacheType)", got["kvCacheType"])
	}
}

// TestExecuteLlamaCppLoad_ExplicitRequestWinsOverProfile — если WebUI/клиент
// передал значение явно, профиль его не перетирает.
func TestExecuteLlamaCppLoad_ExplicitRequestWinsOverProfile(t *testing.T) {
	srv, body := newProfileLoadCaptureServer(t)
	host, port := parseTestHostPort(t, srv.URL)

	mm := newModelManagerWithProfiles(map[string]types.LlamaCppModelProfile{
		"gemma-4": {ContextLength: 8192, BatchSize: 512},
	})

	ctxSize := 131072
	gpuLayers := 30
	batch := 256
	res := mm.executeLlamaCppLoad(host, port, "test-backend", ModelOpRequest{
		Operation:   "load",
		ModelName:   "gemma-4",
		ContextSize: &ctxSize,
		GPULayers:   &gpuLayers,
		BatchSize:   &batch,
	})
	if res == nil || !res.Success {
		t.Fatalf("load should succeed, got %+v", res)
	}

	got := body()
	if v, _ := got["contextSize"].(float64); int(v) != 131072 {
		t.Errorf("contextSize = %v, want 131072 (явный запрос важнее профиля)", got["contextSize"])
	}
	if v, _ := got["gpuLayers"].(float64); int(v) != 30 {
		t.Errorf("gpuLayers = %v, want 30", got["gpuLayers"])
	}
	if v, _ := got["batchSize"].(float64); int(v) != 256 {
		t.Errorf("batchSize = %v, want 256", got["batchSize"])
	}
}

// TestExecuteLlamaCppLoad_ProfileMatchedByGGUFName — WebUI передаёт имя файла
// с расширением .gguf, а профили записаны без него. Профиль должен найтись
// (та же семантика, что в GetModelProfileNumCtx).
func TestExecuteLlamaCppLoad_ProfileMatchedByGGUFName(t *testing.T) {
	srv, body := newProfileLoadCaptureServer(t)
	host, port := parseTestHostPort(t, srv.URL)

	mm := newModelManagerWithProfiles(map[string]types.LlamaCppModelProfile{
		"gemma-4-E4B-it-Q4_K_M": {ContextLength: 32768, NumGPULayers: 20},
	})

	res := mm.executeLlamaCppLoad(host, port, "test-backend", ModelOpRequest{
		Operation: "load",
		ModelName: "gemma-4-E4B-it-Q4_K_M.gguf",
	})
	if res == nil || !res.Success {
		t.Fatalf("load should succeed, got %+v", res)
	}

	got := body()
	if v, _ := got["contextSize"].(float64); int(v) != 32768 {
		t.Errorf("contextSize = %v, want 32768 — профиль не найден по имени с .gguf", got["contextSize"])
	}
	if v, _ := got["gpuLayers"].(float64); int(v) != 20 {
		t.Errorf("gpuLayers = %v, want 20 — профиль не найден по имени с .gguf", got["gpuLayers"])
	}
}

// TestExecuteLlamaCppLoad_NoProfileKeepsBodyMinimal — без профиля тело
// остаётся минимальным (cppworker применит свои дефолты), чтобы не менять
// поведение существующих установок.
func TestExecuteLlamaCppLoad_NoProfileKeepsBodyMinimal(t *testing.T) {
	srv, body := newProfileLoadCaptureServer(t)
	host, port := parseTestHostPort(t, srv.URL)

	mm := newModelManagerWithProfiles(map[string]types.LlamaCppModelProfile{
		"other-model": {ContextLength: 4096},
	})

	res := mm.executeLlamaCppLoad(host, port, "test-backend", ModelOpRequest{
		Operation: "load",
		ModelName: "unknown-model",
	})
	if res == nil || !res.Success {
		t.Fatalf("load should succeed, got %+v", res)
	}

	got := body()
	for _, key := range []string{"contextSize", "batchSize", "gpuLayers", "flashAttn", "useMmap", "kvCacheType"} {
		if _, present := got[key]; present {
			t.Errorf("без профиля в теле не должно быть %q, got %v", key, got[key])
		}
	}
	if _, ok := got["name"]; !ok {
		t.Errorf("в теле должен остаться name: %v", got)
	}
}
