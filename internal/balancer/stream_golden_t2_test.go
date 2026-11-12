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
	"encoding/json"
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
			// R493-12a: нативный done-кадр теперь несёт длительности — их дополняет
			// балансер (load_duration и total = load+prompt+eval). Сами числа
			// переменные по определению, поэтому golden фиксирует ФОРМУ кадра с
			// обнулёнными длительностями (normalizeGoldenDurationsT2), а наличие
			// полей и инвариант суммы проверяются отдельно ниже.
			wantBody: `{"model":"m","message":{"role":"assistant","content":"hi"},"done":false}` + "\n" +
				`{"done":true,"done_reason":"stop","eval_duration":0,"load_duration":0,"message":{"content":"","role":"assistant"},"model":"m","prompt_eval_duration":0,"total_duration":0}` + "\n",
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
				`{"done":true,"done_reason":"stop","eval_duration":0,"load_duration":0,"model":"m","prompt_eval_duration":0,"response":"","total_duration":0}` + "\n",
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
			// R493-12a: нативные кадры несут ДЛИТЕЛЬНОСТИ, а они переменные по
			// определению (время загрузки/префилла/генерации). Golden фиксирует
			// форму кадра, а не числа: иначе он «мигал» бы на каждой машине.
			// Наличие полей и инвариант «сумма отрезков = total» проверяются ниже.
			compare := normalizeGoldenDurationsT2(t, got, c.path)
			want := normalizeGoldenDurationsT2(t, c.wantBody, c.path)
			if compare != want {
				t.Fatalf("golden разошёлся.\nполучено: %q\nожидалось: %q", compare, want)
			}
			if c.terminator == "" {
				return
			}
			// «РОВНО один терминатор»: два [DONE]/два done:true — это два конца
			// потока, и клиент видит ответ оборванным (живой случай R252/T2).
			if n := strings.Count(got, c.terminator); n != 1 {
				t.Fatalf("терминаторов %q: %d, ожидался ровно один.\nтело: %q", c.terminator, n, got)
			}
			// R493-12a: у нативного клиента ОБЯЗАНЫ быть все три отрезка, и они
			// обязаны складываться в total_duration. Без этого «скорость токенов»
			// снова посчитается по чужому окну (живой замер: ждал 62 с, видел 2.15 с).
			if c.path == "/api/chat" || c.path == "/api/generate" {
				assertNativeStatsWindowsT2(t, got)
			}
		})
	}
}

// normalizeGoldenDurationsT2 — обнулить переменные длительности в NDJSON-кадрах,
// чтобы golden сравнивал форму, а не время. Другие пути не трогаем.
func normalizeGoldenDurationsT2(t *testing.T, body, path string) string {
	t.Helper()
	if path != "/api/chat" && path != "/api/generate" {
		return body
	}
	keys := []string{"load_duration", "prompt_eval_duration", "eval_duration", "total_duration", "tokens_per_second"}
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("кадр не JSON: %v (%q)", err, ln)
		}
		for _, k := range keys {
			if _, ok := m[k]; ok {
				if k == "tokens_per_second" {
					delete(m, k) // процент/скорость в golden не фиксируем вовсе
					continue
				}
				m[k] = 0
			}
		}
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("кадр не сериализуется: %v", err)
		}
		out = append(out, string(b))
	}
	return strings.Join(out, "\n") + "\n"
}

// assertNativeStatsWindowsT2 — на нативном пути клиент получает три отрезка,
// и их сумма равна total_duration.
func assertNativeStatsWindowsT2(t *testing.T, body string) {
	t.Helper()
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	last := lines[len(lines)-1]
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(last), &m); err != nil {
		t.Fatalf("последний кадр не JSON: %v (%q)", err, last)
	}
	nums := make(map[string]float64, 4)
	for _, k := range []string{"load_duration", "prompt_eval_duration", "eval_duration", "total_duration"} {
		v, ok := m[k].(float64)
		if !ok {
			t.Fatalf("в нативном done-кадре нет %s — клиент не узнает, сколько ждал: %q", k, last)
		}
		nums[k] = v
	}
	sum := nums["load_duration"] + nums["prompt_eval_duration"] + nums["eval_duration"]
	if diff := nums["total_duration"] - sum; diff > 1e6 || diff < -1e6 {
		t.Fatalf("сумма отрезков %.0f != total_duration %.0f (кадр: %q)", sum, nums["total_duration"], last)
	}
}
