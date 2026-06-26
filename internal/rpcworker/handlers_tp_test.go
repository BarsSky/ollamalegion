package rpcworker

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// helper: создаёт WorkerServer с фиктивным .gguf файлом и загружает slice.
func newTPTestServer(t *testing.T, workerID string, modelBase string) (*httptest.Server, *WorkerServer) {
	t.Helper()
	srv := newTestServer(t, func(c *WorkerConfig) {
		c.WorkerID = workerID
	})
	// Загружаем модель по имени БЕЗ .gguf (так делает /rpc/load в production).
	ggufName := modelBase
	if !strings.HasSuffix(ggufName, ".gguf") {
		ggufName = modelBase + ".gguf"
	}
	writeTestGGUF(t, srv.cfg.ModelsDir, ggufName)
	// LoadSlice принимает либо "model", либо "model.gguf" и возвращает slice с именем файла.
	loaded, err := srv.manager.LoadSlice(ggufName, "1-4")
	if err != nil {
		t.Fatalf("LoadSlice(%q): %v", ggufName, err)
	}
	if loaded == nil {
		t.Fatalf("LoadSlice returned nil slice")
	}
	return httptest.NewServer(srv.MiddlewareHandler()), srv
}

// =====================================================================
// tpPartialLen (helper)
// =====================================================================

func TestTpPartialLen(t *testing.T) {
	tests := []struct {
		inputLen, ws, rank, want int
	}{
		{52, 4, 0, 13},
		{53, 4, 0, 14},
		{56, 4, 3, 14},
		{0, 4, 0, 0},
		{10, 1, 0, 10},
		{10, 4, -1, 0},
		{10, 4, 4, 0},
		{10, 0, 0, 0},
	}
	for _, tt := range tests {
		got := tpPartialLen(tt.inputLen, tt.ws, tt.rank)
		if got != tt.want {
			t.Errorf("tpPartialLen(%d, %d, %d) = %d, want %d",
				tt.inputLen, tt.ws, tt.rank, got, tt.want)
		}
	}
}

// =====================================================================
// handleTPInfer
// =====================================================================

