package balancer

// R99 (S8 плана 2026-10-07): диспетчер пачки изображений.
//
// Проверяем три уровня: (1) чистые функции раскладки/подмены тела/склейки ответов;
// (2) живой фан-аут по двум фейковым image-бэкендам с настоящими HTTP-ответами;
// (3) консервативность — там, где пачка неприменима, поведение НЕ меняется.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// ============ Чистые функции ============

func TestImageBatchPlanR99(t *testing.T) {
	cases := []struct {
		name     string
		n        int
		backends []string
		want     []imageBatchChunkR99
	}{
		{"4 на 2 узла", 4, []string{"a", "b"}, []imageBatchChunkR99{{"a", 2}, {"b", 2}}},
		{"5 на 2 узла", 5, []string{"a", "b"}, []imageBatchChunkR99{{"a", 3}, {"b", 2}}},
		{"1 картинка", 1, []string{"a", "b"}, []imageBatchChunkR99{{"a", 1}}},
		{"узлов больше, чем картинок", 2, []string{"a", "b", "c", "d"},
			[]imageBatchChunkR99{{"a", 1}, {"b", 1}}},
		{"нет картинок", 0, []string{"a", "b"}, nil},
		{"нет узлов", 4, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := imageBatchPlanR99(tc.n, tc.backends)
			if len(got) != len(tc.want) {
				t.Fatalf("задач = %d, ожидалось %d: %+v", len(got), len(tc.want), got)
			}
			total := 0
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("задача %d = %+v, ожидалось %+v", i, got[i], tc.want[i])
				}
				total += got[i].Count
			}
			if len(tc.want) > 0 && total != tc.n {
				t.Errorf("сумма картинок = %d, ожидалось %d (ничего не теряем)", total, tc.n)
			}
		})
	}
}

func TestImageBatchSpecR99(t *testing.T) {
	cases := []struct {
		name      string
		path      string
		body      string
		wantCount int
		wantStyle string
		wantOK    bool
	}{
		{"openai n=4", "/v1/images/generations", `{"prompt":"cat","n":4}`, 4, imageBatchStyleOpenAI, true},
		{"openai без n", "/v1/images/generations", `{"prompt":"cat"}`, 1, imageBatchStyleOpenAI, true},
		{"a1111 batch×iter", "/sdapi/v1/txt2img", `{"batch_size":2,"n_iter":3}`, 6, imageBatchStyleA1111, true},
		{"a1111 только iter", "/sdapi/v1/txt2img", `{"n_iter":2}`, 2, imageBatchStyleA1111, true},
		{"a1111 camelCase", "/sdapi/v1/txt2img", `{"nIter":3}`, 3, imageBatchStyleA1111, true},
		{"a1111 по умолчанию", "/sdapi/v1/txt2img", `{}`, 1, imageBatchStyleA1111, true},
		{"пустое тело", "/v1/images/generations", ``, 1, imageBatchStyleOpenAI, true},
		{"async-путь не пачка", "/api/image/generate", `{"n":4}`, 0, "", false},
		{"edits не пачка", "/v1/images/edits", `{"n":4}`, 0, "", false},
		{"битый JSON", "/v1/images/generations", `{не json`, 0, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			count, style, ok := imageBatchSpecR99([]byte(tc.body), tc.path)
			if ok != tc.wantOK || count != tc.wantCount || style != tc.wantStyle {
				t.Errorf("получено (%d, %q, %v), ожидалось (%d, %q, %v)",
					count, style, ok, tc.wantCount, tc.wantStyle, tc.wantOK)
			}
		})
	}
}

