// image_resources_test.go — R-Image Phase 6 (2026-10-02): тесты VRAM-гейта и
// политик сосуществования image-генерации с текстовым инференсом.
//
// БЕЗ ЖИВОГО ДВИЖКА: image-воркер — httptest-мок, который отдаёт
// GET /api/image/models (контракт Phase 3), GET /api/image/capabilities
// (снимок VRAM) и синхронный ответ генерации. Кластер — ровно сценарий Phase 6:
// image-бэкенд и текстовый бэкенд на ОДНОМ хосте.
//
// Покрытие (нумерация — из задания Phase 6):
//  1. влезает → 200 и запрос реально уходит в воркер;
//  2. не влезает → 503 insufficient_vram + hint (OOM-лестница);
//  3. оценка unknown → решение гейта совпадает с вердиктом
//     types.EvaluateImageVRAM (политику «блокировать ли неизвестную оценку»
//     задаёт замороженный контракт, а не балансер);
//  4. gateDisabled → пропуск;
//  5. headroom учитывается;
//  6. exclusive: второй одновременный запрос ждёт и получает 429;
//  7. offload/dedicated: лок не берётся;
//  8. предохранитель освобождает лок зависшей генерации;
//  9. гонок нет (-race прогоняется в CI; тесты не используют общий мутабельный стейт).
package balancer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ollama-loadbalancer/internal/config"
	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// Мок image-воркера
// ============================================================

// imgResWorkerModels — «нормальный» ответ /api/image/models: модель загружена,
// воркер даёт оценку VRAM 8000 MB.
const imgResWorkerModels = `{"models":[{"name":"flux-test","state":"loaded","family":"flux",` +
	`"size_bytes":4000000000,"vram_estimate_mb":8000,"active_queries":0}],` +
	`"state":"loaded","current_model":"flux-test"}`

// imgResStub — мок image-воркера (sdworker).
type imgResStub struct {
	srv *httptest.Server

	mu         sync.Mutex
	modelsBody string
	capBody    string
	genStatus  int
	genHits    int
	modelsHits int
	// armed — «заряженные» блокировки генерации (одна на запрос): blockGeneration.
	armed []chan struct{}
	// created — ВСЕ созданные каналы блокировки, чтобы unblock() гарантированно
	// отпустил и те, что уже взял обработчик (иначе тест повиснет).
	created []chan struct{}

	genStarted chan struct{}
}

