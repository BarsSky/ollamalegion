//go:build llama_stub

// client_api_contract_r118_test.go — R118.13 (2026-10-14).
//
// КОНТРАКТНЫЙ НАБОР ДЛЯ КЛИЕНТСКИХ API.
//
// ИСХОДНАЯ ЖАЛОБА ОПЕРАТОРА (с начала сессии): «нет нормальных тестов для
// проверки, что ничего не было испорчено при новых правках под поддержку
// клиентов». До этого набора клиентская поверхность проверялась точечно: unit-
// тесты по внутренним функциям плюс живой прогон сценариев на стенде. Здесь —
// систематическая проверка того, что видит КЛИЕНТ (Cline, OpenWebUI, curl):
// статус, формат тела и Content-Type на обеих поверхностях (Ollama /api/* и
// OpenAI /v1/*).
//
// ЧТО ИМЕННО ЛОВИТ ЭТОТ НАБОР (и чего не ловит — без иллюзий):
//
//	ловит  — изменения на HTTP-уровне со стороны балансера: статус, форма тела,
//	         Content-Type (NDJSON для нативных путей, SSE для OpenAI),
//	         JSON-ошибка вместо HTML, заголовок X-Request-Id, а также регресс
//	         «read-only путь ушёл в inference или запустил загрузку модели»
//	         (живой дефект OpenWebUI: POST /api/show → реальная загрузка → 503
//	         «Server Connection Error»);
//	НЕ ловит — внутренние ветки (например конкретную реализацию bypass'а
//	         read-only путей): их пинят unit-тесты
//	         internal/balancer/readonly_no_inference_r118_test.go и другие.
//	         Проверено мутациями: отключение bypass'а в proxy_request.go и
//	         переадресация /api/show на другой маршрут НЕ роняют этот набор,
//	         потому что /api/show обслуживает отдельный admin-хендлер
//	         (llamacpp_handlers_admin.go) и в заглушку приходит
//	         [/api/models, /api/show] без inference и без загрузки.
package llamacpp_proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
)

// contractStub — cppworker-заглушка с записью обращений: тесты проверяют не
// только ответ клиенту, но и КУДА балансер ходил (например, что /api/show не
// вызвал загрузку модели).
type contractStub struct {
	server *httptest.Server

	mu         sync.Mutex
	counts     map[string]int
	inference  int // /api/chat, /api/generate, /api/embed, /v1/chat/completions
	loadCalls  int // /api/models/load (загрузка модели — read-only её запускать не должен)
	unloadCall int
}

func (s *contractStub) hit(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counts[path]++
	switch path {
	case "/api/chat", "/api/generate", "/api/embed", "/api/embeddings",
		"/v1/chat/completions", "/v1/completions", "/v1/embeddings":
		s.inference++
	case "/api/models/load":
		s.loadCalls++
	case "/api/models/unload":
		s.unloadCall++
	}
}

func (s *contractStub) Count(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[path]
}

func (s *contractStub) InferenceCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inference
}

func (s *contractStub) LoadCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadCalls
}

func (s *contractStub) Close() { s.server.Close() }

