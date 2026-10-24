//go:build llama_stub

// auto_continue_client_cap_r174_test.go — R174 (2026-10-18).
//
// НАЙДЕНО СЦЕНАРИЯМИ КЛИЕНТОВ (scripts/webui-e2e/client-matrix.js): на
// стриминговом Ollama-пути воркер видел промпт на 254 символа / 45 токенов
// больше прямого вызова, хотя клиент не присылал ни tools, ни системного
// промпта. Причина — не инъекция инструмента, а auto-continue (R60.21): клиент
// просил num_predict=6, ответ обрывался на лимите, балансер считал это
// «усечением» и слал второй запрос с уже полученным текстом.
//
// Контракт запроса: лимит клиента — это лимит. Агентные клиенты (Cline,
// deepseek harness, hermes) ставят max_tokens на КАЖДЫЙ запрос и полагаются на
// finish_reason=length.
package balancer

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestR174_ClientOutputCapped_Matrix — какие поля считаются лимитом клиента.
func TestR174_ClientOutputCapped_Matrix(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"ollama num_predict", `{"options":{"num_predict":6}}`, true},
		{"ollama num_predict=-1 = без лимита", `{"options":{"num_predict":-1}}`, false},
		{"ollama num_predict=0 = дефолт", `{"options":{"num_predict":0}}`, false},
		{"openai max_tokens", `{"max_tokens":256}`, true},
		{"openai max_completion_tokens", `{"max_completion_tokens":512}`, true},
		{"ничего не задано", `{"messages":[]}`, false},
		{"пустое тело", ``, false},
		{"не JSON", `not json`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, field := ClientOutputCapped([]byte(c.body))
			if got != c.want {
				t.Fatalf("ClientOutputCapped(%q) = %v (%s), want %v", c.body, got, field, c.want)
			}
			if got && field == "" {
				t.Error("лимит найден, но поле не названо — в логе будет непонятно, что именно ограничило")
			}
		})
	}
}

// runStreamProxyR174 — прогнать стриминговый запрос через транспорт балансера и
// вернуть число запросов, которые получил upstream.
func runStreamProxyR174(t *testing.T, extra map[string]interface{}) int32 {
	t.Helper()
	t.Setenv("LB_AUTO_CONTINUE_ON_TRUNCATION", "1")
	t.Setenv("LB_AUTO_CONTINUE_CHAT_POLICY", "")

	upstream, inferReqs := makeRegeneratingUpstreamR66d(t)
	p, _, cleanup := makeTestLlamaProxy(t, upstream.URL)
	defer cleanup()

	bodyObj := map[string]interface{}{
		"model":    "Qwen3.8-27B",
		"messages": []interface{}{map[string]string{"role": "user", "content": "покажи пример кода"}},
		"stream":   true,
	}
	for k, v := range extra {
		bodyObj[k] = v
	}
	bodyBytes, err := json.Marshal(bodyObj)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	if err := p.proxyRequestLlamaCpp(rec, req, "llama_test", bodyBytes); err != nil {
		t.Fatalf("proxyRequestLlamaCpp вернул ошибку: %v", err)
	}
	return atomic.LoadInt32(inferReqs)
}

// TestR174_CappedStream_NoSecondRequest — главный контрфакт: клиент поставил
// лимит → второго (продолжающего) запроса быть НЕ ДОЛЖНО.
func TestR174_CappedStream_NoSecondRequest(t *testing.T) {
	n := runStreamProxyR174(t, map[string]interface{}{
		"options": map[string]interface{}{"num_predict": 6},
	})
	if n != 1 {
		t.Errorf("upstream получил %d запросов, want 1: клиент ограничил ответ (num_predict=6), "+
			"а балансер всё равно продолжил его — клиент получит ответ длиннее заказанного и "+
			"заплатит за двойной промпт (найдено client-matrix: +45 токенов)", n)
	}
}

// TestR174_CappedStream_OpenAIMaxTokens — то же для OpenAI-поверхности.
func TestR174_CappedStream_OpenAIMaxTokens(t *testing.T) {
	n := runStreamProxyR174(t, map[string]interface{}{"max_tokens": 8})
	if n != 1 {
		t.Errorf("upstream получил %d запросов, want 1: max_tokens клиента не уважен", n)
	}
}

// TestR174_UncappedStream_StillContinues — обратная сторона: без лимита клиента
// auto-continue обязан работать как раньше (иначе фикс выключил фичу целиком).
func TestR174_UncappedStream_StillContinues(t *testing.T) {
	n := runStreamProxyR174(t, nil)
	if n < 2 {
		t.Errorf("upstream получил %d запросов, want >=2: без лимита клиента auto-continue "+
			"должен продолжать оборванный ответ (R60.21)", n)
	}
}

// TestR174_TruncatedButCapped_ContentNotExtended — клиент не должен получить
// текст, которого не заказывал: ответ ровно такой, каким его отдал upstream.
func TestR174_TruncatedButCapped_ContentNotExtended(t *testing.T) {
	t.Setenv("LB_AUTO_CONTINUE_ON_TRUNCATION", "1")
	t.Setenv("LB_AUTO_CONTINUE_CHAT_POLICY", "")

	upstream, _ := makeRegeneratingUpstreamR66d(t)
	p, _, cleanup := makeTestLlamaProxy(t, upstream.URL)
	defer cleanup()

	bodyObj := map[string]interface{}{
		"model":    "Qwen3.8-27B",
		"messages": []interface{}{map[string]string{"role": "user", "content": "покажи пример кода"}},
		"stream":   true,
		"options":  map[string]interface{}{"num_predict": 6},
	}
	bodyBytes, _ := json.Marshal(bodyObj)
	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	if err := p.proxyRequestLlamaCpp(rec, req, "llama_test", bodyBytes); err != nil {
		t.Fatalf("proxyRequestLlamaCpp: %v", err)
	}
	out := rec.Body.String()
	if strings.Contains(out, regeneratedR66d) {
		t.Errorf("в ответе клиенту есть ПРОДОЛЖЕНИЕ (regenerated), хотя он поставил лимит: %q", out)
	}
}
