package balancer

// stream_stall_r298_test.go — R298 (2026-10-29): сторож зависшего стрима.
//
// ЧТО ПРОВЕРЯЕМ И ПОЧЕМУ ИМЕННО ЭТО.
//
// Живой дефект: поток молчал, соединение было открыто, терминатора не приходило
// — клиент ждал бесконечно. При этом сторож существовал (R297), но жил внутри
// тика heartbeat в ОДНОМ пути (hijack /v1), а NDJSON-путь (/api/chat — им ходит
// Cline через Ollama-протокол) сторожа не имел вообще: там блокирующий
// scanner.Scan() и на стенде отключённые idle-таймауты.
//
// Отсюда два уровня тестов:
//  1. ЮНИТЫ на streamStallWatch — «часы» ведут себя так, как заявлено:
//     середина стрима, prefill (по умолчанию не трогаем), Touch продлевает,
//     выключенный сторож не стоит ничего;
//  2. ПОВЕДЕНЧЕСКИЙ тест на реальный путь /api/chat: подвисший upstream должен
//     дать кадр-причину + РОВНО ОДИН терминатор, а не тишину.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/internal/settings"
)

// --- 1. Юниты на часы сторожа --------------------------------------------

// TestR298_StallWatch_MidFrameFires — тишина ПОСЛЕ первого кадра снимает стрим.
func TestR298_StallWatch_MidFrameFires(t *testing.T) {
	var got atomic.Value
	w := newStreamStallWatch(150*time.Millisecond, 0, func(ev stallEvent) {
		got.Store(ev)
	})
	w.Start()
	defer w.Stop()

	if !w.Enabled() {
		t.Fatal("сторож должен быть включён при midFrameLimit > 0")
	}
	// Данные уже шли — значит это середина стрима, а не prefill.
	w.Touch()

	ev, fired := waitFiredR298(t, w, 2*time.Second)
	if !fired {
		t.Fatal("сторож не сработал на тишине после первого кадра")
	}
	if ev.prefill {
		t.Error("prefill = true, want false: данные уже приходили")
	}
	if ev.stalled < 150*time.Millisecond {
		t.Errorf("stalled = %s, want >= 150ms", ev.stalled)
	}
	if v := got.Load(); v == nil {
		t.Error("onStall не вызван (блокирующим путям нечем разбудить чтение)")
	} else if v.(stallEvent).stalled < 150*time.Millisecond {
		t.Errorf("onStall: stalled = %s, want >= 150ms", v.(stallEvent).stalled)
	}
}

// TestR298_StallWatch_PrefillIsNotAbortedByDefault — молчание ДО первого кадра
// по умолчанию (firstFrame=0) не обрывается: это честный prefill на CPU.
func TestR298_StallWatch_PrefillIsNotAbortedByDefault(t *testing.T) {
	w := newStreamStallWatch(150*time.Millisecond, 0, nil)
	w.Start()
	defer w.Stop()

	time.Sleep(500 * time.Millisecond)
	if ev, fired := w.Fired(); fired {
		t.Fatalf("сторож сработал на prefill (%+v), а не должен: первый кадр не приходил", ev)
	}
}

// TestR298_StallWatch_FirstFrameFires — отдельный порог для prefill работает,
// когда оператор его включил (проверка живьём, быстрый первый байт на GPU).
func TestR298_StallWatch_FirstFrameFires(t *testing.T) {
	w := newStreamStallWatch(time.Minute, 150*time.Millisecond, nil)
	w.Start()
	defer w.Stop()

	ev, fired := waitFiredR298(t, w, 2*time.Second)
	if !fired {
		t.Fatal("сторож не сработал на тишине до первого кадра при включённом first-frame пороге")
	}
	if !ev.prefill {
		t.Error("prefill = false, want true: данных не было")
	}
}

