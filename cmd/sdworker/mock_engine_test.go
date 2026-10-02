package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/internal/sdbackend"
)

// ============================================================
// Мок sd-server и подставной процесс для тестов cmd/sdworker
// ============================================================
//
// Тот же принцип, что в internal/sdbackend/fakeserver_test.go: readiness
// проверяется через настоящий HTTP-цикл, а «процесс» — через FakeRunner.
// Тесты здесь не дублируют контракт движка детально (это сделано в sdbackend),
// а проверяют HTTP-поверхность воркера.

// mockEngine — минимальный sd-server: capabilities + img_gen + jobs.
type mockEngine struct {
	*httptest.Server
	mu       sync.Mutex
	requests []map[string]any
	failJob  bool
	// generatingForever — джобы никогда не завершаются (для теста cancel 409).
	generatingForever bool
	jobs              map[string]string
}

func newMockEngine() *mockEngine {
	m := &mockEngine{jobs: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/sdcpp/v1/capabilities", func(w http.ResponseWriter, r *http.Request) {
		writeJSONish(w, http.StatusOK, map[string]any{
			"model":           map[string]any{"name": "mock", "stem": "mock", "path": "/mock.gguf"},
			"current_mode":    "img_gen",
			"supported_modes": []string{"img_gen"},
			"samplers":        []string{"euler", "euler_a", "dpm++2m"},
			"schedulers":      []string{"discrete", "karras"},
			"loras":           []any{},
			"upscalers":       []any{},
			"limits": map[string]any{
				"min_width": 64, "max_width": 4096, "min_height": 64, "max_height": 4096,
				"max_batch_count": 8, "max_queue_size": 64,
			},
			"features_by_mode": map[string]any{
				"img_gen": map[string]any{"cancel_queued": true, "cancel_generating": false},
			},
		})
	})
	mux.HandleFunc("/sdcpp/v1/img_gen", func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		m.mu.Lock()
		m.requests = append(m.requests, body)
		id := fmt.Sprintf("job_%d", len(m.requests))
		m.jobs[id] = sdbackend.JobStatusQueued
		m.mu.Unlock()
		writeJSONish(w, http.StatusAccepted, map[string]any{
			"id": id, "kind": "img_gen", "status": "queued",
			"created": time.Now().Unix(), "poll_url": "/sdcpp/v1/jobs/" + id,
		})
	})
	mux.HandleFunc("/sdcpp/v1/jobs/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/sdcpp/v1/jobs/")
		if strings.HasSuffix(rest, "/cancel") {
			m.mu.Lock()
			status := m.jobs[strings.TrimSuffix(rest, "/cancel")]
			m.mu.Unlock()
			if status == sdbackend.JobStatusGenerating {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"job is currently generating and cannot be interrupted yet"}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		m.mu.Lock()
		status := m.jobs[rest]
		forever := m.generatingForever
		fail := m.failJob
		m.mu.Unlock()
		if status == "" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"job not found"}`))
			return
		}
		if status == sdbackend.JobStatusQueued {
			if forever {
				status = sdbackend.JobStatusGenerating
			} else {
				status = sdbackend.JobStatusCompleted
			}
			m.mu.Lock()
			m.jobs[rest] = status
			m.mu.Unlock()
		}
		resp := map[string]any{"id": rest, "kind": "img_gen", "status": status, "queue_position": 0}
		if fail {
			resp["status"] = sdbackend.JobStatusFailed
			resp["error"] = map[string]any{"code": "generation_failed", "message": "boom"}
		} else if status == sdbackend.JobStatusCompleted {
			// Количество картинок — по batch_count последнего запроса: иначе
			// тест «верни ровно столько элементов data[], сколько сгенерировано»
			// не проверяет ничего.
			count := 1
			m.mu.Lock()
			if n := len(m.requests); n > 0 {
				if bc, ok := m.requests[n-1]["batch_count"].(float64); ok && int(bc) > 1 {
					count = int(bc)
				}
			}
			m.mu.Unlock()
			images := make([]map[string]any, 0, count)
			for i := 0; i < count; i++ {
				images = append(images, map[string]any{"index": i, "b64_json": tinyPNG})
			}
			resp["result"] = map[string]any{"output_format": "png", "images": images}
		}
		writeJSONish(w, http.StatusOK, resp)
	})
	m.Server = httptest.NewServer(mux)
	return m
}

// lastRequest — последний принятый img_gen.
func (m *mockEngine) lastRequest() mockImgGenRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.requests) == 0 {
		return mockImgGenRequest{}
	}
	raw, _ := json.Marshal(m.requests[len(m.requests)-1])
	var out mockImgGenRequest
	_ = json.Unmarshal(raw, &out)
	return out
}

type mockImgGenRequest struct {
	Prompt       string `json:"prompt"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	Seed         int64  `json:"seed"`
	BatchCount   int    `json:"batch_count"`
	ClipSkip     *int   `json:"clip_skip"`
	OutputFormat string `json:"output_format"`
}

// fakeRunner — ProcessRunner без реального spawn.
type fakeRunner struct{}

func (fakeRunner) Start(_ context.Context, _ []string) (sdbackend.Process, error) {
	return &fakeProcess{pid: 5555, done: make(chan struct{})}, nil
}

// fakeProcess — «живой» процесс до Stop/Kill.
type fakeProcess struct {
	pid    int
	done   chan struct{}
	once   sync.Once
	output []string
}

func (p *fakeProcess) PID() int { return p.pid }

func (p *fakeProcess) Wait() error {
	<-p.done
	return nil
}

func (p *fakeProcess) Stop(time.Duration) error {
	p.finish()
	return nil
}

func (p *fakeProcess) Kill() error {
	p.finish()
	return nil
}

func (p *fakeProcess) Exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *fakeProcess) Output() string { return "fake sd-server output" }

func (p *fakeProcess) finish() {
	p.once.Do(func() { close(p.done) })
}

// netListen — свободный порт (обёртка, чтобы не тянуть net в тест-файл хендлеров).
func netListen() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") }

// writeJSONish — локальный хелпер мока (не используем хендлерный writeJSON,
// чтобы мок не зависел от продового кода).
func writeJSONish(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
