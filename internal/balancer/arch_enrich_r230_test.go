// arch_enrich_r230_test.go — Round 230 (2026-10-24): балансер добирает
// n_kv_heads/head_dim_k из /api/show (в /api/models их нет), иначе AutoTune
// не может посчитать KV cache.
package balancer

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// archTestServer поднимает fake cppworker с POST /api/show.
func archTestServer(t *testing.T, modelInfo map[string]interface{}, status int) (*httptest.Server, *int32, *chan string) {
	t.Helper()
	var hits int32
	names := make(chan string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/show" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&hits, 1)
		var req struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		select {
		case names <- req.Name:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"model_info": modelInfo})
	}))
	t.Cleanup(srv.Close)
	return srv, &hits, &names
}

func backendForServer(t *testing.T, srv *httptest.Server) backendInfo {
	t.Helper()
	host, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split host/port: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	return backendInfo{id: "b1", host: host, port: port}
}

func newArchPoller() *llamaCppMetricsPoller {
	return &llamaCppMetricsPoller{
		httpClient: &http.Client{Timeout: 3 * time.Second},
	}
}

// TestR230_ArchEnrich_FillsAndCaches — параметры добираются один раз на модель.
func TestR230_ArchEnrich_FillsAndCaches(t *testing.T) {
	srv, hits, names := archTestServer(t, map[string]interface{}{
		"n_kv_heads": 2.0,
		"head_dim_k": 256.0,
		"head_dim_v": 256.0,
		"n_embd":     2560.0,
		"n_layers":   42.0,
	}, http.StatusOK)
	p := newArchPoller()
	b := backendForServer(t, srv)

	models := []types.LlamaCppModel{{
		Name:          "gemma-4-E4B-it-Q4_K_M",
		State:         "loaded",
		ContextLength: 12288,
		NLayers:       42,
		NEmbd:         2560,
	}}
	models = p.enrichLoadedModelsArch(b, models)
	if models[0].NKvHeads != 2 || models[0].HeadDimK != 256 || models[0].HeadDimV != 256 {
		t.Fatalf("arch не заполнена: %+v", models[0])
	}
	if models[0].NEmbd != 2560 {
		t.Fatalf("n_embd не должен меняться: %d", models[0].NEmbd)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("ожидался 1 запрос /api/show, got %d", got)
	}
	select {
	case n := <-*names:
		if n != "gemma-4-E4B-it-Q4_K_M" {
			t.Fatalf("имя модели в запросе: %q", n)
		}
	default:
		t.Fatal("запрос /api/show не был сделан")
	}

	// Второй тик: модель снова без arch (как /api/models) — должен сработать кэш.
	again := []types.LlamaCppModel{{
		Name:          "gemma-4-E4B-it-Q4_K_M",
		State:         "loaded",
		ContextLength: 12288,
		NLayers:       42,
	}}
	again = p.enrichLoadedModelsArch(b, again)
	if again[0].NKvHeads != 2 || again[0].HeadDimK != 256 {
		t.Fatalf("кэш не применился: %+v", again[0])
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("повторный запрос /api/show не нужен (кэш), hits=%d", got)
	}
}

// TestR230_ArchEnrich_SkipsWhenPresent — если /api/models уже отдал arch,
// лишний HTTP-запрос не делается.
func TestR230_ArchEnrich_SkipsWhenPresent(t *testing.T) {
	srv, hits, _ := archTestServer(t, map[string]interface{}{"n_kv_heads": 8.0}, http.StatusOK)
	p := newArchPoller()
	b := backendForServer(t, srv)

	models := []types.LlamaCppModel{{
		Name:          "qwen3-4b",
		State:         "loaded",
		ContextLength: 32768,
		NKvHeads:      8,
		HeadDimK:      128,
	}}
	models = p.enrichLoadedModelsArch(b, models)
	if got := atomic.LoadInt32(hits); got != 0 {
		t.Fatalf("/api/show не должен вызываться, hits=%d", got)
	}
	if models[0].NKvHeads != 8 || models[0].HeadDimK != 128 {
		t.Fatalf("явные значения затёрты: %+v", models[0])
	}
}

// TestR230_ArchEnrich_NoUsableData_NoCache — нули в model_info не кэшируются:
// следующий тик попробует снова (модель могла ещё догружаться).
func TestR230_ArchEnrich_NoUsableData_NoCache(t *testing.T) {
	srv, hits, _ := archTestServer(t, map[string]interface{}{
		"n_kv_heads": 0.0,
		"head_dim_k": 0.0,
	}, http.StatusOK)
	p := newArchPoller()
	b := backendForServer(t, srv)

	load := func() []types.LlamaCppModel {
		m := []types.LlamaCppModel{{Name: "m", State: "loaded", ContextLength: 8192}}
		return p.enrichLoadedModelsArch(b, m)
	}
	if got := load(); got[0].NKvHeads != 0 {
		t.Fatalf("нулевые arch не должны применяться: %+v", got[0])
	}
	if got := load(); got[0].NKvHeads != 0 {
		t.Fatalf("нулевые arch не должны применяться: %+v", got[0])
	}
	if got := atomic.LoadInt32(hits); got != 2 {
		t.Fatalf("неудачные ответы не кэшируются: ожидалось 2 запроса, got %d", got)
	}
}

// TestR230_ArchEnrich_DerivesMissing — если GGUF не отдал head_dim/n_kv_heads,
// они выводятся из n_embd/n_heads (соглашение llama.cpp) и MHA-фолбэка.
func TestR230_ArchEnrich_DerivesMissing(t *testing.T) {
	srv, _, _ := archTestServer(t, map[string]interface{}{
		"n_kv_heads": 0.0,
		"n_heads":    8.0,
		"n_embd":     2560.0,
		"head_dim_k": 0.0,
	}, http.StatusOK)
	p := newArchPoller()
	b := backendForServer(t, srv)

	models := []types.LlamaCppModel{{Name: "gemma4", State: "loaded", ContextLength: 12288, NLayers: 42}}
	models = p.enrichLoadedModelsArch(b, models)
	if models[0].NKvHeads != 8 {
		t.Errorf("n_kv_heads: got %d, want 8 (MHA fallback)", models[0].NKvHeads)
	}
	if models[0].HeadDimK != 320 {
		t.Errorf("head_dim_k: got %d, want 320 (n_embd/n_heads)", models[0].HeadDimK)
	}
}

// TestR230_ArchEnrich_IgnoresUnloaded — для незагруженных моделей запросов нет.
func TestR230_ArchEnrich_IgnoresUnloaded(t *testing.T) {
	srv, hits, _ := archTestServer(t, map[string]interface{}{"n_kv_heads": 2.0, "head_dim_k": 256.0}, http.StatusOK)
	p := newArchPoller()
	b := backendForServer(t, srv)

	models := []types.LlamaCppModel{{Name: "m", State: "loading"}}
	models = p.enrichLoadedModelsArch(b, models)
	if got := atomic.LoadInt32(hits); got != 0 {
		t.Fatalf("для loading-модели запросов быть не должно, hits=%d", got)
	}
}
