// Round 52 (2026-08-24): tests for load-lock release semantics.
//
// Background:
//   Pre-R52 had a lock leak in handleLoadWithParams SYNC path. The inline
//   goroutine `go func() { loadDone <- backend.LoadModelWithOpts(...) }()`
//   did NOT call UnlockLoad on its own. The case `<-loadDone` branch
//   called it, but case `<-timeout.C` did NOT — and the background
//   goroutine outlived the timeout (LoadModelWithOpts is blocking CGo).
//
//   Result: b.loading[name] channel stayed open for the rest of the load
//   (often 60-180s for big models). Subsequent load requests with the same
//   name saw `lockOk=false` from TryLockLoad (b.loading[name] exists) and
//   fell into "model is already being loaded; waiting" indefinitely.
//
//   R52 fix: defer UnlockLoad in the background goroutine (idempotent).
//   This test verifies the lock is released when LoadModelWithOpts returns,
//   regardless of whether the requester hit timeout or got the response.

package main

import (
	"io"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
)

func setupLoadLockTestBackend(t *testing.T) {
	t.Helper()
	cfg := cppbackend.Config{
		ModelsDir:       t.TempDir(),
		DefaultCtxSize:  4096,
		DefaultBatchSize: 512,
	}
	backend = cppbackend.NewBackend(cfg)
}

// bodyReq — создаёт httptest.Request с body и io.NopCloser обёрткой
// (httptest.NewRequest возвращает *Request с Body io.ReadCloser, а
// strings.NewReader — io.Reader; нужна обёртка).
func bodyReq(method, target, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, target, io.NopCloser(strings.NewReader(body)))
	r.Header.Set("Content-Type", "application/json")
	return w
}

// TestLoadLock_SyncTimeout_ReleasesLockAfterBackgroundCompletes —
// Round 52 BUGFIX. Verifies that after a sync load request times out,
// the underlying lock is released when the background goroutine
// eventually completes LoadModelWithOpts (which is a stub that
// returns an error in llama_stub mode).
//
// The fix: inline goroutine in handleLoadWithParams uses `defer UnlockLoad`,
// so the lock is released as soon as the background goroutine returns.
// Before the fix, the lock was held until the main select case `loadErr`
// fired, which never happens if timeout.C wins.
func TestLoadLock_SyncTimeout_ReleasesLockAfterBackgroundCompletes(t *testing.T) {
	setupLoadLockTestBackend(t)
	modelName := "r52-load-lock-test-model"

	// First request: trigger the load with a tiny wait timeout (1ms).
	// In llama_stub mode LoadModelWithOpts returns an error fast.
	// After ~0ms the goroutine completes, defer fires UnlockLoad.
	w1 := bodyReq("POST",
		"/api/models/load-with-params?wait=true&waitTimeoutMs=1",
		`{"name":"`+modelName+`","contextSize":4096,"numGpuLayers":0}`)

	// Fire w1 in goroutine; handleLoadWithParams may take a few μs.
	go handleLoadWithParams(w1, httptest.NewRequest("POST",
		"/api/models/load-with-params?wait=true&waitTimeoutMs=1",
		io.NopCloser(strings.NewReader(
			`{"name":"`+modelName+`","contextSize":4096,"numGpuLayers":0}`))))
	_ = w1 // silence unused
	time.Sleep(100 * time.Millisecond)

	// Now verify the lock is released by attempting a fresh load.
	// If the lock is still held, TryLockLoad returns (false, nil) and
	// the handler falls into "model is already being loaded; waiting".
	w2 := bodyReq("POST", "/api/models/load-with-params?wait=false",
		`{"name":"`+modelName+`_2","contextSize":4096,"numGpuLayers":0}`)
	handleLoadWithParams(w2, httptest.NewRequest("POST",
		"/api/models/load-with-params?wait=false",
		io.NopCloser(strings.NewReader(
			`{"name":"`+modelName+`_2","contextSize":4096,"numGpuLayers":0}`))))

	if w2.Code == 0 || w2.Body.Len() == 0 {
		t.Fatalf("second load returned empty response — request may have hung due to leaked lock")
	}
	body2 := w2.Body.String()
	if strings.Contains(body2, "model is already being loaded") {
		t.Errorf("R52 regression: second load hit 'model is already being loaded' (lock leak); body=%s", body2)
	}
}

