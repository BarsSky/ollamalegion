package tests

import (
	"encoding/json"
	"net/http"
	"testing"

	"ollama-loadbalancer/pkg/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ==================== Тесты фильтрации нерабочих бэкендов ====================

// TestGgufBackends_HidesUnhealthyByDefault проверяет, что /api/v1/gguf/backends
// по умолчанию не возвращает бэкенды со статусами unhealthy/offline/draining/ollama_unavailable.
func TestGgufBackends_HidesUnhealthyByDefault(t *testing.T) {
	testServer, _, proxy, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	// Добавляем healthy llama_cpp бэкенд
	require.NoError(t, proxy.AddBackend(types.Backend{
		ID:                "llamacpp-healthy",
		Name:              "Healthy CppWorker",
		Host:              "localhost",
		CppWorkerPort:     18092,
		Type:              types.BackendTypeLlamaCpp,
		Status:            types.StatusHealthy,
		MaxConcurrentReqs: 4,
	}))

	// Добавляем unhealthy llama_cpp бэкенды
	for _, status := range []types.BackendStatus{
		types.StatusUnhealthy,
		types.StatusOffline,
		types.StatusDraining,
		types.StatusOllamaUnavailable,
	} {
		id := "llamacpp-" + string(status)
		require.NoError(t, proxy.AddBackend(types.Backend{
			ID:                id,
			Name:              string(status),
			Host:              "localhost",
			CppWorkerPort:     18092,
			Type:              types.BackendTypeLlamaCpp,
			Status:            status,
			MaxConcurrentReqs: 4,
		}))
	}

	// По умолчанию должны видеть только healthy
	resp, err := http.Get(baseURL + "/api/v1/gguf/backends")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Backends []map[string]interface{} `json:"backends"`
		Total    int                      `json:"total"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	assert.Equal(t, 1, result.Total)
	require.Len(t, result.Backends, 1)
	assert.Equal(t, "llamacpp-healthy", result.Backends[0]["id"])
}

// TestGgufBackends_IncludeUnhealthyParameter проверяет, что query-параметр
// includeUnhealthy=true возвращает все llama_cpp бэкенды, включая нерабочие.
//
// Round 7 fix: используем РАЗНЫЕ CppWorkerPort для двух бэкендов, потому что
// dedupBackendsByHostPort склеивает записи с одинаковым (host, port) — это
// корректное поведение (один физический endpoint = одна запись).
func TestGgufBackends_IncludeUnhealthyParameter(t *testing.T) {
	testServer, _, proxy, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	// Добавляем healthy и unhealthy llama_cpp бэкенды на РАЗНЫХ портах.
	require.NoError(t, proxy.AddBackend(types.Backend{
		ID:                "llamacpp-healthy",
		Name:              "Healthy CppWorker",
		Host:              "localhost",
		CppWorkerPort:     18092,
		Type:              types.BackendTypeLlamaCpp,
		Status:            types.StatusHealthy,
		MaxConcurrentReqs: 4,
	}))
	require.NoError(t, proxy.AddBackend(types.Backend{
		ID:                "llamacpp-unhealthy",
		Name:              "Unhealthy CppWorker",
		Host:              "localhost",
		CppWorkerPort:     18093, // другой порт → не dedup'ится
		Type:              types.BackendTypeLlamaCpp,
		Status:            types.StatusUnhealthy,
		MaxConcurrentReqs: 4,
	}))

	resp, err := http.Get(baseURL + "/api/v1/gguf/backends?includeUnhealthy=true")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Backends []map[string]interface{} `json:"backends"`
		Total    int                      `json:"total"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	assert.Equal(t, 2, result.Total)
	require.Len(t, result.Backends, 2)

	ids := make([]string, 0, len(result.Backends))
	for _, b := range result.Backends {
		ids = append(ids, b["id"].(string))
	}
	assert.ElementsMatch(t, []string{"llamacpp-healthy", "llamacpp-unhealthy"}, ids)
}

// TestBackendsList_HidesUnhealthyByDefault проверяет, что /api/v1/backends
// по умолчанию не возвращает нерабочие бэкенды.
func TestBackendsList_HidesUnhealthyByDefault(t *testing.T) {
	testServer, _, proxy, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	// Добавляем healthy и unhealthy бэкенды разных типов
	require.NoError(t, proxy.AddBackend(types.Backend{
		ID:                "llamacpp-healthy",
		Name:              "Healthy CppWorker",
		Host:              "localhost",
		CppWorkerPort:     18092,
		Type:              types.BackendTypeLlamaCpp,
		Status:            types.StatusHealthy,
		MaxConcurrentReqs: 4,
	}))
	require.NoError(t, proxy.AddBackend(types.Backend{
		ID:                "llamacpp-unhealthy",
		Name:              "Unhealthy CppWorker",
		Host:              "localhost",
		CppWorkerPort:     18092,
		Type:              types.BackendTypeLlamaCpp,
		Status:            types.StatusUnhealthy,
		MaxConcurrentReqs: 4,
	}))
	require.NoError(t, proxy.AddBackend(types.Backend{
		ID:                "ollama-offline",
		Name:              "Offline Ollama",
		Host:              "localhost",
		OllamaPort:        11434,
		Type:              types.BackendTypeOllama,
		Status:            types.StatusOffline,
		MaxConcurrentReqs: 4,
	}))

	resp, err := http.Get(baseURL + "/api/v1/backends")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Backends []map[string]interface{} `json:"backends"`
		Total    int                      `json:"total"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))

	// Должны остаться только healthy бэкенды: agent-1, agent-2, llamacpp-healthy
	assert.Equal(t, 3, result.Total)
	for _, b := range result.Backends {
		id := b["id"].(string)
		status := b["status"].(string)
		assert.NotContains(t, []string{
			string(types.StatusUnhealthy),
			string(types.StatusOffline),
			string(types.StatusDraining),
			string(types.StatusOllamaUnavailable),
		}, status, "backend %s has unexpected unhealthy status %s", id, status)
	}
}

// TestBackendsList_IncludeUnhealthyParameter проверяет, что /api/v1/backends
// с includeUnhealthy=true возвращает все бэкенды.
func TestBackendsList_IncludeUnhealthyParameter(t *testing.T) {
	testServer, _, proxy, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	// Считаем исходное количество healthy бэкендов
	resp, err := http.Get(baseURL + "/api/v1/backends")
	require.NoError(t, err)
	defer resp.Body.Close()

	var base struct {
		Total int `json:"total"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&base))

	// Добавляем unhealthy бэкенд
	require.NoError(t, proxy.AddBackend(types.Backend{
		ID:                "llamacpp-unhealthy",
		Name:              "Unhealthy CppWorker",
		Host:              "localhost",
		CppWorkerPort:     18092,
		Type:              types.BackendTypeLlamaCpp,
		Status:            types.StatusUnhealthy,
		MaxConcurrentReqs: 4,
	}))

	// Без параметра — unhealthy скрыт
	resp, err = http.Get(baseURL + "/api/v1/backends")
	require.NoError(t, err)
	defer resp.Body.Close()
	var without struct {
		Total int `json:"total"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&without))
	assert.Equal(t, base.Total, without.Total)

	// С параметром includeUnhealthy=true — unhealthy виден
	resp, err = http.Get(baseURL + "/api/v1/backends?includeUnhealthy=true")
	require.NoError(t, err)
	defer resp.Body.Close()
	var with struct {
		Backends []map[string]interface{} `json:"backends"`
		Total    int                      `json:"total"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&with))
	assert.Equal(t, base.Total+1, with.Total)

	found := false
	for _, b := range with.Backends {
		if b["id"].(string) == "llamacpp-unhealthy" {
			found = true
			break
		}
	}
	assert.True(t, found, "unhealthy backend should be present with includeUnhealthy=true")
}
