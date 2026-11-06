//go:build llama_stub

// exact_prompt_tokens_r473_test.go — R473 (2026-11-06): оценка промпта заменяется
// ФАКТОМ от токенизатора модели, когда он доступен.
//
// ЖИВОЕ ЗАВЫШЕНИЕ эвристики на этом стенде: 49731 при реальных 9816; 36601 при
// ~20.7k (Cline считает по нашему же usage). Из завышенного числа рождались
// отказы 413 (до R466), отменённый рост (до R472) и перезагрузки на окно больше
// нужного (40k там, где хватало ~33k).
package balancer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestR473_ExactReplacesEstimate — успешный ответ воркера заменяет оценку точным
// числом плюс запас на преамбулу шаблона.
func TestR473_ExactReplacesEstimate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tokenize" {
			t.Errorf("неожиданный путь %q", r.URL.Path)
		}
		var req map[string]string
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("тело не JSON: %v", err)
		}
		if req["model"] != "m" || req["text"] == "" {
			t.Errorf("в теле нет модели/текста: %v", req)
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"tokens": 1234, "source": "tokenizer", "model_loaded": true,
		})
	}))
	defer srv.Close()

	p := &Proxy{}
	p.ReplaceModelProfiles(nil)
	n, ok := p.callTokenizeR473(srv.URL, "m", "текст промпта")
	if !ok {
		t.Fatal("точный подсчёт не принят")
	}
	if n != 1234 {
		t.Fatalf("tokens = %d, want 1234", n)
	}
}

// TestR473_EstimateSourceIsNotTrusted — воркер ответил source=estimate: это не
// факт, брать его вместо оценки нельзя (иначе мы просто меняем одну эвристику на
// другую).
func TestR473_EstimateSourceIsNotTrusted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"tokens": 999, "source": "estimate", "model_loaded": false,
		})
	}))
	defer srv.Close()

	p := &Proxy{}
	if _, ok := p.callTokenizeR473(srv.URL, "m", "x"); ok {
		t.Fatal("source=estimate принят как факт — это ложь клиенту и решениям")
	}
}

// TestR473_HTTPFailuresFallBack — 404/500/мусор: остаёмся при эвристике, без
// паники и без «нуля вместо окна».
func TestR473_HTTPFailuresFallBack(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"старый воркер без ручки", http.StatusNotFound, `{"error":"404 page not found"}`},
		{"внутренняя ошибка", http.StatusInternalServerError, `boom`},
		{"мусор в теле", http.StatusOK, `not json`},
		{"токенизатор вернул 0", http.StatusOK, `{"tokens":0,"source":"tokenizer"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			p := &Proxy{}
			if _, ok := p.callTokenizeR473(srv.URL, "m", "x"); ok {
				t.Fatalf("%s: отказ воркера принят как точный подсчёт", tc.name)
			}
		})
	}
}

// TestR473_SwitchOff — рубильник выключает точный подсчёт (прежнее поведение).
func TestR473_SwitchOff(t *testing.T) {
	t.Setenv(exactTokensSwitchEnvR473, "0")
	if exactTokensEnabledR473() {
		t.Error("LB_PREFLIGHT_EXACT_TOKENS=0 обязан выключать точный подсчёт")
	}
	t.Setenv(exactTokensSwitchEnvR473, "")
	if !exactTokensEnabledR473() {
		t.Error("по умолчанию точный подсчёт включён")
	}
	t.Setenv(exactTokensSwitchEnvR473, "абракадабра")
	if !exactTokensEnabledR473() {
		t.Error("неизвестное значение = включено (и WARN в лог)")
	}
}

// TestR473_SampleIncludesTools — образец включает tools[]: у агентских клиентов
// схемы инструментов занимают больше места, чем сами сообщения.
func TestR473_SampleIncludesTools(t *testing.T) {
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "editor"}}}
	sample := exactSampleWithToolsR473("привет", tools)
	if !strings.Contains(sample, "привет") || !strings.Contains(sample, `"editor"`) {
		t.Fatalf("в образце нет текста или tools: %s", sample)
	}
	if got := exactSampleWithToolsR473("привет", nil); got != "привет" {
		t.Fatalf("без tools образец = %q, want исходный текст", got)
	}
	if got := exactSampleWithToolsR473("", []map[string]any{}); got != "" {
		t.Fatalf("пустые tools не должны добавлять мусор: %q", got)
	}
}

// TestR473_SmallSampleSkipsCall — мелкий промпт не ходит в сеть: там завышение не
// влияет на решения, а запрос на горячем пути не нужен.
func TestR473_SmallSampleSkipsCall(t *testing.T) {
	p := &Proxy{}
	meta := &RequestMeta{ModelName: "m", ExactSampleText: "короткий промпт"}
	if _, ok := p.exactPromptTokensR473(meta); ok {
		t.Fatal("мелкий образец не должен вызывать токенизатор")
	}
}
