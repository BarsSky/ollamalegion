package sdbackend

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// ============================================================
// Мок sd-server (тестовый)
// ============================================================
//
// ЗАЧЕМ ПОЛНОЦЕННЫЙ МОК, А НЕ ПАРА ЗАГЛУШЕК: контракт движка нетривиален
// (202 + job id → poll → result.images[].b64_json, лимиты в capabilities,
// 409 при отмене generating). Тесты обязаны проверять, что мы правильно
// читаем ИМЕННО эту форму, иначе «зелёные» тесты ничего не доказывают.

// tinyPNG — валидный PNG 1x1 (base64). Content не важен: проверяется, что
// байты доехали из движка до клиента без искажений.
const tinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg=="

// recordedImgGen — записанный запрос img_gen (для проверок нормализации).
type recordedImgGen struct {
	Prompt         string         `json:"prompt"`
	NegativePrompt string         `json:"negative_prompt"`
	Width          int            `json:"width"`
	Height         int            `json:"height"`
	Seed           int64          `json:"seed"`
	BatchCount     int            `json:"batch_count"`
	ClipSkip       *int           `json:"clip_skip"`
	OutputFormat   string         `json:"output_format"`
	SampleParams   map[string]any `json:"sample_params"`
	Raw            map[string]any `json:"-"`
}

// fakepServer — мок sd-server.
type fakepServer struct {
	*httptest.Server
	mu sync.Mutex
	// requests — все принятые img_gen.
	requests []recordedImgGen
	// caps — ответ capabilities (можно подменить, например limits).
	caps map[string]any
	// failSubmit — сделать POST /img_gen ошибкой (status + тело).
	failSubmitStatus int
	failSubmitBody   string
	// failJob — завершить джобу статусом failed.
	failJob bool
	// generatingForever — джоба навсегда в generating (для cancel-тестов).
	generatingForever bool
	// jobs — статусы джоб.
	jobs map[string]string
	// cancels — какие джобы пытались отменить.
	cancels []string
	// delayedReady — сколько запросов capabilities должны вернуть 404 до успеха
	// (эмуляция «процесс поднялся, модель ещё грузится»).
	notReadyCount int
	// capabilityCalls — сколько раз спросили capabilities.
	capabilityCalls int
	// imgDelay — задержка выдачи результата (эмуляция генерации).
	imgDelay time.Duration
	// pendingImages — сколько картинок отдавать (0 = по batch_count).
	pendingImages int
}

func newFakeServer() *fakepServer {
	f := &fakepServer{
		jobs: map[string]string{},
		caps: map[string]any{
			"model":         map[string]any{"name": "fake-model", "stem": "fake-model", "path": "/tmp/fake.gguf"},
			"current_mode":  "img_gen",
			"supported_modes": []string{"img_gen"},
			"samplers":      []string{"euler", "euler_a", "dpm++2m", "lcm"},
			"schedulers":    []string{"discrete", "karras", "smoothstep"},
			"loras":         []map[string]any{{"name": "detail", "path": "detail.safetensors"}},
			"upscalers":     []map[string]any{{"name": "Lanczos", "model": false, "image_upscale": false}},
			"features_by_mode": map[string]any{
				"img_gen": map[string]any{"cancel_queued": true, "cancel_generating": false},
			},
			"limits": map[string]any{
				"min_width": 64, "max_width": 4096, "min_height": 64, "max_height": 4096,
				"max_batch_count": 8, "max_queue_size": 64,
				"max_upscale_width": 8192, "max_upscale_height": 8192,
			},
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/sdcpp/v1/capabilities", f.handleCapabilities)
	mux.HandleFunc("/sdcpp/v1/img_gen", f.handleImgGen)
	mux.HandleFunc("/sdcpp/v1/jobs/", f.handleJob)
	// OpenAI/A1111-ветки движка (нужны только чтобы проверить, что воркер их
	// НЕ использует как основной путь).
	mux.HandleFunc("/v1/images/generations", f.handleOpenAICompat)
	mux.HandleFunc("/sdapi/v1/txt2img", f.handleOpenAICompat)
	f.Server = httptest.NewServer(mux)
	return f
}

func (f *fakepServer) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.capabilityCalls++
	notReady := f.notReadyCount
	if notReady > 0 {
		f.notReadyCount--
	}
	f.mu.Unlock()
	if notReady > 0 {
		// Движок уже слушает, но модель ещё грузится: cap-запрос не отвечает.
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"model is loading"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(f.caps)
}

func (f *fakepServer) handleImgGen(w http.ResponseWriter, r *http.Request) {
	var rec recordedImgGen
	body := map[string]any{}
	_ = json.NewDecoder(r.Body).Decode(&body)
	raw, _ := json.Marshal(body)
	_ = json.Unmarshal(raw, &rec)
	rec.Raw = body

	f.mu.Lock()
	f.requests = append(f.requests, rec)
	if f.failSubmitStatus != 0 {
		status, text := f.failSubmitStatus, f.failSubmitBody
		f.mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(text))
		return
	}
	id := fmt.Sprintf("job_%d", len(f.requests))
	f.jobs[id] = JobStatusQueued
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": id, "kind": "img_gen", "status": JobStatusQueued,
		"created": time.Now().Unix(), "poll_url": "/sdcpp/v1/jobs/" + id,
	})
}

