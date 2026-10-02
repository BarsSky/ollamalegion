// image_request_metrics_test.go — R-Image Phase 8 (2026-10-03): поток
// image-запросов (счётчики, лента, исходы) и разбор метаданных запроса.
//
// ЗАЧЕМ ЭТИ ТЕСТЫ. До Phase 8 про image-трафик не знал никто: ImageRouter шёл
// мимо recordRequest, и в Monitor у живого image-бэкенда было RPS=0.0,
// Avg RT=«-», Active=«0/10». Проверяем ровно то, что оператор видит:
//   - счётчики исходов и in-flight (в том числе async-постановка 202);
//   - ленту последних запросов с параметрами генерации;
//   - отказы гейта и «нет бэкенда» — отдельным исходом, а не тишиной;
//   - что тело запроса после снятия метаданных доезжает до воркера ЦЕЛИКОМ
//     (иначе метрика ломала бы саму генерацию).
package balancer

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// --- 1. Хранилище счётчиков ---------------------------------------------------

func TestImageRequestStore_CountsOKAndFeed(t *testing.T) {
	s := newImageRequestStore()
	h := s.begin("img-1", types.ImageRequestBrief{Path: "/v1/images/generations", Model: "sd15-q4", Prompt: "cat"})
	h.finish(types.ImageRequestStatusOK, http.StatusOK, "", "", 1)

	m := s.snapshot("img-1")
	if m.Total != 1 || m.OK != 1 || m.InFlight != 0 {
		t.Fatalf("snapshot = total:%d ok:%d inFlight:%d, want 1/1/0", m.Total, m.OK, m.InFlight)
	}
	if m.LastDurationMs < 0 {
		t.Errorf("last_duration_ms = %d, want >= 0", m.LastDurationMs)
	}
	if m.RPS <= 0 {
		t.Errorf("rps = %v, want > 0 (запрос только что был)", m.RPS)
	}

	feed := s.recentFor("img-1", 5)
	if len(feed) != 1 {
		t.Fatalf("recent len = %d, want 1", len(feed))
	}
	if feed[0].Status != types.ImageRequestStatusOK || feed[0].Model != "sd15-q4" {
		t.Errorf("recent[0] = %+v, want status=ok model=sd15-q4", feed[0])
	}
	if g := s.aggregate(); g.Total != 1 || g.OK != 1 {
		t.Errorf("aggregate = total:%d ok:%d, want 1/1", g.Total, g.OK)
	}
	if n := len(s.recentGlobal(10)); n != 1 {
		t.Errorf("global recent len = %d, want 1", n)
	}
}

func TestImageRequestStore_CountsInFlightBeforeFinish(t *testing.T) {
	s := newImageRequestStore()
	s.begin("img-1", types.ImageRequestBrief{Path: "/v1/images/generations"})

	if m := s.snapshot("img-1"); m.InFlight != 1 || m.Total != 1 {
		t.Fatalf("до финиша: inFlight=%d total=%d, want 1/1", m.InFlight, m.Total)
	}
	// Лента обязана показывать запрос УЖЕ В ПОЛЁТЕ: иначе долгая генерация
	// (десятки секунд) выглядит как «ничего не пришло».
	feed := s.recentFor("img-1", 5)
	if len(feed) != 1 || feed[0].Status != types.ImageRequestStatusAccepted {
		t.Fatalf("во время генерации recent = %+v, want одну запись accepted", feed)
	}
}

func TestImageRequestStore_FinishIsIdempotent(t *testing.T) {
	s := newImageRequestStore()
	h := s.begin("img-1", types.ImageRequestBrief{Path: "/v1/images/generations"})
	h.finish(types.ImageRequestStatusOK, http.StatusOK, "", "", 1)
	h.finish(types.ImageRequestStatusFailed, http.StatusBadGateway, "image_backend_error", "dup", 0)

	m := s.snapshot("img-1")
	if m.OK != 1 || m.Failed != 0 {
		t.Fatalf("двойной finish посчитан дважды: ok=%d failed=%d", m.OK, m.Failed)
	}
	if feed := s.recentFor("img-1", 5); len(feed) != 1 {
		t.Fatalf("лента = %d записей, want 1 (запись заменяется, а не дублируется)", len(feed))
	}
}

