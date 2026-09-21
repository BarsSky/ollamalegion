//go:build llama_stub

// ollama_chat_contract_r65d_test.go — R65d (2026-09-20) контрактный
// тест-предохранитель для Ollama-совместимости /api/chat и /v1/chat/completions.
//
// ЗАЧЕМ ЭТОТ ФАЙЛ СУЩЕСТВУЕТ
//
// Аудит 2026-09-20 показал: cppworker декодирует тела запросов строгим
// декодером с DisallowUnknownFields (pkg/types/contract_validation.go:71), но
// структуры запросов были Уже, чем реальные тела клиентов. Любое «лишнее»
// Ollama-поле (options, keep_alive, format, think, images) давало HTTP 400
// `unknown field "..."` на штатном запросе — то есть отказ, а не деградацию.
//
// Пакет cmd/cppworker не компилировался в CI со старыми сигнатурами
// (ensureModelLoaded, NewAbortWatcher), поэтому дефект жил незамеченным.
//
// Этот файл фиксирует РЕАЛЬНЫЕ тела популярных клиентов как тест-кейсы. Если
// кто-то сузит chatRequest/openAIChatCompletionRequest и снова начнёт отвечать
// 400 на валидный запрос — тест упадёт с указанием конкретного поля.
//
// Тест намеренно НЕ требует загруженной модели: он проверяет только стадию
// декодирования запроса (до inference), поэтому падение на "model not found"
// или паника stub-моста — это ОК, а вот 400 с "unknown field" — нет.
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// postRawBody отправляет сырое тело на path и возвращает (status, body).
func postRawBody(t *testing.T, srvURL, path, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(srvURL+path, "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("POST %s failed: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// assertNotUnknownField400 — падает, если cppworker отверг тело как
// «неизвестное поле» или «несоответствие типа». Это ровно тот класс дефектов,
// который защищает R65d.
func assertNotUnknownField400(t *testing.T, client, path string, status int, body string) {
	t.Helper()
	if status != http.StatusBadRequest {
		return
	}
	low := strings.ToLower(body)
	for _, bad := range []string{
		"unknown field",
		"cannot unmarshal",
		"invalid json",
	} {
		if strings.Contains(low, bad) {
			t.Errorf("REGRESSION [%s] %s: HTTP 400 из-за строгого декодера (body=%q). "+
				"Клиент прислал валидный запрос, а cppworker отверг его на стадии разбора — "+
				"нужно расширить структуру запроса.", client, path, body)
			return
		}
	}
}

// clientOllamaRequest — тело, которое шлёт Ollama-клиент в /api/chat.
type clientOllamaRequest struct {
	client string
	body   string
}

// ollamaChatClientBodies — реальные тела клиентов Ollama-режима.
var ollamaChatClientBodies = []clientOllamaRequest{
	{
		client: "OpenWebUI (Ollama mode, typical)",
		body: `{
			"model":"dummy",
			"messages":[{"role":"system","content":"You are helpful."},{"role":"user","content":"hi"}],
			"stream":true,
			"options":{
				"num_ctx":8192,
				"num_predict":-1,
				"temperature":0.7,
				"top_p":0.9,
				"top_k":40,
				"repeat_penalty":1.1,
				"repeat_last_n":64,
				"seed":0,
				"stop":[],
				"num_keep":24,
				"min_p":0.0,
				"typical_p":1.0,
				"tfs_z":1.0,
				"mirostat":0,
				"mirostat_tau":5.0,
				"mirostat_eta":0.1,
				"frequency_penalty":0.0,
				"presence_penalty":0.0
			},
			"keep_alive":"5m"
		}`,
	},
	{
		client: "OpenWebUI (Ollama mode, JSON format)",
		body: `{
			"model":"dummy",
			"messages":[{"role":"user","content":"give me json"}],
			"stream":false,
			"format":"json",
			"options":{"num_ctx":4096,"temperature":0.0,"top_k":1}
		}`,
	},
	{
		client: "Ollama CLI / ollama-python (minimal)",
		body: `{"model":"dummy","messages":[{"role":"user","content":"hi"}],"options":{"num_ctx":2048}}`,
	},
	{
		client: "ollama run with unload (keep_alive=0)",
		body: `{"model":"dummy","messages":[{"role":"user","content":"hi"}],"stream":false,"keep_alive":"0"}`,
	},
	{
		client: "Ollama keep_alive as number (seconds)",
		body: `{"model":"dummy","messages":[{"role":"user","content":"hi"}],"keep_alive":300}`,
	},
	{
		client: "thinking model (think=true)",
		body: `{"model":"dummy","messages":[{"role":"user","content":"hi"}],"think":true,"options":{"num_ctx":8192}}`,
	},
	{
		client: "thinking model (think='high')",
		body: `{"model":"dummy","messages":[{"role":"user","content":"hi"}],"think":"high"}`,
	},
	{
		client: "vision request (images in message)",
		body: `{"model":"dummy","messages":[{"role":"user","content":"what is this?","images":["iVBORw0KGgo="]}],"stream":false}`,
	},
	{
		client: "Cline/Roo Code via ollama provider (tools)",
		body: `{
			"model":"dummy",
			"messages":[{"role":"system","content":"You are Cline."},{"role":"user","content":"list files"}],
			"stream":true,
			"tools":[{"type":"function","function":{"name":"read_file","description":"Read a file","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}}],
			"options":{"num_ctx":32768,"temperature":0,"num_predict":4096}
		}`,
	},
	{
		client: "Ollama raw mode + template override",
		body: `{"model":"dummy","messages":[{"role":"user","content":"hi"}],"raw":true,"template":"{{ .Prompt }}","system":"sys"}`,
	},
	{
		client: "Ollama truncate + logprobs",
		body: `{"model":"dummy","messages":[{"role":"user","content":"hi"}],"truncate":false,"logprobs":true,"top_logprobs":5}`,
	},
}

// openAIChatClientBodies — реальные тела клиентов OpenAI-совместимого режима.
var openAIChatClientBodies = []clientOllamaRequest{
	{
		client: "Cline (OpenAI-compatible provider)",
		body: `{
			"model":"dummy",
			"messages":[{"role":"user","content":"hi"}],
			"stream":true,
			"temperature":0,
			"max_tokens":4096,
			"num_ctx":32768,
			"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}],
			"tool_choice":"auto",
			"stream_options":{"include_usage":true}
		}`,
	},
	{
		client: "Roo Code (OpenAI-compatible)",
		body: `{"model":"dummy","messages":[{"role":"user","content":"hi"}],"stream":true,"temperature":0.2,"top_p":0.95,"max_tokens":2048,"seed":42,"stop":["</s>"],"num_ctx":16384}`,
	},
	{
		client: "OpenAI SDK (frequency/presence penalty + user)",
		body: `{"model":"dummy","messages":[{"role":"user","content":"hi"}],"temperature":0.7,"frequency_penalty":0.5,"presence_penalty":0.3,"user":"u-1","stream":false}`,
	},
	{
		client: "OpenAI SDK (response_format)",
		body: `{"model":"dummy","messages":[{"role":"user","content":"json please"}],"response_format":{"type":"json_object"},"stream":false}`,
	},
	{
		client: "OpenWebUI (OpenAI mode, tools + parallel_tool_calls)",
		body: `{"model":"dummy","messages":[{"role":"user","content":"hi"}],"tools":[],"parallel_tool_calls":false,"stream":true,"stream_options":{"include_usage":true}}`,
	},
}

// TestR65d_OllamaChat_AcceptsRealClientBodies — главный предохранитель:
// ни одно валидное Ollama-тело не должно отвергаться строгим декодером.
func TestR65d_OllamaChat_AcceptsRealClientBodies(t *testing.T) {
	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	for _, tc := range ollamaChatClientBodies {
		t.Run(tc.client, func(t *testing.T) {
			status, body := postRawBody(t, srv.URL, "/api/chat", tc.body)
			assertNotUnknownField400(t, tc.client, "/api/chat", status, body)
			t.Logf("%s → HTTP %d", tc.client, status)
		})
	}
}

// TestR65d_OpenAIChat_AcceptsRealClientBodies — то же для /v1/chat/completions.
func TestR65d_OpenAIChat_AcceptsRealClientBodies(t *testing.T) {
	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	for _, tc := range openAIChatClientBodies {
		t.Run(tc.client, func(t *testing.T) {
			status, body := postRawBody(t, srv.URL, "/v1/chat/completions", tc.body)
			assertNotUnknownField400(t, tc.client, "/v1/chat/completions", status, body)
			t.Logf("%s → HTTP %d", tc.client, status)
		})
	}
}

// TestR65d_OllamaGenerate_AcceptsRealClientBodies — /api/generate с полным
// Ollama-набором (регрессия после расширения chatRequest: generateOptions
// переиспользуется, значит структуры связаны).
func TestR65d_OllamaGenerate_AcceptsRealClientBodies(t *testing.T) {
	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	bodies := []clientOllamaRequest{
		{"ollama run (full options)", `{"model":"dummy","prompt":"hi","stream":false,"options":{"num_ctx":4096,"top_k":40,"repeat_penalty":1.1,"seed":7,"num_predict":128,"mirostat":0,"repeat_last_n":64},"keep_alive":"5m"}`},
		{"ollama run (system+format)", `{"model":"dummy","prompt":"hi","system":"sys","format":"json","raw":false,"stream":false}`},
		{"ollama run (raw+template)", `{"model":"dummy","prompt":"hi","raw":true,"template":"{{ .Prompt }}","context":[]}`},
		{"ollama run (images)", `{"model":"dummy","prompt":"describe","images":["iVBORw0KGgo="],"stream":false}`},
	}
	for _, tc := range bodies {
		t.Run(tc.client, func(t *testing.T) {
			status, body := postRawBody(t, srv.URL, "/api/generate", tc.body)
			assertNotUnknownField400(t, tc.client, "/api/generate", status, body)
			t.Logf("%s → HTTP %d", tc.client, status)
		})
	}
}

// TestR65d_ChatRequest_OptionsReachGenerationParams — семантическая проверка:
// значения из options.* и top-level полей действительно доезжают до
// bridge.GenerationParams, а не просто «принимаются декодером и забываются».
//
// Это ключевая регрессия находки 1.2: раньше /api/chat переносил только
// temperature/max_tokens/num_ctx и молча терял top_k/repeat_penalty/seed/stop.
func TestR65d_ChatRequest_OptionsReachGenerationParams(t *testing.T) {
	raw := `{
		"model":"m",
		"messages":[{"role":"user","content":"hi"}],
		"stream":false,
		"options":{
			"temperature":0.25,
			"top_p":0.85,
			"top_k":37,
			"repeat_penalty":1.15,
			"repeat_last_n":128,
			"seed":4242,
			"num_predict":321,
			"stop":["</s>","<|im_end|>"],
			"mirostat":2,
			"mirostat_tau":4.5,
			"mirostat_eta":0.05
		},
		"keep_alive":"10m"
	}`

	var req chatRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal chatRequest: %v", err)
	}

	genReq := buildGenerateRequestFromChat(req, "PROMPT")
	params := buildGenerationParams(genReq)

	if params.Temperature != 0.25 {
		t.Errorf("Temperature = %v, want 0.25", params.Temperature)
	}
	if params.TopP != 0.85 {
		t.Errorf("TopP = %v, want 0.85", params.TopP)
	}
	if params.TopK != 37 {
		t.Errorf("TopK = %v, want 37 (регрессия: top_k терялся)", params.TopK)
	}
	if params.RepeatPenalty != 1.15 {
		t.Errorf("RepeatPenalty = %v, want 1.15 (регрессия: repeat_penalty терялся)", params.RepeatPenalty)
	}
	if params.RepeatLastN != 128 {
		t.Errorf("RepeatLastN = %d, want 128", params.RepeatLastN)
	}
	if params.Seed != 4242 {
		t.Errorf("Seed = %d, want 4242 (регрессия: seed терялся)", params.Seed)
	}
	if params.NPredict != 321 {
		t.Errorf("NPredict = %d, want 321", params.NPredict)
	}
	if params.Mirostat != 2 {
		t.Errorf("Mirostat = %d, want 2", params.Mirostat)
	}
	if params.MirostatTau != 4.5 {
		t.Errorf("MirostatTau = %v, want 4.5", params.MirostatTau)
	}
	if params.MirostatEta != 0.05 {
		t.Errorf("MirostatEta = %v, want 0.05", params.MirostatEta)
	}
	if len(params.StopSequences) != 2 {
		t.Errorf("StopSequences = %v, want 2 entries (регрессия: stop терялся)", params.StopSequences)
	}

	if got := genReq._keepAliveDuration.String(); got != "10m0s" {
		t.Errorf("keep_alive duration = %q, want 10m0s (регрессия: keep_alive терялся)", got)
	}
}

// TestR65d_ChatRequest_TopLevelOverridesOptions — top-level поля /api/chat
// должны побеждать options.* (как в /api/generate).
func TestR65d_ChatRequest_TopLevelOverridesOptions(t *testing.T) {
	temp := 0.0
	req := chatRequest{
		Model:       "m",
		Stream:      false,
		Temperature: &temp, // explicit 0 = greedy (Cline/Aider)
		Options: generateOptions{
			Temperature: 0.9,
			TopK:        10,
		},
	}
	genReq := buildGenerateRequestFromChat(req, "P")
	params := buildGenerationParams(genReq)

	if params.Temperature != 0.0 {
		t.Errorf("Temperature = %v, want 0.0 (top-level explicit 0 должен побеждать options)", params.Temperature)
	}
	if params.TopK != 10 {
		t.Errorf("TopK = %v, want 10 (options.top_k должен сохраниться)", params.TopK)
	}
}

// TestR65d_KeepAliveParsing — нормализация keep_alive (строка/число/-1/0).
func TestR65d_KeepAliveParsing(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string
		wantDur string
	}{
		{"string 5m", `"5m"`, "5m", "5m0s"},
		{"string 0 (unload)", `"0"`, "0", "0s"},
		{"string -1 (forever)", `"-1"`, "-1", defaultKeepAliveDuration.String()},
		{"absent", ``, "", defaultKeepAliveDuration.String()},
		{"number 300s", `300`, "5m0s", "5m0s"},
		{"number -1", `-1`, "-1", defaultKeepAliveDuration.String()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := json.RawMessage(tc.raw)
			got := keepAliveFromRaw(raw)
			if got != tc.want {
				t.Errorf("keepAliveFromRaw(%s) = %q, want %q", tc.raw, got, tc.want)
			}
			if d := parseKeepAlive(got); d.String() != tc.wantDur {
				t.Errorf("parseKeepAlive(%q) = %v, want %v", got, d, tc.wantDur)
			}
		})
	}
}

