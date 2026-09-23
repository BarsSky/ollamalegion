package cppworker_lazy_load

// cppworker_lazy_load_async_r6033_test.go — R60.33 (2026-09-10) контракт
// АСИНХРОННОЙ авто-загрузки + R66b (2026-09-22) регрессия env-переключателя.
//
// Контракт (default с R60.33, R67a: только при LB_AUTO_LOAD_WAIT_SEC=0):
//  1. Клиент на cold start НЕ ждёт загрузку: балансер отвечает 503 +
//     Retry-After + телом, объясняющим что загрузка уже запущена.
//  2. Загрузка реально уходит в cppworker (POST /api/models/load) — в фоне.
//  3. Параллельные запросы одной модели дедуплицируются: ровно ОДНА загрузка.
//
// R67a (2026-09-23) изменил DEFAULT: async-балансер больше не отбивает первый
// запрос 503-й, а ждёт загрузку до LB_AUTO_LOAD_WAIT_SEC (default 180). Этот
// файл проверяет legacy-ветку явным LB_AUTO_LOAD_WAIT_SEC=0; новый контракт —
// cppworker_lazy_load_autoload_wait_r67a_test.go.
//
// Отдельно проверяется, что LB_AUTO_LOAD_ASYNC=0 действительно переключает
// балансер в legacy sync-режим — до R66b переменная читалась только в
// unit-тесте самой функции IsAutoLoadAsyncEnabled(), а NewProxy ставил
// ЖЁСТКОЕ true, то есть задокументированный переключатель не работал.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestCppWorker_LazyLoad_Async_Returns503WithRetryAfter — cold start в
// async-режиме: клиент получает быстрый 503 + Retry-After, а не ждёт загрузку.
//
// R67a: «быстрый 503» теперь только при LB_AUTO_LOAD_WAIT_SEC=0. По умолчанию
// балансер ЖДЁТ загрузку (см. cppworker_lazy_load_autoload_wait_r67a_test.go),
// поэтому legacy-контракт проверяется с явно выключенным ожиданием.
func TestCppWorker_LazyLoad_Async_Returns503WithRetryAfter(t *testing.T) {
	t.Setenv("LB_AUTO_LOAD_ASYNC", "1")
	t.Setenv("LB_AUTO_LOAD_WAIT_SEC", "0")

	const loadDelay = 700 * time.Millisecond
	worker := newMockCppWorkerLazy(loadDelay)
	defer worker.Close()

	proxy := newLazyTestProxy(t, worker.server.URL)

	body, _ := json.Marshal(map[string]interface{}{
		"model":    "lazy-model",
		"messages": []map[string]string{{"role": "user", "content": "Привет!"}},
		"stream":   true,
	})
	req := httptest.NewRequest("POST", "/api/chat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	start := time.Now()
	proxy.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	resp := rec.Result()
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("async cold start: ожидался 503, получено %d: %s", resp.StatusCode, string(respBody))
	}
	// Ответ должен быть БЫСТРЫМ — клиент не ждёт загрузку (иначе OpenWebUI/Cline
	// ловят свой 60-120s timeout, ради чего R60.33 и делался).
	if elapsed > 400*time.Millisecond {
		t.Errorf("async cold start должен отвечать сразу, заняло %v", elapsed)
	}
	if ra := resp.Header.Get("Retry-After"); ra == "" {
		t.Errorf("ожидался заголовок Retry-After, получены заголовки: %v", resp.Header)
	}
	bodyStr := string(respBody)
	if !strings.Contains(bodyStr, "load started") {
		t.Errorf("тело должно сообщать, что загрузка запущена, получено: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "retry_after") {
		t.Errorf("тело должно содержать retry_after, получено: %s", bodyStr)
	}

	// Фоновая загрузка должна реально дойти до cppworker.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && worker.loadCalls.Load() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if got := worker.loadCalls.Load(); got != 1 {
		t.Errorf("ожидался ровно 1 POST /api/models/load в фоне, получено %d", got)
	}

	t.Logf("✅ async cold start: 503 за %v, Retry-After=%s, loadCalls=%d",
		elapsed, resp.Header.Get("Retry-After"), worker.loadCalls.Load())
}