func TestImageRequestStore_AsyncAcceptedThenFinished(t *testing.T) {
	s := newImageRequestStore()
	h := s.begin("img-1", types.ImageRequestBrief{Path: "/api/image/generate"})
	h.markAccepted()

	m := s.snapshot("img-1")
	if m.InFlight != 1 || m.Accepted != 1 {
		t.Fatalf("после 202: inFlight=%d accepted=%d, want 1/1", m.InFlight, m.Accepted)
	}
	if feed := s.recentFor("img-1", 5); len(feed) != 1 || feed[0].Status != types.ImageRequestStatusAccepted {
		t.Fatalf("лента после 202 = %+v, want accepted", feed)
	}

	h.finish(types.ImageRequestStatusFinished, http.StatusAccepted, "", "", 0)
	m = s.snapshot("img-1")
	if m.InFlight != 0 || m.Finished != 1 {
		t.Fatalf("после завершения: inFlight=%d finished=%d, want 0/1", m.InFlight, m.Finished)
	}
	feed := s.recentFor("img-1", 5)
	if len(feed) != 1 || feed[0].Status != types.ImageRequestStatusFinished {
		t.Fatalf("лента после завершения = %+v, want finished (та же запись)", feed)
	}
}

func TestImageRequestStore_GateDeniedIsRejectedNotFailed(t *testing.T) {
	s := newImageRequestStore()
	s.gateDenied("img-1", "insufficient_vram", "не влезает")
	s.gateDenied("img-1", "insufficient_vram", "не влезает")
	s.gateDenied("img-1", "gpu_busy", "занято")

	m := s.snapshot("img-1")
	if m.Rejected != 3 || m.Failed != 0 || m.Total != 3 {
		t.Fatalf("rejected=%d failed=%d total=%d, want 3/0/3", m.Rejected, m.Failed, m.Total)
	}
	if m.GateDeniedByCode["insufficient_vram"] != 2 || m.GateDeniedByCode["gpu_busy"] != 1 {
		t.Errorf("gate_denied_by_code = %v, want insufficient_vram:2 gpu_busy:1", m.GateDeniedByCode)
	}
	if feed := s.recentFor("img-1", 5); len(feed) != 3 || feed[0].Code != "gpu_busy" {
		t.Fatalf("лента отказов = %+v, want 3 записи, свежая gpu_busy", feed)
	}
}

func TestImageRequestStore_FailuresByCode(t *testing.T) {
	s := newImageRequestStore()
	for i := 0; i < 3; i++ {
		h := s.begin("img-1", types.ImageRequestBrief{Path: "/sdapi/v1/txt2img"})
		h.finish(types.ImageRequestStatusFailed, http.StatusInternalServerError, "engine_oom", "boom", 0)
	}
	m := s.snapshot("img-1")
	if m.Failed != 3 || m.FailuresByCode["engine_oom"] != 3 {
		t.Fatalf("failed=%d byCode=%v, want 3 и engine_oom:3", m.Failed, m.FailuresByCode)
	}
	if m.LastDurationMs < 0 || m.AvgDurationMs < 0 {
		t.Errorf("длительности отрицательные: last=%d avg=%d", m.LastDurationMs, m.AvgDurationMs)
	}
}

func TestImageRequestStore_FeedIsBoundedAndNewestFirst(t *testing.T) {
	s := newImageRequestStore()
	// Больше, чем общий предел: проверяем ОБА ограничителя (на бэкенд и общий).
	for i := 0; i < imageRecentLimitGlobal+10; i++ {
		h := s.begin("img-1", types.ImageRequestBrief{Path: "/v1/images/generations", Prompt: "p"})
		h.finish(types.ImageRequestStatusOK, http.StatusOK, "", "", 1)
	}
	feed := s.recentFor("img-1", 1000)
	if len(feed) != imageRecentLimitPerBackend {
		t.Fatalf("лента бэкенда = %d записей, want %d (жёсткий предел памяти)", len(feed), imageRecentLimitPerBackend)
	}
	if !feed[0].At.After(feed[len(feed)-1].At) && !feed[0].At.Equal(feed[len(feed)-1].At) {
		t.Error("лента должна начинаться со свежих записей")
	}
	if n := len(s.recentGlobal(1000)); n != imageRecentLimitGlobal {
		t.Errorf("общая лента = %d записей, want %d", n, imageRecentLimitGlobal)
	}
}

