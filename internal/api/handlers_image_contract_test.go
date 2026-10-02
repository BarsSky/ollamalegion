// handlers_image_contract_test.go — Phase 4 (discovery): тесты HTTP-контракта.
//
// Покрывают обязательные утверждения задачи:
//   - GET /api/v1/image/contract → 200 и ВСЕ обязательные разделы;
//   - tool валиден как JSON-Schema (type=object, properties.prompt, required=[prompt]);
//   - примеры curl собраны с ФАКТИЧЕСКИМ портом из конфига;
//   - GET /api/v1/image/capabilities в кластере без image-бэкендов → 200, total=0;
//   - обе ручки закрыты AuthMiddleware (401 без токена при включённом auth)
//     и принимают только GET.
//
// Декодируем JSON в АНОНИМНЫЕ структуры (а не в imageContractResponse): тест
// обязан проверять ПРОВОД (имена полей), а не только поля Go-структур.
package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// imageContractWorkerCapsJSON — ответ воркера в РЕАЛЬНОЙ форме
// GET /api/image/capabilities (samplers внутри engine, лимиты 64…2048).
const imageContractWorkerCapsJSON = `{
  "ready": true, "state": "loaded", "model": "z-image-turbo-q3-k", "pid": 777,
  "engine": {
    "samplers": ["euler", "euler_a", "lcm"],
    "schedulers": ["discrete", "smoothstep"],
    "loras": [{"name": "detail-tweaker", "path": "/loras/detail.safetensors"}],
    "upscalers": [{"name": "esrgan-x4", "model": true, "image_upscale": false}]
  },
  "models": [{
    "name": "z-image-turbo-q3-k", "state": "loaded", "family": "z_image",
    "size_bytes": 3355000000, "vram_estimate_mb": 4096, "active_queries": 0,
    "defaults": {"steps": 8, "cfgScale": 1, "sampler": "euler", "scheduler": "smoothstep",
                 "width": 512, "height": 1024, "batchCount": 1, "seed": -1}
  }],
  "limits": {
    "min_width": 64, "max_width": 2048, "min_height": 64, "max_height": 2048,
    "size_multiple": 64, "max_batch_count": 8, "min_steps": 1, "max_steps": 100,
    "max_cfg_scale": 30, "queue_size": 64, "queue_in_flight": 0,
    "completed_job_ttl_seconds": 600, "generation_timeout_seconds": 600,
    "seed_always_positive": true, "supports_response_format_url": true, "cancel_queued_only": true
  },
  "warnings": []
}`

// newImageContractWorker — мок воркера с контрактной формой capabilities.
func newImageContractWorker(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/image/capabilities", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(imageContractWorkerCapsJSON))
	})
	mux.HandleFunc("/api/image/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"name":"z-image-turbo-q3-k","state":"loaded"}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// createImageContractTestServer — сервер с одним image_cpp-бэкендом на мок.
// mutate позволяет поменять конфиг (порты, auth) до сборки Server.
func createImageContractTestServer(
	t *testing.T,
	workerURL string,
	mutate func(*types.LoadBalancerConfig),
) (*httptest.Server, *Server) {
	t.Helper()

	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host: "localhost", Port: 18080, APIPort: 18081, OpenAIPort: 18079,
		},
		Backends: []types.Backend{
			{
				ID: "img_contract", Name: "Mock Image Worker", Host: "127.0.0.1",
				ImagePort: portFromURL(t, workerURL), Type: types.BackendTypeImage,
				Weight: 1, MaxConcurrentReqs: 2, Status: types.StatusHealthy,
			},
		},
		Balancing: types.BalancingSettings{
			Algorithm: types.AlgorithmResourceAware, HealthCheckInterval: 10,
			MetricsInterval: 5, RequestTimeout: 30, QueueTimeout: 60,
			QueueMaxSize: 100, QueueWorkers: 4,
		},
		API:  types.APISettings{RateLimit: 1000, RateBurst: 2000},
		Auth: types.AuthConfig{Enabled: false},
	}
	if mutate != nil {
		mutate(config)
	}

	proxy := balancer.NewProxy(config)
	hc := balancer.NewHealthChecker(proxy, 10*time.Second, 3)
	srv := NewServer(proxy, config, hc)
	return httptest.NewServer(srv), srv
}

