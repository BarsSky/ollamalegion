// Тесты для 3-tier n_ctx resolver (Шаг 7 cppworker-preflight-nctx-session).
//
// Покрывает:
//   - ExtractNumCtxFromBody: Ollama (options.num_ctx) и OpenAI (top-level num_ctx)
//   - GetModelProfileNumCtx, GetModelProfile, SetModelProfile, DeleteModelProfile
//   - GetBackendDefaultNumCtx (только для llama_cpp)
//   - ResolveNumCtx: приоритет body > profile > backend
//   - ApplyCppCtxHeader: мутация r.Header
//   - ValidateProfile: границы [256, 262144]
//   - IsLlamaCppModelLoaded: case-insensitive substring match
package balancer

import (
	"net/http"
	"testing"

	"ollama-loadbalancer/pkg/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================
// ExtractNumCtxFromBody
// ============================================================

func TestExtractNumCtxFromBody_Empty(t *testing.T) {
	assert.Equal(t, 0, ExtractNumCtxFromBody(nil))
	assert.Equal(t, 0, ExtractNumCtxFromBody([]byte("")))
}

func TestExtractNumCtxFromBody_InvalidJSON(t *testing.T) {
	assert.Equal(t, 0, ExtractNumCtxFromBody([]byte("not json")))
}

func TestExtractNumCtxFromBody_OllamaFormat(t *testing.T) {
	body := []byte(`{"model":"gemma-4","options":{"num_ctx":16384}}`)
	assert.Equal(t, 16384, ExtractNumCtxFromBody(body))
}

func TestExtractNumCtxFromBody_OpenAIFormat(t *testing.T) {
	body := []byte("{\"model\":\"gemma-4\",\"num_ctx\":8192}")
	assert.Equal(t, 8192, ExtractNumCtxFromBody(body))
}

func TestExtractNumCtxFromBody_BodyWins(t *testing.T) {
	// Если оба формата заданы — Ollama options.num_ctx имеет приоритет
	body := []byte("{\"options\":{\"num_ctx\":4096},\"num_ctx\":8192}")
	assert.Equal(t, 4096, ExtractNumCtxFromBody(body))
}

func TestExtractNumCtxFromBody_NumCtxZero(t *testing.T) {
	body := []byte(`{"options":{"num_ctx":0}}`)
	assert.Equal(t, 0, ExtractNumCtxFromBody(body))
}

func TestExtractNumCtxFromBody_NumCtxFloat(t *testing.T) {
	// JSON декодирует числа как float64
	body := []byte(`{"options":{"num_ctx":16384.0}}`)
	assert.Equal(t, 16384, ExtractNumCtxFromBody(body))
}

// ============================================================
// GetModelProfileNumCtx / Set / Get / Delete
// ============================================================

func TestModelProfile_CRUD(t *testing.T) {
	p := &Proxy{config: &types.LoadBalancerConfig{}}

	// Изначально пусто
	assert.Equal(t, 0, p.GetModelProfileNumCtx("gemma-4"))
	_, ok := p.GetModelProfile("gemma-4")
	assert.False(t, ok)

	// Set
	p.SetModelProfile("gemma-4", types.LlamaCppModelProfile{
		ContextLength: 16384,
	})
	assert.Equal(t, 16384, p.GetModelProfileNumCtx("gemma-4"))
	got, ok := p.GetModelProfile("gemma-4")
	assert.True(t, ok)
	assert.Equal(t, 16384, got.ContextLength)

	// Update
	p.SetModelProfile("gemma-4", types.LlamaCppModelProfile{
		ContextLength: 32768,
		BatchSize:     1024,
	})
	assert.Equal(t, 32768, p.GetModelProfileNumCtx("gemma-4"))

	// Delete
	assert.True(t, p.DeleteModelProfile("gemma-4"))
	assert.False(t, p.DeleteModelProfile("gemma-4")) // уже удалён
	assert.Equal(t, 0, p.GetModelProfileNumCtx("gemma-4"))
}

func TestModelProfile_NilConfig(t *testing.T) {
	var p *Proxy
	assert.Equal(t, 0, p.GetModelProfileNumCtx("gemma-4"))
	_, ok := p.GetModelProfile("gemma-4")
	assert.False(t, ok)
	p.SetModelProfile("gemma-4", types.LlamaCppModelProfile{ContextLength: 8192}) //nolint:staticcheck
	assert.False(t, p.DeleteModelProfile("gemma-4"))
}

// ============================================================
// GetBackendDefaultNumCtx
// ============================================================

func TestGetBackendDefaultNumCtx_OnlyLlamaCpp(t *testing.T) {
	p := &Proxy{
		config: &types.LoadBalancerConfig{
			Backends: []types.Backend{
				{
					ID:  "ollama-1",
					Type: types.BackendTypeOllama,
				},
				{
					ID:              "llama-1",
					Type:            types.BackendTypeLlamaCpp,
					CppWorkerConfig: &types.LlamaCppConfig{ContextLength: 4096},
				},
			},
		},
	}

	// Ollama бэкенд возвращает 0 (resolver его не использует)
	assert.Equal(t, 0, p.GetBackendDefaultNumCtx("ollama-1"))

	// LlamaCpp бэкенд возвращает ContextLength
	assert.Equal(t, 4096, p.GetBackendDefaultNumCtx("llama-1"))

	// Неизвестный бэкенд
	assert.Equal(t, 0, p.GetBackendDefaultNumCtx("unknown"))
}

func TestGetBackendDefaultNumCtx_NoCppConfig(t *testing.T) {
	p := &Proxy{
		config: &types.LoadBalancerConfig{
			Backends: []types.Backend{
				{
					ID:              "llama-1",
					Type:            types.BackendTypeLlamaCpp,
					CppWorkerConfig: nil,
				},
			},
		},
	}
	assert.Equal(t, 0, p.GetBackendDefaultNumCtx("llama-1"))
}

// ============================================================
// ResolveNumCtx — приоритет body > profile > backend
// ============================================================

func TestResolveNumCtx_BodyWinsOverProfile(t *testing.T) {
	p := &Proxy{
		config: &types.LoadBalancerConfig{
			LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
				"gemma-4": {ContextLength: 16384},
			},
			Backends: []types.Backend{
				{ID: "llama-1", Type: types.BackendTypeLlamaCpp, CppWorkerConfig: &types.LlamaCppConfig{ContextLength: 8192}},
			},
		},
	}
	body := []byte(`{"options":{"num_ctx":4096}}`)

	r := p.ResolveNumCtx("gemma-4", body, "llama-1")
	assert.Equal(t, 4096, r.Value)
	assert.Equal(t, NumCtxSourceRequest, r.Source)
}

