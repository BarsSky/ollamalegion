// image_surface_smoke_test.go — R-Image (2026-09-27): клиентский smoke
// (обязательный пункт приёмки Phase 1 плана,
// plans/2026-09-27-image-generation-backend-plan.md §12.5).
//
// Проверяем на РЕАЛЬНОМ балансере с мок-`sd-server`, что готовые клиенты
// работают БЕЗ правок на своей стороне:
//   - SillyTavern, источник «stable-diffusion.cpp server» (OPTIONS → /v1/models →
//     POST /sdapi/v1/txt2img с фиксированным набором полей);
//   - Open WebUI, IMAGE_GENERATION_ENGINE=openai (POST /v1/images/generations);
//   - LibreChat, SD tool (POST /sdapi/v1/txt2img + разбор поля info);
//   - AnythingLLM, провайдер localai (POST /v1/images/generations без
//     response_format);
//   - n8n, legacy-нода OpenAI Images (response_format=b64_json + дропдаун,
//     который фильтрует id по префиксу "dall-").
//
// Плюс два инварианта: текстовый запрос НЕ уходит на image-бэкенд, и при
// отсутствии image-бэкенда клиент получает 503 в OpenAI-конверте ошибки.
package tests

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"
)

// b64PNGStub — «картинка» в том виде, в каком её отдаёт sd-server
// (base64 PNG в data[].b64_json / images[]).
const b64PNGStub = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// imageWorkerMock — мок sd-server: реализует ровно тот API, который нужен
// клиентам, и запоминает, что именно ему прислали.
type imageWorkerMock struct {
	mu        sync.Mutex
	hits      []string
	txt2img   map[string]interface{}
	generated map[string]interface{}
	server    *httptest.Server
}

func newImageWorkerMock(t *testing.T) *imageWorkerMock {
	t.Helper()
	m := &imageWorkerMock{}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		m.mu.Lock()
		m.hits = append(m.hits, r.Method+" "+r.URL.Path)
		m.mu.Unlock()

		switch r.URL.Path {
		case "/sdcpp/v1/capabilities":
			// Health-probe балансера и наш источник capabilities.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"model":{"name":"sd-cpp-local","stem":"z-image-turbo"},"current_mode":"img_gen",
				"supported_modes":["img_gen"],"samplers":["euler","euler_a"],"schedulers":["discrete"],
				"limits":{"min_width":64,"max_width":4096,"min_height":64,"max_height":4096,"max_batch_count":8,"max_queue_size":64}}`))
		case "/v1/models":
			// Так делает реальный sd-server: id жёстко "sd-cpp-local".
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"sd-cpp-local","object":"model","created":1,"owned_by":"local"}]}`))
		case "/sdapi/v1/txt2img":
			var req map[string]interface{}
			_ = json.Unmarshal(body, &req)
			m.mu.Lock()
			m.txt2img = req
			m.mu.Unlock()
			// Ответ A1111-совместимого вида: images[] + parameters (эхо) + info (JSON-СТРОКА).
			resp := map[string]interface{}{
				"images":     []string{b64PNGStub},
				"parameters": req,
				"info":       `{"width":512,"height":512,"seed":12345,"steps":20,"sampler_name":"DDIM","infotexts":["a cat"],"all_seeds":[12345]}`,
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		case "/v1/images/generations":
			var req map[string]interface{}
			_ = json.Unmarshal(body, &req)
			m.mu.Lock()
			m.generated = req
			m.mu.Unlock()
			// Реальный sd-server отдаёт ТОЛЬКО b64_json (никаких url).
			resp := map[string]interface{}{
				"created":       1,
				"output_format": "png",
				"data":          []map[string]interface{}{{"b64_json": b64PNGStub}},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(m.server.Close)
	return m
}

func (m *imageWorkerMock) hitCount(substr string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, h := range m.hits {
		if strings.Contains(h, substr) {
			n++
		}
	}
	return n
}

func (m *imageWorkerMock) lastTxt2Img() map[string]interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.txt2img
}

func (m *imageWorkerMock) lastGeneration() map[string]interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.generated
}

func smokePortFromURL(t *testing.T, raw string) int {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("port from %q: %v", raw, err)
	}
	return p
}

