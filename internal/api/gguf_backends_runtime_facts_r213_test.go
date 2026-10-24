// R213 (2026-10-21): /api/v1/gguf/backends обязан отдавать ФАКТЫ загруженного
// инстанса (kv_cache_type, gpu_layers, parallel, batch_size, ...), а не только
// name/status/contextSize.
//
// Живая жалоба оператора: «везде в WebUI настройках стоит квантизация q4_0, а в
// отображении загруженных параметров — f16». Причина: карточка модели читает
// факты из этого ответа (gguf-renderer-detail-render.js → paramFact/loadedParamsFor),
// ключа kv_cache_type в ответе не было, и рендер печатал догадку «f16» как факт.
//
// Тест падает до правки: в ggufBgModelInfo не было полей, и JSON не содержал
// ни kv_cache_type, ни gpu_layers.
package api

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ollama-loadbalancer/pkg/types"
)

func r213BackendMetrics() types.BackendMetrics {
	reasoning := false
	return types.BackendMetrics{
		ID:            "cppworker-gpu-bundled-agent",
		Host:          "cppworker-gpu",
		CppWorkerPort: 18092,
		Status:        types.StatusHealthy,
		BackendType:   types.BackendTypeLlamaCpp,
		LlamaCpp: types.LlamaCppMetrics{
			LoadedModels: []types.LlamaCppModel{
				{
					Name:           "gemma-4-E4B-it-Q4_K_M",
					Path:           "/app/models/gemma-4-E4B-it-Q4_K_M.gguf",
					State:          "loaded",
					ContextLength:  8192,
					ContextPerSeq:  8192,
					BatchSize:      512,
					Parallel:       1,
					MaxSlots:       1,
					NumGPULayers:   42,
					KvCacheType:    "q4_0",
					Quantization:   "Q4_K_M",
					Size:           4215695776,
					NLayers:        42,
					Architecture:   "gemma4",
					GGUFMaxContext: 131072,
					// R213: воркер не отдаёт gpu_layers — раскладка авторитетна.
					DeviceLayersExact: []types.DeviceLayerRange{
						{Device: "CPU", From: 0, To: 0, Count: 1},
						{Device: "CUDA0", From: 1, To: 42, Count: 42},
					},
					ReasoningEnabled: &reasoning,
				},
			},
		},
	}
}

func TestBuildGgufBgInfo_RuntimeFacts_R213(t *testing.T) {
	info := buildGgufBgInfo(r213BackendMetrics(), types.BackendTypeLlamaCpp)
	require.Len(t, info.Models, 1)
	m := info.Models[0]

	assert.Equal(t, "gemma-4-E4B-it-Q4_K_M", m.Name)
	assert.Equal(t, 8192, m.ContextSize)
	// Главное: kv_cache_type — это то, что оператор видит как «загруженные параметры».
	assert.Equal(t, "q4_0", m.KvCacheType,
		"kv_cache_type обязан доехать до WebUI: без него карточка печатает догадку f16")
	assert.Equal(t, 42, m.GPULayers)
	assert.Equal(t, 512, m.BatchSize)
	assert.Equal(t, 1, m.Parallel)
	assert.Equal(t, 1, m.MaxSlots)
	assert.Equal(t, 8192, m.ContextPerSeq)
	assert.Equal(t, "Q4_K_M", m.Quantization)
	assert.Equal(t, uint64(4215695776), m.SizeBytes)
	assert.Equal(t, 42, m.NLayers)
	assert.Equal(t, "gemma4", m.Architecture)
	assert.Equal(t, 131072, m.GGUFMaxContext)
	require.Len(t, m.DeviceLayersExact, 2)
	require.NotNil(t, m.ReasoningEnabled)
	assert.False(t, *m.ReasoningEnabled)
}

// JSON-имена — контракт с WebUI: рендер читает snake_case (kv_cache_type,
// gpu_layers, context_per_seq, device_layers_exact). Переименование поля в
// camelCase молча вернёт «f16 / 0 слоёв» в карточку.
func TestGgufBgModelInfo_JSONKeys_R213(t *testing.T) {
	info := buildGgufBgInfo(r213BackendMetrics(), types.BackendTypeLlamaCpp)
	raw, err := json.Marshal(info)
	require.NoError(t, err)

	var doc struct {
		Models []map[string]interface{} `json:"models"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	require.Len(t, doc.Models, 1)
	m := doc.Models[0]

	assert.Equal(t, "q4_0", m["kv_cache_type"])
	assert.Equal(t, float64(42), m["gpu_layers"])
	assert.Equal(t, float64(512), m["batch_size"])
	assert.Equal(t, float64(1), m["parallel"])
	assert.Equal(t, float64(1), m["max_slots"])
	assert.Equal(t, float64(8192), m["context_per_seq"])
	assert.Equal(t, "Q4_K_M", m["quantization"])
	assert.Equal(t, "gemma4", m["architecture"])
	assert.Equal(t, float64(131072), m["gguf_max_context"])
	assert.Contains(t, m, "device_layers_exact")
	// status/contextSize — прежний контракт, не должен пострадать.
	assert.Equal(t, "loaded", m["status"])
	assert.Equal(t, float64(8192), m["contextSize"])
}

// Модель без фактов (старая сборка воркера / только загружается) не должна
// выдумывать значения: пустые ключи остаются пустыми (omitempty), UI обязан
// показать прочерк, а не f16.
func TestBuildGgufBgInfo_NoFactsStaysEmpty_R213(t *testing.T) {
	bm := types.BackendMetrics{
		ID:            "cppworker-old",
		Host:          "old",
		CppWorkerPort: 18092,
		Status:        types.StatusHealthy,
		BackendType:   types.BackendTypeLlamaCpp,
		LlamaCpp: types.LlamaCppMetrics{
			LoadedModels: []types.LlamaCppModel{{Name: "m", State: "loaded"}},
		},
	}
	info := buildGgufBgInfo(bm, types.BackendTypeLlamaCpp)
	require.Len(t, info.Models, 1)
	m := info.Models[0]
	assert.Empty(t, m.KvCacheType)
	assert.Zero(t, m.GPULayers)
	assert.Zero(t, m.BatchSize)

	raw, err := json.Marshal(info)
	require.NoError(t, err)
	var doc struct {
		Models []map[string]interface{} `json:"models"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	assert.NotContains(t, doc.Models[0], "kv_cache_type",
		"отсутствующий факт не должен подменяться значением по умолчанию на стороне API")
}