// TestR298_StallWatch_TouchDefers — пока данные идут, сторож молчит; после
// последнего кадра отсчёт начинается заново.
func TestR298_StallWatch_TouchDefers(t *testing.T) {
	w := newStreamStallWatch(400*time.Millisecond, 0, nil)
	w.Start()
	defer w.Stop()

	deadline := time.Now().Add(900 * time.Millisecond)
	for time.Now().Before(deadline) {
		w.Touch()
		time.Sleep(100 * time.Millisecond)
		if _, fired := w.Fired(); fired {
			t.Fatal("сторож сработал, хотя данные приходили каждые 100мс при пороге 400мс")
		}
	}
	// Данные перестали приходить — теперь обязан сработать.
	if _, fired := waitFiredR298(t, w, 2*time.Second); !fired {
		t.Fatal("сторож не сработал после прекращения данных")
	}
}

// TestR298_StallWatch_DisabledCostsNothing — оба порога выключены: горутина не
// запускается, Stop идемпотентен, канал событий закрыт (select по нему не виснет
// навсегда, а сразу знает, что событий не будет).
func TestR298_StallWatch_DisabledCostsNothing(t *testing.T) {
	w := newStreamStallWatch(0, 0, nil)
	if w.Enabled() {
		t.Fatal("Enabled() = true при обоих нулевых порогах")
	}
	w.Start()
	w.Stop()
	w.Stop() // идемпотентность
	if _, fired := w.Fired(); fired {
		t.Fatal("выключенный сторож не должен срабатывать")
	}
	if _, ok := <-w.Events(); ok {
		t.Fatal("канал событий выключенного сторожа должен быть закрыт")
	}
	// nil-сторож не должен паниковать: его методы вызываются из горячих путей.
	var nilWatch *streamStallWatch
	if nilWatch.Enabled() {
		t.Fatal("nil.Enabled() = true")
	}
	nilWatch.Touch()
	nilWatch.Stop()
	if _, fired := nilWatch.Fired(); fired {
		t.Fatal("nil.Fired() = true")
	}
}

// --- 2. Настройки: пороги живут в реестре, а не в константах --------------

// TestR298_StallSettingsInRegistry — оба порога зарегистрированы как горячие
// настройки и читаются геттерами. Полуправка «реестр есть, а геттер читает
// env» здесь проверяется ровно так же, как в R278.
func TestR298_StallSettingsInRegistry(t *testing.T) {
	reg := withSettingsRegistry(t)

	for _, name := range []string{"LB_STREAM_STALL_ABORT_SEC", "LB_STREAM_FIRST_FRAME_ABORT_SEC"} {
		def, ok := reg.Definition(name)
		if !ok {
			t.Fatalf("%s не зарегистрирована в реестре настроек", name)
		}
		if !def.HotReload {
			t.Errorf("%s: HotReload = false, а обязана меняться без рестарта", name)
		}
		if def.Validate != nil {
			if err := def.Validate(def.Default); err != nil {
				t.Errorf("%s: дефолт %d не проходит собственную проверку: %v", name, def.Default, err)
			}
		}
	}

	// Дефолты: середина стрима — 5 минут, prefill — выключен.
	if got := streamStallAbortTimeout(); got != streamStallAbortTimeoutDefault {
		t.Errorf("streamStallAbortTimeout() = %s, want %s", got, streamStallAbortTimeoutDefault)
	}
	if got := streamFirstFrameAbortTimeout(); got != 0 {
		t.Errorf("streamFirstFrameAbortTimeout() = %s, want 0 (prefill по умолчанию не трогаем)", got)
	}

	// Горячее применение.
	if _, err := reg.Set("LB_STREAM_STALL_ABORT_SEC", 10); err != nil {
		t.Fatalf("Set LB_STREAM_STALL_ABORT_SEC: %v", err)
	}
	if got := streamStallAbortTimeout(); got != 10*time.Second {
		t.Errorf("после override: %s, want 10s (hot-apply не работает)", got)
	}
	if _, err := reg.Set("LB_STREAM_FIRST_FRAME_ABORT_SEC", 3); err != nil {
		t.Fatalf("Set LB_STREAM_FIRST_FRAME_ABORT_SEC: %v", err)
	}
	if got := streamFirstFrameAbortTimeout(); got != 3*time.Second {
		t.Errorf("после override: %s, want 3s (hot-apply не работает)", got)
	}
}

