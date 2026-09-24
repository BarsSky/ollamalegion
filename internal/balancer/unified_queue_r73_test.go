package balancer

// R73 (2026-09-24): единая очередь — Ollama-путь ждёт слот в admission-очереди.
//
// До R73 у Ollama-пути (Proxy.ServeHTTP) была своя legacy-очередь
// (QueueManager: канал + пул worker'ов), а у llama.cpp-пути — admission-очередь.
// Разные лимиты, разная справедливость, разная наблюдаемость. Теперь оба пути
// используют одну очередь: waitForInferenceBackend.

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// newUnifiedQueueProxyR73 — прокси с одним ollama-бэкендом и заданной
// вместимостью (n слотов). Admission-очередь создаётся NewProxy; в тестах
// пакета LB_ADMISSION_WAIT_SEC=0 (см. waits_testmain_test.go), поэтому
// ожидание включается явно через setAdmissionWait.
func newUnifiedQueueProxyR73(t *testing.T, maxConcurrent int) *Proxy {
	t.Helper()
	port := freeTCPPortR73(t)
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host: "localhost", Port: 8080, APIPort: 8081,
			StatePath: filepath.Join(t.TempDir(), "state.json"),
		},
		Backends: []types.Backend{{
			ID:                "ollama-1",
			Name:              "stub ollama",
			Host:              "127.0.0.1",
			OllamaPort:        port,
			Type:              types.BackendTypeOllama,
			Status:            types.StatusHealthy,
			MaxConcurrentReqs: maxConcurrent,
			Weight:            1,
		}},
		Balancing: types.BalancingSettings{
			Algorithm:           types.AlgorithmResourceAware,
			QueueMaxSize:        100,
			QueueWorkers:        1,
			QueueTimeout:        30,
			HealthCheckInterval: 10,
			MetricsInterval:     5,
		},
	}
	return newProxyWithCleanup(t, cfg)
}

func freeTCPPortR73(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestTryAcquireAnyBackendSlot_R73(t *testing.T) {
	proxy := newUnifiedQueueProxyR73(t, 1)

	if id := proxy.tryAcquireAnyBackendSlot("m1", types.BackendTypeOllama); id != "ollama-1" {
		t.Fatalf("первый захват слота: got %q, ожидался ollama-1", id)
	}
	if id := proxy.tryAcquireAnyBackendSlot("m1", types.BackendTypeOllama); id != "" {
		t.Errorf("слот занят (maxConcurrent=1), но получен %q", id)
	}
	proxy.releaseSlot("ollama-1")
	if id := proxy.tryAcquireAnyBackendSlot("m1", types.BackendTypeOllama); id != "ollama-1" {
		t.Errorf("после release слот должен снова захватываться, got %q", id)
	}
	proxy.releaseSlot("ollama-1")
}

func TestWaitForInferenceBackend_FastPath_R73(t *testing.T) {
	proxy := newUnifiedQueueProxyR73(t, 1)

	backend, release, waited, position, err := proxy.waitForInferenceBackend(
		context.Background(), "m1", types.BackendTypeOllama, "X-User-Id:u1")
	if err != nil {
		t.Fatalf("ожидался свободный слот, получена ошибка: %v", err)
	}
	if backend != "ollama-1" {
		t.Errorf("backend = %q, ожидался ollama-1", backend)
	}
	if waited != 0 || position != 0 {
		t.Errorf("быстрый путь не должен ждать: waited=%v position=%d", waited, position)
	}
	if release == nil {
		t.Fatal("release не должен быть nil")
	}
	// Слот занят, пока lease не освобождён.
	if id := proxy.tryAcquireAnyBackendSlot("m1", types.BackendTypeOllama); id != "" {
		t.Errorf("слот должен быть занят lease'ом, но получен %q", id)
	}
	release()
	if id := proxy.tryAcquireAnyBackendSlot("m1", types.BackendTypeOllama); id != "ollama-1" {
		t.Errorf("после release() слот должен освободиться, got %q", id)
	}
	proxy.releaseSlot("ollama-1")
}

func TestWaitForInferenceBackend_WaitsForFreeSlot_R73(t *testing.T) {
	proxy := newUnifiedQueueProxyR73(t, 1)
	proxy.setAdmissionWait(3 * time.Second)

	// Занимаем единственный слот.
	if !proxy.tryAcquireSlot("ollama-1") {
		t.Fatal("не удалось занять слот")
	}

	type result struct {
		err      error
		backend  string
		waited   time.Duration
		position int
	}
	done := make(chan result, 1)
	go func() {
		backend, release, waited, position, err := proxy.waitForInferenceBackend(
			context.Background(), "m1", types.BackendTypeOllama, "X-User-Id:u2")
		if release != nil {
			release()
		}
		done <- result{err: err, backend: backend, waited: waited, position: position}
	}()

	// Ожидающий должен встать в очередь.
	time.Sleep(150 * time.Millisecond)
	if waiting := proxy.admission.waitingCount(); waiting != 1 {
		t.Errorf("в очереди ожидающих %d, ожидался 1", waiting)
	}
	proxy.releaseSlot("ollama-1")

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("ожидался слот после освобождения, ошибка: %v", r.err)
		}
		if r.backend != "ollama-1" {
			t.Errorf("backend = %q, ожидался ollama-1", r.backend)
		}
		if r.waited <= 0 {
			t.Errorf("waited = %v, ожидалось > 0", r.waited)
		}
		if r.position == 0 {
			t.Error("position должен быть >= 1 для ожидавшего запроса")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("waitForInferenceBackend не дождался слота")
	}
}