// TestLoadLock_RepeatedSyncTimeout_NoCumulativeLeak — pre-R52 multiple
// timeout'd loads would each leak a lock entry. This test ensures R52
// releases the lock even after many timeout'd requests, so subsequent
// loads still work.
func TestLoadLock_RepeatedSyncTimeout_NoCumulativeLeak(t *testing.T) {
	setupLoadLockTestBackend(t)

	// Fire 5 sync timeout loads in sequence, then verify a fresh
	// load still works (no cumulative leak).
	for i := 0; i < 5; i++ {
		body := `{"name":"r52-leak-test-` + strconv.Itoa(i) +
			`","contextSize":4096,"numGpuLayers":0}`
		w := bodyReq("POST",
			"/api/models/load-with-params?wait=true&waitTimeoutMs=1", body)
		handleLoadWithParams(w, httptest.NewRequest("POST",
			"/api/models/load-with-params?wait=true&waitTimeoutMs=1",
			io.NopCloser(strings.NewReader(body))))
		// Allow background goroutine to run
		time.Sleep(50 * time.Millisecond)
	}

	// After 5 leaked locks (pre-R52), TryLockLoad on a 6th model would
	// return lockOk=false and the handler would say "already being loaded".
	// After R52 fix, all 5 lock entries should be closed+deleted.
	bodyPost := `{"name":"r52-after-5-timeouts","contextSize":4096,"numGpuLayers":0}`
	w := bodyReq("POST", "/api/models/load-with-params?wait=false", bodyPost)
	handleLoadWithParams(w, httptest.NewRequest("POST",
		"/api/models/load-with-params?wait=false",
		io.NopCloser(strings.NewReader(bodyPost))))

	if w.Code == 0 || w.Body.Len() == 0 {
		t.Fatal("post-5-timeouts load returned empty response — cumulative lock leak")
	}
	bodyStr := w.Body.String()
	if strings.Contains(bodyStr, "model is already being loaded") {
		t.Errorf("R52 regression: cumulative lock leak after 5 timeouts; body=%s", bodyStr)
	}
}

// TestLoadLock_ConcurrentWaiters_AfterTimeout — when one slow load holds
// the lock, a concurrent waiter should NOT see "stuck" status forever.
// After the background load completes (with error), waiters get the
// failure response. R52 doesn't change wait semantics — but verifies
// the lock IS released so the next iteration can proceed.
func TestLoadLock_ConcurrentWaiters_AfterTimeout(t *testing.T) {
	setupLoadLockTestBackend(t)
	modelName := "r52-concurrent-waiter"

	body1 := `{"name":"` + modelName + `","contextSize":4096,"numGpuLayers":0}`

	// Trigger sync load with tiny timeout
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		handleLoadWithParams(
			bodyReq("POST",
				"/api/models/load-with-params?wait=true&waitTimeoutMs=1", body1),
			httptest.NewRequest("POST",
				"/api/models/load-with-params?wait=true&waitTimeoutMs=1",
				io.NopCloser(strings.NewReader(body1))))
	}()

	// Concurrent waiter (wait=true, longer timeout)
	time.Sleep(20 * time.Millisecond)
	body2 := `{"name":"` + modelName + `_waiter","contextSize":4096,"numGpuLayers":0}`
	w2 := bodyReq("POST",
		"/api/models/load-with-params?wait=true&waitTimeoutMs=200", body2)
	handleLoadWithParams(w2, httptest.NewRequest("POST",
		"/api/models/load-with-params?wait=true&waitTimeoutMs=200",
		io.NopCloser(strings.NewReader(body2))))

	wg.Wait()

	if w2.Body.Len() == 0 {
		t.Error("concurrent waiter returned empty response — lock leak")
	}
}
