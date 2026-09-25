//go:build llama_stub

// model_not_found_r83_test.go — R83 (2026-09-25).
//
// Живой кейс из лога cppworker:
//
//	gguf_init_from_file: failed to open GGUF file models/qwen3.8:latest.gguf
//	                  (No such file or directory)
//	POST /api/models/load status=500
//
// Клиент просил "qwen3.8:latest", на диске лежал "Qwen3.8-27B-UD-Q4_K_M.gguf".
// Теперь на это отвечает 404 с причиной, списком доступных моделей и подсказкой
// (а не 500 с сырым путём llama.cpp). 404 выбран осознанно: балансер ретраит
// только 503 «model is loading» (internal/balancer/model_management.go:1223),
// поэтому 404 доходит до клиента как причина, а не превращается в retry-шторм.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestR83_ModelNotFoundSuggestion_LiveCase — подсказка обязана узнавать модель по
// нормализованному имени: именно этого не хватало оператору в живом кейсе.
func TestR83_ModelNotFoundSuggestion_LiveCase(t *testing.T) {
	available := []string{"Qwen3.8-27B-UD-Q4_K_M", "gemma-4-E4B-it-Q4_K_M", "Llama-3.1-8B"}

	s := modelNotFoundSuggestion("qwen3.8:latest", available)
	if !strings.Contains(s, "Qwen3.8-27B-UD-Q4_K_M") {
		t.Errorf("подсказка не назвала реальную модель: %q", s)
	}
	if !strings.Contains(s, "qwen3.8:latest") {
		t.Errorf("подсказка не называет то, что просил клиент: %q", s)
	}

	// Имя с расширением и в другом регистре — тот же результат.
	for _, req := range []string{"qwen3.8.gguf", "QWEN3.8:LATEST", "Qwen3.8"} {
		if s := modelNotFoundSuggestion(req, available); !strings.Contains(s, "Qwen3.8-27B-UD-Q4_K_M") {
			t.Errorf("для %q подсказка не нашла модель: %q", req, s)
		}
	}
}

// TestR83_ModelNotFoundSuggestion_NoMatch — когда похожего нет, список доступных
// всё равно должен быть (оператору нужно, что вписать в клиент).
func TestR83_ModelNotFoundSuggestion_NoMatch(t *testing.T) {
	available := []string{"Qwen3.8-27B-UD-Q4_K_M", "gemma-4-E4B-it-Q4_K_M"}

	s := modelNotFoundSuggestion("totally-unrelated-model:latest", available)
	for _, name := range available {
		if !strings.Contains(s, name) {
			t.Errorf("в подсказке нет %q: %q", name, s)
		}
	}
}

func TestR83_ModelNotFoundSuggestion_EmptyCatalog(t *testing.T) {
	s := modelNotFoundSuggestion("anything:latest", nil)
	if !strings.Contains(s, ".gguf") {
		t.Errorf("при пустом каталоге подсказка должна объяснить, что моделей нет: %q", s)
	}
}

// TestR83_WriteModelNotFoundResponse — контракт ответа.
func TestR83_WriteModelNotFoundResponse(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackend()

	rec := httptest.NewRecorder()
	writeModelNotFoundResponse(rec, "qwen3.8:latest", "/app/models/qwen3.8:latest.gguf")

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if rec.Code == http.StatusServiceUnavailable {
		t.Error("не 503: балансер будет ретраить и держать load 3-15 минут")
	}

	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("ответ не JSON: %v (body=%q)", err, rec.Body.String())
	}
	if s, _ := body["code"].(string); s != "model_not_found" {
		t.Errorf("code = %q, want model_not_found", s)
	}
	if s, _ := body["error"].(string); s == "" {
		t.Error("нет поля error — клиент не покажет причину")
	}
	if s, _ := body["model"].(string); s != "qwen3.8:latest" {
		t.Errorf("model = %q, want то, что просил клиент", s)
	}
	if _, ok := body["available_models"].([]interface{}); !ok {
		t.Errorf("нет массива available_models (body=%v)", body)
	}
	if s, _ := body["suggestion"].(string); s == "" {
		t.Error("нет suggestion — оператор не знает, что делать")
	}
}

func TestR83_CommonPrefixLen(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"qwen3.8-27b", "qwen3.8-27b-ud-q4_k_m", 11}, // "qwen3.8-27b" — 11 байт
		{"gemma-4", "llama-3", 0},                    // 'g' != 'l'
		{"abc", "abc", 3},
		{"", "abc", 0},
		{"abc", "", 0},
	}
	for _, tc := range cases {
		if got := commonPrefixLen(tc.a, tc.b); got != tc.want {
			t.Errorf("commonPrefixLen(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
