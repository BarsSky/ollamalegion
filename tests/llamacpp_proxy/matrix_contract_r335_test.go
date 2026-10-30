// matrix_contract_r335_test.go — ЭТАП 2 глобальной проверки API (R335, 2026-10-30).
//
// ЗАЧЕМ ВТОРАЯ МАТРИЦА, ЕСЛИ ЕСТЬ ЖИВАЯ (tests/api_contract/live_matrix_test.go).
// Живая матрица требует поднятого стенда и загруженной модели: её нельзя гонять в
// обычном `go test`, и она не проверяет поведение балансера на «плохих» ответах
// воркера. Этот набор идёт на контрактной заглушке (`contractStub` из
// client_api_contract_r118_test.go): быстро, детерминированно, в CI.
//
// Инварианты те же, что в живой матрице:
//   - статус и Content-Type каждого клиентского маршрута обеих поверхностей;
//   - обязательные поля тела по контракту Ollama и OpenAI;
//   - стрим: каждый кадр — JSON-объект с model/message, терминатор РОВНО один и он
//     последний, done_reason из реестра R328;
//   - SSE: кадры `data:`, финальный `data: [DONE]` последним;
//   - негативные кейсы: неизвестный путь (обе поверхности), битый JSON,
//     неизвестная модель — JSON-тело с полем error.
package llamacpp_proxy

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"ollama-loadbalancer/internal/balancer"
)

// matrixModel — имя модели в заглушке.
const matrixModel = "matrix-contract-model"

// matrixProxy — пара «заглушка + прокси с загруженной моделью».
type matrixProxy struct {
	stub  *contractStub
	proxy *balancer.Proxy
}

// newMatrixProxy — поднять заглушку и прокси для матрицы.
func newMatrixProxy(t *testing.T) *matrixProxy {
	t.Helper()
	stub := newContractStub(t, matrixModel)
	t.Cleanup(stub.Close)
	return &matrixProxy{stub: stub, proxy: newContractProxy(t, stub, matrixModel)}
}

// matrixDo — запрос через клиентскую поверхность балансера.
func matrixDo(t *testing.T, mp *matrixProxy, method, path, body string) (*http.Response, string) {
	t.Helper()
	resp := doContract(mp.proxy, method, path, body, nil)
	return resp, readAll(t, resp)
}

// allowedDoneReason — копия реестра R328 для внешнего пакета.
func allowedDoneReason(reason string) bool {
	switch reason {
	case "stop", "length", "tool_calls", "error", "cancelled":
		return true
	}
	return false
}

// TestR335_OllamaReadOnlyMatrix — информационные маршруты Ollama-поверхности.
func TestR335_OllamaReadOnlyMatrix(t *testing.T) {
	mp := newMatrixProxy(t)
	cases := []struct {
		method, path, body string
		need               []string
	}{
		{http.MethodGet, "/api/version", "", []string{"version"}},
		{http.MethodGet, "/api/tags", "", []string{"models"}},
		{http.MethodGet, "/api/ps", "", []string{"models"}},
		{http.MethodPost, "/api/show", `{"name":"` + matrixModel + `"}`, []string{"model", "details"}},
		{http.MethodGet, "/api/models", "", []string{"models"}},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			resp, raw := matrixDo(t, mp, tc.method, tc.path, tc.body)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("статус %d, want 200; тело: %s", resp.StatusCode, raw)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "json") {
				t.Errorf("Content-Type = %q, want JSON", ct)
			}
			m := assertJSONObject(t, raw, tc.path)
			for _, field := range tc.need {
				if _, ok := m[field]; !ok {
					t.Errorf("нет поля %q: %s", field, raw)
				}
			}
		})
	}
}

// TestR335_OllamaChatNonStreamMatrix — /api/chat без стрима.
func TestR335_OllamaChatNonStreamMatrix(t *testing.T) {
	mp := newMatrixProxy(t)
	body := `{"model":"` + matrixModel + `","messages":[{"role":"user","content":"hi"}],"stream":false}`
	resp, raw := matrixDo(t, mp, http.MethodPost, "/api/chat", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус %d, want 200; тело: %s", resp.StatusCode, raw)
	}
	m := assertJSONObject(t, raw, "/api/chat")
	msg, _ := m["message"].(map[string]interface{})
	if msg == nil {
		t.Fatalf("нет message: %s", raw)
	}
	if role, _ := msg["role"].(string); role != "assistant" {
		t.Errorf("message.role = %q, want assistant", role)
	}
	if done, _ := m["done"].(bool); !done {
		t.Errorf("done != true: %s", raw)
	}
	if reason, _ := m["done_reason"].(string); reason != "" && !allowedDoneReason(reason) {
		t.Errorf("done_reason = %q вне контракта", reason)
	}
}