func newImgResStub(t *testing.T) *imgResStub {
	t.Helper()
	s := &imgResStub{
		modelsBody: imgResWorkerModels,
		capBody:    `{"vram":{"available":false}}`,
		genStatus:  http.StatusOK,
		genStarted: make(chan struct{}, 8),
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(func() {
		s.unblock()
		s.srv.Close()
	})
	return s
}

func (s *imgResStub) handle(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case imageModelsPath:
		s.mu.Lock()
		s.modelsHits++
		body := s.modelsBody
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	case imageWorkerVRAMPath:
		s.mu.Lock()
		body := s.capBody
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	default:
		// Генерация (или любой другой путь, который проксируется как есть).
		s.mu.Lock()
		s.genHits++
		status := s.genStatus
		rel := s.takeBlockLocked()
		s.mu.Unlock()
		select {
		case s.genStarted <- struct{}{}:
		default:
		}
		if rel != nil {
			select {
			case <-rel:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"created":1,"output_format":"png","data":[{"b64_json":"iVBORw0KGgo="}]}`))
	}
}

// setModels — подменить ответ /api/image/models.
func (s *imgResStub) setModels(body string) {
	s.mu.Lock()
	s.modelsBody = body
	s.mu.Unlock()
}

// setWorkerVRAM — воркер сообщает свободную/полную VRAM (снимок nvidia-smi).
func (s *imgResStub) setWorkerVRAM(freeMB, totalMB int) {
	s.mu.Lock()
	s.capBody = fmt.Sprintf(`{"vram":{"available":true,"used_mb":%d,"total_mb":%d,"source":"test"}}`,
		totalMB-freeMB, totalMB)
	s.mu.Unlock()
}

// blockGeneration — «зависает» ровно n СЛЕДУЮЩИХ запросов генерации (нужно для
// проверки лока: второй запрос обязан проходить, а не вставать в ту же пробку).
func (s *imgResStub) blockGeneration(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := 0; i < n; i++ {
		ch := make(chan struct{})
		s.armed = append(s.armed, ch)
		s.created = append(s.created, ch)
	}
}

// takeBlockLocked — взять очередную блокировку (вызывается под s.mu).
func (s *imgResStub) takeBlockLocked() chan struct{} {
	if len(s.armed) == 0 {
		return nil
	}
	ch := s.armed[0]
	s.armed = s.armed[1:]
	return ch
}

// unblock — отпустить все генерации, включая уже начатые.
func (s *imgResStub) unblock() {
	s.mu.Lock()
	all := s.created
	s.created = nil
	s.armed = nil
	s.mu.Unlock()
	for _, ch := range all {
		select {
		case <-ch:
		default:
			close(ch)
		}
	}
}

func (s *imgResStub) hits() (gen, models int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.genHits, s.modelsHits
}

func (s *imgResStub) port(t *testing.T) int {
	t.Helper()
	return portFromURL(t, s.srv.URL)
}

// ============================================================
// Тестовый стенд
// ============================================================

// imgResStubModelUnknown — модель загружена, но оценки VRAM нет ни от воркера,
// ни по размерам файлов (vram_estimate_mb=0, size_bytes=0) → источник unknown.
const imgResStubModelUnknown = `{"models":[{"name":"no-estimate","state":"loaded","family":"other",` +
	`"size_bytes":0,"vram_estimate_mb":0,"active_queries":0}],"state":"loaded","current_model":"no-estimate"}`

// imgResStubModelNotLoaded — ни одной загруженной модели.
const imgResStubModelNotLoaded = `{"models":[{"name":"not-loaded","state":"not_loaded","family":"sd15"}],` +
	`"state":"not_loaded","current_model":""}`

// imgResStubModelLoading — модель грузится.
const imgResStubModelLoading = `{"models":[{"name":"loading-model","state":"loading","family":"flux"}],` +
	`"state":"loading","current_model":"loading-model"}`

// newImgResProxy — прокси с одним image-бэкендом и одним текстовым на том же
// хосте (сценарий сосуществования). extra добавляются к списку бэкендов (для
// проверок «другой хост»). Фоновый поллер метрик при этом ОСТАНАВЛИВАЕТСЯ —
// см. newImgResProxyOpts.
func newImgResProxy(
	t *testing.T,
	settings types.ImageResourceSettings,
	extra ...types.Backend,
) (*Proxy, *imgResStub) {
	t.Helper()
	return newImgResProxyOpts(t, settings, false, extra...)
}

// newImgResProxyOpts — стенд Phase 6 с управлением фоновым поллером.
// keepPoller=true оставляет поллер живым (проверки FIX-1: первый тик не
// немедленный, ленивая догрузка перед гейтом).
func newImgResProxyOpts(
	t *testing.T,
	settings types.ImageResourceSettings,
	keepPoller bool,
	extra ...types.Backend,
) (*Proxy, *imgResStub) {
	t.Helper()

	stub := newImgResStub(t)
	backends := []types.Backend{
		{
			ID:                "img-1",
			Name:              "image worker",
			Host:              "127.0.0.1",
			ImagePort:         stub.port(t),
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
			MaxConcurrentReqs: 4,
		},
	}
	backends = append(backends, extra...)

	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:       "127.0.0.1",
			Port:       18080,
			APIPort:    18081,
			OpenAIPort: 18079,
			StatePath:  filepath.Join(t.TempDir(), "state.json"),
		},
		Backends: backends,
		Balancing: types.BalancingSettings{
			Algorithm:      types.AlgorithmResourceAware,
			RequestTimeout: 10,
			QueueTimeout:   5,
			QueueMaxSize:   50,
			QueueWorkers:   2,
			OperatingMode:  "standard",
			Image:          settings,
		},
	}

	p := newProxyWithCleanup(t, cfg)
	// Поллер в тестах гейта ОСТАНОВЛЕН: тогда тест не зависит от того, когда
	// фоновый тик успел обновить кэш (гейт при пустом/протухшем кэше делает
	// синхронную догрузку — решения детерминированы). Поведение самого поллера
	// проверяется отдельно: TestImageResources_PollerFirstTickIsDelayed.
	if !keepPoller && p.imageRes != nil {
		p.imageRes.Stop()
	}
	t.Cleanup(func() {
		if p.imageRes != nil {
			p.imageRes.Stop()
		}
	})
	for _, b := range p.GetAllBackends() {
		p.UpdateBackendStatus(b.ID, types.StatusHealthy)
	}
	// Короткое ожидание лока: тест «второй запрос» не должен ждать 30 с.
	p.imageRes.queueWaitOverride = 150 * time.Millisecond
	return p, stub
}

// imgResPost — прогон POST через ImageRouter (без реальной сети: httptest recorder).
func imgResPost(t *testing.T, p *Proxy, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	if !p.imageRouter.Route(rec, req) {
		t.Fatalf("Route(%s) = false, want true (путь обязан обслуживаться image-роутером)", path)
	}
	return rec
}

// imgResGet — прогон GET через ImageRouter (управляющие пути).
func imgResGet(t *testing.T, p *Proxy, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	if !p.imageRouter.Route(rec, req) {
		t.Fatalf("Route(%s) = false, want true", path)
	}
	return rec
}

// imgResErrFields — code/message/hint из ответа об ошибке (оба конверта).
func imgResErrFields(t *testing.T, rec *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var raw map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("ответ не JSON: %v (body=%s)", err, rec.Body.String())
	}
	if nested, ok := raw["error"].(map[string]interface{}); ok {
		return nested
	}
	return map[string]interface{}{
		"code":    raw["error"],
		"message": raw["message"],
		"hint":    raw["hint"],
	}
}

func imgResStr(t *testing.T, fields map[string]interface{}, key string) string {
	t.Helper()
	v, _ := fields[key].(string)
	return v
}

// ============================================================
// 1–2. Влезает / не влезает
// ============================================================

// TestImageGate_FitsAllowsAndProxies — (1) оценка влезает в свободную VRAM:
// запрос проксируется в воркер, лок берётся и снимается.
func TestImageGate_FitsAllowsAndProxies(t *testing.T) {
	p, stub := newImgResProxy(t, types.ImageResourceSettings{})
	stub.setWorkerVRAM(14000, 16000) // свободно 14 GB, нужно 8 GB

	rec := imgResPost(t, p, "/v1/images/generations", `{"model":"dall-e-2","prompt":"cat"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	gen, _ := stub.hits()
	if gen != 1 {
		t.Fatalf("image backend hits = %d, want 1 (запрос обязан дойти до воркера)", gen)
	}
	if p.imageRes.gpuLockHeld("127.0.0.1") {
		t.Fatal("лок GPU не снят после ответа — defer Release не сработал")
	}
	if got := p.imageRes.lockAcquired.Load(); got != 1 {
		t.Fatalf("lock_acquired = %d, want 1", got)
	}
}

// TestImageGate_InsufficientVRAM — (2) не влезает → 503 insufficient_vram + hint.
//
// R-Image (2026-10-02): гейт сравнивает с free РАБОЧИЙ НАБОР (веса модели уже
// резидентны, ведь модель загружена), а не полный вес. Для модели 8000 MB
// рабочий набор = 1024 MB, поэтому «не влезает» — это free ниже 1024.
func TestImageGate_InsufficientVRAM(t *testing.T) {
	p, stub := newImgResProxy(t, types.ImageResourceSettings{})
	stub.setWorkerVRAM(700, 16000) // рабочий набор 1024 MB, свободно 700

	rec := imgResPost(t, p, "/v1/images/generations", `{"model":"dall-e-2","prompt":"cat"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body=%s)", rec.Code, rec.Body.String())
	}
	fields := imgResErrFields(t, rec)
	if code := imgResStr(t, fields, "code"); code != types.ImageGateInsufficientVRAM {
		t.Fatalf("error.code = %q, want %q", code, types.ImageGateInsufficientVRAM)
	}
	hint := imgResStr(t, fields, "hint")
	if hint == "" {
		t.Fatal("hint пуст — OOM-лестница обязана дойти до клиента/оператора")
	}
	if !strings.Contains(hint, "quantization") {
		t.Fatalf("hint не содержит OOM-лестницу (types.ImageOOMHint): %q", hint)
	}
	if gen, _ := stub.hits(); gen != 0 {
		t.Fatalf("запрос дошёл до воркера (%d) при отказе гейта", gen)
	}
	if p.imageRes.gateDeniedByReason(t, types.ImageGateInsufficientVRAM) != 1 {
		t.Fatal("счётчик отказов гейта по reasonCode не вырос")
	}
}

// TestImageGate_InsufficientVRAMFlatEnvelope — тот же отказ на не-/v1 пути
// (sdapi) отдаётся плоским конвертом {error,message,hint}.
func TestImageGate_InsufficientVRAMFlatEnvelope(t *testing.T) {
	p, stub := newImgResProxy(t, types.ImageResourceSettings{})
	stub.setWorkerVRAM(700, 16000) // ниже рабочего набора (1024 MB)

	rec := imgResPost(t, p, "/sdapi/v1/txt2img", `{"prompt":"cat"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("body не JSON: %v", err)
	}
	if _, nested := raw["error"].(map[string]interface{}); nested {
		t.Fatal("для не-/v1 пути ожидался ПЛОСКИЙ конверт {error,message,hint}")
	}
	if raw["error"] != types.ImageGateInsufficientVRAM {
		t.Fatalf("error = %v, want %q", raw["error"], types.ImageGateInsufficientVRAM)
	}
	if raw["hint"] == nil {
		t.Fatal("hint обязателен и в плоском конверте")
	}
}

// ============================================================
// 3. Неизвестная оценка
// ============================================================

// TestImageGate_UnknownEstimateFollowsFrozenContract — (3) при неизвестной
// оценке VRAM решение гейта (503/200 и код) обязано СОВПАДАТЬ с вердиктом
// types.EvaluateImageVRAM при тех же настройках.
//
// ТАК ТЕСТ НАПИСАН СОЗНАТЕЛЬНО: политику «блокировать ли неизвестную оценку»
// задаёт замороженный контракт (владелец pkg/types/image_policy.go переводит её
// в opt-in строгость), и балансер не имеет права трактовать её по-своему. Тест
// не ссылается на имя настройки, поэтому переживает её переименование.
func TestImageGate_UnknownEstimateFollowsFrozenContract(t *testing.T) {
	p, stub := newImgResProxy(t, types.ImageResourceSettings{})
	stub.setModels(imgResStubModelUnknown)
	stub.setWorkerVRAM(4000, 16000)

	// Свободная VRAM здесь ИЗВЕСТНА (4000): если контракт запрещает неизвестную
	// оценку, отказ придёт кодом unknown_vram_estimate (для insufficient_vram
	// числа нет — оценка-то неизвестна).
	want := types.EvaluateImageVRAM(
		types.ImageVramEstimate{Source: "unknown"}, 4000, p.config.Balancing.Image)

	rec := imgResPost(t, p, "/v1/images/generations", `{"model":"dall-e-2","prompt":"cat"}`)
	if want.Allowed {
		if rec.Code != http.StatusOK {
			t.Fatalf("контракт разрешает неизвестную оценку, а гейт ответил %d (body=%s)",
				rec.Code, rec.Body.String())
		}
		if gen, _ := stub.hits(); gen != 1 {
			t.Fatalf("запрос не дошёл до воркера при разрешённом вердикте (hits=%d)", gen)
		}
		return
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body=%s)", rec.Code, rec.Body.String())
	}
	fields := imgResErrFields(t, rec)
	if code := imgResStr(t, fields, "code"); code != want.ReasonCode {
		t.Fatalf("error.code = %q, want %q (вердикт контракта)", code, want.ReasonCode)
	}
	if imgResStr(t, fields, "hint") == "" {
		t.Fatal("hint обязателен: оператор должен узнать, что задать vramEstimateMb")
	}
}

// TestImageGate_UnknownEstimateWithoutTextNeighbour — на хосте без текстовых
// соседей неизвестная оценка подчиняется тому же контракту (по умолчанию —
// пропускается, FIX-2), а не отдельной политике балансера.
func TestImageGate_UnknownEstimateWithoutTextNeighbour(t *testing.T) {
	p, stub := newImgResProxy(t, types.ImageResourceSettings{})
	stub.setModels(imgResStubModelUnknown)
	// Уводим ТЕКСТОВЫЙ бэкенд на другой хост: соседей по GPU у image-воркера нет.
	// (Двигаем текстовый, а не image: адрес воркера тест реально опрашивает.)
	p.mu.Lock()
	p.backends["llm-1"].Backend.Host = "10.8.8.8"
	p.mu.Unlock()

	rec := imgResPost(t, p, "/v1/images/generations", `{"model":"dall-e-2","prompt":"cat"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 на хосте без текстовых соседей (body=%s)", rec.Code, rec.Body.String())
	}
}

// ============================================================
// 4–5. gateDisabled и headroom
// ============================================================

// TestImageGate_DisabledSkipsGate — (4) gateDisabled → пропуск, даже когда
// оценка очевидно не влезает.
func TestImageGate_DisabledSkipsGate(t *testing.T) {
	p, stub := newImgResProxy(t, types.ImageResourceSettings{GateDisabled: true})
	stub.setWorkerVRAM(1000, 16000) // нужно 8000 — не влезает

	rec := imgResPost(t, p, "/v1/images/generations", `{"model":"dall-e-2","prompt":"cat"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 при gateDisabled (body=%s)", rec.Code, rec.Body.String())
	}
	if p.imageRes.lockAcquired.Load() != 0 {
		t.Fatal("gateDisabled обязан отключить и лок сосуществования")
	}
}

// TestImageGate_HeadroomConsidered — (5) headroom входит в бюджет.
func TestImageGate_HeadroomConsidered(t *testing.T) {
	// Ровно влезает без запаса: рабочий набор 1024 MB, свободно 1500 MB.
	p, stub := newImgResProxy(t, types.ImageResourceSettings{VramHeadroomMB: 0})
	stub.setWorkerVRAM(1500, 16000)
	if rec := imgResPost(t, p, "/v1/images/generations", `{"prompt":"cat"}`); rec.Code != http.StatusOK {
		t.Fatalf("без headroom: status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	// Тот же расклад, но с запасом 1000 MB (1024+1000=2024 > 1500) → уже не влезает.
	p2, stub2 := newImgResProxy(t, types.ImageResourceSettings{VramHeadroomMB: 1000})
	stub2.setWorkerVRAM(1500, 16000)
	rec := imgResPost(t, p2, "/v1/images/generations", `{"prompt":"cat"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("с headroom: status = %d, want 503 (body=%s)", rec.Code, rec.Body.String())
	}
	if code := imgResStr(t, imgResErrFields(t, rec), "code"); code != types.ImageGateInsufficientVRAM {
		t.Fatalf("error.code = %q, want %q", code, types.ImageGateInsufficientVRAM)
	}
}

// TestImageGate_LoadedModelChecksWorkingSetNotFullWeights — R-Image (2026-10-02):
// дефект, найденный ЖИВЫМ прогоном. Модель уже загружена в движок (веса
// резидентны), free VRAM меньше полного веса — прежний гейт отказывал 503 на
// КАЖДЫЙ запрос, хотя движок готов генерировать. Теперь при загруженной модели
// проверяется рабочий набор (20% веса, 256…1024 MB).
func TestImageGate_LoadedModelChecksWorkingSetNotFullWeights(t *testing.T) {
	// Вес 8000 MB, свободно 2000 MB: полный вес не влезает, рабочий набор (1024) — влезает.
	p, stub := newImgResProxy(t, types.ImageResourceSettings{})
	stub.setWorkerVRAM(2000, 16000)

	rec := imgResPost(t, p, "/v1/images/generations", `{"prompt":"cat"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: при загруженной модели полный вес повторно не требуется (body=%s)",
			rec.Code, rec.Body.String())
	}
	if gen, _ := stub.hits(); gen == 0 {
		t.Fatal("запрос не дошёл до воркера, хотя гейт должен был пропустить")
	}
}

// ============================================================
// 6–8. Лок GPU: exclusive / offload / dedicated / предохранитель
// ============================================================

// TestImageLock_ExclusiveSecondRequestRejected — (6) второй одновременный
// запрос ждёт queueWait, затем получает 429 с Retry-After.
func TestImageLock_ExclusiveSecondRequestRejected(t *testing.T) {
	p, stub := newImgResProxy(t, types.ImageResourceSettings{}) // default = exclusive
	stub.setWorkerVRAM(14000, 16000)
	stub.blockGeneration(1)

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		firstDone <- imgResPost(t, p, "/v1/images/generations", `{"prompt":"cat"}`)
	}()

	// Ждём, пока генерация реально началась (значит, лок точно взят).
	select {
	case <-stub.genStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("первый запрос не дошёл до воркера")
	}
	if !p.imageRes.gpuLockHeld("127.0.0.1") {
		t.Fatal("лок GPU не взят на время генерации (exclusive)")
	}

	start := time.Now()
	second := imgResPost(t, p, "/v1/images/generations", `{"prompt":"dog"}`)
	waited := time.Since(start)

	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("второй запрос: status = %d, want 429 (body=%s)", second.Code, second.Body.String())
	}
	if waited < 100*time.Millisecond {
		t.Fatalf("второй запрос не ждал освобождения лока (%.0f ms)", waited.Seconds()*1000)
	}
	if ra := second.Header().Get("Retry-After"); ra == "" {
		t.Fatal("429 без Retry-After — клиент не поймёт, когда повторять")
	}
	if code := imgResStr(t, imgResErrFields(t, second), "code"); code != ImageGateGPUBusy {
		t.Fatalf("error.code = %q, want %q", code, ImageGateGPUBusy)
	}

	stub.unblock()
	select {
	case rec := <-firstDone:
		if rec.Code != http.StatusOK {
			t.Fatalf("первый запрос: status = %d, want 200", rec.Code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("первый запрос не завершился после unblock")
	}
	if p.imageRes.gpuLockHeld("127.0.0.1") {
		t.Fatal("лок не освобождён после завершения генерации")
	}
}

// TestImageLock_OffloadAndDedicatedTakeNoLock — (7) в offload/dedicated лок не
// берётся (совместная работа с текстом разрешена).
func TestImageLock_OffloadAndDedicatedTakeNoLock(t *testing.T) {
	for _, policy := range []types.ImageCoexistencePolicy{
		types.ImageCoexistenceOffload,
		types.ImageCoexistenceDedicated,
	} {
		t.Run(string(policy), func(t *testing.T) {
			p, stub := newImgResProxy(t, types.ImageResourceSettings{Coexistence: policy})
			stub.setWorkerVRAM(14000, 16000)
			stub.blockGeneration(1)

			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- imgResPost(t, p, "/v1/images/generations", `{"prompt":"cat"}`) }()

			select {
			case <-stub.genStarted:
			case <-time.After(3 * time.Second):
				t.Fatal("запрос не дошёл до воркера")
			}
			if p.imageRes.gpuLockHeld("127.0.0.1") {
				t.Fatalf("политика %q: лок GPU взят, хотя совместная работа разрешена", policy)
			}
			if got := p.imageRes.lockAcquired.Load(); got != 0 {
				t.Fatalf("политика %q: lock_acquired = %d, want 0", policy, got)
			}

			stub.unblock()
			select {
			case rec := <-done:
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200", rec.Code)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("запрос не завершился")
			}
		})
	}
}

// TestImageLock_FuseReleasesStuckGeneration — (8) предохранитель освобождает
// лок зависшей генерации (иначе текстовый трафик встал бы навсегда).
func TestImageLock_FuseReleasesStuckGeneration(t *testing.T) {
	p, stub := newImgResProxy(t, types.ImageResourceSettings{})
	stub.setWorkerVRAM(14000, 16000)
	p.imageRes.lockFuseOverride = 120 * time.Millisecond
	stub.blockGeneration(1)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- imgResPost(t, p, "/v1/images/generations", `{"prompt":"cat"}`) }()

	select {
	case <-stub.genStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("запрос не дошёл до воркера")
	}
	if !p.imageRes.gpuLockHeld("127.0.0.1") {
		t.Fatal("лок не взят")
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !p.imageRes.gpuLockHeld("127.0.0.1") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if p.imageRes.gpuLockHeld("127.0.0.1") {
		t.Fatal("предохранитель не освободил лок зависшей генерации")
	}
	if got := p.imageRes.lockForced.Load(); got != 1 {
		t.Fatalf("lock_force_released = %d, want 1", got)
	}

	// Второй запрос проходит, хотя первая генерация всё ещё «висит».
	second := imgResPost(t, p, "/v1/images/generations", `{"prompt":"dog"}`)
	if second.Code != http.StatusOK {
		t.Fatalf("после предохранителя: status = %d, want 200 (body=%s)", second.Code, second.Body.String())
	}

	stub.unblock()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("первый запрос не завершился")
	}
}

// TestImageLock_NotTakenForControlEndpoints — управляющие пути (/api/image/*,
// /sdapi/v1/interrupt) лок не берут и гейтом не блокируются: иначе оператор не
// смог бы посмотреть состояние или отменить генерацию.
func TestImageLock_NotTakenForControlEndpoints(t *testing.T) {
	p, stub := newImgResProxy(t, types.ImageResourceSettings{})
	stub.setModels(imgResStubModelNotLoaded) // модель не загружена → гейт злой

	if rec := imgResGet(t, p, "/api/image/models"); rec.Code != http.StatusOK {
		t.Fatalf("GET /api/image/models: status = %d, want 200 (управление не гейтится)", rec.Code)
	}
	if rec := imgResPost(t, p, "/sdapi/v1/interrupt", `{}`); rec.Code != http.StatusOK {
		t.Fatalf("POST /sdapi/v1/interrupt: status = %d, want 200", rec.Code)
	}
	if p.imageRes.lockAcquired.Load() != 0 {
		t.Fatal("управляющий запрос взял лок GPU")
	}
}

// ============================================================
// Состояние модели
// ============================================================

// TestImageGate_ModelNotLoaded — нет загруженной модели → честный 503 с
// подсказкой (а не «пропустим наугад»).
func TestImageGate_ModelNotLoaded(t *testing.T) {
	p, stub := newImgResProxy(t, types.ImageResourceSettings{})
	stub.setModels(imgResStubModelNotLoaded)

	rec := imgResPost(t, p, "/v1/images/generations", `{"prompt":"cat"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body=%s)", rec.Code, rec.Body.String())
	}
	fields := imgResErrFields(t, rec)
	if code := imgResStr(t, fields, "code"); code != ImageGateModelNotLoaded {
		t.Fatalf("error.code = %q, want %q", code, ImageGateModelNotLoaded)
	}
	if hint := imgResStr(t, fields, "hint"); !strings.Contains(hint, "load") {
		t.Fatalf("hint не подсказывает загрузить модель: %q", hint)
	}
	if gen, _ := stub.hits(); gen != 0 {
		t.Fatal("запрос ушёл в воркер, хотя модель не загружена")
	}
}

// TestImageGate_ModelLoading — модель грузится → 503 image_model_loading + Retry-After.
func TestImageGate_ModelLoading(t *testing.T) {
	p, stub := newImgResProxy(t, types.ImageResourceSettings{})
	stub.setModels(imgResStubModelLoading)

	rec := imgResPost(t, p, "/v1/images/generations", `{"prompt":"cat"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if code := imgResStr(t, imgResErrFields(t, rec), "code"); code != ImageGateModelLoading {
		t.Fatalf("error.code = %q, want %q", code, ImageGateModelLoading)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("для «модель грузится» нужен Retry-After")
	}
}

// TestImageGate_ContractUnsupportedFailsOpen — воркер не знает контракта
// /api/image/models (нет ключа models): гейт молчит, запрос проксируется.
// Это защищает рабочие конфигурации: «голый» sd-server, 401/403, битый JSON.
func TestImageGate_ContractUnsupportedFailsOpen(t *testing.T) {
	p, stub := newImgResProxy(t, types.ImageResourceSettings{})
	stub.setModels(`{"created":1,"output_format":"png","data":[{"b64_json":"x"}]}`)

	rec := imgResPost(t, p, "/v1/images/generations", `{"prompt":"cat"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fail-open без контракта; body=%s)", rec.Code, rec.Body.String())
	}
}

// TestImageGate_PollerPopulatesCache — поллер собирает метрики image-бэкендов
// (источник данных для гейта и для /api/metrics).
func TestImageGate_PollerPopulatesCache(t *testing.T) {
	p, stub := newImgResProxy(t, types.ImageResourceSettings{})
	p.imageRes.pollAll(context.Background())
	snap := p.imageRes.metricsSnapshot("img-1")
	if snap == nil {
		t.Fatal("поллер не создал снимок метрик image-бэкенда")
	}
	if !snap.contractOK || snap.loaded() == nil || snap.loaded().Name != "flux-test" {
		t.Fatalf("снимок не распознал загруженную модель: %+v", snap)
	}
	if _, models := stub.hits(); models == 0 {
		t.Fatal("поллер не опросил /api/image/models")
	}
}

// TestImageResources_PollerFirstTickIsDelayed — FIX-1: Start() НЕ опрашивает
// воркер немедленно (иначе фоновый poll попадал в тесты и скрипты, считающие
// обращения к /api/image/models: internal/api TestImageBackendProxy_ModelsAlias
// ожидает РОВНО одно обращение), а свежесть метрик к моменту запроса
// обеспечивает ленивая синхронная догрузка ensureFreshFor.
func TestImageResources_PollerFirstTickIsDelayed(t *testing.T) {
	p, stub := newImgResProxyOpts(t, types.ImageResourceSettings{}, true)

	if _, models := stub.hits(); models != 0 {
		t.Fatalf("поллер опросил воркер сразу после старта (models_hits=%d) — первый тик обязан быть отложенным", models)
	}
	time.Sleep(300 * time.Millisecond)
	if _, models := stub.hits(); models != 0 {
		t.Fatalf("поллер опросил воркер до первого интервала (models_hits=%d)", models)
	}

	// Гейт всё равно принимает решение по свежим данным.
	stub.setWorkerVRAM(14000, 16000)
	rec := imgResPost(t, p, "/v1/images/generations", `{"prompt":"cat"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (ленивая догрузка метрик не сработала; body=%s)",
			rec.Code, rec.Body.String())
	}
	if _, models := stub.hits(); models == 0 {
		t.Fatal("ensureFreshFor не опросил воркер перед решением гейта")
	}
}

// TestImageResources_NoImageBackendsNoNetwork — в кластере без image-бэкендов
// поллер не делает НИ ОДНОГО сетевого запроса.
func TestImageResources_NoImageBackendsNoNetwork(t *testing.T) {
	p, stub := newImgResProxyOpts(t, types.ImageResourceSettings{}, true)
	// Оставляем только текстовый бэкенд, причём его порт указывает на мок:
	// если бы поллер (ошибочно) опрашивал не-image бэкенды, запрос был бы виден.
	p.mu.Lock()
	delete(p.backends, "img-1")
	p.backends["llm-1"].Backend.CppWorkerPort = stub.port(t)
	p.mu.Unlock()

	p.imageRes.pollAll(context.Background())
	if _, models := stub.hits(); models != 0 {
		t.Fatalf("pollAll без image-бэкендов сделал %d сетевых запросов", models)
	}
}

// ============================================================
// Свободная VRAM от текстового соседа (реальный «продакшн» путь)
// ============================================================

// TestImageGate_FreeVRAMFromTextNeighbour — воркер VRAM не отдаёт, но у
// текстового бэкенда того же хоста есть available_vram_mb (живой снимок
// cppworker). Гейт обязан им воспользоваться.
func TestImageGate_FreeVRAMFromTextNeighbour(t *testing.T) {
	p, _ := newImgResProxy(t, types.ImageResourceSettings{})
	p.metricsMgr.mu.Lock()
	p.metricsMgr.llamaMetrics["llm-1"] = &types.LlamaCppMetrics{AvailableVRAMMB: 400}
	p.metricsMgr.mu.Unlock()

	rec := imgResPost(t, p, "/v1/images/generations", `{"prompt":"cat"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (рабочий набор 1024 MB, у соседа свободно 400 MB; body=%s)",
			rec.Code, rec.Body.String())
	}
	if code := imgResStr(t, imgResErrFields(t, rec), "code"); code != types.ImageGateInsufficientVRAM {
		t.Fatalf("error.code = %q, want %q", code, types.ImageGateInsufficientVRAM)
	}

	// Устаревший снимок соседа (старше maxTextVramSnapshotAge) не учитываем —
	// иначе гейт решал бы по данным, которым уже нельзя верить.
	p.metricsMgr.mu.Lock()
	p.metricsMgr.metrics["llm-1"] = &types.BackendMetrics{
		ID:        "llm-1",
		Timestamp: time.Now().Add(-3 * maxTextVramSnapshotAge),
	}
	p.metricsMgr.mu.Unlock()

	rec2 := imgResPost(t, p, "/v1/images/generations", `{"prompt":"cat"}`)
	if rec2.Code != http.StatusOK {
		t.Fatalf("устаревший снимок VRAM соседа не должен блокировать: status = %d, want 200", rec2.Code)
	}
}

// ============================================================
// Текстовая сторона exclusive: кандидат на занятом хосте не выбирается
// ============================================================

// TestImageLock_TextSlotWithheldWhileGPUBusy — минимальный хук в селекторе:
// пока image-генерация держит лок, текстовый бэкенд того же хоста не выбирается;
// на другом хосте — выбирается; после освобождения лока — снова выбирается.
func TestImageLock_TextSlotWithheldWhileGPUBusy(t *testing.T) {
	other := types.Backend{
		ID:                "llm-2",
		Name:              "text on another host",
		Host:              "10.8.8.8",
		CppWorkerPort:     18092,
		Type:              types.BackendTypeLlamaCpp,
		Status:            types.StatusHealthy,
		MaxConcurrentReqs: 4,
	}
	p, _ := newImgResProxy(t, types.ImageResourceSettings{}, other)
	textOnly := []types.BackendType{types.BackendTypeLlamaCpp}

	if got := p.selectByResources(textOnly); got != "llm-1" && got != "llm-2" {
		t.Fatalf("до генерации: selectByResources = %q, want текстовый бэкенд", got)
	}

	holder, ok, _ := p.imageRes.acquireLock(context.Background(), "img-1", "127.0.0.1", 0, "flux-test")
	if !ok || holder == nil {
		t.Fatal("не удалось взять лок (тестовый стенд сломан)")
	}

	// Текстовый кандидат на 127.0.0.1 отсеян: остаётся только соседний хост.
	got := p.selectByResources(textOnly)
	if got == "llm-1" {
		t.Fatal("текстовый бэкенд выбран на хосте с активным image-локом (exclusive нарушен)")
	}
	if got != "llm-2" {
		t.Fatalf("selectByResources = %q, want llm-2 (другой хост)", got)
	}
	if p.selectFreeBackendAny(textOnly) == "llm-1" {
		t.Fatal("selectFreeBackendAny выбрал занятый хост")
	}
	// image-бэкенд хук не трогает.
	if p.imageGateBlocksTextBackend(p.GetBackendState("img-1")) {
		t.Fatal("хук заблокировал image-бэкенд — exclusive сломал бы сам image-трафик")
	}
	if !p.imageGateBlocksTextBackend(p.GetBackendState("llm-1")) {
		t.Fatal("хук не заблокировал текстовый бэкенд на занятом хосте")
	}

	holder.Release("test_done")

	// После освобождения лока оба хоста снова свободны, поэтому конкретный
	// победитель зависит от скоринга/порядка обхода карты (флейк: раньше здесь
	// требовался ровно llm-1 и тест падал ~1 раз из 3). Проверяем СЕМАНТИКУ:
	// лок снят, и хост 127.0.0.1 снова доступен для текстового трафика.
	if got := p.selectByResources(textOnly); got != "llm-1" && got != "llm-2" {
		t.Fatalf("после освобождения лока: selectByResources = %q, want текстовый бэкенд", got)
	}
	if p.imageGateBlocksTextBackend(p.GetBackendState("llm-1")) {
		t.Fatal("после освобождения лока кандидат всё ещё блокируется")
	}
}

// ============================================================
// Резолв оценки VRAM (источники)
// ============================================================

// TestImageVRAMEstimateSources — приоритет источников: профиль → воркер →
// размеры файлов → unknown.
func TestImageVRAMEstimateSources(t *testing.T) {
	profilePath := filepath.Join(t.TempDir(), "image-model-profiles.json")
	profileJSON := `{"version":1,"profiles":{"flux-test":{` +
		`"name":"flux-test","family":"flux",` +
		`"files":[{"role":"diffusion","repo":"mock/repo","filename":"flux.gguf"}],` +
		`"defaults":{"steps":4,"cfgScale":1,"sampler":"euler","scheduler":"discrete","width":1024,"height":1024,"batchCount":1,"seed":-1},` +
		`"vramEstimateMb":12345}}}`
	if err := writeFileForTest(profilePath, profileJSON); err != nil {
		t.Fatalf("запись профилей: %v", err)
	}
	t.Setenv(config.EnvImageModelProfilesPath, profilePath)

	p, _ := newImgResProxy(t, types.ImageResourceSettings{})
	res := p.imageRes

	// 1) Профиль балансера приоритетнее числа воркера.
	worker := &imageBackendMetrics{
		backendID:  "img-1",
		contractOK: true,
		models: []imageModelEntry{{
			Name: "flux-test", State: imageStateLoaded, VramEstimateMB: 8000, SizeBytes: 4 << 30,
		}},
	}
	est := res.resolveVRAMEstimate(worker)
	if est.Source != "profile" || est.RequiredMB != 12345 {
		t.Fatalf("профиль должен иметь приоритет: %+v", est)
	}

	// 2) Без профиля — число воркера.
	other := &imageBackendMetrics{
		backendID:  "img-1",
		contractOK: true,
		models: []imageModelEntry{{
			Name: "no-profile-model", State: imageStateLoaded, VramEstimateMB: 6395, SizeBytes: 6 << 30,
		}},
	}
	est = res.resolveVRAMEstimate(other)
	if est.Source != "worker" || est.RequiredMB != 6395 {
		t.Fatalf("источник worker: %+v", est)
	}

	// 3) Только размер файлов × коэффициент.
	filesSize := int64(1) << 30
	filesOnly := &imageBackendMetrics{
		backendID:  "img-1",
		contractOK: true,
		models: []imageModelEntry{{
			Name: "no-profile-model", State: imageStateLoaded, SizeBytes: filesSize,
		}},
	}
	est = res.resolveVRAMEstimate(filesOnly)
	if est.Source != "files" {
		t.Fatalf("источник files: %+v", est)
	}
	wantMB := int(float64(filesSize) / float64(1024*1024) * imageVramFileCoefficient)
	if est.RequiredMB != wantMB {
		t.Fatalf("files-оценка = %d, want %d", est.RequiredMB, wantMB)
	}

	// 4) Ничего нет → unknown (RequiredMB=0).
	empty := &imageBackendMetrics{
		backendID:  "img-1",
		contractOK: true,
		models:     []imageModelEntry{{Name: "no-profile-model", State: imageStateLoaded}},
	}
	est = res.resolveVRAMEstimate(empty)
	if est.Source != "unknown" || est.IsKnown() {
		t.Fatalf("ожидался unknown: %+v", est)
	}
}

// TestImageResources_MetricsSnapshot — счётчики Phase 6 попадают в /api/metrics.
func TestImageResources_MetricsSnapshot(t *testing.T) {
	p, stub := newImgResProxy(t, types.ImageResourceSettings{})
	stub.setWorkerVRAM(700, 16000) // ниже рабочего набора (1024 MB) → отказ гейта
	if rec := imgResPost(t, p, "/v1/images/generations", `{"prompt":"cat"}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	m := p.imageRes.snapshotMetrics()
	if m["gate_denied_total"].(int64) != 1 {
		t.Fatalf("gate_denied_total = %v, want 1", m["gate_denied_total"])
	}
	byReason, ok := m["gate_denied_by_reason"].(map[string]int64)
	if !ok || byReason[types.ImageGateInsufficientVRAM] != 1 {
		t.Fatalf("gate_denied_by_reason = %v, want %s=1", m["gate_denied_by_reason"], types.ImageGateInsufficientVRAM)
	}
	if _, ok := m["backends"].(map[string]interface{}); !ok {
		t.Fatalf("backends отсутствует в снапшоте: %v", m["backends"])
	}
	if m["coexistence_policy"] != string(types.ImageCoexistenceExclusive) {
		t.Fatalf("coexistence_policy = %v, want exclusive по умолчанию", m["coexistence_policy"])
	}
}

// gateDeniedByReason — счётчик отказов по коду причины (для тестов).
func (r *imageResources) gateDeniedByReason(t *testing.T, reason string) int64 {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gateDenied[reason]
}

// writeFileForTest — запись файла-фикстуры (профили image-моделей).
func writeFileForTest(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
