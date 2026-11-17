// reload_busy_guard_r493_test.go — R493 (2026-11-17): перезагрузка не
// начинается, пока на бэкенде идёт чужой ответ.
//
// ЖИВОЙ ФАКТ (архив второго стенда): 06:24:53 запуск reload на n_ctx=194296,
// 06:25:02 узел уже грузился с `partial offload 62/64`, 06:25:43 — следующая
// перезагрузка; ходы Cline при этом шли. Проверки занятости в решателях стоят
// ДО решения, а между решением и фактическим unload+load проходит время — новый
// ход успевает начаться, и его рвут.
//
// Проверяется: (1) хук координатора вызывается и его отказ означает NoOp;
// (2) при отказе до мок-воркера НЕ доходит ни одного reload-запроса;
// (3) при разрешении поведение прежнее (async reload запускается) — то есть
// правка не «выключила перезагрузку вообще».
package balancer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func newBusyGuardSetupR493(t *testing.T) (*NCtxReloadCoordinator, *RequestMeta, *NCtxBackendState, *httptest.Server, *atomic.Int32) {
	t.Helper()
	var reloads atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/reload", "/api/models/load":
			reloads.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true}`))
		default:
			// Как в preflight_nctx_async_test.go: всё, кроме reload/load, — 404,
			// иначе гейт «рост бесполезен» срабатывает на другой заглушке и
			// положительный контроль проверяет не то.
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(backend.Close)

	coord := NewNCtxReloadCoordinator(DefaultNCtxReloadConfig())
	coord.SetConfig(NCtxReloadConfig{
		AutoReloadNCtx:              true,
		AutoReloadVRAMSafetyFactor:  0.85,
		PreflightEnabled:            true,
		PreflightAsyncReload:        true,
		PreflightAsyncRetryAfterSec: 5,
	})
	meta := &RequestMeta{
		RequestedNCtxOverride: 65536,
		EstimatedPromptTokens: 50000,
		RequestedNPredict:     1000,
		ModelName:             "busy-guard-model",
	}
	state := &NCtxBackendState{
		BackendID:       "busy-backend",
		CurrentNCtx:     8192,
		MaxVRAMNCtx:     65536,
		ModelMaxContext: 131072,
	}
	return coord, meta, state, backend, &reloads
}

// TestR493_ReloadGuardBlocksWhileStreamActive — отказ хука = NoOp и ни одного
// reload-запроса к воркеру.
func TestR493_ReloadGuardBlocksWhileStreamActive(t *testing.T) {
	coord, meta, state, backend, reloads := newBusyGuardSetupR493(t)
	coord.SetBeforeReloadGuard(func(backendID, model string) (bool, string) {
		return false, "на бэкенде идёт 1 стрим(ов)"
	})

	loader := &DefaultNCtxReloadHTTPClient{
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
		APIToken:   "test",
		HeaderName: "X-API-Token",
	}
	res, err := coord.RunPreflight(context.Background(), "busy-backend", backend.URL, meta, state, loader)
	if err != nil {
		t.Fatalf("RunPreflight error: %v", err)
	}
	if res.Decision != PreflightNoOp {
		t.Fatalf("решение = %v, ожидался NoOp: перезагрузка при живом стриме недопустима", res.Decision)
	}
	// Дать гипотетической горутине шанс сходить в воркер — и убедиться, что нет.
	time.Sleep(150 * time.Millisecond)
	if n := reloads.Load(); n != 0 {
		t.Fatalf("reload-запросов к воркеру = %d, ожидалось 0: отказ хука обязан останавливать перезагрузку ДО сети", n)
	}
}

// TestR493_ReloadGuardAllowsWhenIdle — положительный контроль: без занятости
// перезагрузка запускается как раньше (правка не выключает рост окна).
func TestR493_ReloadGuardAllowsWhenIdle(t *testing.T) {
	coord, meta, state, backend, reloads := newBusyGuardSetupR493(t)
	coord.SetBeforeReloadGuard(func(backendID, model string) (bool, string) {
		return true, ""
	})

	loader := &DefaultNCtxReloadHTTPClient{
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
		APIToken:   "test",
		HeaderName: "X-API-Token",
	}
	res, err := coord.RunPreflight(context.Background(), "busy-backend", backend.URL, meta, state, loader)
	if err != nil {
		t.Fatalf("RunPreflight error: %v", err)
	}
	if res.Decision != PreflightAsyncReload {
		t.Fatalf("решение = %v, ожидался PreflightAsyncReload (хук разрешил)", res.Decision)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if reloads.Load() > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("reload-запрос к воркеру не пришёл за 2 с при разрешающем хуке")
}
