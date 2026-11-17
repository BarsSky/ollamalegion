// window_serve_as_is_r447_test.go — R447 (2026-11-17, вторая половина правила):
// «рост нужен» ≠ «надо отказать». Проверки ровно на живом факте второго стенда
// (архив 2026-11-16T14:53:34Z, разбор в docs/architecture/context-numbers.ru.md
// §5.5): загруженное окно 180224, оценка промпта 180200, клиент окна не просил,
// на узле чужой стрим — отказа быть НЕ ДОЛЖНО, потому что окно обслуживает
// запрос.
//
// Проверяются оба исхода одной таблицей чисел:
//   - 180200 при окне 180224 → окно обслуживает, отказа/ожидания нет;
//   - 200000 при окне 180224 → окно не обслуживает, отказ (или рост) остаётся;
//   - промпт в 3 токена при окне 2048 (живой дефект самого R447) → ни роста, ни
//     отказа, ни перезагрузки (регресс R447/R330).
package balancer

import (
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// newLoadedWindowProxyR447 — прокси с загруженной моделью "m" и заданным окном
// НА СЛОТ. Ровно та конфигурация, на которой снят архив второго стенда
// (parallel=1: суммарное окно совпадает с окном на слот).
func newLoadedWindowProxyR447(t *testing.T, window int) *Proxy {
	t.Helper()
	p := newGrowthGuardProxyR299()
	p.metricsMgr.mu.Lock()
	p.metricsMgr.llamaMetrics["bk-1"] = &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{{
			Name: "m", State: "loaded",
			ContextLength: window, ContextPerSeq: window,
		}},
	}
	p.metricsMgr.mu.Unlock()
	p.nctxReload.SetLastKnownNCtx("bk-1", window)
	return p
}

// r447BodyWithEstimatedPromptR447 — тело /api/chat, ОЦЕНКА промпта которого
// равна wantTokens.
//
// Зачем подбор, а не просто «длинный текст»: тест обязан воспроизводить живые
// ЧИСЛА (180200 при окне 180224), иначе он перестанет ловить именно тот дефект.
// pkg/tokencount считает ASCII-буквы по 2 руны на токен, а перевод строки после
// сообщения — 1 токен (ExtractRequestMeta добавляет "\n"), поэтому длина
// подбирается одним шагом и проверяется тем же extractRequestMeta, что и в
// решении.
func r447BodyWithEstimatedPromptR447(t *testing.T, p *Proxy, wantTokens int) []byte {
	t.Helper()
	letters := 2 * (wantTokens - 1)
	for attempt := 0; attempt < 4; attempt++ {
		body := []byte(`{"model":"m","messages":[{"role":"user","content":"` +
			strings.Repeat("a", letters) + `"}]}`)
		meta := p.extractRequestMeta(body, "/api/chat")
		if meta == nil {
			t.Fatalf("тело не разобралось как /api/chat")
		}
		got := meta.EstimatedPromptTokens
		if got == wantTokens {
			return body
		}
		// Одна руна-буква стоит полтокена: правим длину на 2 руны за токен.
		letters += 2 * (wantTokens - got)
		if letters <= 0 {
			t.Fatalf("не удалось подобрать оценку %d (получили %d)", wantTokens, got)
		}
	}
	t.Fatalf("не удалось подобрать оценку промпта %d за 4 попытки", wantTokens)
	return nil
}

// disableExactTokensR447 — считать по оценке, без похода в токенизатор.
//
// Тест про правило, а не про сеть: с включённым точным подсчётом (R473)
// 360-килобайтный промпт ушёл бы на /api/tokenize воркера, и число в тесте
// зависело бы от живого воркера.
func disableExactTokensR447(t *testing.T) {
	t.Helper()
	t.Setenv("LB_PREFLIGHT_EXACT_TOKENS", "0")
}

// TestR447_ServeAsIsRule — таблица правила «обслужит ли загруженное окно запрос
// как есть». Правило одно на все пути отказа: guard R299, blocker R288, решение
// R493.
func TestR447_ServeAsIsRule(t *testing.T) {
	cases := []struct {
		name              string
		estimated, loaded int
		requested         int
		want              bool
	}{
		// ЖИВОЙ ФАКТ второго стенда: needed = 184296 > loaded, но окно обслуживает.
		{"оценка 180200 при окне 180224 — окно обслуживает", 180200, 180224, 0, true},
		{"промпт влезает с запасом", 1000, 8192, 0, true},
		{"промпт влезает, но запас под ответ меньше минимума 256", 2000, 2048, 0, true},
		{"промпт ровно в окно", 8192, 8192, 0, true},
		{"промпт не влезает", 200000, 180224, 0, false},
		{"клиент попросил больше загруженного", 3, 2048, 32768, false},
		{"клиент попросил ровно загруженное", 3, 2048, 2048, true},
		{"окно неизвестно — не отказываем", 200000, 0, 0, true},
		{"оценки нет, окно не просили", 0, 2048, 0, true},
		{"оценки нет, клиент попросил больше", 0, 2048, 4096, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, why := windowServeAsIsR447(tc.estimated, tc.loaded, tc.requested)
			if got != tc.want {
				t.Fatalf("windowServeAsIsR447(%d,%d,%d) = %v, want %v (причина: %s)",
					tc.estimated, tc.loaded, tc.requested, got, tc.want, why)
			}
			if why == "" {
				t.Fatal("причина не названа — оператор не поймёт решения")
			}
		})
	}
}

