package balancer

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Round 8 (2026-07-10): tests for graceful load timeout handling.

func TestIsTimeoutError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "client_timeout_exceeded",
			err:  errors.New(`Post "http://cppworker-gpu:18092/api/models/load-with-params": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`),
			want: true,
		},
		{
			name: "context_deadline_exceeded",
			err:  errors.New(`Post "http://example.com": context deadline exceeded`),
			want: true,
		},
		{
			name: "io_timeout",
			err:  errors.New(`read tcp 172.18.0.4:18092: i/o timeout`),
			want: true,
		},
		// Non-timeout errors
		{
			name: "connection_refused",
			err:  errors.New("connection refused"),
			want: false,
		},
		{
			name: "connection_reset",
			err:  errors.New("connection reset by peer"),
			want: false,
		},
		{
			name: "EOF",
			err:  errors.New("unexpected EOF"),
			want: false,
		},
		{
			name: "nil",
			err:  nil,
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isTimeoutError(tc.err)
			if got != tc.want {
				t.Errorf("isTimeoutError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// mockModelsBody is a helper struct for tests.
type mockModelsBody struct {
	Models []struct {
		Name  string `json:"name"`
		State string `json:"state"`
	} `json:"models"`
}

// startMockCppworkerWithModels creates a httptest.Server that responds to
// /api/models with the given model states.
func startMockCppworkerWithStates(t *testing.T, states map[string]string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models" {
			body := mockModelsBody{}
			for name, state := range states {
				body.Models = append(body.Models, struct {
					Name  string `json:"name"`
					State string `json:"state"`
				}{name, state})
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(body)
			return
		}
		http.NotFound(w, r)
	}))
	return server
}

// parseTestHostPort parses "127.0.0.1:54321" into host and port.
func parseTestHostPort(t *testing.T, url string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(url, "http://"))
	if err != nil {
		t.Fatalf("failed to parse %q: %v", url, err)
	}
	var port int
	fmt.Sscanf(portStr, "%d", &port)
	return host, port
}

// TestPollLoadCompletionUntilLoaded_AlreadyLoaded validates that polling returns
// success when model is already in `loaded` state.
func TestPollLoadCompletionUntilLoaded_AlreadyLoaded(t *testing.T) {
	server := startMockCppworkerWithStates(t, map[string]string{
		"test-model": "loaded",
	})
	defer server.Close()
	host, port := parseTestHostPort(t, server.URL)
	mm := NewModelManager(nil)

	result := mm.pollLoadCompletionUntilLoaded(host, port, "test-backend", "test-model", 5*time.Second)
	if result == nil {
		t.Fatal("expected non-nil result for already-loaded model")
	}
	if !result.Success {
		t.Errorf("expected Success=true, got false. Error: %s", result.Error)
	}
}

// TestPollLoadCompletionUntilLoaded_NeverLoads returns nil when model never loads.
func TestPollLoadCompletionUntilLoaded_NeverLoads(t *testing.T) {
	server := startMockCppworkerWithStates(t, map[string]string{
		"test-model": "loading",
	})
	defer server.Close()
	host, port := parseTestHostPort(t, server.URL)
	mm := NewModelManager(nil)

	// Short timeout (3s) — model stays "loading" → returns nil.
	result := mm.pollLoadCompletionUntilLoaded(host, port, "test-backend", "test-model", 3*time.Second)
	if result != nil {
		t.Errorf("expected nil result for model that never loads, got %+v", result)
	}
}

