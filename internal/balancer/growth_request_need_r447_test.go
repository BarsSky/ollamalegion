// growth_request_need_r447_test.go — R447 (2026-11-06): окно растят только по
// требованию запроса, и тело отказа называет те же числа, что его текст.
//
// ЖИВОЙ ДЕФЕКТ, который здесь зафиксирован:
//   - короткий запрос («Привет!», 3 токена) на модель с окном 2048 получал 503
//     «needs 4099 tokens of context … retry in 60 seconds» И запускал
//     перезагрузку модели — из-за бюджета вывода (4096), которого промпту не
//     нужно;
//   - исход зависел от свободной VRAM/RAM (рост разрешён только пока физика
//     позволяет), поэтому один и тот же вход давал 200 или 503 — флак
//     `TestChatStreaming` в tests/llamacpp_proxy (красный CI);
//   - числа в теле пересчитывались ПОСЛЕ запуска перезагрузки и расходились с
//     текстом: `loadedWindowTokens=32768` рядом с «the loaded window is 2048»,
//     `retryAfterSec=30` рядом с «retry in 60 seconds».
package balancer

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// toIntR447 — число из разобранного JSON (float64) в int.
func toIntR447(v interface{}) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return -1
}

// newGrowthGuardProxyWindowR447 — прокси с заданным окном загруженной модели.
func newGrowthGuardProxyWindowR447(window int) *Proxy {
	p := newGrowthGuardProxyR299()
	p.metricsMgr.mu.Lock()
	p.metricsMgr.llamaMetrics["bk-1"] = &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{{Name: "m", ContextLength: window, State: "loaded"}},
	}
	p.metricsMgr.mu.Unlock()
	p.nctxReload.SetLastKnownNCtx("bk-1", window)
	return p
}

// r447SmallStreamBody — живое тело короткого запроса: стриминг, ни окна, ни
// бюджета вывода клиент не называл.
const r447SmallStreamBody = `{"model":"m","messages":[{"role":"user","content":"Привет!"}],"stream":true}`

// TestR447_SmallPromptOnSmallWindowIsNotRefused — окно 2048, промпт 3 токена:
// ни роста, ни отказа, ни подмены тела.
func TestR447_SmallPromptOnSmallWindowIsNotRefused(t *testing.T) {
	// Решение не должно зависеть от того, РАЗРЕШЁН ли рост: именно эта
	// зависимость (свободная VRAM) и делала исход случайным.
	for _, growSwitch := range []string{"", "0"} {
		t.Run("grow_switch="+growSwitch, func(t *testing.T) {
			if growSwitch != "" {
				t.Setenv("LB_NCTX_GROW_FOR_STREAM", growSwitch)
			}
			p := newGrowthGuardProxyWindowR447(2048)

			v := p.growthVerdictR301("rid-test", "bk-1", "m", []byte(r447SmallStreamBody), "/api/chat", true)
			if v.Grow {
				t.Fatalf("запущена перезагрузка модели ради промпта в 3 токена: %+v", v)
			}
			if v.Defer {
				t.Fatalf("отказ запросу, который обслуживается загруженным окном: %+v", v)
			}
			if v.Served {
				t.Fatalf("тело подменять не за чем: запрос не просил другого окна: %+v", v)
			}
			if v.Estimated <= 0 || v.Estimated >= v.Loaded {
				t.Fatalf("оценка промпта = %d при окне %d: тело не воспроизводит живой случай",
					v.Estimated, v.Loaded)
			}
			if v.Needed <= v.Loaded {
				t.Fatalf("нужное окно %d должно превышать загруженное %d — иначе тест не про этот дефект",
					v.Needed, v.Loaded)
			}
		})
	}
}

// TestR664_ClientAskedWindowGrowsInBackground — R664 (шаг A): заявка клиента на
// большее окно НЕ отменена, но и не блокирует запрос: обслуживаем сразу, окно —
// в фоне. Старый контракт: Grow=true и ожидание (живой факт: 59.7 с на запрос с
// промптом 15 токенов).
func TestR664_ClientAskedWindowGrowsInBackground(t *testing.T) {
	p := newGrowthGuardProxyR299() // окно 8192, в теле num_ctx=16384

	v := p.growthVerdictR301("rid-test", "bk-1", "m", []byte(r301StreamBody), "/api/chat", true)
	if v.Grow {
		t.Fatalf("R664: рост по заявке не блокирует запрос (Grow=true), получено %+v", v)
	}
	if v.Defer {
		t.Fatalf("R664: заявка клиента — не повод отказать, получено %+v", v)
	}
	if !v.Served {
		t.Fatalf("R664: тело обязано быть приведено к окну слота, получено %+v", v)
	}
}

