// image_edits_smoke_test.go — img2img/inpaint через РЕАЛЬНЫЙ балансер
// (R-Image, 2026-10-03).
//
// Что проверяем: клиентские пути генерации «по картинке» доходят до
// image-бэкенда и возвращают ровно тот конверт, который ждут клиенты:
//
//   - POST /v1/images/edits (multipart/form-data) — OpenAI Images Edits:
//     prompt + image[] (НЕСКОЛЬКО файлов) + mask → {created, output_format,
//     data:[{b64_json}]}. Так ходят OpenAI SDK, Open WebUI (режим edits) и
//     LobeChat;
//   - POST /sdapi/v1/img2img (JSON) — A1111: init_images[] + mask +
//     denoising_strength → {images:[...], parameters:{...}, info:"<JSON-СТРОКА>"}.
//     Так ходят SillyTavern (источник sd.cpp/A1111) и LibreChat SD tool.
//
// Мок здесь — ВОРКЕР (не движок): он реализует клиентскую поверхность
// sdworker и запоминает, что именно до него доехало. Как и в
// image_surface_smoke_test.go, это проверяет маршрутизацию image-цепочки
// балансером и форму ответа, а не внутренности воркера (они покрыты
// cmd/sdworker/handlers_edits_test.go на моке движка).
package tests

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"
)

// imageEditsWorkerMock — мок image-воркера для edits/img2img.
type imageEditsWorkerMock struct {
	mu sync.Mutex
	// edits — разобранный multipart /v1/images/edits.
	editsPrompt string
	editsImages [][]byte
	editsMask   []byte
	editsN      string
	// img2img — разобранный JSON /sdapi/v1/img2img.
	img2img map[string]interface{}
	server  *httptest.Server
}

func newImageEditsWorkerMock(t *testing.T) *imageEditsWorkerMock {
	t.Helper()
	m := &imageEditsWorkerMock{}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/images/edits":
			// Worker обязан принимать ИМЕННО multipart (иначе OpenAI-клиенты
			// получат 400 ещё до движка).
			if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"message":"expected multipart/form-data","code":"invalid_content_type"}}`))
				return
			}
			if err := r.ParseMultipartForm(8 << 20); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"message":"bad multipart","code":"invalid_multipart"}}`))
				return
			}
			images := [][]byte{}
			for _, field := range []string{"image[]", "image"} {
				if r.MultipartForm == nil {
					break
				}
				for _, fh := range r.MultipartForm.File[field] {
					f, err := fh.Open()
					if err != nil {
						continue
					}
					raw, _ := io.ReadAll(f)
					_ = f.Close()
					images = append(images, raw)
				}
			}
			var mask []byte
			if fh := r.MultipartForm.File["mask"]; len(fh) > 0 {
				f, err := fh[0].Open()
				if err == nil {
					mask, _ = io.ReadAll(f)
					_ = f.Close()
				}
			}
			m.mu.Lock()
			m.editsPrompt = r.FormValue("prompt")
			m.editsImages = images
			m.editsMask = mask
			m.editsN = r.FormValue("n")
			m.mu.Unlock()

			data := []map[string]interface{}{{"b64_json": b64PNGStub}}
			if n, err := strconv.Atoi(strings.TrimSpace(r.FormValue("n"))); err == nil && n > 1 {
				for i := 1; i < n; i++ {
					data = append(data, map[string]interface{}{"b64_json": b64PNGStub})
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"created": 1, "output_format": "png", "data": data, "seed": 424242,
			})
		case "/sdapi/v1/img2img":
			raw, _ := io.ReadAll(r.Body)
			var req map[string]interface{}
			_ = json.Unmarshal(raw, &req)
			m.mu.Lock()
			m.img2img = req
			m.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			// info — JSON-СТРОКА (A1111-контракт).
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"images":     []string{b64PNGStub},
				"parameters": req,
				"info":       `{"width":512,"height":512,"seed":777,"steps":20,"sampler_name":"euler","infotexts":["edit"],"all_seeds":[777]}`,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(m.server.Close)
	return m
}

func (m *imageEditsWorkerMock) lastEdits() (string, [][]byte, []byte, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.editsPrompt, m.editsImages, m.editsMask, m.editsN
}

func (m *imageEditsWorkerMock) lastImg2Img() map[string]interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.img2img
}

// setupImageEditsSmoke — балансер (standard mode) с одним image_cpp-бэкендом,
// чей «воркер» — мок выше, плюс OpenAI-поверхность на эфемерном порту.
func setupImageEditsSmoke(t *testing.T) (*httptest.Server, *imageEditsWorkerMock) {
	t.Helper()

	worker := newImageEditsWorkerMock(t)
	port := smokePortFromURL(t, worker.server.URL)
	host := strings.TrimSuffix(strings.TrimPrefix(worker.server.URL, "http://"), ":"+strconv.Itoa(port))

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
		Backends: []types.Backend{
			{
				ID:                "image-edits-1",
				Host:              host,
				ImagePort:         port,
				Type:              types.BackendTypeImage,
				Status:            types.StatusHealthy,
				MaxConcurrentReqs: 2,
			},
		},
	}

	proxy := balancer.NewProxy(cfg)
	t.Cleanup(func() { _ = proxy.Shutdown(testCtx(t)) })
	for _, b := range proxy.GetAllBackends() {
		proxy.UpdateBackendStatus(b.ID, types.StatusHealthy)
	}

	surface := httptest.NewServer(balancer.NewOpenAISurface(proxy))
	t.Cleanup(surface.Close)
	return surface, worker
}

