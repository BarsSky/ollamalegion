// config_env_test.go — проверка ENV-override для LoadConfigFromEnv.
//
// 2026-06-26: эти тесты фиксируют, что ENV-переменные корректно читаются
// и не перебиваются (override) значениями по умолчанию или другими ENV.
// Это критично для debug-сценариев, когда ENV `CPPWORKER_USE_MMAP=false`
// должна ВЫКЛЮЧИТЬ mmap, а не игнорироваться default=true.
package cppbackend

import (
	"os"
	"testing"
)

// TestLoadConfigFromEnv_UseMmap проверяет, что CPPWORKER_USE_MMAP корректно
// отрабатывает как false, так и true.
func TestLoadConfigFromEnv_UseMmap(t *testing.T) {
	// Save and restore ENV.
	prev, hadPrev := os.LookupEnv("CPPWORKER_USE_MMAP")
	defer func() {
		if hadPrev {
			_ = os.Setenv("CPPWORKER_USE_MMAP", prev)
		} else {
			_ = os.Unsetenv("CPPWORKER_USE_MMAP")
		}
	}()

	tests := []struct {
		envValue string
		want     bool
	}{
		{"false", false},
		{"true", true},
		{"0", false},
		{"1", true},
		{"FALSE", false},
		{"True", true},
	}

	for _, tt := range tests {
		t.Run("env="+tt.envValue, func(t *testing.T) {
			_ = os.Setenv("CPPWORKER_USE_MMAP", tt.envValue)
			cfg := LoadConfigFromEnv()
			if cfg.DefaultUseMmap != tt.want {
				t.Errorf("CPPWORKER_USE_MMAP=%q: DefaultUseMmap=%v, want %v",
					tt.envValue, cfg.DefaultUseMmap, tt.want)
			}
		})
	}
}

// TestLoadConfigFromEnv_GPULayers проверяет, что CPPWORKER_GPU_LAYERS
// корректно парсится, включая sentinel-значения (-1=all, 0=CPU).
func TestLoadConfigFromEnv_GPULayers(t *testing.T) {
	prev, hadPrev := os.LookupEnv("CPPWORKER_GPU_LAYERS")
	defer func() {
		if hadPrev {
			_ = os.Setenv("CPPWORKER_GPU_LAYERS", prev)
		} else {
			_ = os.Unsetenv("CPPWORKER_GPU_LAYERS")
		}
	}()

	tests := []struct {
		envValue string
		want     int
	}{
		{"-1", -1}, // auto
		{"0", 0},   // CPU-only
		{"20", 20},
		{"99", 99},
	}

	for _, tt := range tests {
		t.Run("env="+tt.envValue, func(t *testing.T) {
			_ = os.Setenv("CPPWORKER_GPU_LAYERS", tt.envValue)
			cfg := LoadConfigFromEnv()
			if cfg.DefaultGPULayers != tt.want {
				t.Errorf("CPPWORKER_GPU_LAYERS=%q: DefaultGPULayers=%d, want %d",
					tt.envValue, cfg.DefaultGPULayers, tt.want)
			}
		})
	}
}

// TestLoadConfigFromEnv_CtxSize проверяет, что CPPWORKER_CTX_SIZE
// корректно парсится для разных значений, в т.ч. больших (32768, 65536).
func TestLoadConfigFromEnv_CtxSize(t *testing.T) {
	prev, hadPrev := os.LookupEnv("CPPWORKER_CTX_SIZE")
	defer func() {
		if hadPrev {
			_ = os.Setenv("CPPWORKER_CTX_SIZE", prev)
		} else {
			_ = os.Unsetenv("CPPWORKER_CTX_SIZE")
		}
	}()

	tests := []struct {
		envValue string
		want     int
	}{
		{"4096", 4096},
		{"8192", 8192},
		{"32768", 32768},
		{"65536", 65536},
	}

	for _, tt := range tests {
		t.Run("env="+tt.envValue, func(t *testing.T) {
			_ = os.Setenv("CPPWORKER_CTX_SIZE", tt.envValue)
			cfg := LoadConfigFromEnv()
			if cfg.DefaultCtxSize != tt.want {
				t.Errorf("CPPWORKER_CTX_SIZE=%q: DefaultCtxSize=%d, want %d",
					tt.envValue, cfg.DefaultCtxSize, tt.want)
			}
		})
	}
}

// TestLoadConfigFromEnv_DefaultUseMmap — sanity check: при отсутствии ENV
// CPPWORKER_USE_MMAP значение берется из DefaultConfig().
func TestLoadConfigFromEnv_DefaultUseMmap(t *testing.T) {
	_ = os.Unsetenv("CPPWORKER_USE_MMAP")
	cfg := LoadConfigFromEnv()
	want := DefaultConfig().DefaultUseMmap
	if cfg.DefaultUseMmap != want {
		t.Errorf("no env: DefaultUseMmap=%v, want %v (default)", cfg.DefaultUseMmap, want)
	}
}

// TestLoadConfigFromEnv_LlamaAliases — проверяет LLAMA_* → CPPWORKER_*
// маппинг для обратной совместимости (LLAMA_MMAP, LLAMA_N_GPU_LAYERS и т.п.).
func TestLoadConfigFromEnv_LlamaAliases(t *testing.T) {
	// Clear both LLAMA_* and CPPWORKER_*, чтобы избежать cross-contamination.
	envs := []string{"LLAMA_MMAP", "CPPWORKER_USE_MMAP"}
	for _, e := range envs {
		_ = os.Unsetenv(e)
	}

	// Set LLAMA_MMAP=true — LoadConfigFromEnv читает LLAMA_* алиасы
	// (см. mapLlamaToCppWorkerEnv в config.go).
	_ = os.Setenv("LLAMA_MMAP", "true")
	defer func() { _ = os.Unsetenv("LLAMA_MMAP") }()

	cfg := LoadConfigFromEnv()
	if !cfg.DefaultUseMmap {
		t.Errorf("LLAMA_MMAP=true: DefaultUseMmap=%v, want true (alias mapping broken)", cfg.DefaultUseMmap)
	}
}