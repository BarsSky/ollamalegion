package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"ollama-loadbalancer/pkg/types"
)

// TestExtractCppWorkerHost_FromExplicitConfig — CppWorkerHost из AgentConfig
// имеет наивысший приоритет. Это критично для bundled compose, где compose
// выставляет AGENT_CPPWORKER_HOST=cppworker-gpu (имя сервиса cppworker в compose-сети).
func TestExtractCppWorkerHost_FromExplicitConfig(t *testing.T) {
	t.Parallel()

	a := &Agent{config: &types.AgentConfig{
		BackendType:   types.BackendTypeLlamaCpp,
		CppWorkerHost: "cppworker-gpu",
		CppWorkerURL:  "http://different-host:19999",
		PublicHost:    "agent-container-name",
	}}

	got := a.extractCppWorkerHost()
	assert.Equal(t, "cppworker-gpu", got,
		"CppWorkerHost из AgentConfig должен иметь наивысший приоритет")
}

// TestExtractCppWorkerHost_FromURL — если CppWorkerHost не задан, парсим CppWorkerURL.
func TestExtractCppWorkerHost_FromURL(t *testing.T) {
	t.Parallel()

	a := &Agent{config: &types.AgentConfig{
		BackendType:  types.BackendTypeLlamaCpp,
		CppWorkerURL: "http://10.0.0.5:18092",
		PublicHost:   "agent-container-name",
	}}

	got := a.extractCppWorkerHost()
	assert.Equal(t, "10.0.0.5", got)
}

// TestExtractCppWorkerHost_FromPublicHost — fallback на PublicHost.
func TestExtractCppWorkerHost_FromPublicHost(t *testing.T) {
	t.Parallel()

	a := &Agent{config: &types.AgentConfig{
		BackendType: types.BackendTypeLlamaCpp,
		PublicHost:  "agent-container-name",
	}}

	got := a.extractCppWorkerHost()
	assert.Equal(t, "agent-container-name", got)
}

// TestExtractCppWorkerHost_Localhost — последний resort.
func TestExtractCppWorkerHost_Localhost(t *testing.T) {
	t.Parallel()

	a := &Agent{config: &types.AgentConfig{
		BackendType: types.BackendTypeLlamaCpp,
	}}

	got := a.extractCppWorkerHost()
	assert.Equal(t, "localhost", got)
}

// TestExtractCppWorkerPort_FromExplicitConfig — CppWorkerPort > 0 имеет приоритет.
func TestExtractCppWorkerPort_FromExplicitConfig(t *testing.T) {
	t.Parallel()

	a := &Agent{config: &types.AgentConfig{
		BackendType:   types.BackendTypeLlamaCpp,
		CppWorkerPort: 18092,
		CppWorkerURL:  "http://x:19999",
	}}

	got := a.extractCppWorkerPort()
	assert.Equal(t, 18092, got,
		"CppWorkerPort=18092 (bundled default) должен использоваться, а не 19999 из URL")
}

// TestExtractCppWorkerPort_FromURL — fallback на парсинг CppWorkerURL.
func TestExtractCppWorkerPort_FromURL(t *testing.T) {
	t.Parallel()

	a := &Agent{config: &types.AgentConfig{
		BackendType:  types.BackendTypeLlamaCpp,
		CppWorkerURL: "http://10.0.0.5:18092",
	}}

	got := a.extractCppWorkerPort()
	assert.Equal(t, 18092, got)
}

// TestExtractCppWorkerPort_FallbackLlamaCpp — bundled default 18092 (НЕ 18091).
//
// 2026-06-30: до фикса возвращался 18091 (legacy), что ломало de-dup с
// cppworker-бэкендом, зарегистрированным на 18092.
func TestExtractCppWorkerPort_FallbackLlamaCpp(t *testing.T) {
	t.Parallel()

	a := &Agent{config: &types.AgentConfig{
		BackendType: types.BackendTypeLlamaCpp,
	}}

	got := a.extractCppWorkerPort()
	assert.Equal(t, 18092, got,
		"Fallback для llama_cpp должен быть 18092 (bundled default), а не 18091 (legacy)")
}

// TestExtractCppWorkerPort_ZeroForOllama — для Ollama возвращаем 0, чтобы
// register() не отправлял cppWorkerPort.
func TestExtractCppWorkerPort_ZeroForOllama(t *testing.T) {
	t.Parallel()

	a := &Agent{config: &types.AgentConfig{
		BackendType: types.BackendTypeOllama,
	}}

	got := a.extractCppWorkerPort()
	assert.Equal(t, 0, got)
}

// TestExtractCppWorkerPort_ZeroForUnknown — для неизвестного типа тоже 0.
func TestExtractCppWorkerPort_ZeroForUnknown(t *testing.T) {
	t.Parallel()

	a := &Agent{config: &types.AgentConfig{
		BackendType: types.BackendType("unknown"),
	}}

	got := a.extractCppWorkerPort()
	assert.Equal(t, 0, got)
}

// TestExtractCppWorkerPort_InvalidURL — если URL невалиден, fallback на 18092 для llama_cpp.
func TestExtractCppWorkerPort_InvalidURL(t *testing.T) {
	t.Parallel()

	a := &Agent{config: &types.AgentConfig{
		BackendType:  types.BackendTypeLlamaCpp,
		CppWorkerURL: "://invalid-url",
	}}

	got := a.extractCppWorkerPort()
	assert.Equal(t, 18092, got,
		"При невалидном URL должен использоваться fallback 18092, а не 0")
}