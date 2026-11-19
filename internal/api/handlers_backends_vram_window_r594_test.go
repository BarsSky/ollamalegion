package api

// handlers_backends_vram_window_r594_test.go — R594 (2026-11-19).
//
// ОСНОВАНИЕ. R592/R593 сделали «тень» резерва VRAM: воркер отдаёт оба потолка окна
// (`max_vram_n_ctx` — с резервом, `max_vram_n_ctx_unreserved` — как считает C-мост)
// плюс размер резерва; пуллер их разбирает в LlamaCppMetrics. Оставалось довести до
// карточки бэкенда — здесь проверяется именно серверная половина этого пути:
// `/api/v1/backends` обязан отдать блок `vramWindow`.
//
// ЖИВОЙ ФАКТ (27B/A10, diag 2026-11-19): окно 201216 при безрезервном потолке
// 251254 оставило 414 MB свободной VRAM из 22 502, декод промпта 33 812 токенов
// упал с `llama_decode ... code 1` при `compute buffer = 316.75 MiB`. Оператор не
// видел в интерфейсе ни одного из двух чисел.
//
// Проверяется:
//  1. оба потолка и резерв приходят в ответе;
//  2. решения по-прежнему принимаются по `maxVramNCtx` (поле не подменено
//     безрезервным значением);
//  3. без данных блока нет вовсе — нули не выдаются за факт.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/pkg/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// r594BackendsResponse — проекция ответа: нужен только блок vramWindow.
type r594BackendsResponse struct {
	Backends []struct {
		ID         string `json:"id"`
		VramWindow *struct {
			MaxVramNCtx           int `json:"maxVramNCtx"`
			MaxVramNCtxUnreserved int `json:"maxVramNCtxUnreserved"`
			ReserveMB             int `json:"reserveMB"`
		} `json:"vramWindow"`
	} `json:"backends"`
}

func r594FetchBackends(t *testing.T, srv *Server) r594BackendsResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/backends", nil)
	rec := httptest.NewRecorder()
	srv.backendsHandler(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp r594BackendsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "body=%s", rec.Body.String())
	return resp
}

// TestR594_BackendsVramWindow — оба потолка доходят до карточки бэкенда.
func TestR594_BackendsVramWindow(t *testing.T) {
	server, srv := createProxyTestServer(t, "http://127.0.0.1:1")
	defer server.Close()

	mm := srv.proxy.GetMetricsManager()
	require.NotNil(t, mm)
	mm.UpdateLlamaCppMetrics("llama_mock", &types.LlamaCppMetrics{
		TotalVRAMMB:           22502,
		AvailableVRAMMB:       414,
		MaxVRAMNCtx:           201216, // применяемое (с резервом 2048 MB)
		MaxVRAMNCtxUnreserved: 251254, // как считает C-мост
		VRAMReserveMB:         2048,
	})

	resp := r594FetchBackends(t, srv)
	require.Len(t, resp.Backends, 1)
	w := resp.Backends[0].VramWindow
	require.NotNil(t, w, "блок vramWindow обязан быть в ответе: без него карточка бэкенда "+
		"не покажет ни одного из двух потолков окна")
	assert.EqualValues(t, 201216, w.MaxVramNCtx,
		"maxVramNCtx — применяемое число (с резервом); подменять его безрезервным нельзя")
	assert.EqualValues(t, 251254, w.MaxVramNCtxUnreserved)
	assert.EqualValues(t, 2048, w.ReserveMB)
}

// TestR594_BackendsVramWindowUnknown — нет данных → нет блока (нули не факт).
//
// Два случая намеренно разделены: (1) метрик бэкенда нет вовсе; (2) метрики есть
// (VRAM измерена), но потолки окна нулевые — например, воркер старой сборки.
// Второй случай и есть настоящая проверка сторожа «нули не выдаём за факт»:
// без него блок с тремя нулями выглядел бы в карточке как измеренное значение.
func TestR594_BackendsVramWindowUnknown(t *testing.T) {
	server, srv := createProxyTestServer(t, "http://127.0.0.1:1")
	defer server.Close()

	resp := r594FetchBackends(t, srv)
	require.Len(t, resp.Backends, 1)
	assert.Nil(t, resp.Backends[0].VramWindow,
		"без измеренных чисел блок vramWindow не отдаётся: нули выдавались бы за факт")

	// Метрики есть, потолки окна не посчитаны (старая сборка воркера / VRAM не измерена).
	mm := srv.proxy.GetMetricsManager()
	require.NotNil(t, mm)
	mm.UpdateLlamaCppMetrics("llama_mock", &types.LlamaCppMetrics{TotalVRAMMB: 8192})

	resp = r594FetchBackends(t, srv)
	require.Len(t, resp.Backends, 1)
	assert.Nil(t, resp.Backends[0].VramWindow,
		"VRAM измерена, но потолки окна нулевые — блок не отдаётся (иначе карточка "+
			"покажет «потолок окна 0» как факт)")
}
