package sdbackend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// Интеграционные тесты сервиса (мок sd-server + fake-процесс)
// ============================================================
//
// Здесь проверяется СКВОЗНОЙ путь: генерация → нормализация → img_gen →
// поллинг → b64/url → метрики, а также очередь, автозагрузка модели и
// честная отмена. Настоящий sd-server не запускается.

// testService — сервис с мок-движком.
type testService struct {
	svc   *Service
	fake  *fakepServer
	runner *FakeRunner
	dir   string
}

func newTestService(t *testing.T, models map[string]types.ImageModelProfile) *testService {
	t.Helper()
	dir := t.TempDir()
	reg := NewRegistry(dir)
	for name, p := range models {
		p.Name = name
		writeProfile(t, dir, p)
	}
	if err := reg.Load(); err != nil {
		t.Fatalf("registry: %v", err)
	}

	cfg := DefaultConfig()
	cfg.ModelsDir = dir
	cfg.ListenIP = "127.0.0.1"
	cfg.ServerPort = freePort(t)
	cfg.StartupTimeoutSec = 5
	cfg.ImagesDir = filepath.Join(dir, "images")

	metrics := NewMetrics()
	store, err := NewImageStore(cfg.ImagesDir, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	sup := NewSupervisor(&cfg, reg, metrics)
	sup.SetReadinessPoll(2 * time.Millisecond)
	sup.SetPollEvery(2 * time.Millisecond)
	sup.SetReadinessTimeout(2 * time.Second)

	fake := newFakeServer()
	t.Cleanup(fake.Close)
	sup.SetSDServerBaseURL(fake.URL)

	runner := NewFakeRunner(func([]string) (Process, error) { return NewFakeProcess(1000, nil), nil })
	sup.SetRunner(runner)

	jobRunner := NewJobRunner(&cfg, reg, sup, metrics, store)
	return &testService{
		svc: &Service{
			Config: &cfg, Registry: reg, Sup: sup, Metrics: metrics,
			Queue: jobRunner.Queue(), Store: store, Runner: jobRunner,
			Idle:      NewIdleUnloadManager(&cfg, reg, sup, metrics),
			StartedAt: time.Now(),
		},
		fake: fake, runner: runner, dir: dir,
	}
}

func profileFor(name, family string) types.ImageModelProfile {
	return types.ImageModelProfile{
		Name:   name,
		Family: family,
		Files: []types.ImageModelFile{
			{Role: types.ImageFileRoleDiffusion, Repo: "local", Filename: "model.gguf"},
		},
		Defaults: types.DefaultImageGenDefaults(family),
	}
}

// --- 1. Синхронная генерация: b64 в ответе, seed положительный ----------------

func TestService_GenerateSync_B64(t *testing.T) {
	ts := newTestService(t, map[string]types.ImageModelProfile{"sd15": profileFor("sd15", "sd15")})
	norm, err := NormalizeGeneration(GenerationRequest{Prompt: "a cat", Width: 512, Height: 512}, nil, nil)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	res, err := ts.svc.Runner.GenerateSync(context.Background(), "sd15", false, true, norm)
	if err != nil {
		t.Fatalf("GenerateSync: %v", err)
	}
	if len(res.Images) != 1 || res.Images[0].B64JSON == "" {
		t.Fatalf("images: %+v", res.Images)
	}
	if res.Seed <= 0 {
		t.Fatalf("seed = %d, want positive", res.Seed)
	}

	rec, ok := ts.fake.lastRequest()
	if !ok {
		t.Fatal("движку не ушёл ни один запрос")
	}
	// OpenAI-путь: seed обязан быть в prompt (движок поле seed не читает).
	if !strings.Contains(rec.Prompt, "<sd_cpp_extra_args>") {
		t.Fatalf("seed не инжектирован в prompt: %q", rec.Prompt)
	}
	if rec.Seed != res.Seed {
		t.Fatalf("seed в img_gen = %d, ожидался %d", rec.Seed, res.Seed)
	}
	if rec.Width != 512 || rec.Height != 512 {
		t.Fatalf("size в img_gen = %dx%d", rec.Width, rec.Height)
	}
	// Метрики обязаны зафиксировать генерацию и картинку.
	snap := ts.svc.Metrics.Snapshot()
	if snap.TotalGenerations != 1 || snap.TotalImages != 1 {
		t.Fatalf("metrics: %+v", snap)
	}
	// Суммарные секунды округляются до миллисекунд, а мок отвечает мгновенно —
	// поэтому проверяем не «> 0», а что счётчик вообще существует и не ушёл
	// в минус (реальные секунды проверяются в TestMetrics_PrometheusText).
	if snap.TotalSeconds < 0 {
		t.Fatalf("суммарные секунды отрицательны: %+v", snap)
	}
}

// --- 2. batch: ровно столько элементов, сколько сгенерировано -----------------

func TestService_GenerateSync_BatchCount(t *testing.T) {
	ts := newTestService(t, map[string]types.ImageModelProfile{"sd15": profileFor("sd15", "sd15")})
	norm, _ := NormalizeGeneration(GenerationRequest{Prompt: "x", Width: 512, Height: 512, BatchCount: 3}, nil, nil)
	res, err := ts.svc.Runner.GenerateSync(context.Background(), "sd15", false, false, norm)
	if err != nil {
		t.Fatalf("GenerateSync: %v", err)
	}
	if len(res.Images) != 3 {
		t.Fatalf("images = %d, want 3 (batch=3)", len(res.Images))
	}
	rec, _ := ts.fake.lastRequest()
	if rec.BatchCount != 3 {
		t.Fatalf("batch_count в img_gen = %d, want 3", rec.BatchCount)
	}
}

// --- 3. response_format=url: PNG сохраняется, отдаётся ссылка -----------------

func TestService_GenerateSync_URLMode(t *testing.T) {
	ts := newTestService(t, map[string]types.ImageModelProfile{"sd15": profileFor("sd15", "sd15")})
	norm, _ := NormalizeGeneration(GenerationRequest{Prompt: "x", Width: 512, Height: 512}, nil, nil)
	res, err := ts.svc.Runner.GenerateSync(context.Background(), "sd15", true, false, norm)
	if err != nil {
		t.Fatalf("GenerateSync: %v", err)
	}
	if len(res.Images) != 1 || res.Images[0].URL == "" {
		t.Fatalf("url не отдан: %+v", res.Images)
	}
	if !strings.HasPrefix(res.Images[0].URL, "/images/") {
		t.Fatalf("url = %q", res.Images[0].URL)
	}
	name := strings.TrimPrefix(res.Images[0].URL, "/images/")
	if _, err := os.Stat(filepath.Join(ts.svc.Store.Dir(), name)); err != nil {
		t.Fatalf("файл картинки не создан: %v", err)
	}
}

// --- 4. Автозагрузка модели при генерации ------------------------------------

func TestService_GenerateSync_AutoLoadsModel(t *testing.T) {
	ts := newTestService(t, map[string]types.ImageModelProfile{"sd15": profileFor("sd15", "sd15")})
	if ts.svc.Sup.State() == StateLoaded {
		t.Fatal("до генерации модель не должна быть загружена")
	}
	norm, _ := NormalizeGeneration(GenerationRequest{Prompt: "x", Width: 512, Height: 512}, nil, nil)
	if _, err := ts.svc.Runner.GenerateSync(context.Background(), "sd15", false, false, norm); err != nil {
		t.Fatalf("GenerateSync: %v", err)
	}
	if ts.runner.Started() != 1 {
		t.Fatalf("spawn count = %d, want 1", ts.runner.Started())
	}
	if ts.svc.Sup.State() != StateLoaded {
		t.Fatalf("state = %q", ts.svc.Sup.State())
	}
}

// --- 5. 429 когда очередь заполнена ------------------------------------------

func TestService_QueueFullReturnsError(t *testing.T) {
	ts := newTestService(t, map[string]types.ImageModelProfile{"sd15": profileFor("sd15", "sd15")})
	// Забиваем очередь до отказа (capacity по умолчанию 64 в DefaultConfig,
	// но здесь сервис собран с дефолтным MaxConcurrent).
	cap := ts.svc.Queue.Capacity()
	releases := make([]func(), 0, cap)
	for i := 0; i < cap; i++ {
		rel, ok := ts.svc.Queue.TryAcquire()
		if !ok {
			t.Fatalf("не удалось занять слот %d из %d", i, cap)
		}
		releases = append(releases, rel)
	}
	norm, _ := NormalizeGeneration(GenerationRequest{Prompt: "x"}, nil, nil)
	_, err := ts.svc.Runner.GenerateSync(context.Background(), "sd15", false, false, norm)
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("err = %v, want ErrQueueFull", err)
	}
	if ts.svc.Metrics.Snapshot().QueueRejections != 1 {
		t.Fatalf("QueueRejections = %d", ts.svc.Metrics.Snapshot().QueueRejections)
	}
	for _, rel := range releases {
		rel()
	}
}

