package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/internal/sdbackend"
	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// Тесты HTTP-слоя sdworker (мок sd-server, без реального движка)
// ============================================================
//
// Собираем App на sdbackend.Service с подставным процессом, чей «движок» —
// httptest-мок. Тесты бьют в маршруты через a.setupRouter(): так проверяется и
// mux, и middleware (CORS/OPTIONS), и сами хендлеры.

const tinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg=="

// newTestApp — приложение на мок-движке.
func newTestApp(t *testing.T, models map[string]types.ImageModelProfile) (*App, *mockEngine) {
	t.Helper()
	dir := t.TempDir()
	for name, p := range models {
		p.Name = name
		if p.Family == "" {
			p.Family = "sd15"
		}
		sub := filepath.Join(dir, name)
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		for i := range p.Files {
			if p.Files[i].LocalPath == "" {
				p.Files[i].LocalPath = filepath.Join(sub, p.Files[i].Filename)
			}
		}
		data, _ := json.Marshal(p)
		if err := os.WriteFile(filepath.Join(sub, "profile.json"), data, 0o644); err != nil {
			t.Fatalf("write profile: %v", err)
		}
	}

	cfg := sdbackend.DefaultConfig()
	cfg.ModelsDir = dir
	cfg.ListenIP = "127.0.0.1"
	cfg.ServerPort = freePort(t)
	cfg.StartupTimeoutSec = 4
	// Каталог картинок НЕ внутри каталога моделей: реестр после RefreshIfChanged
	// перечитывает диск и любой подкаталог models/ считает bundle (живой случай:
	// images/ попадал в список моделей как третья «модель»).
	cfg.ImagesDir = filepath.Join(t.TempDir(), "images")

	reg := sdbackend.NewRegistry(dir)
	if err := reg.Load(); err != nil {
		t.Fatalf("registry: %v", err)
	}
	metrics := sdbackend.NewMetrics()
	store, err := sdbackend.NewImageStore(cfg.ImagesDir, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	sup := sdbackend.NewSupervisor(&cfg, reg, metrics)
	sup.SetReadinessPoll(2 * time.Millisecond)
	sup.SetPollEvery(2 * time.Millisecond)
	sup.SetReadinessTimeout(2 * time.Second)

	engine := newMockEngine()
	t.Cleanup(engine.Close)
	sup.SetSDServerBaseURL(engine.URL)
	sup.SetRunner(&fakeRunner{})

	runner := sdbackend.NewJobRunner(&cfg, reg, sup, metrics, store)
	svc := &sdbackend.Service{
		Config: &cfg, Registry: reg, Sup: sup, Metrics: metrics,
		Queue: runner.Queue(), Store: store, Runner: runner,
		Idle:      sdbackend.NewIdleUnloadManager(&cfg, reg, sup, metrics),
		StartedAt: time.Now(),
	}
	app := newApp(svc)
	cleanupSupervisor(t, app)
	return app, engine
}

// cleanupSupervisor гасит субпроцесс sd-server перед удалением temp-каталога.
//
// ЗАЧЕМ: даже fake-процесс «держит» состояние загрузки, а на Windows удаление
// каталога с ещё открытыми файлами падает с «directory is not empty». Плюс это
// ровно то, что обязан делать боевой shutdown.
func cleanupSupervisor(t *testing.T, app *App) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		_ = app.svc.Sup.Unload(ctx)
	})
}