// getJSON — GET и декодирование тела в произвольную структуру.
func getJSON(t *testing.T, url string, headers map[string]string, out interface{}) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	if out != nil {
		require.NoError(t, json.Unmarshal(body, out), "body: %s", string(body))
	}
	return resp.StatusCode, body
}

// ============================================================
// 1. GET /api/v1/image/contract — обязательные разделы
// ============================================================

func TestImageContract_MandatorySections(t *testing.T) {
	balancer.ResetImageCapabilitiesCache()
	t.Cleanup(balancer.ResetImageCapabilitiesCache)

	worker := newImageContractWorker(t)
	server, _ := createImageContractTestServer(t, worker.URL, nil)
	defer server.Close()

	var raw map[string]json.RawMessage
	status, body := getJSON(t, server.URL+"/api/v1/image/contract", nil, &raw)
	require.Equal(t, http.StatusOK, status, "body: %s", string(body))

	for _, section := range []string{
		"generatedAt", "engine_revision", "ports", "auth", "limits", "models",
		"discovery", "endpoints", "requestFields", "examples", "tool", "toolInstructions",
	} {
		assert.Contains(t, raw, section, "обязательный раздел %q отсутствует", section)
	}

	// --- ports: фактические значения из конфига -------------------------------
	var ports struct {
		OpenAI        int  `json:"openai"`
		Proxy         int  `json:"proxy"`
		API           int  `json:"api"`
		Worker        int  `json:"worker"`
		OpenAIEnabled bool `json:"openaiEnabled"`
	}
	require.NoError(t, json.Unmarshal(raw["ports"], &ports))
	assert.Equal(t, 18079, ports.OpenAI)
	assert.Equal(t, 18080, ports.Proxy)
	assert.Equal(t, 18081, ports.API)
	assert.Equal(t, types.DefaultImageWorkerPort, ports.Worker)
	assert.True(t, ports.OpenAIEnabled)

	// --- limits: из агрегата + фиксированные ----------------------------------
	var limits struct {
		MinWidth            int  `json:"min_width"`
		MaxWidth            int  `json:"max_width"`
		MinHeight           int  `json:"min_height"`
		MaxHeight           int  `json:"max_height"`
		SizeMultiple        int  `json:"size_multiple"`
		MaxBatchCount       int  `json:"max_batch_count"`
		MaxQueueSize        int  `json:"max_queue_size"`
		QueueSizePerBackend int  `json:"queue_size_per_backend"`
		ResultTTLSeconds    int  `json:"result_ttl_seconds"`
		CancelQueuedOnly    bool `json:"cancel_queued_only"`
		StepProgress        bool `json:"step_progress"`
		SeedAlwaysPositive  bool `json:"seed_always_positive"`
	}
	require.NoError(t, json.Unmarshal(raw["limits"], &limits))
	assert.Equal(t, 64, limits.MinWidth, "нижняя граница — из агрегата")
	assert.Equal(t, 2048, limits.MaxWidth, "верхняя граница — из агрегата воркера, а не хардкод 4096")
	assert.Equal(t, 64, limits.MinHeight)
	assert.Equal(t, 2048, limits.MaxHeight)
	assert.Equal(t, 64, limits.SizeMultiple)
	assert.Equal(t, 8, limits.MaxBatchCount)
	assert.Equal(t, 64, limits.MaxQueueSize, "сумма очередей = 64 (один воркер)")
	assert.Equal(t, 64, limits.QueueSizePerBackend)
	assert.Equal(t, 600, limits.ResultTTLSeconds)
	assert.True(t, limits.CancelQueuedOnly, "отмена только queued")
	assert.False(t, limits.StepProgress, "прогресса шагов нет")
	assert.True(t, limits.SeedAlwaysPositive)

	// --- models: из агрегата ---------------------------------------------------
	var models struct {
		AllModels []string `json:"all_models"`
		Available bool     `json:"available"`
		Models    []struct {
			BackendID string `json:"backendId"`
			Name      string `json:"name"`
			Family    string `json:"family"`
			State     string `json:"state"`
			Defaults  struct {
				Steps int `json:"steps"`
			} `json:"defaults"`
		} `json:"models"`
	}
	require.NoError(t, json.Unmarshal(raw["models"], &models))
	assert.True(t, models.Available)
	assert.Equal(t, []string{"z-image-turbo-q3-k"}, models.AllModels)
	require.Len(t, models.Models, 1)
	assert.Equal(t, "img_contract", models.Models[0].BackendID)
	assert.Equal(t, "z_image", models.Models[0].Family)
	assert.Equal(t, "loaded", models.Models[0].State)
	assert.Equal(t, 8, models.Models[0].Defaults.Steps, "дефолты модели обязаны доехать")

	// --- endpoints: обязательные ручки с методами ------------------------------
	var endpoints []struct {
		Method   string `json:"method"`
		Path     string `json:"path"`
		Port     string `json:"port"`
		Audience string `json:"audience"`
		Purpose  string `json:"purpose"`
	}
	require.NoError(t, json.Unmarshal(raw["endpoints"], &endpoints))
	byKey := map[string]string{} // "METHOD PATH" → audience
	for _, e := range endpoints {
		byKey[e.Method+" "+e.Path] = e.Audience
		assert.NotEmpty(t, e.Purpose)
	}
	for _, want := range []string{
		http.MethodPost + " /v1/images/generations",
		http.MethodPost + " /sdapi/v1/txt2img",
		http.MethodPost + " /api/image/generate",
		http.MethodGet + " /api/image/jobs/{id}",
		http.MethodGet + " /v1/models",
		http.MethodGet + " /api/v1/image/capabilities",
		http.MethodGet + " /api/v1/image/contract",
	} {
		assert.Contains(t, byKey, want, "обязательная ручка %q отсутствует", want)
	}
	assert.Equal(t, "client", byKey[http.MethodPost+" /v1/images/generations"])
	assert.Equal(t, "client", byKey[http.MethodPost+" /sdapi/v1/txt2img"])
	assert.Equal(t, "management", byKey[http.MethodPost+" /api/image/generate"])

	// --- requestFields: нормализация описана ----------------------------------
	var fields []struct {
		Name          string      `json:"name"`
		Type          string      `json:"type"`
		Required      bool        `json:"required"`
		Minimum       interface{} `json:"minimum"`
		Maximum       interface{} `json:"maximum"`
		MultipleOf    interface{} `json:"multipleOf"`
		Enum          []string    `json:"enum"`
		Ignored       bool        `json:"ignored"`
		Normalization string      `json:"normalization"`
	}
	require.NoError(t, json.Unmarshal(raw["requestFields"], &fields))
	byName := map[string]int{}
	for i, f := range fields {
		byName[f.Name] = i
		assert.NotEmpty(t, f.Normalization, "поле %q без описания нормализации", f.Name)
	}
	for _, want := range []string{
		"prompt", "model", "size", "width", "height", "steps", "cfg_scale",
		"seed", "negative_prompt", "n", "response_format", "output_format",
		"output_compression", "sampler", "scheduler",
	} {
		assert.Contains(t, byName, want, "поле запроса %q не описано", want)
	}
	prompt := fields[byName["prompt"]]
	assert.True(t, prompt.Required, "prompt обязан быть обязательным")
	assert.Equal(t, "string", prompt.Type)

	width := fields[byName["width"]]
	assert.EqualValues(t, 64, width.Minimum)
	assert.EqualValues(t, 2048, width.Maximum, "границы width — из агрегата")
	assert.EqualValues(t, 64, width.MultipleOf, "кратность 64 обязана быть описана")

	seed := fields[byName["seed"]]
	assert.Contains(t, seed.Normalization, "положительн",
		"нормализация seed (random positive) обязана быть описана")
	assert.Contains(t, fields[byName["size"]].Normalization, "64")
	assert.Contains(t, fields[byName["size"]].Normalization, "4096",
		"у size в описании явно указаны абсолютные границы clamp 64…4096")
	assert.Contains(t, fields[byName["size"]].Normalization, "2048",
		"у size указаны и фактические границы живого кластера")
	assert.Contains(t, fields[byName["n"]].Normalization, "1")

	// Лишние OpenAI-поля помечены ignored.
	for _, extra := range []string{"quality", "style", "user", "background"} {
		require.Contains(t, byName, extra)
		assert.True(t, fields[byName[extra]].Ignored, "%s обязан быть помечен ignored", extra)
	}

	// --- enum'ы sampler/scheduler берутся из агрегата --------------------------
	assert.ElementsMatch(t, []string{"euler", "euler_a", "lcm"}, fields[byName["sampler"]].Enum)
	assert.ElementsMatch(t, []string{"discrete", "smoothstep"}, fields[byName["scheduler"]].Enum)
}

