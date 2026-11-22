// context_window_reject_parity_test.go — R679 (2026-11-20, живой прогон матрицы
// `balancer:r261120-r679`): тело 413 «данные не влезают, окна клиент не заявил»
// обязано называть ОДНИ И ТЕ ЖЕ числа на стриминговом пути и на пути
// preflight-движка.
//
// ЖИВОЙ ОТВЕТ (загружено 8192 на слот, промпт 13002 токена):
//
//	{"error":"context_window_exhausted_no_growth_request",
//	 "prompt_tokens":13035, "loaded_window_tokens":8192,
//	 "required_window_tokens":13824, "requested_n_ctx":0,     ← верно
//	 "physical_ceiling_tokens":0, "gguf_max_context":0,
//	 "auto_reload_max_n_ctx":0}                                ← ноль вместо 65536/131072/65536
//
// Причина по коду: транспортный (стриминговый) путь передавал в общий сборщик
// тела `contextWindowExhaustedBodyR664` нули вместо реальных потолков
// (nctx_reload_preflight.go), тогда как preflight-движок передавал настоящие
// (preflight_nctx_reject.go). Отказ существует РАДИ ЧИСЕЛ: по ним клиент решает,
// какое окно заявить, а по «available_context_tokens=0» решение «заявить больше»
// невозможно — ноль читается как «заявить нельзя ничего».
//
// ПРОВЕРКА. Одна фикстура (модель 8192 на слот, GGUF 131072, операторский
// потолок 65536 — числа живого стенда) и одно тело с одинаковым промптом
// прогоняются через ОБА пути; тела сравниваются как структуры (значимые поля —
// побайтово через JSON-маршалинг: порядок ключей у map[string]interface{}
// детерминирован, поэтому равенство структур означает и равенство байтов).
package balancer

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// parityProxyR679 — стенд живого замера: модель «m» загружена 8192 на слот,
// GGUF-потолок модели 131072, операторский потолок авто-роста 65536.
func parityProxyR679(t *testing.T) (*Proxy, *LlamaCppRouter) {
	t.Helper()
	disableExactTokensR447(t) // считаем по оценке: тест про числа тела, не про сеть
	p := &Proxy{
		backends: map[string]*BackendState{
			"bk-1": {Backend: &types.Backend{ID: "bk-1", Host: "127.0.0.1", Status: types.StatusHealthy}},
		},
		metricsMgr: NewMetricsManager(),
		config:     &types.LoadBalancerConfig{},
		nctxReload: NewNCtxReloadCoordinator(NCtxReloadConfig{
			AutoReloadNCtx:    true,
			PreflightEnabled:  true,
			AutoReloadMaxNCtx: 65536, // LB_NCTX_RELOAD_MAX_N_CTX живого стенда
		}),
	}
	p.metricsMgr.mu.Lock()
	p.metricsMgr.llamaMetrics["bk-1"] = &types.LlamaCppMetrics{
		GGUFMaxContext: 131072,
		LoadedModels: []types.LlamaCppModel{{
			Name: "m", State: "loaded",
			ContextLength: 8192, ContextPerSeq: 8192,
			GGUFMaxContext: 131072, MaxSlots: 1,
		}},
	}
	p.metricsMgr.mu.Unlock()
	p.nctxReload.SetLastKnownNCtx("bk-1", 8192)
	return p, &LlamaCppRouter{proxy: p}
}

// streamingBodyR679 — тело /api/chat с оценкой промпта wantTokens и стримингом
// (стриминговость нужна, чтобы запрос пошёл стриминговым путём вердикта R301;
// на числа отказа она не влияет — они считаются по промпту и загруженному окну).
func streamingBodyR679(t *testing.T, p *Proxy, wantTokens int) []byte {
	t.Helper()
	base := r447BodyWithEstimatedPromptR447(t, p, wantTokens)
	body := []byte(strings.Replace(string(base), `"messages"`, `"stream":true,"messages"`, 1))
	meta := p.extractRequestMeta(body, "/api/chat")
	if meta == nil {
		t.Fatalf("тело не разобралось как /api/chat: %s", string(body))
	}
	if meta.EstimatedPromptTokens != wantTokens {
		t.Fatalf("оценка промпта после вставки stream = %d, want %d", meta.EstimatedPromptTokens, wantTokens)
	}
	return body
}