func TestImageRequestStore_RPSUsesSixtySecondWindow(t *testing.T) {
	s := newImageRequestStore()
	for i := 0; i < 6; i++ {
		s.begin("img-1", types.ImageRequestBrief{Path: "/v1/images/generations"})
	}
	// Сдвигаем часть стартов за окно 60 с: RPS должен считать только свежие.
	s.mu.Lock()
	b := s.perBackend["img-1"]
	old := time.Now().Add(-2 * time.Minute)
	b.history[0], b.history[1], b.history[2] = old, old, old
	s.mu.Unlock()

	// Новый старт обрезает окно.
	s.begin("img-1", types.ImageRequestBrief{Path: "/v1/images/generations"})
	// Ожидаем 4 свежих (3 из шести + новый) за 60 с → 4/60.
	got := s.snapshot("img-1").RPS
	want := 4.0 / 60.0
	if got < want-0.0001 || got > want+0.0001 {
		t.Fatalf("rps = %v, want %v (окно 60 с, старые старты отброшены)", got, want)
	}
}

func TestImageRequestStore_PruneOnlyWhenMapGrows(t *testing.T) {
	s := newImageRequestStore()
	s.begin("gone", types.ImageRequestBrief{Path: "/v1/images/generations"}).finish(types.ImageRequestStatusOK, 200, "", "", 1)
	// Маленькая карта: prune НЕ трогает записи (иначе счётчики только что
	// зарегистрированного бэкенда пропадали бы до первого снимка поллера).
	s.prune(map[string]bool{"img-1": true}, time.Now())
	if s.snapshot("gone").Total != 1 {
		t.Fatal("prune удалил счётчики при маленькой карте")
	}

	for i := 0; i < imageRequestStoreMaxBackends+5; i++ {
		id := "b" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('a'+(i/676)%26))
		s.begin(id, types.ImageRequestBrief{Path: "/v1/images/generations"})
	}
	// Делаем запись «старой» и только потом просим prune.
	s.mu.Lock()
	if b := s.perBackend["gone"]; b != nil {
		b.lastAt = time.Now().Add(-2 * time.Hour)
	}
	s.mu.Unlock()
	s.prune(map[string]bool{"img-1": true}, time.Now())
	if s.snapshot("gone").Total != 0 {
		t.Fatal("prune не удалил счётчики исчезнувшего бэкенда при переросшей карте")
	}
	if s.snapshot("img-1").Total != 0 {
		t.Fatal("prune не должен создавать записи для отсутствующих бэкендов")
	}
}

// --- 2. Метаданные запроса ----------------------------------------------------

func TestImageRequestBriefFor_ParsesJSONAndRestoresBody(t *testing.T) {
	const body = `{"model":"dall-e-2","prompt":"рыжий кот","size":"512x768","n":2,"steps":12}`
	r := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r = withRequestSurface(r, surfaceOpenAI)

	brief := imageRequestBriefFor(r)
	if brief.Model != "dall-e-2" || brief.Width != 512 || brief.Height != 768 || brief.Steps != 12 || brief.Batch != 2 {
		t.Fatalf("brief = %+v, want model/size/steps/batch разобраны", brief)
	}
	if brief.Prompt != "рыжий кот" {
		t.Errorf("prompt = %q, want «рыжий кот»", brief.Prompt)
	}
	if brief.Surface != "openai" || brief.Path != "/v1/images/generations" {
		t.Errorf("surface/path = %q/%q", brief.Surface, brief.Path)
	}

	// Тело обязано доехать до воркера ЦЕЛИКОМ (метрика не имеет права ломать
	// генерацию): читаем его ещё раз и сравниваем с исходным.
	got := make([]byte, r.ContentLength)
	if _, err := r.Body.Read(got); err != nil && err.Error() != "EOF" {
		t.Fatalf("read restored body: %v", err)
	}
	if string(got) != body {
		t.Fatalf("восстановленное тело = %q, want исходное", string(got))
	}
}