// ============================================================
// 2. tool — структурно валидная JSON-Schema
// ============================================================

func TestImageContract_ToolSchemaIsValid(t *testing.T) {
	balancer.ResetImageCapabilitiesCache()
	t.Cleanup(balancer.ResetImageCapabilitiesCache)

	worker := newImageContractWorker(t)
	server, _ := createImageContractTestServer(t, worker.URL, nil)
	defer server.Close()

	var payload struct {
		Tool struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Parameters  struct {
				Type       string `json:"type"`
				Required   []string
				Properties map[string]struct {
					Type        string   `json:"type"`
					Description string   `json:"description"`
					Minimum     *float64 `json:"minimum"`
					Maximum     *float64 `json:"maximum"`
					MultipleOf  *float64 `json:"multipleOf"`
					Enum        []string `json:"enum"`
				} `json:"properties"`
				AdditionalProperties bool `json:"additionalProperties"`
			} `json:"parameters"`
		} `json:"tool"`
		ToolInstructions struct {
			URL           string   `json:"url"`
			Method        string   `json:"method"`
			ResponsePath  string   `json:"responsePath"`
			ImageEncoding string   `json:"imageEncoding"`
			Steps         []string `json:"steps"`
		} `json:"toolInstructions"`
	}

	status, body := getJSON(t, server.URL+"/api/v1/image/contract", nil, &payload)
	require.Equal(t, http.StatusOK, status, "body: %s", string(body))

	assert.Equal(t, "generate_image", payload.Tool.Name)
	assert.NotEmpty(t, payload.Tool.Description)

	// --- структурная валидность JSON-Schema -----------------------------------
	params := payload.Tool.Parameters
	assert.Equal(t, "object", params.Type, "parameters.type обязан быть object")
	require.Contains(t, params.Properties, "prompt", "properties.prompt обязателен")
	assert.Equal(t, "string", params.Properties["prompt"].Type)
	assert.Contains(t, params.Required, "prompt", "required обязан содержать prompt")
	assert.False(t, params.AdditionalProperties, "лишние аргументы должны отсекаться схемой")

	for _, name := range []string{"prompt", "negative_prompt", "width", "height", "steps", "cfg", "seed", "model"} {
		prop, ok := params.Properties[name]
		require.True(t, ok, "в схеме инструмента нет поля %q", name)
		assert.NotEmpty(t, prop.Description, "у поля %q нет описания (LLM его не поймёт)", name)
	}

	width := params.Properties["width"]
	require.NotNil(t, width.Minimum)
	require.NotNil(t, width.Maximum)
	require.NotNil(t, width.MultipleOf)
	assert.Equal(t, float64(64), *width.Minimum)
	assert.Equal(t, float64(2048), *width.Maximum)
	assert.Equal(t, float64(64), *width.MultipleOf)
	assert.Equal(t, "integer", width.Type)
	assert.Equal(t, "number", params.Properties["cfg"].Type)
	// enum моделей подставляется из агрегата.
	assert.Equal(t, []string{"z-image-turbo-q3-k"}, params.Properties["model"].Enum)

	// --- инструкция агенту ------------------------------------------------------
	assert.Contains(t, payload.ToolInstructions.URL, ":18079/v1/images/generations")
	assert.Equal(t, http.MethodPost, payload.ToolInstructions.Method)
	assert.Contains(t, payload.ToolInstructions.ResponsePath, "b64_json")
	assert.Contains(t, payload.ToolInstructions.ImageEncoding, "base64")
	assert.GreaterOrEqual(t, len(payload.ToolInstructions.Steps), 3)
}