// TestR679_RefuseBodyHasRealCeilingsOnBothPaths — главная проверка: на
// стриминговом пути в тело 413 попадают РЕАЛЬНЫЕ потолки, и тело совпадает с
// телом preflight-движка.
func TestR679_RefuseBodyHasRealCeilingsOnBothPaths(t *testing.T) {
	srv, reloads := stepBBackendR664(t)
	p, lr := parityProxyR679(t)

	const promptTokens = 13023 // > 8192 загруженных, заявки нет
	body := streamingBodyR679(t, p, promptTokens)

	// 1. СТРИМИНГОВЫЙ путь: вердикт R301 в транспортном preflight.
	_, needsProxy, msg, status := p.preflightNCtxReloadIfNeeded(
		context.Background(), "bk-1", "m", body, "/api/chat")
	if needsProxy {
		t.Fatalf("данные не влезают и заявки нет — запрос не должен уходить на воркер (status=%d)", status)
	}
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("код стримингового отказа = %d, want 413 (тело: %s)", status, msg)
	}
	streamOut := parseBodyR664(t, []byte(msg))
	if got := streamOut["error"]; got != contextWindowExhaustedCodeR664 {
		t.Fatalf("error = %v, want %q: стриминговый путь обязан отдавать шаг B, а не другой отказ (%s)",
			got, contextWindowExhaustedCodeR664, msg)
	}
	t.Logf("413 стримингового пути: %s", msg)

	// Реальные числа живого стенда: потолок на клиента 65536, GGUF 131072,
	// операторский потолок 65536. Ровно они видны в логе maxNumCtxForModel.
	if got := toIntR447(streamOut["physical_ceiling_tokens"]); got != 65536 {
		t.Fatalf("physical_ceiling_tokens = %d, want 65536 (min(GGUF 131072, операторский 65536)) — "+
			"клиент по нулю не может решить, что заявить", got)
	}
	if got := toIntR447(streamOut["gguf_max_context"]); got != 131072 {
		t.Fatalf("gguf_max_context = %d, want 131072 (реальный GGUF модели)", got)
	}
	if got := toIntR447(streamOut["auto_reload_max_n_ctx"]); got != 65536 {
		t.Fatalf("auto_reload_max_n_ctx = %d, want 65536 (операторский потолок авто-роста)", got)
	}
	if got := toIntR447(streamOut["available_context_tokens"]); got != 65536 {
		t.Fatalf("available_context_tokens = %d, want 65536: числа одного тела не согласованы", got)
	}

	// 2. Путь preflight-движка (он же — эталон чисел: там состояние собрано).
	handled, res, rec := runPreflightR664(t, p, lr, srv.URL, body)
	if !handled || res == nil || res.Decision != PreflightReject {
		t.Fatalf("preflight-движок не отказал: handled=%t res=%+v", handled, res)
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("код отказа движка = %d, want 413", rec.Code)
	}
	engineOut := parseBodyR664(t, rec.Body.Bytes())
	t.Logf("413 preflight-движка: %s", rec.Body.String())

	// 3. ОДИН источник правды: тела совпадают по всем полям.
	if diff := diffBodiesR679(engineOut, streamOut); diff != "" {
		t.Fatalf("тела 413 разошлись (%s)\nдвижок:      %s\nстриминг:    %s",
			diff, rec.Body.String(), msg)
	}
	// Ни одна сторона ничего не перезагружала: шаг B — отказ БЕЗ перезагрузки (R294).
	if n := intReloadsR679(reloads); n != 0 {
		t.Fatalf("перезагрузок %d, want 0: отказ шага B модель не трогает", n)
	}
}