func newTestProfile(name, family string) types.ImageModelProfile {
	return types.ImageModelProfile{
		Name:   name,
		Family: family,
		Files: []types.ImageModelFile{
			{Role: types.ImageFileRoleDiffusion, Repo: "local", Filename: "model.gguf"},
		},
		Defaults: types.DefaultImageGenDefaults(family),
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := netListen()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func doJSON(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// --- 1. /health: 200 даже без загруженной модели ------------------------------

func TestHealth_OK_EvenWithoutModel(t *testing.T) {
	app, _ := newTestApp(t, map[string]types.ImageModelProfile{"sd15": newTestProfile("sd15", "sd15")})
	rec := doJSON(t, app.setupRouter(), http.MethodGet, "/health", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp["model_loaded"] != false {
		t.Fatalf("model_loaded = %v, want false", resp["model_loaded"])
	}
	if resp["state"] != sdbackend.StateNotLoaded {
		t.Fatalf("state = %v", resp["state"])
	}
}

// --- 2. OPTIONS → 204 (SillyTavern «Connect») --------------------------------

func TestCORS_OptionsNoContent(t *testing.T) {
	app, _ := newTestApp(t, map[string]types.ImageModelProfile{"sd15": newTestProfile("sd15", "sd15")})
	router := app.setupRouter()
	for _, path := range []string{"/v1/images/generations", "/sdapi/v1/txt2img", "/api/image/capabilities"} {
		req := httptest.NewRequest(http.MethodOptions, path, nil)
		req.Header.Set("Origin", "http://localhost:8000")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Errorf("OPTIONS %s = %d, want 204", path, rec.Code)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:8000" {
			t.Errorf("OPTIONS %s Allow-Origin = %q", path, got)
		}
	}
}

// --- 3. OpenAI /v1/images/generations: b64 + created + data[] ----------------

func TestOpenAI_ImagesGenerations_B64(t *testing.T) {
	app, engine := newTestApp(t, map[string]types.ImageModelProfile{"sd15": newTestProfile("sd15", "sd15")})
	rec := doJSON(t, app.setupRouter(), http.MethodPost, "/v1/images/generations", map[string]any{
		"model":  "dall-e-2",
		"prompt": "a cat on a chair",
		"n":      2,
		"size":   "512x512",
		// Поля, которые обязаны проглатываться без ошибки:
		"quality": "hd", "style": "vivid", "user": "u1", "background": "opaque", "moderation": "low",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Created      int64  `json:"created"`
		OutputFormat string `json:"output_format"`
		Data         []struct {
			B64JSON string `json:"b64_json"`
			URL     string `json:"url"`
		} `json:"data"`
		Seed int64 `json:"seed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp.Created == 0 {
		t.Error("created обязателен в успешном ответе")
	}
	if len(resp.Data) != 2 {
		t.Fatalf("data = %d элементов, want 2 (n=2)", len(resp.Data))
	}
	for i, d := range resp.Data {
		if d.B64JSON == "" {
			t.Fatalf("data[%d].b64_json пуст", i)
		}
		if _, err := base64.StdEncoding.DecodeString(d.B64JSON); err != nil {
			t.Fatalf("data[%d].b64_json не декодируется: %v", i, err)
		}
	}
	if resp.Seed <= 0 {
		t.Fatalf("seed = %d, want positive (ловушка №1)", resp.Seed)
	}
	// Движок обязан получить seed в промпте (OpenAI-путь его не читает) и
	// batch_count = 2.
	req := engine.lastRequest()
	if !strings.Contains(req.Prompt, "<sd_cpp_extra_args>") {
		t.Fatalf("seed не инжектирован в prompt: %q", req.Prompt)
	}
	if req.BatchCount != 2 {
		t.Fatalf("batch_count = %d, want 2", req.BatchCount)
	}
	if req.Width != 512 || req.Height != 512 {
		t.Fatalf("size = %dx%d", req.Width, req.Height)
	}
}

// --- 4. response_format:"url" → наш абсолютный/относительный URL -------------

func TestOpenAI_ImagesGenerations_URLMode(t *testing.T) {
	app, _ := newTestApp(t, map[string]types.ImageModelProfile{"sd15": newTestProfile("sd15", "sd15")})
	rec := doJSON(t, app.setupRouter(), http.MethodPost, "/v1/images/generations", map[string]any{
		"prompt":          "x",
		"response_format": "url",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
			URL     string `json:"url"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Data) != 1 || resp.Data[0].URL == "" {
		t.Fatalf("url не отдан: %+v", resp.Data)
	}
	// Файл должен существовать и раздаваться статикой.
	name := strings.TrimPrefix(resp.Data[0].URL, "/images/")
	if _, err := os.Stat(filepath.Join(app.svc.Store.Dir(), name)); err != nil {
		t.Fatalf("картинка не сохранена: %v", err)
	}
}

// --- 5. Errors: OpenAI-конверт для /v1/* -------------------------------------

func TestOpenAI_ErrorEnvelope(t *testing.T) {
	app, _ := newTestApp(t, map[string]types.ImageModelProfile{"sd15": newTestProfile("sd15", "sd15")})
	router := app.setupRouter()

	// Пустой prompt.
	rec := doJSON(t, router, http.MethodPost, "/v1/images/generations", map[string]any{"prompt": ""})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
	var env struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("error must be an OpenAI envelope, got: %s", rec.Body.String())
	}
	if env.Error.Message == "" || env.Error.Type == "" || env.Error.Code != "prompt_required" {
		t.Fatalf("envelope = %+v", env.Error)
	}

	// Битый size — 400 с кодом invalid_size.
	rec = doJSON(t, router, http.MethodPost, "/v1/images/generations", map[string]any{
		"prompt": "x", "size": "not-a-size",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad size status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"invalid_size"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}

	// Метод не тот.
	rec = doJSON(t, router, http.MethodGet, "/v1/images/generations", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"error"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

// --- 6. size:"auto" (дефолт LibreChat/LobeChat) не должен быть ошибкой -------

func TestOpenAI_SizeAutoAccepted(t *testing.T) {
	app, engine := newTestApp(t, map[string]types.ImageModelProfile{"sdxl": newTestProfile("sdxl", "sdxl")})
	rec := doJSON(t, app.setupRouter(), http.MethodPost, "/v1/images/generations", map[string]any{
		"prompt": "x", "size": "auto",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	req := engine.lastRequest()
	// У sdxl дефолт 1024x1024.
	if req.Width != 1024 || req.Height != 1024 {
		t.Fatalf("size = %dx%d, want 1024x1024 (дефолт профиля)", req.Width, req.Height)
	}
}

// --- 7. GET /v1/models: алиасы для дропдаунов клиентов -----------------------

func TestOpenAI_ModelsAliases(t *testing.T) {
	app, _ := newTestApp(t, map[string]types.ImageModelProfile{"sd15": newTestProfile("sd15", "sd15")})
	rec := doJSON(t, app.setupRouter(), http.MethodGet, "/v1/models", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, id := range []string{"sd-cpp-local", "dall-e-2", "dall-e-3", "sd15"} {
		if !strings.Contains(body, id) {
			t.Errorf("id %q отсутствует в /v1/models: %s", id, body)
		}
	}
	// gpt-image-* НЕ отдаём: клиенты по этому префиксу ждут url вместо b64.
	if strings.Contains(body, "gpt-image") {
		t.Errorf("gpt-image-* не должен рекламироваться: %s", body)
	}
}

// --- 8. A1111 /sdapi/v1/txt2img ----------------------------------------------

func TestSDAPI_Txt2Img(t *testing.T) {
	app, engine := newTestApp(t, map[string]types.ImageModelProfile{"sd15": newTestProfile("sd15", "sd15")})
	rec := doJSON(t, app.setupRouter(), http.MethodPost, "/sdapi/v1/txt2img", map[string]any{
		"prompt":          "a cat",
		"negative_prompt": "bad",
		"width":           768,
		"height":          768,
		"steps":           12,
		"cfg_scale":       6.5,
		"seed":            -1,
		"batch_size":      2,
		"sampler_name":    "euler",
		"scheduler":       "karras",
		"clip_skip":       2,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Images     []string       `json:"images"`
		Parameters map[string]any `json:"parameters"`
		Info       string         `json:"info"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(resp.Images) != 2 {
		t.Fatalf("images = %d, want 2 (batch_size)", len(resp.Images))
	}
	if _, err := base64.StdEncoding.DecodeString(resp.Images[0]); err != nil {
		t.Fatalf("images[0] не b64: %v", err)
	}
	// info — JSON-СТРОКА (так у A1111: LibreChat/SillyTavern её парсят).
	if resp.Info == "" {
		t.Fatal("info обязателен и должен быть JSON-строкой")
	}
	var info map[string]any
	if err := json.Unmarshal([]byte(resp.Info), &info); err != nil {
		t.Fatalf("info не парсится как JSON: %v (%s)", err, resp.Info)
	}
	if seed, _ := info["seed"].(float64); seed <= 0 {
		t.Fatalf("seed в info = %v, want positive", info["seed"])
	}
	// A1111-путь: seed идёт полем, а НЕ блоком в промпте.
	req := engine.lastRequest()
	if strings.Contains(req.Prompt, "<sd_cpp_extra_args>") {
		t.Fatalf("A1111-путь не должен инжектить seed в prompt: %q", req.Prompt)
	}
	if req.Seed <= 0 {
		t.Fatalf("поле seed в img_gen = %d, want positive (иначе overflow движка)", req.Seed)
	}
	if req.ClipSkip == nil || *req.ClipSkip != 2 {
		t.Fatalf("clip_skip = %v, want 2", req.ClipSkip)
	}
	if resp.Parameters["width"] != float64(768) {
		t.Fatalf("parameters.width = %v", resp.Parameters["width"])
	}
}

// --- 9. A1111-заглушки --------------------------------------------------------

func TestSDAPI_Stubs(t *testing.T) {
	app, _ := newTestApp(t, map[string]types.ImageModelProfile{"sd15": newTestProfile("sd15", "sd15")})
	router := app.setupRouter()

	// POST /sdapi/v1/options → 200 (SillyTavern меняет модель при коннекте).
	rec := doJSON(t, router, http.MethodPost, "/sdapi/v1/options", map[string]any{"sd_model_checkpoint": "sd15"})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST options = %d", rec.Code)
	}
	// GET /sdapi/v1/options → без forge_preset (иначе ST уходит в Forge-ветку).
	rec = doJSON(t, router, http.MethodGet, "/sdapi/v1/options", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET options = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "forge_preset") {
		t.Fatalf("forge_preset НЕ должен присутствовать: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "sd_model_checkpoint") {
		t.Fatalf("sd_model_checkpoint отсутствует: %s", rec.Body.String())
	}

	// GET /sdapi/v1/progress.
	rec = doJSON(t, router, http.MethodGet, "/sdapi/v1/progress", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("progress = %d", rec.Code)
	}
	var prog struct {
		Progress float64 `json:"progress"`
		State    struct {
			JobCount int `json:"job_count"`
		} `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &prog); err != nil {
		t.Fatalf("progress JSON: %v", err)
	}
	if prog.Progress != 0 || prog.State.JobCount != 0 {
		t.Fatalf("progress = %+v", prog)
	}

	// POST /sdapi/v1/interrupt → 204.
	rec = doJSON(t, router, http.MethodPost, "/sdapi/v1/interrupt", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("interrupt = %d, want 204", rec.Code)
	}

	// GET /sdapi/v1/sd-vae и /sd-modules → [].
	for _, path := range []string{"/sdapi/v1/sd-vae", "/sdapi/v1/sd-modules"} {
		rec = doJSON(t, router, http.MethodGet, path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d", path, rec.Code)
		}
		if strings.TrimSpace(rec.Body.String()) != "[]" {
			t.Fatalf("%s = %s, want []", path, rec.Body.String())
		}
	}
}

// --- 10. Нативный async: 202 + job id, а потом completed ---------------------

func TestNative_GenerateAsyncJobLifecycle(t *testing.T) {
	app, _ := newTestApp(t, map[string]types.ImageModelProfile{"sd15": newTestProfile("sd15", "sd15")})
	router := app.setupRouter()

	rec := doJSON(t, router, http.MethodPost, "/api/image/generate", map[string]any{
		"prompt": "x", "width": 512, "height": 512, "steps": 8, "batch": 1,
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var submitted struct {
		ID            string `json:"id"`
		State         string `json:"state"`
		PollURL       string `json:"poll_url"`
		QueuePosition int    `json:"queue_position"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &submitted); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if submitted.ID == "" || submitted.PollURL != "/api/image/jobs/"+submitted.ID {
		t.Fatalf("submit response = %+v", submitted)
	}

	// Поллинг до completed.
	deadline := time.Now().Add(10 * time.Second)
	var state string
	for time.Now().Before(deadline) {
		rec = doJSON(t, router, http.MethodGet, "/api/image/jobs/"+submitted.ID, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("job status = %d body=%s", rec.Code, rec.Body.String())
		}
		var out struct {
			Job struct {
				State  string `json:"state"`
				Images []struct {
					B64JSON string `json:"b64_json"`
				} `json:"images"`
			} `json:"job"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		state = out.Job.State
		if state == sdbackend.JobStateCompleted {
			if len(out.Job.Images) != 1 || out.Job.Images[0].B64JSON == "" {
				t.Fatalf("images = %+v", out.Job.Images)
			}
			return
		}
		if state == sdbackend.JobStateFailed {
			t.Fatalf("job failed: %s", rec.Body.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job did not complete, last state = %q", state)
}

// --- 11. Неизвестная джоба → 404 ---------------------------------------------

func TestNative_JobNotFound(t *testing.T) {
	app, _ := newTestApp(t, map[string]types.ImageModelProfile{"sd15": newTestProfile("sd15", "sd15")})
	rec := doJSON(t, app.setupRouter(), http.MethodGet, "/api/image/jobs/img_nope", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
}

// --- 12. Отмена: queued-джоба под нагрузкой не даёт 500 ----------------------

// Полный сценарий «отмена generating-джобы → 409» проверяется в
// internal/sdbackend (jobs_test.go), где есть доступ к внутренностям раннера.
// Здесь проверяются только HTTP-границы: неизвестная джоба и отмена без
// возможности прервать (движок ещё не получил запрос).
func TestNative_CancelEdges(t *testing.T) {
	app, _ := newTestApp(t, map[string]types.ImageModelProfile{"sd15": newTestProfile("sd15", "sd15")})
	router := app.setupRouter()

	// Неизвестная джоба → 404.
	rec := doJSON(t, router, http.MethodPost, "/api/image/jobs/img_nope/cancel", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cancel unknown = %d body=%s", rec.Code, rec.Body.String())
	}

	// Занимаем все слоты: джоба гарантированно останется в queued.
	cap := app.svc.Queue.Capacity()
	held := make([]func(), 0, cap)
	for i := 0; i < cap; i++ {
		rel, ok := app.svc.Queue.TryAcquire()
		if !ok {
			t.Fatalf("slot %d", i)
		}
		held = append(held, rel)
	}
	defer func() {
		for _, r := range held {
			r()
		}
	}()

	// Снимаем один слот для самой джобы (Submit займёт его), остальные держим.
	held[len(held)-1]()
	held = held[:len(held)-1]

	rec = doJSON(t, router, http.MethodPost, "/api/image/generate", map[string]any{"prompt": "x"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit = %d body=%s", rec.Code, rec.Body.String())
	}
	var sub struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &sub)

	// Отмена: либо успели до старта (200 cancelled), либо джоба уже пошла в
	// работу и без engine job id прерывать нечего (409). 500 быть не должно.
	rec = doJSON(t, router, http.MethodPost, "/api/image/jobs/"+sub.ID+"/cancel", nil)
	if rec.Code != http.StatusOK && rec.Code != http.StatusConflict {
		t.Fatalf("cancel = %d body=%s", rec.Code, rec.Body.String())
	}

	// Дожидаемся терминального состояния джобы ПЕРЕД возвратом из теста.
	//
	// ЗАЧЕМ (это лечит флейк, а не проверку): фоновая горутина джобы в
	// ensureLoaded пишет sidecar sd-server.config.json в каталог модели
	// (supervisor.writeSidecar). Если тест успевает вернуться раньше, запись
	// происходит уже во время t.TempDir().RemoveAll — на Windows это
	// «unlinkat <tmp>\sd15: The directory is not empty» (наблюдалось и на
	// состоянии ДО этой правки, ~1 из 4 прогонов пакета).
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer waitCancel()
	if _, err := app.svc.Runner.Wait(waitCtx, sub.ID); err != nil {
		t.Logf("job %s не успела завершиться до очистки: %v", sub.ID, err)
	}
}

// --- 13. Наш /api/image/models + capabilities --------------------------------

func TestNative_ModelsAndCapabilities(t *testing.T) {
	app, _ := newTestApp(t, map[string]types.ImageModelProfile{
		"sd15": newTestProfile("sd15", "sd15"),
		"sdxl": newTestProfile("sdxl", "sdxl"),
	})
	router := app.setupRouter()

	rec := doJSON(t, router, http.MethodGet, "/api/image/models", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("models = %d", rec.Code)
	}
	var models struct {
		Models []map[string]any `json:"models"`
		State  string           `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &models); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(models.Models) != 2 {
		t.Fatalf("models = %d", len(models.Models))
	}
	// Контракт для балансера: snake_case ключи состояния.
	for _, m := range models.Models {
		for _, key := range []string{"name", "state", "size_bytes", "family", "active_queries"} {
			if _, ok := m[key]; !ok {
				t.Errorf("в модели отсутствует ключ %q: %+v", key, m)
			}
		}
	}

	rec = doJSON(t, router, http.MethodGet, "/api/image/capabilities", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("capabilities = %d", rec.Code)
	}
	body := rec.Body.String()
	// engine (сырой ответ движка) появляется только когда модель загружена —
	// до этого движка нет вовсе, поэтому проверяем обязательные поля.
	for _, key := range []string{"limits", "models", "pinned_sd_server_revision", "ready", "state"} {
		if !strings.Contains(body, key) {
			t.Errorf("capabilities без %q: %s", key, body)
		}
	}
	if !strings.Contains(body, `"cancel_queued_only":true`) {
		t.Errorf("capabilities обязаны честно сообщать cancel_queued_only: %s", body)
	}
}

// --- 14. load/unload через HTTP ----------------------------------------------

func TestNative_LoadAndUnload(t *testing.T) {
	app, _ := newTestApp(t, map[string]types.ImageModelProfile{"sd15": newTestProfile("sd15", "sd15")})
	router := app.setupRouter()

	// Неизвестная модель → 404.
	rec := doJSON(t, router, http.MethodPost, "/api/image/models/load", map[string]any{"name": "nope"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown model load = %d body=%s", rec.Code, rec.Body.String())
	}

	// Реальная модель: 202 (async) + прогресс.
	rec = doJSON(t, router, http.MethodPost, "/api/image/models/load", map[string]any{"name": "sd15"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("load = %d body=%s", rec.Code, rec.Body.String())
	}

	deadline := time.Now().Add(10 * time.Second)
	loaded := false
	for time.Now().Before(deadline) {
		r2 := doJSON(t, router, http.MethodGet, "/api/image/models/load/progress", nil)
		if strings.Contains(r2.Body.String(), `"loaded"`) && strings.Contains(r2.Body.String(), `"sd15"`) {
			loaded = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !loaded {
		t.Fatal("модель не загрузилась (прогресс не дошёл до loaded)")
	}

	// Unload.
	rec = doJSON(t, router, http.MethodPost, "/api/image/models/unload", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("unload = %d body=%s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, router, http.MethodGet, "/health", nil)
	if !strings.Contains(rec.Body.String(), `"model_loaded":false`) {
		t.Fatalf("после unload health = %s", rec.Body.String())
	}
}

// --- 15. /metrics содержит обязательные серии --------------------------------

func TestMetricsEndpoint(t *testing.T) {
	app, _ := newTestApp(t, map[string]types.ImageModelProfile{"sd15": newTestProfile("sd15", "sd15")})
	router := app.setupRouter()
	doJSON(t, router, http.MethodPost, "/v1/images/generations", map[string]any{"prompt": "x"})
	rec := doJSON(t, router, http.MethodGet, "/metrics", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"sdworker_active_generations",
		"sdworker_generations_total",
		"sdworker_images_total",
		"sdworker_generation_seconds_total",
		"sdworker_sd_server_up",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics без %q:\n%s", want, body)
		}
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("Content-Type = %q", ct)
	}
}

// --- 16. SSE-прогресс отдаёт начальный снимок --------------------------------

func TestLoadProgressStream_InitialSnapshot(t *testing.T) {
	app, _ := newTestApp(t, map[string]types.ImageModelProfile{"sd15": newTestProfile("sd15", "sd15")})
	router := app.setupRouter()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/image/models/load/progress/stream", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		router.ServeHTTP(rec, req)
		close(done)
	}()
	time.Sleep(150 * time.Millisecond)
	cancel()
	<-done

	body := rec.Body.String()
	if !strings.HasPrefix(body, "data: ") {
		t.Fatalf("SSE не начинается с data: %q", body)
	}
	if !strings.Contains(body, `"state"`) {
		t.Fatalf("в снимке нет state: %q", body)
	}
}

// --- 17. Небезопасные пути отклоняются ---------------------------------------

func TestImagesStatic_NoTraversal(t *testing.T) {
	app, _ := newTestApp(t, map[string]types.ImageModelProfile{"sd15": newTestProfile("sd15", "sd15")})
	router := app.setupRouter()
	req := httptest.NewRequest(http.MethodGet, "/images/../../etc/passwd", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("path traversal вернул 200: %s", rec.Body.String())
	}
}

// --- 18. Незаданный prompt → 400 в нативном контракте ------------------------

func TestNative_GenerateValidation(t *testing.T) {
	app, _ := newTestApp(t, map[string]types.ImageModelProfile{"sd15": newTestProfile("sd15", "sd15")})
	rec := doJSON(t, app.setupRouter(), http.MethodPost, "/api/image/generate", map[string]any{"width": 512})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "prompt is required") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}
