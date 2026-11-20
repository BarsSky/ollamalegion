// cold_load_placement_r659_test.go — R659 (2026-11-20): загрузка вместо
// пассивного ожидания.
//
// ЖИВОЙ ДЕФЕКТ, который фиксируют тесты (архив второго стенда 2026-11-20):
// 27B загружена на занятом мастере (A10, окно 201216), рядом два свободных узла
// (A10-126 с 20.5 ГБ и RTX 4000), второй запрос встал в очередь с маркером
// `any` на 300 с и был отменён. Попыток загрузки на свободных узлах — ноль.
//
// Что закрепляется:
//  1. узел под загрузку выбирается по «влезает ли ОКНО» (веса + KV + резерв), а
//     не «первый свободный»: 8-гигабайтная карта не получает 27B;
//  2. окно, которое не влезает, — не повод начинать загрузку (иначе это была бы
//     загрузка «на авось» и падение на первом decode);
//  3. загрузка уходит воркеру как contextPerSeq (окно НА КЛИЕНТА), а не только
//     contextSize (суммарное): при parallel > 1 второе даёт слоту половину;
//  4. одна загрузка на узел — не по одной на каждую ждущую заявку;
//  5. выключатель автозагрузки не обходится;
//  6. в why_queued появляется НАСТОЯЩАЯ причина («загрузка начата» /
//     «окно не влезает») вместо model_not_loaded;
//  7. заявка, стоящая в единой очереди, запускает загрузку, а не ждёт молча.
package balancer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

const r659Model = "qwen3.8-27b-test"

// r659GiB — гибибайт в байтах (размер весов модели).
const r659GiB = uint64(1024 * 1024 * 1024)

// r659Worker — фальшивый cppworker: health, каталог файлов и /load.
//
// Нужен именно живой HTTP: загрузку запускает warmupModel (health → файлы →
// POST /load с admin-токеном), и проверять надо ТЕЛО запроса, а не намерение.
type r659Worker struct {
	srv     *httptest.Server
	mu      sync.Mutex
	loads   []map[string]interface{}
	filesOK bool
	// R663: живая проверка кандидата идёт в /api/models — фальшивый воркер
	// отвечает так, как это делает настоящий, чтобы можно было проверить отказы.
	modelState     string // "" / "loaded" / "loading"
	canLoadOwn     *bool  // nil = поле отсутствует (старый воркер)
	apiModelsCode  int    // 0 = 200, иначе заданный код
	modelsGETCount int
}

func newR659Worker(t *testing.T) *r659Worker {
	t.Helper()
	w := &r659Worker{filesOK: true}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/models", func(rw http.ResponseWriter, r *http.Request) {
		// R663: живую пробу кандидата отличаем по метке (X-R659-Probe). Опрос
		// поллера метрик в счётчик проб не входит — у него свой интервал 30 с.
		isProbe := r.Header.Get("X-R659-Probe") != ""
		w.mu.Lock()
		if isProbe {
			w.modelsGETCount++
		}
		code := w.apiModelsCode
		state := w.modelState
		canLoadOwn := w.canLoadOwn
		w.mu.Unlock()
		if code != 0 {
			rw.WriteHeader(code)
			return
		}
		models := `[]`
		if state != "" {
			models = `[{"name":"` + r659Model + `","state":"` + state + `"}]`
		}
		own := ""
		if canLoadOwn != nil {
			own = fmt.Sprintf(`"can_load_own_models":%v,"role":"donor","donor":%v,"donor_reason":"rpc_server_enabled",`, *canLoadOwn, !*canLoadOwn)
		}
		rw.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(rw, `{`+own+`"count":`+fmt.Sprint(len(models))+`,"models":`+models+`}`)
	})
	mux.HandleFunc("/api/models/files", func(rw http.ResponseWriter, r *http.Request) {
		if !w.filesOK {
			rw.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(rw, `{"files":[{"name":"Qwen3.8-27B-test.gguf","path":"/models/qwen3.8-27b-test.gguf"}]}`)
	})
	mux.HandleFunc("/load", func(rw http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := map[string]interface{}{}
		_ = json.Unmarshal(raw, &body)
		w.mu.Lock()
		w.loads = append(w.loads, body)
		w.mu.Unlock()
		rw.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(rw, `{"status":"ok"}`)
	})
	w.srv = httptest.NewServer(mux)
	t.Cleanup(w.srv.Close)
	return w
}

