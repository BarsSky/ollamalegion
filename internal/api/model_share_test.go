// model_share_test.go — R89 (2026-10-08): оркестрация переноса модели между
// бэкендами (балансер стримит файл из источника в приёмник).
//
// Проверяем именно то, что нельзя проверить на воркере по отдельности:
//   - API создаёт задание (202) и отдаёт прогресс по /{id};
//   - поток идёт GET источника → POST приёмника БЕЗ буферизации на балансере;
//   - байты доходят до приёмника ровно те же;
//   - «модель уже есть на приёмнике» → skipped, а не повторная перекачка;
//   - ошибку источника видно в задании (состояние failed + текст), а не в тишине.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"
)

// shareMockWorker — мок воркера: отдаёт файл (export) и/или принимает импорт.
type shareMockWorker struct {
	srv *httptest.Server

	mu           sync.Mutex
	content      []byte   // что отдаёт export
	filenames    []string // инвентарь GET /api/models/files
	received     []byte   // что приняли на import
	importCount  int
	exportStatus int // 0 = 200
	importStatus int // 0 = 200
	exportCalls  int
}

func newShareMockWorker(t *testing.T, content []byte, filenames []string) *shareMockWorker {
	t.Helper()
	w := &shareMockWorker{content: content, filenames: filenames}
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/models/files":
			w.mu.Lock()
			files := append([]string{}, w.filenames...)
			w.mu.Unlock()
			items := make([]map[string]any, 0, len(files))
			for _, name := range files {
				items = append(items, map[string]any{"filename": name, "sizeBytes": len(w.content)})
			}
			writeJSONBody(rw, http.StatusOK, map[string]any{"files": items})
		case r.URL.Path == "/api/models/export":
			w.mu.Lock()
			w.exportCalls++
			status := w.exportStatus
			content := w.content
			w.mu.Unlock()
			if status != 0 && status != http.StatusOK {
				writeJSONBody(rw, status, map[string]any{"error": "mock source error", "code": "model_not_found"})
				return
			}
			rw.Header().Set("Content-Type", "application/octet-stream")
			rw.Header().Set("X-Model-Filename", "m.gguf")
			rw.Header().Set("X-Model-SizeBytes", strconv.Itoa(len(content)))
			rw.Header().Set("Content-Length", strconv.Itoa(len(content)))
			if r.Method == http.MethodHead {
				rw.WriteHeader(http.StatusOK)
				return
			}
			_, _ = rw.Write(content)
		case r.URL.Path == "/api/models/import" && r.Method == http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			w.mu.Lock()
			w.received = body
			w.importCount++
			status := w.importStatus
			w.mu.Unlock()
			if status != 0 && status != http.StatusOK {
				writeJSONBody(rw, status, map[string]any{"error": "mock target error"})
				return
			}
			writeJSONBody(rw, http.StatusOK, map[string]any{"filename": r.URL.Query().Get("filename"), "sizeBytes": len(body)})
		default:
			writeJSONBody(rw, http.StatusNotFound, map[string]any{"error": "unexpected path: " + r.URL.Path})
		}
	}))
	t.Cleanup(w.srv.Close)
	return w
}

func (w *shareMockWorker) port(t *testing.T) int {
	t.Helper()
	var port int
	if _, err := fmt.Sscanf(strings.TrimPrefix(w.srv.URL, "http://127.0.0.1:"), "%d", &port); err != nil {
		t.Fatalf("parse mock port from %s: %v", w.srv.URL, err)
	}
	return port
}

func (w *shareMockWorker) snapshot() (received []byte, imports int, exports int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte{}, w.received...), w.importCount, w.exportCalls
}

func writeJSONBody(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// newShareTestServer — балансер с двумя llama_cpp-бэкендами (источник и приёмник).
func newShareTestServer(t *testing.T, source, target *shareMockWorker) (*Server, *httptest.Server) {
	t.Helper()
	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 8080, APIPort: 8081},
		Backends: []types.Backend{
			{ID: "src", Name: "source", Host: "127.0.0.1", CppWorkerPort: source.port(t),
				Type: types.BackendTypeLlamaCpp, Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
			{ID: "dst", Name: "target", Host: "127.0.0.1", CppWorkerPort: target.port(t),
				Type: types.BackendTypeLlamaCpp, Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
		},
		Balancing: types.BalancingSettings{Algorithm: types.AlgorithmResourceAware, HealthCheckInterval: 10,
			MetricsInterval: 5, RequestTimeout: 30, QueueTimeout: 60, QueueMaxSize: 100, QueueWorkers: 4},
		API:  types.APISettings{RateLimit: 500, RateBurst: 1000},
		Auth: types.AuthConfig{Enabled: false},
	}
	proxy := balancer.NewProxy(config)
	server := NewServer(proxy, config, balancer.NewHealthChecker(proxy, 10*time.Second, 3))
	ts := httptest.NewServer(server)
	t.Cleanup(ts.Close)
	// Прокси держит фоновые циклы — гасим их вместе с тестом.
	t.Cleanup(func() { _ = proxy.Shutdown(context.Background()) })
	return server, ts
}

// startShare — POST /api/v1/models/share и разбор задания.
func startShare(t *testing.T, ts *httptest.Server, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(ts.URL+"/api/v1/models/share", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST share: %v", err)
	}
	defer resp.Body.Close()
	var job map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		t.Fatalf("decode share response: %v", err)
	}
	return resp.StatusCode, job
}

