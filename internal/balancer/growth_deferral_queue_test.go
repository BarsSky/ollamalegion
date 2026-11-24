// growth_deferral_queue_test.go — R679 (2026-11-20): очередь отложенного роста
// окна, поведение по шагам.
//
// Соседний growth_deferral_test.go проверяет то же СКВОЗЬ транспорт (живой
// сценарий стенда), здесь — сам механизм: preflight только планирует рост,
// исполняется он на завершении хода, а занятость (идущий ход или чужой стрим)
// переносит исполнение, не теряя заявку.
package balancer

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// reloadCounterR679 — фейковый воркер, считающий перезагрузки окна.
type reloadCounterR679 struct {
	srv      *httptest.Server
	reloads  int32
	mu       sync.Mutex
	target   int
	lastName string
}

func newReloadCounterR679(t *testing.T) *reloadCounterR679 {
	t.Helper()
	c := &reloadCounterR679{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models/load" && r.Method == http.MethodPost {
			var req struct {
				Name        string `json:"name"`
				ContextSize int    `json:"contextSize"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			// R741: сначала цель/имя, ПОТОМ счётчик. Иначе ожидание «счётчик == N»
			// может вернуться раньше, чем записан target, и проверка цели прочитает 0.
			c.mu.Lock()
			c.target = req.ContextSize
			c.lastName = req.Name
			c.mu.Unlock()
			atomic.AddInt32(&c.reloads, 1)
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"status":"loading"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *reloadCounterR679) count() int { return int(atomic.LoadInt32(&c.reloads)) }

func (c *reloadCounterR679) relTarget() (int, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.target, c.lastName
}

// waitReloadsR679 — дождаться, пока фейковый воркер насчитает want перезагрузок.
//
// ЗАЧЕМ. Рост окна запускается АСИНХРОННО — это и есть требование R664/R679
// («не перезагружать под обслуживаемым запросом»). Поэтому сразу после
// drainDeferredGrowthR664 счётчик может быть ещё 0, и проверка «!= 1» ловила не
// дефект, а гонку с фоновой горутиной: на загруженном раннере под покрытием это
// дало плавающий красный CI (прогон 45327320473, шаг «Generate coverage
// (race + atomic)»):
//
//	growth_deferral_queue_test.go:144: перезагрузок 0 после хода, want 1
//
// Ждём до 5 с, опрашивая счётчик (без фиксированного «сна на угад»), и падаем с
// причиной, если перезагрузки не случилось вовсе.
func waitReloadsR679(t *testing.T, w *reloadCounterR679, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if n := w.count(); n >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("перезагрузок %d за 5 с, want %d: отложенный рост обязан исполниться", w.count(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// queueProxyR679 — прокси с загруженной моделью «m» (8192 на слот) и живым
// адресом воркера: без адреса рост не запускается вовсе (R385 проверяет его
// первым), и тест доказывал бы пустоту.
func queueProxyR679(t *testing.T, workerURL string) *Proxy {
	t.Helper()
	disableExactTokensR447(t)
	p := &Proxy{
		backends: map[string]*BackendState{
			"bk-1": {Backend: &types.Backend{
				ID: "bk-1", Host: "127.0.0.1", CppWorkerPort: extractPort(workerURL),
				Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp,
			}},
		},
		metricsMgr: NewMetricsManager(),
		config:     &types.LoadBalancerConfig{},
		nctxReload: NewNCtxReloadCoordinator(NCtxReloadConfig{PreflightEnabled: true}),
		// executeAsyncReload ходит в воркер через p.client: без него тест падал
		// бы паникой вместо проверки поведения.
		client: &http.Client{},
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
	return p
}

// TestR679_PreflightPlansGrowthInsteadOfRunningIt — ГЛАВНОЕ утверждение дефекта
// №1 на уровне механизма: вердикт «расти в фоне» НЕ перезагружает модель сам, а
// оставляет заявку, которую исполняет завершение хода.
func TestR679_PreflightPlansGrowthInsteadOfRunningIt(t *testing.T) {
	w := newReloadCounterR679(t)
	p := queueProxyR679(t, w.srv.URL)

	body := []byte(r301StreamBody) // num_ctx=16384 при загруженных 8192, промпт ~3
	v := p.growthVerdictR301("rid", "bk-1", "m", body, "/api/chat", true)
	if !v.Served {
		t.Fatalf("ожидалось «обслужить с загруженным окном», получено %+v", v)
	}
	if v.Grow || v.Defer || v.Refuse {
		t.Fatalf("R664: заявка не блокирует запрос и не отказывает, получено %+v", v)
	}
	// 1. Перезагрузки при принятии решения НЕТ (живой дефект: она шла здесь).
	if n := w.count(); n != 0 {
		t.Fatalf("перезагрузок %d сразу после preflight, want 0: решение не имеет права "+
			"запускать unload+load под обслуживаемым запросом", n)
	}
	// 2. Рост ПЛАНИРУЕТСЯ: заявка в очереди, цель — ровно заявленное клиентом окно.
	if got := p.pendingGrowthTargetR664("bk-1", "m"); got != 16384 {
		t.Fatalf("отложенная цель роста = %d, want 16384 (заявленное клиентом окно)", got)
	}

	// 3. Ход идёт: разбор очереди не запускает перезагрузку, но и не теряет заявку.
	endTurn := p.beginClientTurnR664("bk-1")
	if got := p.turnsInFlightR664("bk-1"); got != 1 {
		t.Fatalf("учёт ходов = %d, want 1: preflight обязан видеть идущий ход клиента", got)
	}
	p.drainDeferredGrowthR664("bk-1")
	if n := w.count(); n != 0 {
		t.Fatalf("перезагрузок %d при идущем ходе, want 0 (R271/R292: перезагрузка оборвала бы ответ)", n)
	}
	if got := p.pendingGrowthTargetR664("bk-1", "m"); got != 16384 {
		t.Fatalf("заявка потеряна при занятости: цель = %d, want 16384", got)
	}

	// 4. Ход закончился — рост исполняется ровно один раз и на заявленную цель.
	endTurn()
	if got := p.turnsInFlightR664("bk-1"); got != 0 {
		t.Fatalf("после завершения хода учёт = %d, want 0", got)
	}
	p.drainDeferredGrowthR664("bk-1")
	// R741: перезагрузка асинхронна — ждём её, а не проверяем мгновенно.
	waitReloadsR679(t, w, 1)
	target, name := w.relTarget()
	if target != 16384 {
		t.Fatalf("суммарное окно перезагрузки = %d, want 16384 (16384 на клиента × 1 слот)", target)
	}
	if name != "m" {
		t.Fatalf("перезагружалась модель %q, want m", name)
	}
	if got := p.pendingGrowthTargetR664("bk-1", "m"); got != 0 {
		t.Fatalf("заявка осталась в очереди после исполнения (цель %d): перезагрузка повторится", got)
	}
}

// TestR679_DeferredGrowthWaitsForForeignStream — занятость чужим стримом: рост
// переносится, заявка не теряется, окно не уменьшается (R294/R448).
func TestR679_DeferredGrowthWaitsForForeignStream(t *testing.T) {
	w := newReloadCounterR679(t)
	p := queueProxyR679(t, w.srv.URL)

	// Чужой стрим уже идёт, когда preflight принял решение.
	release := beginClientStreamR271(p, "bk-1", "m")
	defer release()

	v := p.growthVerdictR301("rid", "bk-1", "m", []byte(r301StreamBody), "/api/chat", true)
	if !v.Served || v.Grow {
		t.Fatalf("ожидалось «обслужить сейчас + рост в очередь», получено %+v", v)
	}
	if got := v.Active; got < 1 {
		t.Fatalf("active_streams вердикта = %d, want >= 1 (заявка ставится при живом счёте стримов)", got)
	}
	p.drainDeferredGrowthR664("bk-1")
	if n := w.count(); n != 0 {
		t.Fatalf("перезагрузок %d при чужом стриме, want 0 (R271: перезагрузка оборвала бы его)", n)
	}
	if got := p.pendingGrowthTargetR664("bk-1", "m"); got != 16384 {
		t.Fatalf("заявка потеряна при чужом стриме: цель = %d", got)
	}

	// Второй запрос просит БОЛЬШЕ: цель очереди растёт, но не понижается.
	p.deferGrowthAfterTurnR664(queuedGrowthR664{backendID: "bk-1", model: "m", target: 32768})
	if got := p.pendingGrowthTargetR664("bk-1", "m"); got != 32768 {
		t.Fatalf("цель очереди = %d, want 32768 (большая заявка)", got)
	}
	p.deferGrowthAfterTurnR664(queuedGrowthR664{backendID: "bk-1", model: "m", target: 16384})
	if got := p.pendingGrowthTargetR664("bk-1", "m"); got != 32768 {
		t.Fatalf("цель очереди понизилась до %d: рост не может быть уменьшением окна (R294)", got)
	}

	// Стрим закончился — рост исполняется на БОЛЬШУЮ заявку.
	release()
	p.drainDeferredGrowthR664("bk-1")
	if n := w.count(); n != 1 {
		t.Fatalf("перезагрузок %d после освобождения узла, want 1", n)
	}
	if target, _ := w.relTarget(); target != 32768 {
		t.Fatalf("суммарное окно перезагрузки = %d, want 32768 (большая из заявок)", target)
	}
}

// TestR679_DeferredGrowthRespectsSwitch — выключатель роста на стриминге
// действует и на отложенное исполнение (R385/R472): заявка не исполняется и не
// зависает в очереди.
func TestR679_DeferredGrowthRespectsSwitch(t *testing.T) {
	t.Setenv("LB_NCTX_GROW_FOR_STREAM", "0")
	w := newReloadCounterR679(t)
	p := queueProxyR679(t, w.srv.URL)

	p.deferGrowthAfterTurnR664(queuedGrowthR664{
		backendID: "bk-1", model: "m", target: 16384, streaming: true,
	})
	p.drainDeferredGrowthR664("bk-1")
	if n := w.count(); n != 0 {
		t.Fatalf("перезагрузок %d при выключенном росте на стриминге, want 0", n)
	}
	if got := p.pendingGrowthTargetR664("bk-1", "m"); got != 0 {
		t.Fatalf("заявка висит в очереди (%d) при выключенном росте: она не исполнится никогда", got)
	}
}

// TestR679_DeferredGrowthSkipsSatisfiedTarget — цель уже покрыта загруженным
// окном (окно подняли другим путём) — перезагрузки нет: «расти некуда».
func TestR679_DeferredGrowthSkipsSatisfiedTarget(t *testing.T) {
	w := newReloadCounterR679(t)
	p := queueProxyR679(t, w.srv.URL)

	p.deferGrowthAfterTurnR664(queuedGrowthR664{backendID: "bk-1", model: "m", target: 8192})
	p.drainDeferredGrowthR664("bk-1")
	if n := w.count(); n != 0 {
		t.Fatalf("перезагрузок %d при цели, равной загруженному окну, want 0", n)
	}
	if got := p.pendingGrowthTargetR664("bk-1", "m"); got != 0 {
		t.Fatalf("заявка, которая уже покрыта окном, осталась в очереди (цель %d)", got)
	}
}
