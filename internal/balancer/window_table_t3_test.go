// window_table_t3_test.go — T3 из плана «два мозга» (refactoring-plan.ru.md §3):
// таблица вердикта по окну.
//
// ЗАЧЕМ. План просит зафиксировать таблицу «(needed, loaded, requested, активные
// стримы, reload) → вердикт, одинаковый на всех путях» ДО того, как окно будет
// сведено в один decideWindow (этап 3). Сейчас вердикт считают ТРИ потребителя:
//
//  1. гейт переезда    — sessionMoveKeepsWindowNeedR480 (порог = need);
//  2. гейт очереди     — sessionQueueGateR480            (порог = окно своей копии);
//  3. диагностика      — backendRejectionsR460           (причина в why_queued).
//
// Пороги у 1 и 2 РАЗНЫЕ по замыслу R480 (вариант C): переезд требует покрыть
// ПОТРЕБНОСТЬ, очередь — «не хуже, чем у меня сейчас». Но КЛАССИФИКАЦИЯ состояния
// кандидата обязана совпадать: «неизвестно» — допускаем, «известный холодный» —
// нет, «известное меньше порога» — нет, «покрывает» — да. Здесь это и проверяется
// таблицей, а расхождение порогов проверяется отдельно и явно — чтобы оно не
// выяснялось однажды из живого дефекта.
//
// Почему это страж, а не рефакторинг: пока таблица зелёная, любой перенос окна в
// один decideWindow обязан сохранить все три вердикта. Когда этап 3 сведёт пути,
// тест придётся обновить — и это будет сигналом, что решение действительно
// поменялось.
package balancer

import (
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

const (
	t3Model  = "m"
	t3From   = "from"
	t3Cand   = "cand"
	t3Window = 65536
)

func newWindowProxyT3(t *testing.T, candidateWindow int, candidatePollAge time.Duration) *Proxy {
	t.Helper()
	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 8080, APIPort: 8081},
		Backends: []types.Backend{
			{ID: t3From, Name: t3From, Host: "127.0.0.1", OllamaPort: 1, Type: types.BackendTypeLlamaCpp,
				Status: types.StatusHealthy, MaxConcurrentReqs: 1, Weight: 1},
			{ID: t3Cand, Name: t3Cand, Host: "127.0.0.1", OllamaPort: 2, Type: types.BackendTypeLlamaCpp,
				Status: types.StatusHealthy, MaxConcurrentReqs: 1, Weight: 1},
		},
		Balancing: types.BalancingSettings{Algorithm: types.AlgorithmResourceAware, MetricsInterval: 60},
	}
	p := newProxyWithCleanup(t, conf)

	setWindow := func(id string, perSeq int) {
		models := []types.LlamaCppModel{}
		if perSeq > 0 {
			models = append(models, types.LlamaCppModel{Name: t3Model, State: "loaded", ContextLength: perSeq, MaxSlots: 1})
		}
		lm := &types.LlamaCppMetrics{LoadedModels: models, VramKnown: true, AvailableVRAMMB: 20490}
		// Обе карты: llamaMetrics читает окно, а metrics — список причин отказа
		// (без записи в metrics причина будет no_metrics, и до окна дело не дойдёт).
		p.metricsMgr.mu.Lock()
		p.metricsMgr.llamaMetrics[id] = lm
		p.metricsMgr.metrics[id] = &types.BackendMetrics{ID: id, LlamaCpp: *lm}
		p.metricsMgr.mu.Unlock()
	}
	setWindow(t3From, t3Window)
	// Окно кандидата: >0 — известно; 0 при свежей метрике — «холодный»;
	// 0 при СТАРОЙ метрике — «неизвестно» (TTL R480).
	setWindow(t3Cand, candidateWindow)
	p.metricsMgr.MarkLlamaCppPolled(t3Cand, time.Now().Add(-candidatePollAge))
	return p
}

// TestT3_WindowVerdictTable — таблица состояний кандидата при need = окно сессии.
//
// При равных порогах (need == окно своей копии, самый частый случай: клиент не
// назвал num_ctx) гейт переезда и гейт очереди обязаны совпадать ПО КАЖДОМУ
// состоянию. Именно здесь расходятся пути, если кто-то поправит порог в одном
// месте.
func TestT3_WindowVerdictTable(t *testing.T) {
	// Порядок полей — под fieldalignment (линт): строки (указатели) первыми,
	// числа и були последними, иначе структура занимает лишние pointer-байты.
	cases := []struct {
		name      string
		state     string
		pollAge   time.Duration
		candWin   int
		moveWant  bool
		queueWant bool
	}{
		{"неизвестно (метрика устарела)", "unknown", 3 * time.Hour, 0, true, true},
		{"известный холодный (0, свежая метрика)", "cold", time.Second, 0, false, false},
		{"уже́ нужного (32768 < 65536)", "narrow", time.Second, 32768, false, false},
		{"ровно нужное (65536)", "equal", time.Second, 65536, true, true},
		{"больше нужного (131072)", "wide", time.Second, 131072, true, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newWindowProxyT3(t, c.candWin, c.pollAge)
			need := t3Window // клиент не назвал num_ctx: need = окно активной копии

			moveOK, why := p.sessionMoveKeepsWindowNeedR480(t3Cand, t3Model, need, "session_window")
			if moveOK != c.moveWant {
				t.Errorf("гейт переезда: %v (%s), ожидалось %v", moveOK, why, c.moveWant)
			}
			gate, fromWindow := p.sessionQueueGateR480(t3From, t3Model)
			if gate == nil {
				t.Fatalf("гейт очереди не построен (окно копии сессии = %d)", fromWindow)
			}
			if got := gate(t3Cand); got != c.queueWant {
				t.Errorf("гейт очереди: %v, ожидалось %v", got, c.queueWant)
			}
			if moveOK != c.queueWant {
				t.Errorf("вердикты путей РАСХОДЯТСЯ при need == окну копии: переезд=%v, очередь=%v "+
					"(состояние %s). Это ровно то, что T3 обязан ловить", moveOK, c.queueWant, c.state)
			}
		})
	}
}

