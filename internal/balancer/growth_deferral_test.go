// growth_deferral_test.go — R679 (2026-11-20, живой прогон матрицы
// `balancer:r261120-r679`, дефект №1): рост окна по заявке клиента НЕ имеет
// права идти под текущим ходом.
//
// ЖИВОЙ ФАКТ. Запрос: 15 токенов промпта, `options.num_ctx=16384` при
// загруженных 8192 на слот, `stream=true`. Ответ 200, но первый байт — 46.9 с, а
// `load_duration=46.925 s`: модель выгрузилась и загрузилась ПОД ЭТИМ ЖЕ
// запросом. Лог при этом утверждал обратное:
//
//	R664: проактивная заявка клиента — поднимаем окно в фоне, запрос обслуживается сейчас (background_growth.go:51)
//	R385: стриминговый запрос требует большего окна — запускаем перезагрузку модели      (stream_growth.go:214)
//	preflightNCtxReload(async): starting reload target_n_ctx=32768                        (nctx_reload_execute.go:270)
//	R664: рост окна не блокирует запрос — обслуживаем как есть, окно растим в фоне        (growth_stream_guard.go:359)
//
// ПРИЧИНА (по коду). `triggerBackgroundGrowthR664` вызывался СИНХРОННО из
// preflight (growth_stream_guard.go), а `background_growth.go` проверял только
// `active_streams`/`reload_pending`. Текущий запрос стримом ещё не учтён
// (beginClientStreamR271 — ПОСЛЕ admission и после ответа апстрима), поэтому
// счётчик был 0, `deferDestructiveOpR291` тоже видел 0 — и unload+load
// запускался прямо под обслуживаемым запросом.
//
// ЗДЕСЬ закреплено ПОВЕДЕНИЕ, а не имена: тест работает только через реальный
// транспорт (`proxyRequestLlamaCpp`) и счётчики фейкового воркера, поэтому он
// красный на коде до правки и зелёный после.
package balancer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// turnGrowthWorkerR679 — фейковый воркер: отвечает на inference, считает
// ПЕРЕЗАГРУЗКИ и запоминает, сколько их было в момент получения хода клиента.
//
// Именно эта пара чисел и различает «рост в фоне» (0 на момент хода) от «рост
// под текущим ходом» (1 и больше: модель выгружается, пока клиент ждёт).
type turnGrowthWorkerR679 struct {
	srv *httptest.Server

	reloads int32 // POST /api/models/load

	mu           sync.Mutex
	reloadAtChat int32 // сколько перезагрузок было, когда пришёл /api/chat
	chatBody     []byte
	reloadTarget int // contextSize из тела перезагрузки
}