func TestWaitForInferenceBackend_Timeout_R73(t *testing.T) {
	proxy := newUnifiedQueueProxyR73(t, 1)
	proxy.setAdmissionWait(150 * time.Millisecond)

	if !proxy.tryAcquireSlot("ollama-1") {
		t.Fatal("не удалось занять слот")
	}
	defer proxy.releaseSlot("ollama-1")

	start := time.Now()
	_, release, waited, position, err := proxy.waitForInferenceBackend(
		context.Background(), "m1", types.BackendTypeOllama, "X-User-Id:u3")
	if release != nil {
		t.Error("при таймауте release должен быть nil")
	}
	if err != errAdmissionTimeout {
		t.Fatalf("ожидался errAdmissionTimeout, получено %v", err)
	}
	if waited < 150*time.Millisecond {
		t.Errorf("waited = %v, ожидалось >= предела ожидания", waited)
	}
	if position == 0 {
		t.Error("position должен быть >= 1 (запрос стоял в очереди)")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("таймаут сработал слишком поздно: %v", elapsed)
	}
	// После таймаута ожидающих быть не должно (remove по defer).
	if waiting := proxy.admission.waitingCount(); waiting != 0 {
		t.Errorf("после таймаута в очереди осталось %d ожидающих", waiting)
	}
}

func TestWaitForInferenceBackend_Disabled_R73(t *testing.T) {
	proxy := newUnifiedQueueProxyR73(t, 1)
	proxy.setAdmissionWait(0) // как LB_ADMISSION_WAIT_SEC=0

	if !proxy.tryAcquireSlot("ollama-1") {
		t.Fatal("не удалось занять слот")
	}
	defer proxy.releaseSlot("ollama-1")

	_, _, _, _, err := proxy.waitForInferenceBackend(
		context.Background(), "m1", types.BackendTypeOllama, "X-User-Id:u4")
	if err != errAdmissionDisabled {
		t.Fatalf("ожидался errAdmissionDisabled, получено %v", err)
	}
}

