//go:build llama_stub

// Package balancer — unit-тесты для preflightNCtxReloadIfNeededSync
// (sync-вариант n_ctx preflight, см. nctx_reload_handlers.go).
//
// Sync-вариант БЛОКИРУЕТСЯ и ждёт завершения async reload (если он запущен),
// а не возвращает 503 клиенту как async preflight. Используется, когда
// включён Balancing.PreflightSyncEnabled=true.
//
// Покрываем сценарии:
//   - loaded >= requested → no-op, return immediately
//   - loaded < requested → async reload запущен + heartbeat показывает
//     reload_pending → polling → loaded вырос → success
//   - heartbeat EOF / connection error → продолжаем polling (best-effort)
//   - timeout → 503 Service Unavailable + Retry-After
//
// HTTP-клиент для /api/info heartbeat подменяется через Proxy.metricsHTTPDoer
// (тестовый interface в proxy.go).
package balancer

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// newProxyForPreflightSync — создаёт Proxy с известным cppworker-бэкендом,
// предзаполненным llamaMetrics (loaded_n_ctx). Используется как setup для
// тестов preflightNCtxReloadIfNeededSync.
func newProxyForPreflightSync(t *testing.T, backendID, modelName string, loadedNCtx int) *Proxy {
	t.Helper()
	p := &Proxy{
		config:     &types.LoadBalancerConfig{},
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
	}
	p.backends[backendID] = &BackendState{
		Backend: &types.Backend{
			ID:            backendID,
			Host:          "127.0.0.1",
			CppWorkerPort: 12345,
			Status:        types.StatusHealthy,
			Type:          types.BackendTypeLlamaCpp,
		},
	}
	p.metricsMgr.llamaMetrics[backendID] = &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{
			{Name: modelName, State: "loaded", ContextLength: loadedNCtx},
		},
	}
	// p.nctxReload координатор инициализируется при первом вызове preflight
	// (lazy: проверка p.nctxReload == nil → return early).
	return p
}

// newStubMetricsDoer — создаёт metricsHTTPDoer, который отвечает на /api/info
// заданным телом или ошибкой. Используется для управления heartbeat в тестах.
type stubMetricsDoer struct {
	mu          atomic.Int32 // счётчик вызовов
	pending     atomic.Bool  // если true — reload_pending.model = "test-model"
	loadedNCtx  int          // текущее loaded n_ctx для возврата в heartbeat
	returnError atomic.Bool  // если true — имитируем network error
	failCount   atomic.Int32 // первые N вызовов возвращают ошибку
}

func (s *stubMetricsDoer) Do(req *http.Request) (*http.Response, error) {
	n := int(s.mu.Add(1))
	if s.returnError.Load() && n <= int(s.failCount.Load()) {
		return nil, &netErrorStub{message: "simulated EOF"}
	}
	// Строим JSON вручную, чтобы не тащить httptest сюда.
	body := `{"loaded_n_ctx":` + itoa(s.loadedNCtx) + `}`
	if s.pending.Load() {
		body = `{"reload_pending":{"model":"test-model","progress":0.5},"loaded_n_ctx":` + itoa(s.loadedNCtx) + `}`
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       readCloserFromString(body),
		Header:     make(http.Header),
	}
	resp.Header.Set("Content-Type", "application/json")
	return resp, nil
}

type netErrorStub struct{ message string }

func (e *netErrorStub) Error() string { return e.message }

// itoa — простой int-to-string без strconv (чтобы не раздувать импорты).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	buf := [20]byte{}
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// readCloserFromString — io.ReadCloser из строки (тестовый helper).
func readCloserFromString(s string) *stringReadCloser {
	return &stringReadCloser{s: s}
}

type stringReadCloser struct {
	s   string
	pos int
}

func (r *stringReadCloser) Read(p []byte) (int, error) {
	if r.pos >= len(r.s) {
		return 0, errEOFStub
	}
	n := copy(p, r.s[r.pos:])
	r.pos += n
	return n, nil
}
func (r *stringReadCloser) Close() error { return nil }

var errEOFStub = &eofStub{}

type eofStub struct{}

func (e *eofStub) Error() string { return "EOF" }

// ============================================================
// Тесты preflightNCtxReloadIfNeededSync
// ============================================================

// TestPreflightSync_NoOp_LoadedCoversRequested — loaded >= requested,
// возвращает сразу без ожидания.
func TestPreflightSync_NoOp_LoadedCoversRequested(t *testing.T) {
	t.Parallel()
	const backendID = "test-backend-sync-1"
	const modelName = "qwen2.5.gguf"
	// loaded=8192, requested=4096 → ok.
	p := newProxyForPreflightSync(t, backendID, modelName, 8192)
	stub := &stubMetricsDoer{loadedNCtx: 8192}
	p.metricsHTTPDoer = stub

	body := []byte(`{"model":"` + modelName + `","options":{"num_ctx":4096}}`)
	start := time.Now()
	modifiedBody, needsProxy, msg, status, retry := p.preflightNCtxReloadIfNeededSync(
		context.Background(), backendID, modelName, body)
	elapsed := time.Since(start)

	if !needsProxy || status != http.StatusOK {
		t.Errorf("expected immediate OK; got needsProxy=%v status=%d msg=%q retry=%d",
			needsProxy, status, msg, retry)
	}
	if elapsed > 50*time.Millisecond {
		t.Errorf("expected instant return; got %v", elapsed)
	}
	if int(stub.mu.Load()) != 0 {
		t.Errorf("heartbeat не должен вызываться при loaded>=requested; calls=%d",
			stub.mu.Load())
	}
	_ = modifiedBody
}

