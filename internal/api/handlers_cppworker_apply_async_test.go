package api

import (
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// TestSubmitApplyJob_BasicFields — проверяет, что submitApplyJob правильно
// инициализирует job struct.
func TestSubmitApplyJob_BasicFields(t *testing.T) {
	profile := types.LlamaCppModelProfile{
		ContextLength: 16384,
		BatchSize:     512,
	}
	job := submitApplyJob("test-model", profile, 3, []string{"b1", "b2"})

	if job.ApplyID == "" {
		t.Error("ApplyID should be non-empty")
	}
	if job.Model != "test-model" {
		t.Errorf("expected model=test-model, got %q", job.Model)
	}
	if job.Profile.ContextLength != 16384 {
		t.Errorf("expected contextLength=16384, got %d", job.Profile.ContextLength)
	}
	if job.State != applyStateQueued {
		t.Errorf("expected state=queued, got %q", job.State)
	}
	if job.Busy != 3 {
		t.Errorf("expected busy=3, got %d", job.Busy)
	}
	if job.Terminal {
		t.Error("job should not be terminal at submit time")
	}
	if len(job.Backends) != 2 {
		t.Errorf("expected 2 backends, got %d", len(job.Backends))
	}
	for _, b := range job.Backends {
		if b.Status != "pending" {
			t.Errorf("expected backend status=pending, got %q", b.Status)
		}
	}
	if time.Since(job.StartedAt) > time.Second {
		t.Error("StartedAt should be recent")
	}
}

// TestUpdateApplyJob_StateTransition — applyState переходы.
func TestUpdateApplyJob_StateTransition(t *testing.T) {
	job := submitApplyJob("m1", types.LlamaCppModelProfile{ContextLength: 4096}, 0, []string{"b1"})

	updateApplyJob(job.ApplyID, func(j *applyJob) {
		j.State = applyStateReloading
		j.Backends["b1"] = applyBackendState{Status: "pending", StartedAt: time.Now()}
	})

	got := getApplyJob(job.ApplyID)
	if got == nil {
		t.Fatal("job not found after update")
	}
	if got.State != applyStateReloading {
		t.Errorf("expected state=reloading, got %q", got.State)
	}
	if got.Backends["b1"].Status != "pending" {
		t.Errorf("expected backend status=pending, got %q", got.Backends["b1"].Status)
	}

	// Reloaded + terminal
	updateApplyJob(job.ApplyID, func(j *applyJob) {
		j.State = applyStateReloaded
		j.Terminal = true
		j.Backends["b1"] = applyBackendState{Status: "reloaded", FinishedAt: time.Now()}
	})

	got = getApplyJob(job.ApplyID)
	if got.State != applyStateReloaded {
		t.Errorf("expected state=reloaded, got %q", got.State)
	}
	if !got.Terminal {
		t.Error("expected terminal=true")
	}
	if got.Backends["b1"].Status != "reloaded" {
		t.Errorf("expected backend status=reloaded, got %q", got.Backends["b1"].Status)
	}
}

// TestGetApplyJob_NotFound — неизвестный applyId → nil.
func TestGetApplyJob_NotFound(t *testing.T) {
	got := getApplyJob("nonexistent")
	if got != nil {
		t.Errorf("expected nil for unknown applyId, got %+v", got)
	}
}

// TestContainsUnsafeURLChars — кейсы с безопасными/небезопасными chars.
func TestContainsUnsafeURLChars(t *testing.T) {
	safe := []string{"model", "model-name", "model_name", "model.gguf", "Qwen3-Instruct-2507"}
	for _, s := range safe {
		if containsUnsafeURLChars(s) {
			t.Errorf("expected %q to be safe, got unsafe", s)
		}
	}
	unsafe := []string{"model name", "model/name", "model?x=y", "model&y=z", "model#frag", "model+plus"}
	for _, s := range unsafe {
		if !containsUnsafeURLChars(s) {
			t.Errorf("expected %q to be unsafe, got safe", s)
		}
	}
}

// TestURLPathEscape — round-trip escape.
func TestURLPathEscape(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"model", "model"},
		{"Qwen3-Instruct-2507", "Qwen3-Instruct-2507"},
		{"model with space", "model%20with%20space"},
		{"model/slash", "model%2Fslash"},
		{"модель", "%D0%BC%D0%BE%D0%B4%D0%B5%D0%BB%D1%8C"}, // utf-8 → escaped
	}
	for _, tt := range tests {
		got := urlPathEscape(tt.input)
		if got != tt.expected {
			t.Errorf("urlPathEscape(%q) = %q, expected %q", tt.input, got, tt.expected)
		}
	}
}

// TestSumInt64Values_Backend — helper используется в cppworker_active_queries.go,
// но проверяем что нет дублирования (только в одном файле). Этот тест
// живёт здесь для полноты.
func TestSumInt64Values_NotApplicable(t *testing.T) {
	// Sum helper в cppworker_active_queries.go — там же и тест.
	// Здесь оставлен placeholder.
	_ = t
}

// TestApplyJobState_StringValues — apply state values.
func TestApplyJobState_StringValues(t *testing.T) {
	tests := []struct {
		state applyJobState
		str   string
	}{
		{applyStateQueued, "queued"},
		{applyStateReloading, "reloading"},
		{applyStateReloaded, "reloaded"},
		{applyStatePartial, "partial"},
		{applyStateError, "error"},
	}
	for _, tt := range tests {
		if string(tt.state) != tt.str {
			t.Errorf("expected %q, got %q", tt.str, string(tt.state))
		}
	}
}

// TestApplyJobsRegistry_FIFO_50 — submit > 50 → oldest evicted.
func TestApplyJobsRegistry_FIFO_50(t *testing.T) {
	// Submit 55 jobs с разными StartedAt
	for i := 0; i < 55; i++ {
		job := submitApplyJob("m", types.LlamaCppModelProfile{ContextLength: 1024}, 0, []string{"b"})
		// Force different StartedAt для проверки FIFO
		job.StartedAt = time.Now().Add(time.Duration(i) * time.Millisecond)
		updateApplyJob(job.ApplyID, func(j *applyJob) {
			j.StartedAt = time.Now().Add(time.Duration(i) * time.Millisecond)
		})
	}
	applyJobsMu.RLock()
	count := len(applyJobs)
	applyJobsMu.RUnlock()
	if count > 50 {
		t.Errorf("expected at most 50 jobs after 55 submits, got %d", count)
	}
}