func newTurnGrowthWorkerR679(t *testing.T) *turnGrowthWorkerR679 {
	t.Helper()
	w := &turnGrowthWorkerR679{}
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/models/load" && r.Method == http.MethodPost:
			atomic.AddInt32(&w.reloads, 1)
			var req struct {
				Name        string `json:"name"`
				ContextSize int    `json:"contextSize"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			w.mu.Lock()
			w.reloadTarget = req.ContextSize
			w.mu.Unlock()
			// R472: воркер отвечает 202 — загрузка пошла в фоне.
			rw.Header().Set("Content-Type", "application/json")
			rw.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(rw, `{"status":"loading"}`)
		case strings.HasPrefix(r.URL.Path, "/api/chat"):
			atomic.StoreInt32(&w.reloadAtChat, atomic.LoadInt32(&w.reloads))
			body, _ := io.ReadAll(r.Body)
			w.mu.Lock()
			w.chatBody = body
			w.mu.Unlock()
			rw.Header().Set("Content-Type", "application/x-ndjson")
			rw.WriteHeader(http.StatusOK)
			if f, ok := rw.(http.Flusher); ok {
				f.Flush()
			}
			_, _ = io.WriteString(rw, `{"model":"gemma","done":false,"message":{"role":"assistant","content":"привет"}}`+"\n")
			_, _ = io.WriteString(rw, `{"model":"gemma","done":true,"done_reason":"stop","message":{"role":"assistant","content":""}}`+"\n")
		case r.URL.Path == "/api/models":
			// Загруженное окно — то же, что в кэше метрик теста.
			rw.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(rw, `{"count":1,"models":[{"name":"gemma","state":"loaded",`+
				`"context_size":8192,"context_per_seq":8192,"gguf_max_context":131072,"max_slots":1}]}`)
		default:
			rw.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(rw, `{"status":"ok"}`)
		}
	}))
	t.Cleanup(w.srv.Close)
	return w
}

func (w *turnGrowthWorkerR679) reloadCount() int { return int(atomic.LoadInt32(&w.reloads)) }

func (w *turnGrowthWorkerR679) state() (atChat int32, body []byte, target int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return atomic.LoadInt32(&w.reloadAtChat), append([]byte{}, w.chatBody...), w.reloadTarget
}

// waitReloadR679 — дождаться фоновой перезагрузки (она запускается ПОСЛЕ хода,
// поэтому в тесте нет права считать её синхронной).
func (w *turnGrowthWorkerR679) waitReloadR679(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if w.reloadCount() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("перезагрузка окна не запустилась за 3 с после хода (reloads=%d, want %d): "+
		"отложенный рост потерялся", w.reloadCount(), want)
}

// TestR679_GrowthWaitsForCurrentTurn — ГЛАВНЫЙ тест дефекта №1: пока клиент
// обслуживается, перезагрузки нет; тело у воркера согласовано с загруженным
// окном; после хода окно поднимается до заявленного клиентом значения.
func TestR679_GrowthWaitsForCurrentTurn(t *testing.T) {
	disableExactTokensR447(t)
	w := newTurnGrowthWorkerR679(t)
	p, _, _ := makeTestLlamaProxy(t, w.srv.URL)

	// Загруженное окно — то же, что видит живой стенд: 8192 на слот.
	p.metricsMgr.mu.Lock()
	p.metricsMgr.llamaMetrics["llama_test"] = &types.LlamaCppMetrics{
		GGUFMaxContext: 131072,
		LoadedModels: []types.LlamaCppModel{{
			Name: "gemma", State: "loaded",
			ContextLength: 8192, ContextPerSeq: 8192,
			GGUFMaxContext: 131072, MaxSlots: 1,
		}},
	}
	p.metricsMgr.mu.Unlock()
	if p.nctxReload != nil {
		p.nctxReload.SetLastKnownNCtx("llama_test", 8192)
	}

	// Живое тело: промпт в несколько токенов, заявка 16384, стриминг.
	body := []byte(`{"model":"gemma","stream":true,"messages":[{"role":"user","content":"hi"}],` +
		`"options":{"num_ctx":16384,"num_predict":8}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req = req.WithContext(context.WithValue(req.Context(), modelContextKey, "gemma"))
	rec := httptest.NewRecorder()

	start := time.Now()
	if err := p.proxyRequestLlamaCpp(rec, req, "llama_test", body); err != nil {
		t.Fatalf("proxyRequestLlamaCpp: %v", err)
	}
	elapsed := time.Since(start)

	reloadAtChat, chatBody, _ := w.state()
	if len(chatBody) == 0 {
		t.Fatalf("воркер не получил ход клиента: клиент увидел %q", rec.Body.String())
	}
	// 1. ТЕКУЩИЙ ХОД НЕ ЖДЁТ ПЕРЕЗАГРУЗКИ: в момент, когда воркер получил
	// запрос, модель не перезагружалась ни разу.
	if reloadAtChat != 0 {
		t.Fatalf("модель перезагружалась %d раз ДО того, как воркер получил ход клиента: "+
			"рост окна пошёл под текущим запросом (живой дефект: первый байт 46.9 с, "+
			"load_duration=46.925 s)", reloadAtChat)
	}
	// 2. Воркер не должен отвергнуть запрос: тело согласовано с загруженным окном.
	if strings.Contains(string(chatBody), `"num_ctx":16384`) {
		t.Fatalf("воркер получил заявленное клиентом окно (16384) при загруженных 8192 — "+
			"он ответил бы кадром ошибки: %s", string(chatBody))
	}
	if !strings.Contains(string(chatBody), `"num_ctx":8192`) {
		t.Fatalf("в теле для воркера нет окна слота 8192: %s", string(chatBody))
	}
	// 3. Клиент получил полный ответ, а не отказ/обрыв.
	if !strings.Contains(rec.Body.String(), `"done":true`) {
		t.Fatalf("клиент не получил терминальный done:true: %s", rec.Body.String())
	}
	if elapsed > 3*time.Second {
		t.Fatalf("ход занял %v: клиент ждал рост окна", elapsed)
	}

	// 4. РОСТ НЕ ПОТЕРЯН: после хода окно поднимается до заявленного клиентом
	// (16384 на клиента × 1 слот = суммарное 16384).
	w.waitReloadR679(t, 1)
	_, _, target := w.state()
	if target != 16384 {
		t.Fatalf("цель роста = %d, want 16384 (ровно заявленное клиентом окно, R664)", target)
	}
	if n := w.reloadCount(); n != 1 {
		t.Fatalf("перезагрузок %d, want 1: отложенный рост исполняется один раз", n)
	}
	t.Logf("ход обслужен за %v, перезагрузка — после хода, цель %d", elapsed, target)
}
