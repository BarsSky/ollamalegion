package balancer

// window_decision_t3_r493_test.go — R493-12n (этап 3 «два мозга»): T3 на всех
// путях.
//
// Критерий приёмки этапа из `refactoring-plan.ru.md` — «T3 окно: таблица
// (needed, loaded, requested, активные стримы, reload) → вердикт, одинаковый на
// всех путях». Здесь три уровня доказательства:
//
//  1. ЧИСТАЯ ТАБЛИЦА (`TestR493_DecideWindow_*`): вход-числа → вид вердикта.
//     Без кластера, метрик и HTTP: решение проверяемо как функция.
//  2. ПАРИТЕТ ПУТЕЙ (`TestR493_PathParity_*`): тот же запрос через
//     generic-guard (стриминг и не-стриминг), транспортный preflight и план
//     роста R385 даёт ОДИН вид вердикта. Отличие пути — только объявленная
//     способность расти (`GrowBlockedBy`), а не второе правило.
//  3. ОТКАТ И «ТЕНЬ» (`TestR493_Flag*`): при 0 поведение ровно историческое, при
//     2 — историческое поведение + запись расхождений, при 1 — единое решение.

import (
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// windowTestProxyR493 — прокси с одной загруженной моделью и управляемым окном.
//
// ceiling задаётся профилем модели (ContextLengthMax) и потолком авто-роста;
// 0 означает «потолок неизвестен».
func windowTestProxyR493(t *testing.T, model string, loaded, ceiling int, parallel int) *Proxy {
	t.Helper()
	p := &Proxy{
		config:     &types.LoadBalancerConfig{},
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
	}
	p.backends["b1"] = &BackendState{Backend: &types.Backend{
		ID: "b1", Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
	}}
	if ceiling > 0 {
		p.ReplaceModelProfiles(map[string]types.LlamaCppModelProfile{
			model: {ContextLengthMax: ceiling},
		})
	}
	p.nctxReload = NewNCtxReloadCoordinator(NCtxReloadConfig{AutoReloadMaxNCtx: ceiling})
	if parallel < 1 {
		parallel = 1
	}
	p.metricsMgr.llamaMetrics["b1"] = &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{{
			Name: model, State: "loaded",
			ContextLength: loaded, ContextPerSeq: loaded,
			GGUFMaxContext: 262144, MaxSlots: parallel, Parallel: parallel,
		}},
	}
	return p
}

// ── 1. Чистая таблица ────────────────────────────────────────────────────────

func TestR493_DecideWindow_Kinds(t *testing.T) {
	cases := []struct {
		name string
		in   windowInputR493
		want windowKindR493
	}{
		{
			name: "окно известно и достаточно",
			in:   windowInputR493{Needed: 8192, Estimated: 7000, Loaded: 8192},
			want: windowNoopR493,
		},
		{
			name: "загруженное окно неизвестно — не спекулируем",
			in:   windowInputR493{Needed: 8192, Estimated: 7000, Loaded: 0},
			want: windowNoopR493,
		},
		{
			name: "потребность неизвестна",
			in:   windowInputR493{Needed: 0, Estimated: 7000, Loaded: 8192},
			want: windowNoopR493,
		},
		{
			// R385: клиент ЯВНО попросил окно больше загруженного — это и есть
			// требование роста, даже если промпт короткий.
			name: "клиент просит больше загруженного — рост",
			in: windowInputR493{Needed: 16384, Estimated: 900, Requested: 16384,
				Loaded: 8192, Ceiling: 262144},
			want: windowGrowR493,
		},
		{
			// R447: промпт влезает, клиент больше не просил — обслуживаем как есть.
			name: "промпт влезает, клиент больше не просил",
			in:   windowInputR493{Needed: 16384, Estimated: 900, Loaded: 8192},
			want: windowNoopR493,
		},
		{
			name: "промпт не влезает, помех нет — рост",
			in: windowInputR493{Needed: 40000, Estimated: 39000, Loaded: 8192,
				Ceiling: 262144},
			want: windowGrowR493,
		},
		{
			// R288: чужой стрим — ждать, а не перезагружать поверх него.
			name: "идёт стрим — отложить",
			in: windowInputR493{Needed: 40000, Estimated: 39000, Loaded: 8192,
				Active: 1, Ceiling: 262144},
			want: windowDeferR493,
		},
		{
			name: "перезагрузка уже идёт — отложить",
			in: windowInputR493{Needed: 40000, Estimated: 39000, Loaded: 8192,
				ReloadPending: true, Ceiling: 262144},
			want: windowDeferR493,
		},
		{
			// R385/R472: выключатель роста обязан быть ПРИЧИНОЙ отказа.
			name: "рост запрещён путём — отказ с причиной",
			in: windowInputR493{Needed: 40000, Estimated: 39000, Loaded: 8192,
				GrowBlockedBy: "рост окна на стриминге выключен", Ceiling: 262144},
			want: windowRefuseR493,
		},
		{
			name: "запрошено больше потолка — отказ",
			in: windowInputR493{Needed: 300000, Estimated: 290000, Requested: 300000,
				Loaded: 8192, Ceiling: 262144},
			want: windowRefuseR493,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := decideWindowR493(tc.in)
			if d.Kind != tc.want {
				t.Fatalf("вид = %q, ожидался %q (причина: %s)", d.Kind, tc.want, d.Why)
			}
			if d.Why == "" && d.Kind != windowNoopR493 {
				t.Fatal("вердикт без причины: оператору нечего прочитать в логе")
			}
			if d.Loaded != tc.in.Loaded {
				t.Fatalf("вердикт потерял загруженное окно: %d != %d", d.Loaded, tc.in.Loaded)
			}
		})
	}
}

