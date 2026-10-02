// image_router_r_image_test.go — R-Image (2026-09-27): тесты маршрутизации
// запросов генерации изображений.
//
// Покрывают обязательные утверждения Phase 1 плана
// (plans/2026-09-27-image-generation-backend-plan.md §7):
//   - явные признаки image-запроса (endpoint + префикс модели) дают тип image_cpp;
//   - текстовые пути НЕ классифицируются как image;
//   - normalizeBackendType не превращает image_cpp в ollama (главная ловушка);
//   - image-запрос уходит на image-бэкенд и НЕ уходит на текстовый;
//   - выбор бэкенда с allowedTypes={image_cpp} никогда не возвращает текстовый;
//   - OpenAI-поверхность (18079): CORS, OPTIONS→204, отказ на Ollama-путях,
//     отсутствие legacy auto-stream.
package balancer

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// portFromURL — порт из URL тестового сервера (httptest даёт 127.0.0.1:<port>).
func portFromURL(t *testing.T, raw string) int {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url %q: %v", raw, err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse port from %q: %v", raw, err)
	}
	return port
}

// --- 1. Явные признаки image-запроса -----------------------------------------

func TestDetermineRequestBackendType_ImageEndpoints(t *testing.T) {
	p := &Proxy{} // для image-путей config не нужен: возврат происходит раньше

	imagePaths := []string{
		"/v1/images/generations",
		"/v1/images/edits",
		"/v1/images/variations",
		"/sdapi/v1/txt2img",
		"/sdapi/v1/img2img",
		"/api/image/generate",
		"/api/image/jobs/42",
	}
	for _, path := range imagePaths {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		if got := p.determineRequestBackendType(req); got != types.BackendTypeImage {
			t.Errorf("path %s: got %q, want %q", path, got, types.BackendTypeImage)
		}
	}
}

func TestDetermineRequestBackendType_TextPathsAreNotImage(t *testing.T) {
	p := &Proxy{}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if got := p.determineRequestBackendType(req); got == types.BackendTypeImage {
		t.Fatalf("/v1/chat/completions must not be classified as image, got %q", got)
	}
	if got := p.determineRequestBackendType(req); got != types.BackendTypeLlamaCpp {
		t.Fatalf("/v1/chat/completions: got %q, want %q", got, types.BackendTypeLlamaCpp)
	}
}

func TestDetermineRequestBackendType_ImageModelPrefix(t *testing.T) {
	p := &Proxy{}

	imageModels := []string{"sd:sdxl-turbo-q8", "SD:upper-case", "image:flux-schnell", "img/flux", "sd_cpp:z-image"}
	for _, m := range imageModels {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req = withParsedRequest(req, &parsedRequest{Model: m})
		if got := p.determineRequestBackendType(req); got != types.BackendTypeImage {
			t.Errorf("model %q: got %q, want %q", m, got, types.BackendTypeImage)
		}
	}

	// Текстовая модель с обычным именем — не image.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req = withParsedRequest(req, &parsedRequest{Model: "gemma-4-E4B-it-Q4_K_M"})
	if got := p.determineRequestBackendType(req); got == types.BackendTypeImage {
		t.Fatalf("text model classified as image: %q", got)
	}
}

func TestImageModelPrefixHelper(t *testing.T) {
	if hasImageModelPrefix("") {
		t.Error("empty model must not match")
	}
	if hasImageModelPrefix("sdxl-turbo") {
		t.Error("plain model name must not match")
	}
	if !hasImageModelPrefix(" sd:foo ") {
		t.Error("prefix must be detected with surrounding spaces and case-insensitively")
	}
}

// --- 2. Нормализация типа: image_cpp не должен становиться ollama -------------

func TestNormalizeBackendType_KeepsImageCpp(t *testing.T) {
	if got := normalizeBackendType(types.BackendTypeImage); got != types.BackendTypeImage {
		t.Fatalf("normalizeBackendType(image_cpp) = %q — тип молча превращён (главная ловушка)", got)
	}
	// Прежнее поведение не сломано.
	if got := normalizeBackendType(""); got != types.BackendTypeOllama {
		t.Fatalf("empty type must normalize to ollama, got %q", got)
	}
	if got := normalizeBackendType(types.BackendTypeLlamaCpp); got != types.BackendTypeLlamaCpp {
		t.Fatalf("llama_cpp must survive normalization, got %q", got)
	}
}

// --- 3. Изоляция: image-запрос видит только image-бэкенды --------------------

