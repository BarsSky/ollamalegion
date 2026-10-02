// max_output_tokens_r69_test.go — R69 (2026-09-23): совместимость с реальным
// клиентом Cline, который шлёт `max_output_tokens`.
//
// ЖАЛОБА (Cline в VS Code, провайдер Ollama, скриншот 2026-09-23):
//
//	preflight: prompt + n_predict exceeds n_ctx for this backend
//	invalid JSON: json: unknown field "max_output_tokens"
//
// Второе сообщение — ответ cppworker: строгий декодер
// (types.DecodeJSONRequest → DisallowUnknownFields) отвергал КАЖДЫЙ запрос
// Cline, потому что в теле есть `max_output_tokens` (поле OpenAI Responses API,
// которого нет в Ollama-протоколе). Прецедент — `tool_choice` (R66b): тогда
// Cline тоже был полностью неработоспособен против cppworker.
//
// Здесь проверяются три вещи:
//  1. строгий декодер принимает max_output_tokens на /api/chat, /api/generate и
//     /v1/chat/completions (иначе клиент снова получит 400);
//  2. значение реально доезжает до GenerationParams как ОГРАНИЧИТЕЛЬ СВЕРХУ
//     (min() к политике), а не как запрошенная длина — R83/v67: «приму до N»
//     ≠ «сгенерируй N»;
//  3. явные Ollama-поля (options.num_predict / max_tokens) остаются
//     приоритетными.
package main

import (
	"encoding/json"
	"testing"
)

// decodeRaw — строгий декодер cppworker (тот же, что в HTTP-хендлерах):
// проверяет, что поле известно структуре.
func decodeRaw(t *testing.T, body string, dst interface{}) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), dst); err != nil {
		t.Fatalf("json.Unmarshal: %v (body=%s)", err, body)
	}
}

// clineBodyR69 — тело, максимально близкое к тому, что шлёт Cline:
// tools[] + tool_choice + options.num_ctx + max_output_tokens.
const clineBodyR69 = `{"model":"dummy","messages":[{"role":"user","content":"опиши проект"}],` +
	`"stream":false,"options":{"num_ctx":65536},` +
	`"tools":[{"type":"function","function":{"name":"read_file","description":"Read file",` +
	`"parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}}],` +
	`"tool_choice":"auto","max_output_tokens":4096}`

// TestR69_OllamaChat_AcceptsMaxOutputTokens — HTTP-уровень: 400 unknown field
// больше не возвращается (главный предохранитель жалобы).
func TestR69_OllamaChat_AcceptsMaxOutputTokens(t *testing.T) {
	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	status, body := postRawBody(t, srv.URL, "/api/chat", clineBodyR69)
	assertNotUnknownField400(t, "Cline (max_output_tokens)", "/api/chat", status, body)
	if status == 400 && body != "" {
		t.Fatalf("Cline-тело отвергнуто: HTTP %d %s", status, body)
	}
	t.Logf("Cline-тело (max_output_tokens) → HTTP %d", status)
}

// TestR69_OpenAIChat_AcceptsMaxOutputTokens — то же для OpenAI-совместимого
// пути (Cline умеет работать в обоих режимах, поле шлёт в обоих).
func TestR69_OpenAIChat_AcceptsMaxOutputTokens(t *testing.T) {
	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	body := `{"model":"dummy","messages":[{"role":"user","content":"hi"}],"stream":false,` +
		`"max_output_tokens":2048}`
	status, respBody := postRawBody(t, srv.URL, "/v1/chat/completions", body)
	assertNotUnknownField400(t, "OpenAI (max_output_tokens)", "/v1/chat/completions", status, respBody)
	if status == 400 && respBody != "" {
		t.Fatalf("OpenAI-тело с max_output_tokens отвергнуто: HTTP %d %s", status, respBody)
	}
	t.Logf("OpenAI-тело (max_output_tokens) → HTTP %d", status)
}