// TestR493_DecideWindow_R466Cap — завышенная НАША оценка урезается до потолка и
// рост не отменяет, а требование клиента сверх потолка — не урезается.
func TestR493_DecideWindow_R466Cap(t *testing.T) {
	t.Run("выведенное окно урезано до потолка и рост разрешён", func(t *testing.T) {
		d := decideWindowR493(windowInputR493{
			Needed: 300000, Estimated: 290000, Requested: 0, Loaded: 8192, Ceiling: 262144,
		})
		if !d.Capped || d.Needed != 262144 {
			t.Fatalf("ожидалось урезание до 262144 (capped=true), получено needed=%d capped=%t (%s)",
				d.Needed, d.Capped, d.Why)
		}
		if d.Kind != windowGrowR493 {
			t.Fatalf("урезанное до потолка окно обязано расти, вид = %q (%s)", d.Kind, d.Why)
		}
	})
	t.Run("явно запрошенное сверх потолка не урезается, а отказывается", func(t *testing.T) {
		d := decideWindowR493(windowInputR493{
			Needed: 300000, Estimated: 100, Requested: 300000, Loaded: 8192, Ceiling: 262144,
		})
		if d.Capped {
			t.Fatal("требование клиента (requested>0) урезать нельзя — это его окно, а не наша оценка")
		}
		if d.Kind != windowRefuseR493 {
			t.Fatalf("вид = %q, ожидался отказ (%s)", d.Kind, d.Why)
		}
	})
}

// TestR493_DecideWindow_TargetFromOnePlace — цель роста берётся из
// growthTargetR493 и НИКОГДА не ниже загруженного окна (R493-12h).
func TestR493_DecideWindow_TargetFromOnePlace(t *testing.T) {
	for loaded := 0; loaded <= 131072; loaded += 8192 {
		for _, needed := range []int{8193, 16384, 40000, 131073, 149809, 160674} {
			d := decideWindowR493(windowInputR493{
				Needed: needed, Estimated: needed, Loaded: loaded, Ceiling: 262144,
			})
			if d.Kind != windowGrowR493 {
				continue
			}
			want, ok := growthTargetR493(needed, loaded, 262144)
			if !ok || d.Target != want {
				t.Fatalf("needed=%d loaded=%d: цель %d, growthTargetR493 даёт %d (ok=%t)",
					needed, loaded, d.Target, want, ok)
			}
			if loaded > 0 && d.Target <= loaded {
				t.Fatalf("needed=%d loaded=%d: цель %d не больше загруженного — перезагрузка вниз",
					needed, loaded, d.Target)
			}
		}
	}
}