// ============================================================
// 3. Примеры — с фактическим портом из конфига
// ============================================================

func TestImageContract_ExamplesCarryActualPorts(t *testing.T) {
	balancer.ResetImageCapabilitiesCache()
	t.Cleanup(balancer.ResetImageCapabilitiesCache)

	worker := newImageContractWorker(t)
	// Порты заданы НЕ дефолтные: примеры обязаны следовать конфигу.
	server, _ := createImageContractTestServer(t, worker.URL, func(c *types.LoadBalancerConfig) {
		c.LoadBalancer.Port = 28080
		c.LoadBalancer.APIPort = 28081
		c.LoadBalancer.OpenAIPort = 28079
	})
	defer server.Close()

	var payload struct {
		Examples []struct {
			ID     string `json:"id"`
			Title  string `json:"title"`
			Method string `json:"method"`
			URL    string `json:"url"`
			Curl   string `json:"curl"`
		} `json:"examples"`
		Discovery map[string]string `json:"discovery"`
	}
	status, body := getJSON(t, server.URL+"/api/v1/image/contract", nil, &payload)
	require.Equal(t, http.StatusOK, status, "body: %s", string(body))

	require.GreaterOrEqual(t, len(payload.Examples), 3, "нужно минимум 3 curl-примера")

	byID := map[string]string{}
	for _, ex := range payload.Examples {
		byID[ex.ID] = ex.Curl
		assert.Contains(t, ex.Curl, "curl", "пример %q обязан быть curl-командой", ex.ID)
		assert.NotEmpty(t, ex.Title)
	}

	require.Contains(t, byID, "openai-images")
	assert.Contains(t, byID["openai-images"], "http://localhost:28079/v1/images/generations",
		"OpenAI-пример обязан использовать фактический openAiPort из конфига")
	require.Contains(t, byID, "a1111-txt2img")
	assert.Contains(t, byID["a1111-txt2img"], "http://localhost:28079/sdapi/v1/txt2img")
	require.Contains(t, byID, "native-job-status")
	assert.Contains(t, byID["native-job-status"], "http://localhost:28080/api/image/jobs/",
		"пример управления job'ом идёт через proxy-порт")
	assert.NotContains(t, byID["openai-images"], "18079", "дефолтный порт не должен просачиваться")

	assert.Contains(t, payload.Discovery["contract"], ":28081/api/v1/image/contract")
	assert.Contains(t, payload.Discovery["openAIImages"], ":28079/v1/images/generations")
}