// setupImageSmoke — балансер (standard mode) с одним image-бэкендом и одним
// текстовым, плюс OpenAI-поверхность на эфемерном порту httptest.
func setupImageSmoke(t *testing.T, withImage bool) (*httptest.Server, *imageWorkerMock, *httptest.Server) {
	t.Helper()

	imageMock := newImageWorkerMock(t)
	imagePort := smokePortFromURL(t, imageMock.server.URL)
	imageHost := strings.TrimSuffix(strings.TrimPrefix(imageMock.server.URL, "http://"), ":"+strconv.Itoa(imagePort))

	// Текстовый бэкенд — отдельный мок, чтобы текстовый flow не висел на
	// закрытом порту и тест оставался быстрым.
	textHits := 0
	textMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		textHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[],"message":{"role":"assistant","content":"ok"},"done":true}`))
	}))
	t.Cleanup(textMock.Close)
	textPort := smokePortFromURL(t, textMock.URL)

	backends := []types.Backend{
		{
			ID:                "llamacpp-1",
			Host:              "127.0.0.1",
			CppWorkerPort:     textPort,
			Type:              types.BackendTypeLlamaCpp,
			Status:            types.StatusHealthy,
			MaxConcurrentReqs: 4,
		},
	}
	if withImage {
		backends = append(backends, types.Backend{
			ID:                "image-1",
			Host:              imageHost,
			ImagePort:         imagePort,
			Type:              types.BackendTypeImage,
			Status:            types.StatusHealthy,
			MaxConcurrentReqs: 2,
		})
	}

	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:       "127.0.0.1",
			Port:       18080,
			APIPort:    18081,
			OpenAIPort: 18079,
		},
		BackendEngine: types.EngineAuto,
		Balancing: types.BalancingSettings{
			Algorithm:          types.AlgorithmResourceAware,
			ModelAffinity:      false,
			SessionStickiness:  false,
			UseEnhancedScoring: false,
			OperatingMode:      "standard",
			RequestTimeout:     5,
			QueueTimeout:       2,
			QueueMaxSize:       10,
			QueueWorkers:       1,
		},
		Backends: backends,
	}

	proxy := balancer.NewProxy(cfg)
	t.Cleanup(func() { _ = proxy.Shutdown(testCtx(t)) })
	for _, b := range proxy.GetAllBackends() {
		proxy.UpdateBackendStatus(b.ID, types.StatusHealthy)
	}

	surface := httptest.NewServer(balancer.NewOpenAISurface(proxy))
	t.Cleanup(surface.Close)
	return surface, imageMock, textMock
}

// --- SillyTavern: источник "stable-diffusion.cpp server" --------------------

func TestImageSmoke_SillyTavern_SdcppSource(t *testing.T) {
	surface, mock, _ := setupImageSmoke(t, true)

	// 1) Кнопка Connect: OPTIONS /v1/images/generations → 2xx.
	req, _ := http.NewRequest(http.MethodOptions, surface.URL+"/v1/images/generations", nil)
	req.Header.Set("Origin", "http://127.0.0.1:8000")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("OPTIONS failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("Connect (OPTIONS) status = %d, want 204", resp.StatusCode)
	}

	// 2) Список моделей: ST читает data[].id / data[].name.
	resp, err = http.Get(surface.URL + "/v1/models")
	if err != nil {
		t.Fatalf("GET /v1/models failed: %v", err)
	}
	modelsBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(modelsBody), "sd-cpp-local") {
		t.Fatalf("/v1/models must expose sd-cpp-local (ST reads it as model id), got %s", modelsBody)
	}

	// 3) Генерация: ровно такое тело шлёт ST для источника sdcpp.
	stBody := `{"model":"sd-cpp-local","prompt":"a cat sitting on a chair","negative_prompt":"blurry",
		"width":512,"height":512,"steps":20,"cfg_scale":7,"seed":-1,"batch_size":1,
		"sampler_name":"DDIM","scheduler":"discrete","clip_skip":2}`
	resp, err = http.Post(surface.URL+"/sdapi/v1/txt2img", "application/json", strings.NewReader(stBody))
	if err != nil {
		t.Fatalf("txt2img failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("txt2img status = %d, body = %s", resp.StatusCode, body)
	}

	var out struct {
		Images     []string               `json:"images"`
		Parameters map[string]interface{} `json:"parameters"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("txt2img response is not A1111-shaped: %v (%s)", err, body)
	}
	if len(out.Images) != 1 || out.Images[0] == "" {
		t.Fatalf("images[] must contain one base64 image, got %s", body)
	}

	// Все поля ST доехали до воркера без потерь.
	got := mock.lastTxt2Img()
	for _, key := range []string{"prompt", "negative_prompt", "width", "height", "steps", "cfg_scale", "batch_size", "sampler_name", "scheduler", "clip_skip"} {
		if _, ok := got[key]; !ok {
			t.Errorf("field %q lost on the way to the image worker (got %v)", key, got)
		}
	}
}

// --- Open WebUI: IMAGE_GENERATION_ENGINE=openai -----------------------------