// setModelsResponse — переключить ответ /api/models (R663).
func (w *r659Worker) setModelsResponse(state string, canLoadOwn *bool, code int) {
	w.mu.Lock()
	w.modelState = state
	w.canLoadOwn = canLoadOwn
	w.apiModelsCode = code
	w.mu.Unlock()
}

// modelsProbes — сколько раз балансер спрашивал /api/models (кэш проб).
func (w *r659Worker) modelsProbes() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.modelsGETCount
}

func (w *r659Worker) loadRequests() []map[string]interface{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]map[string]interface{}, len(w.loads))
	copy(out, w.loads)
	return out
}

// waitLoads — дождаться асинхронного POST /load (warmupModel работает в горутине).
func (w *r659Worker) waitLoads(t *testing.T, want int) []map[string]interface{} {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := w.loadRequests(); len(got) >= want {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	return w.loadRequests()
}

// r659NoLoad — «загрузки быть не должно»: короткое ожидание и проверка.
// Полные 3 с ждать бессмысленно — отрицание не станет положительным позже.
func r659NoLoad(t *testing.T, w *r659Worker) []map[string]interface{} {
	t.Helper()
	time.Sleep(150 * time.Millisecond)
	return w.loadRequests()
}

// r659LoadDeadline — сколько загрузок пришло за короткое окно (для «ровно одна»).
func r659LoadDeadline(t *testing.T, w *r659Worker, want int) []map[string]interface{} {
	t.Helper()
	deadline := time.Now().Add(700 * time.Millisecond)
	for time.Now().Before(deadline) {
		if got := w.loadRequests(); len(got) >= want {
			time.Sleep(100 * time.Millisecond) // дать шанс лишней (дефектной) загрузке
			return w.loadRequests()
		}
		time.Sleep(10 * time.Millisecond)
	}
	return w.loadRequests()
}

// r659Node — описание узла для теста.
type r659Node struct {
	id      string
	port    int
	slots   int
	active  int
	totalMB uint64
	freeMB  uint64
	loaded  []types.LlamaCppModel
}

// portOfR659 — порт фальшивого воркера из его URL (узлы указывают на него).
func portOfR659(t *testing.T, w *r659Worker) int {
	t.Helper()
	if w == nil || w.srv == nil {
		t.Fatal("фальшивый воркер не поднят")
	}
	u := w.srv.URL
	i := strings.LastIndex(u, ":")
	if i < 0 {
		t.Fatalf("не разобрать порт из %q", u)
	}
	port := 0
	for _, c := range u[i+1:] {
		if c < '0' || c > '9' {
			break
		}
		port = port*10 + int(c-'0')
	}
	return port
}

// r659Proxy — боевой прокси (NewProxy: очередь, сессии, health) с заданными
// узлами и метриками.
//
// Мастер: модель загружена, слот занят (ActiveReqs = MaxConcurrentReqs = 1) —
// ровно то состояние, которое в архиве дало очередь на 300 с.
// Геометрия модели (nLayers/nKvHeads/headDim/size) есть ТОЛЬКО у загруженной
// копии: файл модели один и тот же, поэтому свободному узлу для проверки окна
// нужна именно она.
func r659Proxy(t *testing.T, nodes ...r659Node) (*Proxy, *r659Worker) {
	t.Helper()
	worker := newR659Worker(t)
	backends := make([]types.Backend, 0, len(nodes))
	for _, n := range nodes {
		port := n.port
		if port <= 0 {
			port = portOfR659(t, worker)
		}
		backends = append(backends, types.Backend{
			ID: n.id, Name: n.id, Host: "127.0.0.1",
			OllamaPort: port, CppWorkerPort: port,
			Type: types.BackendTypeLlamaCpp, Engine: types.EngineLlamaCPP,
			Status: types.StatusHealthy, MaxConcurrentReqs: n.slots, Weight: 1,
			CppWorkerApiToken: "test-token",
		})
	}
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host: "localhost", Port: 8080, APIPort: 8081,
			StatePath: filepath.Join(t.TempDir(), "state.json"),
		},
		Backends: backends,
		Balancing: types.BalancingSettings{
			QueueMaxSize:        100,
			HealthCheckInterval: 30,
			MetricsInterval:     30,
			SessionStickiness:   true,
		},
	}
	p := newProxyWithCleanup(t, cfg)
	for _, n := range nodes {
		lm := &types.LlamaCppMetrics{
			VramKnown:       true,
			AvailableVRAMMB: n.freeMB,
			TotalVRAMMB:     n.totalMB,
			LoadedModels:    n.loaded,
		}
		p.metricsMgr.mu.Lock()
		p.metricsMgr.metrics[n.id] = &types.BackendMetrics{ID: n.id, LlamaCpp: *lm}
		p.metricsMgr.llamaMetrics[n.id] = lm
		p.metricsMgr.mu.Unlock()
		// «Узел опрошен» — иначе loadedPerSeqNCtxKnownR480 честно скажет
		// «неизвестно» (метрика ни разу не приходила), и загрузку начинать нельзя.
		p.metricsMgr.MarkLlamaCppPolled(n.id, time.Now())
		p.backends[n.id].ActiveReqs = n.active
	}
	return p, worker
}

