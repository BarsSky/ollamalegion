package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/internal/rptensor"
)

// mockRankTransport — детерминированный RankTransport для тестов API handler.
type mockRankTransport struct {
	rank     int
	latency  time.Duration
	failMode atomic.Bool // если true — возвращает error
}

func (m *mockRankTransport) WorkerID() string { return "mock-w" }
func (m *mockRankTransport) Rank() int       { return m.rank }
func (m *mockRankTransport) InferShard(ctx context.Context, req rptensor.TPShardInferRequest) (*rptensor.TPShardInferResponse, error) {
	if m.failMode.Load() {
		return nil, errSimulated
	}
	if m.latency > 0 {
		time.Sleep(m.latency)
	}
	return &rptensor.TPShardInferResponse{
		Rank:        m.rank,
		Output:      []byte{byte(m.rank + 1)},
		NeedsReduce: true,
	}, nil
}
func (m *mockRankTransport) SyncKVShard(ctx context.Context, sessionID string, shard []byte) error {
	return nil
}
func (m *mockRankTransport) FetchKVShard(ctx context.Context, sessionID string) ([]byte, error) {
	return nil, nil
}

var errSimulated = &simulatedError{}

type simulatedError struct{}

func (s *simulatedError) Error() string { return "simulated rank failure" }

// helper — создаёт Server + регистрирует TP coordinator для теста.
func newServerWithTPCoord(t *testing.T, modelName string, ws, layers int) (*Server, *rptensor.TensorParallelCoordinator) {
	t.Helper()
	srv := &Server{}
	m, err := rptensor.NewShardedModel(modelName, 4096, 14336, layers, 32, 128256)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Partition(rptensor.MegatronPartition, ws); err != nil {
		t.Fatal(err)
	}
	ranks := make([]rptensor.RankTransport, ws)
	for i := 0; i < ws; i++ {
		ranks[i] = &mockRankTransport{rank: i, latency: 1 * time.Millisecond}
	}
	coord, err := rptensor.NewTensorParallelCoordinator(m, ranks, rptensor.CoordinatorConfig{})
	if err != nil {
		t.Fatal(err)
	}
	RegisterTPCoordinator(modelName, coord)
	t.Cleanup(func() { UnregisterTPCoordinator(modelName) })
	return srv, coord
}

func newAuthedRequest(method, path string, body interface{}) *http.Request {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Token", "test-token")
	return req
}

// =====================================================================
// handleRPCModelTPInfer
// =====================================================================

func TestHandleRPCModelTPInfer_Success(t *testing.T) {
	srv, _ := newServerWithTPCoord(t, "test-tp-model", 4, 4)

	body := TPInferAPIRequest{
		ModelName: "test-tp-model",
		Prompt:    "hello world",
	}
	req := newAuthedRequest(http.MethodPost, "/api/v1/rpc/tp/infer", body)
	rr := httptest.NewRecorder()

	srv.handleRPCModelTPInfer(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp TPInferAPIResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.RequestID == "" {
		t.Error("RequestID should be non-empty")
	}
	if resp.WorldSize != 4 || resp.NumLayers != 4 {
		t.Errorf("ws=%d layers=%d", resp.WorldSize, resp.NumLayers)
	}
	if resp.Degraded {
		t.Error("Degraded should be false")
	}
	if len(resp.LayerStats) != 4 {
		t.Errorf("layer_stats: got %d, want 4", len(resp.LayerStats))
	}
	if resp.Output == "" {
		t.Error("output should be non-empty")
	}
}

func TestHandleRPCModelTPInfer_MissingModelName(t *testing.T) {
	srv := &Server{}
	body := TPInferAPIRequest{ModelName: ""}
	req := newAuthedRequest(http.MethodPost, "/api/v1/rpc/tp/infer", body)
	rr := httptest.NewRecorder()

	srv.handleRPCModelTPInfer(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", rr.Code)
	}
}

func TestHandleRPCModelTPInfer_NotConfigured(t *testing.T) {
	srv := &Server{}
	body := TPInferAPIRequest{ModelName: "unknown-model"}
	req := newAuthedRequest(http.MethodPost, "/api/v1/rpc/tp/infer", body)
	rr := httptest.NewRecorder()

	srv.handleRPCModelTPInfer(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("status=%d, want 404", rr.Code)
	}
}

func TestHandleRPCModelTPInfer_MethodNotAllowed(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/rpc/tp/infer", nil)
	rr := httptest.NewRecorder()

	srv.handleRPCModelTPInfer(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status=%d, want 405", rr.Code)
	}
}

func TestHandleRPCModelTPInfer_DisabledCoordinator(t *testing.T) {
	srv, coord := newServerWithTPCoord(t, "disabled-model", 2, 2)
	coord.Close() // disable

	body := TPInferAPIRequest{ModelName: "disabled-model"}
	req := newAuthedRequest(http.MethodPost, "/api/v1/rpc/tp/infer", body)
	rr := httptest.NewRecorder()

	srv.handleRPCModelTPInfer(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d, want 503", rr.Code)
	}
}

func TestHandleRPCModelTPInfer_BadJSON(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/rpc/tp/infer", bytes.NewReader([]byte("not json")))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	srv.handleRPCModelTPInfer(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", rr.Code)
	}
}

