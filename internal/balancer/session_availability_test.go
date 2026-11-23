// session_availability_test.go — R696: привязка сессии не держит заявку в
// очереди, когда свободная копия ТОЙ ЖЕ модели может её обслужить.
//
// ЖИВАЯ ЖАЛОБА (второй стенд, балансер r261120-r681 = этот код):
// «балансер не распределяет сессии по свободным бэкендам с одного IP-адреса: он
// явно видит запрос и ставит в очередь на any, но не распределяет на свободный
// бэкенд. С другого адреса запрос сразу ушёл как надо».
//
// ЧТО ПОКАЗЫВАЮТ ЭТИ ТЕСТЫ:
//  1. заявка сессии, привязанной к ЗАНЯТОМУ узлу, обслуживается свободной копией
//     той же модели, даже если окно её копии в метриках ещё не заполнено
//     (запись о загрузке без окна — «окно неизвестно», а НЕ «модели нет»);
//  2. запрос с ДРУГОГО адреса (другая сессия → другого гейта нет) обслуживается
//     так же, как раньше, — фикс не меняет его путь;
//  3. R493/R459 целы: если свободных копий нет или модели на них нет, заявка
//     ЖДЁТ (в этом и смысл очереди), а не уезжает куда попало;
//  4. привязка сессии не создаётся на узел, где модели заведомо нет;
//  5. жёсткий ресурсный отказ (диск) НЕ обходится, но называется в ответе
//     числами своего лимита — и пишется в лог с подавлением повторов;
//  6. отмена клиентом во время ожидания не выглядит в «Прокси-логах» успешным
//     ответом 200/bytes=0.
package balancer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

const availModelR696 = "m27b"

// diskLowReasonR696 — машинный код причины «мало места на диске» (R460/R696).
// Литерал, а не константа пакета: страж обязан собираться и на исходном коде
// (там этой константы ещё нет) — иначе «красный до правки» нечем доказать.
const diskLowReasonR696 = "disk_low"

// availCopyR696 — одна «копия» модели: что отдаёт воркер и что лежит в метриках.
type availCopyR696 struct {
	id string
	// window — окно на слот в /api/models воркера; 0 = воркер сообщает только
	// context_per_seq (без суммарного context_size) — ровно та форма записи,
	// которую пишет warmupLlamaCppModel → updateLlamaCppRunningModelInMetrics.
	window int
	// totalWindow — ContextLength в метриках балансера (суммарное окно);
	// 0 = записи о загрузке суммарного окна не содержат.
	totalWindow int
	// loaded — модель загружена на узле.
	loaded bool
	// delay — сколько «генерирует» ответ на inference.
	delay time.Duration
}

// availRigR696 — прокси с двумя llama.cpp-копиями + счётчики реальных inference.
type availRigR696 struct {
	proxy  *Proxy
	postsA *atomic.Int64
	postsB *atomic.Int64
	aURL   string
	bURL   string
	ua     string
}

