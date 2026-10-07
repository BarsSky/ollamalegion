// embedded_test.go — тесты встроенного агента (R-Image, 2026-10-07).
//
// ЧТО ЗДЕСЬ ГЛАВНОЕ. Встроенный агент запускается ВНУТРИ процесса воркера, и
// ошибка в его конфиге стоила бы не «нет метрик», а упавшего inference-воркера.
// Поэтому проверяются три свойства:
//
//  1. по умолчанию агент ВЫКЛЮЧЕН (раскатка сборки не меняет поведение стенда);
//  2. ID записи берётся из переменной ИМЕННО этого воркера (SDWORKER_* для
//     image, CPPWORKER_* для llama.cpp) — иначе появится вторая запись о том же
//     процессе, ровно тот дефект, который эта работа и устраняет;
//  3. RunEmbedded не паникует и не фаталит при неполном конфиге.
package agent

import (
	"os"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// setEnv — выставляет env и возвращает restore для defer.
func setEnv(t *testing.T, kv map[string]string) func() {
	t.Helper()
	// Снимаем все переменные, которые читает резолвер: тесты не должны зависеть
	// от окружения машины, где они запускаются.
	names := []string{
		"AGENT_EMBEDDED", "AGENT_ID", "AGENT_PUBLIC_HOST", "AGENT_PORT",
		"AGENT_EMBEDDED_PORT", "AGENT_WORKER_PORT", "AGENT_MAX_CONCURRENT_REQUESTS",
		"AGENT_WEIGHT", "BALANCER_URL", "BALANCER_TOKEN", "GPU_MODE",
		"METRICS_INTERVAL", "COLLECT_INTERVAL", "HEARTBEAT_INTERVAL",
		"API_TOKEN", "CPPWORKER_API_TOKEN", "BALANCER_API_TOKEN",
		"SDWORKER_BACKEND_ID", "SDWORKER_ADVERTISE_HOST",
		"CPPWORKER_REGISTER_NAME", "CPPWORKER_ADVERTISE_HOST",
	}
	prev := make(map[string]*string, len(names))
	for _, n := range names {
		if v, ok := os.LookupEnv(n); ok {
			value := v
			prev[n] = &value
		} else {
			prev[n] = nil
		}
		_ = os.Unsetenv(n)
	}
	for k, v := range kv {
		_ = os.Setenv(k, v)
	}
	return func() {
		for n, p := range prev {
			if p == nil {
				_ = os.Unsetenv(n)
			} else {
				_ = os.Setenv(n, *p)
			}
		}
	}
}

// TestEmbedded_DisabledByDefault — без AGENT_EMBEDDED резолвер молчит.
func TestEmbedded_DisabledByDefault(t *testing.T) {
	defer setEnv(t, map[string]string{})()
	if EmbeddedEnabled() {
		t.Fatal("EmbeddedEnabled() = true без AGENT_EMBEDDED")
	}
	opts := ResolveEmbeddedOptionsForWorker(types.BackendTypeImage, "SDWORKER_BACKEND_ID", "SDWORKER_ADVERTISE_HOST", 18093)
	if opts != (EmbeddedOptions{}) {
		t.Fatalf("выключенный агент обязан вернуть нулевые опции, получили %+v", opts)
	}
}

// TestEmbedded_ImageWorkerIdentity — image-воркер: id/host/порт из своих env.
func TestEmbedded_ImageWorkerIdentity(t *testing.T) {
	defer setEnv(t, map[string]string{
		"AGENT_EMBEDDED":                "on",
		"SDWORKER_BACKEND_ID":           "imageworker",
		"SDWORKER_ADVERTISE_HOST":       "imageworker",
		"BALANCER_URL":                  "http://loadbalancer:18081",
		"BALANCER_TOKEN":                "tok",
		"CPPWORKER_API_TOKEN":           "cpp-tok",
		"AGENT_MAX_CONCURRENT_REQUESTS": "64",
	})()

	opts := ResolveEmbeddedOptionsForWorker(types.BackendTypeImage, "SDWORKER_BACKEND_ID", "SDWORKER_ADVERTISE_HOST", 18093)
	if opts.AgentID != "imageworker" {
		t.Errorf("AgentID = %q, want imageworker", opts.AgentID)
	}
	if opts.Host != "imageworker" {
		t.Errorf("Host = %q, want imageworker", opts.Host)
	}
	if opts.BackendType != types.BackendTypeImage {
		t.Errorf("BackendType = %q, want image_cpp", opts.BackendType)
	}
	if opts.Port != 18093 {
		t.Errorf("Port = %d, want 18093 (порт воркера)", opts.Port)
	}
	if opts.MetricsPort != types.DefaultEmbeddedAgentPort {
		t.Errorf("MetricsPort = %d, want %d (НЕ порт внешнего агента 18032)",
			opts.MetricsPort, types.DefaultEmbeddedAgentPort)
	}
	if opts.MetricsPort == 18032 {
		t.Error("встроенный агент занял порт внешнего агента — два процесса подерутся за порт")
	}
	if opts.CppWorkerAPIToken != "cpp-tok" {
		t.Errorf("CppWorkerAPIToken = %q, want cpp-tok", opts.CppWorkerAPIToken)
	}
	if opts.MaxConcurrentRequests != 64 {
		t.Errorf("MaxConcurrentRequests = %d, want 64", opts.MaxConcurrentRequests)
	}
}

// TestEmbedded_CppWorkerIdentity — llama.cpp-воркер: свои env, свой backendType.
func TestEmbedded_CppWorkerIdentity(t *testing.T) {
	defer setEnv(t, map[string]string{
		"AGENT_EMBEDDED":           "1",
		"CPPWORKER_REGISTER_NAME":  "cppworker-gpu",
		"CPPWORKER_ADVERTISE_HOST": "cppworker-gpu",
		"BALANCER_URL":             "http://loadbalancer:18081",
	})()

	opts := ResolveEmbeddedOptionsForWorker(types.BackendTypeLlamaCpp, "CPPWORKER_REGISTER_NAME", "CPPWORKER_ADVERTISE_HOST", 18092)
	if opts.AgentID != "cppworker-gpu" {
		t.Errorf("AgentID = %q, want cppworker-gpu", opts.AgentID)
	}
	if opts.Port != 18092 {
		t.Errorf("Port = %d, want 18092", opts.Port)
	}
	cfg := BuildEmbeddedConfig(opts)
	if cfg.BackendType != types.BackendTypeLlamaCpp {
		t.Errorf("BackendType = %q, want llama_cpp", cfg.BackendType)
	}
	if cfg.CppWorkerPort != 18092 {
		t.Errorf("CppWorkerPort = %d, want 18092 (нужен для de-dup по host:port)", cfg.CppWorkerPort)
	}
	if cfg.ImagePort != 0 {
		t.Errorf("ImagePort = %d, want 0 у llama.cpp-воркера", cfg.ImagePort)
	}
}

// TestEmbedded_ImageConfigHasNoCppWorkerPort — у image-агента НЕ должно быть
// CppWorkerPort: с ним балансер ушёл бы в ветку de-dup «приклеить к
// cppworker-бэкенду» и приклеил бы image-агента к ТЕКСТОВОЙ записи.
func TestEmbedded_ImageConfigHasNoCppWorkerPort(t *testing.T) {
	defer setEnv(t, map[string]string{
		"AGENT_EMBEDDED":      "yes",
		"SDWORKER_BACKEND_ID": "imageworker",
		"BALANCER_URL":        "http://loadbalancer:18081",
	})()

	opts := ResolveEmbeddedOptionsForWorker(types.BackendTypeImage, "SDWORKER_BACKEND_ID", "SDWORKER_ADVERTISE_HOST", 18093)
	cfg := BuildEmbeddedConfig(opts)
	if cfg.CppWorkerPort != 0 {
		t.Errorf("CppWorkerPort = %d, want 0 у image-агента", cfg.CppWorkerPort)
	}
	if cfg.ImagePort != 18093 {
		t.Errorf("ImagePort = %d, want 18093", cfg.ImagePort)
	}
	if cfg.BackendType != types.BackendTypeImage {
		t.Errorf("BackendType = %q, want image_cpp", cfg.BackendType)
	}
}

// TestEmbedded_RunEmbedded_NeverFatal — неполный конфиг не должен ронять воркер.
//
// ЖИВОЙ СМЫСЛ: RunEmbedded вызывается из main() воркера. Если бы он паниковал
// или возвращал ошибку, которую main трактует как fatal, отсутствие
// BALANCER_URL обрушило бы inference-воркер. Метрики — вспомогательная функция.
func TestEmbedded_RunEmbedded_NeverFatal(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"нет AGENT_EMBEDDED", map[string]string{}},
		{"нет BALANCER_URL", map[string]string{"AGENT_EMBEDDED": "on", "SDWORKER_BACKEND_ID": "w"}},
		{"пустой AgentID без hostname-фолбэка", map[string]string{"AGENT_EMBEDDED": "on", "BALANCER_URL": "http://x"}},
		{"битый AGENT_EMBEDDED_PORT", map[string]string{
			"AGENT_EMBEDDED": "on", "BALANCER_URL": "http://x",
			"SDWORKER_BACKEND_ID": "w", "AGENT_EMBEDDED_PORT": "not-a-port",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer setEnv(t, tc.env)()
			opts := ResolveEmbeddedOptionsForWorker(types.BackendTypeImage, "SDWORKER_BACKEND_ID", "SDWORKER_ADVERTISE_HOST", 18093)
			// Недоступный балансер: регистрация не удастся, но RunEmbedded обязан
			// вернуть управляемый bundle (или nil) и НЕ паниковать.
			bundle := RunEmbedded(opts)
			if bundle != nil {
				bundle.Stop()
			}
		})
	}
}