// TestR447_LiveDefectNoRefusalWhenWindowServesAsIs — РЕГРЕСС НА ЖИВОЙ ФАКТ.
//
// До правки guard отказывал по `needed > loaded` (184296 > 180224) при активном
// стриме, и клиент вместо ответа получал 503 «needs 184296 tokens of context,
// the loaded window is 180224 and cannot grow right now». Промпт при этом
// влезал в окно.
func TestR447_LiveDefectNoRefusalWhenWindowServesAsIs(t *testing.T) {
	disableExactTokensR447(t)          // считаем по оценке: тест про правило, не про сеть
	t.Setenv("LB_WINDOW_UNIFIED", "0") // боевое значение по умолчанию (историческая ветка)

	p := newLoadedWindowProxyR447(t, 180224)
	stop := beginClientStreamR271(p, "bk-1", "m")
	defer stop()

	body := r447BodyWithEstimatedPromptR447(t, p, 180200)
	// Предусловие живого дефекта: на узле идёт чужой стрим (в отказе он и был
	// назван: active streams=1). Считаем тем же источником, что и решение.
	liveActive, livePending := growthBlockersR288(p, "bk-1", "m")
	if liveActive != 1 || livePending {
		t.Fatalf("предусловие не выполнено: active=%d reload_pending=%t, want 1/false",
			liveActive, livePending)
	}
	deferNow, estimated, loaded, needed, _, _ := p.growthDeferR299("bk-1", "m", body, "/api/chat")
	if deferNow {
		t.Fatalf("ОТКАЗ на запрос, который окно обслуживает: estimated=%d loaded=%d needed=%d "+
			"active=%d reload_pending=%t (живой дефект: 180200 при окне 180224)",
			estimated, loaded, needed, liveActive, livePending)
	}
	if estimated != 180200 || loaded != 180224 || needed != 184296 {
		t.Fatalf("числа не воспроизводят живой факт: estimated=%d loaded=%d needed=%d, "+
			"want 180200/180224/184296", estimated, loaded, needed)
	}

	// Тот же вход на стриминговом guard: там окно уже обслуживалось, и это не
	// должно измениться (паритет путей, R493 T3).
	if v := p.growthVerdictR301("rid", "bk-1", "m", body, "/api/chat", true); v.Defer || v.Grow {
		t.Fatalf("стриминговый guard отказал/вырастил там, где окно обслуживает: %+v", v)
	}
	// И на не-стриминговом: историческая ветка отказывала КАЖДОМУ, кому «нужен
	// рост», если на узле шёл стрим — ровно её текст получил клиент второго
	// стенда («… cannot grow right now (active streams=1 …)»).
	if v := p.growthVerdictR301("rid", "bk-1", "m", body, "/api/chat", false); v.Defer {
		t.Fatalf("не-стриминговый вердикт отказал запросу, который окно обслуживает: %+v", v)
	}

	// 200000 токенов в окно 180224 не влезают — отказ (ожидание роста) остаётся.
	big := r447BodyWithEstimatedPromptR447(t, p, 200000)
	deferNow, estimated, loaded, needed, _, _ = p.growthDeferR299("bk-1", "m", big, "/api/chat")
	if !deferNow {
		t.Fatalf("промпт %d не влезает в окно %d — отказ обязан остаться", estimated, loaded)
	}
	if estimated != 200000 || needed != 204096 {
		t.Fatalf("числа отказа: estimated=%d needed=%d, want 200000/204096", estimated, needed)
	}
	if v := p.growthVerdictR301("rid", "bk-1", "m", big, "/api/chat", false); !v.Defer {
		t.Fatalf("не-стриминговый вердикт промолчал на промпте, который не влезает: %+v", v)
	}
}

