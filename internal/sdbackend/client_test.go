package sdbackend

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ============================================================
// Тесты клиента sd-server (контракт /sdcpp/v1/*)
// ============================================================

func TestClient_Capabilities(t *testing.T) {
	fake := newFakeServer()
	defer fake.Close()
	c := NewSDServerClientURL(fake.URL)

	caps, err := c.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if caps.Model.Stem != "fake-model" {
		t.Errorf("model.stem = %q", caps.Model.Stem)
	}
	if caps.Limits.MaxBatchCount != 8 || caps.Limits.MaxQueueSize != 64 {
		t.Errorf("limits не разобраны: %+v", caps.Limits)
	}
	if len(caps.Samplers) == 0 || len(caps.Schedulers) == 0 {
		t.Errorf("samplers/schedulers пусты: %+v", caps)
	}
	// cancel_generating обязан читаться: иначе мы пообещаем клиенту то, чего
	// движок не умеет.
	if feats := caps.FeaturesByMode["img_gen"]; feats == nil || feats["cancel_generating"] {
		t.Errorf("features_by_mode.img_gen.cancel_generating должен быть false: %+v", caps.FeaturesByMode)
	}
	if caps.Raw() == nil {
		t.Error("raw JSON capabilities обязан сохраняться (незнакомые поля не теряем)")
	}
}

// Почему это важно: пока движок грузит модель, capabilities отдаёт 503 —
// readiness-поллер обязан это пережить, а не решить «процесс мёртв».
func TestClient_Capabilities_NotReadyYet(t *testing.T) {
	fake := newFakeServer()
	defer fake.Close()
	fake.notReadyCount = 2
	c := NewSDServerClientURL(fake.URL)

	if _, err := c.Capabilities(context.Background()); err == nil {
		t.Fatal("expected 503 while model is loading")
	}
	if _, err := c.Capabilities(context.Background()); err == nil {
		t.Fatal("expected 503 on second call")
	}
	if _, err := c.Capabilities(context.Background()); err != nil {
		t.Fatalf("third call must succeed: %v", err)
	}
}

func TestClient_SubmitAndWaitJob(t *testing.T) {
	fake := newFakeServer()
	defer fake.Close()
	c := NewSDServerClientURL(fake.URL)

	sub, err := c.SubmitImgGen(context.Background(), &ImgGenRequest{
		Prompt: "cat", Width: 512, Height: 512, Seed: 12345, BatchCount: 2, OutputFormat: "png",
	})
	if err != nil {
		t.Fatalf("SubmitImgGen: %v", err)
	}
	if sub.ID == "" || sub.Status != JobStatusQueued {
		t.Fatalf("submit response: %+v", sub)
	}
	job, err := c.WaitJob(context.Background(), sub.ID, 5*time.Second, time.Millisecond)
	if err != nil {
		t.Fatalf("WaitJob: %v", err)
	}
	if job.Status != JobStatusCompleted {
		t.Fatalf("status = %q", job.Status)
	}
	if job.Result == nil || len(job.Result.Images) != 2 {
		t.Fatalf("result.images: %+v", job.Result)
	}
	if _, err := decodePNG(job.Result.Images[0].B64JSON); err != nil {
		t.Fatalf("b64_json не декодируется: %v", err)
	}
}

// 429 «job queue is full» от движка обязан пробрасываться как UpstreamError с
// сохранением статуса и текста — на нём строятся наши ответы клиенту.
func TestClient_SubmitImgen_QueueFull(t *testing.T) {
	fake := newFakeServer()
	defer fake.Close()
	fake.failSubmitStatus = http.StatusTooManyRequests
	fake.failSubmitBody = `{"error":"job queue is full"}`
	c := NewSDServerClientURL(fake.URL)

	_, err := c.SubmitImgGen(context.Background(), &ImgGenRequest{Prompt: "x"})
	if err == nil {
		t.Fatal("expected 429")
	}
	ue, ok := AsUpstreamError(err)
	if !ok {
		t.Fatalf("error type = %T, want *UpstreamError", err)
	}
	if ue.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d", ue.StatusCode)
	}
	if !strings.Contains(ue.Message, "job queue is full") {
		t.Fatalf("message = %q (текст движка потерян)", ue.Message)
	}
}