// TestPollLoadCompletionUntilLoaded_TransitionFromLoadingToLoaded simulates
// the realistic scenario: model is loading at first poll, then becomes loaded.
// Polling should return success when state transitions to "loaded".
func TestPollLoadCompletionUntilLoaded_TransitionFromLoadingToLoaded(t *testing.T) {
	// Mock server: state transitions from "loading" to "loaded" after 2 polls.
	state := "loading"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models" {
			body := mockModelsBody{}
			body.Models = append(body.Models, struct {
				Name  string `json:"name"`
				State string `json:"state"`
			}{"test-model", state})
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(body)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	host, port := parseTestHostPort(t, server.URL)
	mm := NewModelManager(nil)

	// Flip state to "loaded" after 1 second (mock poll interval is 2s, so the
	// poll after the flip should succeed).
	go func() {
		time.Sleep(3 * time.Second)
		state = "loaded"
	}()

	result := mm.pollLoadCompletionUntilLoaded(host, port, "test-backend", "test-model", 8*time.Second)
	if result == nil {
		t.Fatal("expected non-nil result after state transition")
	}
	if !result.Success {
		t.Errorf("expected Success=true after transition to loaded, got %+v", result)
	}
}

// TestPollLoadCompletionUntilLoaded_OnlyMatchesRequestedModel ensures that
// we only return success when the SPECIFIC requested model is loaded,
// not when any other model happens to be loaded.
func TestPollLoadCompletionUntilLoaded_OnlyMatchesRequestedModel(t *testing.T) {
	server := startMockCppworkerWithStates(t, map[string]string{
		"other-model": "loaded",
	})
	defer server.Close()
	host, port := parseTestHostPort(t, server.URL)
	mm := NewModelManager(nil)

	result := mm.pollLoadCompletionUntilLoaded(host, port, "test-backend", "test-model", 2*time.Second)
	if result != nil {
		t.Errorf("expected nil (test-model not loaded), got %+v", result)
	}
}

// ============================================================
// Round 24 (2026-08-04): 202 Accepted (async load) handling tests.
// cppworker теперь по умолчанию возвращает 202 + Location + estimatedLoadTimeMs
// сразу, а реальная загрузка идёт в background goroutine. Balancer должен
// детектить 202 + status=loading и поллить /api/models пока state не станет
// "loaded" (или maxWait не истечёт).
// ============================================================

// startMockCppworkerAsyncLoad — мок, который:
//   - POST /api/models/load        → 202 Accepted + {status:"loading", estimatedLoadTimeMs}
//   - POST /api/models/load-with-params → то же самое
//   - GET  /api/models             → state="loaded" через transitionAfter секунд
//     (до этого — state="loading", transitionAfter=0 → сразу loaded)
//
// stateTransition нужен для теста "сначала loading, потом loaded".
func startMockCppworkerAsyncLoad(t *testing.T, transitionAfter time.Duration) (*httptest.Server, *atomic.Value) {
	t.Helper()
	state := &atomic.Value{}
	state.Store("loading")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/load", "/api/models/load-with-params":
			// Return 202 Accepted with location header and async-load body.
			w.Header().Set("Location", "/api/models/load/progress?model=test-model")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"status":              "loading",
				"name":                "test-model",
				"path":                "/models/test.gguf",
				"loadingSizeBytes":    5368709120, // 5GB
				"estimatedLoadTimeMs": 57200,      // ~57s, как для gemma-4
				"progressUrl":         "/api/models/load/progress?model=test-model",
				"model": map[string]interface{}{
					"name":  "test-model",
					"path":  "/models/test.gguf",
					"state": "loading",
				},
			})
			return
		case "/api/models":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"models": []map[string]interface{}{
					{"name": "test-model", "state": state.Load().(string)},
				},
				"count": 1,
			})
			return
		}
		http.NotFound(w, r)
	}))
	if transitionAfter > 0 {
		go func() {
			time.Sleep(transitionAfter)
			state.Store("loaded")
		}()
	} else {
		// transitionAfter=0 → сразу loaded
		state.Store("loaded")
	}
	return server, state
}

// TestExecuteLlamaCppLoad_202ImmediateLoaded — cppworker сразу отдаёт
// state=loaded в /api/models. Balancer детектит 202 + поллит один раз → success.
func TestExecuteLlamaCppLoad_202ImmediateLoaded(t *testing.T) {
	server, _ := startMockCppworkerAsyncLoad(t, 0)
	defer server.Close()
	host, port := parseTestHostPort(t, server.URL)
	mm := NewModelManager(nil)

	result := mm.executeLlamaCppLoad(host, port, "test-backend", ModelOpRequest{
		Operation: "load",
		ModelName: "test-model",
	})
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if !result.Success {
		t.Errorf("expected Success=true, got false. Error: %s", result.Error)
	}
}

