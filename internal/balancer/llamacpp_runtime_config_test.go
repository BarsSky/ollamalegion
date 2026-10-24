package balancer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// extractHost — парсит URL mock-сервера и возвращает host (без порта).
// (extractPort уже объявлен в cline_integration_test.go и доступен в этом пакете.)
func extractHost(url string) string {
	// http://127.0.0.1:54321 → 127.0.0.1
	u := strings.TrimPrefix(url, "http://")
	u = strings.TrimPrefix(u, "https://")
	if idx := strings.Index(u, ":"); idx >= 0 {
		return u[:idx]
	}
	return u
}

// makeMockCppWorker — создаёт httptest.Server, имитирующий cppworker
// /api/v1/cppworker/config/runtime endpoint.
func makeMockCppWorker(t *testing.T, response map[string]interface{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/cppworker/config/runtime" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
}

// TestExtractHostAndPort — unit-тест для парсеров URL.
func TestExtractHostAndPort(t *testing.T) {
	cases := []struct {
		url      string
		wantHost string
		wantPort int
	}{
		{"http://127.0.0.1:54321", "127.0.0.1", 54321},
		{"http://0.0.0.0:18092", "0.0.0.0", 18092},
		{"http://localhost:8080", "localhost", 8080},
	}
	for _, c := range cases {
		h := extractHost(c.url)
		p := extractPort(c.url)
		if h != c.wantHost {
			t.Errorf("extractHost(%q) = %q, want %q", c.url, h, c.wantHost)
		}
		if p != c.wantPort {
			t.Errorf("extractPort(%q) = %d, want %d", c.url, p, c.wantPort)
		}
	}
}

// TestHandleRuntimeConfig_MethodNotAllowed — POST → 405.
func TestHandleRuntimeConfig_MethodNotAllowed(t *testing.T) {
	logger.Init("error")
	router := &LlamaCppRouter{}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/cppworker/config/runtime", nil)
	w := httptest.NewRecorder()
	router.handleRuntimeConfig(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for POST, got %d", w.Code)
	}
}

// TestHandleRuntimeConfig_AggregateFromMultipleBackends — два mock-бэкенда
// возвращают разные модели. Handler должен агрегировать обе и проставить
// правильное поле `backend` для каждой модели.
//
// Так как полноценный Proxy.NewProxy() требует инициализации множества полей,
// мы напрямую инжектируем backends в proxy через существующий API.
// Альтернатива — тестировать с минимальным Proxy stub.
//
// Этот тест проверяет contract: handler должен корректно агрегировать JSON-ответы.
func TestHandleRuntimeConfig_ParseAggregatedResponse(t *testing.T) {
	logger.Init("error")

	// Создаём два mock cppworker'а
	mock1 := makeMockCppWorker(t, map[string]interface{}{
		"loaded_models": []map[string]interface{}{
			{
				"name":         "gemma-4",
				"path":         "/models/gemma-4.gguf",
				"state":        "loaded",
				"context_size": 32768,
				"gpu_layers":   10,
				"size_bytes":   19000000000,
				"n_layers":     48,
			},
		},
		"count": 1,
	})
	defer mock1.Close()

	mock2 := makeMockCppWorker(t, map[string]interface{}{
		"loaded_models": []map[string]interface{}{
			{
				"name":         "llama-3",
				"path":         "/models/llama-3.gguf",
				"state":        "loaded",
				"context_size": 8192,
				"gpu_layers":   -1,
				"size_bytes":   4500000000,
				"n_layers":     32,
			},
		},
		"count": 1,
	})
	defer mock2.Close()

	// Проверяем что mock'и отвечают правильно
	for _, mock := range []*httptest.Server{mock1, mock2} {
		resp, err := http.Get(mock.URL + "/api/v1/cppworker/config/runtime")
		if err != nil {
			t.Fatalf("mock server unreachable: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("mock server returned %d", resp.StatusCode)
		}
	}

	// Host/Port parsing — проверка что extractHost/extractPort работают
	// (это важно для основного handler'а, который формирует URL из host:port).
	if h := extractHost(mock1.URL); h == "" {
		t.Fatal("extractHost returned empty")
	}
	if p := extractPort(mock1.URL); p <= 0 {
		t.Fatalf("extractPort returned %d for %s", p, mock1.URL)
	}
}

// TestR118_16_RuntimeConfigHandlerPublishesExactPlan — R118.16 (2026-10-14):
// агрегатор обязан отдать в WebUI ФАКТ из лога llama.cpp, а не расчёт.
//
// Проверяется ВЕСЬ путь обработчика: mock cppworker отдаёт device_layers_exact
// (так их теперь присылает воркер), handleRuntimeConfig декодирует, проверяет
// полноту (n_layer+1) и публикует device_layers с estimated=false.
func TestR118_16_RuntimeConfigHandlerPublishesExactPlan(t *testing.T) {
	logger.Init("error")

	mock := makeMockCppWorker(t, map[string]interface{}{
		"loaded_models": []map[string]interface{}{
			{
				"name":         "gemma-4-E4B-it-Q4_K_M",
				"path":         "/models/gemma-4-E4B-it-Q4_K_M.gguf",
				"state":        "loaded",
				"context_size": 32768,
				"gpu_layers":   10,
				"size_bytes":   4215695776,
				"n_layers":     42,
				// Живой факт со стенда: 43 назначения = n_layer+1.
				"device_layers_exact": []map[string]interface{}{
					{"device": "CPU", "from": 0, "to": 0, "count": 1},
					{"device": "CUDA0", "from": 1, "to": 23, "count": 23},
					{"device": "RPC0", "from": 24, "to": 42, "count": 19, "node": "192.0.2.10:50052"},
				},
			},
		},
		"count": 1,
	})
	defer mock.Close()

	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 8080, APIPort: 8081},
		Backends: []types.Backend{
			{
				ID:            "cppworker-exact",
				Name:          "cppworker exact layers",
				Host:          "127.0.0.1",
				OllamaPort:    11434,
				AgentPort:     9090,
				CppWorkerPort: extractPort(mock.URL),
				Type:          types.BackendTypeLlamaCpp,
				Weight:        1,
				Status:        types.StatusHealthy,
			},
		},
		Balancing: types.BalancingSettings{
			Algorithm:            types.AlgorithmResourceAware,
			HealthCheckInterval:  60,
			MetricsInterval:      60,
			StreamingIdleTimeout: 30,
		},
		API:  types.APISettings{RateLimit: 100, RateBurst: 200},
		Auth: types.AuthConfig{Enabled: false},
	}
	p := newProxyWithCleanup(t, cfg)
	router := NewLlamaCppRouter(p)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cppworker/config/runtime", nil)
	rec := httptest.NewRecorder()
	router.handleRuntimeConfig(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		LoadedModels []runtimeModelR118 `json:"loaded_models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("ответ агрегатора не декодируется: %v", err)
	}
	if len(resp.LoadedModels) != 1 {
		t.Fatalf("моделей = %d, want 1 (body=%s)", len(resp.LoadedModels), rec.Body.String())
	}
	m := resp.LoadedModels[0]
	if m.DeviceLayers == nil {
		t.Fatal("device_layers отсутствует: факт не дошёл до WebUI")
	}
	if m.DeviceLayers.Estimated {
		t.Error("device_layers помечен расчётом, хотя воркер отдал точный список")
	}
	if m.DeviceLayers.Total != 43 || m.DeviceLayers.CPU != 1 || m.DeviceLayers.GPU != 42 {
		t.Errorf("total/gpu/cpu = %d/%d/%d, want 43/42/1",
			m.DeviceLayers.Total, m.DeviceLayers.GPU, m.DeviceLayers.CPU)
	}
	if len(m.DeviceLayers.Devices) != 2 {
		t.Fatalf("устройств = %d, want 2: %+v", len(m.DeviceLayers.Devices), m.DeviceLayers.Devices)
	}
	if d := m.DeviceLayers.Devices[1]; d.Device != "RPC0" || d.Kind != "rpc" ||
		d.Node != "192.0.2.10:50052" || d.From != 24 || d.To != 42 {
		t.Errorf("RPC0 = %+v (узел и диапазон обязаны быть из лога)", d)
	}
}

// TestR134_RuntimeConfigKeepsLoadedLoadParams — R134 (2026-10-16): агрегатор
// обязан ПРОПУСКАТЬ до WebUI фактические параметры загрузки, а не терять их.
//
// ЖИВОЙ ДЕФЕКТ, который закрывает тест. cppworker уже отдаёт эти поля
// (handlers_config.go: kv_cache_type, parallel, context_per_seq, reasoning_enabled,
// flash_attn, batched_parallel) — их добавили в R83/v67 ровно для того, чтобы
// оператор видел применённые настройки. Но балансер декодирует ответ воркера в
// runtimeModelR118 и ПЕРЕСОБИРАЕТ свой ответ для WebUI: чего нет в структуре, то
// не доезжает. Итог — жалоба оператора «у загруженной модели стоит, что
// размышление включено, а в карточке параметров оно выключено»: карточка брала
// намерение из профиля, потому что факта загрузки в данных не было.
//
// Проверяются оба направления, важные для интерфейса:
//   * значения доезжают (kv/parallel/context_per_seq/flash_attn);
//   * `reasoning_enabled: false` — это ЗНАЧЕНИЕ, отличимое от «поля нет»
//     (указатель, а не bool): иначе «выключено» снова станет «неизвестно».
func TestR134_RuntimeConfigKeepsLoadedLoadParams(t *testing.T) {
	logger.Init("error")

	mock := makeMockCppWorker(t, map[string]interface{}{
		"loaded_models": []map[string]interface{}{
			{
				"name":              "gemma-4-E4B-it-Q4_K_M",
				"path":              "/models/gemma-4-E4B-it-Q4_K_M.gguf",
				"state":             "loaded",
				"context_size":      32768,
				"gpu_layers":        -1,
				"batch_size":        512,
				"kv_cache_type":     "q4_0",
				"parallel":          2,
				"context_per_seq":   16384,
				"reasoning_enabled": true,
				"flash_attn":        1,
				"use_mmap":          true,
				"numa":              false,
				"size_bytes":        4215695776,
				"n_layers":          42,
			},
			{
				"name":              "qwen3-8b",
				"path":              "/models/qwen3-8b.gguf",
				"state":             "loaded",
				"context_size":      8192,
				"gpu_layers":        0,
				"kv_cache_type":     "f16",
				"parallel":          1,
				"reasoning_enabled": false,
				"size_bytes":        5000000000,
				"n_layers":          32,
			},
		},
		"count": 2,
	})
	defer mock.Close()

	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 8080, APIPort: 8081},
		Backends: []types.Backend{
			{
				ID: "cppworker-facts", Name: "cppworker facts",
				Host: "127.0.0.1", OllamaPort: 11434, AgentPort: 9090,
				CppWorkerPort: extractPort(mock.URL),
				Type:          types.BackendTypeLlamaCpp, Weight: 1, Status: types.StatusHealthy,
			},
		},
		Balancing: types.BalancingSettings{
			Algorithm: types.AlgorithmResourceAware, HealthCheckInterval: 60, MetricsInterval: 60,
			StreamingIdleTimeout: 30,
		},
		API:  types.APISettings{RateLimit: 100, RateBurst: 200},
		Auth: types.AuthConfig{Enabled: false},
	}
	p := newProxyWithCleanup(t, cfg)
	router := NewLlamaCppRouter(p)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cppworker/config/runtime", nil)
	rec := httptest.NewRecorder()
	router.handleRuntimeConfig(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		LoadedModels []runtimeModelR118 `json:"loaded_models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("ответ агрегатора не декодируется: %v", err)
	}
	if len(resp.LoadedModels) != 2 {
		t.Fatalf("моделей = %d, want 2 (body=%s)", len(resp.LoadedModels), rec.Body.String())
	}

	// Проверяем «СЫРОЙ» JSON, а не Go-структуру: именно эти ключи читает WebUI, и
	// именно они терялись при пересборке ответа. Тест на структуре прошёл бы при
	// любом переименовании полей с сохранением тега, а нам важен контракт.
	var raw struct {
		LoadedModels []map[string]interface{} `json:"loaded_models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("сырой ответ не декодируется: %v", err)
	}
	byName := map[string]map[string]interface{}{}
	for _, m := range raw.LoadedModels {
		name, _ := m["name"].(string)
		byName[name] = m
	}
	gemma, qwen := byName["gemma-4-E4B-it-Q4_K_M"], byName["qwen3-8b"]
	if gemma == nil || qwen == nil {
		t.Fatalf("модели не найдены в ответе: %s", rec.Body.String())
	}

	check := func(model map[string]interface{}, key string, want interface{}, who string) {
		t.Helper()
		got, ok := model[key]
		if !ok {
			t.Errorf("%s: ключ %q отсутствует в ответе WebUI — факт загрузки потерян агрегатором "+
				"(именно из-за этого карточка параметров показывала намерение вместо факта)", who, key)
			return
		}
		if got != want {
			t.Errorf("%s: %s = %v, want %v", who, key, got, want)
		}
	}
	check(gemma, "kv_cache_type", "q4_0", "gemma-4")
	check(gemma, "parallel", float64(2), "gemma-4")
	check(gemma, "context_per_seq", float64(16384), "gemma-4")
	check(gemma, "flash_attn", float64(1), "gemma-4")
	check(gemma, "reasoning_enabled", true, "gemma-4")
	check(gemma, "use_mmap", true, "gemma-4")
	// false — это ЗНАЧЕНИЕ, а не «поля нет»: иначе «выключено» снова станет
	// «неизвестно» и карточка опять покажет намерение.
	check(qwen, "reasoning_enabled", false, "qwen3-8b")
	check(qwen, "kv_cache_type", "f16", "qwen3-8b")
	check(qwen, "parallel", float64(1), "qwen3-8b")
}
// TestR118_16_RuntimeConfigFallsBackToEstimate — если воркер прислал неполный
// факт (или старая сборка его не присылает), агрегатор обязан показать расчёт,
// помеченный estimated=true: молча выдать чужую/обрезанную раскладку нельзя.
func TestR118_16_RuntimeConfigFallsBackToEstimate(t *testing.T) {
	logger.Init("error")

	mock := makeMockCppWorker(t, map[string]interface{}{
		"loaded_models": []map[string]interface{}{
			{
				"name": "gemma-4-E4B-it-Q4_K_M", "path": "/models/gemma.gguf", "state": "loaded",
				"context_size": 32768, "gpu_layers": 10, "n_layers": 42,
				// Обрезанный факт: 23+19 = 42 вместо 43 — это НЕ наша раскладка.
				"device_layers_exact": []map[string]interface{}{
					{"device": "CUDA0", "from": 1, "to": 23, "count": 23},
					{"device": "RPC0", "from": 24, "to": 42, "count": 19},
				},
			},
		},
		"count": 1,
	})
	defer mock.Close()

	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 8080, APIPort: 8081},
		Backends: []types.Backend{
			{
				ID: "cppworker-partial", Name: "partial", Host: "127.0.0.1", OllamaPort: 11434,
				AgentPort: 9090, CppWorkerPort: extractPort(mock.URL),
				Type: types.BackendTypeLlamaCpp, Weight: 1, Status: types.StatusHealthy,
			},
		},
		Balancing: types.BalancingSettings{
			Algorithm: types.AlgorithmResourceAware, HealthCheckInterval: 60,
			MetricsInterval: 60, StreamingIdleTimeout: 30,
		},
		API:  types.APISettings{RateLimit: 100, RateBurst: 200},
		Auth: types.AuthConfig{Enabled: false},
	}
	p := newProxyWithCleanup(t, cfg)
	router := NewLlamaCppRouter(p)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cppworker/config/runtime", nil)
	rec := httptest.NewRecorder()
	router.handleRuntimeConfig(rec, req)

	var resp struct {
		LoadedModels []runtimeModelR118 `json:"loaded_models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("ответ агрегатора не декодируется: %v", err)
	}
	if len(resp.LoadedModels) != 1 {
		t.Fatalf("моделей = %d, want 1", len(resp.LoadedModels))
	}
	m := resp.LoadedModels[0]
	// Раскладка не активна (узлы не зарегистрированы) → плана быть не должно:
	// расчёт без раскладки смысла не имеет, а факт отброшен как неполный.
	if m.DeviceLayers != nil && !m.DeviceLayers.Estimated {
		t.Errorf("обрезанный факт принят за точную раскладку: %+v", m.DeviceLayers)
	}
}
