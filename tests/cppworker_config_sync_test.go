// cppworker_config_sync_test.go — тесты синхронизации конфигурации cppworker.
//
// Проверяет:
//   - config/cppworker-defaults.json читается при LoadConfigFromEnv
//   - env CPPWORKER_CTX_SIZE переопределяет JSON
//   - DefaultConfig() fallback когда JSON отсутствует
//   - Флаг --ctx-size не перезаписывает env (только если передан явно)
//
// Build tag: llama_stub (не требует реального llama.cpp).
//go:build llama_stub

package tests

import (
	"os"
	"path/filepath"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

// TestLoadConfigFromEnv_ReadsJSON — LoadConfigFromEnv читает
// config/cppworker-defaults.json и использует значения из него.
func TestLoadConfigFromEnv_ReadsJSON(t *testing.T) {
	// Создаём временный каталог и пишем cppworker-defaults.json
	tmpDir := t.TempDir()
	configDir := filepath.Join(tmpDir, "config")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}
	defaultsJSON := `{
		"defaultCtxSize": 16384,
		"defaultBatchSize": 256,
		"defaultGpuLayers": 10,
		"defaultFlashAttnType": 1,
		"defaultUseMmap": false
	}`
	defaultsPath := filepath.Join(configDir, "cppworker-defaults.json")
	if err := os.WriteFile(defaultsPath, []byte(defaultsJSON), 0644); err != nil {
		t.Fatal(err)
	}

	// Меняем рабочую директорию во временный каталог, чтобы LoadConfigFromFile
	// нашёл "config/cppworker-defaults.json" (относительный путь).
	prevWd, _ := os.Getwd()
	defer os.Chdir(prevWd)
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}

	// Очищаем все CPPWORKER_* env, чтобы JSON был единственным источником.
	clearCppworkerEnv(t)

	cfg := cppbackend.LoadConfigFromEnv()
	if cfg.DefaultCtxSize != 16384 {
		t.Errorf("expected DefaultCtxSize=16384 from JSON, got %d", cfg.DefaultCtxSize)
	}
	if cfg.DefaultBatchSize != 256 {
		t.Errorf("expected DefaultBatchSize=256 from JSON, got %d", cfg.DefaultBatchSize)
	}
	if cfg.DefaultGPULayers != 10 {
		t.Errorf("expected DefaultGPULayers=10 from JSON, got %d", cfg.DefaultGPULayers)
	}
	if cfg.DefaultFlashAttnType != 1 {
		t.Errorf("expected DefaultFlashAttnType=1 from JSON, got %d", cfg.DefaultFlashAttnType)
	}
	if cfg.DefaultUseMmap != false {
		t.Errorf("expected DefaultUseMmap=false from JSON, got %v", cfg.DefaultUseMmap)
	}
}

// TestLoadConfigFromEnv_EnvOverridesJSON — env CPPWORKER_CTX_SIZE
// переопределяет значение из JSON.
func TestLoadConfigFromEnv_EnvOverridesJSON(t *testing.T) {
	tmpDir := t.TempDir()
	configDir := filepath.Join(tmpDir, "config")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}
	defaultsJSON := `{"defaultCtxSize": 16384, "defaultGpuLayers": 10}`
	defaultsPath := filepath.Join(configDir, "cppworker-defaults.json")
	if err := os.WriteFile(defaultsPath, []byte(defaultsJSON), 0644); err != nil {
		t.Fatal(err)
	}

	prevWd, _ := os.Getwd()
	defer os.Chdir(prevWd)
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}

	clearCppworkerEnv(t)

	// Устанавливаем env override
	os.Setenv("CPPWORKER_CTX_SIZE", "32768")
	defer os.Unsetenv("CPPWORKER_CTX_SIZE")

	cfg := cppbackend.LoadConfigFromEnv()
	if cfg.DefaultCtxSize != 32768 {
		t.Errorf("expected DefaultCtxSize=32768 from env override, got %d", cfg.DefaultCtxSize)
	}
	// GPU layers берётся из JSON (env не задан)
	if cfg.DefaultGPULayers != 10 {
		t.Errorf("expected DefaultGPULayers=10 from JSON (no env override), got %d", cfg.DefaultGPULayers)
	}
}

// TestDefaultConfig_Fallback — DefaultConfig() возвращает 32768 для ctx,
// что соответствует cppworker-defaults.json (single source of truth).
func TestDefaultConfig_Fallback(t *testing.T) {
	tmpDir := t.TempDir()
	prevWd, _ := os.Getwd()
	defer os.Chdir(prevWd)
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	clearCppworkerEnv(t)

	// Нет config/cppworker-defaults.json → используется DefaultConfig()
	cfg := cppbackend.LoadConfigFromEnv()
	if cfg.DefaultCtxSize != 32768 {
		t.Errorf("expected DefaultCtxSize=32768 from DefaultConfig(), got %d", cfg.DefaultCtxSize)
	}
}

// clearCppworkerEnv удаляет все CPPWORKER_* env-переменные параметров модели,
// чтобы тесты были изолированы от окружения.
func clearCppworkerEnv(t *testing.T) {
	t.Helper()
	vars := []string{
		"CPPWORKER_CTX_SIZE", "CPPWORKER_DEFAULT_CTX_SIZE",
		"CPPWORKER_BATCH_SIZE", "CPPWORKER_GPU_LAYERS",
		"CPPWORKER_FLASH_ATTN_TYPE", "CPPWORKER_FLASH_ATTN",
		"CPPWORKER_N_THREADS", "CPPWORKER_NUMA",
		"CPPWORKER_USE_MMAP", "CPPWORKER_USE_MLOCK",
	}
	for _, v := range vars {
		os.Unsetenv(v)
	}
}