// newAvailRigR696 собирает стенд из двух копий (A — привязанная, B — свободная).
func newAvailRigR696(t *testing.T, a, b availCopyR696) *availRigR696 {
	t.Helper()
	var postsA, postsB atomic.Int64

	mk := func(c availCopyR696, posts *atomic.Int64) (*httptest.Server, string) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/version", "/health", "/api/info":
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"version":"test","status":"ok"}`)
				return
			case "/api/models", "/api/tags":
				w.Header().Set("Content-Type", "application/json")
				if !c.loaded {
					_, _ = io.WriteString(w, `{"count":0,"models":[]}`)
					return
				}
				if c.window > 0 {
					_, _ = fmt.Fprintf(w,
						`{"count":1,"models":[{"name":%q,"path":"/m27b.gguf","state":"loaded",`+
							`"context_size":%d,"context_per_seq":%d,"max_slots":1,"parallel":1}]}`,
						availModelR696, c.window, c.window)
					return
				}
				// Воркер модель знает, окна не сообщает: ровно так выглядит
				// запись о загрузке, пока поллер не принёс context_per_seq.
				_, _ = fmt.Fprintf(w, `{"count":1,"models":[{"name":%q,"state":"loaded"}]}`, availModelR696)
				return
			}
			if r.Method != http.MethodPost {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"status":"ok"}`)
				return
			}
			// Считаем ТОЛЬКО inference: /api/models/load и /api/models/reload —
			// служебные POST'ы (автозагрузка/перезагрузка окна), их наличие не
			// значит, что узел обслужил заявку.
			if r.URL.Path != "/api/generate" && r.URL.Path != "/api/chat" &&
				r.URL.Path != "/v1/chat/completions" && r.URL.Path != "/v1/completions" {
				// Узел без модели файла не имеет: загрузка здесь невозможна
				// (R659 не должен «поднять» копию, которой нет).
				if !c.loaded {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = io.WriteString(w, `{"error":"model file is not present on this node"}`)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"status":"ok"}`)
				return
			}
			posts.Add(1)
			if c.delay > 0 {
				time.Sleep(c.delay)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"response":"ok","done":true}`)
		}))
		t.Cleanup(srv.Close)
		return srv, srv.URL
	}
	sA, aURL := mk(a, &postsA)
	sB, bURL := mk(b, &postsB)
	hA, pA := hostPort(sA.URL)
	hB, pB := hostPort(sB.URL)

	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "0.0.0.0", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: a.id, Host: hA, OllamaPort: pA, CppWorkerPort: pA, Weight: 1,
				MaxConcurrentReqs: 1, Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp},
			{ID: b.id, Host: hB, OllamaPort: pB, CppWorkerPort: pB, Weight: 1,
				MaxConcurrentReqs: 1, Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp},
		},
		Balancing: types.BalancingSettings{
			ModelAffinity: true, SessionStickiness: true,
			FirstByteTimeout: 30, StreamingIdleTimeout: 120, RequestTimeout: 30,
			QueueMaxSize: 100, SessionTTL: 900,
		},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{MaxUsagePercent: 95, MaxVRAMUsagePercent: 95},
			CPU: types.CPULimits{MaxUsagePercent: 95},
			// Диск: заведомо «много свободно», чтобы ресурсный гейт не мешал.
			Disk:   types.DiskLimits{MinFreeMB: 1024},
			Memory: types.MemoryLimits{MaxUsagePercent: 95},
		},
	}
	p := newProxyWithCleanup(t, cfg)
	setCopyMetricsR696(p, a)
	setCopyMetricsR696(p, b)
	return &availRigR696{proxy: p, postsA: &postsA, postsB: &postsB, aURL: aURL, bURL: bURL,
		ua: "Cline/1.0 (VSCode)"}
}

// setCopyMetricsR696 — метрики копии так, как их кладёт боевой путь агента:
// запись в metrics (её читает селектор: наличие модели) и в llamaMetrics (оттуда
// берётся окно на слот). Окно 0 = «модель есть, окно неизвестно».
func setCopyMetricsR696(p *Proxy, c availCopyR696) {
	m := &types.BackendMetrics{
		ID: c.id, Timestamp: time.Now(), Status: types.StatusHealthy, HasAgent: true,
		GPU:    types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 1024, MemoryFree: 23000},
		System: types.SystemMetrics{CPUUsagePercent: 5, MemoryTotal: 65536, MemoryUsed: 16384, MemoryFree: 49152, DiskFree: 200000},
	}
	if c.loaded {
		m.LlamaCpp = types.LlamaCppMetrics{
			LoadedModels: []types.LlamaCppModel{{
				Name: availModelR696, State: "loaded",
				ContextLength: c.totalWindow, ContextPerSeq: c.window, MaxSlots: 1, Parallel: 1,
			}},
			VramKnown: true, AvailableVRAMMB: 23000,
		}
	}
	p.UpdateMetrics(c.id, m)
	p.metricsMgr.MarkLlamaCppPolled(c.id, time.Now())
}

// reqR696 — запрос от клиента с указанным IP (IP — из RemoteAddr).
func (r *availRigR696) reqR696(ip string) *http.Request {
	body, _ := json.Marshal(map[string]interface{}{
		"model": availModelR696, "prompt": "hi", "stream": false,
	})
	req := httptest.NewRequest("POST", "/api/generate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", r.ua)
	req.RemoteAddr = ip + ":12345"
	return req
}

// pinR696 — сессия клиента IP1 уже работала на бэкенде a.
func (r *availRigR696) pinR696(t *testing.T, ip string) string {
	t.Helper()
	probe := r.reqR696(ip)
	name := r.proxy.getClientName(probe)
	sid := r.proxy.getSessionIDWithModel(probe, name, availModelR696)
	r.proxy.sessionMgr.SetWithProtocol(sid, "a", availModelR696, name, ip, r.ua,
		RequestProtocol("/api/generate"))
	return sid
}

