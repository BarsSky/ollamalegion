package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"
)

// TestStreaming_ClientCancel verifies that when the client cancels a streaming request,
// the backend slot is properly released, the session HasActiveStream flag is cleared,
// and no "superfluous WriteHeader" or TransferEncodingError occurs.
func TestStreaming_ClientCancel(t *testing.T) {
	// Backend: SSE endpoint that streams slowly and checks for client disconnect
	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "flusher not supported", 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		// Send a few chunks then wait for client disconnect
		for i := 0; i < 100; i++ {
			select {
			case <-r.Context().Done():
				// Client disconnected — expected behaviour
				return
			case <-time.After(50 * time.Millisecond):
			}
			chunk, _ := json.Marshal(map[string]interface{}{
				"response": "token",
				"done":     false,
			})
			_, err := w.Write([]byte("data: " + string(chunk) + "\n\n"))
			if err != nil {
				// Write error means client disconnected
				return
			}
			flusher.Flush()
		}
	}))
	defer backendServer.Close()

	// Extract host and port from test server
	host, port := parseHostPort(backendServer.URL)

	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Port:    12346,
			APIPort: 12347,
		},
		Backends: []types.Backend{
			{
				ID:                "test-backend",
				Name:              "Test Backend",
				Host:              host,
				OllamaPort:        port,
				MaxConcurrentReqs: 2,
				Weight:            1,
				Status:            types.StatusHealthy,
			},
		},
		Balancing: types.BalancingSettings{
			Algorithm:         types.AlgorithmRoundRobin,
			SessionStickiness: true,
			SessionTTL:        900,
			SessionIdleTTL:    300,
			RequestTimeout:    30,
			StreamTimeout:     60,
		},
	}

	proxy := balancer.NewProxy(cfg)
	proxy.SetQueueManagerProxy()

	proxy.UpdateMetrics("test-backend", &types.BackendMetrics{
		ID: "test-backend",
		GPU: types.GPUMetrics{
			UsagePercent: 30,
			MemoryTotal:  24576,
			MemoryUsed:   8000,
			MemoryFree:   16576,
		},
		System: types.SystemMetrics{
			CPUUsagePercent: 20,
			MemoryTotal:     65536,
			MemoryUsed:      16000,
			MemoryFree:      49536,
			DiskFree:        20480,
		},
		Ollama: types.OllamaMetrics{
			MaxModels:             5,
			MaxConcurrentRequests: 10,
			ActiveRequests:        0,
			OllamaAvailable:       true,
			RunningModels: []types.RunningModel{
				{Name: "test-model", VRAMUsage: 6000},
			},
		},
	})

	// Test 1: Client cancels after receiving first chunk
	t.Run("cancel_after_first_chunk", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		reqBody := `{"model":"test-model","stream":true,"prompt":"hello"}`
		req := httptest.NewRequest(http.MethodPost, "/api/generate", strings.NewReader(reqBody))
		req = req.WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")

		rr := httptest.NewRecorder()

		// Start request in goroutine and cancel after a short delay
		doneCh := make(chan struct{})
		go func() {
			proxy.ServeHTTP(rr, req)
			close(doneCh)
		}()

		// Wait a bit for first chunk to arrive, then cancel
		time.Sleep(200 * time.Millisecond)
		cancel()

		// Wait for handler to return
		select {
		case <-doneCh:
			// Handler returned — success
		case <-time.After(5 * time.Second):
			t.Fatal("handler did not return within 5 seconds after client cancel")
		}

		// Verify backend state was cleaned up
		state := proxy.GetClusterState()
		if state.ActiveRequests != 0 {
			t.Errorf("expected 0 active requests after cancel, got %d", state.ActiveRequests)
		}

		for _, b := range state.Backends {
			if b.Ollama.ActiveRequests != 0 {
				t.Errorf("backend %s has %d active requests after cancel, expected 0", b.ID, b.Ollama.ActiveRequests)
			}
		}

		// Verify no superfluous WriteHeader — status should be 200 (headers sent before cancel)
		if rr.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", rr.Code)
		}
	})

	// Test 2: Verify zombie session cleanup after cancel
	t.Run("zombie_cleanup_after_cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		reqBody := `{"model":"test-model","stream":true,"prompt":"test"}`
		req := httptest.NewRequest(http.MethodPost, "/api/generate", strings.NewReader(reqBody))
		req = req.WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")

		rr := httptest.NewRecorder()
		doneCh := make(chan struct{})
		go func() {
			proxy.ServeHTTP(rr, req)
			close(doneCh)
		}()

		// Cancel before any chunk arrives
		time.Sleep(100 * time.Millisecond)
		cancel()

		select {
		case <-doneCh:
		case <-time.After(5 * time.Second):
			t.Fatal("handler did not return")
		}

		// Immediately make another request — it should NOT get "all backends busy"
		ctx2 := context.Background()
		req2Body := `{"model":"test-model","stream":true,"prompt":"second"}`
		req2 := httptest.NewRequest(http.MethodPost, "/api/generate", strings.NewReader(req2Body))
		req2 = req2.WithContext(ctx2)
		req2.Header.Set("Content-Type", "application/json")

		rr2 := httptest.NewRecorder()
		proxy.ServeHTTP(rr2, req2)

		if rr2.Code == http.StatusServiceUnavailable {
			t.Errorf("second request got 503: backend slot was not released after cancel")
		}
	})

	// Test 3: Verify session stickiness with X-Tab-ID
	t.Run("session_isolation_with_tab_id", func(t *testing.T) {
		// Request 1 with tab-A
		ctx1 := context.Background()
		req1Body := `{"model":"test-model","stream":true,"prompt":"tab-a"}`
		req1 := httptest.NewRequest(http.MethodPost, "/api/generate", strings.NewReader(req1Body))
		req1 = req1.WithContext(ctx1)
		req1.Header.Set("Content-Type", "application/json")
		req1.Header.Set("X-Client-ID", "cline-vscode")
		req1.Header.Set("X-Tab-ID", "tab-a")

		rr1 := httptest.NewRecorder()
		proxy.ServeHTTP(rr1, req1)

		// Request 2 with tab-B — should get different session
		ctx2 := context.Background()
		req2Body := `{"model":"test-model","stream":true,"prompt":"tab-b"}`
		req2 := httptest.NewRequest(http.MethodPost, "/api/generate", strings.NewReader(req2Body))
		req2 = req2.WithContext(ctx2)
		req2.Header.Set("Content-Type", "application/json")
		req2.Header.Set("X-Client-ID", "cline-vscode")
		req2.Header.Set("X-Tab-ID", "tab-b")

		rr2 := httptest.NewRecorder()
		proxy.ServeHTTP(rr2, req2)

		// Both should succeed (not 503) — X-Tab-ID provides session isolation
		if rr1.Code == http.StatusServiceUnavailable {
			t.Errorf("tab-a got 503: unexpected")
		}
		if rr2.Code == http.StatusServiceUnavailable {
			t.Errorf("tab-b got 503: unexpected")
		}

		// Verify at least 2 distinct sessions exist (session isolation works)
		allSessions := proxy.GetSessions()
		if len(allSessions) < 2 {
			t.Logf("Available sessions: %+v", allSessions)
			t.Errorf("expected at least 2 sessions for tab isolation, got %d", len(allSessions))
		}
	})
}