func TestImageBatchBodyR99_PreservesOtherFields(t *testing.T) {
	body := []byte(`{"prompt":"кот","steps":20,"size":"512x512","n":4,"seed":42}`)

	openai, err := imageBatchBodyR99(body, imageBatchStyleOpenAI, 2)
	if err != nil {
		t.Fatalf("openai: %v", err)
	}
	var fields map[string]interface{}
	if err := json.Unmarshal(openai, &fields); err != nil {
		t.Fatalf("результат не JSON: %v", err)
	}
	if fields["n"] != float64(2) {
		t.Errorf("n = %v, ожидалось 2", fields["n"])
	}
	// Остальные поля запроса обязаны дойти до движка без изменений.
	for key, want := range map[string]interface{}{"prompt": "кот", "steps": float64(20), "size": "512x512", "seed": float64(42)} {
		if fields[key] != want {
			t.Errorf("%s = %v, ожидалось %v", key, fields[key], want)
		}
	}

	a1111, err := imageBatchBodyR99([]byte(`{"prompt":"кот","batch_size":4,"n_iter":2}`), imageBatchStyleA1111, 3)
	if err != nil {
		t.Fatalf("a1111: %v", err)
	}
	fields = map[string]interface{}{}
	if err := json.Unmarshal(a1111, &fields); err != nil {
		t.Fatalf("результат не JSON: %v", err)
	}
	if fields["batch_size"] != float64(3) || fields["n_iter"] != float64(1) {
		t.Errorf("a1111: batch_size=%v n_iter=%v, ожидалось 3 и 1", fields["batch_size"], fields["n_iter"])
	}

	if _, err := imageBatchBodyR99([]byte(`{не json`), imageBatchStyleOpenAI, 2); err == nil {
		t.Error("битое тело должно давать ошибку, а не тихо уезжать в движок")
	}
}

func TestMergeImageBatchResponsesR99(t *testing.T) {
	openai := [][]byte{
		[]byte(`{"created":111,"data":[{"b64_json":"A"},{"b64_json":"B"}]}`),
		[]byte(`{"created":222,"data":[{"b64_json":"C"}]}`),
	}
	merged, images, err := mergeImageBatchResponsesR99(imageBatchStyleOpenAI, openai)
	if err != nil {
		t.Fatalf("openai merge: %v", err)
	}
	if images != 3 {
		t.Errorf("картинок = %d, ожидалось 3", images)
	}
	var payload struct {
		Created int64               `json:"created"`
		Data    []map[string]string `json:"data"`
	}
	if err := json.Unmarshal(merged, &payload); err != nil {
		t.Fatalf("merged не JSON: %v", err)
	}
	// Порядок задач сохраняется: клиент получает картинки предсказуемо.
	if len(payload.Data) != 3 || payload.Data[0]["b64_json"] != "A" ||
		payload.Data[1]["b64_json"] != "B" || payload.Data[2]["b64_json"] != "C" {
		t.Errorf("порядок/состав нарушены: %+v", payload.Data)
	}
	if payload.Created != 111 {
		t.Errorf("created = %d, ожидалось значение первой задачи (111)", payload.Created)
	}

	a1111 := [][]byte{
		[]byte(`{"images":["A"],"parameters":{"steps":20},"info":"v1"}`),
		[]byte(`{"images":["B"],"parameters":{"steps":99},"info":"v2"}`),
	}
	merged, images, err = mergeImageBatchResponsesR99(imageBatchStyleA1111, a1111)
	if err != nil {
		t.Fatalf("a1111 merge: %v", err)
	}
	if images != 2 {
		t.Errorf("картинок = %d, ожидалось 2", images)
	}
	var a1111Payload struct {
		Images     []string        `json:"images"`
		Parameters json.RawMessage `json:"parameters"`
		Info       string          `json:"info"`
	}
	if err := json.Unmarshal(merged, &a1111Payload); err != nil {
		t.Fatalf("merged не JSON: %v", err)
	}
	if len(a1111Payload.Images) != 2 || a1111Payload.Images[0] != "A" || a1111Payload.Images[1] != "B" {
		t.Errorf("images = %v", a1111Payload.Images)
	}
	// parameters/info — от ПЕРВОЙ задачи: смесь настроек разных узлов в одном
	// ответе была бы враньём о том, с какими параметрами получены картинки.
	if !strings.Contains(string(a1111Payload.Parameters), "20") {
		t.Errorf("parameters должны быть от первой задачи: %s", a1111Payload.Parameters)
	}
	if a1111Payload.Info != "v1" {
		t.Errorf("info = %q, ожидалось v1", a1111Payload.Info)
	}

	if _, _, err := mergeImageBatchResponsesR99(imageBatchStyleOpenAI, [][]byte{[]byte(`{битый`)}); err == nil {
		t.Error("битый ответ задачи должен ломать склейку, а не давать пустой ответ")
	}
	if _, _, err := mergeImageBatchResponsesR99(imageBatchStyleOpenAI, nil); err == nil {
		t.Error("пустой набор ответов — ошибка")
	}
}

