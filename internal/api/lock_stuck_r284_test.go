package api

// lock_stuck_r284_test.go — R284 (2026-10-29): health и ping обязаны быть ЧЕСТНЫМИ.
//
// ЖИВОЙ ДЕФЕКТ (стенд r284): балансер уходил в дедлок на p.mu под нагрузкой
// агентских метрик. /api/v1/health не отвечал вовсе (клиент видел 000 после 40 с),
// а контейнер оставался healthy, потому что Docker healthcheck ходит на
// /api/v1/ping — обработчик, который лок не трогал и отвечал 200 при мёртвом API.
//
// Тест занимает p.mu НАДОЛГО (через MutateBackend с блокирующим колбэком) и
// проверяет, что оба обработчика отвечают 503 с reason=lock_stuck, а после
// освобождения лока снова 200. Так «зелёный статус при мёртвом API» невозможен.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// holdProxyMutex — занимает p.mu на d через MutateBackend (публичный путь,
// поэтому тест не лезет внутрь пакета balancer).
func holdProxyMutex(t *testing.T, s *Server, d time.Duration) {
	t.Helper()
	backendID := "lock-stuck-backend"
	if err := s.proxy.AddBackend(types.Backend{
		ID: backendID, Name: backendID, Host: "127.0.0.1",
		MaxConcurrentReqs: 1, Status: types.StatusHealthy,
		Type: types.BackendTypeLlamaCpp, CppWorkerPort: 18092,
	}); err != nil {
		t.Fatalf("AddBackend: %v", err)
	}
	started := make(chan struct{})
	go func() {
		close(started)
		_ = s.proxy.MutateBackend(backendID, func(b *types.Backend) {
			time.Sleep(d)
		})
	}()
	<-started
	// Дать горутине реально взять p.mu.
	time.Sleep(50 * time.Millisecond)
}

// TestR284_PingReportsLockStuck — liveness НЕ врёт, когда кластерный лок залип.
func TestR284_PingReportsLockStuck(t *testing.T) {
	s := newAgentCapacityServer(t)
	holdProxyMutex(t, s, 3*time.Second)

	rec := httptest.NewRecorder()
	s.pingHandler(rec, httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("ping при залипшем локе: HTTP %d (%s), want 503", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "lock_stuck") {
		t.Fatalf("в ответе нет reason=lock_stuck: %s", rec.Body.String())
	}
}

// TestR284_HealthReportsLockStuck — глубокая проверка называет причину.
func TestR284_HealthReportsLockStuck(t *testing.T) {
	s := newAgentCapacityServer(t)
	holdProxyMutex(t, s, 4*time.Second)

	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.healthHandler(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	}()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("health повис вместо ответа 503 lock_stuck")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("health при залипшем локе: HTTP %d (%s), want 503", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "lock_stuck") || !strings.Contains(body, "lockWaitMs") {
		t.Fatalf("health не объяснил причину: %s", body)
	}
}

// TestR284_HandlersRecoverAfterLockRelease — после освобождения лока оба
// обработчика снова 200 (иначе 503 «залипло» навсегда и Docker перезапускал бы
// контейнер по кругу).
func TestR284_HandlersRecoverAfterLockRelease(t *testing.T) {
	s := newAgentCapacityServer(t)
	holdProxyMutex(t, s, 1500*time.Millisecond)
	time.Sleep(2 * time.Second) // лок отпущен

	rec := httptest.NewRecorder()
	s.pingHandler(rec, httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("ping после освобождения лока: HTTP %d (%s), want 200", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	s.healthHandler(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if rec.Code != http.StatusOK && rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("health после освобождения лока: HTTP %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "lock_stuck") {
		t.Fatalf("health всё ещё сообщает lock_stuck после освобождения лока: %s", rec.Body.String())
	}
}