// busyWithOwnStreamR696 — бэкенд a занят собственным стримом этой сессии: ровно
// то, что в жизни делает предыдущий ход агента (слот 1/1 + inflight сессии).
func (r *availRigR696) busyWithOwnStreamR696(sid string) {
	r.proxy.AcquireSlotForTest("a")
	r.proxy.admission.markInflight(sid, 1)
}

// busyWithForeignR696 — бэкенд a занят чужим запросом.
func (r *availRigR696) busyWithForeignR696() {
	r.proxy.AcquireSlotForTest("a")
}

type availResultR696 struct {
	code      int
	elapsed   time.Duration
	waitHdr   string
	body      string
	queueWait string
}

func (r *availRigR696) doR696(ip string) availResultR696 {
	rec := httptest.NewRecorder()
	start := time.Now()
	r.proxy.ServeHTTP(rec, r.reqR696(ip))
	return availResultR696{
		code:      rec.Code,
		elapsed:   time.Since(start),
		waitHdr:   rec.Header().Get("X-Queue-Wait-Ms"),
		body:      rec.Body.String(),
		queueWait: rec.Header().Get("X-Queue-Wait-Sec"),
	}
}

// ---------------------------------------------------------------------------
// 1. Главный кейс живой жалобы: заявка сессии, привязанной к ЗАНЯТОМУ узлу,
// обслуживается свободной копией той же модели — без ожидания в очереди.
// ---------------------------------------------------------------------------

// queueRigR696 — прокси для проверки РЕШЕНИЯ очереди (без HTTP-моков): привязанная
// копия a занята, свободная b держит запись о загрузке с окном ТОЛЬКО на слот.
func queueRigR696(t *testing.T) *Proxy {
	t.Helper()
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "127.0.0.1", Port: 18090, APIPort: 18091},
		Backends: []types.Backend{
			{ID: "a", Name: "a", Host: "127.0.0.1", OllamaPort: 1, CppWorkerPort: 1,
				Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy, MaxConcurrentReqs: 1, Weight: 1},
			{ID: "b", Name: "b", Host: "127.0.0.1", OllamaPort: 2, CppWorkerPort: 2,
				Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy, MaxConcurrentReqs: 1, Weight: 1},
		},
		Balancing: types.BalancingSettings{ModelAffinity: true, SessionStickiness: true, MetricsInterval: 60},
	}
	p := newProxyWithCleanup(t, cfg)
	setQueueWindowR696(p, "a", 131072, 131072)
	setQueueWindowR696(p, "b", 0, 131072)
	return p
}

// setQueueWindowR696 — запись о загрузке: total (ContextLength) и perSeq
// (ContextPerSeq). perSeq-only — форма, которую пишет warmupLlamaCppModel.
func setQueueWindowR696(p *Proxy, id string, total, perSeq int) {
	lm := &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{{
			Name: availModelR696, State: "loaded",
			ContextLength: total, ContextPerSeq: perSeq, MaxSlots: 1, Parallel: 1,
		}},
		VramKnown: true, AvailableVRAMMB: 23000,
	}
	m := &types.BackendMetrics{
		ID: id, Status: types.StatusHealthy,
		GPU:    types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 1024, MemoryFree: 23000},
		System: types.SystemMetrics{CPUUsagePercent: 5, MemoryTotal: 65536, MemoryUsed: 16384, MemoryFree: 49152, DiskTotal: 300000, DiskFree: 200000},
	}
	if perSeq > 0 || total > 0 {
		m.LlamaCpp = *lm
	}
	p.UpdateMetrics(id, m)
	p.metricsMgr.MarkLlamaCppPolled(id, time.Now())
}