// TestR298_StallSettingsZeroDisablesGarbageFallsBack — контракт env-значения:
// отрицательное число — это МУСОР (опечатка), а не «выключить»: NormalizeEnv
// возвращает дефолт настройки, как и для остальных горячих лимитов. Выключает
// сторож ровно ноль.
func TestR298_StallSettingsZeroDisablesGarbageFallsBack(t *testing.T) {
	withSettingsRegistry(t)

	t.Setenv("LB_STREAM_STALL_ABORT_SEC", "-15")
	t.Setenv("LB_STREAM_FIRST_FRAME_ABORT_SEC", "-15")
	// Реестр читает env при создании, поэтому пересоздаём его после Setenv.
	settings.Init("")
	if got := streamStallAbortTimeout(); got != streamStallAbortTimeoutDefault {
		t.Errorf("LB_STREAM_STALL_ABORT_SEC=-15: %s, want дефолт %s (мусор → дефолт, а не выключение)",
			got, streamStallAbortTimeoutDefault)
	}
	if got := streamFirstFrameAbortTimeout(); got != 0 {
		t.Errorf("LB_STREAM_FIRST_FRAME_ABORT_SEC=-15: %s, want дефолт 0", got)
	}

	// Явный ноль — вот это и есть «выключить».
	t.Setenv("LB_STREAM_STALL_ABORT_SEC", "0")
	settings.Init("")
	if got := streamStallAbortTimeout(); got != 0 {
		t.Errorf("LB_STREAM_STALL_ABORT_SEC=0: %s, want 0 (сторож выключен)", got)
	}
	if err := settings.Default().ResetAll(); err != nil {
		t.Fatalf("ResetAll: %v", err)
	}
}

// --- 3. Поведение: подвисший upstream на пути /api/chat --------------------

// hangingNDJSONUpstream — воркер, который отдал пару кадров и «завис»:
// соединение живо, данных нет, [DONE] не приходит. Именно это и было на стенде
// (docker pause / зависший llama_decode).
func hangingNDJSONUpstream(t *testing.T, chunks int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !(strings.HasSuffix(r.URL.Path, "/api/chat") ||
			strings.HasSuffix(r.URL.Path, "/v1/chat/completions")) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		isNative := strings.HasSuffix(r.URL.Path, "/api/chat")
		if isNative {
			w.Header().Set("Content-Type", "application/x-ndjson")
		} else {
			w.Header().Set("Content-Type", "text/event-stream")
		}
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for i := 0; i < chunks; i++ {
			var payload []byte
			if isNative {
				payload, _ = json.Marshal(map[string]interface{}{
					"model":      "gemma-4-E4B-it-Q4_K_M",
					"created_at": "2026-10-29T00:00:00Z",
					"message":    map[string]interface{}{"role": "assistant", "content": fmt.Sprintf("chunk-%d", i)},
					"done":       false,
				})
				payload = append(payload, '\n')
			} else {
				body, _ := json.Marshal(map[string]interface{}{
					"id":      "chatcmpl-hang",
					"object":  "chat.completion.chunk",
					"created": 1700000000,
					"model":   "gemma-4-E4B-it-Q4_K_M",
					"choices": []map[string]interface{}{{
						"index":         0,
						"delta":         map[string]interface{}{"content": fmt.Sprintf("chunk-%d", i)},
						"finish_reason": nil,
					}},
				})
				payload = []byte("data: " + string(body) + "\n\n")
			}
			if _, err := w.Write(payload); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		// Зависаем: ни данных, ни закрытия. Выходим только когда клиент уйдёт.
		<-r.Context().Done()
	}))
}

