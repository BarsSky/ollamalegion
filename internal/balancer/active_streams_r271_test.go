package balancer

// active_streams_r271_test.go — R271 (2026-10-29): чужой запрос не рвёт чужой
// стрим.
//
// ЖИВОЙ ДЕФЕКТ (второй стенд, Cline + DSH параллельно). Первый клиент стримил
// ответ, второй запросил другое окно n_ctx → preflight решил перезагрузить
// модель → cppworker для смены n_ctx делает unload+load → у первого поток
// закрылся без терминального маркера, и клиент увидел «stream ended by upstream
// before completion», хотя воркер был жив.
//
// Тесты держат три инварианта:
//   - стримы считаются по БЭКЕНДУ и по МОДЕЛИ (по глобальному WaitGroup этого
//     не понять, он для graceful shutdown);
//   - разрушительная операция (перезагрузка) не начинается, пока на бэкенде
//     есть активные стримы, и НЕ начинается вовсе, если ждать не разрешено;
//   - обрыв, вызванный нашей же перезагрузкой, называется своим именем, а не
//     «upstream оборвался».

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// TestR271_StreamCountersPerBackendAndModel — учёт по бэкенду и по модели,
// освобождение идемпотентно, nil-прокси безопасен.
func TestR271_StreamCountersPerBackendAndModel(t *testing.T) {
	p := &Proxy{}

	releaseA1 := beginClientStreamR271(p, "b1", "m1")
	releaseA2 := beginClientStreamR271(p, "b1", "m1")
	releaseB := beginClientStreamR271(p, "b1", "m2")
	releaseOther := beginClientStreamR271(p, "b2", "m1")

	if got := p.activeStreamsOnBackend("b1"); got != 3 {
		t.Fatalf("activeStreamsOnBackend(b1) = %d, want 3", got)
	}
	if got := p.activeStreamsOnModel("b1", "m1"); got != 2 {
		t.Fatalf("activeStreamsOnModel(b1,m1) = %d, want 2", got)
	}
	if got := p.activeStreamsOnBackend("b2"); got != 1 {
		t.Fatalf("activeStreamsOnBackend(b2) = %d, want 1", got)
	}

	releaseA1()
	releaseA1() // повторный вызов не должен ломать счётчик
	if got := p.activeStreamsOnModel("b1", "m1"); got != 1 {
		t.Fatalf("после повторного release: activeStreamsOnModel = %d, want 1", got)
	}
	releaseA2()
	if got := p.activeStreamsOnModel("b1", "m1"); got != 0 {
		t.Fatalf("после release всех m1: = %d, want 0", got)
	}
	releaseB()
	releaseOther()
	if got := p.activeStreamsOnBackend("b1"); got != 0 {
		t.Fatalf("activeStreamsOnBackend(b1) = %d, want 0", got)
	}

	// nil-прокси и пустой backendID не паникуют.
	var nilProxy *Proxy
	beginClientStreamR271(nilProxy, "b1", "m1")()
	beginClientStreamR271(p, "", "m1")()
	if got := nilProxy.activeStreamsOnBackend("b1"); got != 0 {
		t.Fatalf("nil proxy: = %d, want 0", got)
	}
}

// TestR271_WaitStreamsIdle — ждём завершения чужого стрима, а не выгружаем
// модель под ним.
func TestR271_WaitStreamsIdle(t *testing.T) {
	p := &Proxy{}

	// Стримов нет — разрешение сразу, без ожидания.
	start := time.Now()
	if !p.waitStreamsIdleR271(context.Background(), "b1", "m1", time.Second) {
		t.Fatal("без активных стримов ожидание должно возвращать true сразу")
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("без стримов ждать нечего, а ждали %v", elapsed)
	}

	release := beginClientStreamR271(p, "b1", "m1")

	// Активный стрим и «ждать нельзя» → отказ немедленно.
	if p.waitStreamsIdleR271(context.Background(), "b1", "m1", 0) {
		t.Fatal("при активном стриме и max=0 ожидание должно вернуть false")
	}

	// Стрим завершается в процессе ожидания → дождались.
	go func() {
		time.Sleep(120 * time.Millisecond)
		release()
	}()
	if !p.waitStreamsIdleR271(context.Background(), "b1", "m1", 3*time.Second) {
		t.Fatal("после завершения стрима ожидание должно вернуть true")
	}

	// Стрим не завершается → по таймауту false (не выгружаем модель).
	release2 := beginClientStreamR271(p, "b1", "m1")
	defer release2()
	if p.waitStreamsIdleR271(context.Background(), "b1", "m1", 300*time.Millisecond) {
		t.Fatal("живой стрим не должен давать разрешение на выгрузку")
	}
}