// TestExecuteLlamaCppLoad_202TransitionLoadingToLoaded — cppworker сначала
// возвращает state=loading, через 3 сек state=loaded. Polling должен дождаться.
func TestExecuteLlamaCppLoad_202TransitionLoadingToLoaded(t *testing.T) {
	server, _ := startMockCppworkerAsyncLoad(t, 3*time.Second)
	defer server.Close()
	host, port := parseTestHostPort(t, server.URL)
	mm := NewModelManager(nil)

	start := time.Now()
	result := mm.executeLlamaCppLoad(host, port, "test-backend", ModelOpRequest{
		Operation: "load",
		ModelName: "test-model",
	})
	elapsed := time.Since(start)

	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if !result.Success {
		t.Errorf("expected Success=true, got false. Error: %s", result.Error)
	}
	// Should have polled at least once (poll interval ~2s, transition at 3s).
	if elapsed < 2*time.Second {
		t.Errorf("execution too fast (%v) — did we actually poll?", elapsed)
	}
	if elapsed > 15*time.Second {
		t.Errorf("execution too slow (%v) — polling should be bounded", elapsed)
	}
}

// TestExecuteLlamaCppLoad_202NeverLoads — cppworker всегда state=loading.
// Polling исчерпывает maxWait → возвращается error.
func TestExecuteLlamaCppLoad_202NeverLoads(t *testing.T) {
	// Используем mock который никогда не переключает state.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/load", "/api/models/load-with-params":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"status":              "loading",
				"estimatedLoadTimeMs": 1000, // 1s estimate → maxWait ~11.5s + 10s = 11.5s
			})
			return
		case "/api/models":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"models": []map[string]interface{}{
					{"name": "test-model", "state": "loading"},
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	host, port := parseTestHostPort(t, server.URL)
	mm := NewModelManager(nil)

	result := mm.executeLlamaCppLoad(host, port, "test-backend", ModelOpRequest{
		Operation: "load",
		ModelName: "test-model",
	})
	if result == nil {
		t.Fatal("expected non-nil result (load failed after polling)")
	}
	if result.Success {
		t.Errorf("expected Success=false (model never loaded), got true")
	}
	if !strings.Contains(result.Error, "not ready") {
		t.Errorf("expected error to mention 'not ready', got: %s", result.Error)
	}
}

// TestExecuteLlamaCppLoad_202AlreadyLoadedStatus — cppworker возвращает 202
// со status=already_loaded (модель только что загружена другой горутиной).
// Balancer НЕ должен поллить, а сразу вернуть success.
func TestExecuteLlamaCppLoad_202AlreadyLoadedStatus(t *testing.T) {
	var pollCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/load", "/api/models/load-with-params":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			// status=already_loaded → не идём в polling path.
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"status": "already_loaded",
				"model":  map[string]interface{}{"name": "test-model", "state": "loaded"},
			})
			return
		case "/api/models":
			atomic.AddInt32(&pollCount, 1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"models": []map[string]interface{}{{"name": "test-model", "state": "loaded"}},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	host, port := parseTestHostPort(t, server.URL)
	mm := NewModelManager(nil)

	result := mm.executeLlamaCppLoad(host, port, "test-backend", ModelOpRequest{
		Operation: "load",
		ModelName: "test-model",
	})
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if !result.Success {
		t.Errorf("expected Success=true, got false. Error: %s", result.Error)
	}
	// /api/models НЕ должен был быть опрошен (status=already_loaded не triggers polling).
	if atomic.LoadInt32(&pollCount) > 0 {
		t.Errorf("expected 0 polls for already_loaded status, got %d", pollCount)
	}
}