// TestR298_HangingUpstream_GetsReasonAndOneTerminator — главный поведенческий
// тест R298. Раньше такой стрим висел вечно (терминатора нет, причина не
// названа), и клиент Cline получал «ответ не пришёл».
func TestR298_HangingUpstream_GetsReasonAndOneTerminator(t *testing.T) {
	reg := withSettingsRegistry(t)
	if _, err := reg.Set("LB_STREAM_STALL_ABORT_SEC", 1); err != nil {
		t.Fatalf("Set: %v", err)
	}

	upstream := hangingNDJSONUpstream(t, 2)
	defer upstream.Close()

	p, _, cleanup := makeTestLlamaProxy(t, upstream.URL)
	defer cleanup()

	bodyBytes, _ := json.Marshal(map[string]interface{}{
		"model":    "gemma-4-E4B-it-Q4_K_M",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	// Контекст заведомо длиннее порога сторожа, но короче «вечности»: тест
	// обязан упасть, если сторож не работает, а не висеть сам.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	start := time.Now()
	if err := p.proxyRequestLlamaCpp(rec, req, "llama_test", bodyBytes); err != nil {
		t.Fatalf("proxyRequestLlamaCpp: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 15*time.Second {
		t.Fatalf("стрим вернулся через %s — сторож не сработал (порог 1с)", elapsed)
	}

	chunks := parseNDJSONResponse(t, bytes.NewReader(rec.Body.Bytes()))
	if len(chunks) == 0 {
		t.Fatalf("тело пустое: клиент не получил ни причины, ни терминатора; body=%q", rec.Body.String())
	}

	var reasonChunk, terminal map[string]interface{}
	terminators := 0
	for _, c := range chunks {
		if done, _ := c["done"].(bool); done {
			terminators++
			terminal = c
			continue
		}
		if _, hasErr := c["error"]; hasErr {
			reasonChunk = c
		}
	}

	if reasonChunk == nil {
		t.Fatalf("нет кадра-причины (done:false + error); body=%s", rec.Body.String())
	}
	if got, _ := reasonChunk["error_type"].(string); got != streamAbortUpstreamStall {
		t.Errorf("error_type = %q, want %q (виноват зависший upstream, а не EOF)", got, streamAbortUpstreamStall)
	}
	if got, _ := reasonChunk["error"].(string); got != streamAbortClientMessage(streamAbortUpstreamStall) {
		t.Errorf("error = %q, want %q", got, streamAbortClientMessage(streamAbortUpstreamStall))
	}
	if terminal == nil {
		t.Fatalf("нет терминатора (done:true); body=%s", rec.Body.String())
	}
	if got, _ := terminal["done_reason"].(string); got != "error" {
		t.Errorf("done_reason = %q, want \"error\" (обрыв — не естественный конец)", got)
	}
	if terminators != 1 {
		t.Errorf("терминаторов %d, want ровно 1; body=%s", terminators, rec.Body.String())
	}
	t.Logf("OK: зависший upstream снят за %s, причина=%s, терминаторов=%d", elapsed.Round(time.Millisecond), streamAbortUpstreamStall, terminators)
}

// TestR298_WatchdogOffMeansWaitForever — выключенный сторож не рвёт стрим:
// контракт «0 = прежнее поведение» соблюдается (оператор может вернуть
// LB_STREAMING_NEVER_TIMEOUT-семантику одной настройкой).
func TestR298_WatchdogOffMeansWaitForever(t *testing.T) {
	reg := withSettingsRegistry(t)
	if _, err := reg.Set("LB_STREAM_STALL_ABORT_SEC", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, err := reg.Set("LB_STREAM_FIRST_FRAME_ABORT_SEC", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}

	upstream := hangingNDJSONUpstream(t, 2)
	defer upstream.Close()
	p, _, cleanup := makeTestLlamaProxy(t, upstream.URL)
	defer cleanup()

	bodyBytes, _ := json.Marshal(map[string]interface{}{
		"model":    "gemma-4-E4B-it-Q4_K_M",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	// Клиент сам сдаётся через 300мс — так проверяется именно то, что сторож
	// выключен, а не то, что тест дождался пяти минут.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	req = req.WithContext(ctx)

	_ = p.proxyRequestLlamaCpp(rec, req, "llama_test", bodyBytes)
	for _, c := range parseNDJSONResponse(t, bytes.NewReader(rec.Body.Bytes())) {
		if got, _ := c["error_type"].(string); got == streamAbortUpstreamStall {
			t.Fatalf("сторож выключен, но стрим помечен как %q", got)
		}
	}
}

// waitFiredR298 — дождаться срабатывания через Fired() (блокирующие пути узнают
// о стороже именно так, а не через канал).
func waitFiredR298(t *testing.T, w *streamStallWatch, max time.Duration) (stallEvent, bool) {
	t.Helper()
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if ev, fired := w.Fired(); fired {
			return ev, true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return stallEvent{}, false
}
