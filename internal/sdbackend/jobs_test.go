package sdbackend

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// Тесты отмены джоб
// ============================================================
//
// ЧЕСТНОСТЬ КОНТРАКТА: sd-server умеет отменять только queued-джобы
// (features_by_mode.img_gen.cancel_generating == false). Воркер обязан:
//   - отменять то, что ещё не ушло в движок (queued);
//   - пробрасывать 409 движка КАК ЕСТЬ (HTTP 409, текст «cannot be
//     interrupted»), а не превращать в 500 и не обещать mid-flight cancel.

// newRunnerForJobs — раннер с мок-движком и fake-процессом.
func newRunnerForJobs(t *testing.T) (*JobRunner, *fakepServer, *Supervisor) {
	t.Helper()
	dir := t.TempDir()
	reg := NewRegistry(dir)
	writeProfile(t, dir, simpleProfile("sd15"))
	if err := reg.Load(); err != nil {
		t.Fatalf("registry: %v", err)
	}
	cfg := DefaultConfig()
	cfg.ModelsDir = dir
	cfg.ServerPort = freePort(t)
	cfg.StartupTimeoutSec = 5
	cfg.ImagesDir = t.TempDir()

	metrics := NewMetrics()
	store, err := NewImageStore(cfg.ImagesDir, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	sup := NewSupervisor(&cfg, reg, metrics)
	sup.SetReadinessPoll(2 * time.Millisecond)
	sup.SetReadinessTimeout(2 * time.Second)
	sup.SetPollEvery(5 * time.Millisecond)

	fake := newFakeServer()
	t.Cleanup(fake.Close)
	sup.SetSDServerBaseURL(fake.URL)
	sup.SetRunner(NewFakeRunner(func([]string) (Process, error) { return NewFakeProcess(1, nil), nil }))

	return NewJobRunner(&cfg, reg, sup, metrics, store), fake, sup
}

// registerProcessingJob — регистрирует джобу в состоянии processing с уже
// известным engine job id. Так проверяется ВЕТКА ОТМЕНЫ воркера без гонки с
// фоновым исполнением: ровно это состояние возникает между SubmitImgGen и
// получением результата.
func registerProcessingJob(runner *JobRunner, engineID string) string {
	entry := &jobEntry{
		rec: JobRecord{
			ID: NewJobID(), State: JobStateProcessing, Model: "sd15",
			EngineID: engineID, Created: time.Now().UTC(),
		},
		done: make(chan struct{}),
	}
	runner.registerJobInternal(entry, engineID)
	return entry.rec.ID
}

// Джоба, которую движок уже взял в работу: cancel обязан вернуть 409 и текст
// движка, сохранённый дословно.
func TestCancel_GeneratingReturns409(t *testing.T) {
	runner, fake, sup := newRunnerForJobs(t)
	// Модель должна быть загружена — иначе движка нет и отменять нечего.
	if _, err := sup.Load(context.Background(), "sd15", LoadOptions{}); err != nil {
		t.Fatalf("load: %v", err)
	}
	// Движок держит джобу в generating.
	fake.mu.Lock()
	fake.jobs["job_ctrl"] = JobStatusGenerating
	fake.mu.Unlock()

	id := registerProcessingJob(runner, "job_ctrl")

	err := runner.Cancel(id)
	if err == nil {
		t.Fatal("ожидалась ошибка отмены generating-джобы")
	}
	// ВАЖНО: sentinel ErrCancelGenerating здесь НЕ возвращается — ошибка
	// приходит от движка (UpstreamError 409), и воркер обязан сохранить её
	// статус и текст. Проверяем именно машинный код отмены.
	if code := errorCodeOf(err); code != "cannot_cancel_generating" {
		t.Fatalf("код ошибки = %q, want cannot_cancel_generating (err=%v)", code, err)
	}
	if !strings.Contains(err.Error(), "cannot be interrupted") {
		t.Fatalf("текст движка потерян: %v", err)
	}
	// HTTP-статус обязан остаться 409 (а не 500): HTTP-слой опирается на это.
	ue, ok := AsUpstreamError(err)
	if !ok || ue.StatusCode != http.StatusConflict {
		t.Fatalf("upstream status = %v (ok=%v), want 409", ue, ok)
	}
	if fake.cancelCount() != 1 {
		t.Fatalf("в движок ушло %d cancel-запросов, want 1", fake.cancelCount())
	}
}

// Если движок ещё не получил запрос (engine job id неизвестен), отмена тоже
// честно отвечает «нельзя прервать» — и НЕ выдаёт 200 cancelled.
func TestCancel_ProcessingWithoutEngineID(t *testing.T) {
	runner, _, _ := newRunnerForJobs(t)
	id := registerProcessingJob(runner, "")
	err := runner.Cancel(id)
	if !errors.Is(err, ErrCancelGenerating) {
		t.Fatalf("err = %v, want ErrCancelGenerating", err)
	}
}

// Отмена queued-джобы (ещё не отданной движку) — успешна и не трогает движок.
func TestCancel_QueuedSucceeds(t *testing.T) {
	runner, fake, sup := newRunnerForJobs(t)
	if _, err := sup.Load(context.Background(), "sd15", LoadOptions{}); err != nil {
		t.Fatalf("load: %v", err)
	}

	// Регистрируем джобу в queued вручную: у неё ещё нет engine job id, и
	// именно это состояние обязано отменяться успешно.
	entry := &jobEntry{
		rec: JobRecord{
			ID: NewJobID(), State: JobStateQueued, Model: "sd15",
			Created: time.Now().UTC(),
		},
		done: make(chan struct{}),
	}
	runner.mu.Lock()
	runner.jobs[entry.rec.ID] = entry
	runner.inQueue = append(runner.inQueue, entry.rec.ID)
	runner.mu.Unlock()

	if err := runner.Cancel(entry.rec.ID); err != nil {
		t.Fatalf("cancel queued: %v", err)
	}
	got := runner.Get(entry.rec.ID)
	if got == nil || got.State != JobStateCancelled {
		t.Fatalf("state = %+v, want cancelled", got)
	}
	if fake.cancelCount() != 0 {
		t.Fatalf("для queued-джобы cancel в движок уходить не должен (он её не знает)")
	}
}

// Терминальная джоба отмене не подлежит.
func TestCancel_TerminalRejected(t *testing.T) {
	runner, _, sup := newRunnerForJobs(t)
	if _, err := sup.Load(context.Background(), "sd15", LoadOptions{}); err != nil {
		t.Fatalf("load: %v", err)
	}
	norm, _ := NormalizeGeneration(GenerationRequest{Prompt: "x"}, nil, nil)
	rec, err := runner.Submit(context.Background(), "sd15", false, false, norm)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := runner.Wait(ctx, rec.ID); err != nil {
		t.Fatalf("wait: %v", err)
	}
	err = runner.Cancel(rec.ID)
	if !errors.Is(err, ErrCancelNotAllowed) {
		t.Fatalf("err = %v, want ErrCancelNotAllowed", err)
	}
	if code := errorCodeOf(err); code != "job_already_finished" {
		t.Fatalf("code = %q", code)
	}
}

// Проверка, что errorCodeOf правильно классифицирует ключевые ошибки —
// на этом коде строятся HTTP-статусы воркера.
func TestErrorCodeOf(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{ErrQueueFull, "queue_full"},
		{ErrTimeout, "generation_timeout"},
		{ErrNotLoaded, "model_not_loaded"},
		{ErrLoading, "model_loading"},
		{ErrModelNotFound, "model_not_found"},
		{ErrNoModelConfigured, "model_not_found"},
		{ErrJobNotFound, "job_not_found"},
		{ErrCancelGenerating, "cannot_cancel_generating"},
		{ErrCancelNotAllowed, "job_already_finished"},
		{context.Canceled, "cancelled"},
		{&UpstreamError{StatusCode: 429, Message: "job queue is full"}, "engine_queue_full"},
		{&UpstreamError{StatusCode: 409, Message: "nope"}, "cannot_cancel_generating"},
		{&UpstreamError{StatusCode: 410, Message: "gone"}, "job_gone"},
		{&UpstreamError{StatusCode: 400, Message: "bad"}, "invalid_generation_params"},
		{&FlagIncompatibleError{Bin: "sd-server"}, "sd_server_incompatible"},
		{&StartupError{Bin: "sd-server"}, "sd_server_startup_failed"},
		{&EngineJobError{Status: JobStatusFailed, Code: "generation_failed", Message: "x"}, "generation_failed"},
	}
	for _, c := range cases {
		if got := errorCodeOf(c.err); got != c.want {
			t.Errorf("errorCodeOf(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// Джоба обязана получить статус failed, если движок вернул failed.
func TestGenerateSync_EngineFailedJob(t *testing.T) {
	runner, fake, sup := newRunnerForJobs(t)
	fake.failJob = true
	if _, err := sup.Load(context.Background(), "sd15", LoadOptions{}); err != nil {
		t.Fatalf("load: %v", err)
	}
	norm, _ := NormalizeGeneration(GenerationRequest{Prompt: "x"}, nil, nil)
	_, err := runner.GenerateSync(context.Background(), "sd15", false, false, norm)
	if err == nil {
		t.Fatal("ожидалась ошибка")
	}
	if code := errorCodeOf(err); code != "generation_failed" {
		t.Fatalf("code = %q (%v)", code, err)
	}
	if runner.ProfileFor("sd15") == nil {
		t.Fatal("ProfileFor сломан")
	}
}

// --- 429 от движка (его собственная очередь) пробрасывается как есть ---------

func TestGenerateSync_EngineQueueFull(t *testing.T) {
	runner, fake, sup := newRunnerForJobs(t)
	fake.failSubmitStatus = http.StatusTooManyRequests
	fake.failSubmitBody = `{"error":"job queue is full"}`
	if _, err := sup.Load(context.Background(), "sd15", LoadOptions{}); err != nil {
		t.Fatalf("load: %v", err)
	}
	norm, _ := NormalizeGeneration(GenerationRequest{Prompt: "x"}, nil, nil)
	_, err := runner.GenerateSync(context.Background(), "sd15", false, false, norm)
	if err == nil {
		t.Fatal("ожидалась 429")
	}
	if code := errorCodeOf(err); code != "engine_queue_full" {
		t.Fatalf("code = %q (%v)", code, err)
	}
}

var _ = types.ImageModelProfile{}