// newImageTestProxy собирает прокси с одним image-бэкендом и одним текстовым.
func newImageTestProxy(t *testing.T, imagePort int) *Proxy {
	t.Helper()

	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:       "127.0.0.1",
			Port:       18080,
			APIPort:    18081,
			OpenAIPort: 18079,
			StatePath:  t.TempDir() + "/state.json",
		},
		Backends: []types.Backend{
			{
				ID:                "img-1",
				Name:              "image worker",
				Host:              "127.0.0.1",
				ImagePort:         imagePort,
				Type:              types.BackendTypeImage,
				Status:            types.StatusHealthy,
				MaxConcurrentReqs: 2,
			},
			{
				ID:                "llm-1",
				Name:              "llama.cpp worker",
				Host:              "127.0.0.1",
				CppWorkerPort:     18092,
				Type:              types.BackendTypeLlamaCpp,
				Status:            types.StatusHealthy,
				MaxConcurrentReqs: 2,
			},
		},
		Balancing: types.BalancingSettings{
			Algorithm:      types.AlgorithmResourceAware,
			RequestTimeout: 10,
			QueueTimeout:   5,
			QueueMaxSize:   50,
			QueueWorkers:   2,
			OperatingMode:  "standard",
		},
	}

	p := newProxyWithCleanup(t, cfg)
	for _, b := range p.GetAllBackends() {
		p.UpdateBackendStatus(b.ID, types.StatusHealthy)
	}
	return p
}

func TestImageRouter_SelectsOnlyImageBackends(t *testing.T) {
	p := newImageTestProxy(t, 18093)

	allowed := []types.BackendType{types.BackendTypeImage}
	if got := p.SelectBackendWithType("llama3", types.BackendTypeImage); got == "llm-1" {
		t.Fatal("selectBackend with image_cpp allowed returned a text (llama_cpp) backend")
	}
	if got := p.selectFreeBackendAny(allowed); got == "llm-1" {
		t.Fatal("selectFreeBackendAny(image_cpp) returned a text backend")
	}
	if got := p.selectByResources(allowed); got == "llm-1" {
		t.Fatal("selectByResources(image_cpp) returned a text backend")
	}
}

// TestGetDefaultAllowedTypes_NeverContainsImage — инвариант «image только по
// явному признаку»: если тип запроса не определён (bt == "", типичный случай
// текстового /api/generate в смешанном режиме), image_cpp НЕ должен попадать в
// список допустимых, иначе текст мог бы уйти на image-бэкенд.
func TestGetDefaultAllowedTypes_NeverContainsImage(t *testing.T) {
	for mode := range types.ModeBackendTypes {
		cfg := &types.LoadBalancerConfig{
			Balancing: types.BalancingSettings{OperatingMode: mode},
		}
		p := &Proxy{config: cfg}
		for _, bt := range p.getDefaultAllowedTypes() {
			if bt == types.BackendTypeImage {
				t.Errorf("mode %q: getDefaultAllowedTypes() contains image_cpp — текст сможет уйти на image-бэкенд", mode)
			}
		}
		// Явный image-запрос по-прежнему разрешает ровно image.
		explicit := p.getAllowedTypesList(types.BackendTypeImage)
		if len(explicit) != 1 || explicit[0] != types.BackendTypeImage {
			t.Errorf("mode %q: explicit image request must allow exactly image_cpp, got %v", mode, explicit)
		}
	}

	// Смешанный кластер, image-бэкенд единственный «свободный»: текстовый
	// запрос без явного типа не должен выбрать его.
	p := newImageTestProxy(t, 18093)
	if got := p.selectBackend("gemma-4-E4B-it-Q4_K_M", ""); got == "img-1" {
		t.Fatal("text request with undetermined type was routed to the image backend")
	}
	if got := p.selectByResources(p.getDefaultAllowedTypes()); got == "img-1" {
		t.Fatal("selectByResources(default types) returned the image backend")
	}
}

// --- 4. Полный HTTP-путь через OpenAI-поверхность ----------------------------