// r659LoadedModel — запись о загруженной копии С геометрией (как её отдаёт
// воркер в /api/models): без неё KV на нужное окно не посчитать.
func r659LoadedModel(sizeBytes uint64, nLayers, nKvHeads, headDim, perSeq int) types.LlamaCppModel {
	return types.LlamaCppModel{
		Name:          r659Model,
		State:         "loaded",
		Size:          sizeBytes,
		NLayers:       nLayers,
		NKvHeads:      nKvHeads,
		HeadDimK:      headDim,
		Architecture:  "qwen3",
		KvCacheType:   "f16",
		ContextPerSeq: perSeq,
		ContextLength: perSeq,
		MaxSlots:      1,
		Parallel:      1,
	}
}

// r659MasterAndFree — мастер с занятым слотом + свободный A10 (20.5 ГБ свободных).
//
// Геометрия подобрана так, чтобы KV считался честно и граница «влезает /
// не влезает» была различима: 8 слоёв × 2 KV-головы × head_dim 64 при f16 —
// это 4 КБ на токен, то есть 512 МБ на окно 131072 и 4 ГиБ на окно 1048576.
func r659MasterAndFree(t *testing.T, sizeBytes uint64) (*Proxy, *r659Worker) {
	t.Helper()
	return r659Proxy(t,
		r659Node{
			id: "master", port: 1, slots: 1, active: 1,
			totalMB: 23028, freeMB: 2471,
			loaded: []types.LlamaCppModel{r659LoadedModel(sizeBytes, 8, 2, 64, 131072)},
		},
		r659Node{
			id: "a10", port: -1, slots: 1, active: 0,
			totalMB: 23028, freeMB: 20490,
		},
	)
}

// TestR659_PlanChoosesFreeNodeByWindowFit — свободный узел выбран, окно влезает.
func TestR659_PlanChoosesFreeNodeByWindowFit(t *testing.T) {
	p, _ := r659MasterAndFree(t, 8*r659GiB) // веса 8 ГиБ

	plan, ok := p.planColdLoadR659(r659Model, types.BackendTypeLlamaCpp, 131072)
	if !ok {
		t.Fatal("свободный узел с 20.5 ГиБ и окном 131072 обязан быть выбран под загрузку")
	}
	if plan.BackendID != "a10" {
		t.Fatalf("выбран %q, ожидался свободный узел a10", plan.BackendID)
	}
	if plan.Window != 131072 {
		t.Fatalf("окно загрузки = %d, ожидалось 131072 (нужное окно заявки)", plan.Window)
	}
	if !plan.Fit.Verified || !plan.Fit.Fits {
		t.Fatalf("вердикт по окну обязан быть проверенным и положительным: %+v", plan.Fit)
	}
	// Числа вердикта обязаны сходиться: веса + KV + резерв = need.
	if got, want := plan.Fit.NeedMB, plan.Fit.WeightsMB+plan.Fit.KVMB+plan.Fit.ReserveMB; got != want {
		t.Fatalf("need_mb = %d, ожидалось weights+kv+reserve = %d", got, want)
	}
	if plan.Fit.KVMB != 512 {
		t.Fatalf("kv_mb = %d, ожидалось 512 (131072 токена × 4 КБ)", plan.Fit.KVMB)
	}
	if plan.Fit.FreeMB != 20490 {
		t.Fatalf("free_mb = %d, ожидалось 20490 (свободная VRAM узла)", plan.Fit.FreeMB)
	}
}