// TestR447_ShortPromptSmallWindowNoGrowthNoReload — регресс R447/R330.
//
// Короткий промпт при окне 2048: оценка здесь 2 токена (живой случай R447 —
// «Привет!», 3 токена), а needed = оценка + 4096 = 4098, то есть `needed >
// loaded` — прежний guard отказывал и запускал перезагрузку модели. Окно
// обслуживает такой запрос: ни роста, ни отказа.
func TestR447_ShortPromptSmallWindowNoGrowthNoReload(t *testing.T) {
	p := newLoadedWindowProxyR447(t, 2048)
	stop := beginClientStreamR271(p, "bk-1", "m")
	defer stop()

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`)

	deferNow, estimated, loaded, needed, _, _ := p.growthDeferR299("bk-1", "m", body, "/api/chat")
	if deferNow {
		t.Fatalf("отказ короткому промпту (estimated=%d, loaded=%d, needed=%d) — живой дефект R447",
			estimated, loaded, needed)
	}
	if estimated <= 0 || estimated >= loaded {
		t.Fatalf("оценка %d при окне %d: тело не воспроизводит короткий запрос", estimated, loaded)
	}
	if needed <= loaded {
		t.Fatalf("needed=%d должно превышать loaded=%d — иначе тест не про этот дефект", needed, loaded)
	}

	v := p.growthVerdictR301("rid", "bk-1", "m", body, "/api/chat", true)
	if v.Grow || v.Defer || v.Served {
		t.Fatalf("ожидалось «обслуживаем как есть»: grow=%v defer=%v served=%v (%+v)",
			v.Grow, v.Defer, v.Served, v)
	}
}

// TestR447_UnifiedDecisionAgreesWithGuard — единое решение (R493) и исторический
// guard отвечают ОДИНАКОВО на том же входе: иначе «два мозга» снова разойдутся
// ровно на живом случае.
func TestR447_UnifiedDecisionAgreesWithGuard(t *testing.T) {
	disableExactTokensR447(t)
	t.Setenv("LB_WINDOW_UNIFIED", "1")

	p := newLoadedWindowProxyR447(t, 180224)
	stop := beginClientStreamR271(p, "bk-1", "m")
	defer stop()

	body := r447BodyWithEstimatedPromptR447(t, p, 180200)
	d, ok := p.windowDecisionForR493("bk-1", "m", body, "/api/chat", false)
	if !ok {
		t.Fatal("вход единого решения не собрался")
	}
	if d.Kind != windowNoopR493 {
		t.Fatalf("единое решение: вид %q, ожидался %q (причина: %s)", d.Kind, windowNoopR493, d.Why)
	}
	if !d.PromptFits {
		t.Fatalf("промпт %d в окне %d обязан влезать", d.Estimated, d.Loaded)
	}
	if deferNow, _, _, _, _, _ := p.growthDeferR299("bk-1", "m", body, "/api/chat"); deferNow {
		t.Fatal("guard на едином решении отказал там, где решение говорит «обслуживаем»")
	}
	// Вердикт R301 в режиме единого решения — тот же вид на обоих вариантах пути.
	if v := p.growthVerdictR301("rid", "bk-1", "m", body, "/api/chat", false); v.Defer {
		t.Fatalf("не-стриминговый вердикт (единое решение) отказал обслуживаемому запросу: %+v", v)
	}

	// Промпт не влезает — оба пути дают «ждать/расти».
	big := r447BodyWithEstimatedPromptR447(t, p, 200000)
	d, ok = p.windowDecisionForR493("bk-1", "m", big, "/api/chat", false)
	if !ok {
		t.Fatal("вход единого решения не собрался")
	}
	if d.Kind != windowDeferR493 {
		t.Fatalf("единое решение: вид %q, ожидался %q (причина: %s)", d.Kind, windowDeferR493, d.Why)
	}
	if deferNow, _, _, _, _, _ := p.growthDeferR299("bk-1", "m", big, "/api/chat"); !deferNow {
		t.Fatal("guard промолчал там, где решение говорит «ждать»")
	}
}

// TestR447_BlockerServesAsIs — R288 (отказ ДО admission-очереди) считает тем же
// правилом: запрос, который окно обслуживает, в очередь не ставим и НЕ
// отказываем.
func TestR447_BlockerServesAsIs(t *testing.T) {
	p := newLoadedWindowProxyR447(t, 180224)
	stop := beginClientStreamR271(p, "bk-1", "m")
	defer stop()

	// 180200 при окне 180224 — окно обслуживает: блокировки (и 503) быть не должно.
	small := &RequestMeta{ModelName: "m", EstimatedPromptTokens: 180200}
	if blocked, needed, loaded := p.contextGrowthBlocker("bk-1", "m", small); blocked {
		t.Fatalf("блокировка запроса, который окно обслуживает: needed=%d loaded=%d", needed, loaded)
	}

	// 200000 при окне 180224 — не обслуживает: отказ с числами остаётся.
	big := &RequestMeta{ModelName: "m", EstimatedPromptTokens: 200000}
	blocked, needed, loaded := p.contextGrowthBlocker("bk-1", "m", big)
	if !blocked {
		t.Fatalf("промпт 200000 в окно %d не влезает — отказ обязан остаться", 180224)
	}
	if needed != 204096 || loaded != 180224 {
		t.Fatalf("числа отказа: needed=%d loaded=%d, want 204096/180224", needed, loaded)
	}
}
