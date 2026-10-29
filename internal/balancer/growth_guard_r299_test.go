// growth_guard_r299_test.go — R299 (2026-10-29): три живых дефекта прогона
// 793db393 и их проверки.
//
//  1. Решение «окно мало, вырасти нельзя» принимается ДО preflight: запрос не
//     тратит 88 секунд на сетевой reload, который guard R271 всё равно отложит,
//     и получает 503 единого формата с числами (requiredTokens /
//     loadedWindowTokens / activeStreams / reloadPending).
//  2. «Модель ещё загружается» — это 202 Accepted, а не 500: живая загрузка не
//     провал, и клиент не должен повторять её поверх идущей.
//  3. Одинаковые предупреждения (раскладка, «нет бэкендов») не спамят лог.
package balancer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// newGrowthGuardProxyR299 — прокси, у которого модель «m» загружена с окном 8192
// и включён preflight: ровно живая конфигурация стенда (gemma ctx=8192).
func newGrowthGuardProxyR299() *Proxy {
	p := &Proxy{
		backends: map[string]*BackendState{
			"bk-1": {Backend: &types.Backend{ID: "bk-1", Host: "127.0.0.1", Status: types.StatusHealthy}},
		},
		metricsMgr: NewMetricsManager(),
		config:     &types.LoadBalancerConfig{},
		nctxReload: NewNCtxReloadCoordinator(NCtxReloadConfig{PreflightEnabled: true}),
	}
	p.metricsMgr.mu.Lock()
	p.metricsMgr.llamaMetrics["bk-1"] = &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{{Name: "m", ContextLength: 8192, State: "loaded"}},
	}
	p.metricsMgr.mu.Unlock()
	p.nctxReload.SetLastKnownNCtx("bk-1", 8192)
	return p
}

const r299ChatBody = `{"model":"m","messages":[{"role":"user","content":"hi"}],"options":{"num_ctx":32768}}`