// waitShare — опрос GET /api/v1/models/share/{id} до завершения задания.
func waitShare(t *testing.T, ts *httptest.Server, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last map[string]any
	for time.Now().Before(deadline) {
		resp, err := http.Get(ts.URL + "/api/v1/models/share/" + id)
		if err != nil {
			t.Fatalf("GET share job: %v", err)
		}
		var job map[string]any
		derr := json.NewDecoder(resp.Body).Decode(&job)
		resp.Body.Close()
		if derr != nil {
			t.Fatalf("decode job: %v", derr)
		}
		last = job
		if state, _ := job["state"].(string); state != shareStateRunning {
			return job
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("задание не завершилось за 15 с: %v", last)
	return last
}

func firstTarget(t *testing.T, job map[string]any) map[string]any {
	t.Helper()
	targets, _ := job["targets"].([]any)
	if len(targets) == 0 {
		t.Fatalf("в задании нет целей: %v", job)
	}
	target, _ := targets[0].(map[string]any)
	return target
}

// TestModelShare_API_StreamsFileToTarget — главный сценарий: файл уехал на
// приёмник целиком, состояние задания — done.
func TestModelShare_API_StreamsFileToTarget(t *testing.T) {
	payload := bytes.Repeat([]byte("GGUF"), 80_000) // 320 000 байт
	src := newShareMockWorker(t, payload, []string{"m.gguf"})
	dst := newShareMockWorker(t, nil, nil)
	_, ts := newShareTestServer(t, src, dst)

	status, job := startShare(t, ts, `{"source":"src","model":"m.gguf","targets":["dst"]}`)
	if status != http.StatusAccepted {
		t.Fatalf("status=%d, want 202 (перенос асинхронный): %v", status, job)
	}
	id, _ := job["id"].(string)
	if id == "" {
		t.Fatalf("в ответе нет id задания: %v", job)
	}

	done := waitShare(t, ts, id)
	if done["state"] != shareStateDone {
		t.Fatalf("состояние задания = %v, want done (%v)", done["state"], done)
	}
	target := firstTarget(t, done)
	if target["state"] != shareTargetDone {
		t.Errorf("состояние цели = %v, want done (%v)", target["state"], target)
	}
	if got := int64(target["bytes"].(float64)); got != int64(len(payload)) {
		t.Errorf("перенесено %d байт, want %d", got, len(payload))
	}

	received, imports, _ := dst.snapshot()
	if !bytes.Equal(received, payload) {
		t.Errorf("приёмник получил %d байт, отличающихся от источника", len(received))
	}
	if imports != 1 {
		t.Errorf("импортов на приёмнике = %d, want 1", imports)
	}
}

// TestModelShare_API_SkipsWhenTargetAlreadyHasModel — если модель уже есть и
// перезапись не запрошена, гигабайты не гоняем.
func TestModelShare_API_SkipsWhenTargetAlreadyHasModel(t *testing.T) {
	payload := bytes.Repeat([]byte("A"), 1024)
	src := newShareMockWorker(t, payload, []string{"m.gguf"})
	dst := newShareMockWorker(t, nil, []string{"m.gguf"}) // модель уже лежит
	_, ts := newShareTestServer(t, src, dst)

	_, job := startShare(t, ts, `{"source":"src","model":"m.gguf","targets":["dst"]}`)
	done := waitShare(t, ts, job["id"].(string))
	if done["state"] != shareStateDone {
		t.Fatalf("состояние = %v, want done", done["state"])
	}
	target := firstTarget(t, done)
	if target["state"] != shareTargetSkipped {
		t.Errorf("состояние цели = %v, want skipped (%v)", target["state"], target)
	}
	if target["note"] == "" {
		t.Errorf("skipped без объяснения: %v", target)
	}
	if _, imports, _ := dst.snapshot(); imports != 0 {
		t.Errorf("импортов = %d, want 0 (перекачка не нужна)", imports)
	}
}

// TestModelShare_API_ReportsSourceError — ошибка источника видна в задании.
func TestModelShare_API_ReportsSourceError(t *testing.T) {
	src := newShareMockWorker(t, []byte("data"), []string{"m.gguf"})
	src.mu.Lock()
	src.exportStatus = http.StatusNotFound
	src.mu.Unlock()
	dst := newShareMockWorker(t, nil, nil)
	_, ts := newShareTestServer(t, src, dst)

	_, job := startShare(t, ts, `{"source":"src","model":"m.gguf","targets":["dst"]}`)
	done := waitShare(t, ts, job["id"].(string))
	if done["state"] != shareStateFailed {
		t.Fatalf("состояние = %v, want failed (%v)", done["state"], done)
	}
	errText, _ := done["error"].(string)
	if !strings.Contains(errText, "src") {
		t.Errorf("в ошибке нет имени источника: %q", errText)
	}
	if _, imports, _ := dst.snapshot(); imports != 0 {
		t.Errorf("при ошибке источника импорт всё равно случился")
	}
}

// TestModelShare_API_ReportsTargetError — отказ приёмника (например 409 на
// существующий файл без overwrite) попадает в состояние цели.
func TestModelShare_API_ReportsTargetError(t *testing.T) {
	payload := bytes.Repeat([]byte("B"), 2048)
	src := newShareMockWorker(t, payload, []string{"m.gguf"})
	dst := newShareMockWorker(t, nil, nil)
	dst.mu.Lock()
	dst.importStatus = http.StatusConflict
	dst.mu.Unlock()
	_, ts := newShareTestServer(t, src, dst)

	_, job := startShare(t, ts, `{"source":"src","model":"m.gguf","targets":["dst"]}`)
	done := waitShare(t, ts, job["id"].(string))
	if done["state"] != shareStatePartial {
		t.Fatalf("состояние = %v, want partial (%v)", done["state"], done)
	}
	target := firstTarget(t, done)
	if target["state"] != shareTargetFailed {
		t.Errorf("состояние цели = %v, want failed", target["state"])
	}
	if errText, _ := target["error"].(string); !strings.Contains(errText, "409") {
		t.Errorf("в ошибке цели нет HTTP-кода: %q", errText)
	}
}

// TestModelShare_API_ValidatesRequest — понятные отказы на некорректный запрос.
func TestModelShare_API_ValidatesRequest(t *testing.T) {
	src := newShareMockWorker(t, []byte("x"), []string{"m.gguf"})
	dst := newShareMockWorker(t, nil, nil)
	_, ts := newShareTestServer(t, src, dst)

	cases := []struct{ name, body string }{
		{"без source", `{"model":"m.gguf","targets":["dst"]}`},
		{"без targets", `{"source":"src","model":"m.gguf"}`},
		{"только источник как цель", `{"source":"src","model":"m.gguf","targets":["src"]}`},
		{"неизвестный приёмник", `{"source":"src","model":"m.gguf","targets":["nope"]}`},
	}
	for _, c := range cases {
		status, job := startShare(t, ts, c.body)
		if status == http.StatusAccepted {
			t.Errorf("%s: status=202, ожидался отказ (%v)", c.name, job)
			continue
		}
		if _, ok := job["error"]; !ok {
			t.Errorf("%s: в отказе нет поля error: %v", c.name, job)
		}
	}
}

// TestModelShare_API_CancelStopsRemainingTargets — отмена помечает оставшиеся
// цели, не оставляя задание «вечно running».
func TestModelShare_API_CancelStopsRemainingTargets(t *testing.T) {
	src := newShareMockWorker(t, bytes.Repeat([]byte("C"), 64), []string{"m.gguf"})
	dst := newShareMockWorker(t, nil, nil)
	_, ts := newShareTestServer(t, src, dst)

	_, job := startShare(t, ts, `{"source":"src","model":"m.gguf","targets":["dst"]}`)
	id := job["id"].(string)
	resp, err := http.Post(ts.URL+"/api/v1/models/share/"+id+"/cancel", "application/json", nil)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel: status=%d", resp.StatusCode)
	}
	var cancelled map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&cancelled)
	if cancelled["id"] != id {
		t.Errorf("отменено не то задание: %v", cancelled["id"])
	}
	// Задание либо успело завершиться (крошечный файл), либо отменено — но не
	// осталось в running навсегда.
	final := waitShare(t, ts, id)
	if state, _ := final["state"].(string); state == shareStateRunning {
		t.Errorf("после отмены задание осталось running")
	}
}