// TestR493_DecideWindow_NeverShrinkMapsToServe — полоса, где потолок режет цель
// НИЖЕ загруженного окна: роста нет (R493-12h — окно не опускаем), и если клиент
// просил больше загруженного, запрос обслуживается подменой num_ctx.
//
// Полоса узкая и требует потолка, НЕ кратного 256 (иначе выравненный вниз потолок
// всегда попадает в само загруженное окно): потолок 100095 при загруженных
// 100000 — выравнивание вниз по 256 даёт 99840, то есть меньше окна.
func TestR493_DecideWindow_NeverShrinkMapsToServe(t *testing.T) {
	d := decideWindowR493(windowInputR493{
		Needed: 100050, Estimated: 99000, Requested: 100050,
		Loaded: 100000, Ceiling: 100095,
	})
	if d.Kind != windowServeR493 {
		t.Fatalf("вид = %q, ожидалось «обслужить» (причина: %s)", d.Kind, d.Why)
	}
	if d.Target != 0 {
		t.Fatalf("цель роста %d при запрете опускать окно — это перезагрузка вниз", d.Target)
	}
}

// TestR493_VerdictMapping_Refuse — «рост невозможен» не значит «отказать»:
// запрос обслуживается, если влезает сам промпт, и отказывается, если нет.
func TestR493_VerdictMapping_Refuse(t *testing.T) {
	blocked := windowInputR493{
		Needed: 16384, Estimated: 900, Requested: 16384, Loaded: 8192,
		GrowBlockedBy: "рост окна на стриминге выключен",
	}
	d := decideWindowR493(blocked)
	if d.Kind != windowRefuseR493 {
		t.Fatalf("вид = %q, ожидался отказ (%s)", d.Kind, d.Why)
	}
	if !d.PromptFits {
		t.Fatal("промпт 900 в окне 8192 обязан влезать")
	}
	v, _ := windowVerdictFromDecisionR493("rid", d, true)
	if !v.Served {
		t.Fatalf("при влезающем промпте запрос обязан обслуживаться, вердикт %+v", v)
	}

	// Промпт не влезает — честный отказ с числами и сроком повтора.
	blocked.Estimated = 20000
	blocked.Needed = 24000
	d = decideWindowR493(blocked)
	if d.PromptFits {
		t.Fatal("промпт 20000 не влезает в окно 8192")
	}
	v, _ = windowVerdictFromDecisionR493("rid", d, true)
	if !v.Defer {
		t.Fatalf("при невлезающем промпте обязан быть отказ, вердикт %+v", v)
	}
	if v.RetryAfterSec != growthDeferRetryAfterSecR447 {
		t.Fatalf("срок повтора %d, ожидался %d", v.RetryAfterSec, growthDeferRetryAfterSecR447)
	}

	// Промпт влезает, клиент больше не просил — делать нечего (без пустого тела).
	blocked.Estimated = 900
	blocked.Needed = 16384
	blocked.Requested = 0
	d = decideWindowR493(blocked)
	v, acted := windowVerdictFromDecisionR493("rid", d, true)
	if acted || v.Served || v.Defer || v.Grow {
		t.Fatalf("подменять нечего и отказывать не за что, получено %+v (acted=%t)", v, acted)
	}
}

// ── 2. Паритет путей (T3) ────────────────────────────────────────────────────

// TestR493_PathParity_InputsDifferOnlyByCapability — вход решения, собранный для
// стриминга и для не-стриминга по одному и тому же запросу, отличается ТОЛЬКО
// объявленной способностью расти.
func TestR493_PathParity_InputsDifferOnlyByCapability(t *testing.T) {
	const model = "m"
	p := windowTestProxyR493(t, model, 8192, 262144, 1)
	body := []byte(r301LongStreamBody())

	up, ok := p.windowInputForR493("b1", model, body, "/api/chat", true)
	if !ok {
		t.Fatal("вход решения не собрался")
	}
	down, ok := p.windowInputForR493("b1", model, body, "/api/chat", false)
	if !ok {
		t.Fatal("вход решения не собрался")
	}
	if up.GrowBlockedBy != "" {
		t.Fatalf("рост включён и узел не донор — способность обязана быть пустой: %q", up.GrowBlockedBy)
	}
	up.GrowBlockedBy, down.GrowBlockedBy = "", ""
	if up != down {
		t.Fatalf("входы разошлись не только способностью:\n стриминг: %+v\n не-стриминг: %+v", up, down)
	}
}