// ============================================================
// 4. GET /api/v1/image/capabilities — агрегат
// ============================================================

func TestImageCapabilities_AggregatesHealthyBackend(t *testing.T) {
	balancer.ResetImageCapabilitiesCache()
	t.Cleanup(balancer.ResetImageCapabilitiesCache)

	worker := newImageContractWorker(t)
	server, _ := createImageContractTestServer(t, worker.URL, nil)
	defer server.Close()

	var payload struct {
		GeneratedAt     string `json:"generatedAt"`
		Cached          bool   `json:"cached"`
		CacheTTLSeconds int    `json:"cache_ttl_seconds"`
		Backends        struct {
			Total    int `json:"total"`
			Probed   int `json:"probed"`
			Answered int `json:"answered"`
			Failed   int `json:"failed"`
			Probes   []struct {
				BackendID string `json:"backendId"`
				OK        bool   `json:"ok"`
				Source    string `json:"source"`
			} `json:"probes"`
		} `json:"backends"`
		Samplers []struct {
			Name     string   `json:"name"`
			Backends []string `json:"backends"`
		} `json:"samplers"`
		Loras     []struct{ Name string } `json:"loras"`
		Upscalers []struct{ Name string } `json:"upscalers"`
		AllModels []string                `json:"all_models"`
		Limits    struct {
			MaxWidth     int `json:"max_width"`
			MaxQueueSize int `json:"max_queue_size"`
		} `json:"limits"`
	}

	status, body := getJSON(t, server.URL+"/api/v1/image/capabilities", nil, &payload)
	require.Equal(t, http.StatusOK, status, "body: %s", string(body))
	require.NotEmpty(t, payload.GeneratedAt, "generatedAt обязателен")

	assert.Equal(t, 1, payload.Backends.Total)
	assert.Equal(t, 1, payload.Backends.Probed)
	assert.Equal(t, 1, payload.Backends.Answered)
	assert.Equal(t, 0, payload.Backends.Failed)
	require.Len(t, payload.Backends.Probes, 1)
	assert.Equal(t, "img_contract", payload.Backends.Probes[0].BackendID)
	assert.True(t, payload.Backends.Probes[0].OK)
	assert.Equal(t, "worker", payload.Backends.Probes[0].Source)

	require.Len(t, payload.Samplers, 3)
	assert.Equal(t, []string{"img_contract"}, payload.Samplers[0].Backends,
		"источник каждой возможности обязан быть указан")
	assert.Len(t, payload.Loras, 1)
	assert.Len(t, payload.Upscalers, 1)
	assert.Equal(t, []string{"z-image-turbo-q3-k"}, payload.AllModels)
	assert.Equal(t, 2048, payload.Limits.MaxWidth)
	assert.Equal(t, 64, payload.Limits.MaxQueueSize)

	// Второй вызов в пределах TTL — из кэша (страница/агент не долбят воркеры).
	var cached struct {
		Cached bool `json:"cached"`
	}
	status2, _ := getJSON(t, server.URL+"/api/v1/image/capabilities", nil, &cached)
	require.Equal(t, http.StatusOK, status2)
	assert.True(t, cached.Cached, "второй вызов обязан прийти из TTL-кэша")
}