// pngBytes1x1 — байты валидного PNG (тот же stub, что отдаёт движок).
func pngBytes1x1(t *testing.T) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(b64PNGStub)
	if err != nil {
		t.Fatalf("b64PNGStub: %v", err)
	}
	return raw
}

// TestImageEditsSmoke_OpenAIEditsMultipart — клиент → балансер → воркер:
// multipart с ДВУМЯ image[] + mask, ответ в OpenAI-конверте.
func TestImageEditsSmoke_OpenAIEditsMultipart(t *testing.T) {
	surface, worker := setupImageEditsSmoke(t)
	png := pngBytes1x1(t)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", "dall-e-2")
	_ = mw.WriteField("prompt", "make it snow")
	_ = mw.WriteField("n", "2")
	_ = mw.WriteField("size", "512x512")
	for i := 0; i < 2; i++ {
		fw, err := mw.CreateFormFile("image[]", "init.png")
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		if _, err := fw.Write(png); err != nil {
			t.Fatalf("write file: %v", err)
		}
	}
	fw, err := mw.CreateFormFile("mask", "mask.png")
	if err != nil {
		t.Fatalf("create mask: %v", err)
	}
	_, _ = fw.Write(png)
	if err := mw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, surface.URL+"/v1/images/edits", &buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("images/edits failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}

	var out struct {
		Created      int64  `json:"created"`
		OutputFormat string `json:"output_format"`
		Data         []struct {
			B64JSON string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("ответ не OpenAI-формы: %v (%s)", err, body)
	}
	if out.Created == 0 || len(out.Data) != 2 || out.Data[0].B64JSON == "" {
		t.Fatalf("created/data обязательны (n=2): %s", body)
	}

	prompt, images, mask, n := worker.lastEdits()
	if prompt != "make it snow" {
		t.Errorf("prompt = %q", prompt)
	}
	if len(images) != 2 {
		t.Fatalf("до воркера доехало изображений: %d, want 2 (image[])", len(images))
	}
	if !bytes.Equal(images[0], png) {
		t.Error("байты init-картинки искажены по пути")
	}
	if len(mask) == 0 {
		t.Error("mask не доехал до воркера")
	}
	if n != "2" {
		t.Errorf("n = %q, want 2", n)
	}
}

// TestImageEditsSmoke_A1111Img2Img — клиент → балансер → воркер: JSON A1111
// с init_images/denoising_strength и ответ с info как JSON-СТРОКОЙ.
func TestImageEditsSmoke_A1111Img2Img(t *testing.T) {
	surface, worker := setupImageEditsSmoke(t)

	reqBody := `{"prompt":"cyberpunk","init_images":["data:image/png;base64,` + b64PNGStub + `"],
		"denoising_strength":0.45,"steps":20,"cfg_scale":7,"seed":-1,"batch_size":1,"sampler_name":"euler"}`
	resp, err := http.Post(surface.URL+"/sdapi/v1/img2img", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("img2img failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}

	var out struct {
		Images     []string               `json:"images"`
		Parameters map[string]interface{} `json:"parameters"`
		Info       string                 `json:"info"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("ответ не A1111-формы: %v (%s)", err, body)
	}
	if len(out.Images) == 0 || out.Images[0] == "" {
		t.Fatalf("images[0] обязателен: %s", body)
	}
	if out.Parameters == nil {
		t.Error("parameters (эхо запроса) обязателен")
	}
	// info — JSON-СТРОКА (LibreChat/SillyTavern делают JSON.parse).
	var info struct {
		Width     int      `json:"width"`
		Height    int      `json:"height"`
		Seed      int64    `json:"seed"`
		Infotexts []string `json:"infotexts"`
	}
	if err := json.Unmarshal([]byte(out.Info), &info); err != nil {
		t.Fatalf("info обязан быть JSON-строкой: %v (%q)", err, out.Info)
	}
	if info.Width == 0 || info.Height == 0 || info.Seed == 0 {
		t.Errorf("info неполон: %+v", info)
	}

	got := worker.lastImg2Img()
	if got["init_images"] == nil {
		t.Fatal("init_images потерян по пути")
	}
	if ds, ok := got["denoising_strength"].(float64); !ok || ds != 0.45 {
		t.Errorf("denoising_strength = %v, want 0.45", got["denoising_strength"])
	}
	init, _ := got["init_images"].([]interface{})
	if len(init) == 0 || init[0] == "" {
		t.Fatal("init_images[0] пуст")
	}
}
