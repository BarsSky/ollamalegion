package rpccoordinator

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"ollama-loadbalancer/internal/rptensor"
)

// mockTPServer — реализует /rpc/tp/{infer,kv_sync,kv_fetch} через httptest.
//
// По умолчанию: TPInfer возвращает partial output длиной
// tpPartialLen-equivalent (rank+1 байт × partial_len). Это
// гарантирует детерминированный вывод без зависимости от
// rptensor.TPPartialLen (чтобы тест не зависел от точной формулы).
type mockTPServer struct {
	mu       sync.Mutex
	infers   []rptensor.TPShardInferRequest // capture for assertions
	kvSync   []kvSyncRecord
}

type kvSyncRecord struct {
	SessionID string
	Rank      int
	Shard     []byte
}

func newMockTPServer() *mockTPServer {
	return &mockTPServer{}
}

func (m *mockTPServer) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/rpc/tp/infer", func(w http.ResponseWriter, r *http.Request) {
		var req rptensor.TPShardInferRequest
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		m.infers = append(m.infers, req)
		m.mu.Unlock()

		// Stub: output = 1 байт с rank+1 (детерминированно).
		out := []byte{byte(req.Rank + 1)}
		resp := rptensor.TPShardInferResponse{
			Rank:        req.Rank,
			Output:      out,
			KVShard:     []byte{byte(req.Rank)},
			NeedsReduce: true,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/rpc/tp/kv_sync", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			SessionID string `json:"session_id"`
			Rank      int    `json:"rank"`
			Shard     []byte `json:"shard"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		m.kvSync = append(m.kvSync, kvSyncRecord{
			SessionID: req.SessionID,
			Rank:      req.Rank,
			Shard:     req.Shard,
		})
		m.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status":     "saved",
			"session_id": req.SessionID,
			"rank":       req.Rank,
		})
	})

	mux.HandleFunc("/rpc/tp/kv_fetch", func(w http.ResponseWriter, r *http.Request) {
		sessionID := r.URL.Query().Get("session_id")
		rankStr := r.URL.Query().Get("rank")
		if sessionID == "" || rankStr == "" {
			http.Error(w, "missing params", http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, rec := range m.kvSync {
			if rec.SessionID == sessionID && rec.Rank == parseIntOrZero(rankStr) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"session_id": sessionID,
					"rank":       rec.Rank,
					"shard":      rec.Shard,
				})
				return
			}
		}
		http.Error(w, "not found", http.StatusNotFound)
	})
	return mux
}

func parseIntOrZero(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// helper для создания WorkerClient с mockTPServer.
func newClientForMock(t *testing.T, mock *mockTPServer) (*WorkerClient, *httptest.Server) {
	ts := httptest.NewServer(mock.handler(t))
	host, port := splitHostPort(ts.URL)
	wc := NewWorkerClient("mock-w", host, port, "http", &http.Client{})
	return wc, ts
}

func splitHostPort(url string) (string, int) {
	// url like "http://127.0.0.1:43511" → "127.0.0.1", 43511
	u := strings.TrimPrefix(url, "http://")
	idx := strings.LastIndex(u, ":")
	if idx < 0 {
		return u, 80
	}
	host := u[:idx]
	pstr := u[idx+1:]
	port := 0
	for _, c := range pstr {
		if c < '0' || c > '9' {
			break
		}
		port = port*10 + int(c-'0')
	}
	return host, port
}

// =====================================================================
// TPInferSlice
// =====================================================================

func TestTPInferSlice_Success(t *testing.T) {
	mock := newMockTPServer()
	wc, ts := newClientForMock(t, mock)
	defer ts.Close()

	req := rptensor.TPShardInferRequest{
		ModelName: "test",
		Rank:      2,
		WorldSize: 4,
		Layer:     5,
		Input:     []byte("hello"),
	}
	resp, err := wc.TPInferSlice(context.Background(), req)
	if err != nil {
		t.Fatalf("TPInferSlice: %v", err)
	}
	if resp.Rank != 2 {
		t.Errorf("rank: got %d, want 2", resp.Rank)
	}
	if len(resp.Output) != 1 || resp.Output[0] != 3 {
		t.Errorf("output: got %v, want [3]", resp.Output)
	}
	if !resp.NeedsReduce {
		t.Error("NeedsReduce should be true")
	}

	// Проверяем что mock действительно получил запрос.
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.infers) != 1 {
		t.Fatalf("mock infers: got %d, want 1", len(mock.infers))
	}
	if mock.infers[0].ModelName != "test" || mock.infers[0].Layer != 5 {
		t.Errorf("mock captured wrong request: %+v", mock.infers[0])
	}
}

func TestTPInferSlice_HTTPError(t *testing.T) {
	mock := newMockTPServer()
	wc, ts := newClientForMock(t, mock)
	defer ts.Close()

	// Закрываем сервер чтобы получить connection refused.
	ts.Close()

	_, err := wc.TPInferSlice(context.Background(), rptensor.TPShardInferRequest{
		Rank: 0, WorldSize: 1, Layer: 0,
	})
	if err == nil {
		t.Error("expected connection error")
	}
}

func TestTPInferSlice_BuildURL(t *testing.T) {
	wc := NewWorkerClient("w", "host", 1234, "http", &http.Client{})
	if got := wc.BaseURL(); got != "http://host:1234" {
		t.Errorf("BaseURL: got %q, want %q", got, "http://host:1234")
	}
}

// =====================================================================
// TPKvSync / TPKvFetch
// =====================================================================

func TestTPKvSync_Fetch_Roundtrip(t *testing.T) {
	mock := newMockTPServer()
	wc, ts := newClientForMock(t, mock)
	defer ts.Close()

	ctx := context.Background()
	// Sync shard for rank 0.
	if err := wc.TPKvSync(ctx, "sess-1", 0, []byte("hello")); err != nil {
		t.Fatalf("TPKvSync: %v", err)
	}
	// Sync shard for rank 1.
	if err := wc.TPKvSync(ctx, "sess-1", 1, []byte("world")); err != nil {
		t.Fatalf("TPKvSync rank 1: %v", err)
	}

	// Fetch rank 0.
	got, err := wc.TPKvFetch(ctx, "sess-1", 0)
	if err != nil {
		t.Fatalf("TPKvFetch rank 0: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("rank 0 shard: got %q, want %q", got, "hello")
	}

	// Fetch rank 1.
	got, err = wc.TPKvFetch(ctx, "sess-1", 1)
	if err != nil {
		t.Fatalf("TPKvFetch rank 1: %v", err)
	}
	if string(got) != "world" {
		t.Errorf("rank 1 shard: got %q, want %q", got, "world")
	}
}

func TestTPKvFetch_NotFound(t *testing.T) {
	mock := newMockTPServer()
	wc, ts := newClientForMock(t, mock)
	defer ts.Close()

	// Нет shard → (nil, nil) без ошибки.
	got, err := wc.TPKvFetch(context.Background(), "no-such-session", 0)
	if err != nil {
		t.Errorf("expected nil error for not-found, got %v", err)
	}
	if got != nil {
		t.Errorf("expected nil shard, got %q", got)
	}
}

func TestTPKvSync_HTTPError(t *testing.T) {
	mock := newMockTPServer()
	wc, ts := newClientForMock(t, mock)
	defer ts.Close()
	ts.Close() // connection refused

	err := wc.TPKvSync(context.Background(), "s", 0, []byte("x"))
	if err == nil {
		t.Error("expected error on connection refused")
	}
}

// =====================================================================
// Backward-compat: WorkerClient.InferSlice остался без изменений
// =====================================================================

func TestWorkerClient_InferSlice_BackwardCompat(t *testing.T) {
	// Проверяем что добавление TPInferSlice не сломало InferSlice.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rpc/infer" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"worker_id":"mock","slice_id":"1-4","output":"ok","tokens_used":1,"latency_ms":5}`))
	}))
	defer ts.Close()

	host, port := splitHostPort(ts.URL)
	wc := NewWorkerClient("w", host, port, "http", &http.Client{})

	got, err := wc.InferSlice(context.Background(), SliceInferRequest{
		ModelName:  "test",
		StartLayer: 1,
		EndLayer:   4,
		Input:      []byte("hi"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte("ok")) {
		t.Errorf("InferSlice response: got %q", got)
	}
}