func TestImageRequestBriefFor_SkipsMultipartAndOversized(t *testing.T) {
	mp := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader("--boundary--"))
	mp.Header.Set("Content-Type", "multipart/form-data; boundary=boundary")
	if brief := imageRequestBriefFor(mp); brief.Model != "" || brief.Prompt != "" {
		t.Errorf("multipart разобран (%+v) — картинки в ленте метрик недопустимы", brief)
	}

	big := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"prompt":"`+strings.Repeat("x", imageRequestMetaBodyLimit+10)+`"}`))
	big.Header.Set("Content-Type", "application/json")
	before := big.ContentLength
	if brief := imageRequestBriefFor(big); brief.Prompt != "" {
		t.Error("крупное тело не должно разбираться")
	}
	if big.ContentLength != before {
		t.Error("ContentLength изменён — тело поедет обрезанным")
	}
}

func TestImageRequestBriefFor_PromptArrayAndRuneTruncation(t *testing.T) {
	long := strings.Repeat("к", imagePromptLimit+50)
	r := httptest.NewRequest(http.MethodPost, "/sdapi/v1/txt2img",
		strings.NewReader(`{"prompt":["`+long+`"],"batch_size":3,"width":640,"height":640}`))
	r.Header.Set("Content-Type", "application/json")

	brief := imageRequestBriefFor(r)
	if brief.Batch != 3 || brief.Width != 640 {
		t.Errorf("brief = %+v, want batch=3 640x640", brief)
	}
	if got := len([]rune(brief.Prompt)); got != imagePromptLimit+1 { // +1 на «…»
		t.Errorf("длина промпта = %d рун, want %d (+многоточие)", got, imagePromptLimit+1)
	}
}

func TestPeekImageErrorBody_OpenAIAndPlainEnvelopes(t *testing.T) {
	openai := &http.Response{
		StatusCode: 500,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"out of memory","type":"invalid_request_error","code":"engine_oom"}}`)),
	}
	code, msg := peekImageErrorBody(openai)
	if code != "engine_oom" || msg != "out of memory" {
		t.Fatalf("OpenAI-конверт: code=%q msg=%q", code, msg)
	}
	// Тело восстановлено — копирование ответа клиенту не пострадало.
	rest, _ := io.ReadAll(openai.Body)
	if !strings.Contains(string(rest), "engine_oom") {
		t.Error("тело ошибки не восстановлено после подглядывания")
	}

	plain := &http.Response{
		StatusCode: 500,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":"sd-server: connection refused"}`)),
	}
	code, msg = peekImageErrorBody(plain)
	if code != "" || !strings.Contains(msg, "connection refused") {
		t.Fatalf("плоский конверт: code=%q msg=%q", code, msg)
	}

	html := &http.Response{
		StatusCode: 502,
		Header:     http.Header{"Content-Type": []string{"text/html"}},
		Body:       io.NopCloser(strings.NewReader("<html>bad gateway</html>")),
	}
	if code, msg := peekImageErrorBody(html); code != "" || msg != "" {
		t.Errorf("не-JSON тело не должно разбираться: code=%q msg=%q", code, msg)
	}
}

// --- 3. HTTP-путь: генерация через OpenAI-поверхность ------------------------

// fakeImageWorker поднимает воркер с контрактом /api/image/models и настраиваемым
// ответом на генерацию.
func fakeImageWorker(t *testing.T, gen http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/image/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"state":"loaded"}`))
			return
		}
		gen(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestImageRequestMetrics_GenerationIsCountedPerBackend(t *testing.T) {
	worker := fakeImageWorker(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"iVBORw0KGgo="}]}`))
	})
	p := newImageTestProxy(t, portFromURL(t, worker.URL))
	surface := httptest.NewServer(NewOpenAISurface(p))
	defer surface.Close()

	body := `{"model":"dall-e-2","prompt":"a cat","size":"512x512","n":1,"steps":8}`
	resp, err := http.Post(surface.URL+"/v1/images/generations", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	store := p.imageResources().imageRequests()
	m := store.snapshot("img-1")
	if m.Total != 1 || m.OK != 1 || m.InFlight != 0 {
		t.Fatalf("после генерации: total=%d ok=%d inFlight=%d, want 1/1/0", m.Total, m.OK, m.InFlight)
	}
	feed := store.recentFor("img-1", 5)
	if len(feed) != 1 {
		t.Fatalf("лента = %d записей, want 1", len(feed))
	}
	got := feed[0]
	if got.Path != "/v1/images/generations" || got.Model != "dall-e-2" || got.Width != 512 || got.Height != 512 {
		t.Errorf("запись ленты = %+v, want путь/модель/размер из запроса", got)
	}
	if got.Prompt != "a cat" || got.Status != types.ImageRequestStatusOK || got.Images != 1 {
		t.Errorf("запись ленты = %+v, want prompt=a cat status=ok images=1", got)
	}
	if got.Surface != "openai" {
		t.Errorf("surface = %q, want openai (запрос шёл на 18079)", got.Surface)
	}

	// Те же данные обязаны доехать до /api/v1/metrics: именно их читает Monitor.
	snap := p.imageResources().snapshotMetrics()
	agg, ok := snap["requests"].(map[string]interface{})
	if !ok {
		t.Fatalf("в snapshotMetrics нет агрегата requests: %v", snap["requests"])
	}
	if agg["total"] != int64(1) || agg["ok"] != int64(1) {
		t.Errorf("агрегат requests = %v, want total=1 ok=1", agg)
	}
	if _, hasRecent := agg["recent"]; !hasRecent {
		t.Error("в агрегате requests нет общей ленты recent")
	}
	backends, _ := snap["backends"].(map[string]interface{})
	entry, _ := backends["img-1"].(map[string]interface{})
	if entry["requests"] == nil {
		t.Fatalf("в per-backend снимке нет requests: %v", entry)
	}
}