// TestEmbedded_Validate_RejectsBadOptions — то, что должно отсекаться до сети.
func TestEmbedded_Validate_RejectsBadOptions(t *testing.T) {
	cases := []struct {
		name string
		opts EmbeddedOptions
	}{
		{"пустой id", EmbeddedOptions{BalancerURL: "http://x", BackendType: types.BackendTypeImage, MetricsPort: 18033}},
		{"пустой balancer", EmbeddedOptions{AgentID: "a", BackendType: types.BackendTypeImage, MetricsPort: 18033}},
		{"чужой backendType", EmbeddedOptions{AgentID: "a", BalancerURL: "http://x", BackendType: "banana", MetricsPort: 18033}},
		{"плохой порт", EmbeddedOptions{AgentID: "a", BalancerURL: "http://x", BackendType: types.BackendTypeImage, MetricsPort: 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.opts.validate(); err == nil {
				t.Fatal("validate() = nil, want error")
			}
		})
	}
}

// TestEmbedded_Validate_AcceptsImageAndLlama — положительный контроль: валидные
// наборы проходят, иначе все проверки выше были бы бессмысленны.
func TestEmbedded_Validate_AcceptsImageAndLlama(t *testing.T) {
	for _, bt := range []types.BackendType{types.BackendTypeImage, types.BackendTypeLlamaCpp} {
		opts := EmbeddedOptions{
			AgentID: "worker", Host: "worker", BalancerURL: "http://lb:18081",
			BackendType: bt, Port: 18093, MetricsPort: types.DefaultEmbeddedAgentPort,
		}
		if err := opts.validate(); err != nil {
			t.Errorf("%s: validate() = %v, want nil", bt, err)
		}
	}
}
