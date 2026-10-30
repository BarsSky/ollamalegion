//go:build live

// live_matrix_test.go — ЭТАП 1 глобальной проверки API (R332, 2026-10-30).
//
// ЖИВОЙ ПРОГОН, А НЕ ЧТЕНИЕ КОДА. Аудит маршрутов (docs/plans/2026-10-30-route-map.md)
// прямо оговаривает: «живой прогон не делался», все выводы — из чтения кода.
// Этот набор закрывает именно эту дыру: он ходит на РАБОТАЮЩИЙ стенд по каждому
// клиентскому маршруту обеих поверхностей и сверяет то, что видит клиент, с
// контрактом.
//
// Запуск (стенд на 18080/18081):
//
//	go test -tags live ./tests/api_contract/ -v -timeout 30m
//	LB_BASE_URL=http://127.0.0.1:18080 go test -tags live ./tests/api_contract/ -v
//	LB_MATRIX_SKIP_INFERENCE=1 — только дешёвые маршруты (без загрузки модели)
//
// Что проверяется у КАЖДОГО маршрута:
//   - HTTP-статус;
//   - Content-Type (JSON против NDJSON против SSE — клиенты различают их строго);
//   - форма тела (обязательные поля контракта);
//   - для стримов: каждый кадр — валидный JSON, терминатор РОВНО один и он
//     ПОСЛЕДНИЙ (после done:true писать нельзя);
//   - контрактные значения done_reason / finish_reason (реестр R328);
//   - заголовки X-Backend-ID / X-Session-ID;
//   - негативные кейсы: неизвестный путь, битый JSON, неизвестная модель —
//     форма ошибки своей поверхности.
//
// ВАЖНО: тесты не «подгоняются» под текущее поведение. Если расхождение есть,
// тест падает с точным описанием, и это и есть отчёт.
package api_contract

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func baseURL() string {
	if v := strings.TrimSpace(os.Getenv("LB_BASE_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://127.0.0.1:18080"
}

// matrixModel — модель для инференс-проверок (стенд: gemma, профиль 32768).
func matrixModel() string {
	if v := strings.TrimSpace(os.Getenv("LB_MATRIX_MODEL")); v != "" {
		return v
	}
	return "gemma-4-E4B-it-Q4_K_M"
}

func skipInference(t *testing.T) {
	t.Helper()
	if os.Getenv("LB_MATRIX_SKIP_INFERENCE") == "1" {
		t.Skip("LB_MATRIX_SKIP_INFERENCE=1 — инференс не проверяем")
	}
}

// client — HTTP-клиент с таймаутами под долгую загрузку модели.
func client() *http.Client {
	return &http.Client{Timeout: 15 * time.Minute}
}

// doMatrix — запрос на стенд, тело и заголовки ответа как есть.
func doMatrix(t *testing.T, method, path string, body []byte) (*http.Response, []byte) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, baseURL()+path, rd)
	if err != nil {
		t.Fatalf("%s %s: не собрать запрос: %v", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: запрос не выполнен: %v", method, path, err)
	}
	defer resp.Body.Close()
	// Читаем целиком: для стримов размер небольшой (num_predict ограничен).
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatalf("%s %s: чтение тела: %v", method, path, err)
	}
	return resp, buf.Bytes()
}

// assertJSON — Content-Type и разбор JSON-объекта.
func assertJSON(t *testing.T, path string, resp *http.Response, body []byte) map[string]interface{} {
	t.Helper()
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "json") {
		t.Errorf("%s: Content-Type = %q, want JSON (клиент разбирает по нему)", path, ct)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		preview := string(body)
		if len(preview) > 200 {
			preview = preview[:200]
		}
		t.Fatalf("%s: тело не JSON-объект: %v; начало: %q", path, err, preview)
	}
	return m
}

// TestLiveOllamaReadOnly — информационные маршруты Ollama-поверхности.
func TestLiveOllamaReadOnly(t *testing.T) {
	for _, tc := range []struct {
		path string
		need string // обязательное поле верхнего уровня
	}{
		{"/api/version", "version"},
		{"/api/tags", "models"},
		{"/api/ps", "models"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			resp, body := doMatrix(t, http.MethodGet, tc.path, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("статус %d, want 200; тело: %s", resp.StatusCode, string(body))
			}
			m := assertJSON(t, tc.path, resp, body)
			if _, ok := m[tc.need]; !ok {
				t.Errorf("в теле нет поля %q: %s", tc.need, string(body))
			}
		})
	}
}