// TestR69_OllamaGenerate_AcceptsMaxOutputTokens — /api/generate (структуры
// связаны через normalizeGenerateRequest).
func TestR69_OllamaGenerate_AcceptsMaxOutputTokens(t *testing.T) {
	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	body := `{"model":"dummy","prompt":"hi","stream":false,"max_output_tokens":1024}`
	status, respBody := postRawBody(t, srv.URL, "/api/generate", body)
	assertNotUnknownField400(t, "Ollama generate (max_output_tokens)", "/api/generate", status, respBody)
	if status == 400 && respBody != "" {
		t.Fatalf("/api/generate с max_output_tokens отвергнут: HTTP %d %s", status, respBody)
	}
	t.Logf("/api/generate (max_output_tokens) → HTTP %d", status)
}

// TestR69_MaxOutputTokensReachesParams — семантика (R83/v67):
//
//	max_output_tokens = ВЕРХНЯЯ ГРАНИЦА объёма ответа, НЕ запрошенная длина.
//
// Было (R69): значение подставлялось в MaxTokens, то есть «клиент примет до
// 4096» превращалось в «сгенерируй до 4096» — а Cline шлёт 32000-64000 «на
// всякий случай», и модель получала право генерировать десятки минут.
//
// Проверяем три инварианта: (1) значение распарсилось; (2) оно НЕ стало
// MaxTokens; (3) оно доехало до GenerationParams как ограничитель сверху.
func TestR69_MaxOutputTokensReachesParams(t *testing.T) {
	req := chatRequest{}
	decodeRaw(t, clineBodyR69, &req)
	if req.MaxOutputTokens == nil || *req.MaxOutputTokens != 4096 {
		t.Fatalf("max_output_tokens не распарсился: %+v", req.MaxOutputTokens)
	}
	genReq := buildGenerateRequestFromChat(req, "PROMPT")
	if genReq.MaxTokens != 0 {
		t.Errorf("MaxTokens = %d, ожидалось 0: max_output_tokens — верхняя граница, "+
			"а не запрошенная длина ответа (иначе «приму до 4096» = «сгенерируй 4096»)", genReq.MaxTokens)
	}
	if genReq._upperBoundNumPredict != 4096 {
		t.Errorf("верхняя граница = %d, ожидалось 4096 (значение обязано читаться)", genReq._upperBoundNumPredict)
	}

	// Граница обязана доезжать до параметров генерации как min() к политике.
	params := buildGenerationParams(genReq)
	if params.NPredict > 4096 {
		t.Errorf("NPredict = %d: верхняя граница клиента не применена (должно быть ≤ 4096)", params.NPredict)
	}
}

// TestR69_UpperBoundOnlyClampsDown — «приму до 4096» не должно ПОДНИМАТЬ длину
// выше политики (64000 у Cline ≠ 64000 токенов генерации).
func TestR69_UpperBoundOnlyClampsDown(t *testing.T) {
	body := `{"model":"gemma-4-E4B-it-Q4_K_M","messages":[{"role":"user","content":"2+2?"}],` +
		`"max_output_tokens":64000}`
	req := chatRequest{}
	decodeRaw(t, body, &req)
	genReq := buildGenerateRequestFromChat(req, "P")
	params := buildGenerationParams(genReq)
	if params.NPredict >= 64000 {
		t.Fatalf("NPredict = %d: большое max_output_tokens стало длиной генерации — "+
			"именно это давало десятки минут генерации и «пустой ответ» по клиентскому таймауту",
			params.NPredict)
	}
}

// TestR69_ExplicitNumPredictWins — приоритет Ollama-полей: если клиент задал
// options.num_predict (или top-level max_tokens), max_output_tokens не должен
// их перебивать.
func TestR69_ExplicitNumPredictWins(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"options":{"num_predict":123},"max_output_tokens":4096}`
	req := chatRequest{}
	decodeRaw(t, body, &req)
	genReq := buildGenerateRequestFromChat(req, "P")
	if genReq.MaxTokens != 123 {
		t.Errorf("MaxTokens = %d, ожидалось 123 (options.num_predict приоритетнее max_output_tokens)", genReq.MaxTokens)
	}

	body2 := `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"max_tokens":77,"max_output_tokens":4096}`
	req2 := chatRequest{}
	decodeRaw(t, body2, &req2)
	genReq2 := buildGenerateRequestFromChat(req2, "P")
	if genReq2.MaxTokens != 77 {
		t.Errorf("MaxTokens = %d, ожидалось 77 (top-level max_tokens приоритетнее)", genReq2.MaxTokens)
	}
}