// TestR493_PathParity_SameVerdictOnAllPaths — один вход → один вид вердикта на
// ВСЕХ путях: generic-guard (оба варианта), транспортный preflight (growthDeferR299)
// и план роста R385.
func TestR493_PathParity_SameVerdictOnAllPaths(t *testing.T) {
	t.Setenv("LB_WINDOW_UNIFIED", "1")
	const model = "m"

	cases := []struct {
		name   string
		body   string
		loaded int
		want   windowKindR493
	}{
		{"окно достаточно", r301StreamBody, 16384, windowNoopR493},
		{"клиент просит больше загруженного — рост (R385)", r301StreamBody, 8192, windowGrowR493},
		{"длинный промпт, роста никто не мешает", r301LongStreamBody(), 8192, windowGrowR493},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := windowTestProxyR493(t, model, tc.loaded, 262144, 1)
			body := []byte(tc.body)

			streamDecision, ok := p.windowDecisionForR493("b1", model, body, "/api/chat", true)
			if !ok {
				t.Fatal("решение (стриминг) не собралось")
			}
			plainDecision, ok := p.windowDecisionForR493("b1", model, body, "/api/chat", false)
			if !ok {
				t.Fatal("решение (не-стриминг) не собралось")
			}
			if streamDecision.Kind != tc.want || plainDecision.Kind != tc.want {
				t.Fatalf("вид: стриминг %q, не-стриминг %q, ожидался %q",
					streamDecision.Kind, plainDecision.Kind, tc.want)
			}
			if streamDecision.Target != plainDecision.Target {
				t.Fatalf("цель роста разошлась по путям: %d против %d",
					streamDecision.Target, plainDecision.Target)
			}

			// Путь 1: guard до очереди (generic) — оба варианта стриминга.
			vStream := p.growthVerdictR301("rid", "b1", model, body, "/api/chat", true)
			vPlain := p.growthVerdictR301("rid", "b1", model, body, "/api/chat", false)
			if got := windowVerdictKindLegacyR493(vStream); got != tc.want {
				t.Fatalf("guard(streaming) дал %q, ожидался %q", got, tc.want)
			}
			// Не-стриминг рост исполняет preflight, поэтому вердикт сообщает числа
			// и цель, но 503 не предписывает: действие — «идём дальше как есть».
			if vPlain.Grow {
				t.Fatal("не-стриминговый вердикт не имеет права предписывать 503: рост исполняет preflight")
			}
			if vPlain.Needed != streamDecision.Needed || vPlain.Loaded != streamDecision.Loaded {
				t.Fatalf("числа вердикта разошлись: %+v против %+v", vPlain, streamDecision)
			}
			if tc.want == windowGrowR493 && vStream.Target != streamDecision.Target {
				t.Fatalf("guard не донёс цель роста: %d против %d", vStream.Target, streamDecision.Target)
			}

			// Путь 2: вход транспортного preflight (R299/R288).
			deferNow, _, _, _, _, _ := p.growthDeferR299("b1", model, body, "/api/chat")
			if deferNow != (tc.want == windowDeferR493) {
				t.Fatalf("growthDeferR299 дал defer=%t при виде %q", deferNow, tc.want)
			}

			// Путь 3: план роста R385 (исторический расчёт цели) обязан согласиться
			// с единым решением о том, РАСТИМ ли (цель считает growthTargetR493).
			plan := p.streamGrowthPlanR385("b1", model, streamDecision.Needed)
			if plan.Grow != (tc.want == windowGrowR493) {
				t.Fatalf("plan.Grow=%t при виде %q (refuse=%q note=%q)",
					plan.Grow, tc.want, plan.Refuse, plan.Note)
			}
			if plan.Grow && plan.Target != streamDecision.Target {
				t.Fatalf("цель R385 %d разошлась с целью решения %d", plan.Target, streamDecision.Target)
			}
		})
	}
}

// TestR493_PathParity_CapabilityIsTheOnlyDifference — когда путь НЕ умеет расти,
// вид вердикта отличается ровно на «отказ вместо роста», причина названа.
func TestR493_PathParity_CapabilityIsTheOnlyDifference(t *testing.T) {
	t.Setenv("LB_WINDOW_UNIFIED", "1")
	t.Setenv("LB_NCTX_GROW_FOR_STREAM", "0")
	const model = "m"
	p := windowTestProxyR493(t, model, 8192, 262144, 1)
	body := []byte(r301LongStreamBody())

	blocked, _ := p.windowDecisionForR493("b1", model, body, "/api/chat", true)
	free, _ := p.windowDecisionForR493("b1", model, body, "/api/chat", false)
	if blocked.Kind != windowRefuseR493 {
		t.Fatalf("с выключателем роста вид = %q, ожидался отказ (%s)", blocked.Kind, blocked.Why)
	}
	if free.Kind != windowGrowR493 {
		t.Fatalf("путь, умеющий растить, обязан расти: вид %q (%s)", free.Kind, free.Why)
	}
	if !strings.Contains(blocked.Why, "выключен") {
		t.Fatalf("причина отказа обязана называть выключатель: %q", blocked.Why)
	}
	if blocked.Needed != free.Needed || blocked.Loaded != free.Loaded {
		t.Fatalf("числа разошлись между путями: %+v против %+v", blocked, free)
	}
}