// TestR335_OllamaChatStreamMatrix — /api/chat потоком: форма каждого кадра.
func TestR335_OllamaChatStreamMatrix(t *testing.T) {
	mp := newMatrixProxy(t)
	body := `{"model":"` + matrixModel + `","messages":[{"role":"user","content":"hi"}],"stream":true}`
	resp, raw := matrixDo(t, mp, http.MethodPost, "/api/chat", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус %d, want 200; тело: %s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "ndjson") {
		t.Errorf("Content-Type = %q, want application/x-ndjson", ct)
	}
	if resp.Header.Get("X-Backend-ID") == "" {
		t.Error("нет заголовка X-Backend-ID")
	}

	var frames []map[string]interface{}
	sc := bufio.NewScanner(strings.NewReader(raw))
	sc.Buffer(make([]byte, 0), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue // heartbeat-пустая строка (R333) — кадром не считается
		}
		var f map[string]interface{}
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			t.Fatalf("кадр не JSON: %v (%q)", err, line)
		}
		frames = append(frames, f)
	}
	if len(frames) == 0 {
		t.Fatal("поток пуст")
	}
	doneIdx := -1
	for i, f := range frames {
		for _, field := range []string{"model", "message"} {
			if _, ok := f[field]; !ok {
				t.Errorf("кадр %d: нет поля %q (не объект контракта Ollama): %v", i, field, f)
			}
		}
		if done, _ := f["done"].(bool); done {
			if doneIdx >= 0 {
				t.Errorf("второй терминатор: кадры %d и %d", doneIdx, i)
			}
			doneIdx = i
			if reason, _ := f["done_reason"].(string); !allowedDoneReason(reason) {
				t.Errorf("кадр %d: done_reason = %q вне контракта", i, reason)
			}
		}
	}
	if doneIdx < 0 {
		t.Fatal("нет кадра done:true")
	}
	if doneIdx != len(frames)-1 {
		t.Errorf("после терминатора ещё %d кадров", len(frames)-1-doneIdx)
	}
}

// TestR335_OllamaGenerateAndEmbedMatrix — /api/generate, /api/embed, /api/embeddings.
func TestR335_OllamaGenerateAndEmbedMatrix(t *testing.T) {
	mp := newMatrixProxy(t)

	t.Run("/api/generate", func(t *testing.T) {
		body := `{"model":"` + matrixModel + `","prompt":"hi","stream":false}`
		resp, raw := matrixDo(t, mp, http.MethodPost, "/api/generate", body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("статус %d, want 200; тело: %s", resp.StatusCode, raw)
		}
		m := assertJSONObject(t, raw, "/api/generate")
		if _, ok := m["response"]; !ok {
			t.Errorf("нет response: %s", raw)
		}
		if done, _ := m["done"].(bool); !done {
			t.Errorf("done != true: %s", raw)
		}
	})

	for _, tc := range []struct{ path, field string }{
		{"/api/embed", "embeddings"},
		{"/api/embeddings", "embedding"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			body := `{"model":"` + matrixModel + `","input":"hi","prompt":"hi"}`
			resp, raw := matrixDo(t, mp, http.MethodPost, tc.path, body)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("статус %d, want 200; тело: %s", resp.StatusCode, raw)
			}
			m := assertJSONObject(t, raw, tc.path)
			if _, ok := m[tc.field]; !ok {
				t.Errorf("нет поля %q: %s", tc.field, raw)
			}
		})
	}
}