// Ошибка движка приходит СТРОКОЙ (не OpenAI-конвертом) — парсер обязан понять обе формы.
func TestUpstreamError_ParsesBothEnvelopes(t *testing.T) {
	flat := upstreamError(400, []byte(`{"error":"invalid generation parameters"}`))
	ue, ok := AsUpstreamError(flat)
	if !ok || ue.Message != "invalid generation parameters" {
		t.Fatalf("flat envelope: %+v", flat)
	}
	openai := upstreamError(400, []byte(`{"error":{"code":"bad_request","message":"nope"}}`))
	ue, ok = AsUpstreamError(openai)
	if !ok || ue.Message != "nope" || ue.Code != "bad_request" {
		t.Fatalf("openai envelope: %+v", openai)
	}
	empty := upstreamError(500, nil)
	if empty == nil {
		t.Fatal("nil body must still produce an error")
	}
}

// Отмена generating-джобы: движок отдаёт 409 — мы НЕ должны это маскировать.
func TestClient_CancelJob_GeneratingConflicts(t *testing.T) {
	fake := newFakeServer()
	defer fake.Close()
	c := NewSDServerClientURL(fake.URL)

	// Регистрируем джобу в состоянии generating.
	fake.mu.Lock()
	fake.jobs["job_gen"] = JobStatusGenerating
	fake.mu.Unlock()

	err := c.CancelJob(context.Background(), "job_gen")
	if err == nil {
		t.Fatal("expected 409 for generating job")
	}
	ue, ok := AsUpstreamError(err)
	if !ok || ue.StatusCode != http.StatusConflict {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(ue.Message, "cannot be interrupted") {
		t.Fatalf("message = %q", ue.Message)
	}
	if fake.cancelCount() != 1 {
		t.Fatalf("cancel calls = %d", fake.cancelCount())
	}
}

func TestClient_JobNotFound(t *testing.T) {
	fake := newFakeServer()
	defer fake.Close()
	c := NewSDServerClientURL(fake.URL)
	_, err := c.Job(context.Background(), "job_missing")
	ue, ok := AsUpstreamError(err)
	if !ok || ue.StatusCode != http.StatusNotFound {
		t.Fatalf("err = %v", err)
	}
}

func TestClient_WaitJob_Timeout(t *testing.T) {
	fake := newFakeServer()
	defer fake.Close()
	fake.generatingForever = true
	c := NewSDServerClientURL(fake.URL)

	sub, err := c.SubmitImgGen(context.Background(), &ImgGenRequest{Prompt: "x"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	job, err := c.WaitJob(context.Background(), sub.ID, 80*time.Millisecond, 5*time.Millisecond)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if job == nil || job.Status != JobStatusGenerating {
		t.Fatalf("последний статус обязан возвращаться вместе с таймаутом: %+v", job)
	}
}

func TestClient_WaitJob_ContextCancel(t *testing.T) {
	fake := newFakeServer()
	defer fake.Close()
	fake.generatingForever = true
	c := NewSDServerClientURL(fake.URL)
	sub, _ := c.SubmitImgGen(context.Background(), &ImgGenRequest{Prompt: "x"})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.WaitJob(ctx, sub.ID, 5*time.Second, 5*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
}

func TestClient_FailedJob(t *testing.T) {
	fake := newFakeServer()
	defer fake.Close()
	fake.failJob = true
	c := NewSDServerClientURL(fake.URL)
	sub, _ := c.SubmitImgGen(context.Background(), &ImgGenRequest{Prompt: "x"})
	job, err := c.WaitJob(context.Background(), sub.ID, 5*time.Second, time.Millisecond)
	if err != nil {
		t.Fatalf("WaitJob: %v", err)
	}
	if job.Status != JobStatusFailed {
		t.Fatalf("status = %q", job.Status)
	}
	msg, code := jobErrorText(job)
	if code != "generation_failed" || !strings.Contains(msg, "empty results") {
		t.Fatalf("msg=%q code=%q", msg, code)
	}
}