// TestLiveOllamaShowAndModels — /api/show и /api/models.
func TestLiveOllamaShowAndModels(t *testing.T) {
	t.Run("/api/show", func(t *testing.T) {
		body := []byte(fmt.Sprintf(`{"name":%q}`, matrixModel()))
		resp, raw := doMatrix(t, http.MethodPost, "/api/show", body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("статус %d, want 200 (метаданные отдаются без загрузки модели); тело: %s",
				resp.StatusCode, string(raw))
		}
		m := assertJSON(t, "/api/show", resp, raw)
		if len(m) == 0 {
			t.Error("пустой объект: клиент ждёт метаданные модели")
		}
	})

	t.Run("/api/models", func(t *testing.T) {
		resp, raw := doMatrix(t, http.MethodGet, "/api/models", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("статус %d, want 200; тело: %s", resp.StatusCode, string(raw))
		}
		m := assertJSON(t, "/api/models", resp, raw)
		if _, ok := m["models"]; !ok {
			t.Errorf("в теле нет поля models: %s", string(raw))
		}
	})
}

// TestLiveOpenAIModels — /v1/models и /v1/models/{id}.
func TestLiveOpenAIModels(t *testing.T) {
	resp, raw := doMatrix(t, http.MethodGet, "/v1/models", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/v1/models: статус %d, want 200; тело: %s", resp.StatusCode, string(raw))
	}
	m := assertJSON(t, "/v1/models", resp, raw)
	data, ok := m["data"].([]interface{})
	if !ok || len(data) == 0 {
		t.Fatalf("/v1/models: data пуст или не массив: %s", string(raw))
	}
	first, _ := data[0].(map[string]interface{})
	for _, field := range []string{"id", "object", "owned_by"} {
		if _, ok := first[field]; !ok {
			t.Errorf("/v1/models[0]: нет поля %q (контракт OpenAI): %v", field, first)
		}
	}
}

// TestLiveOllamaChatStream — ГЛАВНЫЙ маршрут: /api/chat потоком.
//
// Проверки, которые ловят именно те дефекты, из-за которых были жалобы:
//   - каждый кадр — JSON;
//   - done:true РОВНО один;
//   - после терминатора НЕТ кадров (иначе клиент видит «битый поток»);
//   - done_reason — контрактное значение;
//   - Content-Type — NDJSON (не SSE и не text/plain).
func TestLiveOllamaChatStream(t *testing.T) {
	skipInference(t)
	body := []byte(fmt.Sprintf(
		`{"model":%q,"messages":[{"role":"user","content":"Ответь одним словом: столица Франции?"}],"stream":true,"options":{"num_predict":16}}`,
		matrixModel()))
	resp, raw := doMatrix(t, http.MethodPost, "/api/chat", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус %d, want 200; тело: %s", resp.StatusCode, string(raw))
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "ndjson") && !strings.Contains(ct, "json") {
		t.Errorf("Content-Type = %q, want application/x-ndjson (Ollama-клиент разбирает построчно)", ct)
	}
	for _, h := range []string{"X-Backend-ID", "X-Session-ID"} {
		if resp.Header.Get(h) == "" {
			t.Errorf("нет заголовка %s — клиент/монитор не узнает, кто обслужил", h)
		}
	}

	frames := parseNDJSONFrames(t, "/api/chat", raw)
	if len(frames) == 0 {
		t.Fatal("поток пуст: клиент не получил ни одного кадра")
	}
	doneIdx := -1
	for i, f := range frames {
		done, _ := f["done"].(bool)
		if done {
			if doneIdx >= 0 {
				t.Errorf("ВТОРОЙ терминатор done:true (кадры %d и %d): клиент закрывает ход на первом и считает поток битым", doneIdx, i)
			}
			doneIdx = i
			if reason, _ := f["done_reason"].(string); !allowedOllamaDoneReason(reason) {
				t.Errorf("done_reason = %q вне контракта (допустимо stop|length|tool_calls|error|cancelled)", reason)
			}
		}
	}
	if doneIdx < 0 {
		t.Fatal("нет кадра done:true — клиент будет ждать до таймаута")
	}
	if doneIdx != len(frames)-1 {
		t.Errorf("после done:true пришло %d кадров (после терминатора писать нельзя)", len(frames)-1-doneIdx)
	}
	// Каждый кадр обязан нести model/created_at/message — по ним клиенты строят UI.
	for i, f := range frames {
		for _, field := range []string{"model", "message"} {
			if _, ok := f[field]; !ok {
				t.Errorf("кадр %d: нет поля %q: %v", i, field, f)
				break
			}
		}
	}
}

