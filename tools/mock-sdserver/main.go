// Command mock-sdserver — минимальный мок stable-diffusion.cpp (`sd-server`)
// для живых E2E-проверок image-цепочки без сборки самого движка.
//
// Зачем отдельный бинарь, а не httptest: супервизор sdworker'а СПАВНИТ движок
// как субпроцесс и передаёт ему argv (--listen-ip/--listen-port/--model/...),
// поэтому мок обязан (а) принимать эти аргументы, (б) слушать реальный порт,
// (в) отдавать настоящий нативный async API (`/sdcpp/v1/*`). httptest так не
// умеет — он живёт в тестовом процессе.
//
// Использование:
//
//	go run ./tools/mock-sdserver --listen-port 19093
//	# затем: SDWORKER_SD_SERVER_BIN=<путь к собранному mock-sdserver>
//
// Полезные переменные:
//
//	MOCK_SD_GENERATION_MS — сколько «генерировать» картинку (по умолчанию 300 мс)
//	MOCK_SD_SEED_FIXED    — если 1, всегда возвращать один и тот же seed (эмуляция
//	                        ловушки sd.cpp: OpenAI-путь не читает seed и берёт 42)
//	MOCK_SD_FAIL_SPAWN    — если 1, процесс падает на старте (проверка классификации
//	                        ошибок супервизором)
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// b64PNG1x1 — валидный PNG 1x1 в base64 (то, что клиенты ожидают в b64_json).
const b64PNG1x1 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

type job struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Status    string    `json:"status"`
	Created   int64     `json:"created"`
	Started   *int64    `json:"started"`
	Completed *int64    `json:"completed"`
	Result    any       `json:"result"`
	Error     any       `json:"error"`
	readyAt   time.Time `json:"-"`
}

type server struct {
	mu        sync.Mutex
	jobs      map[string]*job
	seq       int
	model     string
	genDelay  time.Duration
	fixedSeed bool
}

func main() {
	// Аргументы парсим вручную: супервизор передаёт флаги самого sd.cpp
	// (--model/--vae/--diffusion-fa/...), которых мок не знает, и стандартный
	// flag.Parse на них бы упал.
	port := 1234
	model := "sd-cpp-local"
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--listen-port", "-port":
			if i+1 < len(args) {
				if v, err := strconv.Atoi(args[i+1]); err == nil {
					port = v
				}
				i++
			}
		case "--listen-ip", "--host":
			i++ // адрес игнорируем: слушаем на всех интерфейсах
		case "--model", "--diffusion-model":
			if i+1 < len(args) {
				model = strings.TrimSuffix(baseName(args[i+1]), ".gguf")
				i++
			}
		}
	}

	if os.Getenv("MOCK_SD_FAIL_SPAWN") == "1" {
		fmt.Fprintln(os.Stderr, "error: unknown argument --mock-fail (simulated sd-server spawn failure)")
		os.Exit(1)
	}

	genDelay := 300 * time.Millisecond
	if v := os.Getenv("MOCK_SD_GENERATION_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			genDelay = time.Duration(n) * time.Millisecond
		}
	}

	s := &server{
		jobs:      make(map[string]*job),
		model:     model,
		genDelay:  genDelay,
		fixedSeed: os.Getenv("MOCK_SD_SEED_FIXED") == "1",
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/sdcpp/v1/capabilities", s.handleCapabilities)
	mux.HandleFunc("/sdcpp/v1/img_gen", s.handleImgGen)
	mux.HandleFunc("/sdcpp/v1/jobs/", s.handleJob)
	mux.HandleFunc("/v1/images/generations", s.handleOpenAIImages)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/sdapi/v1/txt2img", s.handleTxt2Img)
	mux.HandleFunc("/", s.handleFallback)

	addr := fmt.Sprintf("0.0.0.0:%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("mock-sdserver: listen %s: %v", addr, err)
	}
	log.Printf("mock-sdserver: listening on %s (model=%q, generation=%s, fixedSeed=%v)", addr, model, genDelay, s.fixedSeed)
	if err := http.Serve(ln, logRequests(mux)); err != nil {
		log.Fatalf("mock-sdserver: serve: %v", err)
	}
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("mock-sdserver: %s %s", r.Method, r.URL.RequestURI())
		next.ServeHTTP(w, r)
	})
}

func (s *server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"model": map[string]any{
			"name": "sd-cpp-local",
			"stem": s.model,
			"path": "/mock/" + s.model + ".gguf",
		},
		"current_mode":    "img_gen",
		"supported_modes": []string{"img_gen"},
		"samplers":        []string{"euler", "euler_a", "dpm++2m", "lcm", "ddim_trailing"},
		"schedulers":      []string{"discrete", "karras", "exponential", "smoothstep"},
		"loras":           []any{},
		"upscalers":       []any{},
		"limits": map[string]any{
			"min_width": 64, "max_width": 4096,
			"min_height": 64, "max_height": 4096,
			"max_batch_count": 8, "max_queue_size": 64,
		},
	})
}

