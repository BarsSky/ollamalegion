//go:build llama_stub

// llamacpp_async_load_breaker_r66d_test.go — R66d (2026-09-22).
//
// РЕГРЕСС: асинхронный auto-load (R60.33, LB_AUTO_LOAD_ASYNC=1 — дефолт бандла)
// при провале загрузки только ЛОГИРОВАЛ ошибку и никогда не вызывал
// loadBackoff.recordFailure. Circuit breaker открывался только на sync-пути,
// поэтому при async-загрузке клиент получал бесконечный цикл:
//
//	POST /api/chat → 503 {"error":"model 'X' load started, waiting ... 30-180s",
//	                      "retry_after":90}
//
// даже когда cppworker падал за миллисекунды.
//
// ЖИВОЙ КЕЙС (найден при проверке реального клиента Cline на стенде):
// Cline настроен на модель gemma-4-E4B-it-Q4_K_M, файла на диске нет →
// cppworker: 'gguf_init_from_file: failed to open GGUF file ... No such file
// or directory' (3 мс), а /api/v1/models/operations показывал операцию load в
// статусе "running" уже 1m18s, и каждый следующий запрос снова получал
// «подожди 90 секунд».
//
// После фикса провалы async-загрузки копятся в breaker'е, и после
// maxConsecutiveFailures (3) клиент получает явную ошибку
// "circuit breaker open ... recent load failures, retry later".

package balancer

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// TestEnsureModelLoaded_AsyncFailure_OpensBreaker_R66d — 3 провала async-load
// подряд должны открыть breaker (до фикса он не открывался никогда).
func TestEnsureModelLoaded_AsyncFailure_OpensBreaker_R66d(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models", "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"count":0,"models":[]}`))
		case "/api/models/load", "/api/models/load-with-params":
			// Эмулируем провал загрузки (как отсутствующий GGUF-файл).
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"failed to load model from models/gemma-4-E4B-it-Q4_K_M.gguf: No such file or directory"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	host, port := splitHostPort(t, srv.URL)
	mm := NewModelManager(nil)
	p := &Proxy{
		config: &types.LoadBalancerConfig{
			LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
				"async-broken": {
					// НЕ disabled — breaker должен работать
					ContextLength: 8192,
					NumGPULayers:  -1,
					Disabled:      false,
				},
			},
		},
		backends:     map[string]*BackendState{},
		metricsMgr:   NewMetricsManager(),
		modelManager: mm,
		// Ключевой флаг: async-режим (LB_AUTO_LOAD_ASYNC=1 — дефолт бандла).
		lbAutoLoadAsync: true,
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

	// Кикаем async-загрузку до открытия breaker'а. Каждый вызов возвращает
	// "load started" (не блокирует), а провал горутины должен попасть в breaker.
	deadline := time.Now().Add(15 * time.Second)
	var (
		opened bool
		reason string
	)
	for attempt := 0; attempt < 12 && time.Now().Before(deadline); attempt++ {
		_, _ = lr.ensureModelLoadedOnBackend("test-bk", "async-broken")
		// Даём горутине async-загрузки время завершиться (mock падает сразу,
		// но операция идёт через HTTP + releaseOp).
		time.Sleep(250 * time.Millisecond)
		if skip, r, _ := lr.loadBackoff.shouldSkip("test-bk", "async-broken"); skip {
			opened, reason = true, r
			break
		}
	}

	if !opened {
		t.Fatalf("breaker не открылся после async-провалов загрузки (reason=%q) — "+
			"значит провалы async-load снова не доходят до loadBackoff.recordFailure", reason)
	}

	// И клиент теперь получает явную ошибку вместо бесконечного «подожди 90с».
	_, err := lr.ensureModelLoadedOnBackend("test-bk", "async-broken")
	if err == nil {
		t.Fatal("после открытия breaker'а ожидалась ошибка, получили nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "circuit breaker") {
		t.Errorf("ошибка после открытия breaker'а = %q, ожидалось упоминание circuit breaker", err.Error())
	}
}

// TestEnsureModelLoaded_AsyncInProgress_DoesNotOpenBreaker_R66d — защита от
// ложного открытия: «operation ... already in progress» — это дедупликация
// (предыдущая async-загрузка ещё идёт и может завершиться успешно), а не
// провал, и не должно копить failures.
func TestEnsureModelLoaded_AsyncInProgress_DoesNotOpenBreaker_R66d(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models", "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"count":0,"models":[]}`))
		case "/api/models/load", "/api/models/load-with-params":
			// Медленная «успешная» загрузка: держим соединение, пока тест не
			// отпустит. Пока она идёт, следующие кики получают «already in
			// progress».
			<-release
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"loaded"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	defer close(release)

	host, port := splitHostPort(t, srv.URL)
	mm := NewModelManager(nil)
	p := &Proxy{
		config: &types.LoadBalancerConfig{
			LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
				"async-slow": {ContextLength: 8192, NumGPULayers: -1},
			},
		},
		backends:        map[string]*BackendState{},
		metricsMgr:      NewMetricsManager(),
		modelManager:    mm,
		lbAutoLoadAsync: true,
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

	// Первый кик запускает долгую загрузку, остальные попадают в «уже идёт».
	for i := 0; i < 5; i++ {
		_, _ = lr.ensureModelLoadedOnBackend("test-bk", "async-slow")
		time.Sleep(100 * time.Millisecond)
	}

	if skip, reason, _ := lr.loadBackoff.shouldSkip("test-bk", "async-slow"); skip {
		t.Errorf("breaker не должен открываться на «already in progress» (reason=%q)", reason)
	}
}