// TestR447_DeferredBodyUsesVerdictNumbers — тело «отложено» (R291/R299) описывает
// ТО ЖЕ состояние, что и решение, даже если метрики успели измениться.
//
// Вход — заявка клиента, которую путь выполнить не может (рост на стриминге
// выключен выключателем, R472): тут отказ остаётся «повтори позже» с числами. Случай
// «заявки нет вовсе» проверяется рядом (TestR447_RefusalBodyUsesVerdictNumbers) —
// там отказ окончательный, 413.
func TestR447_DeferredBodyUsesVerdictNumbers(t *testing.T) {
	t.Setenv("LB_NCTX_GROW_FOR_STREAM", "0")
	p := newGrowthGuardProxyR299()
	// Длинный промпт + явная заявка клиента: данные не влезают, но основание роста
	// у запроса есть — значит это не шаг B, а «рост невозможен на этом пути».
	body := []byte(strings.Replace(r301LongStreamBody(), `"stream":true`,
		`"stream":true,"options":{"num_ctx":131072}`, 1))

	v := p.growthVerdictR301("rid-test", "bk-1", "m", body, "/api/chat", true)
	if !v.Defer {
		t.Fatalf("ожидался отказ «подождите» с числами, получено %+v", v)
	}
	if v.Refuse {
		t.Fatalf("заявка у запроса ЕСТЬ — это не отказ шага B: %+v", v)
	}

	// Пока клиенту писали ответ, балансер «перезагрузил» модель: метрики уже
	// показывают 32768. Прежний writer пересчитал бы числа — и соврал бы.
	p.metricsMgr.mu.Lock()
	p.metricsMgr.llamaMetrics["bk-1"] = &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{{Name: "m", ContextLength: 32768, State: "loaded"}},
	}
	p.metricsMgr.mu.Unlock()

	ctx := withGrowthNumbersR447(nil, v) // nil-ctx не должен паниковать
	if ctx != nil {
		t.Fatal("withGrowthNumbersR447(nil) обязан вернуть nil")
	}
	ctx = withGrowthNumbersR447(context.Background(), v)

	rec := httptest.NewRecorder()
	writePreflightDeferredR299Ctx(rec, p, ctx, "bk-1", "m", body, "/api/chat",
		growthDeferMessageR301("m", v), growthDeferRetryAfterSecR447)

	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("тело не JSON: %v (%s)", err, rec.Body.String())
	}
	if got := toIntR447(out["loadedWindowTokens"]); got != v.Loaded {
		t.Fatalf("loadedWindowTokens = %d, want %d (число текста) — тело разошлось с причиной",
			got, v.Loaded)
	}
	if got := toIntR447(out["requiredTokens"]); got != v.Needed {
		t.Fatalf("requiredTokens = %d, want %d", got, v.Needed)
	}
	if got := toIntR447(out["retryAfterSec"]); got != growthDeferRetryAfterSecR447 {
		t.Fatalf("retryAfterSec = %d, want %d", got, growthDeferRetryAfterSecR447)
	}
}

// TestR447_OversizedPromptRefusedWithoutDeclaration — R664 (шаг B): промпт,
// который не влезает в окно, больше НЕ растит его сам: без заявки клиента это
// был бы рост под нашу же оценку промпта. Ответ — 413 с числами (сколько в
// промпте, сколько загружено, сколько нужно заявить), модель не трогаем.
func TestR447_OversizedPromptRefusedWithoutDeclaration(t *testing.T) {
	p := newGrowthGuardProxyR299()

	v := p.growthVerdictR301("rid-test", "bk-1", "m", []byte(r301LongStreamBody()), "/api/chat", true)
	if !v.Refuse {
		t.Fatalf("промпт не влезает и окно не заявлено — ожидался отказ с числами, получено %+v", v)
	}
	if v.Grow || v.Defer {
		t.Fatalf("рост/«подождите» на этот вход запрещены: окно без заявки не вырастет (получено %+v)", v)
	}
	if v.Estimated <= v.Loaded {
		t.Fatalf("тест обязан воспроизводить «не влезает»: estimated=%d loaded=%d", v.Estimated, v.Loaded)
	}
	if v.Required < v.Estimated || v.Required <= v.Loaded {
		t.Fatalf("required=%d: заявка меньше промпта (%d) или не больше загруженного (%d) бесполезна",
			v.Required, v.Estimated, v.Loaded)
	}
}