func TestWaitForInferenceBackend_Overloaded_R73(t *testing.T) {
	proxy := newUnifiedQueueProxyR73(t, 1)
	proxy.setAdmissionWait(2 * time.Second)
	// Порог backpressure = 90% от queueMaxSize: при maxSize=1 один ожидающий
	// уже переполняет очередь.
	proxy.queueMgr.maxSize = 1

	if !proxy.tryAcquireSlot("ollama-1") {
		t.Fatal("не удалось занять слот")
	}
	defer proxy.releaseSlot("ollama-1")

	first := make(chan error, 1)
	go func() {
		_, release, _, _, err := proxy.waitForInferenceBackend(
			context.Background(), "m1", types.BackendTypeOllama, "X-User-Id:u5")
		if release != nil {
			release()
		}
		first <- err
	}()
	time.Sleep(200 * time.Millisecond)
	if waiting := proxy.admission.waitingCount(); waiting != 1 {
		t.Fatalf("ожидался 1 ожидающий, получено %d", waiting)
	}

	_, _, _, _, err := proxy.waitForInferenceBackend(
		context.Background(), "m1", types.BackendTypeOllama, "X-User-Id:u6")
	if err != errAdmissionOverloaded {
		t.Errorf("ожидался errAdmissionOverloaded, получено %v", err)
	}

	proxy.releaseSlot("ollama-1")
	select {
	case err := <-first:
		// Первый ожидающий мог получить слот или таймаутнуть (2 c) — важен
		// только факт, что переполнение не сломало его ожидание.
		if err != nil && err != errAdmissionTimeout {
			t.Errorf("первый ожидающий вернул неожиданную ошибку: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("первый ожидающий не завершился")
	}
}

func TestWaitForInferenceBackend_ContextCanceled_R73(t *testing.T) {
	proxy := newUnifiedQueueProxyR73(t, 1)
	proxy.setAdmissionWait(5 * time.Second)

	if !proxy.tryAcquireSlot("ollama-1") {
		t.Fatal("не удалось занять слот")
	}
	defer proxy.releaseSlot("ollama-1")

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	_, _, _, _, err := proxy.waitForInferenceBackend(ctx, "m1", types.BackendTypeOllama, "X-User-Id:u7")
	if err == nil {
		t.Fatal("ожидалась ошибка отмены контекста")
	}
	if !strings.Contains(err.Error(), "context") {
		t.Errorf("ожидалась ошибка контекста, получено %v", err)
	}
}

// TestServeHTTP_UnifiedQueue_OllamaPath_R73 — end-to-end: два параллельных
// запроса на бэкенд с одним слотом оба получают 200, а второй — заголовки
// очереди (раньше его обслуживал legacy QueueManager с другим поведением).
func TestServeHTTP_UnifiedQueue_OllamaPath_R73(t *testing.T) {
	var inFlight int32
	var maxInFlight int32
	var mu sync.Mutex
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/chat"), strings.HasPrefix(r.URL.Path, "/api/generate"):
			cur := atomic.AddInt32(&inFlight, 1)
			for {
				old := atomic.LoadInt32(&maxInFlight)
				if cur <= old || atomic.CompareAndSwapInt32(&maxInFlight, old, cur) {
					break
				}
			}
			time.Sleep(250 * time.Millisecond)
			atomic.AddInt32(&inFlight, -1)
			mu.Lock()
			defer mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"model":      "m1",
				"created_at": time.Now().UTC().Format(time.RFC3339),
				"message":    map[string]interface{}{"role": "assistant", "content": "ok"},
				"done":       true,
			})
		default:
			// /api/tags, health и прочее — пустой успех.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[]}`))
		}
	}))
	defer stub.Close()

	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(stub.URL, "http://"))
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("не удалось разобрать порт stub-сервера: %v", err)
	}

	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host: "localhost", Port: 8080, APIPort: 8081,
			StatePath: filepath.Join(t.TempDir(), "state.json"),
		},
		Backends: []types.Backend{{
			ID: "ollama-1", Name: "stub", Host: "127.0.0.1", OllamaPort: port,
			Type: types.BackendTypeOllama, Status: types.StatusHealthy,
			MaxConcurrentReqs: 1, Weight: 1,
		}},
		Balancing: types.BalancingSettings{
			Algorithm: types.AlgorithmResourceAware, QueueMaxSize: 100,
			QueueWorkers: 1, QueueTimeout: 30,
		},
	}
	proxy := newProxyWithCleanup(t, cfg)
	proxy.setAdmissionWait(5 * time.Second)

	body := `{"model":"m1","messages":[{"role":"user","content":"hi"}],"stream":false}`
	doRequest := func(user string) (*httptest.ResponseRecorder, http.Header) {
		req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-User-Id", user)
		rec := httptest.NewRecorder()
		proxy.ServeHTTP(rec, req)
		return rec, rec.Result().Header
	}

	type res struct {
		header http.Header
		code   int
	}
	results := make(chan res, 2)
	start := make(chan struct{})
	for i, user := range []string{"u1", "u2"} {
		go func(i int, user string) {
			<-start
			rec, hdr := doRequest(user)
			results <- res{header: hdr, code: rec.Code}
		}(i, user)
	}
	close(start)

	var codes []int
	var queued int
	for i := 0; i < 2; i++ {
		select {
		case r := <-results:
			codes = append(codes, r.code)
			if r.header.Get("X-Queue-Wait-Ms") != "" {
				queued++
			}
		case <-time.After(20 * time.Second):
			t.Fatal("запросы не завершились за 20 с")
		}
	}
	for _, code := range codes {
		if code != http.StatusOK {
			t.Errorf("HTTP %d, ожидался 200 (оба запроса должны обслужиться через очередь)", code)
		}
	}
	if queued == 0 {
		t.Error("ни один из параллельных запросов не получил X-Queue-Wait-Ms — очередь не сработала")
	}
	if got := atomic.LoadInt32(&maxInFlight); got > 1 {
		t.Errorf("на бэкенде одновременно было %d запросов, а вместимость = 1 (слоты не соблюдаются)", got)
	}
	if processed := atomic.LoadInt64(&proxy.queueMgr.processed); processed == 0 {
		t.Error("processed_total legacy-очереди не пополняется (RecordUnified не вызван)")
	}
	proxy.queueMgr.historyMu.RLock()
	historyLen := len(proxy.queueMgr.completedHistory)
	proxy.queueMgr.historyMu.RUnlock()
	if historyLen == 0 {
		t.Error("история очереди пуста — /api/v1/queue/history останется без данных")
	}
}