// TestR335_OpenAIMatrix — /v1: модели, chat (оба режима), completions, embeddings.
func TestR335_OpenAIMatrix(t *testing.T) {
	mp := newMatrixProxy(t)

	t.Run("/v1/models", func(t *testing.T) {
		resp, raw := matrixDo(t, mp, http.MethodGet, "/v1/models", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("статус %d, want 200; тело: %s", resp.StatusCode, raw)
		}
		m := assertJSONObject(t, raw, "/v1/models")
		data, ok := m["data"].([]interface{})
		if !ok || len(data) == 0 {
			t.Fatalf("data пуст: %s", raw)
		}
		first, _ := data[0].(map[string]interface{})
		for _, field := range []string{"id", "object", "owned_by"} {
			if _, ok := first[field]; !ok {
				t.Errorf("/v1/models[0]: нет %q: %v", field, first)
			}
		}
	})

	t.Run("/v1/chat/completions (не-стрим)", func(t *testing.T) {
		body := `{"model":"` + matrixModel + `","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`
		resp, raw := matrixDo(t, mp, http.MethodPost, "/v1/chat/completions", body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("статус %d, want 200; тело: %s", resp.StatusCode, raw)
		}
		m := assertJSONObject(t, raw, "/v1/chat/completions")
		choices, ok := m["choices"].([]interface{})
		if !ok || len(choices) == 0 {
			t.Fatalf("choices пуст: %s", raw)
		}
		ch, _ := choices[0].(map[string]interface{})
		if _, ok := ch["message"]; !ok {
			t.Errorf("нет message: %v", ch)
		}
		if _, ok := ch["finish_reason"]; !ok {
			t.Errorf("нет finish_reason: %v", ch)
		}
		if _, ok := m["usage"]; !ok {
			t.Errorf("нет usage: %s", raw)
		}
	})

	t.Run("/v1/chat/completions (SSE)", func(t *testing.T) {
		body := `{"model":"` + matrixModel + `","messages":[{"role":"user","content":"hi"}],"max_tokens":16,"stream":true}`
		resp, raw := matrixDo(t, mp, http.MethodPost, "/v1/chat/completions", body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("статус %d, want 200; тело: %s", resp.StatusCode, raw)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
			t.Errorf("Content-Type = %q, want text/event-stream", ct)
		}
		if !strings.Contains(raw, "data: [DONE]") {
			t.Errorf("нет терминатора data: [DONE]: %.300s", raw)
		}
		// После [DONE] не должно быть data-кадров.
		idx := strings.Index(raw, "data: [DONE]")
		if idx >= 0 {
			after := strings.TrimSpace(raw[idx+len("data: [DONE]"):])
			if strings.Contains(after, "data: {") {
				t.Errorf("кадры после [DONE]: %.200s", after)
			}
		}
	})

	t.Run("/v1/completions", func(t *testing.T) {
		body := `{"model":"` + matrixModel + `","prompt":"hi","max_tokens":16}`
		resp, raw := matrixDo(t, mp, http.MethodPost, "/v1/completions", body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("статус %d, want 200; тело: %s", resp.StatusCode, raw)
		}
		m := assertJSONObject(t, raw, "/v1/completions")
		choices, _ := m["choices"].([]interface{})
		if len(choices) == 0 {
			t.Fatalf("choices пуст: %s", raw)
		}
		ch, _ := choices[0].(map[string]interface{})
		if _, ok := ch["text"]; !ok {
			t.Errorf("нет text: %v", ch)
		}
	})

	t.Run("/v1/embeddings", func(t *testing.T) {
		body := `{"model":"` + matrixModel + `","input":"hi"}`
		resp, raw := matrixDo(t, mp, http.MethodPost, "/v1/embeddings", body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("статус %d, want 200; тело: %s", resp.StatusCode, raw)
		}
		m := assertJSONObject(t, raw, "/v1/embeddings")
		if _, ok := m["data"]; !ok {
			t.Errorf("нет data: %s", raw)
		}
	})
}

// TestR335_NegativeMatrix — форма ошибок на заглушке.
func TestR335_NegativeMatrix(t *testing.T) {
	mp := newMatrixProxy(t)

	cases := []struct {
		name     string
		method   string
		path     string
		body     string
		wantCode []int
	}{
		{"неизвестный путь Ollama", http.MethodGet, "/api/definitely-not-a-route", "", []int{404}},
		{"неизвестный путь OpenAI", http.MethodGet, "/v1/definitely-not-a-route", "", []int{404}},
		{"битый JSON", http.MethodPost, "/api/chat", `{"model": `, []int{400}},
		{"неизвестная модель", http.MethodPost, "/api/chat",
			`{"model":"nope","messages":[{"role":"user","content":"hi"}],"stream":false}`, []int{404}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, raw := matrixDo(t, mp, tc.method, tc.path, tc.body)
			ok := false
			for _, want := range tc.wantCode {
				if resp.StatusCode == want {
					ok = true
				}
			}
			if !ok {
				t.Errorf("статус %d, want %v; тело: %s", resp.StatusCode, tc.wantCode, raw)
			}
			m := assertJSONObject(t, raw, tc.name)
			if _, ok := m["error"]; !ok {
				t.Errorf("в теле нет поля error: %.200s", raw)
			}
		})
	}
}

// TestR335_RegistryMgmtMatrix — реестровые ручки Ollama (R336: их требует стюард
// инвентаря маршрутов).
//
// Контракт для клиента один: ответ — JSON-объект. Коды различаются по смыслу
// (pull/create/copy/delete — 200, push — 501 «не поддерживается»), но текстовая
// ошибка или HTML здесь недопустимы: Ollama CLI/OpenWebUI разбирают тело.
func TestR335_RegistryMgmtMatrix(t *testing.T) {
	mp := newMatrixProxy(t)
	body := `{"model":"` + matrixModel + `","name":"` + matrixModel + `","from":"` + matrixModel + `"}`

	cases := []struct {
		path     string
		wantCode []int
	}{
		{"/api/pull", []int{200}},
		{"/api/create", []int{200}},
		{"/api/copy", []int{200}},
		{"/api/delete", []int{200}},
		{"/api/push", []int{501}},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			resp, raw := matrixDo(t, mp, http.MethodPost, tc.path, body)
			ok := false
			for _, want := range tc.wantCode {
				if resp.StatusCode == want {
					ok = true
				}
			}
			if !ok {
				t.Errorf("статус %d, want %v; тело: %s", resp.StatusCode, tc.wantCode, raw)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "json") {
				t.Errorf("Content-Type = %q, want JSON (текст клиент не разберёт)", ct)
			}
			assertJSONObject(t, raw, tc.path)
		})
	}
}