// TestR696_StickySessionQueueTakesFreeCopy — заявка ТОЙ ЖЕ сессии (тот же IP)
// получает слот на свободной копии, а не ждёт занятую.
//
// На исходном коде гейт R459/R480 видит у b «окно 0» и отказывает ей как
// холодной («модель не загружена»), поэтому заявка стоит в очереди до бюджета.
func TestR696_StickySessionQueueTakesFreeCopy(t *testing.T) {
	t.Setenv("LB_ADMISSION_WAIT_SEC", "1")
	p := queueRigR696(t)
	const sid = "sess-same-ip"
	p.sessionMgr.Set(sid, "a", availModelR696, "Cline", "10.0.0.1", "Cline/1.0")
	p.backends["a"].ActiveReqs = 1
	p.admission.markInflight(sid, 1)

	// Условие теста: модель на b загружена, запись несёт окно на слот без суммарного.
	lm, ok := p.metricsMgr.SnapshotLlamaCppMetrics("b")
	if !ok || len(lm.LoadedModels) == 0 || lm.LoadedModels[0].ContextPerSeq <= 0 || lm.LoadedModels[0].ContextLength != 0 {
		t.Fatalf("условие теста не выполнено: у b должна быть запись «окно на слот, без суммарного»: %+v", lm)
	}
	if !p.backendHasModelByID("b", availModelR696) {
		t.Fatal("условие теста не выполнено: модель на b обязана считаться загруженной")
	}

	from := p.stickyBackendForGateR493(sid)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	id, release, waited, _, err := p.waitForInferenceBackendWithMoveR480(ctx, availModelR696, "", sid, from, 0, 0)
	if release != nil {
		defer release()
	}
	if err != nil || id != "b" {
		t.Fatalf("заявка той же сессии не получила свободную копию: backend=%q waited=%v err=%v "+
			"(на исходном коде здесь очередь ждала занятую копию весь бюджет)", id, waited, err)
	}
	if waited > 300*time.Millisecond {
		t.Fatalf("заявка ждала %v свободную копию той же модели", waited)
	}
	if p.backends["b"].ActiveReqs != 1 {
		t.Fatalf("слот свободной копии не занят (ActiveReqs=%d)", p.backends["b"].ActiveReqs)
	}
}

// TestR696_PerSeqOnlyWindowIsVisible — окно на слот, пришедшее БЕЗ суммарного
// context_size, обязано быть видно всем читателям окна.
//
// Это корень живой жалобы: гейты очереди и R303/preflight читают окно через
// loadedPerSeqNCtx / loadedWindowFromCache, а те смотрели ТОЛЬКО на
// ContextLength. Запись warmupLlamaCppModel → updateLlamaCppRunningModelInMetrics
// несёт только ContextPerSeq, поэтому свежеподнятая копия выглядела «без модели»:
// свободный бэкенд исключался из выбора для живой сессии, а preflight на нём же
// отказывал 503 «модель не загружена здесь».
func TestR696_PerSeqOnlyWindowIsVisible(t *testing.T) {
	p := queueRigR696(t)
	if got := p.loadedPerSeqNCtx("b", availModelR696); got != 131072 {
		t.Fatalf("loadedPerSeqNCtx(b) = %d, ожидалось 131072 (окно на слот есть в записи)", got)
	}
	n, state := p.loadedWindowFromCache("b", availModelR696)
	if state != loadedWindowLoaded || n <= 0 {
		t.Fatalf("loadedWindowFromCache(b) = (%d,%v), ожидалось (131072, loaded): "+
			"иначе копия считается «модели нет» и preflight/R303 отказывают с нуля", n, state)
	}
	// У копии без записи о модели поведение не меняется: «неизвестно», а не «загружено».
	p.metricsMgr.ClearBackendMetrics("b")
	p.UpdateMetrics("b", &types.BackendMetrics{ID: "b", Status: types.StatusHealthy})
	if n, state := p.loadedWindowFromCache("b", availModelR696); state == loadedWindowLoaded {
		t.Fatalf("копия без записи о модели не может считаться загруженной: (%d,%v)", n, state)
	}
}

// ---------------------------------------------------------------------------
// 2. R493 цел: свободных копий нет — заявка ЖДЁТ (и упирается в бюджет).
// ---------------------------------------------------------------------------