// TestT3_ThresholdsDifferBySessionState — расхождение порогов переезда и очереди
// НЕ произвольное, а следует из правила R480 (вариант C):
//
//   - АКТИВНАЯ сессия: need = max(потребность запроса, окно своей копии) — то есть
//     переезд требует не меньше, чем очередь, и «мягче очереди» быть не может;
//   - ПРОСТАИВАЮЩАЯ сессия: need = потребность запроса (порог может быть меньше
//     окна копии) — переезд на более узкую копию РАЗРЕШЁН осознанно:
//     сессия не работает, терять нечего, а узкая копия может быть свободной.
//
// Фиксируем оба направления явно. Первая версия теста требовала «переезд никогда
// не мягче очереди» и упала на втором случае — то есть моё предположение было
// неверным, а не код: правило про активность сессии записано в R480 и покрыто
// TestR480_IdleSessionUsesRequestNeed.
func TestT3_ThresholdsDifferBySessionState(t *testing.T) {
	t.Setenv("LB_R459_SESSION_TTL_SEC", "900")
	t.Setenv("LB_R459_REQUEST_NEED", "on")

	// Активная сессия: need >= окна копии (65536), значит переезд не мягче очереди.
	p := newWindowProxyT3(t, 32768, time.Second)
	p.seedSessionR480("s-t3-active", time.Now())
	need, source := p.sessionMoveNeedR480(t3From, t3Model, "s-t3-active", 0, 1200)
	if need < t3Window {
		t.Fatalf("активная сессия: need=%d (source=%s), ожидалось не меньше окна копии %d",
			need, source, t3Window)
	}
	if ok, _ := p.sessionMoveKeepsWindowNeedR480(t3Cand, t3Model, need, source); ok {
		t.Errorf("активная сессия: переезд на узкую копию (32768) разрешён при need=%d — "+
			"живая сессия теряет окно", need)
	}

	// Простаивающая сессия (окно «остыло»): need = потребность запроса, переезд
	// на узкую копию осознанно разрешён.
	p2 := newWindowProxyT3(t, 32768, time.Second)
	need2, source2 := p2.sessionMoveNeedR480(t3From, t3Model, "s-t3-idle", 0, 1200)
	if need2 != 1200 || source2 != "request" {
		t.Fatalf("простаивающая сессия: need=%d source=%s, ожидалось 1200/request", need2, source2)
	}
	if ok, why := p2.sessionMoveKeepsWindowNeedR480(t3Cand, t3Model, need2, source2); !ok {
		t.Errorf("простаивающая сессия: переезд на копию 32768 при need=1200 запрещён (%s) — "+
			"правило R480 разрешает: терять нечего", why)
	}
}

// TestT3_RejectionsAgreeWithGate — диагностика не врёт: причина
// window_too_small появляется ровно там, где гейт переезда отказывает по окну.
func TestT3_RejectionsAgreeWithGate(t *testing.T) {
	need := t3Window
	for _, candWin := range []int{0, 32768, 65536, 131072} {
		p := newWindowProxyT3(t, candWin, time.Second)
		moveOK, _ := p.sessionMoveKeepsWindowNeedR480(t3Cand, t3Model, need, "test")
		list := p.backendRejectionsR460(t3Model, types.BackendTypeLlamaCpp,
			&backendRejectionMoveR480{Need: need, NeedSource: "test"})

		var reason string
		for _, r := range list {
			if r.BackendID == t3Cand {
				reason = r.Reason
			}
		}
		if moveOK && reason == rejectWindowTooSmallR460 {
			t.Errorf("кандидат win=%d: гейт переезда ПУСКАЕТ, а why_queued говорит %q — диагностика врёт",
				candWin, reason)
		}
		if !moveOK && candWin > 0 && reason != rejectWindowTooSmallR460 && reason != rejectModelAbsentR460 {
			t.Errorf("кандидат win=%d: гейт переезда отказывает, а причина %q не про окно и не про "+
				"отсутствие модели", candWin, reason)
		}
	}
}
