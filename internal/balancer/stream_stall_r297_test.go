//go:build llama_stub

// stream_stall_r297_test.go — R297 (2026-10-29): сторож зависшего стрима.
//
// ЖИВОЙ ДЕФЕКТ: поток «повис» — последний кадр в 15:01:20, соединение открыто,
// терминатора нет, клиент ждёт вечно. На стенде `LB_STREAMING_NEVER_TIMEOUT=1`
// отключает все общие idle-таймауты, поэтому тишину посреди потока не ловил
// никто.
//
// Здесь проверяется ПОВЕДЕНИЕ, а не факт наличия кода:
//   - upstream отдаёт ОДИН кадр и замолкает, НЕ закрывая соединение (мёртвый-
//     но-не-закрытый воркер — ровно то, что было на стенде);
//   - клиент обязан получить кадр-причину и РОВНО ОДИН терминатор + `[DONE]`
//     в пределах порога, а не ждать вечно.
//
// Второй тест держит границу: до ПЕРВОГО кадра сторож не срабатывает (молчание
// в начале — это честный prefill, его обрывать нельзя).
package balancer

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestR297_StallTimeoutSetting — порог живёт в реестре настроек (одно место
// правды), а не константой в горячем пути.
func TestR297_StallTimeoutSetting(t *testing.T) {
	if got := streamStallAbortTimeout(); got != streamStallAbortTimeoutDefault {
		t.Fatalf("по умолчанию: %v, ожидалось %v", got, streamStallAbortTimeoutDefault)
	}

	t.Setenv("LB_STREAM_STALL_ABORT_SEC", "7")
	if got := streamStallAbortTimeout(); got != 7*time.Second {
		t.Fatalf("из окружения: %v, ожидалось 7s", got)
	}

	reg := withSettingsRegistry(t)
	if _, err := reg.Set("LB_STREAM_STALL_ABORT_SEC", 2); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := streamStallAbortTimeout(); got != 2*time.Second {
		t.Fatalf("override из WebUI/API: %v, ожидалось 2s (настройка не горячая?)", got)
	}

	// 0 = оператор сознательно выключил сторож (прежнее «ждать вечно»).
	if _, err := reg.Set("LB_STREAM_STALL_ABORT_SEC", 0); err != nil {
		t.Fatalf("Set(0): %v", err)
	}
	if got := streamStallAbortTimeout(); got != 0 {
		t.Fatalf("0 должно отключать сторож, получено %v", got)
	}
}

// stallProxy — минимальная обвязка: единственный upstream, который отдаёт
// заданное число кадров и затем МОЛЧИТ, не закрывая соединение.
func stallSetupR297(t *testing.T, firstChunks int, stall time.Duration) (client *http.Client, url string, stop func()) {
	t.Helper()

	stopCh := make(chan struct{})
	var stopOnce sync.Once

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for i := 0; i < firstChunks; i++ {
			_, _ = io.WriteString(w, `data: {"id":"chat-1","object":"chat.completion.chunk","created":1700000000,"model":"stall-model","choices":[{"index":0,"delta":{"content":"first"},"finish_reason":null}]}`+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
		// Воркер ЖИВ, но данных больше не будет: соединение не закрывается.
		// Именно это и есть «зависший, но не умерший» upstream.
		//
		// Выход по stopCh — чтобы закрытие теста не зависело от того, дойдёт ли
		// отмена контекста (иначе Cleanup ждёт вечно, и весь прогон висит).
		select {
		case <-r.Context().Done():
		case <-stopCh:
		}
	}))

	p := makeTestProxy(1) // heartbeat-тик 1 с: сторож проверяется на нём же
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upReq, err := http.NewRequestWithContext(r.Context(), "POST", upstream.URL+"/v1/chat/completions", strings.NewReader(`{}`))
		if err != nil {
			t.Logf("upstream request build: %v", err)
			return
		}
		resp, err := http.DefaultClient.Do(upReq)
		if err != nil {
			t.Logf("upstream request: %v", err)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("writer не поддерживает Hijack — тест не воспроизводит живой путь")
			resp.Body.Close()
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), modelContextKey, "stall-model"))
		if err := p.proxyRequestOpenAIStreamingHijacked(w, r, resp, "stall-backend", hj); err != nil {
			t.Logf("proxyRequestOpenAIStreamingHijacked: %v", err)
		}
	}))

	return &http.Client{Timeout: stall}, proxySrv.URL + "/v1/chat/completions", func() {
		stopOnce.Do(func() {
			close(stopCh)
			// Сначала рвём соединения: иначе Close() ждёт горутины обработчиков,
			// которые могут стоять в записи клиенту.
			proxySrv.CloseClientConnections()
			proxySrv.Close()
			upstream.CloseClientConnections()
			upstream.Close()
		})
	}
}

