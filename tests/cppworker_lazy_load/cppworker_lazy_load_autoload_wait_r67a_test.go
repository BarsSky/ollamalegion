// cppworker_lazy_load_autoload_wait_r67a_test.go — R67a (2026-09-23).
//
// КОНТРАКТ ИЗМЕНИЛСЯ: в R60.33 async-режим отвечал на cold start быстрым
// 503+Retry-After (клиент повторял запрос). Жалоба R67: «клиенту сразу
// прилетает ошибка что будет загружена через 30-180 секунд, и только повторный
// запрос проходит». Теперь балансер ЖДЁТ загрузку ограниченное время
// (LB_AUTO_LOAD_WAIT_SEC, default 180) и обслуживает ПЕРВЫЙ же запрос.
//
// Здесь проверяются оба контракта на интеграционном уровне:
//  1. legacy (LB_AUTO_LOAD_WAIT_SEC=0) — быстрый 503 + Retry-After (как R60.33);
//  2. новый (LB_AUTO_LOAD_WAIT_SEC>0) — запрос дожидается загрузки и получает
//     200 + реальный поток, без повторного запроса.
package cppworker_lazy_load

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
)

// coldStartChat — один запрос /api/chat (stream) к незагруженной модели.
func coldStartChat(t *testing.T, proxy *balancer.Proxy) (*http.Response, string, time.Duration) {
	t.Helper()
	body, _ := json.Marshal(map[string]interface{}{
		"model":    "lazy-model",
		"messages": []map[string]string{{"role": "user", "content": "Привет!"}},
		"stream":   true,
	})
	req := httptest.NewRequest("POST", "/api/chat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-Id", "r67a-user")
	rec := httptest.NewRecorder()

	start := time.Now()
	proxy.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	resp := rec.Result()
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return resp, string(respBody), elapsed
}

// TestCppWorker_LazyLoad_WaitsForLoad_R67a — новый контракт: первый запрос
// дожидается загрузки (mock грузит 700 мс) и получает ответ, а не 503.
func TestCppWorker_LazyLoad_WaitsForLoad_R67a(t *testing.T) {
	// Явно включаем ожидание (в остальных тестах пакета legacy-режим).
	t.Setenv("LB_AUTO_LOAD_ASYNC", "1")
	t.Setenv("LB_AUTO_LOAD_WAIT_SEC", "30")

	const loadDelay = 700 * time.Millisecond
	worker := newMockCppWorkerLazy(loadDelay)
	defer worker.Close()

	proxy := newLazyTestProxy(t, worker.server.URL)

	resp, bodyStr, elapsed := coldStartChat(t, proxy)

	if resp.StatusCode == http.StatusServiceUnavailable {
		t.Fatalf("R67a: запрос не должен получать 503 при включённом ожидании (через %v): %s", elapsed, bodyStr)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ожидался 200 после ожидания загрузки, получено %d: %s", resp.StatusCode, bodyStr)
	}
	// Запрос должен был реально дождаться загрузки, а не отдать пустоту сразу.
	if elapsed < loadDelay/2 {
		t.Errorf("ответ пришёл за %v — похоже, загрузку не ждали (delay=%v)", elapsed, loadDelay)
	}
	if !strings.Contains(bodyStr, "lazy-model") && !strings.Contains(bodyStr, "message") {
		t.Errorf("ожидался поток от загруженной модели, получено: %s", bodyStr)
	}
	// Загрузка должна была уйти в cppworker ровно один раз.
	if got := worker.loadCalls.Load(); got == 0 {
		t.Error("cppworker не получил POST /api/models/load")
	}
}

// TestCppWorker_LazyLoad_LegacyImmediate503_R67a — legacy-ручка
// LB_AUTO_LOAD_WAIT_SEC=0 сохраняет поведение R60.33 (быстрый 503+Retry-After).
func TestCppWorker_LazyLoad_LegacyImmediate503_R67a(t *testing.T) {
	t.Setenv("LB_AUTO_LOAD_ASYNC", "1")
	t.Setenv("LB_AUTO_LOAD_WAIT_SEC", "0")

	worker := newMockCppWorkerLazy(700 * time.Millisecond)
	defer worker.Close()

	proxy := newLazyTestProxy(t, worker.server.URL)

	resp, bodyStr, elapsed := coldStartChat(t, proxy)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("legacy-режим: ожидался 503, получено %d: %s", resp.StatusCode, bodyStr)
	}
	if elapsed > 400*time.Millisecond {
		t.Errorf("legacy-режим должен отвечать сразу, заняло %v", elapsed)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("ожидался заголовок Retry-After")
	}
	if !strings.Contains(bodyStr, "load started") {
		t.Errorf("тело должно сообщать о старте загрузки, получено: %s", bodyStr)
	}
}