// ============ Живой фан-аут ============

type imageBatchStubR99 struct {
	srv *httptest.Server
	mu  sync.Mutex
	// got — счётчики ТОЛЬКО по запросам генерации: фоновые пробы балансера
	// (capabilities/health) тоже стучатся в image-бэкенд, и без этого фильтра
	// тест мерил бы их, а не раскладку.
	got []imageBatchStubHitR99
}

type imageBatchStubHitR99 struct {
	Path  string
	Count int
}

func newImageBatchStubR99(t *testing.T) *imageBatchStubR99 {
	t.Helper()
	stub := &imageBatchStubR99{}
	stub.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var payload struct {
			N         int `json:"n"`
			BatchSize int `json:"batch_size"`
		}
		_ = json.Unmarshal(raw, &payload)
		count := payload.N
		if count <= 0 {
			count = payload.BatchSize
		}
		if count <= 0 {
			count = 1
		}
		stub.mu.Lock()
		stub.got = append(stub.got, imageBatchStubHitR99{Path: r.URL.Path, Count: count})
		stub.mu.Unlock()

		data := make([]map[string]string, 0, count)
		for i := 0; i < count; i++ {
			data = append(data, map[string]string{"b64_json": "image"})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"created": 1, "data": data})
	}))
	t.Cleanup(stub.srv.Close)
	return stub
}

func (s *imageBatchStubR99) requests() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int, 0, len(s.got))
	for _, hit := range s.got {
		out = append(out, hit.Count)
	}
	return out
}

// generationRequests — только запросы генерации (без фоновых проб).
func (s *imageBatchStubR99) generationRequests() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int, 0, len(s.got))
	for _, hit := range s.got {
		if strings.HasPrefix(hit.Path, "/v1/images/") || hit.Path == "/sdapi/v1/txt2img" {
			out = append(out, hit.Count)
		}
	}
	return out
}

// newImageBatchProxyR99 — прокси с двумя image-бэкендами на фейковых воркерах.
//
// GPUIndex разные (0 и 1) намеренно: лок GPU берётся по паре (хост, индекс), и на
// одной машине два узла без индексов считались бы одним локом — второй узел
// получил бы отказ гейта и в раскладке не участвовал. На живом стенде это две
// карты на двух машинах, здесь — две карты на одной.
func newImageBatchProxyR99(t *testing.T, stubCount int) (*Proxy, []*imageBatchStubR99) {
	t.Helper()
	stubs := make([]*imageBatchStubR99, 0, stubCount)
	backends := make([]types.Backend, 0, stubCount)
	for i := 0; i < stubCount; i++ {
		stub := newImageBatchStubR99(t)
		stubs = append(stubs, stub)
		_, port, _ := parseBackendHostPort(strings.TrimPrefix(stub.srv.URL, "http://"))
		backends = append(backends, types.Backend{
			ID:                "img-" + string(rune('a'+i)),
			Name:              "image worker",
			Host:              "127.0.0.1",
			ImagePort:         port,
			GPUIndex:          types.GPUIndexPtr(i),
			Type:              types.BackendTypeImage,
			Status:            types.StatusHealthy,
			MaxConcurrentReqs: 2,
		})
	}

	cfg := createTestConfig()
	cfg.Backends = backends
	cfg.Balancing.OperatingMode = "standard"
	p := newProxyWithCleanup(t, cfg)
	for _, b := range p.GetAllBackends() {
		p.UpdateBackendStatus(b.ID, types.StatusHealthy)
	}
	return p, stubs
}

func postImageGenerationsR99(t *testing.T, p *Proxy, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	if !p.imageRouter.Route(rec, req) {
		t.Fatalf("Route не обработал image-запрос (rec=%d)", rec.Code)
	}
	return rec
}