// TestR679_RefuseBodyCeilingsUnknownStayZero — контрпример: потолок НЕ известен
// (метрик нет) — ноль остаётся нулём. Иначе тест выше можно было бы «починить»
// выдуманным числом, и отказ снова врал бы клиенту.
func TestR679_RefuseBodyCeilingsUnknownStayZero(t *testing.T) {
	disableExactTokensR447(t)
	p := &Proxy{
		backends: map[string]*BackendState{
			"bk-1": {Backend: &types.Backend{ID: "bk-1", Host: "127.0.0.1", Status: types.StatusHealthy}},
		},
		metricsMgr: NewMetricsManager(),
		config:     &types.LoadBalancerConfig{},
		nctxReload: NewNCtxReloadCoordinator(NCtxReloadConfig{PreflightEnabled: true}),
	}
	// Окно загруженной модели известно (иначе решения нет), GGUF — нет.
	p.metricsMgr.mu.Lock()
	p.metricsMgr.llamaMetrics["bk-1"] = &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{{
			Name: "m", State: "loaded", ContextLength: 8192, ContextPerSeq: 8192,
		}},
	}
	p.metricsMgr.mu.Unlock()
	p.nctxReload.SetLastKnownNCtx("bk-1", 8192)

	body := streamingBodyR679(t, p, 13023)
	_, _, msg, status := p.preflightNCtxReloadIfNeeded(context.Background(), "bk-1", "m", body, "/api/chat")
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("код = %d, want 413 (тело: %s)", status, msg)
	}
	out := parseBodyR664(t, []byte(msg))
	if got := toIntR447(out["physical_ceiling_tokens"]); got != 0 {
		t.Fatalf("physical_ceiling_tokens = %d, want 0: потолок неизвестен — число было бы выдумкой", got)
	}
	if got := toIntR447(out["gguf_max_context"]); got != 0 {
		t.Fatalf("gguf_max_context = %d, want 0: воркер GGUF не сообщал", got)
	}
}

// diffBodiesR679 — первое расхождение двух тел 413 строкой ("" — совпали).
//
// Сравниваем ВСЕ поля, а не подмножество: тела собирает один сборщик, и любое
// расхождение означает, что пути снова считают по-разному (класс R493 «два
// мозга»). Порядок ключей в map[string]interface{} при json.Marshal
// детерминирован, поэтому равенство структур = равенство байтов.
func diffBodiesR679(a, b map[string]interface{}) string {
	var keys []string
	for k := range a {
		keys = append(keys, k)
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			keys = append(keys, k)
		}
	}
	for _, k := range keys {
		av, aok := a[k]
		bv, bok := b[k]
		switch {
		case !aok:
			return "поле " + k + " есть только у стримингового пути"
		case !bok:
			return "поле " + k + " есть только у пути движка"
		case !jsonEqualR679(av, bv):
			return "поле " + k + ": движок=" + jsonStrR679(av) + ", стриминг=" + jsonStrR679(bv)
		}
	}
	return ""
}

func jsonEqualR679(a, b interface{}) bool {
	return jsonStrR679(a) == jsonStrR679(b)
}

// jsonStrR679 — значение в сопоставимом виде: числа без «1.3023e+04», которое
// fmt печатает для float64, и без форматирования платформы.
func jsonStrR679(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return "<nil>"
	case float64:
		return strconvR679(int64(t))
	case bool:
		if t {
			return "true"
		}
		return "false"
	case string:
		return t
	default:
		return fmt.Sprintf("%v", t)
	}
}

// strconvR679 — целое без знака процента и без экспоненты.
func strconvR679(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [24]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// intReloadsR679 — счётчик перезагрузок из фикстуры (атомарный int32).
func intReloadsR679(p *int32) int {
	if p == nil {
		return 0
	}
	return int(atomic.LoadInt32(p))
}