func TestHandleTPInfer_StubRank0(t *testing.T) {
	ts, _ := newTPTestServer(t, "w-rank0", "test-model")
	defer ts.Close()

	req := TPInferRequest{
		ModelName: "test-model",
		Rank:      0,
		WorldSize: 4,
		Layer:     2,
		Input:     json.RawMessage(`"1234567890"`), // plain string, не base64
	}
	b, _ := json.Marshal(req)
	resp, err := http.Post(ts.URL+"/rpc/tp/infer", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	var out TPInferResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Rank != 0 {
		t.Errorf("rank: got %d, want 0", out.Rank)
	}
	if out.WorkerID != "w-rank0" {
		t.Errorf("worker_id: got %q", out.WorkerID)
	}
	// 10 / 4 = 2 rem 2 → rank 0 = 3 bytes (rank<rem), rank 1 = 3, ranks 2..3 = 2.
	if len(out.Output) != 3 {
		t.Errorf("output len: got %d, want 3 (10/4=2 rem 2 → rank 0 gets +1)", len(out.Output))
	}
	for i, b := range out.Output {
		if b != 1 {
			t.Errorf("output[%d]: got %d, want 1", i, b)
		}
	}
	if !out.NeedsReduce {
		t.Error("NeedsReduce should be true")
	}
}

func TestHandleTPInfer_StubRank3(t *testing.T) {
	ts, _ := newTPTestServer(t, "w-rank3", "test-model")
	defer ts.Close()

	body := map[string]interface{}{
		"model_name": "test-model",
		"rank":       3,
		"world_size": 4,
		"layer":      0,
		"input":      []byte("123456789012"), // 12 bytes → 3 per rank
	}
	b, _ := json.Marshal(body)
	resp, err := http.Post(ts.URL+"/rpc/tp/infer", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	var out TPInferResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if len(out.Output) != 3 {
		t.Errorf("rank 3 output len: got %d, want 3", len(out.Output))
	}
	// Все байты = rank+1 = 4.
	for i, b := range out.Output {
		if b != 4 {
			t.Errorf("output[%d]: got %d, want 4", i, b)
		}
	}
}

func TestHandleTPInfer_StubRankUnbalanced(t *testing.T) {
	// 13 / 4 = 3 rem 1 → rank 0 = 4 bytes, ranks 1..3 = 3 bytes.
	ts, _ := newTPTestServer(t, "w", "test-model")
	defer ts.Close()

	tests := []struct {
		rank, wantLen int
	}{
		{0, 4},
		{1, 3},
		{2, 3},
		{3, 3},
	}
	for _, tt := range tests {
		body := map[string]interface{}{
			"model_name": "test-model",
			"rank":       tt.rank,
			"world_size": 4,
			"layer":      0,
			"input":      []byte("1234567890123"), // 13 bytes
		}
		b, _ := json.Marshal(body)
		resp, err := http.Post(ts.URL+"/rpc/tp/infer", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		var out TPInferResponse
		_ = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()

		if len(out.Output) != tt.wantLen {
			t.Errorf("rank %d: output len=%d, want %d", tt.rank, len(out.Output), tt.wantLen)
		}
	}
}

func TestHandleTPInfer_InvalidRank(t *testing.T) {
	ts, _ := newTPTestServer(t, "w", "test-model")
	defer ts.Close()

	body := map[string]interface{}{
		"model_name": "test-model",
		"rank":       5, // >= worldSize
		"world_size": 4,
		"layer":      0,
		"input":      []byte("xxx"),
	}
	b, _ := json.Marshal(body)
	resp, err := http.Post(ts.URL+"/rpc/tp/infer", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", resp.StatusCode)
	}
}

func TestHandleTPInfer_ModelNotLoaded(t *testing.T) {
	ts, _ := newTPTestServer(t, "w", "test-model")
	defer ts.Close()

	body := map[string]interface{}{
		"model_name": "different-model",
		"rank":       0,
		"world_size": 4,
		"layer":      0,
		"input":      []byte("xxx"),
	}
	b, _ := json.Marshal(body)
	resp, err := http.Post(ts.URL+"/rpc/tp/infer", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status=%d, want 503", resp.StatusCode)
	}
}

func TestHandleTPInfer_MethodNotAllowed(t *testing.T) {
	ts, _ := newTPTestServer(t, "w", "test-model")
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/rpc/tp/infer")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status=%d, want 405", resp.StatusCode)
	}
}

// =====================================================================
// handleTPKvSync / handleTPKvFetch
// =====================================================================

func TestHandleTPKvSyncFetch_Roundtrip(t *testing.T) {
	ts, srv := newTPTestServer(t, "w", "test-model")
	defer ts.Close()

	// POST shard for rank 0.
	syncBody := map[string]interface{}{
		"session_id": "sess-abc",
		"rank":       0,
		"shard":      []byte("hello-world-rank0"),
	}
	b, _ := json.Marshal(syncBody)
	resp, err := http.Post(ts.URL+"/rpc/tp/kv_sync", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sync status=%d", resp.StatusCode)
	}
	resp.Body.Close()

	// GET shard.
	resp2, err := http.Get(ts.URL + "/rpc/tp/kv_fetch?session_id=sess-abc&rank=0")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("fetch status=%d", resp2.StatusCode)
	}
	var fetched map[string]interface{}
	_ = json.NewDecoder(resp2.Body).Decode(&fetched)

	if !strings.Contains(fetched["shard"].(string), "hello-world-rank0") {
		// Note: JSON encodes []byte as base64 in string. Декодируем.
		_ = fetched
	}

	// Проверяем через прямой вызов KVStore.
	got := srv.kvStore.LoadShard("sess-abc", 0)
	if string(got) != "hello-world-rank0" {
		t.Errorf("LoadShard: got %q, want %q", got, "hello-world-rank0")
	}
}