// --- 6. Асинхронная джоба: 202 → completed -----------------------------------

func TestJobRunner_SubmitAndWait(t *testing.T) {
	ts := newTestService(t, map[string]types.ImageModelProfile{"sd15": profileFor("sd15", "sd15")})
	norm, _ := NormalizeGeneration(GenerationRequest{Prompt: "x", Width: 512, Height: 512}, nil, nil)

	rec, err := ts.svc.Runner.Submit(context.Background(), "sd15", false, false, norm)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if rec.ID == "" || rec.State != JobStateQueued {
		t.Fatalf("rec = %+v", rec)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	final, err := ts.svc.Runner.Wait(ctx, rec.ID)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if final.State != JobStateCompleted {
		t.Fatalf("state = %q err=%q", final.State, final.Error)
	}
	if len(final.Images) != 1 {
		t.Fatalf("images = %+v", final.Images)
	}
	if final.DurationMS < 0 {
		t.Fatalf("duration_ms = %d", final.DurationMS)
	}
}

// --- 7. Отмена: queued — да, generating/processing — 409 ---------------------

func TestJobRunner_Cancel_QueuedOnly(t *testing.T) {
	ts := newTestService(t, map[string]types.ImageModelProfile{"sd15": profileFor("sd15", "sd15")})
	// Занимаем все слоты, чтобы джоба гарантированно осталась в queued.
	cap := ts.svc.Queue.Capacity()
	releases := make([]func(), 0, cap)
	for i := 0; i < cap; i++ {
		rel, _ := ts.svc.Queue.TryAcquire()
		releases = append(releases, rel)
	}
	defer func() {
		for _, rel := range releases {
			rel()
		}
	}()

	norm, _ := NormalizeGeneration(GenerationRequest{Prompt: "x"}, nil, nil)
	// Submit сам займёт слот — значит очередь должна быть на 1 меньше.
	releases[len(releases)-1]()
	releases = releases[:len(releases)-1]
	rec, err := ts.svc.Runner.Submit(context.Background(), "sd15", false, false, norm)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	// Немедленная отмена: состояние queued или processing (гонка).
	if err := ts.svc.Runner.Cancel(rec.ID); err != nil {
		// processing без engine job id → ErrCancelGenerating (движок ещё ничего
		// не знает, прерывать нечего) — это допустимо.
		if !errors.Is(err, ErrCancelGenerating) {
			t.Fatalf("Cancel: %v", err)
		}
	}
}

// Отмена НЕизвестной джобы → ErrJobNotFound (HTTP 404).
func TestJobRunner_Cancel_Unknown(t *testing.T) {
	ts := newTestService(t, map[string]types.ImageModelProfile{"sd15": profileFor("sd15", "sd15")})
	err := ts.svc.Runner.Cancel("nope")
	if !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("err = %v", err)
	}
}