func TestImageSmoke_OpenWebUI_OpenAIEngine(t *testing.T) {
	surface, mock, _ := setupImageSmoke(t, true)

	// Так выглядит запрос Open WebUI в openai-режиме (IMAGE_GENERATION_MODEL=dall-e-2,
	// IMAGE_SIZE=512x512). response_format приходит только для не-gpt-image моделей.
	body := `{"model":"dall-e-2","prompt":"a red fox in the snow","n":1,"size":"512x512","response_format":"b64_json"}`
	resp, err := http.Post(surface.URL+"/v1/images/generations", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("images/generations failed: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, raw)
	}
	var out struct {
		Created int `json:"created"`
		Data    []struct {
			B64 string `json:"b64_json"`
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("bad response: %v (%s)", err, raw)
	}
	if len(out.Data) != 1 || out.Data[0].B64 == "" {
		t.Fatalf("data[0].b64_json required (Open WebUI falls back to it when url is absent), got %s", raw)
	}
	if sent := mock.lastGeneration(); sent["size"] != "512x512" {
		t.Errorf("size must pass through unchanged, worker saw %v", sent["size"])
	}
}

// --- LibreChat: SD tool (SD_WEBUI_URL) --------------------------------------

func TestImageSmoke_LibreChat_StableDiffusionTool(t *testing.T) {
	surface, mock, _ := setupImageSmoke(t, true)

	// Тело из LibreChat StableDiffusion.js (env SD_WEBUI_URL).
	body := `{"prompt":"cyberpunk city","negative_prompt":"blurry","cfg_scale":4.5,"steps":22,"width":1024,"height":1024}`
	resp, err := http.Post(surface.URL+"/sdapi/v1/txt2img", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("txt2img failed: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, raw)
	}

	var out struct {
		Images []string `json:"images"`
		Info   string   `json:"info"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("bad response: %v", err)
	}
	if len(out.Images) == 0 {
		t.Fatal("LibreChat needs data.images[0]")
	}
	// LibreChat делает JSON.parse(data.info) и читает width/height/seed/infotexts.
	var info struct {
		Width     int      `json:"width"`
		Height    int      `json:"height"`
		Seed      int64    `json:"seed"`
		Infotexts []string `json:"infotexts"`
	}
	if err := json.Unmarshal([]byte(out.Info), &info); err != nil {
		t.Fatalf("info must be a JSON string (LibreChat parses it): %v (%q)", err, out.Info)
	}
	if info.Width == 0 || info.Height == 0 || len(info.Infotexts) == 0 {
		t.Errorf("info must carry width/height/infotexts, got %+v", info)
	}
	if got := mock.lastTxt2Img()["cfg_scale"]; got == nil {
		t.Error("cfg_scale lost")
	}
}

// --- AnythingLLM: провайдер localai (IMAGE_GEN_LOCALAI_BASE_PATH) ------------

func TestImageSmoke_AnythingLLM_LocalAIProvider(t *testing.T) {
	surface, mock, _ := setupImageSmoke(t, true)

	// AnythingLLM НЕ форсирует response_format — ждём b64_json без него.
	body := `{"model":"sd-cpp-local","prompt":"a lighthouse at night","size":"1024x1024","n":1}`
	resp, err := http.Post(surface.URL+"/v1/images/generations", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("failed: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "b64_json") {
		t.Fatalf("AnythingLLM accepts b64_json or url; got %s", raw)
	}
	if got := mock.lastGeneration(); got["prompt"] == nil {
		t.Error("prompt lost")
	}
}

// --- n8n: legacy-нода OpenAI Images ----------------------------------------

func TestImageSmoke_n8n_LegacyOpenAINode(t *testing.T) {
	surface, _, _ := setupImageSmoke(t, true)

	// Дропдаун модели в n8n фильтрует id по префиксу "dall-".
	resp, err := http.Get(surface.URL + "/v1/models")
	if err != nil {
		t.Fatalf("GET /v1/models failed: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(raw), `"dall-`) {
		t.Fatalf("n8n model dropdown filters ids by ^dall- ; got %s", raw)
	}

	// Нода всегда шлёт response_format=b64_json при responseFormat=binaryData.
	body := `{"model":"dall-e-2","prompt":"a robot","response_format":"b64_json"}`
	resp, err = http.Post(surface.URL+"/v1/images/generations", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("failed: %v", err)
	}
	defer resp.Body.Close()
	raw, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), "b64_json") {
		t.Fatalf("n8n needs data[].b64_json, status=%d body=%s", resp.StatusCode, raw)
	}
}

// --- Инварианты --------------------------------------------------------------

// Текстовый запрос не должен попадать на image-бэкенд (и наоборот).
func TestImageSmoke_TextNeverReachesImageBackend(t *testing.T) {
	surface, mock, _ := setupImageSmoke(t, true)

	body := `{"model":"gemma-4-E4B-it-Q4_K_M","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(surface.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("chat/completions failed: %v", err)
	}
	resp.Body.Close()

	if n := mock.hitCount("/v1/images/generations"); n != 0 {
		t.Fatalf("text request hit the image backend %d time(s)", n)
	}
	if n := mock.hitCount("/sdapi/v1"); n != 0 {
		t.Fatalf("text request hit the A1111 API %d time(s)", n)
	}
}

// Без image-бэкенда клиент получает 503 в OpenAI-конверте ошибки,
// а не молчаливый 404 или проваливание в текстовый flow.
func TestImageSmoke_NoImageBackend_OpenAIErrorEnvelope(t *testing.T) {
	surface, _, _ := setupImageSmoke(t, false)

	body := `{"model":"dall-e-2","prompt":"a cat"}`
	resp, err := http.Post(surface.URL+"/v1/images/generations", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("failed: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (no image backend), body = %s", resp.StatusCode, raw)
	}
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("error must be an OpenAI envelope, got %s", raw)
	}
	if envelope.Error.Code != "image_backend_unavailable" {
		t.Errorf("error.code = %q, want image_backend_unavailable", envelope.Error.Code)
	}
	if envelope.Error.Message == "" {
		t.Error("error.message must be set (SDKs print it)")
	}
}