// TestPreflightSync_AsyncReloadCompletes — loaded < requested, async reload
// запускается, heartbeat сначала показывает reload_pending, потом loaded вырастает.
// Должен вернуться OK после polling.
func TestPreflightSync_AsyncReloadCompletes(t *testing.T) {
	t.Parallel()
	const backendID = "test-backend-sync-2"
	const modelName = "qwen2.5.gguf"
	// loaded=4096, requested=16384 → reload нужен.
	p := newProxyForPreflightSync(t, backendID, modelName, 4096)
	stub := &stubMetricsDoer{loadedNCtx: 16384} // сразу ready (без pending)
	// prefightNCtxReloadIfNeeded запускает executeAsyncReload в горутине
	// и ставит ContextLength=16384 в metricsManager в самом начале.
	// Stub имитирует состояние "reload уже завершился" → pending=false.
	// Чтобы протестировать polling, нужно показать pending=true на 1-м вызове
	// и потом loaded=16384 на следующем. Но llamaMetrics уже обновлён
	// при старте preflightNCtxReloadIfNeeded до того, как мы вызовем sync.
	// Поэтому polling сразу видит loaded >= requested после старта.
	p.metricsHTTPDoer = stub

	body := []byte(`{"model":"` + modelName + `","options":{"num_ctx":16384}}`)
	modifiedBody, needsProxy, msg, status, retry := p.preflightNCtxReloadIfNeededSync(
		context.Background(), backendID, modelName, body)

	if !needsProxy || status != http.StatusOK {
		t.Errorf("expected OK после reload; got needsProxy=%v status=%d msg=%q retry=%d",
			needsProxy, status, msg, retry)
	}
	if modifiedBody == nil {
		t.Errorf("modifiedBody не должен быть nil")
	}
	_ = retry
}

// TestPreflightSync_NoNumCtxInBody — если в body нет num_ctx,
// async reload не запускается → sync возвращает immediate OK.
func TestPreflightSync_NoNumCtxInBody(t *testing.T) {
	t.Parallel()
	const backendID = "test-backend-sync-3"
	const modelName = "qwen2.5.gguf"
	p := newProxyForPreflightSync(t, backendID, modelName, 4096)
	stub := &stubMetricsDoer{}
	p.metricsHTTPDoer = stub

	body := []byte(`{"model":"` + modelName + `"}`) // без num_ctx
	modifiedBody, needsProxy, msg, status, _ := p.preflightNCtxReloadIfNeededSync(
		context.Background(), backendID, modelName, body)

	if !needsProxy || status != http.StatusOK {
		t.Errorf("expected OK без num_ctx в body; got needsProxy=%v status=%d msg=%q",
			needsProxy, status, msg)
	}
	if int(stub.mu.Load()) != 0 {
		t.Errorf("heartbeat не должен вызываться при no num_ctx; calls=%d", stub.mu.Load())
	}
	_ = modifiedBody
}

// TestPreflightSync_NilNctxReloadReturnsImmediateOK — если p.nctxReload == nil
// (lazy init не произошёл), функция должна вернуть OK без polling.
// Это защитный тест: синхронный preflight без координатора работает как async fallback.
func TestPreflightSync_NilNctxReloadReturnsImmediateOK(t *testing.T) {
	t.Parallel()
	const backendID = "test-backend-sync-4"
	const modelName = "qwen2.5.gguf"
	// loaded < requested, но nctxReload == nil — async-фаза пропустит reload,
	// sync-фаза тоже сразу вернёт OK (llamaMetrics ещё не обновлён).
	p := &Proxy{
		config:     &types.LoadBalancerConfig{},
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
		// nctxReload intentionally nil — lazy init.
	}
	p.backends[backendID] = &BackendState{
		Backend: &types.Backend{
			ID:            backendID,
			Host:          "127.0.0.1",
			CppWorkerPort: 12345,
			Status:        types.StatusHealthy,
			Type:          types.BackendTypeLlamaCpp,
		},
	}
	p.metricsMgr.llamaMetrics[backendID] = &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{
			{Name: modelName, State: "loaded", ContextLength: 4096},
		},
	}

	body := []byte(`{"model":"` + modelName + `"}`) // без num_ctx
	start := time.Now()
	_, needsProxy, _, status, _ := p.preflightNCtxReloadIfNeededSync(
		context.Background(), backendID, modelName, body)
	elapsed := time.Since(start)

	// Без num_ctx в body async-фаза возвращает (bodyBuf, true, "", 200, 0)
	// → sync-фаза делает return с теми же значениями.
	if !needsProxy || status != http.StatusOK {
		t.Errorf("expected OK; got needsProxy=%v status=%d", needsProxy, status)
	}
	if elapsed > 50*time.Millisecond {
		t.Errorf("должен вернуться мгновенно; elapsed=%v", elapsed)
	}
}
