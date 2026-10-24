package api

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"ollama-loadbalancer/pkg/types"
)

// R119: /api/v1/gguf/backends врал статусом модели — поле Status жёстко
// выставлялось в "loaded" независимо от m.State.
//
// Живой дефект (2026-10-15, стенд): cppworker отдавал state="loading",
// context_size=0, а страница GGUF показывала «загружено». Оператор не видел
// ни идущей загрузки, ни провала загрузки.
//
// Тест падает до правки: buildGgufBgInfo возвращал "loaded" для loading/error.
func TestBuildGgufBgInfo_HonestModelStatus_R119(t *testing.T) {
	bm := types.BackendMetrics{
		ID:            "cppworker-gpu",
		Host:          "cppworker-gpu",
		CppWorkerPort: 18092,
		Status:        types.StatusHealthy,
		BackendType:   types.BackendTypeLlamaCpp,
		LlamaCpp: types.LlamaCppMetrics{
			LoadedModels: []types.LlamaCppModel{
				{Name: "m-loaded", State: "loaded", ContextLength: 32768},
				{Name: "m-loading", State: "loading", ContextLength: 0},
				{Name: "m-error", State: "error", ContextLength: 0},
				{Name: "m-nostate", ContextLength: 8192},
			},
		},
	}

	info := buildGgufBgInfo(bm, types.BackendTypeLlamaCpp)
	assert.Len(t, info.Models, 4)

	byName := map[string]ggufBgModelInfo{}
	for _, m := range info.Models {
		byName[m.Name] = m
	}

	assert.Equal(t, "loaded", byName["m-loaded"].Status)
	assert.Equal(t, "loading", byName["m-loading"].Status,
		"loading-модель обязана показываться как loading (иначе оператор видит «загружено» при ctx=0)")
	assert.Equal(t, "error", byName["m-error"].Status,
		"провалившаяся загрузка обязана показываться как error")
	assert.Equal(t, "loaded", byName["m-nostate"].Status,
		"пустой state (старые сборки cppworker) = loaded, как в поллере балансера")

	// Контекст по-прежнему прокидывается как есть.
	assert.Equal(t, 32768, byName["m-loaded"].ContextSize)
	assert.Equal(t, 0, byName["m-loading"].ContextSize)
}

// Контроль: warming-up модель без записи в LoadedModels остаётся "loading",
// но если она уже есть в LoadedModels (с честным state), дубликат не создаётся.
func TestBuildGgufBgInfo_WarmingUpNoDuplicate_R119(t *testing.T) {
	bm := types.BackendMetrics{
		ID:            "cppworker-gpu",
		Host:          "cppworker-gpu",
		CppWorkerPort: 18092,
		Status:        types.StatusHealthy,
		BackendType:   types.BackendTypeLlamaCpp,
		LlamaCpp: types.LlamaCppMetrics{
			LoadedModels: []types.LlamaCppModel{
				{Name: "m-loading", State: "loading"},
			},
		},
		WarmingUpModels: []string{"m-loading", "m-warm-only"},
	}

	info := buildGgufBgInfo(bm, types.BackendTypeLlamaCpp)
	assert.Len(t, info.Models, 2, "loading-модель не должна дублироваться warming-up списком")
	for _, m := range info.Models {
		assert.Equal(t, "loading", m.Status, "модель %q", m.Name)
	}
}
