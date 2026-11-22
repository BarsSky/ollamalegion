// window_refuse_no_growth_request_r664_test.go — R664, ШАГ B (2026-11-20):
// «данные не влезают, а окно клиент не заявил» → 413 с числами, а не рост
// вслепую.
//
// ЧТО БЫЛО. Рост окна запускался по НАШЕЙ оценке промпта: клиент не присылал
// num_ctx (Cline не присылает его никогда), балансер выводил «нужное» окно
// (оценка + бюджет вывода + slack), выгружал модель и грузил её заново — минуты
// простоя ради числа, которого никто не просил. Исход зависел от свободной VRAM:
// тот же вход давал то reload, то 503 «подождите».
//
// ЧТО СТАЛО (согласовано с заказчиком). Основание роста одно — ЗАЯВКА клиента
// (X-Cpp-Ctx, то есть num_ctx в теле; R83/R68). Данные не влезают и заявки нет —
// ответ окончательный и с числами:
//
//	HTTP 413
//	{"error":"context_window_exhausted_no_growth_request",
//	 "prompt_tokens":…, "loaded_window_tokens":…, "required_window_tokens":…, …}
//
// Модель НЕ перезагружается (R294: окно не уменьшаем, чужой ход не рвём), а
// клиент/оператор видит, ЧТО заявить.
package balancer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// stepBBackendR664 — фейковый воркер: перезагрузку принимает и отчитывается
// новым окном, всё остальное — 404 (адаптивной стратегии нет → рост разрешён).
// Счётчик reload'ов — доказательство, что отказ шага B модель НЕ трогал.
func stepBBackendR664(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var reloads int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models/reload" && r.Method == http.MethodPost {
			atomic.AddInt32(&reloads, 1)
			var req struct {
				ContextSize int `json:"contextSize"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"reloaded","context_size":` +
				itoaR664(req.ContextSize) + `}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &reloads
}

// itoaR664 — маленький помощник, чтобы не тянуть strconv в один вызов.
func itoaR664(v int) string {
	if v <= 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

// stepBProxyR664 — стенд шага B: модель «m» загружена окном 8192 на слот,
// физический потолок 262144 (GGUF), preflight включён.
func stepBProxyR664(t *testing.T) (*Proxy, *LlamaCppRouter) {
	t.Helper()
	disableExactTokensR447(t) // считаем по оценке: тест про правило, не про сеть
	p := &Proxy{
		backends: map[string]*BackendState{
			"bk-1": {Backend: &types.Backend{ID: "bk-1", Host: "127.0.0.1", Status: types.StatusHealthy}},
		},
		metricsMgr: NewMetricsManager(),
		config:     &types.LoadBalancerConfig{},
		nctxReload: NewNCtxReloadCoordinator(NCtxReloadConfig{
			AutoReloadNCtx:   true,
			PreflightEnabled: true,
		}),
	}
	p.metricsMgr.mu.Lock()
	p.metricsMgr.llamaMetrics["bk-1"] = &types.LlamaCppMetrics{
		GGUFMaxContext: 262144,
		LoadedModels: []types.LlamaCppModel{{
			Name: "m", State: "loaded",
			ContextLength: 8192, ContextPerSeq: 8192,
			GGUFMaxContext: 262144, MaxSlots: 1,
		}},
	}
	p.metricsMgr.mu.Unlock()
	p.nctxReload.SetLastKnownNCtx("bk-1", 8192)
	return p, &LlamaCppRouter{proxy: p}
}

// runPreflightR664 — прогон через НАСТОЯЩИЙ вход роутера: preflight пишет ответ
// в recorder, и handled=true означает, что клиент получил этот ответ.
func runPreflightR664(t *testing.T, p *Proxy, lr *LlamaCppRouter, backendURL string, body []byte) (bool, *PreflightResult, *httptest.ResponseRecorder) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(string(body)))
	handled, res := lr.runInferencePreflight(runInferencePreflightArgs{
		w: rec, r: req, body: body, model: "m",
		backendID: "bk-1", backendURL: backendURL,
	})
	return handled, res, rec
}

// parseBodyR664 — тело ответа как JSON-объект.
func parseBodyR664(t *testing.T, raw []byte) map[string]interface{} {
	t.Helper()
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("тело ответа не JSON: %v (%s)", err, string(raw))
	}
	return out
}

// assertWindowExhaustedR664 — ОДНА проверка контракта шага B: код ошибки и три
// числа, согласованных между собой.
//
// Согласованность обязательна: отказ существует ради чисел, по которым клиент
// решает, ЧТО заявить. Если required < prompt — заявка не поможет; если
// loaded >= required — окно уже покрывает промпт, и отказ был бы ложью.
func assertWindowExhaustedR664(t *testing.T, raw []byte, wantPrompt int) map[string]interface{} {
	t.Helper()
	out := parseBodyR664(t, raw)
	if got := out["error"]; got != contextWindowExhaustedCodeR664 {
		t.Fatalf("error = %v, want %q (тело: %s)", got, contextWindowExhaustedCodeR664, string(raw))
	}
	prompt := toIntR447(out["prompt_tokens"])
	loaded := toIntR447(out["loaded_window_tokens"])
	required := toIntR447(out["required_window_tokens"])
	if prompt != wantPrompt {
		t.Fatalf("prompt_tokens = %d, want %d (оценка промпта, по которой принято решение)", prompt, wantPrompt)
	}
	if loaded <= 0 {
		t.Fatalf("loaded_window_tokens = %d, want > 0: клиент должен видеть, что загружено", loaded)
	}
	if required < prompt {
		t.Fatalf("required_window_tokens = %d меньше prompt_tokens = %d: такая заявка не поможет", required, prompt)
	}
	if loaded >= required {
		t.Fatalf("loaded_window_tokens = %d не меньше required_window_tokens = %d: окно уже покрывает промпт", loaded, required)
	}
	if got := toIntR447(out["requested_n_ctx"]); got != 0 {
		t.Fatalf("requested_n_ctx = %d, want 0: отказ ровно про ОТСУТСТВИЕ заявки", got)
	}
	for _, key := range []string{"reason", "what_to_do", "model", "backend_id", "available_context_tokens"} {
		if _, ok := out[key]; !ok {
			t.Fatalf("в теле нет поля %q: %s", key, string(raw))
		}
	}
	return out
}

// TestR664_StepB_NoRequestRefusedWithNumbers — ОСНОВНОЙ СЛУЧАЙ ШАГА B: данные не
// влезают, окно клиент не заявил → HTTP 413 с машинным кодом и числами, модель
// НЕ перезагружается.
func TestR664_StepB_NoRequestRefusedWithNumbers(t *testing.T) {
	srv, reloads := stepBBackendR664(t)
	p, lr := stepBProxyR664(t)

	body := r447BodyWithEstimatedPromptR447(t, p, 13023) // 13023 > 8192 загруженных
	handled, res, rec := runPreflightR664(t, p, lr, srv.URL, body)

	if !handled {
		t.Fatal("запрос не обработан: preflight промолчал, хотя данные не влезают и заявки нет")
	}
	if res == nil || res.Decision != PreflightReject {
		t.Fatalf("decision = %+v, want PreflightReject (413)", res)
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("код = %d, want 413: клиент должен отличить «заяви окно» от «подожди»", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After = %q: это ответ «повтори позже», а окно без заявки не вырастет", got)
	}
	if raw := rec.Body.Bytes(); !strings.Contains(string(raw), "context_window_exhausted_no_growth_request") {
		t.Fatalf("в теле нет кода отказа: %s", string(raw))
	}
	out := assertWindowExhaustedR664(t, rec.Body.Bytes(), 13023)
	// Реальное тело — в лог теста: отчёт обязан называть то, что видит клиент.
	t.Logf("ответ клиенту: HTTP %d %s", rec.Code, rec.Body.String())

	// Доступный потолок назван в том же теле: «заявите больше» иначе могло бы
	// оказаться советом заявить больше, чем модель держит.
	if got := toIntR447(out["available_context_tokens"]); got != 262144 {
		t.Fatalf("available_context_tokens = %d, want 262144 (GGUF-потолок на клиента)", got)
	}
	if n := atomic.LoadInt32(reloads); n != 0 {
		t.Fatalf("модель перезагружалась %d раз(а): шаг B — отказ БЕЗ перезагрузки", n)
	}
}

// TestR664_StepB_DeclaredWindowStillGrowsAndServes — ЗАЯВКА ЕСТЬ: рост идёт (шаг
// A), заявка обслуживается, отказа 413 нет — запрос уходит на воркер как обычно
// (клиент получает 200).
func TestR664_StepB_DeclaredWindowStillGrowsAndServes(t *testing.T) {
	srv, reloads := stepBBackendR664(t)
	p, lr := stepBProxyR664(t)

	// Тот же непомещающийся промпт (13023), но клиент заявил окно 16384.
	body := r447BodyWithEstimatedPromptR664(t, p, 13023, 16384)
	handled, res, rec := runPreflightR664(t, p, lr, srv.URL, body)

	if handled {
		t.Fatalf("клиент заявил окно — рост выполняется, отказ недопустим: %s", rec.Body.String())
	}
	if res == nil || res.Decision != PreflightReload {
		t.Fatalf("decision = %+v, want PreflightReload (рост по заявке)", res)
	}
	if res.TargetNCtx != 16384 {
		t.Fatalf("TargetNCtx = %d, want 16384: цель роста — РОВНО заявленное окно", res.TargetNCtx)
	}
	if n := atomic.LoadInt32(reloads); n != 1 {
		t.Fatalf("перезагрузок %d, want 1: заявка клиента — основание роста (R83/R385)", n)
	}
	if rec.Body.Len() != 0 || rec.Code != http.StatusOK {
		t.Fatalf("клиенту написан ответ %d %q: при заявке запрос обслуживается (роутер проксирует дальше)",
			rec.Code, rec.Body.String())
	}
}

// TestR664_StepB_DeclaredAboveCeilingKeepsCeilingRefusal — данные не влезают,
// заявка ЕСТЬ, но выше физического потолка: остаётся СУЩЕСТВУЮЩИЙ отказ по
// потолку (makeWindowTooBigReject), а не новый код шага B.
func TestR664_StepB_DeclaredAboveCeilingKeepsCeilingRefusal(t *testing.T) {
	srv, reloads := stepBBackendR664(t)
	p, lr := stepBProxyR664(t)

	body := r447BodyWithEstimatedPromptR664(t, p, 13023, 400000) // потолок 262144
	handled, res, rec := runPreflightR664(t, p, lr, srv.URL, body)
	if !handled || res == nil || res.Decision != PreflightReject {
		t.Fatalf("declared above ceiling: handled=%t res=%+v, want 413 reject", handled, res)
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("код = %d, want 413", rec.Code)
	}
	out := parseBodyR664(t, rec.Body.Bytes())
	if got := out["error"]; got == contextWindowExhaustedCodeR664 {
		t.Fatalf("заявка выше потолка получила код шага B (%v): это ДРУГАЯ причина — потолок", got)
	}
	if got := toIntR447(out["requested_n_ctx"]); got != 400000 {
		t.Fatalf("requested_n_ctx = %d, want 400000 (числа заявки в отказе по потолку)", got)
	}
	if got := toIntR447(out["backend_ceiling_n_ctx"]); got != 262144 {
		t.Fatalf("backend_ceiling_n_ctx = %d, want 262144", got)
	}
	if n := atomic.LoadInt32(reloads); n != 0 {
		t.Fatalf("перезагрузок %d, want 0: окно выше потолка не поднимаем", n)
	}
}

// TestR664_StepB_FitsWithoutRequestServesAsIs — КОНТРПРИМЕР: данные влезают, окно
// клиент не заявил → обслуживание как есть. Ни отказа, ни роста: шаг B не должен
// превратиться в «отказываем всем, кто не назвал num_ctx».
func TestR664_StepB_FitsWithoutRequestServesAsIs(t *testing.T) {
	srv, reloads := stepBBackendR664(t)
	p, lr := stepBProxyR664(t)

	body := r447BodyWithEstimatedPromptR447(t, p, 900) // 900 < 8192 загруженных
	handled, res, rec := runPreflightR664(t, p, lr, srv.URL, body)

	if handled {
		t.Fatalf("влезающий запрос не должен получать ответ preflight'а: %d %s",
			rec.Code, rec.Body.String())
	}
	if res == nil || res.Decision != PreflightNoOp {
		t.Fatalf("decision = %+v, want PreflightNoOp (обслуживаем загруженным окном)", res)
	}
	if n := atomic.LoadInt32(reloads); n != 0 {
		t.Fatalf("перезагрузок %d, want 0: данные влезают, роста не нужно", n)
	}
}

// r447BodyWithEstimatedPromptR664 — тело /api/chat с заданной оценкой промпта и
// заявленным клиентом окном numCtx (0 — не заявлять).
func r447BodyWithEstimatedPromptR664(t *testing.T, p *Proxy, wantTokens, numCtx int) []byte {
	t.Helper()
	base := r447BodyWithEstimatedPromptR447(t, p, wantTokens)
	if numCtx <= 0 {
		return base
	}
	// Вставляем options.num_ctx в уже разобранное тело — так число заявки
	// остаётся ровно тем, что в тесте, а длина промпта не меняется.
	return []byte(strings.Replace(string(base), `"messages"`,
		`"options":{"num_ctx":`+itoaR664(numCtx)+`},"messages"`, 1))
}
