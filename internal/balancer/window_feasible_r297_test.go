package balancer

// window_feasible_r297_test.go — R297 (2026-10-29): проверка выполнимости окна
// ДО разрушительных действий, числа отказа и done_reason при обрыве.
//
// Живые дефекты, которые держат эти тесты:
//  1. reload с невозможным окном выгружал модель и падал — стенд оставался без
//     модели (проверка физики обязана стоять ДО unload: см. api-слой);
//  2. в теле отказа стояло `required=4098` (оценка промпта) при запрошенных
//     32768 — оператор не понимал, чего не хватает;
//  3. при `docker kill` воркера клиент получал `done_reason="truncated"` —
//     «ответ закончился естественно» вместо «соединение умерло».

import (
	"encoding/json"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR297_WindowFeasibility — потолок проверяется по факту, «неизвестно» не
// запрещает загрузку (запрет по догадке дороже: оператор теряет модель).
func TestR297_WindowFeasibility(t *testing.T) {
	// Заявленное оператором окно (профиль) — это и есть потолок.
	p := newCeilingTestProxy(t, "m", &types.LlamaCppModelProfile{ContextLength: 40960}, 32768)
	p.backends = map[string]*BackendState{
		"bk-1": {Backend: &types.Backend{ID: "bk-1", Host: "h", Status: types.StatusHealthy}},
	}

	// Окно не задано — проверять нечего.
	if ok, _, _ := p.WindowFeasibility("bk-1", "m", 0); !ok {
		t.Fatal("requested=0 не должен считаться невозможным")
	}

	// Запрос выше заявленного окна отвергается С ЧИСЛОМ потолка.
	ok, ceiling, why := p.WindowFeasibility("bk-1", "m", 200000)
	if ok {
		t.Fatalf("окно 200000 при заявленном 40960 признано возможным (ceiling=%d, why=%s)", ceiling, why)
	}
	if ceiling != 40960 {
		t.Fatalf("потолок в отказе = %d, want 40960 (заявленное окно)", ceiling)
	}

	// окно ниже потолка — допустимо.
	if ok, _, why := p.WindowFeasibility("bk-1", "m", 4096); !ok {
		t.Fatalf("окно 4096 отвергнуто: %s", why)
	}

	// R267-контракт: env-потолок авто-роста НЕ ограничивает явное действие
	// оператора. Профиля/GGUF нет — потолок неизвестен, значит нельзя отказывать;
	// иначе ручная загрузка на 65536 при LB_NCTX_RELOAD_MAX_N_CTX=32768 получала
	// бы «невозможно» на физически возможной модели.
	pEnvOnly := &Proxy{
		backends: map[string]*BackendState{
			"bk-1": {Backend: &types.Backend{ID: "bk-1", Host: "h", Status: types.StatusHealthy}},
		},
		metricsMgr: NewMetricsManager(),
		config:     &types.LoadBalancerConfig{},
		nctxReload: NewNCtxReloadCoordinator(NCtxReloadConfig{AutoReloadMaxNCtx: 32768}),
	}
	if ok, ceiling, why := pEnvOnly.WindowFeasibility("bk-1", "m", 65536); !ok {
		t.Fatalf("env-потолок авто-роста не должен запрещать ручную загрузку: ceiling=%d why=%s", ceiling, why)
	}

	// Неизвестный бэкенд — не отказываем (неизвестно ≠ нельзя).
	if ok, _, _ := p.WindowFeasibility("no-such", "m", 200000); !ok {
		t.Fatal("при неизвестном бэкенде нельзя отказывать по догадке")
	}
	// nil-прокси не паникует.
	var np *Proxy
	if ok, _, _ := np.WindowFeasibility("bk-1", "m", 200000); !ok {
		t.Fatal("nil-proxy должен считать окно возможным")
	}
}

// TestR297_ContextGrowthNumbersRequestedFloor — «нужно» учитывает ЯВНО запрошенное
// клиентом окно, а не только оценку промпта.
func TestR297_ContextGrowthNumbersRequestedFloor(t *testing.T) {
	p := &Proxy{
		backends: map[string]*BackendState{
			"bk-1": {Backend: &types.Backend{ID: "bk-1", Host: "h", Status: types.StatusHealthy}},
		},
		metricsMgr: NewMetricsManager(),
		config:     &types.LoadBalancerConfig{},
	}
	p.cacheLoadedContextLength("bk-1", "m", 16384)

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"options":{"num_ctx":32768}}`)
	est, loaded, needed := p.contextGrowthNumbers("bk-1", "m", body, "/api/chat")
	if loaded != 16384 {
		t.Fatalf("loaded = %d, want 16384", loaded)
	}
	if needed != 32768 {
		t.Fatalf("needed = %d (est=%d), want 32768 — запрошенное окно обязано быть нижней границей", needed, est)
	}

	// Загруженное окно неизвестно → числа не выдумываем (прежний контракт).
	p2 := &Proxy{backends: map[string]*BackendState{}, metricsMgr: NewMetricsManager(),
		config: &types.LoadBalancerConfig{}}
	if _, loaded, needed := p2.contextGrowthNumbers("nope", "m", body, "/api/chat"); loaded != 0 || needed != 0 {
		t.Fatalf("при неизвестном окне числа выдуманы: loaded=%d needed=%d", loaded, needed)
	}
}

// TestR297_BlockerWhenReloadPending — «нет стримов» ≠ «рост возможен»: если
// перезагрузка уже идёт, запрос всё равно откладывается (живой факт: 40.9 с
// ожидания и тело с `active streams=0`).
func TestR297_BlockerWhenReloadPending(t *testing.T) {
	p := &Proxy{
		backends: map[string]*BackendState{
			"bk-1": {Backend: &types.Backend{ID: "bk-1", Host: "h", Status: types.StatusHealthy}},
		},
		metricsMgr: NewMetricsManager(),
		config:     &types.LoadBalancerConfig{},
		nctxReload: NewNCtxReloadCoordinator(NCtxReloadConfig{}),
	}
	p.cacheLoadedContextLength("bk-1", "m", 16384)

	meta := &RequestMeta{EstimatedPromptTokens: 100, RequestedNCtxOverride: 32768}

	// Перезагрузка не идёт и стримов нет — не вмешиваемся.
	if blocked, _, _ := p.contextGrowthBlocker("bk-1", "m", meta); blocked {
		t.Fatal("не должны блокировать, когда рост возможен")
	}

	// Помечаем перезагрузку как идущую — теперь блокируем даже без стримов.
	release, ok := p.nctxReload.BeginExternalReload("bk-1", "m")
	if !ok {
		t.Fatal("не удалось пометить перезагрузку идущей")
	}
	defer release()
	blocked, needed, loaded := p.contextGrowthBlocker("bk-1", "m", meta)
	if !blocked {
		t.Fatal("при идущей перезагрузке запрос обязан откладываться, а не ждать слот")
	}
	if needed != 32768 || loaded != 16384 {
		t.Fatalf("числа отказа: needed=%d loaded=%d, want 32768/16384", needed, loaded)
	}
}

// TestR297_AbortDoneReasonMapping — обрыв воркера это ERROR, а не «truncated».
func TestR297_AbortDoneReasonMapping(t *testing.T) {
	// SSE: finish_reason терминатора.
	frames := abortFramesR296("/v1/chat/completions", "m", streamAbortUpstreamError)
	if len(frames) != 2 {
		t.Fatalf("SSE: кадров %d, want 2 (причина + терминатор)", len(frames))
	}
	var term map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(strings.TrimSpace(string(frames[1])), "data: ")), &term); err != nil {
		t.Fatalf("SSE терминатор не JSON: %v", err)
	}
	choices, _ := term["choices"].([]interface{})
	if len(choices) == 0 {
		t.Fatalf("SSE терминатор без choices: %s", string(frames[1]))
	}
	if fr, _ := choices[0].(map[string]interface{})["finish_reason"].(string); fr != "error" {
		t.Fatalf("SSE finish_reason = %q, want error", fr)
	}

	// NDJSON: done_reason терминатора.
	nframes := abortFramesR296("/api/chat", "m", streamAbortUpstreamEOF)
	if len(nframes) != 2 {
		t.Fatalf("NDJSON: кадров %d, want 2", len(nframes))
	}
	var term2 map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(nframes[1]))), &term2); err != nil {
		t.Fatalf("NDJSON терминатор не JSON: %v", err)
	}
	if dr, _ := term2["done_reason"].(string); dr != "error" {
		t.Fatalf("NDJSON done_reason = %q, want error", dr)
	}
	if done, _ := term2["done"].(bool); !done {
		t.Fatal("NDJSON терминатор обязан быть done:true")
	}

	// Наш лимит строки — по-прежнему «обрезано», а не ошибка.
	lframes := abortFramesR296("/api/chat", "m", streamAbortOurLimit)
	var term3 map[string]interface{}
	_ = json.Unmarshal([]byte(strings.TrimSpace(string(lframes[len(lframes)-1]))), &term3)
	if dr, _ := term3["done_reason"].(string); dr != "length" {
		t.Fatalf("our_limit done_reason = %q, want length", dr)
	}
}