func (f *fakepServer) handleJob(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/sdcpp/v1/jobs/")
	if strings.HasSuffix(rest, "/cancel") {
		id := strings.TrimSuffix(rest, "/cancel")
		f.mu.Lock()
		f.cancels = append(f.cancels, id)
		status := f.jobs[id]
		f.mu.Unlock()
		if status == JobStatusGenerating {
			// Ровно то поведение движка, которое мы обязаны пробросить как есть.
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"job is currently generating and cannot be interrupted yet"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"cancelled"}`))
		return
	}
	id := rest
	f.mu.Lock()
	status := f.jobs[id]
	failJob := f.failJob
	forever := f.generatingForever
	delay := f.imgDelay
	imgCount := f.pendingImages
	f.mu.Unlock()

	if status == "" {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"job not found"}`))
		return
	}
	if delay > 0 {
		time.Sleep(delay)
	}
	if status == JobStatusQueued {
		if forever {
			status = JobStatusGenerating
		} else {
			status = JobStatusCompleted
		}
		f.mu.Lock()
		f.jobs[id] = status
		f.mu.Unlock()
	}
	resp := map[string]any{"id": id, "kind": "img_gen", "status": status, "queue_position": 0}
	switch {
	case status == JobStatusFailed || failJob:
		resp["status"] = JobStatusFailed
		resp["error"] = map[string]any{"code": "generation_failed", "message": "generate_image returned empty results"}
	case status == JobStatusGenerating:
		// ничего не добавляем — клиент продолжит поллинг
	default:
		if imgCount <= 0 {
			imgCount = 1
			// Уважаем batch_count, если он есть в последнем запросе.
			f.mu.Lock()
			if n := len(f.requests); n > 0 && f.requests[n-1].BatchCount > 1 {
				imgCount = f.requests[n-1].BatchCount
			}
			f.mu.Unlock()
		}
		images := make([]map[string]any, 0, imgCount)
		for i := 0; i < imgCount; i++ {
			images = append(images, map[string]any{"index": i, "b64_json": tinyPNG})
		}
		resp["result"] = map[string]any{"output_format": "png", "images": images}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleOpenAICompat — OpenAI/A1111-ветки движка. Воркер обязан ходить в
// /sdcpp/v1/*, поэтому этот хендлер просто считает вызовы (тест проверяет,
// что их нет).
func (f *fakepServer) handleOpenAICompat(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, recordedImgGen{Prompt: "__LEGACY_PATH_USED__"})
	f.mu.Unlock()
	writeJSONish(w, http.StatusOK, map[string]any{"data": []any{}, "images": []any{}})
}