func (s *server) handleImgGen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)

	s.mu.Lock()
	s.seq++
	id := fmt.Sprintf("job_mock_%d", s.seq)
	now := time.Now().Unix()
	j := &job{
		ID:      id,
		Kind:    "img_gen",
		Status:  "queued",
		Created: now,
		readyAt: time.Now().Add(s.genDelay),
	}
	s.jobs[id] = j
	s.mu.Unlock()

	log.Printf("mock-sdserver: img_gen accepted id=%s prompt=%q%s", id, truncate(fmt.Sprint(body["prompt"]), 60), imgGenSummary(body))
	writeJSON(w, http.StatusAccepted, map[string]any{
		"id": id, "kind": "img_gen", "status": "queued", "created": now,
		"poll_url": "/sdcpp/v1/jobs/" + id,
	})
}

func (s *server) handleJob(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/sdcpp/v1/jobs/")
	if strings.HasSuffix(id, "/cancel") {
		id = strings.TrimSuffix(id, "/cancel")
		s.mu.Lock()
		j, ok := s.jobs[id]
		if ok && j.Status == "queued" {
			j.Status = "cancelled"
		}
		status := j.Status
		s.mu.Unlock()
		switch {
		case !ok:
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "job not found"})
		case status == "generating":
			// Как настоящий sd-server: отмена в полёте невозможна.
			writeJSON(w, http.StatusConflict, map[string]any{"error": "job is currently generating and cannot be interrupted yet"})
		default:
			writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": status})
		}
		return
	}

	s.mu.Lock()
	j, ok := s.jobs[id]
	if !ok {
		s.mu.Unlock()
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "job not found"})
		return
	}
	if (j.Status == "queued" || j.Status == "generating") && time.Now().After(j.readyAt) {
		now := time.Now().Unix()
		j.Status = "completed"
		j.Started = &now
		j.Completed = &now
		j.Result = map[string]any{
			"output_format": "png",
			"images": []map[string]any{
				{"index": 0, "b64_json": b64PNG1x1},
			},
		}
	}
	if j.Status == "queued" {
		j.Status = "generating"
	}
	snapshot := *j
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, &snapshot)
}

func (s *server) handleOpenAIImages(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	if strings.TrimSpace(fmt.Sprint(body["prompt"])) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "prompt is required"})
		return
	}
	// Настоящий sd-server отдаёт ТОЛЬКО b64_json (никаких url).
	writeJSON(w, http.StatusOK, map[string]any{
		"created":       time.Now().Unix(),
		"output_format": "png",
		"data":          []map[string]any{{"b64_json": b64PNG1x1}},
	})
}

func (s *server) handleModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data": []map[string]any{
			{"id": "sd-cpp-local", "object": "model", "created": time.Now().Unix(), "owned_by": "local"},
		},
	})
}

func (s *server) handleTxt2Img(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)

	seed := int64(42)
	if s.fixedSeed {
		// Эмуляция ловушки: OpenAI-путь sd.cpp не читает seed и берёт 42.
		seed = 42
	} else if v, ok := body["seed"].(float64); ok && int64(v) >= 0 {
		seed = int64(v)
	} else {
		seed = time.Now().UnixNano() % 1_000_000
	}
	width := jsonInt(body["width"], 512)
	height := jsonInt(body["height"], 512)
	steps := jsonInt(body["steps"], 20)

	info, _ := json.Marshal(map[string]any{
		"width": width, "height": height, "seed": seed, "steps": steps,
		"sampler_name": fmt.Sprint(body["sampler_name"]),
		"infotexts":    []string{fmt.Sprint(body["prompt"])},
		"all_seeds":    []int64{seed},
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"images":     []string{b64PNG1x1},
		"parameters": body,
		"info":       string(info),
	})
}

func (s *server) handleFallback(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotFound, map[string]any{"error": "mock-sdserver: unknown path " + r.URL.Path})
}

// --- helpers ---------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func jsonInt(v any, def int) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return def
}

func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// imgGenSummary — компактная сводка img2img-полей для лога.
//
// R-Image (2026-10-02): нужна живому E2E, чтобы доказать, что init_image/mask/
// strength доехали до движка, а не потерялись в воркере. Сам мок эти поля
// игнорирует (картинку не генерирует), поэтому лог — единственное наблюдаемое
// доказательство на проводе.
func imgGenSummary(body map[string]any) string {
	yesNo := func(v any) string {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			return "yes"
		}
		return "no"
	}
	strength := "n/a"
	if f, ok := body["strength"].(float64); ok {
		strength = strconv.FormatFloat(f, 'f', -1, 64)
	}
	batch := 1
	if f, ok := body["batch_count"].(float64); ok && f > 0 {
		batch = int(f)
	}
	// width/height могут лежать и в корне, и внутри sample_params запроса — мок
	// смотрит только корень (этого достаточно для сводки).
	return fmt.Sprintf(" init=%s mask=%s strength=%s batch=%d",
		yesNo(body["init_image"]), yesNo(body["mask_image"]), strength, batch)
}