// ── 3. Флаг: откат и «тень» ─────────────────────────────────────────────────

// TestR493_FlagOffKeepsLegacyBehaviour — при 0 вердикт РАВЕН историческому на
// таблице входов: снятие флага возвращает прежнее поведение один-в-один.
func TestR493_FlagOffKeepsLegacyBehaviour(t *testing.T) {
	t.Setenv("LB_WINDOW_UNIFIED", "0")
	const model = "m"
	bodies := []string{r301StreamBody, r301LongStreamBody()}

	for _, loaded := range []int{4096, 8192, 16384, 32768} {
		p := windowTestProxyR493(t, model, loaded, 262144, 1)
		for _, body := range bodies {
			for _, streaming := range []bool{true, false} {
				got := p.growthVerdictR301("rid", "b1", model, []byte(body), "/api/chat", streaming)
				want := p.growthVerdictLegacyR301("rid", "b1", model, []byte(body), "/api/chat", streaming)
				if windowVerdictKindLegacyR493(got) != windowVerdictKindLegacyR493(want) ||
					got.Needed != want.Needed || got.Loaded != want.Loaded ||
					got.RetryAfterSec != want.RetryAfterSec {
					t.Fatalf("флаг 0 изменил вердикт: получили %+v, исторический %+v", got, want)
				}
			}
		}
	}
}

// TestR493_ShadowModeKeepsLegacyDecisions — при 2 поведение историческое (вердикт
// равен историческому), но единое решение считается: расхождений быть не должно
// на таблице, иначе «тень» показала бы их на стенде.
func TestR493_ShadowModeKeepsLegacyDecisions(t *testing.T) {
	t.Setenv("LB_WINDOW_UNIFIED", "2")
	const model = "m"

	for _, loaded := range []int{4096, 8192, 16384, 32768} {
		p := windowTestProxyR493(t, model, loaded, 262144, 1)
		for _, body := range []string{r301StreamBody, r301LongStreamBody()} {
			got := p.growthVerdictR301("rid", "b1", model, []byte(body), "/api/chat", true)
			want := p.growthVerdictLegacyR301("rid", "b1", model, []byte(body), "/api/chat", true)
			if windowVerdictKindLegacyR493(got) != windowVerdictKindLegacyR493(want) {
				t.Fatalf("режим «тень» изменил поведение: %q против исторического %q",
					windowVerdictKindLegacyR493(got), windowVerdictKindLegacyR493(want))
			}
			// Единое решение обязано СЧИТАТЬСЯ и совпадать с историческим на
			// стриминге: расхождение означало бы, что включать режим 1 нельзя.
			d, ok := p.windowDecisionForR493("b1", model, []byte(body), "/api/chat", true)
			if !ok {
				t.Fatal("единое решение не собралось")
			}
			if d.Kind != windowVerdictKindLegacyR493(want) {
				t.Fatalf("единое решение %q расходится с историческим %q (нужен разбор: %s)",
					d.Kind, windowVerdictKindLegacyR493(want), d.Why)
			}
		}
	}
}

// TestR493_FlagModes — три режима и безопасное поведение на мусоре.
//
// Флаг ЧИСЛОВОЙ намеренно: один разбор на один переключатель. Слова
// «true»/«shadow» понимал бы только второй парсер, а два парсера одного флага
// расходятся — это ровно тот класс, который эта правка и убирает.
func TestR493_FlagModes(t *testing.T) {
	cases := map[string]int{
		"":      windowUnifiedOffR493,
		"0":     windowUnifiedOffR493,
		"1":     windowUnifiedOnR493,
		"2":     windowUnifiedShadowR493,
		"мусор": windowUnifiedOffR493,
		"7":     windowUnifiedOffR493,
		"-1":    windowUnifiedOffR493,
	}
	for raw, want := range cases {
		t.Run("значение="+raw, func(t *testing.T) {
			t.Setenv("LB_WINDOW_UNIFIED", raw)
			if got := windowUnifiedModeR493(); got != want {
				t.Fatalf("режим = %d, ожидался %d", got, want)
			}
		})
	}
}
