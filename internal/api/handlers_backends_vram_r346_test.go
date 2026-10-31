package api

// handlers_backends_vram_r346_test.go — R346 (2026-10-30).
//
// ОСНОВАНИЕ. docs/plans/2026-10-30-metrics-visibility-decisions.md: поле
// `llamaCpp.gpuInfo` объявлено мёртвым, а фолбэк «нет NVML у агента → берём VRAM
// из поллера» в handlers_backends.go читал ИМЕННО его.
//
// ЧТО БЫЛО НЕ ТАК ДО R346. Поллер cppworker каждый тик писал в метрики
// total_vram_mb/available_vram_mb, но фолбэк читал LlamaCppMetrics.GPUInfo,
// который не заполняет ни один путь балансера. Итог в bundled-режиме (воркер без
// агентской NVML): agent metrics.GPU пуст → фолбэк срабатывал формально, но
// lm.GPUInfo == nil, поэтому gpu.memoryTotal оставался нулём. Монитор показывал
// 0 VRAM при живых данных в поллере — и «почему не поднялась вторая копия»
// приходилось выяснять по сырому JSON /api/v1/cluster.
//
// Проверяется:
//  1. при пустых agent-метриках VRAM берётся из поллера (total/free/used/util);
//  2. available > total не превращается в отрицательный used (защита от мусора);
//  3. при нулевых метриках поллера gpu НЕ подменяется выдуманными числами.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/pkg/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// r346BackendsResponse — минимальная проекция ответа /api/v1/backends: нужен
// только gpu одного бэкенда, остальные поля не проверяются.
type r346BackendsResponse struct {
	Backends []struct {
		ID  string `json:"id"`
		GPU struct {
			UsagePercent float64 `json:"usagePercent"`
			MemoryTotal  uint64  `json:"memoryTotal"`
			MemoryUsed   uint64  `json:"memoryUsed"`
			MemoryFree   uint64  `json:"memoryFree"`
		} `json:"gpu"`
	} `json:"backends"`
}

func r346FetchBackends(t *testing.T, srv *Server) r346BackendsResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/backends", nil)
	rec := httptest.NewRecorder()
	srv.backendsHandler(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp r346BackendsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "body=%s", rec.Body.String())
	return resp
}

// TestR346_BackendsVRAMFromPoller — без agent-метрик VRAM приходит из поллера.
func TestR346_BackendsVRAMFromPoller(t *testing.T) {
	server, srv := createProxyTestServer(t, "http://127.0.0.1:1")
	defer server.Close()

	mm := srv.proxy.GetMetricsManager()
	require.NotNil(t, mm)
	// 8 ГБ карта, свободно 5 ГБ: ожидаем used=3072, util≈37.5%.
	mm.UpdateLlamaCppMetrics("llama_mock", &types.LlamaCppMetrics{
		TotalVRAMMB:     8192,
		AvailableVRAMMB: 5120,
	})

	resp := r346FetchBackends(t, srv)
	require.Len(t, resp.Backends, 1)

	b := resp.Backends[0]
	assert.Equal(t, "llama_mock", b.ID)
	assert.EqualValues(t, 8192, b.GPU.MemoryTotal,
		"total VRAM обязан приходить из поллера: agent-NVML у bundled-воркера нет")
	assert.EqualValues(t, 5120, b.GPU.MemoryFree)
	assert.EqualValues(t, 3072, b.GPU.MemoryUsed)
	assert.InDelta(t, 37.5, b.GPU.UsagePercent, 0.01)
}

// TestR346_BackendsVRAMClampsFreeAboveTotal — мусор от воркера не даёт
// отрицательный memoryUsed и util > 100%.
func TestR346_BackendsVRAMClampsFreeAboveTotal(t *testing.T) {
	server, srv := createProxyTestServer(t, "http://127.0.0.1:1")
	defer server.Close()

	mm := srv.proxy.GetMetricsManager()
	require.NotNil(t, mm)
	// Воркер посчитал свободной больше, чем всего (RPC-узлы/гонка снимков).
	mm.UpdateLlamaCppMetrics("llama_mock", &types.LlamaCppMetrics{
		TotalVRAMMB:     8192,
		AvailableVRAMMB: 9000,
	})

	resp := r346FetchBackends(t, srv)
	require.Len(t, resp.Backends, 1)

	b := resp.Backends[0]
	assert.EqualValues(t, 8192, b.GPU.MemoryTotal)
	assert.EqualValues(t, 8192, b.GPU.MemoryFree, "free не может превышать total")
	assert.EqualValues(t, 0, b.GPU.MemoryUsed, "used = total - free не должно уходить в uint64-переполнение")
	assert.InDelta(t, 0.0, b.GPU.UsagePercent, 0.01)
}

// TestR346_BackendsVRAMUnknownStaysZero — поллер молчит → нулей не выдумываем.
func TestR346_BackendsVRAMUnknownStaysZero(t *testing.T) {
	server, srv := createProxyTestServer(t, "http://127.0.0.1:1")
	defer server.Close()

	resp := r346FetchBackends(t, srv)
	require.Len(t, resp.Backends, 1)

	b := resp.Backends[0]
	assert.EqualValues(t, 0, b.GPU.MemoryTotal,
		"без данных VRAM остаётся нулём: подставлять «примерную» карту нельзя")
	assert.EqualValues(t, 0, b.GPU.MemoryFree)
}