func TestOpenAISurface_ImageRequestRoutedToImageBackend(t *testing.T) {
	var imageHits int32
	imageWorker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// R-Image Phase 6: балансер сам опрашивает контракт воркера
		// (GET /api/image/models) — поллером метрик и перед решением гейта.
		// Такие запросы не «генерация» и не считаются попаданием в image-путь.
		// Ответ на контракт здесь намеренно пустой (без ключа models): гейт
		// считает данные недостоверными и пропускает запрос (fail-open), как и
		// для «голого» sd-server. Проверка самого гейта — image_resources_test.go.
		if r.URL.Path == "/api/image/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"state":"loaded"}`))
			return
		}
		atomic.AddInt32(&imageHits, 1)
		if r.URL.Path != "/v1/images/generations" {
			t.Errorf("unexpected path on image backend: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"output_format":"png","data":[{"b64_json":"iVBORw0KGgo="}]}`))
	}))
	defer imageWorker.Close()

	imagePort := portFromURL(t, imageWorker.URL)
	p := newImageTestProxy(t, imagePort)

	surface := httptest.NewServer(NewOpenAISurface(p))
	defer surface.Close()

	body := `{"model":"dall-e-2","prompt":"a cat","size":"512x512","n":1}`
	resp, err := http.Post(surface.URL+"/v1/images/generations", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&imageHits); got != 1 {
		t.Fatalf("image backend hits = %d, want 1", got)
	}
}

func TestOpenAISurface_RejectsOllamaPaths(t *testing.T) {
	p := newImageTestProxy(t, 18093)
	surface := httptest.NewServer(NewOpenAISurface(p))
	defer surface.Close()

	for _, path := range []string{"/api/tags", "/api/chat", "/api/pull"} {
		resp, err := http.Get(surface.URL + path)
		if err != nil {
			t.Fatalf("GET %s failed: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want 404 (Ollama-native endpoint on OpenAI surface)", path, resp.StatusCode)
		}
	}

	// Management-плоскость /api/v1/* на этой поверхности НЕ блокируется:
	// её обслуживает API-сервер (проверяем, что мы не отдали 404 «not_supported
	// on openai surface» — сам форвард зависит от наличия API-сервера).
	resp, err := http.Get(surface.URL + "/api/v1/backends")
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			t.Error("/api/v1/backends must not be rejected as an Ollama-native path")
		}
	}
}

func TestOpenAISurface_OptionsPreflightAndCORS(t *testing.T) {
	p := newImageTestProxy(t, 18093)
	surface := httptest.NewServer(NewOpenAISurface(p))
	defer surface.Close()

	req, err := http.NewRequest(http.MethodOptions, surface.URL+"/v1/images/generations", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Origin", "http://localhost:3000")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("OPTIONS failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("OPTIONS status = %d, want 204 (SillyTavern Connect relies on it)", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Errorf("Access-Control-Allow-Origin = %q, want echo of Origin", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Headers"); got == "" {
		t.Error("Access-Control-Allow-Headers must be set for browser clients")
	}
}

// --- 5. Отсутствие legacy auto-stream на OpenAI-поверхности ------------------

func TestParseRequestBody_StreamDefaultDependsOnSurface(t *testing.T) {
	p := &Proxy{}
	body := `{"model":"gemma-4-E4B-it-Q4_K_M","prompt":"hi"}`

	legacy := httptest.NewRequest(http.MethodPost, "/api/generate", strings.NewReader(body))
	if parsed := p.parseRequestBody(legacy); !parsed.Stream {
		t.Error("legacy surface (18080) must keep stream=true default")
	}

	openai := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	openai = withRequestSurface(openai, surfaceOpenAI)
	if parsed := p.parseRequestBody(openai); parsed.Stream {
		t.Error("openai surface (18079) must NOT enable stream by default")
	}

	// Явный stream:true уважается на обеих поверхностях.
	explicit := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true}`))
	explicit = withRequestSurface(explicit, surfaceOpenAI)
	if parsed := p.parseRequestBody(explicit); !parsed.Stream {
		t.Error("explicit stream:true must be honored on the openai surface")
	}
}

// --- 6. Алиасы image-моделей в /v1/models ------------------------------------

func TestImageModelIDsForModelsList(t *testing.T) {
	withImage := newImageTestProxy(t, 18093)
	ids := withImage.imageModelIDsForModelsList()
	if len(ids) == 0 {
		t.Fatal("cluster with an image backend must expose image model aliases")
	}
	var hasDallE, hasNative bool
	for _, id := range ids {
		if strings.HasPrefix(id, "dall-") {
			hasDallE = true
		}
		if id == "sd-cpp-local" {
			hasNative = true
		}
	}
	if !hasDallE {
		t.Error("dall-* alias is required: legacy n8n node filters model ids by that prefix")
	}
	if !hasNative {
		t.Error("sd-cpp-local alias (what sd-server itself reports) must be present")
	}

	// Кластер без image-бэкендов не должен подмешивать dall-e.
	textOnly := &Proxy{}
	if got := textOnly.imageModelIDsForModelsList(); got != nil {
		t.Fatalf("text-only cluster must not expose image aliases, got %v", got)
	}
}
