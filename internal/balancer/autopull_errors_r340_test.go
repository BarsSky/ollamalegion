// autopull_errors_r340_test.go — R340 (2026-10-30).
//
// Ветка авто-pull отдавала три исхода текстом (http.Error): «pull не удался»,
// «нет свободного слота на новом бэкенде», «pull прошёл, но запрос упал». Весь
// остальной API отвечает JSON-объектом с полем error, и клиенты (Cline/OpenWebUI)
// разбирают именно JSON — на text/plain они показывают пустоту или «invalid json».
//
// Тесты фиксируют: JSON-форму по поверхности, осмысленный статус, сохранение
// причины в теле.
package balancer

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// decodeAutoPullR340 — разобрать тело ответа помощника.
func decodeAutoPullR340(t *testing.T, rec *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Errorf("Content-Type = %q, want JSON", ct)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("тело не JSON: %v (%q)", err, rec.Body.String())
	}
	return m
}

// TestR340_PullFailedIsJSONOllama — Ollama-поверхность: error строкой, 404.
func TestR340_PullFailedIsJSONOllama(t *testing.T) {
	rec := httptest.NewRecorder()
	writeAutoPullErrorR340(rec, "ollama", "gemma", http.StatusNotFound,
		"model not found on any backend and auto-pull failed", errors.New("registry timeout"))

	if rec.Code != http.StatusNotFound {
		t.Errorf("статус %d, want 404", rec.Code)
	}
	m := decodeAutoPullR340(t, rec)
	errText, ok := m["error"].(string)
	if !ok || errText == "" {
		t.Fatalf("error не строка (контракт Ollama): %v", m["error"])
	}
	for _, want := range []string{"auto-pull failed", "registry timeout"} {
		if !strings.Contains(errText, want) {
			t.Errorf("в error нет %q: %q", want, errText)
		}
	}
	if m["model"] != "gemma" {
		t.Errorf("model = %v, want gemma (клиент должен видеть, о какой модели речь)", m["model"])
	}
}

// TestR340_NoSlotIsJSONOpenAI — OpenAI-поверхность: error объектом, 503.
func TestR340_NoSlotIsJSONOpenAI(t *testing.T) {
	rec := httptest.NewRecorder()
	writeAutoPullErrorR340(rec, "openai", "gemma", http.StatusServiceUnavailable,
		`model pulled but no free slot on backend "b2"`, nil)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("статус %d, want 503 (повторимо)", rec.Code)
	}
	m := decodeAutoPullR340(t, rec)
	errObj, ok := m["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("error не объект (контракт OpenAI): %v", m["error"])
	}
	for _, field := range []string{"message", "type", "code"} {
		if _, ok := errObj[field]; !ok {
			t.Errorf("в error нет поля %q: %v", field, m["error"])
		}
	}
	if code, _ := errObj["code"].(float64); int(code) != http.StatusServiceUnavailable {
		t.Errorf("error.code = %v, want 503", errObj["code"])
	}
	if msg, _ := errObj["message"].(string); !strings.Contains(msg, "no free slot") {
		t.Errorf("причина потеряна: %q", msg)
	}
}

// TestR340_RetryFailedIsBadGateway — «pull прошёл, запрос упал» — 502, не 500.
func TestR340_RetryFailedIsBadGateway(t *testing.T) {
	rec := httptest.NewRecorder()
	writeAutoPullErrorR340(rec, "ollama", "gemma", http.StatusBadGateway,
		"model pulled but the retried request failed", errors.New("upstream 500"))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("статус %d, want 502 (ошибка воркера, а не клиента)", rec.Code)
	}
	m := decodeAutoPullR340(t, rec)
	if reason, _ := m["reason"].(string); !strings.Contains(reason, "retried request failed") {
		t.Errorf("reason = %q", reason)
	}
	if detail, _ := m["detail"].(string); !strings.Contains(detail, "upstream 500") {
		t.Errorf("detail потерял исходную ошибку: %q", detail)
	}
}

// TestR340_FormByPath — форма выбирается по пути запроса.
func TestR340_FormByPath(t *testing.T) {
	cases := map[string]string{
		"/v1/chat/completions": "openai",
		"/api/chat":            "ollama",
		"/api/generate":        "ollama",
		"":                     "ollama",
	}
	for path, want := range cases {
		if got := autoPullErrorFormR340(path); got != want {
			t.Errorf("autoPullErrorFormR340(%q) = %q, want %q", path, got, want)
		}
	}
}

// TestR340_NilWriterSafe — nil-writer не паникует (защита от misuse).
func TestR340_NilWriterSafe(t *testing.T) {
	writeAutoPullErrorR340(nil, "ollama", "m", http.StatusNotFound, "x", nil)
}

// TestR340_ProxyHasNoPlainTextAutoPullErrors — стюард: ветка авто-pull больше не
// отдаёт ошибки через http.Error (текстом).
//
// Проверка по исходнику, а не по поведению: воспроизвести все три исхода
// авто-pull в тесте можно только с полным менеджером pull и живым реестром — это
// было бы медленно и хрупко. Текстовый ответ виден по характерному шаблону,
// который мы убрали; если он вернётся, стюард упадёт.
func TestR340_ProxyHasNoPlainTextAutoPullErrors(t *testing.T) {
	raw, err := os.ReadFile("proxy.go")
	if err != nil {
		t.Fatalf("чтение proxy.go: %v", err)
	}
	src := string(raw)
	for _, bad := range []string{
		`http.Error(w, fmt.Sprintf("Model '%s' pulled but`,
		`http.Error(w, fmt.Sprintf("Model '%s' not found on any backend`,
	} {
		if strings.Contains(src, bad) {
			t.Errorf("ветка авто-pull снова отвечает текстом: %q — клиент не разберёт тело ошибки", bad)
		}
	}
}