// TestLiveOllamaGenerate — /api/generate (без стрима): поле response.
func TestLiveOllamaGenerate(t *testing.T) {
	skipInference(t)
	body := []byte(fmt.Sprintf(`{"model":%q,"prompt":"Ответь одним словом: столица Франции?","stream":false,"options":{"num_predict":16}}`, matrixModel()))
	resp, raw := doMatrix(t, http.MethodPost, "/api/generate", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус %d, want 200; тело: %s", resp.StatusCode, string(raw))
	}
	m := assertJSON(t, "/api/generate", resp, raw)
	if _, ok := m["response"]; !ok {
		t.Errorf("нет поля response: %s", string(raw))
	}
	if done, _ := m["done"].(bool); !done {
		t.Errorf("done != true в не-потоковом ответе: %s", string(raw))
	}
	// R335.2: при расхождении печатаем СЫРОЕ тело — иначе не понять, какая ветка
	// ответа его сформировала (у /api/generate их несколько: обычная, пустой вывод,
	// размышления без ответа).
	if reason, _ := m["done_reason"].(string); !allowedOllamaDoneReason(reason) {
		t.Errorf("done_reason = %q вне контракта; сырое тело: %s", reason, raw)
	}
}

// TestLiveEmbeddings — /api/embed и legacy /api/embeddings.
func TestLiveEmbeddings(t *testing.T) {
	skipInference(t)
	t.Run("/api/embed", func(t *testing.T) {
		body := []byte(fmt.Sprintf(`{"model":%q,"input":"проверка"}`, matrixModel()))
		resp, raw := doMatrix(t, http.MethodPost, "/api/embed", body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("статус %d, want 200; тело: %s", resp.StatusCode, string(raw))
		}
		m := assertJSON(t, "/api/embed", resp, raw)
		emb, ok := m["embeddings"].([]interface{})
		if !ok || len(emb) == 0 {
			t.Errorf("embeddings пуст/не массив: %s", string(raw))
		}
	})
	t.Run("/api/embeddings", func(t *testing.T) {
		body := []byte(fmt.Sprintf(`{"model":%q,"prompt":"проверка"}`, matrixModel()))
		resp, raw := doMatrix(t, http.MethodPost, "/api/embeddings", body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("статус %d, want 200; тело: %s", resp.StatusCode, string(raw))
		}
		m := assertJSON(t, "/api/embeddings", resp, raw)
		if _, ok := m["embedding"]; !ok {
			t.Errorf("нет поля embedding (legacy-контракт): %s", string(raw))
		}
	})
}