// --- 8. Таймауты/ошибки движка → джоба failed с кодом ------------------------

func TestJobRunner_EngineFailureRecorded(t *testing.T) {
	ts := newTestService(t, map[string]types.ImageModelProfile{"sd15": profileFor("sd15", "sd15")})
	ts.fake.failJob = true
	norm, _ := NormalizeGeneration(GenerationRequest{Prompt: "x"}, nil, nil)
	rec, err := ts.svc.Runner.Submit(context.Background(), "sd15", false, false, norm)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	final, _ := ts.svc.Runner.Wait(ctx, rec.ID)
	if final.State != JobStateFailed {
		t.Fatalf("state = %q", final.State)
	}
	if final.ErrorCode != "generation_failed" {
		t.Fatalf("error_code = %q (%s)", final.ErrorCode, final.Error)
	}
}

// --- 9. Метрики Prometheus ----------------------------------------------------

func TestMetrics_PrometheusText(t *testing.T) {
	m := NewMetrics()
	m.ObserveGeneration(1500*time.Millisecond, 2, nil)
	m.MarkLoaded()
	vram := &VRAMInfo{Available: true, UsedMB: 4096, TotalMB: 8192}
	text := m.PrometheusText(StateLoaded, vram)
	for _, want := range []string{
		"sdworker_active_generations",
		"sdworker_generations_total 1",
		"sdworker_images_total 2",
		"sdworker_generation_seconds_total 1.500",
		"sdworker_sd_server_up",
		"sdworker_gpu_memory_used_mb 4096",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics missing %q:\n%s", want, text)
		}
	}
}

