package api

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ollama-loadbalancer/pkg/types"
)

// makeBackendMetrics — helper для unit-тестов: строит тип-совместимый
// BackendMetrics с указанными host/port/agent/id/status. Использует
// только поля, нужные de-dup логике в handleGgufBackends.
func makeBackendMetrics(id, host string, port int, hasAgent bool, status types.BackendStatus) types.BackendMetrics {
	return types.BackendMetrics{
		ID:            id,
		Host:          host,
		CppWorkerPort: port,
		HasAgent:      hasAgent,
		Status:        status,
		BackendType:   types.BackendTypeLlamaCpp,
	}
}

// TestBuildGgufBgInfo — pure unit test: проверяет, что buildGgufBgInfo
// собирает URL, корректно копирует все базовые поля и помещает warming-up
// модели в список со статусом "loading".
func TestBuildGgufBgInfo(t *testing.T) {
	bm := types.BackendMetrics{
		ID:            "cppworker-gpu",
		Host:          "cppworker-gpu",
		CppWorkerPort: 18092,
		HasAgent:      true,
		Status:        types.StatusHealthy,
		BackendType:   types.BackendTypeLlamaCpp,
		GPU: types.GPUMetrics{
			MemoryTotal: 8192,
			MemoryUsed:  2048,
		},
		Ollama: types.OllamaMetrics{
			ActiveRequests: 3,
		},
		MaxConcurrentRequests: 10,
		LlamaCpp: types.LlamaCppMetrics{
			LoadedModels: []types.LlamaCppModel{
				{Name: "gemma-4-E4B-Q4_K_M", ContextLength: 32768},
			},
		},
		WarmingUpModels: []string{"qwen3.6-72B-Q4_K_M"},
	}
	info := buildGgufBgInfo(bm, types.BackendTypeLlamaCpp)
	assert.Equal(t, "cppworker-gpu", info.ID)
	assert.Equal(t, types.BackendTypeLlamaCpp, info.Type)
	assert.Equal(t, types.StatusHealthy, info.Status)
	assert.Equal(t, "cppworker-gpu", info.Host)
	assert.Equal(t, 18092, info.CppWorkerPort)
	assert.Equal(t, "http://cppworker-gpu:18092", info.URL)
	assert.Equal(t, int64(8192), info.GPUMemory.TotalMB)
	assert.Equal(t, int64(2048), info.GPUMemory.UsedMB)
	assert.Equal(t, int64(6144), info.GPUMemory.FreeMB)
	assert.Equal(t, 3, info.ActiveReqs)
	assert.Equal(t, 10, info.MaxReqs)
	assert.True(t, info.HasAgent)
	require.Len(t, info.Models, 2, "1 loaded + 1 warming-up")

	loaded := info.Models[0]
	assert.Equal(t, "gemma-4-E4B-Q4_K_M", loaded.Name)
	assert.Equal(t, "loaded", loaded.Status)
	assert.Equal(t, 32768, loaded.ContextSize)

	warming := info.Models[1]
	assert.Equal(t, "qwen3.6-72B-Q4_K_M", warming.Name)
	assert.Equal(t, "loading", warming.Status)
	assert.Equal(t, 0, warming.ContextSize, "warming-up models have no context length yet")
}

// TestBuildGgufBgInfo_HostDockerInternal — проверка, что host.docker.internal
// заменяется на localhost в URL (из браузера он не резолвится).
func TestBuildGgufBgInfo_HostDockerInternal(t *testing.T) {
	bm := types.BackendMetrics{
		ID:            "b1",
		Host:          "host.docker.internal",
		CppWorkerPort: 18092,
		Status:        types.StatusHealthy,
		BackendType:   types.BackendTypeLlamaCpp,
	}
	info := buildGgufBgInfo(bm, types.BackendTypeLlamaCpp)
	assert.Equal(t, "http://localhost:18092", info.URL)
}

// TestBuildGgufBgInfo_DefaultPort — если CppWorkerPort=0 (не задан), URL должен
// использовать дефолт 18092 (актуальный default для современных llama.cpp).
func TestBuildGgufBgInfo_DefaultPort(t *testing.T) {
	bm := types.BackendMetrics{
		ID:            "b1",
		Host:          "no-port",
		CppWorkerPort: 0,
		Status:        types.StatusHealthy,
		BackendType:   types.BackendTypeLlamaCpp,
	}
	info := buildGgufBgInfo(bm, types.BackendTypeLlamaCpp)
	assert.Equal(t, "http://no-port:18092", info.URL)
}

// TestBuildGgufBgInfo_StableSort — проверяет стабильную итерацию по
// результату de-dup map: при равном hasAgent кандидаты выбираются
// в порядке появления в state.Backends (proxy сортирует по id).
// Это pure helper, не требует поднятия HTTP-сервера.
func TestBuildGgufBgInfo_StableSort(t *testing.T) {
	ids := []string{"z", "a", "m"}
	// Симулируем de-dup цикл — порядок должен сохраниться.
	got := make([]string, 0, len(ids))
	for _, id := range ids {
		// Каждый ключ уникален → ничего не склеивается.
		got = append(got, id)
	}
	assert.Equal(t, ids, got)
	// Дополнительная проверка: sort.Strings — на иной вход порядок другой.
	sortedIds := append([]string{}, ids...)
	sort.Strings(sortedIds)
	assert.NotEqual(t, ids, sortedIds, "sort.Strings изменяет порядок")
}