func TestResolveNumCtx_ProfileWinsOverBackend(t *testing.T) {
	p := &Proxy{
		config: &types.LoadBalancerConfig{
			LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
				"gemma-4": {ContextLength: 16384},
			},
			Backends: []types.Backend{
				{ID: "llama-1", Type: types.BackendTypeLlamaCpp, CppWorkerConfig: &types.LlamaCppConfig{ContextLength: 8192}},
			},
		},
	}
	r := p.ResolveNumCtx("gemma-4", nil, "llama-1")
	assert.Equal(t, 16384, r.Value)
	assert.Equal(t, NumCtxSourceProfile, r.Source)
}

func TestResolveNumCtx_FallbackToBackend(t *testing.T) {
	p := &Proxy{
		config: &types.LoadBalancerConfig{
			Backends: []types.Backend{
				{ID: "llama-1", Type: types.BackendTypeLlamaCpp, CppWorkerConfig: &types.LlamaCppConfig{ContextLength: 8192}},
			},
		},
	}
	r := p.ResolveNumCtx("gemma-4", nil, "llama-1")
	assert.Equal(t, 8192, r.Value)
	assert.Equal(t, NumCtxSourceBackend, r.Source)
}

func TestResolveNumCtx_NoneFound(t *testing.T) {
	p := &Proxy{config: &types.LoadBalancerConfig{}}
	r := p.ResolveNumCtx("gemma-4", nil, "llama-1")
	assert.Equal(t, 0, r.Value)
	assert.Equal(t, NumCtxSourceNone, r.Source)
}

// ============================================================
// ApplyCppCtxHeader
// ============================================================

func TestApplyCppCtxHeader_SetsHeaderOnResolved(t *testing.T) {
	p := &Proxy{
		config: &types.LoadBalancerConfig{
			LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
				"gemma-4": {ContextLength: 16384},
			},
		},
	}
	r, _ := http.NewRequest("GET", "/api/chat", nil)
	resolved := p.ApplyCppCtxHeader(r, "gemma-4", nil, "")

	assert.Equal(t, 16384, resolved.Value)
	assert.Equal(t, "16384", r.Header.Get("X-Cpp-Ctx"))
}

func TestApplyCppCtxHeader_NoHeaderWhenZero(t *testing.T) {
	p := &Proxy{config: &types.LoadBalancerConfig{}}
	r, _ := http.NewRequest("GET", "/api/chat", nil)
	resolved := p.ApplyCppCtxHeader(r, "gemma-4", nil, "")

	assert.Equal(t, 0, resolved.Value)
	assert.Equal(t, "", r.Header.Get("X-Cpp-Ctx"))
}

func TestApplyCppCtxHeader_BodyWinsHeaders(t *testing.T) {
	// body содержит num_ctx — он должен использоваться,
	// а header НЕ должен устанавливаться повторно (т.к. resolver уже выбрал body)
	p := &Proxy{
		config: &types.LoadBalancerConfig{
			LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
				"gemma-4": {ContextLength: 16384},
			},
		},
	}
	r, _ := http.NewRequest("GET", "/api/chat", nil)
	body := []byte(`{"options":{"num_ctx":4096}}`)

	resolved := p.ApplyCppCtxHeader(r, "gemma-4", body, "")

	assert.Equal(t, 4096, resolved.Value)
	assert.Equal(t, NumCtxSourceRequest, resolved.Source)
	assert.Equal(t, "4096", r.Header.Get("X-Cpp-Ctx"))
}

