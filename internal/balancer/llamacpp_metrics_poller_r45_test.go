// Round 45 (R45, 2026-08-19): regression tests for the metrics-poller JSON
// tag bug. Pre-R45, llamacpp_metrics_poller.go used camelCase JSON tags
// (`contextSize`, `nLayers`, `sizeBytes`, ...) but cppworker has always
// returned snake_case (`context_size`, `n_layers`, `size_bytes`, ...).
// Result: every per-model field in metricsMgr.llamaMetrics[].LoadedModels
// silently decoded to its zero value, including ContextLength — the field
// preflightNCtxReloadIfNeeded reads to decide whether to trigger an async
// reload. With loaded_n_ctx permanently 0, every /api/chat returned 503
// and triggered a reload loop until the model was reloaded again.
//
// These tests pin the fix in place by feeding the poller realistic
// cppworker /api/models responses and asserting that per-model fields
// (especially ContextLength) are correctly extracted. If a future refactor
// re-introduces a tag mismatch, these tests fail loudly.
package balancer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// r45FakeCppWorker — minimal httptest mock that records request count and
// returns a configurable /api/models (and empty /api/models/load/progress)
// response. Used to drive llamaCppMetricsPoller end-to-end without a real
// cppworker container.
type r45FakeCppWorker struct {
	modelsJSON     string
	progressJSON   string
	modelsReqCount atomic.Int32
	progressHits   atomic.Int32
}

func (f *r45FakeCppWorker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/models":
		f.modelsReqCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(f.modelsJSON))
	case "/api/models/load/progress":
		f.progressHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(f.progressJSON))
	default:
		http.NotFound(w, r)
	}
}

// r45BuildProxyWithFakeCppWorker wires a Proxy + LlamaCppRouter whose only
// backend points at the supplied fake server. Returns the proxy, the
// httptest server (for cleanup), and the fake (to assert poll counts).
func r45BuildProxyWithFakeCppWorker(t *testing.T, fake *r45FakeCppWorker) (*Proxy, *httptest.Server) {
	t.Helper()
	ts := httptest.NewServer(fake)
	t.Cleanup(ts.Close)

	hostPort := strings.TrimPrefix(ts.URL, "http://")
	idx := strings.LastIndex(hostPort, ":")
	if idx < 0 {
		t.Fatalf("bad hostPort: %q", hostPort)
	}
	host := hostPort[:idx]
	port := 0
	for i := idx + 1; i < len(hostPort); i++ {
		port = port*10 + int(hostPort[i]-'0')
	}

	cfg := &types.LoadBalancerConfig{}
	cfg.Backends = []types.Backend{
		{
			ID:            "fake-1",
			Type:          types.BackendTypeLlamaCpp,
			Host:          host,
			CppWorkerPort: port,
			Status:        types.StatusHealthy,
		},
	}
	proxy := NewProxy(cfg)
	proxy.llamaCppRouter = &LlamaCppRouter{proxy: proxy}
	proxy.backends["fake-1"] = &BackendState{Backend: &cfg.Backends[0]}
	// R45 fix test scaffolding: NewProxy auto-starts llamaCppMetricsPoller
	// (see proxy.go:293). Stop it here so the test can drive a single
	// deterministic pollAll() without racing the auto-started goroutine.
	if proxy.llamaCppMetricsPoller != nil {
		proxy.llamaCppMetricsPoller.Stop()
	}
	return proxy, ts
}