// TestR65d_FormatAndThinkParsing — format/think принимают оба допустимых типа.
func TestR65d_FormatAndThinkParsing(t *testing.T) {
	if !formatWantsJSON(json.RawMessage(`"json"`)) {
		t.Error(`formatWantsJSON("json") = false, want true`)
	}
	if !formatWantsJSON(json.RawMessage(`{"type":"object"}`)) {
		t.Error(`formatWantsJSON(schema) = false, want true`)
	}
	if formatWantsJSON(json.RawMessage(`""`)) {
		t.Error(`formatWantsJSON("") = true, want false`)
	}
	if formatWantsJSON(nil) {
		t.Error(`formatWantsJSON(nil) = true, want false`)
	}

	if en, ex := thinkEnabledFromRaw(json.RawMessage(`true`)); !en || !ex {
		t.Errorf("think=true → (%v,%v), want (true,true)", en, ex)
	}
	if en, ex := thinkEnabledFromRaw(json.RawMessage(`false`)); en || !ex {
		t.Errorf("think=false → (%v,%v), want (false,true)", en, ex)
	}
	if en, ex := thinkEnabledFromRaw(json.RawMessage(`"high"`)); !en || !ex {
		t.Errorf(`think="high" → (%v,%v), want (true,true)`, en, ex)
	}
	if _, ex := thinkEnabledFromRaw(nil); ex {
		t.Error("think absent should be explicit=false")
	}
}
