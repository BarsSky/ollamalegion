//go:build llama_stub

// R60.23 (2026-09-09): tests for empty-modelName guard in
// preflightNCtxReloadIfNeeded + executeAsyncReload.
//
// Bug being tested: если client прислал запрос с пустым model (например,
// POST /api/load без body, или OpenWebUI preload probe), parseRequestBody
// возвращал пустой model, balancer-овский context был пустой, OLD
// preflightNCtxReloadIfNeeded срабатывал с modelName="", запускал
// async reload через executeAsyncReload → POST /api/models/load
// с `"name": ""` → cppworker возвращал 400 "name is required".
// State-машина coordinator зависала в "reloading", все последующие
// /api/chat запросы получали 503 "retry in 30s".
//
// Fix: early-return в preflightNCtxReloadIfNeeded (skip preflight) +
// early-return в executeAsyncReload (defensive double-check).
package balancer

import (
	"net/http"
	"testing"
	"time"
)

// TestPreflightNCtxReload_EmptyModel_SkipsPreflight — главный регрессионный
// тест. При modelName=="" preflight должен:
//   - вернуть (body, true, "", 200) — пропустить preflight, проксировать как есть
//   - НЕ запускать executeAsyncReload
//   - НЕ блокировать IsReloadPending state
//
// Воспроизводит root cause: R60.23 OpenWebUI cut-off.
func TestPreflightNCtxReload_EmptyModel_SkipsPreflight(t *testing.T) {
	modelName := "test-model"
	cppWorker, _, reloadCalls := makeMockCppWorkerWithReload(t, modelName, 4096)
	p := buildProxyWithMockInitialCtx(t, cppWorker.URL, modelName, 4096)

	// Body с пустым model — имитация /api/load probe или malformed request.
	body := []byte(`{}`)

	newBody, ok, msg, status := p.preflightNCtxReloadIfNeeded(nil, "test-backend", "", body, "/api/load")

	if !ok {
		t.Errorf("preflight should skip (needsProxy=true) for empty modelName; got ok=false msg=%q status=%d", msg, status)
	}
	if status != http.StatusOK {
		t.Errorf("preflight status=%d, want 200 (skip — proxy as-is to cppworker)", status)
	}
	if msg != "" {
		t.Errorf("preflight msg=%q, want empty (no error)", msg)
	}
	if string(newBody) != string(body) {
		t.Errorf("preflight should NOT modify body when skipping; got %q, want %q", newBody, body)
	}

	// Reload НЕ должен был вызваться (даже через 200ms wait).
	time.Sleep(200 * time.Millisecond)
	if got := len(*reloadCalls); got != 0 {
		t.Errorf("expected 0 reload calls (empty modelName → skip preflight), got %d", got, *reloadCalls)
	}
}

// TestPreflightNCtxReload_EmptyModel_NoIsReloadPending — после skip preflight
// IsReloadPending должен остаться false. Без этого повторные /api/load
// запросы с пустым model могли бы зависнуть в 503.
func TestPreflightNCtxReload_EmptyModel_NoIsReloadPending(t *testing.T) {
	modelName := "test-model"
	cppWorker, _, _ := makeMockCppWorkerWithReload(t, modelName, 4096)
	p := buildProxyWithMockInitialCtx(t, cppWorker.URL, modelName, 4096)

	body := []byte(`{}`)

	// 3 запроса с пустым model.
	for i := 0; i < 3; i++ {
		_, ok, _, _ := p.preflightNCtxReloadIfNeeded(nil, "test-backend", "", body, "/api/load")
		if !ok {
			t.Errorf("iteration %d: preflight should skip (ok=true) for empty modelName", i)
		}
		if p.nctxReload.IsReloadPending("test-backend", "") {
			t.Errorf("iteration %d: IsReloadPending should be false (no reload was registered)", i)
		}
	}
}

// TestExecuteAsyncReload_EmptyModel_NoCall — defensive guard в executeAsyncReload.
// Если какой-то caller (включая багнутую версию preflight) вызовет с пустым
// modelName, executeAsyncReload должен early-return без HTTP-запроса.
func TestExecuteAsyncReload_EmptyModel_NoCall(t *testing.T) {
	modelName := "test-model"
	cppWorker, _, reloadCalls := makeMockCppWorkerWithReload(t, modelName, 4096)
	p := buildProxyWithMockInitialCtx(t, cppWorker.URL, modelName, 4096)

	// Запускаем executeAsyncReload с пустым modelName.
	p.executeAsyncReload("test-backend", "", 65536, p.backends["test-backend"].Backend)

	// Даём goroutine шанс запуститься.
	time.Sleep(200 * time.Millisecond)

	// cppworker НЕ должен был получить POST /api/models/load (мы скипнули).
	if got := len(*reloadCalls); got != 0 {
		t.Errorf("executeAsyncReload(empty) должен skip reload, но cppworker получил %d вызов(ов): %v", got, *reloadCalls)
	}
}

// TestPreflightNCtxReload_EmptyModel_ValidRequest_StillProxies — sanity check:
// empty-model skip не ломает валидные запросы с modelName.
func TestPreflightNCtxReload_EmptyModel_ValidRequest_StillProxies(t *testing.T) {
	modelName := "test-model"
	cppWorker, _, _ := makeMockCppWorkerWithReload(t, modelName, 4096)
	p := buildProxyWithMockInitialCtx(t, cppWorker.URL, modelName, 4096)

	// Валидный body с model и prompt, который помещается в 4096.
	body := []byte(`{"model":"` + modelName + `","messages":[{"role":"user","content":"test"}]}`)

	// Smart-skip: prompt маленький → reload не нужен, body не модифицируется,
	// preflight возвращает ok=true, status=200.
	newBody, ok, msg, status := p.preflightNCtxReloadIfNeeded(nil, "test-backend", modelName, body, "/api/chat")

	if !ok || status != http.StatusOK {
		t.Errorf("valid request: preflight should succeed (smart-skip); ok=%v msg=%q status=%d", ok, msg, status)
	}
	if newBody == nil {
		t.Error("newBody should not be nil")
	}
}