// TestR659_NarrowCardRejectedForBigModel — 8-гигабайтная карта не выбирается под
// 27B: это тот самый дефект R248/«27B на 8 ГБ», только теперь он закреплён и на
// новой ветке решения.
func TestR659_NarrowCardRejectedForBigModel(t *testing.T) {
	p, _ := r659Proxy(t,
		r659Node{
			id: "master", port: 1, slots: 1, active: 1,
			totalMB: 23028, freeMB: 2471,
			loaded: []types.LlamaCppModel{r659LoadedModel(17*r659GiB, 8, 2, 64, 131072)},
		},
		r659Node{
			id: "rtx4000", port: -1, slots: 1, active: 0,
			totalMB: 8188, freeMB: 6000,
		},
	)

	if plan, ok := p.planColdLoadR659(r659Model, types.BackendTypeLlamaCpp, 131072); ok {
		t.Fatalf("17 ГиБ модель не должны выбирать на узел с 6 ГиБ свободной VRAM, выбран %q (%+v)",
			plan.BackendID, plan.Fit)
	}
}

// TestR659_WindowDoesNotFitNoLoad — окно не влезает: загрузку НЕ начинаем.
// Свободного места хватает на веса, но не на веса + KV нужного окна — именно
// так 27B с окном 201216 «садился» на 20.5 ГБ и падал на первом decode.
func TestR659_WindowDoesNotFitNoLoad(t *testing.T) {
	p, worker := r659Proxy(t,
		r659Node{
			id: "master", port: 1, slots: 1, active: 1,
			totalMB: 23028, freeMB: 2471,
			loaded: []types.LlamaCppModel{r659LoadedModel(17*r659GiB, 8, 2, 64, 262144)},
		},
		r659Node{
			id: "a10", port: -1, slots: 1, active: 0,
			totalMB: 23028, freeMB: 20490,
		},
	)

	// 17 ГиБ веса + 4 ГиБ KV (окно 1048576) + 1.1 ГиБ резерв ≈ 22.1 ГиБ > 20.5 ГиБ.
	plan, ok := p.planColdLoadR659(r659Model, types.BackendTypeLlamaCpp, 1048576)
	if ok {
		t.Fatalf("окно 1048576 не влезает (нужно %+v) — загрузку начинать нельзя", plan.Fit)
	}
	if got := r659NoLoad(t, worker); len(got) != 0 {
		t.Fatalf("POST /load отправлен, хотя окно не влезает: %v", got)
	}
}

// TestR659_StartLoadSendsPerSeqWindow — загрузка уходит с contextPerSeq (окно на
// клиента), а не только с contextSize (суммарным): при parallel > 1 второе даёт
// слоту половину запрошенного.
func TestR659_StartLoadSendsPerSeqWindow(t *testing.T) {
	p, worker := r659MasterAndFree(t, 8*r659GiB)

	plan, ok := p.planColdLoadR659(r659Model, types.BackendTypeLlamaCpp, 131072)
	if !ok {
		t.Fatal("план не построен")
	}
	if !p.startColdLoadR659(plan, r659Model) {
		t.Fatal("загрузка не запущена")
	}
	loads := worker.waitLoads(t, 1)
	if len(loads) != 1 {
		t.Fatalf("POST /load: %d, ожидался ровно 1", len(loads))
	}
	body := loads[0]
	if got, _ := body["contextPerSeq"].(float64); int(got) != 131072 {
		t.Fatalf("contextPerSeq = %v, ожидалось 131072 (окно на клиента)", body["contextPerSeq"])
	}
	if got, _ := body["contextSize"].(float64); int(got) != 131072 {
		t.Fatalf("contextSize = %v, ожидалось 131072", body["contextSize"])
	}
	if name, _ := body["name"].(string); name != r659Model {
		t.Fatalf("name = %v, ожидалось %q", body["name"], r659Model)
	}
}