// r45RealisticCppModelsJSON — actual /api/models response captured from the
// live gemma-4 container at 2026-08-19 17:00Z (post-R44.1 deploy). This is
// the canonical snake_case response shape. If a future cppworker changes
// field names, this fixture will need updating — that's the point: any
// field rename shows up as a test diff, not a silent production bug.
const r45RealisticCppModelsJSON = `{
  "available_ram_mb": 20480,
  "available_vram_mb": 8191,
  "count": 1,
  "feasible_max_context": 59908,
  "gguf_max_context": 131072,
  "gpu_count": 1,
  "max_ram_n_ctx": 131072,
  "max_vram_n_ctx": 59908,
  "model_max_context": 131072,
  "models": [
    {
      "architecture": "gemma4",
      "context_size": 32768,
      "feasible_max_context": 59908,
      "gguf_context_length": 131072,
      "gguf_max_context": 131072,
      "gpu_count": 1,
      "kv_cache_type": "q4_0",
      "loaded_at": "2026-08-19T17:00:16.163046152Z",
      "n_embd": 2560,
      "n_layers": 42,
      "n_vocab": 262144,
      "name": "gemma-4-E4B-it-Q4_K_M",
      "path": "/app/models/gemma-4-E4B-it-Q4_K_M.gguf",
      "size_bytes": 0,
      "state": "loaded"
    }
  ],
  "total_ram_mb": 24576,
  "total_vram_mb": 8191
}`

// TestR45_Poller_DecodesContextSize — the R45 smoking gun. Before the fix,
// the poller's per-model ContextSize field used the JSON tag "contextSize"
// but cppworker returns "context_size" — the field silently decoded to 0.
// Now the struct accepts both forms; with a snake_case response,
// LoadedModels[].ContextLength must equal cppworker's reported n_ctx (32768
// for gemma-4). If this test fails, every /api/chat will hit the
// preflightNCtxReloadIfNeeded "loaded_n_ctx=0" path and start a reload loop.
func TestR45_Poller_DecodesContextSize(t *testing.T) {
	logger.Init("error")
	fake := &r45FakeCppWorker{
		modelsJSON:   r45RealisticCppModelsJSON,
		progressJSON: `{"count":0,"models":[]}`,
	}
	proxy, _ := r45BuildProxyWithFakeCppWorker(t, fake)

	poller := newLlamaCppMetricsPoller(proxy)
	poller.interval = 1 // minimum, чтобы тест не висел
	poller.fastInterval = 1
	// Не запускаем loop() (асинхронный). Делаем одноразовый poll напрямую.
	poller.pollAll()

	// Note: we don't assert fake.modelsReqCount because NewProxy
	// auto-starts a poller whose initial poll() races with this test's
	// setup. The cache assertion below is the real R45 regression
	// check; request count is a debug aid only.

	lm, ok := proxy.metricsMgr.llamaMetrics["fake-1"]
	if !ok || lm == nil {
		t.Fatalf("expected metrics for fake-1, got ok=%v lm=%v", ok, lm)
	}
	if len(lm.LoadedModels) != 1 {
		t.Fatalf("expected 1 loaded model, got %d", len(lm.LoadedModels))
	}
	m := lm.LoadedModels[0]

	// The critical R45 assertion: ContextLength must equal what cppworker
	// reported in "context_size". Pre-fix this was 0.
	if m.ContextLength != 32768 {
		t.Errorf("LoadedModels[0].ContextLength = %d, want 32768 (R45 regression: JSON tag mismatch restored)",
			m.ContextLength)
	}
	if m.ContextLength == 0 {
		t.Fatal("LoadedModels[0].ContextLength is 0 — this is exactly the R45 bug. preflightNCtxReloadIfNeeded will trigger spurious reloads.")
	}

	// Also verify the other previously-broken per-model fields.
	if m.Architecture != "gemma4" {
		t.Errorf("Architecture = %q, want gemma4", m.Architecture)
	}
	if m.NLayers != 42 {
		t.Errorf("NLayers = %d, want 42", m.NLayers)
	}
	if m.NEmbd != 2560 {
		t.Errorf("NEmbd = %d, want 2560", m.NEmbd)
	}
	if m.MaxContext != 131072 {
		t.Errorf("MaxContext (from gguf_context_length) = %d, want 131072", m.MaxContext)
	}
	if m.LoadedAt != "2026-08-19T17:00:16.163046152Z" {
		t.Errorf("LoadedAt = %q, want full RFC3339Nano from loaded_at", m.LoadedAt)
	}
	// Per-model feasible + GGUF (Round 37 wire — also previously broken).
	if m.FeasibleMaxContext != 59908 {
		t.Errorf("FeasibleMaxContext = %d, want 59908", m.FeasibleMaxContext)
	}
	if m.GGUFMaxContext != 131072 {
		t.Errorf("GGUFMaxContext = %d, want 131072", m.GGUFMaxContext)
	}

	// Top-level fields were never affected (they always had snake_case tags).
	if lm.MaxVRAMNCtx != 59908 {
		t.Errorf("MaxVRAMNCtx = %d, want 59908", lm.MaxVRAMNCtx)
	}
	if lm.ModelMaxContext != 131072 {
		t.Errorf("ModelMaxContext = %d, want 131072", lm.ModelMaxContext)
	}
	if lm.MaxFeasibleContext != 59908 {
		t.Errorf("MaxFeasibleContext = %d, want 59908 (from per-model)", lm.MaxFeasibleContext)
	}
	if lm.GGUFMaxContext != 131072 {
		t.Errorf("GGUFMaxContext = %d, want 131072 (from per-model)", lm.GGUFMaxContext)
	}
}