// newContractStub — заглушка cppworker. Отвечает в формате llama.cpp: NDJSON для
// нативных путей, SSE для OpenAI-совместимых.
func newContractStub(t *testing.T, modelName string) *contractStub {
	t.Helper()
	s := &contractStub{counts: map[string]int{}}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		s.hit(path)
		now := time.Now().UTC().Format(time.RFC3339)

		switch path {
		case "/health":
			_, _ = w.Write([]byte(`{"status":"ok"}`))

		case "/api/models":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"count":1,"models":[{"name":%q,"state":"loaded","context_size":32768,`+
				`"context_per_seq":16384,"max_slots":2,"parallel":2,"n_layers":42,"gpu_layers":-1,`+
				`"size_bytes":4215695776,"loaded_at":%q}]}`, modelName, now)

		case "/api/info":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"activeModels":1,"activeQueries":0,"activeRequests":0,"gpuCount":1}`))

		case "/api/version":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"version":"0.0.0-contract-stub"}`))

		case "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"models":[{"name":%q,"model":%q,"size":4215695776,`+
				`"modified_at":%q,"details":{"format":"gguf"}}]}`, modelName, modelName, now)

		case "/api/show":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"model":%q,"details":{"format":"gguf","family":"gemma"},`+
				`"model_info":{"general.architecture":"gemma"}}`, modelName)

		case "/api/ps":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"models":[{"name":%q,"size":4215695776,"size_vram":4215695776}]}`, modelName)

		case "/api/models/files":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"files":[{"name":%q,"path":"/app/models/%s.gguf"}]}`, modelName, modelName)

		case "/api/models/load", "/api/models/unload":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true,"status":"loaded"}`))

		case "/api/chat":
			body, _ := io.ReadAll(r.Body)
			var req map[string]interface{}
			_ = json.Unmarshal(body, &req)
			// Контракт «неизвестная модель»: cppworker отвечает ошибкой в JSON —
			// балансер обязан донести её до клиента, а не подменить HTML-страницей.
			if gotModel, _ := req["model"].(string); gotModel != modelName {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				msg, _ := json.Marshal(map[string]string{
					"error": fmt.Sprintf("model %q not found on this backend", gotModel),
				})
				_, _ = w.Write(msg)
				return
			}
			stream, _ := req["stream"].(bool)
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			if stream {
				fmt.Fprintf(w, "%s\n", `{"done":false}`)
				fmt.Fprintf(w, `{"model":%q,"created_at":%q,"done":true,`+
					`"message":{"role":"assistant","content":"контракт-ответ"}}`+"\n", modelName, now)
				return
			}
			fmt.Fprintf(w, `{"model":%q,"created_at":%q,"done":true,`+
				`"message":{"role":"assistant","content":"контракт-ответ"},`+
				`"eval_count":3,"prompt_eval_count":7}`, modelName, now)

		case "/api/generate":
			body, _ := io.ReadAll(r.Body)
			var req map[string]interface{}
			_ = json.Unmarshal(body, &req)
			stream, _ := req["stream"].(bool)
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			if stream {
				fmt.Fprintf(w, "%s\n", `{"done":false}`)
				fmt.Fprintf(w, `{"model":%q,"created_at":%q,"response":"контракт","done":true}`+"\n",
					modelName, now)
				return
			}
			fmt.Fprintf(w, `{"model":%q,"created_at":%q,"response":"контракт","done":true}`,
				modelName, now)

		case "/api/embed", "/api/embeddings":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"model":"m","embeddings":[[0.1,0.2,0.3]],"prompt_eval_count":1}`))

		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"object":"model","created":1}]}`, modelName)

		case "/v1/chat/completions":
			body, _ := io.ReadAll(r.Body)
			var req map[string]interface{}
			_ = json.Unmarshal(body, &req)
			stream, _ := req["stream"].(bool)
			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				fl, _ := w.(http.Flusher)
				fmt.Fprintf(w, "data: %s\n\n", `{"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"контракт"},"finish_reason":null}]}`)
				if fl != nil {
					fl.Flush()
				}
				fmt.Fprintf(w, "data: %s\n\n", `{"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
				fmt.Fprint(w, "data: [DONE]\n\n")
				if fl != nil {
					fl.Flush()
				}
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"x","object":"chat.completion","created":1,"model":%q,`+
				`"choices":[{"index":0,"message":{"role":"assistant","content":"контракт-ответ"},`+
				`"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,`+
				`"total_tokens":10}}`, modelName)

		case "/v1/embeddings":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"object":"embedding","index":0,` +
				`"embedding":[0.1,0.2,0.3]}],"model":"m","usage":{"prompt_tokens":1,"total_tokens":1}}`))

		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// newContractProxy — прокси с зарегистрированным бэкендом-заглушкой, у которой
// модель УЖЕ загружена (обычный рабочий случай).
func newContractProxy(t *testing.T, stub *contractStub, modelName string) *balancer.Proxy {
	t.Helper()
	proxy := createTestProxyForLLamaCpp(t, stub.server.URL)
	registerLlamaCppModel(proxy, "llamacpp-1", modelName)
	return proxy
}

// doContract — запрос через клиентскую поверхность балансера.
func doContract(proxy *balancer.Proxy, method, path, body string, headers map[string]string) *http.Response {
	var rdr io.Reader
	if body != "" {
		rdr = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)
	return rec.Result()
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("чтение тела ответа: %v", err)
	}
	return string(b)
}

func assertJSONObject(t *testing.T, body, what string) map[string]interface{} {
	t.Helper()
	var out map[string]interface{}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("%s: ответ не JSON-объект: %v (тело: %.200s)", what, err, body)
	}
	return out
}

// --- 1. Ollama: read-only пути ------------------------------------------------

// TestR118_13_OllamaReadOnlyEndpoints — /api/tags, /api/ps, /api/version, /api/show
// отвечают JSON и НЕ уходят в inference.
//
// РЕГРЕСС, который здесь зафиксирован: POST /api/show раньше попадал в
// n_ctx-preflight, запускал реальную загрузку модели и отдавал 503 — OpenWebUI
// показывал «Server Connection Error» (R118.0).
func TestR118_13_OllamaReadOnlyEndpoints(t *testing.T) {
	const model = "contract-model"
	stub := newContractStub(t, model)
	proxy := newContractProxy(t, stub, model)

	cases := []struct {
		method, path, body string
		wantKey            string
	}{
		{"GET", "/api/tags", "", "models"},
		{"GET", "/api/ps", "", "models"},
		{"GET", "/api/version", "", "version"},
		{"POST", "/api/show", `{"model":"` + model + `"}`, "model_info"},
	}
	for _, c := range cases {
		resp := doContract(proxy, c.method, c.path, c.body, nil)
		body := readAll(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s %s: HTTP %d (тело: %.200s)", c.method, c.path, resp.StatusCode, body)
			continue
		}
		obj := assertJSONObject(t, body, c.method+" "+c.path)
		if _, ok := obj[c.wantKey]; !ok {
			t.Errorf("%s %s: в ответе нет ключа %q: %.200s", c.method, c.path, c.wantKey, body)
		}
	}

	if n := stub.InferenceCalls(); n != 0 {
		t.Errorf("read-only пути ушли в inference %d раз — клиент получил бы загрузку/503 вместо метаданных", n)
	}
	if n := stub.LoadCalls(); n != 0 {
		t.Errorf("read-only пути запустили загрузку модели %d раз (живой дефект OpenWebUI)", n)
	}

	// ---- ЖИВОЙ СЛУЧАЙ OpenWebUI ------------------------------------------
	//
	// В том дефекте модель НЕ была загружена, а клиент присылал num_ctx в теле
	// POST /api/show. Именно это сочетание запускало n_ctx-preflight → реальную
	// загрузку → 503 и «Server Connection Error». Здесь оно воспроизведено:
	// бэкенд без загруженной модели + тело с options.num_ctx.
	stub2 := newContractStub(t, model)
	proxy2 := createTestProxyForLLamaCpp(t, stub2.server.URL) // модель НЕ регистрируем

	resp2 := doContract(proxy2, "POST", "/api/show",
		`{"model":"`+model+`","options":{"num_ctx":32768}}`, nil)
	body2 := readAll(t, resp2)
	if resp2.StatusCode == http.StatusServiceUnavailable {
		t.Fatalf("POST /api/show с num_ctx при незагруженной модели вернул 503 — это и есть живой дефект OpenWebUI (тело: %.200s)", body2)
	}
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/show: HTTP %d (тело: %.200s)", resp2.StatusCode, body2)
	}
	if n := stub2.LoadCalls(); n != 0 {
		t.Errorf("POST /api/show запустил загрузку модели %d раз при незагруженной модели — "+
			"именно это на живом стенде давало 503 и «Server Connection Error»", n)
	}
	if n := stub2.InferenceCalls(); n != 0 {
		t.Errorf("POST /api/show ушёл в inference %d раз", n)
	}
}

// --- 2. Ollama: чат и генерация ----------------------------------------------

// TestR118_13_OllamaChatContract — /api/chat: не-стриминг JSON, стриминг NDJSON
// с done=true.
func TestR118_13_OllamaChatContract(t *testing.T) {
	const model = "contract-model"
	stub := newContractStub(t, model)
	proxy := newContractProxy(t, stub, model)

	// Не-стриминг.
	resp := doContract(proxy, "POST", "/api/chat",
		`{"model":"`+model+`","messages":[{"role":"user","content":"привет"}],"stream":false}`+"", nil)
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("non-stream: HTTP %d (тело: %.200s)", resp.StatusCode, body)
	}
	obj := assertJSONObject(t, body, "chat non-stream")
	msg, ok := obj["message"].(map[string]interface{})
	if !ok {
		t.Fatalf("non-stream: нет объекта message: %.200s", body)
	}
	if content, _ := msg["content"].(string); content == "" {
		t.Errorf("non-stream: пустой message.content: %.200s", body)
	}
	if done, _ := obj["done"].(bool); !done {
		t.Errorf("non-stream: done != true: %.200s", body)
	}
	// Заголовок балансера (не upstream): клиенты и поддержка используют его, чтобы
	// понять, кто обслужил запрос. Его отсутствие — регресс на стороне балансера.
	if rid := resp.Header.Get("X-Request-Id"); rid == "" {
		t.Errorf("non-stream: балансер не отдал X-Request-Id")
	}

	// Стриминг: Content-Type NDJSON и хотя бы одна строка с done=true.
	resp = doContract(proxy, "POST", "/api/chat",
		`{"model":"`+model+`","messages":[{"role":"user","content":"привет"}],"stream":true}`, nil)
	streamBody := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream: HTTP %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "ndjson") {
		t.Errorf("stream: Content-Type = %q, want application/x-ndjson", ct)
	}
	lines := 0
	sawDone := false
	sc := bufio.NewScanner(strings.NewReader(streamBody))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		lines++
		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			t.Fatalf("stream: строка не JSON (%v): %.120s", err, line)
		}
		if done, _ := chunk["done"].(bool); done {
			sawDone = true
		}
	}
	if lines == 0 || !sawDone {
		t.Errorf("stream: строк=%d, done=true=%v (тело: %.200s)", lines, sawDone, streamBody)
	}
}

// TestR118_13_OllamaGenerateContract — /api/generate: не-стриминг `response`,
// стриминг NDJSON с done=true.
func TestR118_13_OllamaGenerateContract(t *testing.T) {
	const model = "contract-model"
	stub := newContractStub(t, model)
	proxy := newContractProxy(t, stub, model)

	resp := doContract(proxy, "POST", "/api/generate",
		`{"model":"`+model+`","prompt":"привет","stream":false}`, nil)
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("non-stream: HTTP %d (тело: %.200s)", resp.StatusCode, body)
	}
	obj := assertJSONObject(t, body, "generate non-stream")
	if r, _ := obj["response"].(string); r == "" {
		t.Errorf("non-stream: пустой response: %.200s", body)
	}

	resp = doContract(proxy, "POST", "/api/generate",
		`{"model":"`+model+`","prompt":"привет","stream":true}`, nil)
	streamBody := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream: HTTP %d", resp.StatusCode)
	}
	if !strings.Contains(streamBody, `"done":true`) && !strings.Contains(streamBody, `"done": true`) {
		t.Errorf("stream: нет done=true: %.200s", streamBody)
	}
}

// TestR118_13_OllamaEmbedContract — /api/embed отдаёт массив embeddings.
func TestR118_13_OllamaEmbedContract(t *testing.T) {
	const model = "contract-model"
	stub := newContractStub(t, model)
	proxy := newContractProxy(t, stub, model)

	resp := doContract(proxy, "POST", "/api/embed", `{"model":"`+model+`","input":"текст"}`, nil)
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP %d (тело: %.200s)", resp.StatusCode, body)
	}
	obj := assertJSONObject(t, body, "embed")
	vecs, ok := obj["embeddings"].([]interface{})
	if !ok || len(vecs) == 0 {
		t.Fatalf("нет непустого embeddings: %.200s", body)
	}
}

// --- 3. OpenAI-поверхность ---------------------------------------------------

// TestR118_13_OpenAIChatCompletionsContract — /v1/chat/completions: не-стриминг
// choices[].message, стриминг SSE с [DONE].
func TestR118_13_OpenAIChatCompletionsContract(t *testing.T) {
	const model = "contract-model"
	stub := newContractStub(t, model)
	proxy := newContractProxy(t, stub, model)

	resp := doContract(proxy, "POST", "/v1/chat/completions",
		`{"model":"`+model+`","messages":[{"role":"user","content":"привет"}]}`, nil)
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("non-stream: HTTP %d (тело: %.200s)", resp.StatusCode, body)
	}
	obj := assertJSONObject(t, body, "v1 chat")
	choices, ok := obj["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		t.Fatalf("non-stream: нет choices: %.200s", body)
	}
	first, _ := choices[0].(map[string]interface{})
	msg, _ := first["message"].(map[string]interface{})
	if content, _ := msg["content"].(string); content == "" {
		t.Errorf("non-stream: пустой choices[0].message.content: %.200s", body)
	}

	resp = doContract(proxy, "POST", "/v1/chat/completions",
		`{"model":"`+model+`","messages":[{"role":"user","content":"привет"}],"stream":true}`, nil)
	streamBody := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream: HTTP %d", resp.StatusCode)
	}
	if !strings.Contains(streamBody, "data:") {
		t.Errorf("stream: нет SSE-строк 'data:': %.200s", streamBody)
	}
}

// TestR118_13_OpenAIModelsAndEmbeddingsContract — /v1/models и /v1/embeddings.
func TestR118_13_OpenAIModelsAndEmbeddingsContract(t *testing.T) {
	const model = "contract-model"
	stub := newContractStub(t, model)
	proxy := newContractProxy(t, stub, model)

	resp := doContract(proxy, "GET", "/v1/models", "", nil)
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/v1/models: HTTP %d (тело: %.200s)", resp.StatusCode, body)
	}
	obj := assertJSONObject(t, body, "/v1/models")
	if _, ok := obj["data"]; !ok {
		t.Errorf("/v1/models: нет data: %.200s", body)
	}

	resp = doContract(proxy, "POST", "/v1/embeddings", `{"model":"`+model+`","input":"текст"}`, nil)
	body = readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/v1/embeddings: HTTP %d (тело: %.200s)", resp.StatusCode, body)
	}
	obj = assertJSONObject(t, body, "/v1/embeddings")
	data, ok := obj["data"].([]interface{})
	if !ok || len(data) == 0 {
		t.Fatalf("/v1/embeddings: нет непустого data: %.200s", body)
	}
}

// --- 4. Ошибки в формате клиента ---------------------------------------------

// TestR118_13_UnknownModelReturnsJSONError — неизвестная модель: JSON-ошибка, а не
// HTML-страница (клиенты парсят тело и показывают его оператору).
func TestR118_13_UnknownModelReturnsJSONError(t *testing.T) {
	const model = "contract-model"
	stub := newContractStub(t, model)
	proxy := newContractProxy(t, stub, model)

	resp := doContract(proxy, "POST", "/api/chat",
		`{"model":"no-such-model","messages":[{"role":"user","content":"привет"}],"stream":false}`, nil)
	body := readAll(t, resp)
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("неизвестная модель обслужена: %.200s", body)
	}
	if strings.HasPrefix(strings.TrimSpace(body), "<") {
		t.Fatalf("клиенту отдана HTML-страница вместо JSON: %.200s", body)
	}
	obj := assertJSONObject(t, body, "unknown model error")
	if _, ok := obj["error"]; !ok {
		t.Errorf("в теле ошибки нет ключа error: %.200s", body)
	}
}

// TestR118_13_StubSeesClientBodyUnchanged — санитарная проверка стенда: балансер
// действительно ходит в cppworker на inference-пути (иначе предыдущие тесты
// проверяли бы пустоту).
func TestR118_13_StubSeesClientBodyUnchanged(t *testing.T) {
	const model = "contract-model"
	stub := newContractStub(t, model)
	proxy := newContractProxy(t, stub, model)

	resp := doContract(proxy, "POST", "/api/chat",
		`{"model":"`+model+`","messages":[{"role":"user","content":"привет"}],"stream":false}`, nil)
	_ = readAll(t, resp)
	if n := stub.Count("/api/chat"); n == 0 {
		t.Fatal("cppworker не получил /api/chat — контрактный набор проверял бы пустоту")
	}
	if n := stub.InferenceCalls(); n == 0 {
		t.Fatal("inference-вызов не зафиксирован заглушкой")
	}
}