// TestR659_DedupSingleLoadPerNode — две заявки и повторный шаг ожидания не
// порождают вторую загрузку: загрузка одна, ждать её можно всем.
func TestR659_DedupSingleLoadPerNode(t *testing.T) {
	p, worker := r659MasterAndFree(t, 8*r659GiB)

	if _, ok := p.maybeStartColdLoadR659(r659Model, types.BackendTypeLlamaCpp, 131072); !ok {
		t.Fatal("первая загрузка не запущена")
	}
	if _, ok := p.maybeStartColdLoadR659(r659Model, types.BackendTypeLlamaCpp, 131072); ok {
		t.Fatal("вторая попытка не должна запускать ещё одну загрузку (дедупликация)")
	}
	if got := r659LoadDeadline(t, worker, 1); len(got) != 1 {
		t.Fatalf("POST /load: %d, ожидался 1 (дедупликация по узлу и модели)", len(got))
	}
}

// TestR659_AutoloadDisabledNoLoad — выключатель автозагрузки не обходится:
// оператор запретил поднимать модели из запроса — значит, не поднимаем.
func TestR659_AutoloadDisabledNoLoad(t *testing.T) {
	p, worker := r659MasterAndFree(t, 8*r659GiB)
	p.lbAutoLoadAsync = false // SyncModelLoad.Enabled по умолчанию false

	if _, ok := p.maybeStartColdLoadR659(r659Model, types.BackendTypeLlamaCpp, 131072); ok {
		t.Fatal("при выключенной автозагрузке загрузку начинать нельзя")
	}
	if got := r659NoLoad(t, worker); len(got) != 0 {
		t.Fatalf("POST /load при выключенной автозагрузке: %v", got)
	}
}

// TestR659_LoadedCandidateNotReloaded — узел, где модель УЖЕ загружена, не
// получает повторную загрузку (иначе можно перезагрузить её с более узким окном —
// сжатие под живым клиентом, R294/R459).
func TestR659_LoadedCandidateNotReloaded(t *testing.T) {
	p, _ := r659Proxy(t,
		r659Node{
			id: "master", port: 1, slots: 1, active: 1,
			totalMB: 23028, freeMB: 2471,
			loaded: []types.LlamaCppModel{r659LoadedModel(8*r659GiB, 8, 2, 64, 131072)},
		},
		r659Node{
			id: "a10", port: -1, slots: 2, active: 0,
			totalMB: 23028, freeMB: 20490,
			loaded: []types.LlamaCppModel{r659LoadedModel(8*r659GiB, 8, 2, 64, 131072)},
		},
	)

	if plan, ok := p.planColdLoadR659(r659Model, types.BackendTypeLlamaCpp, 131072); ok {
		t.Fatalf("узел с уже загруженной моделью — не кандидат под загрузку (выбран %q)", plan.BackendID)
	}
}

// TestR659_WhyQueuedNamesLoadStarted — после запуска загрузки why_queued
// называет её («загрузка начата»), а не «модель не загружена».
func TestR659_WhyQueuedNamesLoadStarted(t *testing.T) {
	p, _ := r659MasterAndFree(t, 8*r659GiB)
	if _, ok := p.maybeStartColdLoadR659(r659Model, types.BackendTypeLlamaCpp, 131072); !ok {
		t.Fatal("загрузка не запущена")
	}
	row := r659RejectionRow(t, p, 131072)
	if row.Reason != rejectModelLoadingR460 {
		t.Fatalf("причина = %q, ожидалась %q (загрузка уже идёт)", row.Reason, rejectModelLoadingR460)
	}
	if !strings.Contains(row.Detail, "загрузка начата") {
		t.Fatalf("подробность не называет загрузку: %q", row.Detail)
	}
}

// TestR659_WhyQueuedNamesWindowDoesNotFit — если окно не влезает, причина обязана
// называть это ЧИСЛАМИ: «подожди загрузку» такой отказ не лечит.
func TestR659_WhyQueuedNamesWindowDoesNotFit(t *testing.T) {
	p, _ := r659Proxy(t,
		r659Node{
			id: "master", port: 1, slots: 1, active: 1,
			totalMB: 23028, freeMB: 2471,
			loaded: []types.LlamaCppModel{r659LoadedModel(17*r659GiB, 8, 2, 64, 262144)},
		},
		r659Node{
			id: "a10", port: -1, slots: 1, active: 0,
			totalMB: 23028, freeMB: 20490,
		},
	)

	row := r659RejectionRow(t, p, 1048576)
	if row.Reason != rejectWindowDoesNotFitR460 {
		t.Fatalf("причина = %q, ожидалась %q (окно не влезает: %s)",
			row.Reason, rejectWindowDoesNotFitR460, row.Detail)
	}
	if !strings.Contains(row.Detail, "не влезает") || !strings.Contains(row.Detail, "свободно") {
		t.Fatalf("подробность не называет числа: %q", row.Detail)
	}
}

