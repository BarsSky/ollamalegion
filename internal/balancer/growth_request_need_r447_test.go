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

// TestR447_ClientAskedWindowStillGrows — требование клиента (R385) не отменено.
func TestR447_ClientAskedWindowStillGrows(t *testing.T) {
	p := newGrowthGuardProxyR299() // окно 8192, в теле num_ctx=16384

	v := p.growthVerdictR301("rid-test", "bk-1", "m", []byte(r301StreamBody), "/api/chat", true)
	if !v.Grow {
		t.Fatalf("клиент попросил 16384 при загруженных 8192 — рост обязателен, получено %+v", v)
	}
}

// TestR447_OversizedPromptStillGrows — промпт, который не влезает, тоже растит.
func TestR447_OversizedPromptStillGrows(t *testing.T) {
	p := newGrowthGuardProxyR299()

	v := p.growthVerdictR301("rid-test", "bk-1", "m", []byte(r301LongStreamBody()), "/api/chat", true)
	if !v.Grow {
		t.Fatalf("промпт не влезает в окно — рост обязателен, получено %+v", v)
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
		{"промпт не влезает", 4096, 2048, 0, true},
		{"клиент попросил больше загруженного", 3, 2048, 32768, true},
		{"клиент попросил ровно загруженное", 3, 2048, 2048, false},
		{"оценки нет, окно не просили", 0, 2048, 0, false},
		{"оценки нет, клиент попросил больше", 0, 2048, 4096, true},
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

// TestR447_DeferredBodyUsesVerdictNumbers — тело отказа описывает ТО ЖЕ
// состояние, что и его текст, даже если метрики успели измениться.
func TestR447_DeferredBodyUsesVerdictNumbers(t *testing.T) {
	// Рост выключен принудительно: тест про ТЕЛО отказа, а не про то, можно ли
	// поднять окно на этой машине (иначе исход зависел бы от свободной VRAM).
	t.Setenv("LB_NCTX_GROW_FOR_STREAM", "0")
	p := newGrowthGuardProxyR299()
	body := []byte(r301LongStreamBody())

	v := p.growthVerdictR301("rid-test", "bk-1", "m", body, "/api/chat", true)
	if !v.Defer {
		t.Fatalf("ожидался отказ (промпт не влезает), получено %+v", v)
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