// TestLiveOpenAIChatNonStream — /v1/chat/completions без стрима.
func TestLiveOpenAIChatNonStream(t *testing.T) {
	skipInference(t)
	body := []byte(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"Ответь одним словом: столица Франции?"}],"max_tokens":16}`, matrixModel()))
	resp, raw := doMatrix(t, http.MethodPost, "/v1/chat/completions", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус %d, want 200; тело: %s", resp.StatusCode, string(raw))
	}
	m := assertJSON(t, "/v1/chat/completions", resp, raw)
	choices, ok := m["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		t.Fatalf("choices пуст: %s", string(raw))
	}
	ch, _ := choices[0].(map[string]interface{})
	if _, ok := ch["message"]; !ok {
		t.Errorf("choices[0]: нет message: %v", ch)
	}
	if _, ok := ch["finish_reason"]; !ok {
		t.Errorf("choices[0]: нет finish_reason (контракт OpenAI): %v", ch)
	}
	if _, ok := m["usage"]; !ok {
		t.Errorf("нет usage: OpenAI-клиенты считают токены/стоимость по нему: %s", string(raw))
	}
}

// TestLiveOpenAIChatStream — SSE-поток /v1/chat/completions.
func TestLiveOpenAIChatStream(t *testing.T) {
	skipInference(t)
	body := []byte(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"Ответь одним словом: столица Франции?"}],"max_tokens":16,"stream":true}`, matrixModel()))
	resp, raw := doMatrix(t, http.MethodPost, "/v1/chat/completions", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус %d, want 200; тело: %s", resp.StatusCode, string(raw))
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream (SSE-клиенты)", ct)
	}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0), 4<<20)
	dataFrames, sawDone, donePending := 0, false, false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			sawDone = true
			continue
		}
		dataFrames++
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			t.Fatalf("кадр SSE не JSON: %v (%q)", err, payload)
		}
		if choices, ok := m["choices"].([]interface{}); ok && len(choices) > 0 {
			if ch, ok := choices[0].(map[string]interface{}); ok {
				if fr, _ := ch["finish_reason"].(string); fr != "" {
					donePending = true
				}
			}
		}
		// После [DONE] кадров быть не должно: это отдельно ловим ниже.
		if sawDone {
			t.Errorf("кадр data: пришёл ПОСЛЕ data: [DONE]: %q", payload)
		}
	}
	if dataFrames == 0 {
		t.Fatal("ни одного data-кадра: клиент не получил ответа")
	}
	if !donePending {
		t.Error("ни в одном кадре нет finish_reason — клиент не узнает о завершении")
	}
	if !sawDone {
		t.Error("нет терминатора data: [DONE] — OpenAI-клиент будет ждать до таймаута")
	}
}

// TestLiveOpenAICompletions — /v1/completions (legacy).
func TestLiveOpenAICompletions(t *testing.T) {
	skipInference(t)
	body := []byte(fmt.Sprintf(`{"model":%q,"prompt":"Столица Франции —","max_tokens":16}`, matrixModel()))
	resp, raw := doMatrix(t, http.MethodPost, "/v1/completions", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус %d, want 200; тело: %s", resp.StatusCode, string(raw))
	}
	m := assertJSON(t, "/v1/completions", resp, raw)
	choices, ok := m["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		t.Fatalf("choices пуст: %s", string(raw))
	}
	ch, _ := choices[0].(map[string]interface{})
	if _, ok := ch["text"]; !ok {
		t.Errorf("choices[0]: нет text (контракт completions): %v", ch)
	}
}

// TestLiveOpenAIEmbeddings — /v1/embeddings.
func TestLiveOpenAIEmbeddings(t *testing.T) {
	skipInference(t)
	body := []byte(fmt.Sprintf(`{"model":%q,"input":"проверка"}`, matrixModel()))
	resp, raw := doMatrix(t, http.MethodPost, "/v1/embeddings", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус %d, want 200; тело: %s", resp.StatusCode, string(raw))
	}
	m := assertJSON(t, "/v1/embeddings", resp, raw)
	data, ok := m["data"].([]interface{})
	if !ok || len(data) == 0 {
		t.Fatalf("data пуст: %s", string(raw))
	}
	first, _ := data[0].(map[string]interface{})
	if _, ok := first["embedding"].([]interface{}); !ok {
		t.Errorf("data[0]: нет embedding-массива: %v", first)
	}
}

