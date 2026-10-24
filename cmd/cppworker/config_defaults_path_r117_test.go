// config_defaults_path_r117_test.go — R117 (2026-10-14): запись runtime-настроек
// не должна попадать в дерево репозитория.
//
// ЖИВОЙ ДЕФЕКТ (найден при разборе передачи R115/R116). saveConfigToDefaultsFile
// писал по ОТНОСИТЕЛЬНОМУ пути config/cppworker-defaults.json, а тесты пакета
// (round12_nparallel_config_test.go, handlers_config_test.go,
// rpc_config_api_r115_test.go) вызывают handleCppWorkerUpdateConfig из рабочего
// каталога пакета — cmd/cppworker/. Поэтому обычный `go test ./cmd/cppworker/`
// затирал ОТСЛЕЖИВАЕМЫЙ git-файл cmd/cppworker/config/cppworker-defaults.json
// содержимым тестовой заглушки:
//
//	"modelsDir":  "C:\\Users\\<...>\\Temp\\TestPUTConfig_DefaultNParallel_NullSkip<...>\\001",
//	"defaultCtxSize": 0, "defaultGpuLayers": 0, "defaultNParallel": 4,
//	"rpcEnabled": false
//
// Это не косметика: config/ копируется в образ cppworker, то есть загрязнённое
// дерево даёт узел, который ищет модели по пути Windows-теста. Здесь:
//   - defaultsFilePath() уважает CPPWORKER_CONFIG_DIR (в тестах — temp);
//   - TestMain ставит её для ВСЕХ тестов пакета, чтобы ни один тест не мог
//     писать в дерево репозитория, даже если про него забыли.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

// TestMain изолирует запись настроек для всего пакета.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "cppworker-test-config-")
	if err == nil {
		_ = os.Setenv("CPPWORKER_CONFIG_DIR", dir)
	}
	code := m.Run()
	if err == nil {
		_ = os.RemoveAll(dir)
	}
	os.Exit(code)
}

// TestR117_DefaultsPathHonoursEnvDir — путь берётся из CPPWORKER_CONFIG_DIR,
// а без неё остаётся исторический config/cppworker-defaults.json.
func TestR117_DefaultsPathHonoursEnvDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CPPWORKER_CONFIG_DIR", dir)
	if got, want := defaultsFilePath(), filepath.Join(dir, "cppworker-defaults.json"); got != want {
		t.Errorf("defaultsFilePath() = %q, want %q", got, want)
	}

	t.Setenv("CPPWORKER_CONFIG_DIR", "  ") // пусто/пробелы → дефолт
	if got, want := defaultsFilePath(), filepath.Join("config", "cppworker-defaults.json"); got != want {
		t.Errorf("defaultsFilePath() без CPPWORKER_CONFIG_DIR = %q, want %q", got, want)
	}
}

// TestR117_HandlerDoesNotTouchRepoConfig — регрессия живого дефекта: успешный
// PUT /api/v1/cppworker/config пишет НАСТРОЙКУ в изолированный каталог и НЕ
// меняет отслеживаемый файл в дереве репозитория.
func TestR117_HandlerDoesNotTouchRepoConfig(t *testing.T) {
	repoPath := filepath.Join("config", "cppworker-defaults.json")
	before, beforeErr := os.ReadFile(repoPath)

	dir := t.TempDir()
	t.Setenv("CPPWORKER_CONFIG_DIR", filepath.Join(dir, "config"))

	prev := currentConfig
	t.Cleanup(func() { currentConfig = prev })
	currentConfig = &cppbackend.Config{
		Host:             "127.0.0.1",
		Port:             18091,
		ModelsDir:        dir,
		DefaultNParallel: 2,
	}

	body := `{"defaultNParallel":3}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/cppworker/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleCppWorkerUpdateConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("PUT config: HTTP %d (%s)", w.Code, w.Body.String())
	}
	if currentConfig.DefaultNParallel != 3 {
		t.Fatalf("defaultNParallel = %d, want 3 (настройка не применена)",
			currentConfig.DefaultNParallel)
	}

	// Настройка сохранена — но в каталоге из CPPWORKER_CONFIG_DIR.
	saved, err := os.ReadFile(defaultsFilePath())
	if err != nil {
		t.Fatalf("настройка не сохранена в %s: %v", defaultsFilePath(), err)
	}
	var cfg cppbackend.Config
	if err := json.Unmarshal(saved, &cfg); err != nil {
		t.Fatalf("сохранённый конфиг не парсится: %v", err)
	}
	if cfg.DefaultNParallel != 3 {
		t.Errorf("в файле defaultNParallel = %d, want 3", cfg.DefaultNParallel)
	}

	// Отслеживаемый файл репозитория — не тронут.
	after, afterErr := os.ReadFile(repoPath)
	if (beforeErr == nil) != (afterErr == nil) {
		t.Fatalf("тест создал/удалил %s в дереве репозитория (%v → %v)",
			repoPath, beforeErr, afterErr)
	}
	if string(before) != string(after) {
		t.Errorf("тест изменил отслеживаемый %s: прогон `go test ./cmd/cppworker/` "+
			"загрязняет дерево, а файл копируется в образ", repoPath)
	}
}
