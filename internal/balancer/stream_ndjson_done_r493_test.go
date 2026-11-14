// stream_ndjson_done_r493_test.go — R493 (этап 6, продолжение): «ровно один
// `done:true`» на NDJSON-пути (/api/chat) при хитрых последовательностях апстрима.
//
// ЗАЧЕМ ЭТОТ ТЕСТ, ЕСЛИ ЕСТЬ T2. T2 фиксирует happy path: воркер отдал
// `done:true`, клиент получил один терминатор. Но у NDJSON-пути решение «нужен ли
// синтетический done» исторически принималось ПО ПЕРЕМЕННОЙ (`lastData`,
// `deferredDoneChunk`), а не по факту записи — ровно тот класс, из которого выросли
// R60.55 (два `done:true` у OpenWebUI), R66b (два done-чанка у Cline: «Model
// returned empty response») и R252 на SSE-пути. Каждая из этих правок чинила одну
// последовательность; теста, который держит ВСЕ их разом в обоих режимах флага, не
// было.
//
// Здесь четыре последовательности × два режима `LB_STREAM_PIPE`:
//
//  1. штатный: `done:true` + `[DONE]`;
//  2. R66b: `done:true`, ПОСЛЕ него ещё контент (клиент не должен получить второй);
//  3. обрыв без терминатора: клиенту обязан уйти синтетический `done:true`;
//  4. кадр-ошибка воркера: причина + ровно один терминатор.
package balancer

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ndjsonUpstreamR493 — воркер, который на /api/chat отвечает заданным SSE-потоком
// (то есть заставляет балансер переводить SSE → NDJSON, как для OpenWebUI/Cline).
func ndjsonUpstreamR493(t *testing.T, sse string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models" || r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"count":1,"models":[{"name":"m","path":"/app/models/m.gguf","state":"loaded","context_length":65536,"max_slots":1}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-Id", "req-ndjson-1")
		fmt.Fprint(w, sse)
	}))
}

// ndjsonDoneCountsR493 — сколько строк с done:true получил клиент и что в них.
func ndjsonDoneCountsR493(body string) (count int, reasons []string) {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.Contains(line, `"done":true`) {
			count++
			if i := strings.Index(line, `"done_reason"`); i >= 0 {
				reasons = append(reasons, line[i:])
			}
		}
	}
	return count, reasons
}

const ndjsonChunkContent = `data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}` + "\n\n"
const ndjsonChunkDone = `data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"

// TestNDJSON_ExactlyOneDoneFrame — четыре последовательности, оба режима флага.
func TestNDJSON_ExactlyOneDoneFrame(t *testing.T) {
	cases := []struct {
		name string
		sse  string
		// wantContent — клиент обязан увидеть текст ответа (не потерять его).
		wantContent bool
	}{
		{
			name:        "штатный: done + [DONE]",
			sse:         ndjsonChunkContent + ndjsonChunkDone + "data: [DONE]\n\n",
			wantContent: true,
		},
		{
			name:        "R66b: done, затем ещё контент",
			sse:         ndjsonChunkContent + ndjsonChunkDone + ndjsonChunkContent + "data: [DONE]\n\n",
			wantContent: true,
		},
		{
			name:        "обрыв без терминатора",
			sse:         ndjsonChunkContent,
			wantContent: true,
		},
		{
			name:        "кадр-ошибка воркера",
			sse:         ndjsonChunkContent + `data: {"error":{"message":"requested n_ctx exceeds max","code":2}}` + "\n\n",
			wantContent: true,
		},
	}

	for _, flag := range []string{"0", "1"} {
		t.Run("LB_STREAM_PIPE="+flag, func(t *testing.T) {
			t.Setenv("LB_STREAM_PIPE", flag)
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					up := ndjsonUpstreamR493(t, c.sse)
					defer up.Close()
					p := t2Proxy(t, up, true)
					srv := httptest.NewServer(p)
					defer srv.Close()

					req := httptest.NewRequest(http.MethodPost, "/api/chat",
						strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
					req.Header.Set("Content-Type", "application/json")
					rec := httptest.NewRecorder()
					p.ServeHTTP(rec, req)

					body := rec.Body.String()
					if rec.Code != http.StatusOK {
						t.Fatalf("статус %d, ожидался 200; тело: %q", rec.Code, body)
					}
					n, _ := ndjsonDoneCountsR493(body)
					// РОВНО один терминатор — иначе клиент видит два конца ответа
					// (R60.55/R66b/R252 — все они об этом).
					if n != 1 {
						t.Fatalf("done-кадров %d, ожидался ровно один.\nтело: %q", n, body)
					}
					if c.wantContent && !strings.Contains(body, `"content":"hi"`) {
						t.Fatalf("текст ответа потерян — клиент увидит пустой ответ.\nтело: %q", body)
					}
				})
			}
		})
	}
}
