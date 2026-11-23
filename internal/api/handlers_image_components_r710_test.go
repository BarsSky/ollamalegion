// handlers_image_components_r710_test.go — R710 (2026-11-24): плоскость балансера
// для добавления недостающих компонентов image-набора.
//
// ЧТО ПРОВЕРЯЕТСЯ И ПОЧЕМУ ИМЕННО ТАК:
//
//  1. GET /api/v1/image/components/catalog отвечается ЛОКАЛЬНО (данные
//     sdbackend/components.go). Это важно: каталог нужен странице, даже когда в
//     кластере НЕТ ни одного image-бэкенда — иначе кнопка «Добавить VAE» не
//     смогла бы показать, откуда взять файл. Проверка идёт на сервере БЕЗ
//     бэкендов.
//  2. POST /api/v1/image/models/{name}/components уходит на ВОРКЕР, который
//     держит модель: с ?backendId= — прямо, без него — поиском по каталогам
//     воркеров. Ответ воркера (202 + trackingId) доставляется как есть.
//  3. Неизвестный подпуть под /api/v1/image/models/ — 404, а существующие точные
//     маршруты (/api/v1/image/models, /catalog) не перехвачены префиксом.
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"
)

// componentWorker — мок воркера, который принимает components-запрос.
type componentWorker struct {
	server    *httptest.Server
	hits      int
	lastBody  string
	lastPath  string
	lastToken string
}

func newComponentWorker() *componentWorker {
	w := &componentWorker{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/image/models", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"models": []map[string]any{{"name": "qwen-edit-2511", "family": "qwen_image"}},
		})
	})
	mux.HandleFunc("/api/image/models/qwen-edit-2511/components", func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(rw, "use POST", http.StatusMethodNotAllowed)
			return
		}
		body := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(body)
		}
		w.hits++
		w.lastBody = string(body)
		w.lastPath = r.URL.Path
		w.lastToken = r.Header.Get("X-API-Token")
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"status":     "started",
			"role":       "vae",
			"trackingId": "qwen-edit-2511~components~vae",
		})
	})
	w.server = httptest.NewServer(mux)
	return w
}

func (w *componentWorker) close() { w.server.Close() }

// ============================================================
// 1. Каталог отвечается без бэкендов
// ============================================================