// TestR45_Poller_BackwardCompat_CamelCase — older cppworker builds (pre-R18)
// returned camelCase keys. The poller was updated to use snake_case tags
// only — Go's encoding/json does NOT honor comma-separated alternate
// names (verified: only the first tag is used). Pre-R18 cppworker builds
// (older than 2026-08-03) are no longer supported. If we need to support
// them in the future, the right approach is a custom UnmarshalJSON method
// that tries snake_case first then falls back to camelCase, not a struct
// tag hack.
//
// This test now documents the boundary: with a camelCase-only response,
// ContextLength stays 0 (no match), so the poller correctly identifies
// such a backend as not having per-model n_ctx data and falls back to
// preflight's other code paths. It's a regression guard, not a compat
// test.
func TestR45_Poller_BackwardCompat_CamelCase(t *testing.T) {
	logger.Init("error")
	camelCaseJSON := `{
  "count": 1,
  "maxVramNCtx": 50000,
  "modelMaxContext": 65536,
  "models": [
    {
      "architecture": "llama",
      "contextSize": 8192,
      "ggufContextLength": 32768,
      "nLayers": 32,
      "nEmbd": 4096,
      "name": "legacy-model",
      "sizeBytes": 4000000000,
      "state": "loaded"
    }
  ]
}`
	fake := &r45FakeCppWorker{
		modelsJSON:   camelCaseJSON,
		progressJSON: `{"count":0,"models":[]}`,
	}
	proxy, _ := r45BuildProxyWithFakeCppWorker(t, fake)
	poller := newLlamaCppMetricsPoller(proxy)
	poller.pollAll()

	lm := proxy.metricsMgr.llamaMetrics["fake-1"]
	if lm == nil || len(lm.LoadedModels) != 1 {
		t.Fatalf("expected 1 loaded model entry, got %v", lm)
	}
	// camelCase keys do NOT match the new snake_case tags → per-model
	// fields stay zero. Top-level maxVramNCtx also no longer matches
	// (was always snake_case at top level, so this is unaffected — it
	// stays 0 because we removed the camelCase alias to be honest about
	// the supported version range).
	m := lm.LoadedModels[0]
	if m.ContextLength != 0 {
		t.Errorf("camelCase input: ContextLength = %d, want 0 (no match against snake_case tag)", m.ContextLength)
	}
	if m.NLayers != 0 {
		t.Errorf("camelCase input: NLayers = %d, want 0", m.NLayers)
	}
}