// =====================================================================
// handleRPCModelTPStatus
// =====================================================================

func TestHandleRPCModelTPStatus_Success(t *testing.T) {
	srv, _ := newServerWithTPCoord(t, "model-a", 4, 8)
	newServerWithTPCoord(t, "model-b", 2, 4)

	req := newAuthedRequest(http.MethodGet, "/api/v1/rpc/tp/status", nil)
	rr := httptest.NewRecorder()

	srv.handleRPCModelTPStatus(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp TPStatusAPIResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Count != 2 {
		t.Errorf("Count=%d, want 2", resp.Count)
	}
	if len(resp.Models) != 2 {
		t.Fatalf("Models: got %d, want 2", len(resp.Models))
	}
	// Найдём model-a.
	foundA := false
	for _, m := range resp.Models {
		if m.ModelName == "model-a" {
			if m.WorldSize != 4 || m.NumLayers != 8 {
				t.Errorf("model-a: ws=%d layers=%d, want 4/8", m.WorldSize, m.NumLayers)
			}
			foundA = true
		}
	}
	if !foundA {
		t.Error("model-a not found in status")
	}
}

func TestHandleRPCModelTPStatus_Empty(t *testing.T) {
	srv := &Server{}
	req := newAuthedRequest(http.MethodGet, "/api/v1/rpc/tp/status", nil)
	rr := httptest.NewRecorder()

	srv.handleRPCModelTPStatus(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	var resp TPStatusAPIResponse
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp.Count != 0 {
		t.Errorf("Count=%d, want 0", resp.Count)
	}
}

func TestHandleRPCModelTPStatus_MethodNotAllowed(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/rpc/tp/status", nil)
	rr := httptest.NewRecorder()

	srv.handleRPCModelTPStatus(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status=%d, want 405", rr.Code)
	}
}

// =====================================================================
// Registry
// =====================================================================

func TestTPRegistry_RegisterUnregister(t *testing.T) {
	m, _ := rptensor.NewShardedModel("m", 4096, 14336, 2, 32, 128256)
	_ = m.Partition(rptensor.MegatronPartition, 2)
	ranks := []rptensor.RankTransport{
		&mockRankTransport{rank: 0},
		&mockRankTransport{rank: 1},
	}
	coord, _ := rptensor.NewTensorParallelCoordinator(m, ranks, rptensor.CoordinatorConfig{})

	RegisterTPCoordinator("test-registry-model", coord)
	if LookupTPCoordinator("test-registry-model") != coord {
		t.Error("registered coord not found")
	}

	UnregisterTPCoordinator("test-registry-model")
	if LookupTPCoordinator("test-registry-model") != nil {
		t.Error("coord should be unregistered")
	}
}

func TestTPRegistry_UnregisterMissing(t *testing.T) {
	// Не паникует.
	UnregisterTPCoordinator("never-existed")
}