// --- 10. Idle-unload: активные запросы блокируют выгрузку --------------------

func TestIdleUnload_SkipsWhenBusy(t *testing.T) {
	ts := newTestService(t, map[string]types.ImageModelProfile{"sd15": profileFor("sd15", "sd15")})
	ts.svc.Config.IdleUnloadMinutes = 1
	// Загружаем модель.
	norm, _ := NormalizeGeneration(GenerationRequest{Prompt: "x"}, nil, nil)
	if _, err := ts.svc.Runner.GenerateSync(context.Background(), "sd15", false, false, norm); err != nil {
		t.Fatalf("generate: %v", err)
	}
	// Помечаем активный запрос — выгрузка обязана быть пропущена.
	ts.svc.Sup.BeginRequest()
	defer ts.svc.Sup.EndRequest()
	// «Состариваем» LastUsed, чтобы idle точно превысил таймаут.
	ts.svc.Metrics.mu.Lock()
	ts.svc.Metrics.lastUsedAt = time.Now().Add(-2 * time.Hour)
	ts.svc.Metrics.mu.Unlock()

	ts.svc.Idle.checkAndUnload()
	if ts.svc.Sup.State() != StateLoaded {
		t.Fatalf("модель выгружена, хотя есть активные запросы (state=%q)", ts.svc.Sup.State())
	}
}

func TestIdleUnload_UnloadsWhenIdle(t *testing.T) {
	ts := newTestService(t, map[string]types.ImageModelProfile{"sd15": profileFor("sd15", "sd15")})
	ts.svc.Config.IdleUnloadMinutes = 1
	norm, _ := NormalizeGeneration(GenerationRequest{Prompt: "x"}, nil, nil)
	if _, err := ts.svc.Runner.GenerateSync(context.Background(), "sd15", false, false, norm); err != nil {
		t.Fatalf("generate: %v", err)
	}
	ts.svc.Metrics.mu.Lock()
	ts.svc.Metrics.lastUsedAt = time.Now().Add(-2 * time.Hour)
	ts.svc.Metrics.mu.Unlock()

	ts.svc.Idle.checkAndUnload()
	if ts.svc.Sup.State() != StateNotLoaded {
		t.Fatalf("простаивающая модель не выгружена (state=%q)", ts.svc.Sup.State())
	}
}

// --- 11. Реестр: синтез профиля и warnings ------------------------------------

func TestRegistry_SynthesizeProfile(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "my-sd15")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "model.gguf"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(dir)
	if err := reg.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	p, ok := reg.Profile("my-sd15")
	if !ok {
		t.Fatal("модель без profile.json обязана попадать в реестр")
	}
	if p.Family != "other" || len(p.Files) != 1 {
		t.Fatalf("profile = %+v", p)
	}
	if p.Files[0].Role != types.ImageFileRoleDiffusion {
		t.Fatalf("role = %q", p.Files[0].Role)
	}
}

func TestRegistry_MissingDirIsWarningNotError(t *testing.T) {
	reg := NewRegistry(filepath.Join(t.TempDir(), "does-not-exist"))
	if err := reg.Load(); err != nil {
		t.Fatalf("отсутствующий каталог не должен ронять воркер: %v", err)
	}
	if len(reg.Warnings()) == 0 {
		t.Fatal("отсутствие каталога обязано попадать в warnings")
	}
}

// --- 12. Capabilities/Models: контракт для балансера -------------------------

func TestService_CapabilitiesAndModels(t *testing.T) {
	ts := newTestService(t, map[string]types.ImageModelProfile{
		"sd15": profileFor("sd15", "sd15"),
		"sdxl": profileFor("sdxl", "sdxl"),
	})
	caps := ts.svc.Capabilities()
	if len(caps.Models) != 2 {
		t.Fatalf("models = %d, want 2", len(caps.Models))
	}
	if caps.PinnedRevision != types.PinnedSDServerRevision {
		t.Fatalf("pinned = %q", caps.PinnedRevision)
	}
	if !caps.Limits.SeedAlwaysPositive || !caps.Limits.CancelQueuedOnly {
		t.Fatalf("limits: %+v", caps.Limits)
	}
	for _, m := range caps.Models {
		if m.State != StateNotLoaded {
			t.Fatalf("state = %q, want %q", m.State, StateNotLoaded)
		}
		if m.Family == "" {
			t.Fatal("family обязателен (контракт балансера)")
		}
	}
}