// ============================================================
// 5. Кластер без image-бэкендов → 200 и total=0
// ============================================================

func TestImageCapabilities_EmptyCluster(t *testing.T) {
	balancer.ResetImageCapabilitiesCache()
	t.Cleanup(balancer.ResetImageCapabilitiesCache)

	// Кластер только с текстовым (llama_cpp) бэкендом — image_cpp нет вовсе.
	server, _ := createProxyTestServer(t, "http://127.0.0.1:9")
	defer server.Close()

	var raw map[string]json.RawMessage
	status, body := getJSON(t, server.URL+"/api/v1/image/capabilities", nil, &raw)
	require.Equal(t, http.StatusOK, status, "пустой кластер — это не ошибка: body %s", string(body))

	var agg struct {
		Backends struct {
			Total    int `json:"total"`
			Probed   int `json:"probed"`
			Answered int `json:"answered"`
			Failed   int `json:"failed"`
		} `json:"backends"`
	}
	require.NoError(t, json.Unmarshal(body, &agg))
	assert.Equal(t, 0, agg.Backends.Total)
	assert.Equal(t, 0, agg.Backends.Probed)
	assert.Equal(t, 0, agg.Backends.Answered)
	assert.Equal(t, 0, agg.Backends.Failed)

	// Пустые МАССИВЫ, а не null: клиент не должен ловить null.
	assert.NotContains(t, string(body), "null",
		"в пустом агрегате не должно быть null — только пустые массивы")

	// Контракт в пустом кластере тоже валиден (границы — дефолты движка).
	var contract struct {
		Limits struct {
			MinWidth     int `json:"min_width"`
			MaxWidth     int `json:"max_width"`
			MaxQueueSize int `json:"max_queue_size"`
			ResultTTLSec int `json:"result_ttl_seconds"`
		} `json:"limits"`
		Models struct {
			Available bool     `json:"available"`
			AllModels []string `json:"all_models"`
		} `json:"models"`
		Tool struct {
			Parameters struct {
				Type       string                     `json:"type"`
				Properties map[string]json.RawMessage `json:"properties"`
				Required   []string                   `json:"required"`
			} `json:"parameters"`
		} `json:"tool"`
	}
	status2, body2 := getJSON(t, server.URL+"/api/v1/image/contract", nil, &contract)
	require.Equal(t, http.StatusOK, status2, "body: %s", string(body2))
	assert.Equal(t, 64, contract.Limits.MinWidth)
	assert.Equal(t, 4096, contract.Limits.MaxWidth, "без бэкендов — дефолтные границы движка")
	assert.Equal(t, 0, contract.Limits.MaxQueueSize, "сумма очередей пустого кластера = 0")
	assert.Equal(t, 600, contract.Limits.ResultTTLSec)
	assert.False(t, contract.Models.Available)
	assert.Empty(t, contract.Models.AllModels)
	assert.Equal(t, "object", contract.Tool.Parameters.Type)
	assert.Contains(t, contract.Tool.Parameters.Properties, "prompt")
	assert.Equal(t, []string{"prompt"}, contract.Tool.Parameters.Required)
}