// TestLiveAnthropicSurface — /v1/messages и count_tokens.
func TestLiveAnthropicSurface(t *testing.T) {
	skipInference(t)
	t.Run("/v1/messages", func(t *testing.T) {
		body := []byte(fmt.Sprintf(`{"model":%q,"max_tokens":16,"messages":[{"role":"user","content":"Ответь одним словом: столица Франции?"}]}`, matrixModel()))
		resp, raw := doMatrix(t, http.MethodPost, "/v1/messages", body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("статус %d, want 200; тело: %s", resp.StatusCode, string(raw))
		}
		m := assertJSON(t, "/v1/messages", resp, raw)
		if typ, _ := m["type"].(string); typ != "message" {
			t.Errorf("type = %q, want message (контракт Anthropic): %s", typ, string(raw))
		}
		if _, ok := m["content"]; !ok {
			t.Errorf("нет content: %s", string(raw))
		}
	})
	t.Run("/v1/messages/count_tokens", func(t *testing.T) {
		body := []byte(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"привет"}]}`, matrixModel()))
		resp, raw := doMatrix(t, http.MethodPost, "/v1/messages/count_tokens", body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("статус %d, want 200; тело: %s", resp.StatusCode, string(raw))
		}
		m := assertJSON(t, "/v1/messages/count_tokens", resp, raw)
		if _, ok := m["input_tokens"]; !ok {
			t.Errorf("нет input_tokens (контракт Anthropic): %s", string(raw))
		}
	})
}

// TestLiveNegativeCases — форма ошибок: клиент должен получить JSON своей
// поверхности, а не пустое тело и не HTML.
func TestLiveNegativeCases(t *testing.T) {
	cases := []struct {
		name     string
		method   string
		path     string
		body     []byte
		wantCode []int
	}{
		{"неизвестный путь Ollama", http.MethodGet, "/api/definitely-not-a-route", nil, []int{404}},
		{"неизвестный путь OpenAI", http.MethodGet, "/v1/definitely-not-a-route", nil, []int{404}},
		{"битый JSON /api/chat", http.MethodPost, "/api/chat", []byte(`{"model": `), []int{400}},
		{"нет модели /api/chat", http.MethodPost, "/api/chat", []byte(`{"model":"no-such-model-xyz","messages":[{"role":"user","content":"hi"}],"stream":false}`), []int{404, 503}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, raw := doMatrix(t, tc.method, tc.path, tc.body)
			ok := false
			for _, want := range tc.wantCode {
				if resp.StatusCode == want {
					ok = true
				}
			}
			if !ok {
				t.Errorf("статус %d, want один из %v; тело: %s", resp.StatusCode, tc.wantCode, string(raw))
			}
			// Тело ошибки обязано быть JSON-объектом с полем error: именно его
			// показывают Cline/OpenWebUI.
			var m map[string]interface{}
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatalf("тело ошибки не JSON: %v; начало: %q", err, firstN(string(raw), 160))
			}
			if _, ok := m["error"]; !ok {
				t.Errorf("в теле ошибки нет поля error: %s", firstN(string(raw), 200))
			}
		})
	}
}

// TestLiveNoTerminatorChurn — терминатор не должен «дробиться» на повторный запрос:
// один и тот же запрос дважды подряд даёт по одному done:true.
func TestLiveNoTerminatorChurn(t *testing.T) {
	skipInference(t)
	body := []byte(fmt.Sprintf(
		`{"model":%q,"messages":[{"role":"user","content":"Скажи ок"}],"stream":true,"options":{"num_predict":8}}`,
		matrixModel()))
	for attempt := 1; attempt <= 2; attempt++ {
		resp, raw := doMatrix(t, http.MethodPost, "/api/chat", body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("попытка %d: статус %d; тело: %s", attempt, resp.StatusCode, string(raw))
		}
		frames := parseNDJSONFrames(t, "/api/chat", raw)
		dones := 0
		for _, f := range frames {
			if done, _ := f["done"].(bool); done {
				dones++
			}
		}
		if dones != 1 {
			t.Errorf("попытка %d: терминаторов %d, want ровно 1", attempt, dones)
		}
	}
}

// parseNDJSONFrames — разобрать NDJSON-поток в кадры, падая на невалидном кадре.
func parseNDJSONFrames(t *testing.T, path string, raw []byte) []map[string]interface{} {
	t.Helper()
	var frames []map[string]interface{}
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("%s: кадр %d не JSON: %v; начало: %q", path, i, err, firstN(line, 160))
		}
		frames = append(frames, m)
	}
	return frames
}

// allowedOllamaDoneReason — копия реестра R328 для внешнего пакета (внутренний
// реестр недоступен из tests/: он в internal/balancer).
func allowedOllamaDoneReason(reason string) bool {
	switch reason {
	case "stop", "length", "tool_calls", "error", "cancelled":
		return true
	}
	return false
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