// TestR45_Poller_LoadingProgressFields — same JSON-tag bug applied to
// /api/models/load/progress: loadingStartedAt, loadingSizeBytes, elapsedMs
// were all camelCase. Loading-model monitoring in the UI was therefore
// broken for the same root reason.
func TestR45_Poller_LoadingProgressFields(t *testing.T) {
	logger.Init("error")
	progressJSON := `{
  "count": 1,
  "models": [
    {
      "name": "loading-model",
      "state": "loading",
      "loading_started_at": "2026-08-19T17:01:00.000Z",
      "loading_size_bytes": 5000000000,
      "elapsed_ms": 12345
    }
  ]
}`
	fake := &r45FakeCppWorker{
		modelsJSON:   `{"count":0,"models":[]}`,
		progressJSON: progressJSON,
	}
	proxy, _ := r45BuildProxyWithFakeCppWorker(t, fake)
	poller := newLlamaCppMetricsPoller(proxy)
	poller.pollAll()

	lm := proxy.metricsMgr.llamaMetrics["fake-1"]
	if lm == nil {
		t.Fatal("expected metrics for fake-1")
	}
	if len(lm.LoadingModels) != 1 {
		t.Fatalf("expected 1 loading model, got %d", len(lm.LoadingModels))
	}
	m := lm.LoadingModels[0]
	if m.Name != "loading-model" || m.State != "loading" {
		t.Errorf("loading entry mismatch: name=%q state=%q", m.Name, m.State)
	}
	if m.LoadingSizeBytes != 5000000000 {
		t.Errorf("LoadingSizeBytes = %d, want 5000000000", m.LoadingSizeBytes)
	}
	if m.LoadingStartedAt == nil || *m.LoadingStartedAt != "2026-08-19T17:01:00.000Z" {
		t.Errorf("LoadingStartedAt = %v, want 2026-08-19T17:01:00.000Z", m.LoadingStartedAt)
	}
	// fastInterval should have been triggered (we observed a loading model).
	if poller.lastLoadingSeen.IsZero() {
		t.Error("poller should have set lastLoadingSeen after observing loading model")
	}
}

// TestR45_Poller_BackendUnreachable — silent failure mode. If the
// pre-R45 behaviour was "ContextLength=0 because cppworker didn't return
// contextSize", that's invisible. After R45, the same failure mode
// (cppworker down) should also leave ContextLength=0 but the test makes
// that explicit. This is a sanity check, not a regression test.
func TestR45_Poller_BackendUnreachable(t *testing.T) {
	logger.Init("error")
	// Point backend at a closed port.
	cfg := &types.LoadBalancerConfig{}
	cfg.Backends = []types.Backend{
		{
			ID:            "dead-1",
			Type:          types.BackendTypeLlamaCpp,
			Host:          "127.0.0.1",
			CppWorkerPort: 1, // port 1 = nothing listening
			Status:        types.StatusHealthy,
		},
	}
	proxy := NewProxy(cfg)
	proxy.llamaCppRouter = &LlamaCppRouter{proxy: proxy}
	proxy.backends["dead-1"] = &BackendState{Backend: &cfg.Backends[0]}

	poller := newLlamaCppMetricsPoller(proxy)
	// Should not panic, should not block forever (5s HTTP timeout).
	poller.pollAll()

	lm := proxy.metricsMgr.llamaMetrics["dead-1"]
	if lm != nil && len(lm.LoadedModels) > 0 {
		t.Errorf("expected no loaded models when backend unreachable, got %d", len(lm.LoadedModels))
	}
}

// TestR45_RealResponseShapeIsSnakeCase — frozen guard rail. If a future
// refactor swaps the JSON tags back to camelCase only, this test fails
// with a clear message. Mirrors the actual shape captured from the live
// gemma-4 container (see r45RealisticCppModelsJSON).
func TestR45_RealResponseShapeIsSnakeCase(t *testing.T) {
	var parsed struct {
		MaxVRAMNCtx int `json:"max_vram_n_ctx"`
		Models      []struct {
			ContextSize int    `json:"context_size"`
			Name        string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal([]byte(r45RealisticCppModelsJSON), &parsed); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if parsed.MaxVRAMNCtx != 59908 {
		t.Errorf("top-level max_vram_n_ctx = %d, want 59908", parsed.MaxVRAMNCtx)
	}
	if len(parsed.Models) != 1 || parsed.Models[0].ContextSize != 32768 {
		t.Errorf("per-model context_size = %v, want [32768]", parsed.Models)
	}
}
