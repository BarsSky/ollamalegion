//go:build llama_stub

// autoload_wait_r67a_test.go — R67a (2026-09-23).
//
// ЖАЛОБА: «если модель не загружена, клиенту сразу прилетает ошибка что будет
// она загружена через 30-180 секунд, и только повторный запрос проходит».
//
// Механизм (до фикса): ensureModelLoadedOnBackend в async-режиме (default)
// кикал загрузку в горутине и СРАЗУ возвращал ошибку
// «model X load started, waiting for cppworker to finish (~30-180s)» → хендлер
// отдавал 503+Retry-After. Первый запрос клиента не обслуживался никогда.
//
// После фикса: ждём готовности ограниченное время (LB_AUTO_LOAD_WAIT_SEC,
// default 180s), опрашивая cppworker напрямую (/api/models/load/progress и
// /api/models). Успели — обслуживаем ЭТОТ ЖЕ запрос; не успели — прежний 503.
package balancer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// TestAutoLoadWaitTimeout_EnvParsing_R67a — LB_AUTO_LOAD_WAIT_SEC.
func TestAutoLoadWaitTimeout_EnvParsing_R67a(t *testing.T) {
	t.Setenv("LB_AUTO_LOAD_WAIT_SEC", "")
	if got := AutoLoadWaitTimeout(); got != autoLoadWaitDefaultSec*time.Second {
		t.Errorf("default = %s, want %ds", got, autoLoadWaitDefaultSec)
	}
	t.Setenv("LB_AUTO_LOAD_WAIT_SEC", "42")
	if got := AutoLoadWaitTimeout(); got != 42*time.Second {
		t.Errorf("42 → %s", got)
	}
	t.Setenv("LB_AUTO_LOAD_WAIT_SEC", "0")
	if got := AutoLoadWaitTimeout(); got != 0 {
		t.Errorf("0 (прежнее поведение) → %s", got)
	}
	t.Setenv("LB_AUTO_LOAD_WAIT_SEC", "-1")
	if got := AutoLoadWaitTimeout(); got != 30*time.Minute {
		t.Errorf("отрицательное («ждать сколько нужно») → %s", got)
	}
	t.Setenv("LB_AUTO_LOAD_WAIT_SEC", "abc")
	if got := AutoLoadWaitTimeout(); got != autoLoadWaitDefaultSec*time.Second {
		t.Errorf("мусор → дефолт, got %s", got)
	}
}

// TestRequestSessionKey_R67a — различаем разных пользователей OpenWebUI.
func TestRequestSessionKey_R67a(t *testing.T) {
	mk := func(h map[string]string, body string) string {
		r := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(body))
		for k, v := range h {
			r.Header.Set(k, v)
		}
		return RequestSessionKey(r, []byte(body))
	}

	if got := mk(map[string]string{"X-User-Id": "u-42"}, ""); got != "X-User-Id:u-42" {
		t.Errorf("X-User-Id → %q", got)
	}
	if got := mk(map[string]string{"X-OpenWebUI-User-Id": "u-7"}, ""); got != "X-OpenWebUI-User-Id:u-7" {
		t.Errorf("X-OpenWebUI-User-Id → %q", got)
	}
	if got := mk(map[string]string{"X-Session-Id": "s-1"}, ""); got != "X-Session-Id:s-1" {
		t.Errorf("X-Session-Id → %q", got)
	}
	if got := mk(nil, `{"user":"body-user"}`); got != "user:body-user" {
		t.Errorf("body user → %q", got)
	}
	if got := mk(nil, `{"session_id":"sess-9"}`); got != "session:sess-9" {
		t.Errorf("body session_id → %q", got)
	}
	if got := mk(nil, `{"model":"m"}`); got != "" {
		t.Errorf("без идентификаторов → %q (аноним)", got)
	}
}

// newAutoloadWaitHarness — Proxy + LlamaCppRouter с одним бэкендом.
func newAutoloadWaitHarness(t *testing.T, handler http.HandlerFunc, wait time.Duration) (*LlamaCppRouter, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	host, port := splitHostPort(t, srv.URL)

	mm := NewModelManager(nil)
	p := &Proxy{
		config:          &types.LoadBalancerConfig{},
		backends:        map[string]*BackendState{},
		metricsMgr:      NewMetricsManager(),
		modelManager:    mm,
		lbAutoLoadAsync: true,
		lbAutoLoadWait:  wait,
	}
	mm.proxy = p
	p.backends["test-bk"] = &BackendState{
		Backend: &types.Backend{
			ID:            "test-bk",
			Host:          host,
			Status:        types.StatusHealthy,
			Type:          types.BackendTypeLlamaCpp,
			CppWorkerPort: port,
		},
	}
	lr := NewLlamaCppRouter(p)
	return lr, srv
}

// TestWaitForModelLoad_LoadingThenLoaded_R67a — ожидание переживает state=loading.
func TestWaitForModelLoad_LoadingThenLoaded_R67a(t *testing.T) {
	var polls int32
	lr, _ := newAutoloadWaitHarness(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/load/progress":
			n := atomic.AddInt32(&polls, 1)
			w.Header().Set("Content-Type", "application/json")
			if n < 2 {
				_, _ = w.Write([]byte(`{"state":"loading","name":"m"}`))
				return
			}
			_, _ = w.Write([]byte(`{"state":"loaded","name":"m"}`))
		case "/api/models":
			_, _ = w.Write([]byte(`{"count":0,"models":[]}`))
		default:
			http.NotFound(w, r)
		}
	}, 10*time.Second)

	start := time.Now()
	if err := lr.waitForModelLoad(context.Background(), "test-bk", "m", 10*time.Second); err != nil {
		t.Fatalf("waitForModelLoad: %v", err)
	}
	if elapsed := time.Since(start); elapsed < autoLoadPollInterval {
		t.Errorf("вернулось слишком быстро (%s) — опроса loading→loaded не было", elapsed)
	}
	if atomic.LoadInt32(&polls) < 2 {
		t.Errorf("опросов прогресса = %d, want >= 2", polls)
	}
}