// TestR297_StallWatchdogEmitsTerminator — тишина посреди потока закрывается
// кадром-причиной и ровно одним терминатором.
func TestR297_StallWatchdogEmitsTerminator(t *testing.T) {
	reg := withSettingsRegistry(t)
	if _, err := reg.Set("LB_STREAM_STALL_ABORT_SEC", 2); err != nil {
		t.Fatalf("Set: %v", err)
	}

	client, url, stop := stallSetupR297(t, 1, 20*time.Second)
	defer stop()

	start := time.Now()
	resp, err := client.Post(url, "application/json", strings.NewReader(`{"model":"stall-model","stream":true}`))
	if err != nil {
		t.Fatalf("клиент не получил ответ: %v (сторож не сработал — поток висел ?)", err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	elapsed := time.Since(start)
	if readErr != nil {
		t.Fatalf("чтение тела: %v (поток не был закрыт терминатором)", readErr)
	}
	text := string(body)

	if !strings.Contains(text, `"content":"first"`) {
		t.Errorf("первый кадр воркера не доехал до клиента; body=%s", text)
	}
	if c := strings.Count(text, `"finish_reason":"error"`); c != 1 {
		t.Errorf("ожидался РОВНО один терминатор с finish_reason=error, получено %d; body=%s", c, text)
	}
	if c := strings.Count(text, "data: [DONE]"); c != 1 {
		t.Errorf("ожидался РОВНО один data: [DONE], получено %d; body=%s", c, text)
	}
	// R298: причина срабатывания сторожа теперь называется своим именем —
	// `upstream_stall` (модель перестала присылать данные), а не generic
	// `upstream_error` (ошибка ЧТЕНИЯ, которой тут не было: соединение живое).
	if !strings.Contains(text, `"reason":"`+streamAbortUpstreamStall+`"`) {
		t.Errorf("в кадре-причине нет reason=%s; body=%s", streamAbortUpstreamStall, text)
	}
	// Порог 2 с + тик 1 с: больше 10 с — значит сторож не работает, а поток
	// закрылся по чему-то другому (например, по клиентскому таймауту).
	if elapsed > 10*time.Second {
		t.Errorf("сторож сработал через %v при пороге 2s — это не сторож", elapsed)
	}
	t.Logf("OK: зависший поток закрыт за %v; кадров-причин=1, терминатор=1", elapsed)
}

// TestR297_StallWatchdogWaitsForFirstFrame — ДО первого кадра сторож молчит:
// иначе мы бы убивали честный prefill (CPU-модель считает промпт минутами).
func TestR297_StallWatchdogWaitsForFirstFrame(t *testing.T) {
	reg := withSettingsRegistry(t)
	if _, err := reg.Set("LB_STREAM_STALL_ABORT_SEC", 2); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Ни одного кадра: upstream принимает запрос и молчит.
	client, url, stop := stallSetupR297(t, 0, 5*time.Second)
	defer stop()

	start := time.Now()
	resp, err := client.Post(url, "application/json", strings.NewReader(`{"model":"stall-model","stream":true}`))
	elapsed := time.Since(start)
	if err == nil {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("сторож закрыл поток ДО первого кадра за %v; body=%s — prefill убит", elapsed, string(body))
	}
	// Ожидаемо: клиентский таймаут (5 с) раньше порога сторожа (он не включён).
	if elapsed < 4*time.Second {
		t.Errorf("поток закрылся за %v — слишком рано для клиентского таймаута 5s", elapsed)
	}
	t.Logf("OK: до первого кадра сторож не срабатывает (клиентский таймаут через %v)", elapsed)
}