func TestR696_StickySessionWaitsWhenNoFreeCopy(t *testing.T) {
	t.Setenv("LB_ADMISSION_WAIT_SEC", "1")

	rig := newAvailRigR696(t,
		availCopyR696{id: "a", window: 131072, totalWindow: 131072, loaded: true},
		availCopyR696{id: "b", window: 131072, totalWindow: 131072, loaded: true},
	)
	sid := rig.pinR696(t, "10.0.0.1")
	rig.busyWithOwnStreamR696(sid)
	rig.proxy.AcquireSlotForTest("b") // свободных слотов нет ни у кого

	res := rig.doR696("10.0.0.1")
	if res.code != http.StatusServiceUnavailable {
		t.Fatalf("при занятых узлах ожидался 503 после ожидания, получен %d: %s",
			res.code, availTruncR696(res.body, 300))
	}
	if res.elapsed < 800*time.Millisecond {
		t.Fatalf("ответ за %v: заявка не ждала слот, а это значит, что очередь обошли", res.elapsed)
	}
	if !strings.Contains(res.body, "backend_rejections") {
		t.Fatalf("в теле 503 нет причин отказа по бэкендам (R460): %s", availTruncR696(res.body, 400))
	}
	if rig.postsA.Load() != 0 || rig.postsB.Load() != 0 {
		t.Fatalf("занятые узлы получили запросы (a=%d b=%d) — слот не должен был захватиться",
			rig.postsA.Load(), rig.postsB.Load())
	}
}

// ---------------------------------------------------------------------------
// 3. R459 цел: модель есть ТОЛЬКО у занятого — заявка ждёт, на холодную копию
// не уезжает (переезд = выгрузка+загрузка посреди агентского хода).
// ---------------------------------------------------------------------------

func TestR696_StickySessionWaitsWhenModelOnlyOnPinnedBackend(t *testing.T) {
	t.Setenv("LB_ADMISSION_WAIT_SEC", "1")

	rig := newAvailRigR696(t,
		availCopyR696{id: "a", window: 131072, totalWindow: 131072, loaded: true},
		availCopyR696{id: "b", window: 131072, totalWindow: 0, loaded: false},
	)
	sid := rig.pinR696(t, "10.0.0.1")
	rig.busyWithOwnStreamR696(sid)

	res := rig.doR696("10.0.0.1")
	if res.code != http.StatusServiceUnavailable {
		t.Fatalf("модели на свободном узле нет — ожидался 503 после ожидания, получен %d: %s",
			res.code, availTruncR696(res.body, 300))
	}
	if rig.postsB.Load() != 0 {
		t.Fatalf("холодная копия получила %d inference-запросов, хотя модели на ней нет",
			rig.postsB.Load())
	}
	if !strings.Contains(res.body, string(rejectModelAbsentR460)) &&
		!strings.Contains(res.body, string(rejectModelLoadingR460)) {
		t.Fatalf("причина «модели нет / грузится» не названа в теле 503: %s", availTruncR696(res.body, 400))
	}
}

// ---------------------------------------------------------------------------
// 4. Привязка сессии не создаётся на узел, где модели заведомо нет.
// ---------------------------------------------------------------------------

func TestR696_SessionBindingNotCreatedOnModelLessBackend(t *testing.T) {
	t.Setenv("LB_ADMISSION_WAIT_SEC", "0")

	// Обе копии без модели: селектор выберет свободный узел (P4/3b), и вот
	// привязывать сессию к нему нельзя — следующая же её заявка вернётся туда,
	// где модели нет, и будет ждать загрузку (живой архив: 3 срабатывания R303
	// за 13 минут).
	rig := newAvailRigR696(t,
		availCopyR696{id: "a", window: 0, totalWindow: 0, loaded: false},
		availCopyR696{id: "b", window: 0, totalWindow: 0, loaded: false},
	)
	ip := "10.0.0.1"
	probe := rig.reqR696(ip)
	name := rig.proxy.getClientName(probe)
	sid := rig.proxy.getSessionIDWithModel(probe, name, availModelR696)

	res := rig.doR696(ip)
	if res.code == http.StatusOK {
		t.Fatalf("условие теста не выполнено: без модели на узлах 200 получить неоткуда: %s",
			availTruncR696(res.body, 300))
	}
	// Невакуумность: запрос дошёл до выбора бэкенда — в прокси-логе есть backend.
	logs := rig.proxy.proxyLogger.GetLast(5)
	if len(logs) == 0 || logs[len(logs)-1].BackendID == "" {
		t.Fatalf("условие теста не выполнено: бэкенд не выбирался (logs=%d)", len(logs))
	}
	chosen := logs[len(logs)-1].BackendID
	if bound, _ := rig.proxy.sessionMgr.GetBackendID(sid); bound != "" {
		t.Fatalf("сессия привязана к узлу %q, на котором модели нет (выбран был %q): "+
			"следующая заявка этой сессии уйдёт туда и будет ждать загрузку", bound, chosen)
	}
}