// TestWaitForModelLoad_FailedReportsCppworkerError_R67a — реальная ошибка
// загрузки должна дойти до клиента, а не превратиться в общий 503.
func TestWaitForModelLoad_FailedReportsCppworkerError_R67a(t *testing.T) {
	lr, _ := newAutoloadWaitHarness(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/load/progress":
			_, _ = w.Write([]byte(`{"state":"failed","error":"failed to open GGUF file: No such file or directory"}`))
		case "/api/models":
			_, _ = w.Write([]byte(`{"count":0,"models":[]}`))
		default:
			http.NotFound(w, r)
		}
	}, 5*time.Second)

	err := lr.waitForModelLoad(context.Background(), "test-bk", "m", 5*time.Second)
	if err == nil {
		t.Fatal("ожидалась ошибка загрузки")
	}
	if !strings.Contains(err.Error(), "No such file") {
		t.Errorf("ошибка должна содержать причину от cppworker, got: %v", err)
	}
}

// TestWaitForModelLoad_Timeout_R67a — не успели: возвращаем ошибку таймаута
// (хендлер отдаст прежний 503+Retry-After, модель продолжит грузиться).
func TestWaitForModelLoad_Timeout_R67a(t *testing.T) {
	lr, _ := newAutoloadWaitHarness(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/load/progress":
			_, _ = w.Write([]byte(`{"state":"loading"}`))
		case "/api/models":
			_, _ = w.Write([]byte(`{"count":0,"models":[]}`))
		default:
			http.NotFound(w, r)
		}
	}, 50*time.Millisecond)

	err := lr.waitForModelLoad(context.Background(), "test-bk", "m", 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("ожидался таймаут, got: %v", err)
	}
}

// TestWaitForModelLoad_ModelPresentWithoutProgress_R67a — cppworker снял запись
// прогресса, но модель уже в /api/models (loaded) → считаем готовой.
func TestWaitForModelLoad_ModelPresentWithoutProgress_R67a(t *testing.T) {
	lr, _ := newAutoloadWaitHarness(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/load/progress":
			http.NotFound(w, r) // записи нет
		case "/api/models":
			_, _ = w.Write([]byte(`{"count":1,"models":[{"name":"m","state":"loaded"}]}`))
		default:
			http.NotFound(w, r)
		}
	}, 5*time.Second)

	if err := lr.waitForModelLoad(context.Background(), "test-bk", "m", 5*time.Second); err != nil {
		t.Fatalf("модель в /api/models со state=loaded должна считаться готовой: %v", err)
	}
}

// TestEnsureModelLoaded_AsyncWaitsAndServes_R67a — главный регресс: async-режим
// с включённым ожиданием возвращает (true, nil), а не sentinel-ошибку, поэтому
// ПЕРВЫЙ же запрос клиента обслуживается.
func TestEnsureModelLoaded_AsyncWaitsAndServes_R67a(t *testing.T) {
	var progressPolls int32
	lr, _ := newAutoloadWaitHarness(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/load", "/api/models/load-with-params":
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"status":"loading","name":"m"}`))
		case "/api/models/load/progress":
			n := atomic.AddInt32(&progressPolls, 1)
			if n < 2 {
				_, _ = w.Write([]byte(`{"state":"loading"}`))
				return
			}
			_, _ = w.Write([]byte(`{"state":"loaded"}`))
		case "/api/models", "/v1/models":
			_, _ = w.Write([]byte(`{"count":0,"models":[]}`))
		default:
			http.NotFound(w, r)
		}
	}, 15*time.Second)

	ok, err := lr.ensureModelLoadedOnBackend("test-bk", "m", warmupOptions{Ctx: context.Background(), SessionKey: "X-User-Id:u-1"})
	if err != nil {
		t.Fatalf("при включённом ожидании модель должна дождаться и вернуть nil, got: %v", err)
	}
	if !ok {
		t.Fatal("ensureModelLoadedOnBackend вернул ok=false, ожидалось true")
	}
}

// TestEnsureModelLoaded_AsyncWaitZeroKeepsLegacy_R67a — LB_AUTO_LOAD_WAIT_SEC=0
// сохраняет прежнее поведение (сразу 503) для тех, кому нужен мгновенный ответ.
func TestEnsureModelLoaded_AsyncWaitZeroKeepsLegacy_R67a(t *testing.T) {
	lr, _ := newAutoloadWaitHarness(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/load", "/api/models/load-with-params":
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"status":"loading"}`))
		case "/api/models", "/v1/models":
			_, _ = w.Write([]byte(`{"count":0,"models":[]}`))
		case "/api/models/load/progress":
			_, _ = w.Write([]byte(`{"state":"loading"}`))
		default:
			http.NotFound(w, r)
		}
	}, 0) // ожидание выключено

	ok, err := lr.ensureModelLoadedOnBackend("test-bk", "m")
	if ok {
		t.Fatal("ok=true при выключенном ожидании — модель ещё не загружена")
	}
	if err == nil || !strings.Contains(err.Error(), "load started") {
		t.Fatalf("ожидалась sentinel-ошибка «load started», got: %v", err)
	}
}