// r659RejectionRow — строка why_queued по свободному узлу a10.
func r659RejectionRow(t *testing.T, p *Proxy, need int) backendRejectionR460 {
	t.Helper()
	list := p.backendRejectionsR460(r659Model, types.BackendTypeLlamaCpp,
		&backendRejectionMoveR480{Need: need, NeedSource: "session_window"})
	for _, row := range list {
		if row.BackendID == "a10" {
			return row
		}
	}
	t.Fatalf("в why_queued нет строки по свободному узлу: %+v", list)
	return backendRejectionR460{}
}

// TestR659_UnverifiedPlanStillTries — геометрии нет (воркер не отдаёт
// nLayers/nKvHeads/headDim), но узел достоверно свободен и VRAM известна:
// загрузку всё равно пробуем, но помечаем verified=false. «Не грузить никогда»
// хуже, чем попытка (тот же компромисс, что у этапа 3b с «VRAM неизвестна»).
func TestR659_UnverifiedPlanStillTries(t *testing.T) {
	p, _ := r659Proxy(t,
		r659Node{
			id: "master", port: 1, slots: 1, active: 1,
			totalMB: 23028, freeMB: 2471,
			// Запись есть, но БЕЗ геометрии (nLayers=0): KV не посчитать.
			loaded: []types.LlamaCppModel{{
				Name: r659Model, State: "loaded", Size: 8 * r659GiB, ContextPerSeq: 131072,
			}},
		},
		r659Node{
			id: "a10", port: -1, slots: 1, active: 0,
			totalMB: 23028, freeMB: 20490,
		},
	)

	plan, ok := p.planColdLoadR659(r659Model, types.BackendTypeLlamaCpp, 131072)
	if !ok {
		t.Fatal("без геометрии решение всё равно должно приниматься (паритет с этапом 3b)")
	}
	if plan.Fit.Verified {
		t.Fatalf("вердикт без геометрии не может быть проверенным: %+v", plan.Fit)
	}
}

// TestR659_ColdStartStillUsesThreeC — модель не загружена нигде: этот путь не
// R659, его решает обычный выбор (этап 3c), и подменять его нельзя.
func TestR659_ColdStartStillUsesThreeC(t *testing.T) {
	p, _ := r659Proxy(t,
		r659Node{id: "a", port: -1, slots: 1, totalMB: 23028, freeMB: 20490},
		r659Node{id: "b", port: -1, slots: 1, totalMB: 23028, freeMB: 20490},
	)

	if plan, ok := p.planColdLoadR659(r659Model, types.BackendTypeLlamaCpp, 131072); ok {
		t.Fatalf("при пустом кластере решает обычный выбор, а не R659 (выбран %q)", plan.BackendID)
	}
}

// TestR659_QueueAttemptsLoadWhileWaiting — заявка, стоящая в единой очереди,
// запускает загрузку, а не ждёт молча: это ровно тот дефект, из-за которого на
// стенде освобождали слот 300 с при простаивающих узлах.
func TestR659_QueueAttemptsLoadWhileWaiting(t *testing.T) {
	p, worker := r659Proxy(t,
		r659Node{
			id: "master", port: 1, slots: 1, active: 1,
			totalMB: 23028, freeMB: 2471,
			loaded: []types.LlamaCppModel{r659LoadedModel(8*r659GiB, 8, 2, 64, 131072)},
		},
		r659Node{id: "a10", port: -1, slots: 1, active: 0, totalMB: 23028, freeMB: 20490},
	)
	p.setAdmissionWait(1500 * time.Millisecond)
	// Сессия привязана к занятому мастеру с окном 131072 — состояние из архива.
	p.sessionMgr.SetWithProtocol("s-arch", "master", r659Model, "cli", "127.0.0.1", "ua", "")

	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	_, release, _, _, err := p.waitForInferenceBackendWithMoveR480(
		ctx, r659Model, types.BackendTypeLlamaCpp, "s-arch", "master", 131072, 8192)
	if release != nil {
		release()
	}
	if err != nil && !isR659SoftWaitErr(err) {
		t.Fatalf("неожиданная ошибка ожидания: %v", err)
	}
	if got := worker.waitLoads(t, 1); len(got) != 1 {
		t.Fatalf("пока заявка ждала в очереди, загрузка не начата (POST /load: %d)", len(got))
	}
	if got, _ := worker.loadRequests()[0]["contextPerSeq"].(float64); int(got) != 131072 {
		t.Fatalf("окно загрузки = %v, ожидалось 131072", worker.loadRequests()[0]["contextPerSeq"])
	}
}

