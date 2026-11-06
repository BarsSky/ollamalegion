//go:build llama_stub

// handlers_tokenize_r467_test.go — R467 (2026-11-06): внешний точный подсчёт
// токенов.
//
// ЗАЧЕМ. Балансер решает про окно по эвристике (завышает в 2–3 раза), а
// токенизатор модели есть только в воркере. Эндпоинт даёт этот токенизатор
// наружу, причём ЧЕСТНО говорит, чем посчитано: "tokenizer" (модель загружена)
// или "estimate" (модель не загружена — грузить её ради подсчёта нельзя).
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func postTokenizeR467(t *testing.T, body string) (int, map[string]interface{}) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/tokenize", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handleTokenizeR467(rec, req)
	var out map[string]interface{}
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

// TestR467_RequiresInput — без текста/messages и без модели считать нечего:
// понятная 400, а не ноль, который клиент примет за факт.
func TestR467_RequiresInput(t *testing.T) {
	code, out := postTokenizeR467(t, `{"model":"m"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("пустое тело: код %d, want 400 (получено %v)", code, out)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "text") || !strings.Contains(msg, "messages") {
		t.Errorf("ошибка не объясняет, что передать: %q", msg)
	}

	code, out = postTokenizeR467(t, `{"text":"привет"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("без модели: код %d, want 400 (получено %v)", code, out)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "model") {
		t.Errorf("ошибка не называет model: %q", msg)
	}

	code, _ = postTokenizeR467(t, `{not json`)
	if code != http.StatusBadRequest {
		t.Fatalf("битый JSON: код %d, want 400", code)
	}
}

// TestR467_MethodNotAllowed — только POST: подсчёт не должен случаться по GET.
func TestR467_MethodNotAllowed(t *testing.T) {
	rec := httptest.NewRecorder()
	handleTokenizeR467(rec, httptest.NewRequest(http.MethodGet, "/api/tokenize", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: код %d, want 405", rec.Code)
	}
}

// TestR467_EstimateIsLabelled — без загруженной модели ответ обязан называть
// источник "estimate": иначе клиент примет эвристику за точное число (ровно
// класс дефекта, который R466 лечил на стороне балансера).
func TestR467_EstimateIsLabelled(t *testing.T) {
	code, out := postTokenizeR467(t, `{"model":"no-such-model","text":"hello world"}`)
	if code != http.StatusOK {
		t.Fatalf("код %d, want 200 (получено %v)", code, out)
	}
	if src, _ := out["source"].(string); src != "estimate" {
		t.Fatalf("source = %q, want estimate (модель не загружена)", src)
	}
	if loaded, _ := out["model_loaded"].(bool); loaded {
		t.Fatal("model_loaded=true для незагруженной модели")
	}
	if tokens, _ := out["tokens"].(float64); tokens <= 0 {
		t.Errorf("tokens = %v, want > 0", out["tokens"])
	}
	if note, _ := out["note"].(string); note == "" {
		t.Error("нет пояснения, почему это оценка — клиент не поймёт, можно ли верить числу")
	}
	if notice, _ := out["template_notice"].(string); !strings.Contains(notice, "template") {
		t.Errorf("нет оговорки про chat template: %q", notice)
	}
}

// TestR467_MessagesSum — режим messages: сумма по сообщениям плюс по каждому
// отдельно (клиенту нужно видеть, где дорого).
func TestR467_MessagesSum(t *testing.T) {
	body := `{"model":"no-such-model","messages":[
		{"role":"system","content":"ты ассистент"},
		{"role":"user","content":"объясни разницу между a и b"}
	]}`
	code, out := postTokenizeR467(t, body)
	if code != http.StatusOK {
		t.Fatalf("код %d, want 200", code)
	}
	msgs, ok := out["messages"].([]interface{})
	if !ok || len(msgs) != 2 {
		t.Fatalf("messages = %v, want 2 элемента", out["messages"])
	}
	total := 0
	for _, raw := range msgs {
		m := raw.(map[string]interface{})
		total += int(m["tokens"].(float64))
	}
	if got := int(out["tokens"].(float64)); got != total {
		t.Errorf("tokens = %d, а сумма по сообщениям = %d — числа расходятся", got, total)
	}
}