func TestImageBatchR99_SplitsFourImagesAcrossTwoBackends(t *testing.T) {
	p, stubs := newImageBatchProxyR99(t, 2)

	rec := postImageGenerationsR99(t, p, `{"prompt":"кот","n":4,"size":"512x512"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Data []map[string]string `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("ответ не JSON: %v", err)
	}
	if len(payload.Data) != 4 {
		t.Fatalf("картинок в ответе = %d, ожидалось 4 (склейка обязана не терять)", len(payload.Data))
	}

	// Каждый узел получил РОВНО одну задачу на 2 картинки — это и есть раскладка.
	for i, stub := range stubs {
		got := stub.generationRequests()
		if len(got) != 1 {
			t.Errorf("узел %d получил %d запросов, ожидался 1: %v", i, len(got), got)
			continue
		}
		if got[0] != 2 {
			t.Errorf("узел %d получил n=%d, ожидалось 2", i, got[0])
		}
	}

	// Следы раскладки в ответе: на живом стенде по ним видно, что пачка разошлась.
	if batch := rec.Header().Get("X-LB-Image-Batch"); !strings.Contains(batch, "2 backends") {
		t.Errorf("X-LB-Image-Batch = %q", batch)
	}
	if parts := rec.Header().Get("X-LB-Image-Batch-Parts"); parts == "" {
		t.Error("X-LB-Image-Batch-Parts должен показывать, кто сколько считал")
	}
}

func TestImageBatchR99_DoesNotBatchSingleImage(t *testing.T) {
	p, stubs := newImageBatchProxyR99(t, 2)

	rec := postImageGenerationsR99(t, p, `{"prompt":"кот","n":1}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-LB-Image-Batch"); got != "" {
		t.Errorf("одна картинка не раскладывается, но заголовок есть: %q", got)
	}
	total, counts := 0, []int{}
	for _, stub := range stubs {
		got := stub.generationRequests()
		total += len(got)
		counts = append(counts, got...)
	}
	if total != 1 {
		t.Errorf("запросов к воркерам = %d, ожидался 1 (обычный путь): %v", total, counts)
	}
}

func TestImageBatchR99_FallsBackWithOneBackend(t *testing.T) {
	p, stubs := newImageBatchProxyR99(t, 1)

	rec := postImageGenerationsR99(t, p, `{"prompt":"кот","n":4}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-LB-Image-Batch"); got != "" {
		t.Errorf("с одним узлом раскладки нет: %q", got)
	}
	// Запрос ушёл целиком (n=4), а не порезанный: без раскладки поведение прежнее.
	got := stubs[0].generationRequests()
	if len(got) != 1 || got[0] != 4 {
		t.Errorf("запросы к единственному узлу = %v, ожидался [4]", got)
	}
}

func TestImageBatchR99_AsyncPathNotBatched(t *testing.T) {
	p, stubs := newImageBatchProxyR99(t, 2)

	// /api/image/generate — постановка асинхронной джобы: у неё нет «общего
	// ответа» из картинок, и раскладывать её нельзя.
	req := httptest.NewRequest(http.MethodPost, "/api/image/generate", strings.NewReader(`{"prompt":"кот","batch_count":4}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.imageRouter.Route(rec, req)

	for i, stub := range stubs {
		got := stub.generationRequests()
		if len(got) > 1 {
			t.Errorf("узел %d получил %d запросов: асинхронную постановку раскладывать нельзя: %v", i, len(got), got)
		}
	}
}

func TestImageBatchR99_PartialFailureIsNotSilent(t *testing.T) {
	p, stubs := newImageBatchProxyR99(t, 2)

	// Второй узел начинает отвечать ошибкой: клиент обязан получить ошибку, а не
	// «2 картинки вместо 4» — молча недодать изображения хуже всего.
	stubs[1].srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"CUDA out of memory","code":"oom"}}`))
	})

	rec := postImageGenerationsR99(t, p, `{"prompt":"кот","n":4}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("частичный отказ не должен выглядеть успехом: %s", rec.Body.String())
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("HTTP %d, ожидался статус упавшего узла (500): %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-LB-Image-Batch-Failed-Backend"); got != "img-b" {
		t.Errorf("ответ должен называть упавший узел, получено %q", got)
	}
	if !strings.Contains(rec.Body.String(), "out of memory") {
		t.Errorf("текст ошибки движка должен дойти до клиента: %s", rec.Body.String())
	}
}