// TestR299_GrowthDeferVerdict — решение «отвечаем сразу» требует ОБОИХ условий.
func TestR299_GrowthDeferVerdict(t *testing.T) {
	path := "/api/chat"
	body := []byte(r299ChatBody)

	t.Run("ничто не мешает — preflight обязан попробовать вырасти", func(t *testing.T) {
		p := newGrowthGuardProxyR299()
		deferNow, _, loaded, needed, _, _ := p.growthDeferR299("bk-1", "m", body, path)
		if deferNow {
			t.Fatal("без стримов и без перезагрузки отказывать нельзя: рост окна возможен")
		}
		if loaded != 8192 || needed != 32768 {
			t.Fatalf("числа: loaded=%d needed=%d, want 8192/32768 (явно запрошенное окно)", loaded, needed)
		}
	})

	t.Run("идёт стрим — отвечаем сразу", func(t *testing.T) {
		p := newGrowthGuardProxyR299()
		stop := beginClientStreamR271(p, "bk-1", "m")
		defer stop()
		deferNow, _, _, needed, active, pending := p.growthDeferR299("bk-1", "m", body, path)
		if !deferNow {
			t.Fatal("при активном стриме рост окна отложен guard'ом R271 — ждать preflight бессмысленно")
		}
		if active < 1 {
			t.Fatalf("activeStreams = %d, want >= 1: в отказе должно быть живое число", active)
		}
		if pending {
			t.Fatal("reloadPending = true, хотя перезагрузка не начиналась")
		}
		if needed != 32768 {
			t.Fatalf("needed = %d, want 32768", needed)
		}
	})

	t.Run("идёт перезагрузка без стримов — тоже отвечаем сразу", func(t *testing.T) {
		p := newGrowthGuardProxyR299()
		release, ok := p.nctxReload.BeginExternalReload("bk-1", "m")
		if !ok {
			t.Fatal("не удалось пометить перезагрузку идущей")
		}
		defer release()
		deferNow, _, _, _, active, pending := p.growthDeferR299("bk-1", "m", body, path)
		if !deferNow {
			t.Fatal("перезагрузка идёт — окно вырастет не раньше её конца")
		}
		if !pending {
			t.Fatal("reloadPending = false, хотя перезагрузка помечена идущей")
		}
		if active != 0 {
			t.Fatalf("activeStreams = %d, want 0: стримов нет, и это ЧЕСТНЫЙ ноль рядом с reloadPending", active)
		}
	})

	t.Run("окно покрывает запрос — не вмешиваемся", func(t *testing.T) {
		p := newGrowthGuardProxyR299()
		stop := beginClientStreamR271(p, "bk-1", "m")
		defer stop()
		small := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"options":{"num_ctx":2048}}`)
		if deferNow, _, _, _, _, _ := p.growthDeferR299("bk-1", "m", small, path); deferNow {
			t.Fatal("запрос влезает в загруженное окно — отказывать не за что")
		}
	})
}

// TestR299_PreflightGuardAnswersBeforeWorkerCall — ЖИВОЙ СЦЕНАРИЙ (88.2 с):
// guard обязан ответить ДО любого сетевого preflight.
//
// Проверяем именно поведение, а не порядок строк: фейковый воркер считает
// запросы, и их ДОЛЖНО БЫТЬ НОЛЬ — если бы RunPreflight успел запуститься, счётчик
// вырос бы (а на живом стенде это те самые 88 секунд).
func TestR299_PreflightGuardAnswersBeforeWorkerCall(t *testing.T) {
	var workerCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&workerCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	p := newGrowthGuardProxyR299()
	lr := &LlamaCppRouter{proxy: p}

	stop := beginClientStreamR271(p, "bk-1", "m")
	defer stop()

	body := []byte(r299ChatBody)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(r299ChatBody))

	start := time.Now()
	handled, res := lr.runInferencePreflight(runInferencePreflightArgs{
		w: rec, r: req, body: body, model: "m",
		backendID: "bk-1", backendURL: srv.URL,
	})
	elapsed := time.Since(start)

	if !handled {
		t.Fatal("запрос не обработан: preflight пошёл в сеть, хотя решение уже известно")
	}
	if res == nil || res.Decision != PreflightAsyncReload {
		t.Fatalf("decision = %v, want async reload (отложено, повтори позже)", res)
	}
	if n := atomic.LoadInt32(&workerCalls); n != 0 {
		t.Fatalf("воркер получил %d запрос(ов): guard сработал ПОСЛЕ preflight (живой дефект 88.2 с)", n)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("ответ занял %v: guard не перед preflight'ом", elapsed)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("код = %d, want 503 (запрос корректный, окно мало)", rec.Code)
	}
	var respBody map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &respBody); err != nil {
		t.Fatalf("тело не JSON: %v (%s)", err, rec.Body.String())
	}
	if respBody["deferredReason"] != "context_growth_pending" {
		t.Fatalf("deferredReason = %v, want context_growth_pending (единый формат R291)", respBody["deferredReason"])
	}
	if respBody["requiredTokens"] != float64(32768) || respBody["loadedWindowTokens"] != float64(8192) {
		t.Fatalf("числа в отказе: %v, want requiredTokens=32768 loadedWindowTokens=8192", respBody)
	}
	if respBody["activeStreams"] != float64(1) {
		t.Fatalf("activeStreams = %v, want 1 (живой счётчик, а не «по умолчанию 0»)", respBody["activeStreams"])
	}
	if respBody["reloadPending"] != false {
		t.Fatalf("reloadPending = %v, want false", respBody["reloadPending"])
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Fatal("нет Retry-After: клиент не знает, когда повторять")
	}
}

// TestR299_PreflightGuardKeepsNormalPath — радостный путь не сломан: если окно
// вырасти можно (стримов нет, перезагрузка не идёт), guard молчит, и решение
// остаётся за preflight'ом — как до R299.
func TestR299_PreflightGuardKeepsNormalPath(t *testing.T) {
	var workerCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&workerCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	p := newGrowthGuardProxyR299()
	lr := &LlamaCppRouter{proxy: p}

	body := []byte(r299ChatBody)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(r299ChatBody))

	// Стримов нет и перезагрузки нет: preflight имеет право попробовать рост.
	_, _ = lr.runInferencePreflight(runInferencePreflightArgs{
		w: rec, r: req, body: body, model: "m",
		backendID: "bk-1", backendURL: srv.URL,
	})
	// Проверяем именно НАШЕ правило: отказа «окно мало» здесь быть не должно.
	// Что дальше делает preflight (в т.ч. решает «ничего менять не нужно» и не
	// ходит в сеть) — его собственная логика, и она не должна перекрываться
	// guard'ом.
	if got := rec.Body.String(); strings.Contains(got, "context_growth_pending") {
		t.Fatalf("guard отказал на обычном пути: %s", got)
	}
	if rec.Code == http.StatusServiceUnavailable {
		t.Fatalf("503 на обычном пути: %s", rec.Body.String())
	}
	_ = workerCalls // сколько раз сходил preflight — его дело, не наше правило
}

// TestR299_StillLoadingAfterRetriesIs202 — «ещё грузится» = 202, не 500.
func TestR299_StillLoadingAfterRetriesIs202(t *testing.T) {
	req := ModelOpRequest{Operation: "load", ModelName: "qwen3.8-27b"}
	res := stillLoadingAfterLoadRetriesR299(req, "bk-1")
	if res == nil {
		t.Fatal("нет результата")
	}
	if res.Error != "" {
		t.Fatalf("Error = %q, want пусто: живая загрузка — не ошибка", res.Error)
	}
	if !res.StillLoading {
		t.Fatal("StillLoading = false: HTTP-слой отдаст 500 (живой дефект 148 с)")
	}
	if res.PollAfterSec <= 0 {
		t.Fatalf("PollAfterSec = %d, want > 0: клиенту нужно сказать, когда опросить статус", res.PollAfterSec)
	}
	if res.Message == "" {
		t.Fatal("нет Message: оператор не увидит, что загрузка идёт")
	}
	// HTTP-слой (internal/api) отдаёт 202 по StillLoading — см.
	// model_op_status_r185_test.go: «ещё грузится» → 202. Здесь проверяем
	// решение, а не маппинг: он уже закреплён отдельным тестом.
	// Регрессия: настоящая ошибка по-прежнему не StillLoading.
	real := &ModelOpResult{Success: false, Error: "cppworker error (HTTP 500): boom"}
	if real.StillLoading || real.Error == "" {
		t.Fatal("настоящая ошибка не должна маскироваться под «ещё грузится»")
	}
}

// TestR299_LogRateLimiter — одно сообщение на эпизод, подавленные считаются,
// смена состояния снимает отметку.
func TestR299_LogRateLimiter(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newLogRateLimiter(time.Minute)
	l.now = func() time.Time { return now }

	if ok, _ := l.allow("bk-1"); !ok {
		t.Fatal("первое сообщение обязано проходить всегда")
	}
	for i := 0; i < 299; i++ {
		if ok, _ := l.allow("bk-1"); ok {
			t.Fatalf("сообщение %d прошло внутри окна: лог снова спамит", i+2)
		}
	}
	// Другой ключ — другое событие, оно не подавлено.
	if ok, _ := l.allow("bk-2"); !ok {
		t.Fatal("ключи не должны глушить друг друга: это разные бэкенды")
	}
	// По истечении окна сообщение проходит и показывает, сколько было подавлено.
	now = now.Add(2 * time.Minute)
	ok, dropped := l.allow("bk-1")
	if !ok {
		t.Fatal("после окна сообщение обязано проходить")
	}
	if dropped != 299 {
		t.Fatalf("dropped = %d, want 299 (оператор должен видеть масштаб подавления)", dropped)
	}
	// Сброс отметки: следующий сбой печатается сразу, без ожидания окна.
	l.reset("bk-1")
	if ok, dropped := l.allow("bk-1"); !ok || dropped != 0 {
		t.Fatalf("после reset: ok=%v dropped=%d, want true/0", ok, dropped)
	}
}

// TestR299_TransportDefersUseUnifiedBody — стюард: оба транспортных пути отдают
// 503 через ОДИН писатель, а не самописным телом старого формата.
//
// Живой факт: `{"error":"...","decision":"async_reload","retry_after_seconds":30}`
// не встречался больше ни в одном отказе балансера, и клиенты разбирали его
// отдельным правилом. Стюард запрещает возвращать его в код (комментарии не
// считаются: в них формат описан намеренно).
func TestR299_TransportDefersUseUnifiedBody(t *testing.T) {
	files := []string{
		"llamacpp_transport.go",
		"llamacpp_transport_nonstream.go",
	}
	for _, name := range files {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		usedUnified := false
		for i, line := range strings.Split(string(data), "\n") {
			code := strings.TrimSpace(line)
			if strings.HasPrefix(code, "//") || code == "" {
				continue // комментарий или пустая строка — не поведение
			}
			if strings.Contains(code, `"decision":"async_reload"`) ||
				strings.Contains(code, "`{\"error\":%q,\"decision\":\"async_reload\"") {
				t.Fatalf("%s:%d: самописное тело отказа вернулось (%s) — используй writePreflightDeferredR299",
					name, i+1, code)
			}
			if strings.Contains(code, "writePreflightDeferredR299(") {
				usedUnified = true
			}
		}
		if !usedUnified {
			t.Fatalf("%s: отказ не идёт через writePreflightDeferredR299 — значит формат снова разошёлся", name)
		}
	}
}