// ============================================================
// 6. Middleware: auth + только GET
// ============================================================

func TestImageContractRoutes_RequireAuth(t *testing.T) {
	balancer.ResetImageCapabilitiesCache()
	t.Cleanup(balancer.ResetImageCapabilitiesCache)

	worker := newImageContractWorker(t)
	server, _ := createImageContractTestServer(t, worker.URL, func(c *types.LoadBalancerConfig) {
		c.Auth = types.AuthConfig{Enabled: true, Tokens: []string{"secret-token"}}
	})
	defer server.Close()

	for _, path := range []string{"/api/v1/image/contract", "/api/v1/image/capabilities"} {
		status, _ := getJSON(t, server.URL+path, nil, nil)
		assert.Equal(t, http.StatusUnauthorized, status,
			"%s обязан требовать токен (management-плоскость)", path)

		status, body := getJSON(t, server.URL+path, map[string]string{"X-API-Token": "secret-token"}, nil)
		assert.Equal(t, http.StatusOK, status, "%s с токеном обязан отвечать 200: %s", path, string(body))
	}
}

func TestImageContractRoutes_RejectNonGET(t *testing.T) {
	balancer.ResetImageCapabilitiesCache()
	t.Cleanup(balancer.ResetImageCapabilitiesCache)

	worker := newImageContractWorker(t)
	server, _ := createImageContractTestServer(t, worker.URL, nil)
	defer server.Close()

	for _, path := range []string{"/api/v1/image/contract", "/api/v1/image/capabilities"} {
		resp, err := http.Post(server.URL+path, "application/json", nil)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode, path)
	}
}