// TestR447_GrowthNeedRule — правило «нужен ли рост» отдельно от прокси.
func TestR447_GrowthNeedRule(t *testing.T) {
	cases := []struct {
		name              string
		estimated, loaded int
		requested         int
		wantNeed          bool
	}{
		{"крошечный промпт, окно не просили", 3, 2048, 0, false},
		{"промпт влезает с запасом, окно не просили", 1000, 2048, 0, false},
		{"промпт влезает, но под ответ меньше минимума", 2000, 2048, 0, true},
		{"промпт съел окно целиком", 2048, 2048, 0, true},
		// ШАГ B: «данные не влезают без заявки» — не немедленный рост, а отказ.
		{"промпт не влезает, окна не просили (шаг B: отказ)", 4096, 2048, 0, false},
		{"промпт не влезает, окно заявлено (рост по заявке)", 4096, 2048, 8192, true},
		{"клиент попросил больше загруженного (R664: рост в фоне, не немедленный)", 3, 2048, 32768, false},
		{"клиент попросил ровно загруженное", 3, 2048, 2048, false},
		{"оценки нет, окно не просили", 0, 2048, 0, false},
		{"оценки нет, клиент попросил больше (R664: рост в фоне)", 0, 2048, 4096, false},
		{"загруженное окно неизвестно", 3, 0, 16384, false},
	}
	for _, tc := range cases {
		need, why := growthNeedR447(tc.estimated, tc.loaded, tc.requested)
		if need != tc.wantNeed {
			t.Fatalf("%s: need = %v, want %v (причина: %s)", tc.name, need, tc.wantNeed, why)
		}
		if why == "" {
			t.Fatalf("%s: причина не названа — оператор не поймёт решения", tc.name)
		}
	}
}

// TestR447_RefusalBodyUsesVerdictNumbers — тело отказа описывает ТО ЖЕ состояние,
// что и решение, даже если метрики успели измениться.
//
// R664 (шаг B): когда окна клиент не заявил, отказ окончательный (413 с числами),
// а не «отложено». Здесь проверяется ТЕЛО: три числа и код берутся из вердикта, а
// не пересчитываются после перезагрузки (иначе клиент увидит окно, которого ещё
// нет, — живой дефект R447).
func TestR447_RefusalBodyUsesVerdictNumbers(t *testing.T) {
	t.Setenv("LB_NCTX_GROW_FOR_STREAM", "0")
	p := newGrowthGuardProxyR299()
	body := []byte(r301LongStreamBody())

	v := p.growthVerdictR301("rid-test", "bk-1", "m", body, "/api/chat", true)
	if !v.Refuse {
		t.Fatalf("ожидался отказ с числами (промпт не влезает, заявки нет), получено %+v", v)
	}

	// Пока клиенту писали ответ, балансер «перезагрузил» модель: метрики уже
	// показывают 32768. Прежний writer пересчитал бы числа — и соврал бы.
	p.metricsMgr.mu.Lock()
	p.metricsMgr.llamaMetrics["bk-1"] = &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{{Name: "m", ContextLength: 32768, State: "loaded"}},
	}
	p.metricsMgr.mu.Unlock()

	// R679: тело собирается из ИМЕНОВАННЫХ чисел (windowRejectNumbersR664), а не
	// из одиннадцати позиционных аргументов — в позиционном вызове пропуск
	// значений (как три нуля на стриминговом пути) не виден ни компилятору, ни
	// читателю.
	raw := contextWindowExhaustedBodyR664(windowRejectNumbersR664{
		BackendID:          "bk-1",
		Model:              "m",
		PromptTokens:       v.Estimated,
		LoadedWindow:       v.Loaded,
		RequiredWindow:     v.Required,
		AvailablePerClient: v.Available,
		Slots:              1,
	})
	var out map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("тело не JSON: %v (%s)", err, raw)
	}
	if got := toIntR447(out["loaded_window_tokens"]); got != v.Loaded {
		t.Fatalf("loaded_window_tokens = %d, want %d (число решения) — тело разошлось с причиной",
			got, v.Loaded)
	}
	if got := toIntR447(out["prompt_tokens"]); got != v.Estimated {
		t.Fatalf("prompt_tokens = %d, want %d", got, v.Estimated)
	}
	if got := toIntR447(out["required_window_tokens"]); got != v.Required {
		t.Fatalf("required_window_tokens = %d, want %d", got, v.Required)
	}
}

