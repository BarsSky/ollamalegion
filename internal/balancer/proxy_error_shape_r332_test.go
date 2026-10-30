// proxy_error_shape_r332_test.go — R332 (2026-10-30).
//
// НАЙДЕНО ЖИВОЙ МАТРИЦЕЙ API (tests/api_contract): запрос на неизвестный путь
// уходил в generic-прокси, воркер отвечал своим `http.NotFound` — текстом
// "404 page not found" с Content-Type: text/plain. Балансер пересылал это как
// есть, и клиент (Cline/OpenWebUI) не мог показать ошибку: он разбирает тело как
// JSON.
//
// Тесты фиксируют нормализацию: форма ошибки определяется ПОВЕРХНОСТЬЮ запроса,
// статус сохраняется, JSON-ошибки воркера не переписываются.
package balancer

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// upstreamErrRespR332 — ответ воркера с заданным статусом и телом.
func upstreamErrRespR332(status int, contentType, body string) *http.Response {
	h := make(http.Header)
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	return &http.Response{
		StatusCode: status,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// readBodyR332 — прочитать и разобрать итоговое тело.
func readBodyR332(t *testing.T, resp *http.Response) (map[string]interface{}, string) {
	t.Helper()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("чтение тела: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("тело не JSON: %v (%q)", err, string(raw))
	}
	return m, string(raw)
}

// TestR332_PlainTextErrorBecomesJSONOllama — Ollama-поверхность: error — строка.
func TestR332_PlainTextErrorBecomesJSONOllama(t *testing.T) {
	resp := upstreamErrRespR332(http.StatusNotFound, "text/plain; charset=utf-8", "404 page not found\n")
	out := normalizeUpstreamErrorBodyR332("/api/definitely-not-a-route", resp)
	if out.StatusCode != http.StatusNotFound {
		t.Errorf("статус изменился на %d, want 404 (клиент должен видеть тот же код)", out.StatusCode)
	}
	if ct := out.Header.Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Errorf("Content-Type = %q, want JSON", ct)
	}
	m, raw := readBodyR332(t, out)
	errField, ok := m["error"].(string)
	if !ok || errField == "" {
		t.Fatalf("error не строка: %s", raw)
	}
	if !strings.Contains(errField, "404") {
		t.Errorf("в error нет исходного текста воркера: %q", errField)
	}
	if m["path"] != "/api/definitely-not-a-route" {
		t.Errorf("path = %v, want исходный путь (диагностика оператора)", m["path"])
	}
}

// TestR332_PlainTextErrorBecomesJSONOpenAI — OpenAI-поверхность: error — объект.
func TestR332_PlainTextErrorBecomesJSONOpenAI(t *testing.T) {
	resp := upstreamErrRespR332(http.StatusNotFound, "text/plain", "404 page not found\n")
	out := normalizeUpstreamErrorBodyR332("/v1/definitely-not-a-route", resp)

	m, raw := readBodyR332(t, out)
	errObj, ok := m["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("error не объект (контракт OpenAI): %s", raw)
	}
	for _, field := range []string{"message", "type", "code"} {
		if _, ok := errObj[field]; !ok {
			t.Errorf("в error нет поля %q: %s", field, raw)
		}
	}
	if code, _ := errObj["code"].(float64); int(code) != http.StatusNotFound {
		t.Errorf("code = %v, want 404", errObj["code"])
	}
}

// TestR332_JSONErrorUntouched — JSON-ошибка воркера проходит как есть.
func TestR332_JSONErrorUntouched(t *testing.T) {
	body := `{"error":"model not found","code":404}`
	resp := upstreamErrRespR332(http.StatusNotFound, "application/json", body)
	out := normalizeUpstreamErrorBodyR332("/api/show", resp)
	if out.Body == nil {
		t.Fatal("тело потеряно")
	}
	raw, _ := io.ReadAll(out.Body)
	if string(raw) != body {
		t.Errorf("JSON-ошибка переписана:\n got %q\nwant %q", string(raw), body)
	}
	if ct := out.Header.Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Errorf("Content-Type = %q", ct)
	}
}

// TestR332_SuccessAndStreamsUntouched — успешные ответы и стримы не трогаем.
func TestR332_SuccessAndStreamsUntouched(t *testing.T) {
	cases := []*http.Response{
		upstreamErrRespR332(http.StatusOK, "application/x-ndjson", `{"done":false}`+"\n"),
		upstreamErrRespR332(http.StatusOK, "text/event-stream", "data: {}\n\n"),
		upstreamErrRespR332(http.StatusOK, "text/plain", "ok"),
	}
	for _, resp := range cases {
		out := normalizeUpstreamErrorBodyR332("/api/chat", resp)
		raw, _ := io.ReadAll(out.Body)
		if string(raw) != "ok" && !strings.Contains(string(raw), "done") && !strings.Contains(string(raw), "data:") {
			t.Errorf("успешный ответ изменён: %q", string(raw))
		}
	}
}

// TestR332_FiveHundredsAlsoNormalized — 5xx воркера тоже приводим к JSON.
func TestR332_FiveHundredsAlsoNormalized(t *testing.T) {
	resp := upstreamErrRespR332(http.StatusInternalServerError, "text/plain", "panic: boom")
	out := normalizeUpstreamErrorBodyR332("/api/chat", resp)
	if out.StatusCode != http.StatusInternalServerError {
		t.Errorf("статус изменился на %d, want 500", out.StatusCode)
	}
	m, _ := readBodyR332(t, out)
	if msg, _ := m["error"].(string); !strings.Contains(msg, "boom") {
		t.Errorf("исходный текст ошибки потерян: %v", m["error"])
	}
}

// TestR332_NilSafe — nil-ответ не паникует.
func TestR332_NilSafe(t *testing.T) {
	if out := normalizeUpstreamErrorBodyR332("/api/chat", nil); out != nil {
		t.Error("nil должен остаться nil")
	}
}