func TestImageRequestMetrics_FailedGenerationCarriesEngineCode(t *testing.T) {
	worker := fakeImageWorker(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"out of memory","code":"engine_oom"}}`))
	})
	p := newImageTestProxy(t, portFromURL(t, worker.URL))
	surface := httptest.NewServer(NewOpenAISurface(p))
	defer surface.Close()

	resp, err := http.Post(surface.URL+"/v1/images/generations", "application/json",
		strings.NewReader(`{"prompt":"a cat"}`))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	resp.Body.Close()

	store := p.imageResources().imageRequests()
	m := store.snapshot("img-1")
	if m.Failed != 1 || m.OK != 0 {
		t.Fatalf("failed=%d ok=%d, want 1/0", m.Failed, m.OK)
	}
	if m.FailuresByCode["engine_oom"] != 1 {
		t.Errorf("failures_by_code = %v, want engine_oom:1 (код движка обязан быть виден)", m.FailuresByCode)
	}
	feed := store.recentFor("img-1", 1)
	if len(feed) != 1 || feed[0].Code != "engine_oom" || feed[0].Error != "out of memory" {
		t.Errorf("запись ленты = %+v, want code=engine_oom error=out of memory", feed)
	}
	if feed[0].HTTPStatus != http.StatusInternalServerError {
		t.Errorf("httpStatus = %d, want 500", feed[0].HTTPStatus)
	}
}

func TestImageRequestMetrics_NoBackendIsRejectedNotSilent(t *testing.T) {
	p := newProxyWithCleanup(t, &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "127.0.0.1", StatePath: t.TempDir() + "/state.json"},
		Balancing: types.BalancingSettings{
			Algorithm: types.AlgorithmResourceAware, OperatingMode: "standard",
			RequestTimeout: 5, QueueTimeout: 2, QueueMaxSize: 10, QueueWorkers: 1,
		},
	})
	surface := httptest.NewServer(NewOpenAISurface(p))
	defer surface.Close()

	resp, err := http.Post(surface.URL+"/v1/images/generations", "application/json",
		strings.NewReader(`{"prompt":"a cat"}`))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (нет image-бэкенда)", resp.StatusCode)
	}

	store := p.imageResources().imageRequests()
	agg := store.aggregate()
	if agg.Rejected != 1 {
		t.Fatalf("rejected = %d, want 1 (отказ «нет бэкенда» обязан быть в метриках)", agg.Rejected)
	}
	if agg.GateDeniedByCode["image_backend_unavailable"] != 1 {
		t.Errorf("gate_denied_by_code = %v, want image_backend_unavailable:1", agg.GateDeniedByCode)
	}
}

// --- 4. Счётчик картинок и утилиты -------------------------------------------

func TestImageResponseImages(t *testing.T) {
	brief := types.ImageRequestBrief{Batch: 4}
	if got := imageResponseImages(brief, http.StatusOK); got != 4 {
		t.Errorf("images = %d, want 4", got)
	}
	if got := imageResponseImages(brief, http.StatusInternalServerError); got != 0 {
		t.Errorf("на ошибке images = %d, want 0", got)
	}
	if got := imageResponseImages(types.ImageRequestBrief{}, http.StatusOK); got != 1 {
		t.Errorf("images без batch = %d, want 1", got)
	}
}

func TestPercentile(t *testing.T) {
	sorted := []int64{10, 20, 30, 40, 50}
	if got := percentile(sorted, 0.50); got != 30 {
		t.Errorf("p50 = %d, want 30", got)
	}
	if got := percentile(sorted, 0.95); got != 50 {
		t.Errorf("p95 = %d, want 50", got)
	}
	if got := percentile(nil, 0.5); got != 0 {
		t.Errorf("p50 пустой выборки = %d, want 0", got)
	}
}