// TestR447_GrowBodySaysSameRetryAsText — в тексте «retry in 60 seconds», в теле
// ровно 60 (раньше транспорт писал свои 30).
func TestR447_GrowBodySaysSameRetryAsText(t *testing.T) {
	p := newGrowthGuardProxyR299()
	v := growthVerdictR301{
		Grow: true, Streaming: true, RetryAfterSec: streamGrowthRetryAfterSecR385,
		Estimated: 3, Loaded: 8192, Needed: 16384,
	}
	msg := growthStartsMessageR385("m", v)
	if !strings.Contains(msg, "retry in 60 seconds") {
		t.Fatalf("текст причины не называет срок: %q", msg)
	}

	rec := httptest.NewRecorder()
	writePreflightDeferredR299Ctx(rec, p, withGrowthNumbersR447(context.Background(), v),
		"bk-1", "m", []byte(r301StreamBody), "/api/chat", msg, growthDeferRetryAfterSecR447)

	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("тело не JSON: %v (%s)", err, rec.Body.String())
	}
	if got := toIntR447(out["retryAfterSec"]); got != streamGrowthRetryAfterSecR385 {
		t.Fatalf("retryAfterSec = %d, а текст обещает %d — клиент не поймёт, когда повторять",
			got, streamGrowthRetryAfterSecR385)
	}
	if got := toIntR447(out["loadedWindowTokens"]); got != v.Loaded {
		t.Fatalf("loadedWindowTokens = %d, want %d", got, v.Loaded)
	}
}

// TestR447_NoSnapshotKeepsLegacyNumbers — без снимка writer считает числа как
// раньше (старые вызовы не сломаны).
func TestR447_NoSnapshotKeepsLegacyNumbers(t *testing.T) {
	p := newGrowthGuardProxyR299()
	body := []byte(r301LongStreamBody())

	rec := httptest.NewRecorder()
	writePreflightDeferredR299(rec, p, "bk-1", "m", body, "/api/chat", "legacy", 30)

	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("тело не JSON: %v", err)
	}
	if got := toIntR447(out["loadedWindowTokens"]); got != 8192 {
		t.Fatalf("loadedWindowTokens = %d, want 8192 (из метрик)", got)
	}
}

// TestR447_TransportFillsSnapshot — слот контекста заполняется вердиктом в
// транспортном preflight (иначе writer снова пересчитает числа).
func TestR447_TransportFillsSnapshot(t *testing.T) {
	// Рост выключен: проверяется передача чисел вердикта, а не физика машины.
	t.Setenv("LB_NCTX_GROW_FOR_STREAM", "0")
	p := newGrowthGuardProxyR299()
	body := []byte(r301LongStreamBody())

	ctx, slot := growthNumbersSlotR447(context.Background())
	_, needsProxy, msg, status := p.preflightNCtxReloadIfNeeded(ctx, "bk-1", "m", body, "/api/chat")
	if needsProxy || status == 200 {
		t.Fatalf("ожидался отказ на промпт, который не влезает: needsProxy=%v status=%d", needsProxy, status)
	}
	if !slot.Set {
		t.Fatalf("снимок чисел не заполнен (msg=%q)", msg)
	}
	if slot.Loaded != 8192 || slot.Needed <= slot.Loaded {
		t.Fatalf("числа снимка не совпадают с вердиктом: %+v", *slot)
	}
}