func writeJSONish(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// --- доступ к записям ---

func (f *fakepServer) lastRequest() (recordedImgGen, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return recordedImgGen{}, false
	}
	return f.requests[len(f.requests)-1], true
}

func (f *fakepServer) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakepServer) cancelCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.cancels)
}

func (f *fakepServer) setJobStatus(id, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs[id] = status
}

func (f *fakepServer) hostPort() (string, int) {
	addr := f.Listener.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port
}

// decodePNG — проверка, что b64 действительно декодируется (тесты результата).
func decodePNG(b64 string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(b64)
}

// ============================================================
// FakeProcess — подставной субпроцесс (без реального spawn)
// ============================================================
//
// ПОЧЕМУ ТАК: запускать настоящий sd-server в unit-тестах нельзя (его нет в
// окружении, а падение процесса не детерминировано). FakeProcess «поднимает»
// httptest-сервер мока на порту, который ждёт супервизор, — то есть readiness
// проверяется ЧЕРЕЗ РЕАЛЬНЫЙ HTTP И TCP, а не через заглушку клиента.

// FakeProcess — реализация Process.
type FakeProcess struct {
	mu      sync.Mutex
	pid     int
	output  []string
	exitErr error
	exited  bool
	stopped bool
	done    chan struct{}
	onStop  func()
}

// NewFakeProcess — процесс, который «живёт» до Stop/Kill.
func NewFakeProcess(pid int, onStop func()) *FakeProcess {
	return &FakeProcess{pid: pid, done: make(chan struct{}), onStop: onStop}
}

func (p *FakeProcess) AppendOutput(line string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.output = append(p.output, line)
}

func (p *FakeProcess) Output() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.Join(p.output, "\n")
}

func (p *FakeProcess) PID() int { return p.pid }

func (p *FakeProcess) Wait() error {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitErr
}

func (p *FakeProcess) Stop(time.Duration) error {
	p.finish(nil)
	return nil
}

func (p *FakeProcess) Kill() error {
	p.finish(nil)
	return nil
}

// Exited — для readiness-поллера (exited() в supervisor.go).
func (p *FakeProcess) Exited() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exited
}

func (p *FakeProcess) finish(err error) {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	p.stopped = true
	p.exited = true
	p.exitErr = err
	cb := p.onStop
	p.mu.Unlock()
	close(p.done)
	if cb != nil {
		cb()
	}
}

// Crash — эмуляция падения процесса (например OOM VAE).
func (p *FakeProcess) Crash(err error) { p.finish(err) }

// FakeRunner — ProcessRunner, отдающий заранее подготовленные процессы.
type FakeRunner struct {
	mu      sync.Mutex
	argvs   [][]string
	next    func(argv []string) (Process, error)
	started int
	last    *FakeProcess
}

// NewFakeRunner — раннер, который вызывает fn на каждый Start.
func NewFakeRunner(fn func(argv []string) (Process, error)) *FakeRunner {
	return &FakeRunner{next: fn}
}

func (r *FakeRunner) Start(_ context.Context, argv []string) (Process, error) {
	r.mu.Lock()
	r.argvs = append(r.argvs, append([]string{}, argv...))
	r.started++
	fn := r.next
	r.mu.Unlock()
	if fn == nil {
		return nil, fmt.Errorf("no fake process configured")
	}
	p, err := fn(argv)
	if fp, ok := p.(*FakeProcess); ok {
		r.mu.Lock()
		r.last = fp
		r.mu.Unlock()
	}
	return p, err
}

// lastProcess — последний созданный FakeProcess (для теста «внезапная смерть»).
func (r *FakeRunner) lastProcess() *FakeProcess {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

func (r *FakeRunner) Argvs() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, 0, len(r.argvs))
	for _, a := range r.argvs {
		out = append(out, append([]string{}, a...))
	}
	return out
}

func (r *FakeRunner) Started() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.started
}
