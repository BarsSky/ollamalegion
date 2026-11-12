// stream_golden_t2_test.go — T2 из плана «два мозга» (refactoring-plan.ru.md §3):
// golden-байты NDJSON/SSE и правило «РОВНО ОДИН терминатор».
//
// ЗАЧЕМ. Клиентские контракты стрима разъезжались не раз (R252 «поток закрылся без
// [DONE]», R261 «кадры после конца потока», R377 «два сборщика терминального
// кадра»). T2 фиксирует то, что видит клиент, побайтово: если форма кадра или
// число терминаторов изменится, тест это покажет, а не живой пользователь.
//
// ЖИВОЙ ДЕФЕКТ, НАЙДЕННЫЙ ЭТИМ ТЕСТОМ (2026-11-12). На /v1/chat/completions
// клиент получал `data: [DONE]` ДВАЖДЫ: поток отдавал маркер апстрима, а гарантия
// R252 решала «нужен ли терминатор» по последнему кадру, записанному циклом, — и
// дописывала второй. Исправлено трекером записанных байт (sseDoneTrackerR252):
// «ушёл ли терминатор» — факт о потоке, а не о переменной.
//
// Шесть комбинаций = три поверхности × {stream, не-stream}. Апстрим честно
// соблюдает поле stream: на не-stream отвечает JSON, на stream — NDJSON (Ollama-
// поверхность) или SSE (OpenAI-поверхность).
package balancer

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// t2Upstream — фейковый cppworker. Отдаёт /api/models (иначе гейт R325 не пустит
// запрос), а на инференс — тот формат, который соответствует поверхности и полю
// stream из запроса.
func t2Upstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/models" || r.URL.Path == "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"count":1,"models":[{"name":"m","path":"/app/models/m.gguf","state":"loaded","context_length":65536,"max_slots":1}]}`)
		case strings.HasPrefix(r.URL.Path, "/v1/"):
			stream := strings.Contains(readAllStringT2(r), `"stream":true`)
			if !stream {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		default:
			stream := strings.Contains(readAllStringT2(r), `"stream":true`)
			if !stream {
				w.Header().Set("Content-Type", "application/json")
				if strings.Contains(r.URL.Path, "generate") {
					fmt.Fprint(w, `{"model":"m","response":"hi","done":true,"done_reason":"stop"}`)
				} else {
					fmt.Fprint(w, `{"model":"m","message":{"role":"assistant","content":"hi"},"done":true,"done_reason":"stop"}`)
				}
				return
			}
			w.Header().Set("Content-Type", "application/x-ndjson")
			if strings.Contains(r.URL.Path, "generate") {
				fmt.Fprint(w, "{\"model\":\"m\",\"response\":\"hi\",\"done\":false}\n")
				fmt.Fprint(w, "{\"model\":\"m\",\"response\":\"\",\"done\":true}\n")
			} else {
				fmt.Fprint(w, "{\"model\":\"m\",\"message\":{\"role\":\"assistant\",\"content\":\"hi\"},\"done\":false}\n")
				fmt.Fprint(w, "{\"model\":\"m\",\"message\":{\"role\":\"assistant\",\"content\":\"\"},\"done\":true}\n")
			}
		}
	}))
}

// readAllStringT2 — тело запроса строкой (нужно только чтобы понять stream).
func readAllStringT2(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	buf := make([]byte, 4096)
	n, _ := r.Body.Read(buf)
	return string(buf[:n])
}

// t2Proxy — прокси с одним llama.cpp-узлом, смотрящим на фейковый апстрим.
func t2Proxy(t *testing.T, srv *httptest.Server, modelLoaded bool) *Proxy {
	t.Helper()
	hostPort := strings.TrimPrefix(srv.URL, "http://")
	host := hostPort[:strings.LastIndex(hostPort, ":")]
	port, err := strconv.Atoi(hostPort[strings.LastIndex(hostPort, ":")+1:])
	if err != nil {
		t.Fatalf("порт тестового апстрима: %v", err)
	}
	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 8080, APIPort: 8081},
		Backends: []types.Backend{{
			ID: "b1", Name: "b1", Host: host, OllamaPort: port, CppWorkerPort: port,
			Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
			MaxConcurrentReqs: 1, Weight: 1,
		}},
		Balancing: types.BalancingSettings{Algorithm: types.AlgorithmResourceAware, ModelAffinity: true},
	}
	p := newProxyWithCleanup(t, conf)
	if modelLoaded {
		p.SetBackendMetrics("b1", &types.BackendMetrics{
			ID: "b1", Status: types.StatusHealthy,
			LlamaCpp: types.LlamaCppMetrics{
				LoadedModels: []types.LlamaCppModel{{Name: "m", State: "loaded", ContextLength: 65536, MaxSlots: 1}},
				VramKnown:    true,
			},
		})
	}
	return p
}

// TestT2_SixCombinationsGolden — golden-байты и ровно один терминатор.
func TestT2_SixCombinationsGolden(t *testing.T) {
	srv := t2Upstream(t)
	defer srv.Close()

	cases := []struct {
		name string
		path string
		body string
		// wantBody — что обязан увидеть клиент (golden). Для стримов терминатор
		// входит в golden: их число — часть контракта.
		wantBody string
		// terminator — подстрока терминатора этого формата ("" для не-stream).
		terminator string
	}{
		{
			name: "api/chat stream",
			path: "/api/chat",
			body: `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			wantBody: `{"model":"m","message":{"role":"assistant","content":"hi"},"done":false}` + "\n" +
				`{"done":true,"done_reason":"stop","message":{"content":"","role":"assistant"},"model":"m"}` + "\n",
			terminator: `"done":true`,
		},
		{
			name:     "api/chat non-stream",
			path:     "/api/chat",
			body:     `{"model":"m","stream":false,"messages":[{"role":"user","content":"hi"}]}`,
			wantBody: `{"model":"m","message":{"role":"assistant","content":"hi"},"done":true,"done_reason":"stop"}`,
		},
		{
			name: "api/generate stream",
			path: "/api/generate",
			body: `{"model":"m","stream":true,"prompt":"hi"}`,
			wantBody: `{"model":"m","response":"hi","done":false}` + "\n" +
				`{"done":true,"done_reason":"stop","model":"m","response":""}` + "\n",
			terminator: `"done":true`,
		},
		{
			name:     "api/generate non-stream",
			path:     "/api/generate",
			body:     `{"model":"m","stream":false,"prompt":"hi"}`,
			wantBody: `{"model":"m","response":"hi","done":true,"done_reason":"stop"}`,
		},
		{
			name: "v1/chat/completions stream",
			path: "/v1/chat/completions",
			body: `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			// Ведущий "\n" — граница SSE-события R277 (withSSEEventBoundary): первый
			// кадр отдаётся после пустой строки-разделителя. Клиенты это игнорируют,
			// но golden фиксирует фактические байты.
			wantBody: "\n" + `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}` + "\n\n" +
				`data: [DONE]` + "\n\n",
			terminator: "data: [DONE]",
		},
		{
			name:     "v1/chat/completions non-stream",
			path:     "/v1/chat/completions",
			body:     `{"model":"m","stream":false,"messages":[{"role":"user","content":"hi"}]}`,
			wantBody: `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := t2Proxy(t, srv, true)
			req := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(c.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			p.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("статус %d, ожидался 200; тело: %q", w.Code, w.Body.String())
			}
			got := w.Body.String()
			if got != c.wantBody {
				t.Fatalf("golden разошёлся.\nполучено: %q\nожидалось: %q", got, c.wantBody)
			}
			if c.terminator == "" {
				return
			}
			// «РОВНО один терминатор»: два [DONE]/два done:true — это два конца
			// потока, и клиент видит ответ оборванным (живой случай R252/T2).
			if n := strings.Count(got, c.terminator); n != 1 {
				t.Fatalf("терминаторов %q: %d, ожидался ровно один.\nтело: %q", c.terminator, n, got)
			}
		})
	}
}