// ---------------------------------------------------------------------------
// 5. Жёсткий ресурсный отказ: не обходится, называется числами, лог подавлен.
// ---------------------------------------------------------------------------

func TestR696_ResourceBlockedFreeCopyIsNamedWithNumbers(t *testing.T) {
	rig := newAvailRigR696(t,
		availCopyR696{id: "a", window: 131072, totalWindow: 131072, loaded: true},
		availCopyR696{id: "b", window: 131072, totalWindow: 131072, loaded: true},
	)
	p := rig.proxy
	// Живой архив второго стенда: свободный A10-126 с ТОЙ ЖЕ моделью исключён
	// диском (9480 МБ < 10240 МБ). Порог НЕ понижаем — проверяем, что отказ
	// остаётся отказом и при этом становится названным.
	p.config.Resources.Disk.MinFreeMB = 10240
	m, _ := p.metricsMgr.SnapshotBackendMetrics("b")
	m.System.DiskTotal = 300000
	m.System.DiskFree = 9480
	p.UpdateMetrics("b", m)

	resetSoftLogThrottleR488()
	buf := logger.NewCaptureBuffer()
	restore := logger.CaptureToBuffer(buf)
	if p.checkResourceLimitsLocked("b") {
		restore()
		t.Fatal("узел с 9480 МБ свободного диска при пороге 10240 МБ не должен проходить ресурсный гейт")
	}
	// Гейт не обходится: второй и третий проход тоже отказывают.
	for i := 0; i < 2; i++ {
		if p.checkResourceLimitsLocked("b") {
			restore()
			t.Fatal("повторный проход ресурсного гейта пропустил узел с малым диском")
		}
	}
	restore()
	// И жёсткий отказ пишется в лог с подавлением повторов: три прохода — одна
	// строка. В живом архиве их было 1565 за 13 минут, и кольцо логов состояло
	// из них, а не из строк request trace.
	if got := strings.Count(buf.String(), "backend disk space critical, blocking"); got != 1 {
		t.Fatalf("жёсткий ресурсный отказ напечатан %d раз за три прохода, ожидалась одна строка — "+
			"без подавления он вытесняет из кольца логи запросов", got)
	}
	// Невакуумность троттлинга: жёсткий отказ пишется в лог через подавитель
	// (иначе 1565 WARN за 13 минут вытесняют кольцо логов).
	if softLogAllowedR488("b", "disk_low", time.Now(), softLogEveryR488) {
		t.Fatal("жёсткий ресурсный отказ не прошёл через подавитель лога (softLogAllowedR488): " +
			"в живом архиве это 1565 WARN за 13 минут")
	}

	list := p.backendRejectionsR460(availModelR696, "")
	reason, detail, found := "", "", false
	for _, r := range list {
		if r.BackendID == "b" {
			reason, detail, found = r.Reason, r.Detail, true
		}
	}
	if !found {
		t.Fatalf("узел b вообще не попал в список причин: %+v", list)
	}
	if reason != diskLowReasonR696 {
		t.Fatalf("причина %q, ожидалась %q: «мало места на диске» — это не общий resource_limit",
			reason, diskLowReasonR696)
	}
	for _, want := range []string{"9480", "10240"} {
		if !strings.Contains(detail, want) {
			t.Errorf("в причине нет числа %q: %q — без чисел оператору нечего решать", want, detail)
		}
	}
	// И сама причина обязана доехать до тела 503 (R460): общий ответ строит
	// writeAdmissionUnavailable, поэтому проверяем через ту же функцию списка.
	t.Logf("причина названа: %s (%s)", reason, detail)
}

// ---------------------------------------------------------------------------
// 6. Заявка ждёт занятую копию, потому что свободная исключена ресурсным
// гейтом, — и клиент видит ПОЧЕМУ, а не молчаливое ожидание.
// ---------------------------------------------------------------------------

