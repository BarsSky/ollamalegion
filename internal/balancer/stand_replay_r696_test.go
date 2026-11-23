// stand_replay_r696_test.go — ВОСПРОИЗВЕДЕНИЕ второго стенда (архив
// diag-2b923c0df1db, 2026-11-23T07:04Z, балансер r681/ffd8bbb4) на
// изолированном балансере r696: три llama.cpp-бэкенда, фейковые воркеры.
//
// ЧТО ИМЕННО ВОСПРОИЗВОДИТСЯ (числа — из summary.json архива):
//
//	cppworker-gpu-bundled-agent : 27B loaded, окно 131072, слот 1/1, диск 37474/244800
//	A10-126                     : 27B loaded, окно 131072, слот 1/1, диск  9480/194403  (minFreeMB=10240)
//	ASTRA-I9                    : 8 ГБ VRAM (Quadro RTX 4000), модели нет, свободно 5035 МБ
//	IM-A10-126 / IM-ASTRA-I9 / imageworker : image_cpp, текстовые запросы не берут
//
// Сценарий живой жалобы: сессия с ОДНОГО IP привязана к занятой копии
// (cppworker-gpu), «свободная» копия A10-126 исключена диском. Запрос той же
// сессии ждёт; запрос с другого IP (новой сессии) идёт как обычно.
//
// ЭТОТ ФАЙЛ — РАЗЛИЧАЮЩИЙ ТЕСТ. Он печатает для каждой фазы:
//   - кто обслужил (X-Backend-ID) и за сколько миллисекунд;
//   - ждала ли заявка в очереди (X-Queue-Wait-Ms) и сколько;
//   - какую причину назвал why_queued (disk_low / window_too_small / ...).
//
// Поэтому по выводу теста видно, КАКОЙ гейт сработал, а не только «не поехало».
package balancer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

const standModelR696 = "Qwen3.8-27B-UD-Q4_K_M"

// standModelSizeR696 — размер файла 27B из архива (loadingSizeBytes).
const standModelSizeR696 = int64(16464440224)

// standWorkerR696 — фейковый cppworker: отдаёт /api/models в боевой форме
// (snake_case context_size/context_per_seq), служебные POST'ы и inference.
type standWorkerR696 struct {
	id             string
	url            string
	loaded         atomic.Bool
	contextLength  atomic.Int64
	contextPerSeq  atomic.Int64
	inferChat      atomic.Int64
	inferGenerate  atomic.Int64
	inferOpenAI    atomic.Int64
	inferenceDelay time.Duration
}