func TestImageComponentsCatalog_LocalWithoutBackends_R710(t *testing.T) {
	// Сервер БЕЗ image-бэкендов: каталог обязан ответить всё равно.
	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 8080, APIPort: 8081},
		Balancing: types.BalancingSettings{
			HealthCheckInterval: 10, MetricsInterval: 5, RequestTimeout: 30, QueueMaxSize: 100,
		},
		API:  types.APISettings{RateLimit: 1000, RateBurst: 2000},
		Auth: types.AuthConfig{Enabled: false},
	}
	proxy := balancer.NewProxy(config)
	srv := NewServer(proxy, config, nil)
	server := httptest.NewServer(srv)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/image/components/catalog?family=qwen_image&roles=vae,llm")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var payload struct {
		Family     string `json:"family"`
		Known      bool   `json:"known"`
		Count      int    `json:"count"`
		MaxFiles   int    `json:"maxFiles"`
		MaxSizeMB  int    `json:"maxSizeMb"`
		Components []struct {
			Role    string   `json:"role"`
			Repo    string   `json:"repo"`
			Files   []string `json:"files"`
			SizeMB  int64    `json:"sizeMb"`
			Default bool     `json:"default"`
			Family  string   `json:"family"`
		} `json:"components"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))
	assert.Equal(t, "qwen_image", payload.Family)
	assert.True(t, payload.Known, "для qwen_image каталог обязан быть известен")
	assert.NotZero(t, payload.Count)
	assert.NotZero(t, payload.MaxFiles)
	assert.NotZero(t, payload.MaxSizeMB)
	require.NotEmpty(t, payload.Components)
	first := payload.Components[0]
	assert.Equal(t, "vae", first.Role, "VAE — первая роль (без неё движок не стартует)")
	assert.True(t, first.Default, "первым обязан идти default-компонент")
	assert.NotEmpty(t, first.Repo)
	assert.NotEmpty(t, first.Files)
	assert.Greater(t, first.SizeMB, int64(0), "sizeMb обязателен: UI показывает объём в подтверждении")
	assert.Equal(t, "qwen_image", first.Family)

	// Неизвестное семейство — 200 с known=false и пустым массивом.
	resp2, err := http.Get(server.URL + "/api/v1/image/components/catalog?family=pixart")
	require.NoError(t, err)
	defer resp2.Body.Close()
	require.Equal(t, http.StatusOK, resp2.StatusCode)
	var unknown struct {
		Known      bool  `json:"known"`
		Components []any `json:"components"`
	}
	require.NoError(t, json.NewDecoder(resp2.Body).Decode(&unknown))
	assert.False(t, unknown.Known)
	assert.Empty(t, unknown.Components)
}

// ============================================================
// 2. POST уходит на воркер (явный backendId и поиск по каталогу)
// ============================================================

func TestImageModelComponents_PostProxiesToWorker_R710(t *testing.T) {
	worker := newComponentWorker()
	defer worker.close()

	server, _ := createImageTestServer(t, worker.server.URL)
	defer server.Close()

	body := `{"role":"vae","repo":"Comfy-Org/Qwen-Image_ComfyUI","files":["split_files/vae/qwen_image_vae.safetensors"],"default":true}`
	resp, err := http.Post(server.URL+"/api/v1/image/models/qwen-edit-2511/components?backendId=img_mock",
		"application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusAccepted, resp.StatusCode, "воркер отвечает 202 — балансер доставляет как есть")

	var payload map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))
	assert.Equal(t, "started", payload["status"])
	assert.Equal(t, "qwen-edit-2511~components~vae", payload["trackingId"])

	assert.Equal(t, 1, worker.hits)
	assert.Equal(t, "/api/image/models/qwen-edit-2511/components", worker.lastPath)
	assert.JSONEq(t, body, worker.lastBody, "тело уходит на воркер без изменений")
}

func TestImageModelComponents_PostFindsBackendByModel_R710(t *testing.T) {
	worker := newComponentWorker()
	defer worker.close()

	server, _ := createImageTestServer(t, worker.server.URL)
	defer server.Close()

	// Без ?backendId=: балансер спрашивает каталоги воркеров и находит модель.
	resp, err := http.Post(server.URL+"/api/v1/image/models/qwen-edit-2511/components",
		"application/json", strings.NewReader(`{"role":"vae","repo":"acme/vae","files":["ae.safetensors"]}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	assert.Equal(t, 1, worker.hits)
}

func TestImageModelComponents_PostUnknownModel_R710(t *testing.T) {
	worker := newComponentWorker()
	defer worker.close()

	server, _ := createImageTestServer(t, worker.server.URL)
	defer server.Close()

	resp, err := http.Post(server.URL+"/api/v1/image/models/no-such-model/components",
		"application/json", strings.NewReader(`{"role":"vae","repo":"acme/vae","files":["ae.safetensors"]}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Equal(t, 0, worker.hits, "запрос не должен уходить на воркер, если модели нигде нет")
}

// ============================================================
// 3. Префиксный маршрут не ломает точные
// ============================================================

func TestImageModelComponents_RouteOrdering_R710(t *testing.T) {
	worker := newComponentWorker()
	defer worker.close()

	server, _ := createImageTestServer(t, worker.server.URL)
	defer server.Close()

	// Точный /api/v1/image/models продолжает работать (агрегат).
	resp, err := http.Get(server.URL + "/api/v1/image/models")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode,
		"/api/v1/image/models перехвачен префиксным маршрутом — сломалась бы страница «Модели на диске»")

	// Чужой подпуть — 404, а не «202 на всё».
	resp2, err := http.Post(server.URL+"/api/v1/image/models/qwen-edit-2511/whatever",
		"application/json", strings.NewReader(`{}`))
	require.NoError(t, err)
	defer resp2.Body.Close()
	require.Equal(t, http.StatusNotFound, resp2.StatusCode)

	// GET на components — 405 (метод известен, но не тот).
	resp3, err := http.Get(server.URL + "/api/v1/image/models/qwen-edit-2511/components")
	require.NoError(t, err)
	defer resp3.Body.Close()
	require.Equal(t, http.StatusMethodNotAllowed, resp3.StatusCode)
}