func TestR696_QueueNamesResourceBlockedFreeCopy(t *testing.T) {
	t.Setenv("LB_ADMISSION_WAIT_SEC", "1")

	rig := newAvailRigR696(t,
		availCopyR696{id: "a", window: 131072, totalWindow: 131072, loaded: true},
		availCopyR696{id: "b", window: 131072, totalWindow: 131072, loaded: true},
	)
	p := rig.proxy
	p.config.Resources.Disk.MinFreeMB = 10240
	m, _ := p.metricsMgr.SnapshotBackendMetrics("b")
	m.System.DiskTotal = 300000
	m.System.DiskFree = 9480
	p.UpdateMetrics("b", m)

	sid := rig.pinR696(t, "10.0.0.1")
	rig.busyWithOwnStreamR696(sid)

	res := rig.doR696("10.0.0.1")
	if res.code != http.StatusServiceUnavailable {
		t.Fatalf("свободная копия исключена ресурсным гейтом — ожидался 503 после ожидания, "+
			"получен %d: %s", res.code, availTruncR696(res.body, 300))
	}
	if !strings.Contains(res.body, string(diskLowReasonR696)) {
		t.Fatalf("в теле 503 причина не названа (%q ожидался): %s",
			diskLowReasonR696, availTruncR696(res.body, 600))
	}
	if !strings.Contains(res.body, "9480") {
		t.Fatalf("в теле 503 нет чисел диска — оператор не увидит, почему свободный узел не взял заявку: %s",
			availTruncR696(res.body, 600))
	}
	if rig.postsB.Load() != 0 {
		t.Fatalf("исключённый по ресурсам узел всё-таки получил %d запросов — гейт обойдён", rig.postsB.Load())
	}
}

// ---------------------------------------------------------------------------
// 7. Отмена клиентом во время ожидания: в «Прокси-логах» это не 200/bytes=0.
// ---------------------------------------------------------------------------

func TestR696_CancelWhileQueuedIsNotLoggedAsSuccess(t *testing.T) {
	t.Setenv("LB_ADMISSION_WAIT_SEC", "30")

	rig := newAvailRigR696(t,
		availCopyR696{id: "a", window: 131072, totalWindow: 131072, loaded: true, delay: 5 * time.Second},
		availCopyR696{id: "b", window: 131072, totalWindow: 131072, loaded: true},
	)
	p := rig.proxy
	sid := rig.pinR696(t, "10.0.0.1")
	rig.busyWithOwnStreamR696(sid)
	rig.proxy.AcquireSlotForTest("b") // ждать придётся

	req := rig.reqR696("10.0.0.1")
	ctx, cancel := contextWithCancelR696()
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { p.ServeHTTP(rec, req); close(done) }()
	time.Sleep(150 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ServeHTTP не завершился после отмены клиента")
	}

	logs := p.proxyLogger.GetLast(5)
	if len(logs) == 0 {
		t.Fatal("в прокси-логе нет записи об отменённом запросе")
	}
	last := logs[len(logs)-1]
	if last.StatusCode == http.StatusOK {
		t.Fatalf("отменённая клиентом заявка записана как 200 (bytes=0, first_byte=-1): "+
			"в живом архиве это неотличимо от пустого УСПЕШНОГО ответа (statusCode=%d)", last.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// 8. R303: событие «сессия залипла на узле без модели» обязано называть сессию
// и причину — иначе его нельзя связать с живым ходом клиента.
// ---------------------------------------------------------------------------

func TestR696_R303LogCarriesSessionAndReason(t *testing.T) {
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, "proxy.go", nil, 0)
	if err != nil {
		t.Fatalf("не разобрать proxy.go: %v", err)
	}
	found := 0
	ast.Inspect(af, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		first, ok := call.Args[0].(*ast.BasicLit)
		if !ok || first.Kind != token.STRING || !strings.Contains(first.Value, "R303: сессия залипла") {
			return true
		}
		found++
		keys := map[string]bool{}
		for _, arg := range call.Args[1:] {
			if lit, ok := arg.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				keys[strings.Trim(lit.Value, `"`)] = true
			}
		}
		for _, need := range []string{"session", "reason", "from", "to"} {
			if !keys[need] {
				t.Errorf("в логе R303 нет поля %q: живой разбор не может связать событие с сессией "+
					"и причиной", need)
			}
		}
		return true
	})
	if found == 0 {
		t.Fatal("строка лога R303 не найдена в proxy.go — страж ослеп")
	}
}

// contextWithCancelR696 — отменяемый контекст запроса (клиент оборвал соединение).
func contextWithCancelR696() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

func availTruncR696(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