// TestCppWorker_LazyLoad_Async_DedupsConcurrentLoads — 3 параллельных запроса
// одной модели в async-режиме → ровно одна фоновая загрузка (mm.tryAcquireOp).
func TestCppWorker_LazyLoad_Async_DedupsConcurrentLoads(t *testing.T) {
	t.Setenv("LB_AUTO_LOAD_ASYNC", "1")
	// R67a: с включённым ожиданием клиенты тоже дедуплицируются, но тест держит
	// дедлайн 5s — оставляем legacy-режим, чтобы он мерял дедупликацию, а не
	// ожидание загрузки.
	t.Setenv("LB_AUTO_LOAD_WAIT_SEC", "0")

	// loadDelay намеренно большой: все три клиента обязаны попасть в окно
	// ОДНОЙ загрузки, иначе тест меряет не дедупликацию, а планировщик Go
	// (под нагрузкой полного прогона `go test ./...` короткая задержка
	// приводила к тому, что третий клиент приходил уже после завершения
	// первой загрузки и легитимно инициировал вторую).
	const loadDelay = 2 * time.Second
	worker := newMockCppWorkerLazy(loadDelay)
	defer worker.Close()

	proxy := newLazyTestProxy(t, worker.server.URL)

	const clients = 3
	var wg sync.WaitGroup
	codes := make([]int, clients)
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			body, _ := json.Marshal(map[string]interface{}{
				"model":    "lazy-model",
				"messages": []map[string]string{{"role": "user", "content": "Привет!"}},
				"stream":   true,
			})
			req := httptest.NewRequest("POST", "/api/chat", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			proxy.ServeHTTP(rec, req)
			codes[idx] = rec.Code
		}(i)
	}
	wg.Wait()

	// Каждый клиент получает ЛИБО 503 (загрузка уже запущена → клиент
	// ретраит по Retry-After), ЛИБО 200 (балансер поставил запрос в
	// warming-очередь и дождался готовности модели). Оба варианта штатные;
	// дефект — любой другой статус.
	for i, c := range codes {
		if c != http.StatusServiceUnavailable && c != http.StatusOK {
			t.Errorf("клиент %d: неожиданный статус %d (ожидался 503 или 200)", i, c)
		}
	}

	// Ждём завершения фоновой загрузки и проверяем дедупликацию.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		worker.mu.Lock()
		state := worker.state["lazy-model"]
		worker.mu.Unlock()
		if state == "loaded" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// ГЛАВНОЕ свойство: параллельные запросы одной модели дают РОВНО одну
	// загрузку (mm.tryAcquireOp держит операцию на всё время load).
	if got := worker.loadCalls.Load(); got != 1 {
		t.Errorf("параллельные запросы должны дать РОВНО 1 загрузку (дедупликация mm.tryAcquireOp), получено %d", got)
	}

	// Инференс допустим только после того, как модель стала loaded.
	if worker.chatCalls.Load() > 0 {
		worker.mu.Lock()
		state := worker.state["lazy-model"]
		worker.mu.Unlock()
		if state != "loaded" {
			t.Errorf("инференс ушёл при state=%q (модель ещё не готова)", state)
		}
	}
	t.Logf("✅ async dedup: %d клиентов → loadCalls=%d, chatCalls=%d, коды=%v",
		clients, worker.loadCalls.Load(), worker.chatCalls.Load(), codes)
}

// TestCppWorker_LazyLoad_EnvSwitchesToSyncMode — R66b: LB_AUTO_LOAD_ASYNC=0
// обязан переключать балансер в sync-режим (регрессия: env не читался, стояло
// жёсткое true).
func TestCppWorker_LazyLoad_EnvSwitchesToSyncMode(t *testing.T) {
	const loadDelay = 300 * time.Millisecond
	worker := newMockCppWorkerLazy(loadDelay)
	defer worker.Close()

	// sync: клиент ДОЖИДАЕТСЯ загрузки и получает 200.
	t.Setenv("LB_AUTO_LOAD_ASYNC", "0")
	syncProxy := newLazyTestProxy(t, worker.server.URL)

	body, _ := json.Marshal(map[string]interface{}{
		"model":    "lazy-model",
		"messages": []map[string]string{{"role": "user", "content": "Привет!"}},
		"stream":   false,
	})
	req := httptest.NewRequest("POST", "/api/chat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	start := time.Now()
	syncProxy.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		respBody, _ := io.ReadAll(rec.Result().Body)
		t.Fatalf("sync-режим (LB_AUTO_LOAD_ASYNC=0): ожидался 200, получено %d: %s", rec.Code, string(respBody))
	}
	if elapsed < loadDelay {
		t.Errorf("sync-режим должен ждать загрузку (%v), занял %v", loadDelay, elapsed)
	}
	if got := worker.loadCalls.Load(); got != 1 {
		t.Errorf("sync-режим: ожидалась 1 загрузка, получено %d", got)
	}
	t.Logf("✅ LB_AUTO_LOAD_ASYNC=0 → sync: 200 за %v, loadCalls=%d", elapsed, worker.loadCalls.Load())
}
