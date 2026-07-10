package balancer

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
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