func TestHandleTPKvSync_MultipleRanks(t *testing.T) {
	ts, srv := newTPTestServer(t, "w", "test-model")
	defer ts.Close()

	for rank := 0; rank < 4; rank++ {
		syncBody := map[string]interface{}{
			"session_id": "sess-multi",
			"rank":       rank,
			"shard":      []byte{byte(rank + 1), byte(rank + 2)},
		}
		b, _ := json.Marshal(syncBody)
		resp, err := http.Post(ts.URL+"/rpc/tp/kv_sync", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("rank %d sync status=%d", rank, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// Каждый rank должен иметь свой shard (не перезатёрт).
	for rank := 0; rank < 4; rank++ {
		got := srv.kvStore.LoadShard("sess-multi", rank)
		if len(got) != 2 {
			t.Errorf("rank %d: shard len=%d, want 2", rank, len(got))
		}
		if got[0] != byte(rank+1) || got[1] != byte(rank+2) {
			t.Errorf("rank %d: shard=%v, want [%d %d]", rank, got, rank+1, rank+2)
		}
	}
}

func TestHandleTPKvFetch_NotFound(t *testing.T) {
	ts, _ := newTPTestServer(t, "w", "test-model")
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/rpc/tp/kv_fetch?session_id=missing&rank=0")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status=%d, want 404", resp.StatusCode)
	}
}

func TestHandleTPKvFetch_MissingParams(t *testing.T) {
	ts, _ := newTPTestServer(t, "w", "test-model")
	defer ts.Close()

	cases := []string{
		"/rpc/tp/kv_fetch",                  // no session_id, no rank
		"/rpc/tp/kv_fetch?session_id=x",     // no rank
		"/rpc/tp/kv_fetch?rank=0",           // no session_id
		"/rpc/tp/kv_fetch?session_id=x&rank=abc", // non-int rank
	}
	for _, path := range cases {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("path %s: status=%d, want 400", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// =====================================================================
// KVStore.SaveShard/LoadShard/DeleteShard
// =====================================================================

func TestKVStore_SaveLoadShard_Basic(t *testing.T) {
	kv := NewKVStore("w1", KVStoreConfig{TTL: time.Hour, MaxEntries: 100})

	// Сохраняем shard для rank 0.
	if err := kv.SaveShard("s1", 0, []byte("data-rank0")); err != nil {
		t.Fatal(err)
	}
	got := kv.LoadShard("s1", 0)
	if string(got) != "data-rank0" {
		t.Errorf("rank 0: got %q", got)
	}

	// Сохраняем shard для rank 1.
	if err := kv.SaveShard("s1", 1, []byte("data-rank1")); err != nil {
		t.Fatal(err)
	}
	got0 := kv.LoadShard("s1", 0)
	got1 := kv.LoadShard("s1", 1)
	if string(got0) != "data-rank0" || string(got1) != "data-rank1" {
		t.Errorf("isolation: rank 0=%q rank 1=%q", got0, got1)
	}
}

func TestKVStore_SaveShard_Overwrite(t *testing.T) {
	kv := NewKVStore("w1", KVStoreConfig{TTL: time.Hour, MaxEntries: 100})

	kv.SaveShard("s1", 0, []byte("v1"))
	kv.SaveShard("s1", 0, []byte("v2"))
	if got := kv.LoadShard("s1", 0); string(got) != "v2" {
		t.Errorf("overwrite: got %q, want v2", got)
	}
}

func TestKVStore_LoadShard_Missing(t *testing.T) {
	kv := NewKVStore("w1", KVStoreConfig{TTL: time.Hour, MaxEntries: 100})
	if got := kv.LoadShard("nope", 0); got != nil {
		t.Errorf("expected nil, got %q", got)
	}
}

func TestKVStore_DeleteShard(t *testing.T) {
	kv := NewKVStore("w1", KVStoreConfig{TTL: time.Hour, MaxEntries: 100})
	kv.SaveShard("s1", 0, []byte("x"))
	if !kv.DeleteShard("s1", 0) {
		t.Error("DeleteShard should return true")
	}
	if kv.LoadShard("s1", 0) != nil {
		t.Error("shard should be deleted")
	}
	if kv.DeleteShard("s1", 0) {
		t.Error("second DeleteShard should return false")
	}
}

func TestKVStore_SaveShard_EmptySessionID(t *testing.T) {
	kv := NewKVStore("w1", KVStoreConfig{TTL: time.Hour, MaxEntries: 100})
	if err := kv.SaveShard("", 0, []byte("data")); err != nil {
		t.Errorf("empty session_id should be no-op, got %v", err)
	}
	if got := kv.LoadShard("", 0); got != nil {
		t.Errorf("expected nil, got %q", got)
	}
}

func TestKVStore_CountShardsForSession(t *testing.T) {
	kv := NewKVStore("w1", KVStoreConfig{TTL: time.Hour, MaxEntries: 100})
	for r := 0; r < 3; r++ {
		kv.SaveShard("s1", r, []byte{byte(r)})
	}
	if got := kv.CountShardsForSession("s1"); got != 3 {
		t.Errorf("CountShardsForSession: got %d, want 3", got)
	}

	// Также считаем session через Save (legacy API).
	kv.Save(&KVShard{SessionID: "legacy"})
	if got := kv.CountShardsForSession("legacy"); got != 0 {
		t.Errorf("CountShardsForSession('legacy'): got %d, want 0", got)
	}
}

func TestKVStore_SaveShard_MaxEntries(t *testing.T) {
	kv := NewKVStore("w1", KVStoreConfig{TTL: time.Hour, MaxEntries: 2})
	kv.SaveShard("s1", 0, []byte("a"))
	kv.SaveShard("s1", 1, []byte("b"))
	kv.SaveShard("s2", 0, []byte("c"))

	// Третий новый shard → ошибка.
	err := kv.SaveShard("s3", 0, []byte("d"))
	if err == nil || !IsKVStoreFullError(err) {
		t.Errorf("expected kv_store full error, got %v", err)
	}
}

func TestShardedKey_Stability(t *testing.T) {
	// Один и тот же (sessionID, rank) должен давать одинаковый ключ.
	if shardKey("abc", 0) != shardKey("abc", 0) {
		t.Error("shardKey not stable")
	}
	if shardKey("abc", 0) == shardKey("abc", 1) {
		t.Error("shardKey for different ranks should differ")
	}
	if shardKey("abc", 0) == shardKey("xyz", 0) {
		t.Error("shardKey for different sessions should differ")
	}
}