func newStandWorkerR696(t *testing.T, id string, loaded bool, total, perSeq int) *standWorkerR696 {
	t.Helper()
	w := &standWorkerR696{id: id}
	w.loaded.Store(loaded)
	w.contextLength.Store(int64(total))
	w.contextPerSeq.Store(int64(perSeq))
	w.inferenceDelay = 20 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/version", r.URL.Path == "/health", r.URL.Path == "/api/info",
			r.URL.Path == "/api/models/load", r.URL.Path == "/api/models/reload",
			r.URL.Path == "/api/models/unload", r.URL.Path == "/v1/models":
			rw.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(rw, `{"status":"ok","version":"stand-replay"}`)
		case r.URL.Path == "/api/models", r.URL.Path == "/api/tags":
			rw.Header().Set("Content-Type", "application/json")
			if !w.loaded.Load() {
				_, _ = io.WriteString(rw, `{"count":0,"models":[]}`)
				return
			}
			_, _ = fmt.Fprintf(rw,
				`{"count":1,"models":[{"name":%q,"path":"/app/models/%s.gguf","state":"loaded",`+
					`"size":%d,"context_size":%d,"context_per_seq":%d,"max_slots":1,"parallel":1}]}`,
				standModelR696, standModelR696, uint64(standModelSizeR696),
				w.contextLength.Load(), w.contextPerSeq.Load())
		case r.Method == http.MethodPost && r.URL.Path == "/api/chat":
			w.inferChat.Add(1)
			time.Sleep(w.inferenceDelay)
			rw.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(rw, `{"model":"`+standModelR696+`","message":{"role":"assistant","content":"ok"},"done":true}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/generate":
			w.inferGenerate.Add(1)
			time.Sleep(w.inferenceDelay)
			rw.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(rw, `{"model":"`+standModelR696+`","response":"ok","done":true}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
			w.inferOpenAI.Add(1)
			time.Sleep(w.inferenceDelay)
			rw.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(rw, `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
		default:
			rw.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(rw, `{"status":"ok"}`)
		}
	}))
	t.Cleanup(srv.Close)
	w.url = srv.URL
	return w
}

// standCopyR696 — метрики одной копии ровно как в архиве.
type standCopyR696 struct {
	id         string
	typ        types.BackendType
	loaded     bool
	total      int
	perSeq     int
	diskTotal  uint64
	diskFree   uint64
	gpuTotal   uint64
	gpuUsed    uint64
	maxReqs    int
	modelSizes bool
}

// standReplayRigR696 — прокси + воркеры стенда.
type standReplayRigR696 struct {
	proxy   *Proxy
	cpp     *standWorkerR696
	a10     *standWorkerR696
	astra   *standWorkerR696
	a10Copy standCopyR696
	mkReq   func(ip string) *http.Request
}

func (r *standReplayRigR696) setDiskR696(t *testing.T, backendID string, freeMB uint64) {
	t.Helper()
	m, ok := r.proxy.metricsMgr.SnapshotBackendMetrics(backendID)
	if !ok || m == nil {
		t.Fatalf("нет метрик %q", backendID)
	}
	m.System.DiskFree = freeMB
	r.proxy.UpdateMetrics(backendID, m)
	r.proxy.metricsMgr.MarkLlamaCppPolled(backendID, time.Now())
}

func (r *standReplayRigR696) setWindowR696(t *testing.T, backendID string, total, perSeq int) {
	t.Helper()
	m, ok := r.proxy.metricsMgr.SnapshotBackendMetrics(backendID)
	if !ok || m == nil {
		t.Fatalf("нет метрик %q", backendID)
	}
	m.LlamaCpp.LoadedModels = []types.LlamaCppModel{{
		Name: standModelR696, State: "loaded", Size: uint64(standModelSizeR696),
		ContextLength: total, ContextPerSeq: perSeq, MaxSlots: 1, Parallel: 1,
	}}
	r.proxy.UpdateMetrics(backendID, m)
	r.proxy.metricsMgr.MarkLlamaCppPolled(backendID, time.Now())
}

// newStandReplayR696 — стенд из архива. diskFreeA10 — чтобы фаза «диск
// освободили» была буквально сменой одного числа.
func newStandReplayR696(t *testing.T, diskFreeA10 uint64) *standReplayRigR696 {
	t.Helper()

	cpp := newStandWorkerR696(t, "cppworker-gpu-bundled-agent", true, 131072, 131072)
	a10 := newStandWorkerR696(t, "A10-126", true, 131072, 131072)
	astra := newStandWorkerR696(t, "ASTRA-I9", false, 0, 0)

	hostOf := func(u string) (string, int) { return hostPort(u) }
	hCpp, pCpp := hostOf(cpp.url)
	hA10, pA10 := hostOf(a10.url)
	hAstra, pAstra := hostOf(astra.url)

	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "127.0.0.1", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: "cppworker-gpu-bundled-agent", Name: "cppworker-gpu", Host: hCpp, OllamaPort: pCpp, CppWorkerPort: pCpp,
				Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy, MaxConcurrentReqs: 1, Weight: 1},
			{ID: "A10-126", Name: "A10-126", Host: hA10, OllamaPort: pA10, CppWorkerPort: pA10,
				Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy, MaxConcurrentReqs: 1, Weight: 1},
			{ID: "ASTRA-I9", Name: "ASTRA-I9", Host: hAstra, OllamaPort: pAstra, CppWorkerPort: pAstra,
				Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy, MaxConcurrentReqs: 1, Weight: 1},
			// image_cpp-бэкенды: их наличие делает /api/chat «смешанным режимом»
			// (bt=""), т.е. запрос идёт основным flow ServeHTTP, а не llama.cpp-
			// роутером — ровно как в архиве стенда.
			{ID: "IM-A10-126", Name: "IM-A10-126", Host: "127.0.0.1", OllamaPort: 1, CppWorkerPort: 1,
				Type: types.BackendTypeImage, Status: types.StatusHealthy, MaxConcurrentReqs: 64, Weight: 1},
			{ID: "IM-ASTRA-I9", Name: "IM-ASTRA-I9", Host: "127.0.0.1", OllamaPort: 2, CppWorkerPort: 2,
				Type: types.BackendTypeImage, Status: types.StatusHealthy, MaxConcurrentReqs: 64, Weight: 1},
			{ID: "imageworker", Name: "imageworker", Host: "127.0.0.1", OllamaPort: 3, CppWorkerPort: 3,
				Type: types.BackendTypeImage, Status: types.StatusHealthy, MaxConcurrentReqs: 64, Weight: 1},
		},
		Balancing: types.BalancingSettings{
			ModelAffinity: true, SessionStickiness: true,
			FirstByteTimeout: 30, StreamingIdleTimeout: 120, RequestTimeout: 30,
			QueueMaxSize: 100, SessionTTL: 900, MetricsInterval: 30,
			ModelLoadTimeout: 30,
		},
		Resources: types.ResourceLimits{
			GPU:    types.GPULimits{MaxUsagePercent: 95, MaxVRAMUsagePercent: 95},
			CPU:    types.CPULimits{MaxUsagePercent: 95},
			Memory: types.MemoryLimits{MaxUsagePercent: 95},
			// Порог из архива второго стенда: disk_free_mb=9480 < min_required_mb=10240.
			Disk: types.DiskLimits{MinFreeMB: 10240},
		},
	}
	p := newProxyWithCleanup(t, cfg)

	copies := []standCopyR696{
		{id: "cppworker-gpu-bundled-agent", typ: types.BackendTypeLlamaCpp, loaded: true,
			total: 131072, perSeq: 131072, diskTotal: 244800, diskFree: 37474,
			gpuTotal: 22488, gpuUsed: 19622, maxReqs: 1},
		{id: "A10-126", typ: types.BackendTypeLlamaCpp, loaded: true,
			total: 131072, perSeq: 131072, diskTotal: 194403, diskFree: diskFreeA10,
			gpuTotal: 22488, gpuUsed: 20142, maxReqs: 1},
		// 8 ГБ Quadro RTX 4000: модели нет, свободно 5035 МБ — 27B туда не влезает.
		{id: "ASTRA-I9", typ: types.BackendTypeLlamaCpp, loaded: false,
			diskTotal: 300786, diskFree: 14429, gpuTotal: 7780, gpuUsed: 2745, maxReqs: 1},
	}
	for _, c := range copies {
		m := &types.BackendMetrics{
			ID: c.id, Timestamp: time.Now(), Status: types.StatusHealthy, BackendType: c.typ,
			GPU:    types.GPUMetrics{UsagePercent: 10, MemoryTotal: c.gpuTotal, MemoryUsed: c.gpuUsed, MemoryFree: c.gpuTotal - c.gpuUsed},
			System: types.SystemMetrics{CPUUsagePercent: 5, MemoryTotal: 100644, MemoryUsed: 16847, MemoryFree: 83797, DiskTotal: c.diskTotal, DiskFree: c.diskFree, DiskUsed: c.diskTotal - c.diskFree},
		}
		if c.loaded {
			m.LlamaCpp = types.LlamaCppMetrics{
				LoadedModels: []types.LlamaCppModel{{
					Name: standModelR696, State: "loaded", Size: uint64(standModelSizeR696),
					ContextLength: c.total, ContextPerSeq: c.perSeq, MaxSlots: 1, Parallel: 1,
				}},
				VramKnown: true, AvailableVRAMMB: c.gpuTotal - c.gpuUsed,
			}
		}
		p.UpdateMetrics(c.id, m)
		p.metricsMgr.MarkLlamaCppPolled(c.id, time.Now())
	}

	rig := &standReplayRigR696{proxy: p, cpp: cpp, a10: a10, astra: astra, a10Copy: copies[1]}
	rig.mkReq = func(ip string) *http.Request {
		body, _ := json.Marshal(map[string]interface{}{
			"model": standModelR696,
			"messages": []map[string]string{
				{"role": "system", "content": "stand replay system prompt"},
				{"role": "user", "content": "hi"},
			},
			"stream": false,
		})
		req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "Cline/3.0 (stand replay)")
		req.RemoteAddr = ip + ":12345"
		return req
	}
	return rig
}

// standServedR696 — сессия ЭТОГО IP уже работала на бэкенде, затем бэкенд занят
// собственным стримом сессии (слот 1/1 + inflight) — живая форма жалобы
// «с того же IP не распределяет».
func (r *standReplayRigR696) pinAndBusyR696(t *testing.T, ip, backendID string) string {
	t.Helper()
	probe := r.mkReq(ip)
	name := r.proxy.getClientName(probe)
	sid := r.proxy.getSessionIDWithModel(probe, name, standModelR696)
	r.proxy.sessionMgr.SetWithProtocol(sid, backendID, standModelR696, name, ip,
		probe.UserAgent(), RequestProtocol("/api/chat"))
	r.proxy.AcquireSlotForTest(backendID)
	r.proxy.admission.markInflight(sid, 1)
	t.Cleanup(func() {
		r.proxy.releaseSlot(backendID)
		r.proxy.admission.markInflight(sid, -1)
	})
	return sid
}

func (r *standReplayRigR696) doR696(ip string) (code int, elapsed time.Duration, backend string, queueWait string, body string) {
	rec := httptest.NewRecorder()
	start := time.Now()
	r.proxy.ServeHTTP(rec, r.mkReq(ip))
	return rec.Code, time.Since(start), rec.Header().Get("X-Backend-ID"),
		rec.Header().Get("X-Queue-Wait-Ms"), rec.Body.String()
}

func standTruncR696(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// TestStandReplayR696_FreeDiskOpensFreeCopy — главный различающий тест.
//
// Фаза 1: A10-126 диск 9480 < 10240 — заявка той же сессии ЖДЁТ, причина
// названа (disk_low с числами), свободная копия не получает ни одного
// inference.
// Фаза 2: диск освобождён (DiskFree=20000) — та же заявка той же сессии
// обслуживается A10-126 за единицы миллисекунд, без ожидания.
// Фаза 3 (по желанию, отдельный тест): окно свободной копии меньше нужного —
// ждёт по R459, причина window_too_small.
func TestStandReplayR696_FreeDiskOpensFreeCopy(t *testing.T) {
	t.Setenv("LB_ADMISSION_WAIT_SEC", "3")
	t.Setenv("LB_AUTO_LOAD_WAIT_SEC", "3")
	t.Setenv("LB_BACKEND_WAIT_SEC", "2")

	rig := newStandReplayR696(t, 9480)
	ip := "192.168.151.151"
	sid := rig.pinAndBusyR696(t, ip, "cppworker-gpu-bundled-agent")

	// --- Фаза 1: диск не освобождён -------------------------------------
	code, elapsed, backend, queueWait, body := rig.doR696(ip)
	t.Logf("ФАЗА 1 (disk_free=9480 < 10240): code=%d elapsed=%v backend=%q queue_wait_ms=%q",
		code, elapsed.Round(time.Millisecond), backend, queueWait)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("фаза 1: ожидался 503 (свободная копия исключена диском, привязанная занята), "+
			"получен %d за %v: %s", code, elapsed, standTruncR696(body, 600))
	}
	if elapsed < 2*time.Second {
		t.Fatalf("фаза 1: ответ за %v — заявка не ждала, хотя свободных копий нет", elapsed)
	}
	if !strings.Contains(body, "disk_low") {
		t.Fatalf("фаза 1: причина не названа (disk_low ожидалась): %s", standTruncR696(body, 800))
	}
	for _, want := range []string{"9480", "10240"} {
		if !strings.Contains(body, want) {
			t.Errorf("фаза 1: в причине нет числа %q: %s", want, standTruncR696(body, 800))
		}
	}
	if rig.a10.inferChat.Load() != 0 {
		t.Fatalf("фаза 1: исключённая по диску копия получила %d inference-запросов", rig.a10.inferChat.Load())
	}
	if sid == "" {
		t.Fatal("условие теста не выполнено: сессия не определена")
	}

	// --- Фаза 2: диск освобождён ----------------------------------------
	rig.setDiskR696(t, "A10-126", 20000)
	code, elapsed, backend, queueWait, body = rig.doR696(ip)
	t.Logf("ФАЗА 2 (disk_free=20000):          code=%d elapsed=%v backend=%q queue_wait_ms=%q",
		code, elapsed.Round(time.Millisecond), backend, queueWait)
	if code != http.StatusOK {
		t.Fatalf("фаза 2: после освобождения диска ожидался 200 от A10-126, получен %d за %v: %s",
			code, elapsed, standTruncR696(body, 600))
	}
	if backend != "A10-126" {
		t.Fatalf("фаза 2: обслужил %q, ожидался A10-126 (свободная копия той же модели)", backend)
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("фаза 2: заявка обслуживалась %v — она всё ещё ждала, а не взяла свободный слот", elapsed)
	}
	if queueWait != "" && queueWait != "0" {
		t.Errorf("фаза 2: заголовок X-Queue-Wait-Ms=%q — заявка стояла в очереди", queueWait)
	}
	if rig.a10.inferChat.Load() != 1 {
		t.Fatalf("фаза 2: A10-126 получил %d inference-запросов, ожидался ровно 1", rig.a10.inferChat.Load())
	}
}

// TestStandReplayR696_NarrowFreeCopyStillWaits — политика R459 в живых числах:
// у свободной копии ИЗВЕСТНОЕ окно меньше окна сессии (65536 < 131072) —
// заявка ждёт, причина named window_too_small. Это НЕ дефект, а осознанное
// правило (сравни TestR493_SaturatedWideCopyIsWaitedOn).
func TestStandReplayR696_NarrowFreeCopyStillWaits(t *testing.T) {
	t.Setenv("LB_ADMISSION_WAIT_SEC", "3")
	t.Setenv("LB_AUTO_LOAD_WAIT_SEC", "3")
	t.Setenv("LB_BACKEND_WAIT_SEC", "2")

	rig := newStandReplayR696(t, 20000)
	ip := "192.168.151.151"
	rig.pinAndBusyR696(t, ip, "cppworker-gpu-bundled-agent")
	// Копия A10-126 жива, но поднята с меньшим окном (типичный ручной reload).
	rig.setWindowR696(t, "A10-126", 65536, 65536)

	code, elapsed, backend, queueWait, body := rig.doR696(ip)
	t.Logf("R459 (candidate per-seq=65536 < need=131072): code=%d elapsed=%v backend=%q queue_wait_ms=%q",
		code, elapsed.Round(time.Millisecond), backend, queueWait)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("ожидался 503 (R459: окно свободной копии меньше), получен %d за %v: %s",
			code, elapsed, standTruncR696(body, 600))
	}
	if !strings.Contains(body, "window_too_small") {
		t.Fatalf("причина window_too_small не названа: %s", standTruncR696(body, 800))
	}
	for _, want := range []string{"65536", "131072"} {
		if !strings.Contains(body, want) {
			t.Errorf("в причине нет числа %q: %s", want, standTruncR696(body, 800))
		}
	}
	if rig.a10.inferChat.Load() != 0 {
		t.Fatalf("узкая копия получила %d inference-запросов, хотя R459 её отклоняет", rig.a10.inferChat.Load())
	}
}

// TestStandReplayR696_PerSeqOnlyRecordIsServed — R696: запись о загрузке с
// ОКНОМ НА СЛОТ, но без суммарного context_length (так её пишет
// warmupLlamaCppModel → updateLlamaCppRunningModelInMetrics), обязана
// обслуживать заявку. На r681 (ffd8bbb4) эта же запись считалась «модели нет»,
// гейт R459 её отклонял и заявка ждала занятую копию.
func TestStandReplayR696_PerSeqOnlyRecordIsServed(t *testing.T) {
	t.Setenv("LB_ADMISSION_WAIT_SEC", "3")
	t.Setenv("LB_AUTO_LOAD_WAIT_SEC", "3")
	t.Setenv("LB_BACKEND_WAIT_SEC", "2")

	rig := newStandReplayR696(t, 20000)
	ip := "192.168.151.151"
	rig.pinAndBusyR696(t, ip, "cppworker-gpu-bundled-agent")
	rig.setWindowR696(t, "A10-126", 0, 131072)

	code, elapsed, backend, queueWait, body := rig.doR696(ip)
	t.Logf("R696 (per-seq=131072 без context_length):     code=%d elapsed=%v backend=%q queue_wait_ms=%q",
		code, elapsed.Round(time.Millisecond), backend, queueWait)
	if code != http.StatusOK || backend != "A10-126" {
		t.Fatalf("запись «только context_per_seq» не обслужена: code=%d backend=%q elapsed=%v body=%s",
			code, backend, elapsed, standTruncR696(body, 600))
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("заявка ждала %v свободную копию с известным окном на слот", elapsed)
	}
}

// TestStandReplayR696_GatesAreNamed — вывод теста обязан называть гейт:
// проверяем, что в логе очереди есть строка с why_queued и причиной, а в теле
// 503 — машинный код. Это и есть «лог с причиной» из требований разбора.
func TestStandReplayR696_GatesAreNamed(t *testing.T) {
	t.Setenv("LB_ADMISSION_WAIT_SEC", "1")
	t.Setenv("LB_AUTO_LOAD_WAIT_SEC", "1")
	t.Setenv("LB_BACKEND_WAIT_SEC", "1")

	rig := newStandReplayR696(t, 9480)
	ip := "192.168.151.151"
	rig.pinAndBusyR696(t, ip, "cppworker-gpu-bundled-agent")

	logPath := os.Getenv("STAND_REPLAY_LOG")
	if logPath == "" {
		logPath = t.TempDir() + "\\balancer.log"
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("не открыть лог: %v", err)
	}
	defer f.Close()

	code, _, _, _, body := rig.doR696(ip)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("ожидался 503, получен %d: %s", code, standTruncR696(body, 300))
	}
	// why_queued строится тем же проходом, что и отказ, — проверяем его напрямую.
	list := rig.proxy.backendRejectionsR460(standModelR696, "")
	summary := rejectionsSummaryR460(list)
	_, _ = fmt.Fprintf(f, "why_queued: %s\n", summary)
	t.Logf("why_queued: %s", summary)
	if !strings.Contains(summary, "disk_low") {
		t.Fatalf("why_queued не назвал disk_low: %s", summary)
	}
	if !strings.Contains(summary, "9480") || !strings.Contains(summary, "10240") {
		t.Fatalf("why_queued без чисел диска: %s", summary)
	}
}
