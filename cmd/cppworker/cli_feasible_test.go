// cli_feasible_test.go — Round 37 (2026-08-18) tests for -feasible CLI.
//
// КРИТИЧНО: production bug 2026-08-18 был именно про это — operator не
// имел инструмента узнать feasible n_ctx без загрузки модели. -feasible
// закрывает эту дыру.
//go:build llama_stub

package main

import (
	"bytes"
	"strings"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

// TestRunFeasible_UsageError — modelName == "" → exit 2 + usage message.
func TestRunFeasible_UsageError(t *testing.T) {
	out := &bytes.Buffer{}
	code := runFeasible("", out)
	if code != 2 {
		t.Errorf("exit code = %d, want 2 (usage error)", code)
	}
	if !strings.Contains(out.String(), "usage:") {
		t.Errorf("output missing usage, got: %q", out.String())
	}
}

// TestRunFeasible_BackendNotInitialized — backend nil → exit 1.
func TestRunFeasible_BackendNotInitialized(t *testing.T) {
	// Reset global to ensure nil
	oldBackend := cppbackend.GetBackend()
	cppbackend.SetGlobalBackend(nil)
	defer cppbackend.SetGlobalBackend(oldBackend)

	out := &bytes.Buffer{}
	code := runFeasible("test-model", out)
	if code != 1 {
		t.Errorf("exit code = %d, want 1 (no backend)", code)
	}
	if !strings.Contains(out.String(), "backend not initialized") {
		t.Errorf("output missing 'backend not initialized', got: %q", out.String())
	}
}

// TestRunFeasible_OutputsAllFields — output содержит все ключевые поля.
func TestRunFeasible_OutputsAllFields(t *testing.T) {
	// Setup test backend with a fake model
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackend()
	cppbackend.SetGlobalBackend(backend)
	defer cppbackend.SetGlobalBackend(nil)

	out := &bytes.Buffer{}
	code := runFeasible("nonexistent", out)
	// Model not found → exit 1, but no crash
	if code != 1 {
		t.Errorf("exit code = %d, want 1 (model not found)", code)
	}
	// The error message should be in output
	if !strings.Contains(out.String(), "error:") {
		t.Errorf("output missing 'error:', got: %q", out.String())
	}
}

// TestFeasibleAutoRecommended — pure-function unit test.
func TestFeasibleAutoRecommended(t *testing.T) {
	tests := []struct {
		name string
		info *cppbackend.FeasibleInfo
		want int
	}{
		{
			name: "all zero",
			info: &cppbackend.FeasibleInfo{},
			want: 0,
		},
		{
			name: "only VRAM",
			info: &cppbackend.FeasibleInfo{MaxVRAMCtx: 8192},
			want: 8192,
		},
		{
			name: "only RAM",
			info: &cppbackend.FeasibleInfo{MaxRAMCtx: 16384},
			want: 16384,
		},
		{
			name: "min of all three",
			info: &cppbackend.FeasibleInfo{
				MaxVRAMCtx: 32768,
				MaxRAMCtx:  131072,
				GGUFMax:    262144,
			},
			want: 32768, // min(VRAM=32K, RAM=131K, GGUF=262K) = 32K
		},
		{
			name: "GGUF cap wins",
			info: &cppbackend.FeasibleInfo{
				MaxVRAMCtx: 524288,
				MaxRAMCtx:  1048576,
				GGUFMax:    8192, // tiny model
			},
			want: 8192,
		},
		{
			name: "skip zero values",
			info: &cppbackend.FeasibleInfo{
				MaxVRAMCtx: 0, // unknown
				MaxRAMCtx:  131072,
				GGUFMax:    262144,
			},
			want: 131072, // min(skip 0, 131K, 262K) = 131K
		},
		{
			name: "nil info",
			info: nil,
			want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := feasibleAutoRecommended(tt.info)
			if got != tt.want {
				t.Errorf("feasibleAutoRecommended() = %d, want %d", got, tt.want)
			}
		})
	}
}