// TestR271_ReloadDeferredWhileStreamActive — перезагрузка НЕ трогает воркер, пока
// идёт чужой стрим; после завершения — трогает.
func TestR271_ReloadDeferredWhileStreamActive(t *testing.T) {
	// Ждать не разрешаем (0): при активном стриме операция обязана отмениться
	// сразу, без HTTP-запроса к воркеру.
	t.Setenv("LB_RELOAD_DEFER_MAX_SEC", "0")
	if got := reloadDeferMaxWaitR271(); got != 0 {
		t.Fatalf("reloadDeferMaxWaitR271() = %v, want 0", got)
	}

	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("split host/port: %v", err)
	}
	port, _ := strconv.Atoi(portStr)

	backend := &types.Backend{
		ID: "b1", Host: host, CppWorkerPort: port,
		Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy, MaxConcurrentReqs: 1,
	}
	p := &Proxy{
		backends: map[string]*BackendState{"b1": {Backend: backend}},
		config:   &types.LoadBalancerConfig{},
		client:   &http.Client{Timeout: 3 * time.Second},
	}

	release := beginClientStreamR271(p, "b1", "qwen3.8")

	p.executeAsyncReload("b1", "qwen3.8", 32768, backend)
	if got := atomic.LoadInt64(&hits); got != 0 {
		t.Fatalf("перезагрузка ушла в воркер под активным стримом: hits=%d", got)
	}

	// Стрим завершился — теперь перезагрузка разрешена.
	release()
	p.executeAsyncReload("b1", "qwen3.8", 32768, backend)
	if got := atomic.LoadInt64(&hits); got == 0 {
		t.Fatal("после завершения стрима перезагрузка должна пройти")
	}

	// И она помечена как разрушительная — чтобы обрыв чужого стрима объяснялся
	// ею, а не «upstream оборвался».
	if _, ok := p.recentDestructiveOpR271("b1", "qwen3.8", time.Minute); !ok {
		t.Fatal("перезагрузка не отмечена как разрушительная операция")
	}
}

// TestR271_BalancerReloadAbortIsNotUpstream — причина обрыва называется своим
// именем, и клиенту не сообщается про «upstream before completion».
func TestR271_BalancerReloadAbortIsNotUpstream(t *testing.T) {
	if streamAbortBlamesUpstream(streamAbortBalancerReload) {
		t.Fatal("обрыв из-за нашей перезагрузки не должен сваливаться на upstream")
	}
	msg := streamAbortClientMessage(streamAbortBalancerReload)
	if !strings.Contains(msg, "reload") {
		t.Fatalf("в тексте должна быть причина (reload), получили %q", msg)
	}
	if strings.Contains(msg, "upstream before completion") {
		t.Fatalf("наш обрыв не должен выглядеть как обрыв upstream: %q", msg)
	}

	// Отметка действует ограниченное время: старая перезагрузка не объясняет
	// сегодняшний обрыв.
	p := &Proxy{}
	p.noteDestructiveOpR271("b1", "m1", destructiveReloadR271)
	if _, ok := p.recentDestructiveOpR271("b1", "m1", time.Minute); !ok {
		t.Fatal("свежая отметка должна находиться")
	}
	if _, ok := p.recentDestructiveOpR271("b1", "m1", -time.Second); ok {
		t.Fatal("старая отметка не должна применяться к новому обрыву")
	}
	if _, ok := p.recentDestructiveOpR271("b2", "m1", time.Minute); ok {
		t.Fatal("отметка не должна протекать на другой бэкенд")
	}
}
