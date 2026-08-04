package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"ollama-loadbalancer/pkg/types"
)

// TestLoad - проверка загрузки конфигурации из файла
func TestLoad(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "test-config.json")

	// Создаём конфигурацию напрямую в types
	data := []byte(`{
		"loadBalancer": {"host": "localhost", "port": 8080},
		"backends": [{"id": "b1", "name": "Backend 1", "host": "192.0.2.1", "ollamaPort": 11434}],
		"balancing": {"algorithm": "round-robin", "requestTimeout": 30}
	}`)
	err := os.WriteFile(configPath, data, 0644)
	require.NoError(t, err)

	loaded, err := Load(configPath)
	require.NoError(t, err)

	cfg := loaded.Get()
	assert.Equal(t, "localhost", cfg.LoadBalancer.Host)
	assert.Equal(t, 8080, cfg.LoadBalancer.Port)
	assert.Equal(t, 1, len(cfg.Backends))
	assert.Equal(t, "b1", cfg.Backends[0].ID)
}

// TestLoadNotFound - проверка загрузки несуществующего файла
func TestLoadNotFound(t *testing.T) {
	t.Parallel()

	_, err := Load("/nonexistent/path/config.json")
	assert.Error(t, err)
}

// TestLoadInvalidJSON - проверка загрузки некорректного JSON
func TestLoadInvalidJSON(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "invalid.json")

	err := os.WriteFile(configPath, []byte("{invalid json"), 0644)
	require.NoError(t, err)

	_, err = Load(configPath)
	assert.Error(t, err)
}

// TestSave - проверка сохранения конфигурации
func TestSave(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	data := []byte(`{"loadBalancer": {"host": "localhost", "port": 8080}}`)
	err := os.WriteFile(configPath, data, 0644)
	require.NoError(t, err)

	cfg, err := Load(configPath)
	require.NoError(t, err)

	err = cfg.Save()
	assert.NoError(t, err)

	// Проверяем что файл всё ещё существует
	_, err = os.Stat(configPath)
	assert.NoError(t, err)
}

// TestSaveFromEnv - проверка невозможности сохранения конфига из env
func TestSaveFromEnv(t *testing.T) {
	t.Setenv("LB_HOST", "0.0.0.0")
	cfg, err := LoadFromEnv()
	require.NoError(t, err)

	err = cfg.Save()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "environment")
}

// TestGet - проверка получения конфигурации
func TestGet(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	data := []byte(`{"loadBalancer": {"host": "testhost", "port": 9090}}`)
	err := os.WriteFile(configPath, data, 0644)
	require.NoError(t, err)

	cfg, err := Load(configPath)
	require.NoError(t, err)

	result := cfg.Get()
	assert.NotNil(t, result)
	assert.Equal(t, "testhost", result.LoadBalancer.Host)
	assert.Equal(t, 9090, result.LoadBalancer.Port)
}

// TestGetBackend - проверка получения бэкенда
func TestGetBackend(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	data := []byte(`{
		"backends": [
			{"id": "b1", "name": "Backend 1"},
			{"id": "b2", "name": "Backend 2"}
		]
	}`)
	err := os.WriteFile(configPath, data, 0644)
	require.NoError(t, err)

	cfg, err := Load(configPath)
	require.NoError(t, err)

	backend := cfg.GetBackend("b1")
	assert.NotNil(t, backend)
	assert.Equal(t, "Backend 1", backend.Name)

	backend = cfg.GetBackend("nonexistent")
	assert.Nil(t, backend)
}

// TestAddBackend - проверка добавления бэкенда
func TestAddBackend(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	data := []byte(`{"backends": [{"id": "b1", "name": "Backend 1"}]}`)
	err := os.WriteFile(configPath, data, 0644)
	require.NoError(t, err)

	cfg, err := Load(configPath)
	require.NoError(t, err)

	cfg.AddBackend(types.Backend{ID: "b2", Name: "Backend 2"})
	assert.Equal(t, 2, len(cfg.Get().Backends))
}

// TestRemoveBackend - проверка удаления бэкенда
func TestRemoveBackend(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	data := []byte(`{"backends": [{"id": "b1", "name": "Backend 1"}, {"id": "b2", "name": "Backend 2"}]}`)
	err := os.WriteFile(configPath, data, 0644)
	require.NoError(t, err)

	cfg, err := Load(configPath)
	require.NoError(t, err)

	removed := cfg.RemoveBackend("b1")
	assert.True(t, removed)
	assert.Equal(t, 1, len(cfg.Get().Backends))

	// Удаление несуществующего
	removed = cfg.RemoveBackend("nonexistent")
	assert.False(t, removed)
	assert.Equal(t, 1, len(cfg.Get().Backends))
}

// TestLoadFromEnv - проверка загрузки из переменных окружения
func TestLoadFromEnv(t *testing.T) {
	t.Setenv("LB_HOST", "0.0.0.0")
	t.Setenv("LB_PORT", "9090")
	t.Setenv("LB_API_PORT", "9091")
	t.Setenv("LB_BACKEND_HOSTS", "host1:11434,host2:11435")

	cfg, err := LoadFromEnv()
	require.NoError(t, err)

	config := cfg.Get()
	assert.Equal(t, "0.0.0.0", config.LoadBalancer.Host)
	assert.Equal(t, 9090, config.LoadBalancer.Port)
	assert.Equal(t, 9091, config.LoadBalancer.APIPort)
}

// TestSetDefaults - проверка установки значений по умолчанию
func TestSetDefaults(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "empty.json")

	err := os.WriteFile(configPath, []byte("{}"), 0644)
	require.NoError(t, err)

	loaded, err := Load(configPath)
	require.NoError(t, err)

	config := loaded.Get()
	assert.Equal(t, "0.0.0.0", config.LoadBalancer.Host)
	assert.Equal(t, 18080, config.LoadBalancer.Port)
	assert.Equal(t, 18081, config.LoadBalancer.APIPort)
	assert.Equal(t, 120, config.Balancing.RequestTimeout)
	assert.Equal(t, 100, config.Balancing.QueueMaxSize)
	assert.Equal(t, 4, config.Balancing.QueueWorkers)
}