// isR659SoftWaitErr — ошибки ожидания, допустимые в тесте (таймаут/отмена):
// предмет проверки — начатая загрузка, а не то, успел ли освободиться слот.
func isR659SoftWaitErr(err error) bool {
	if err == nil {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "deadline") ||
		strings.Contains(msg, "context canceled") ||
		strings.Contains(msg, "admission")
}

// TestR659_RequestServedFromNewCopyAfterLoad — вторая половина обещания R659:
// после того как загрузка на свободном узле закончилась, заявка, стоящая в
// очереди, ОБСЛУЖИВАЕТСЯ этой новой копией, а не ждёт свою до таймаута.
//
// Без этой проверки можно «начать загрузку» и всё равно не отдать запрос: гейт
// R459 сравнивает окно кандидата с окном текущей копии сессии, и копия, поднятая
// с тем же окном, обязана его проходить.
func TestR659_RequestServedFromNewCopyAfterLoad(t *testing.T) {
	p, worker := r659Proxy(t,
		r659Node{
			id: "master", port: 1, slots: 1, active: 1,
			totalMB: 23028, freeMB: 2471,
			loaded: []types.LlamaCppModel{r659LoadedModel(8*r659GiB, 8, 2, 64, 131072)},
		},
		r659Node{id: "a10", port: -1, slots: 1, active: 0, totalMB: 23028, freeMB: 20490},
	)
	p.setAdmissionWait(8 * time.Second)
	p.sessionMgr.SetWithProtocol("s-live", "master", r659Model, "cli", "127.0.0.1", "ua", "")

	// Заявка ждёт в очереди (мастер занят, свободный узел — «холодный», R459).
	type result struct {
		id  string
		err error
	}
	done := make(chan result, 1)
	go func() {
		id, release, _, _, err := p.waitForInferenceBackendWithMoveR480(
			context.Background(), r659Model, types.BackendTypeLlamaCpp, "s-live", "master", 131072, 8192)
		if release != nil {
			release()
		}
		done <- result{id: id, err: err}
	}()

	// Пока заявка ждёт — загрузка на свободном узле обязана начаться.
	if got := worker.waitLoads(t, 1); len(got) != 1 {
		t.Fatalf("загрузка не начата (POST /load: %d)", len(got))
	}
	if got, _ := worker.loadRequests()[0]["contextPerSeq"].(float64); int(got) != 131072 {
		t.Fatalf("окно загрузки = %v, ожидалось 131072", worker.loadRequests()[0]["contextPerSeq"])
	}

	// Загрузка закончилась: узел сообщил модель с тем же окном (так это видят
	// поллер и updateLlamaCppRunningModelInMetrics).
	loaded := r659LoadedModel(8*r659GiB, 8, 2, 64, 131072)
	p.metricsMgr.mu.Lock()
	if lm := p.metricsMgr.llamaMetrics["a10"]; lm != nil {
		lm.LoadedModels = []types.LlamaCppModel{loaded}
	}
	p.metricsMgr.mu.Unlock()
	p.metricsMgr.MarkLlamaCppPolled("a10", time.Now())

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("после загрузки заявка обязана обслужиться, получено: %v", res.err)
		}
		if res.id != "a10" {
			t.Fatalf("заявка обслужена на %q, ожидался новый узел a10", res.id)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("заявка не обслужена новой копией: очередь продолжает ждать занятый мастер, " +
			"хотя модель уже поднята на свободном узле")
	}
}