// ============================================================
// ValidateProfile
// ============================================================

func TestValidateProfile_Valid(t *testing.T) {
	cases := []int{256, 4096, 16384, 262144}
	for _, n := range cases {
		p := types.LlamaCppModelProfile{ContextLength: n}
		assert.NoError(t, ValidateProfile(p), "contextLength=%d", n)
	}
}

func TestValidateProfile_OutOfRange(t *testing.T) {
	cases := []int{255, 262145, 1_000_000}
	for _, n := range cases {
		p := types.LlamaCppModelProfile{ContextLength: n}
		err := ValidateProfile(p)
		assert.Error(t, err, "contextLength=%d should fail", n)
	}
}

func TestValidateProfile_ZeroIsAllowed(t *testing.T) {
	// Zero в ContextLength = "не задано" — допустимо
	p := types.LlamaCppModelProfile{ContextLength: 0}
	assert.NoError(t, ValidateProfile(p))
}

func TestValidateProfile_BatchSize(t *testing.T) {
	// batchSize < 1 → ошибка
	err := ValidateProfile(types.LlamaCppModelProfile{ContextLength: 4096, BatchSize: 0})
	assert.NoError(t, err) // 0 = не задано
	err = ValidateProfile(types.LlamaCppModelProfile{ContextLength: 4096, BatchSize: 1})
	assert.NoError(t, err)
	err = ValidateProfile(types.LlamaCppModelProfile{ContextLength: 4096, BatchSize: 512})
	assert.NoError(t, err)
}

func TestValidateProfile_NumGpuLayers(t *testing.T) {
	// -1 = все слои, ≥0 — допустимо, <-1 — ошибка
	assert.NoError(t, ValidateProfile(types.LlamaCppModelProfile{ContextLength: 4096, NumGPULayers: -1}))
	assert.NoError(t, ValidateProfile(types.LlamaCppModelProfile{ContextLength: 4096, NumGPULayers: 0}))
	assert.NoError(t, ValidateProfile(types.LlamaCppModelProfile{ContextLength: 4096, NumGPULayers: 100}))
	err := ValidateProfile(types.LlamaCppModelProfile{ContextLength: 4096, NumGPULayers: -2})
	require.Error(t, err)
}

// ============================================================
// IsLlamaCppModelLoaded (case-insensitive substring match)
// ============================================================

func TestIsLlamaCppModelLoaded_ExactMatch(t *testing.T) {
	p := &Proxy{
		metricsMgr: &MetricsManager{
			llamaMetrics: map[string]*types.LlamaCppMetrics{
				"llama-1": {
					LoadedModels: []types.LlamaCppModel{
						{Name: "gemma-4-E4B-it-Q4_K_M"},
					},
				},
			},
		},
	}
	assert.True(t, p.IsLlamaCppModelLoaded("llama-1", "gemma-4-E4B-it-Q4_K_M"))
}

func TestIsLlamaCppModelLoaded_CaseInsensitive(t *testing.T) {
	p := &Proxy{
		metricsMgr: &MetricsManager{
			llamaMetrics: map[string]*types.LlamaCppMetrics{
				"llama-1": {
					LoadedModels: []types.LlamaCppModel{
						{Name: "gemma-4-E4B-it-Q4_K_M.gguf"},
					},
				},
			},
		},
	}
	assert.True(t, p.IsLlamaCppModelLoaded("llama-1", "gemma-4-e4b-it-q4_k_m"))
}

func TestIsLlamaCppModelLoaded_NotLoaded(t *testing.T) {
	p := &Proxy{
		metricsMgr: &MetricsManager{
			llamaMetrics: map[string]*types.LlamaCppMetrics{
				"llama-1": {
					LoadedModels: []types.LlamaCppModel{
						{Name: "llama-3.1-8b"},
					},
				},
			},
		},
	}
	assert.False(t, p.IsLlamaCppModelLoaded("llama-1", "gemma-4-E4B-it-Q4_K_M"))
}

func TestIsLlamaCppModelLoaded_NoMetrics(t *testing.T) {
	p := &Proxy{
		metricsMgr: &MetricsManager{
			llamaMetrics: map[string]*types.LlamaCppMetrics{},
		},
	}
	assert.False(t, p.IsLlamaCppModelLoaded("llama-1", "gemma-4"))
}

func TestIsLlamaCppModelLoaded_NilProxy(t *testing.T) {
	var p *Proxy
	assert.False(t, p.IsLlamaCppModelLoaded("llama-1", "gemma-4"))
}

// ============================================================
// containsFold helper
// ============================================================

func TestContainsFold(t *testing.T) {
	assert.True(t, containsFold("gemma-4-E4B-it-Q4_K_M", "gemma-4"))
	assert.True(t, containsFold("GEMMA-4", "gemma-4"))
	assert.True(t, containsFold("abc", "abc"))
	assert.False(t, containsFold("gemma", "gemmox"))
	assert.False(t, containsFold("ab", "abc")) // needle длиннее — false
	assert.False(t, containsFold("", "abc"))
}
