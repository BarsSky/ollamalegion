//go:build llama_stub

// history_reasoning_echo_test.go — R106 (2026-10-11).
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА (Cline → балансер :18080, модель qwen3.8-27b):
//
//	invalid JSON: json: unknown field "thinking"
//
// при попытке ПРОДОЛЖИТЬ СТАРУЮ СЕССИЮ.
//
// Что происходило:
//  1. `message.thinking` — каноническое имя reasoning-поля в Ollama API
//     (docs/architecture/balancer-cppworker-contract.md §4.1); наши поверхности отдают
//     рассуждения под обоими именами — нативный /api/chat как `reasoning`
//     (handlers_chat.go:1070), трансляция SSE→NDJSON балансера как `thinking`
//     (internal/balancer/llamacpp_translate_resp.go:970), /api/generate как
//     `thinking`.
//  2. Клиент (Cline) нормализует ответ ассистента в своё поле `thinking` и при
//     продолжении сессии отправляет историю обратно — вместе с ним.
//  3. cppworker декодирует тело строгим декодером
//     (types.DecodeJSONRequest → DisallowUnknownFields,
//     pkg/types/contract_validation.go:71), в chatMessage поля `thinking` не было
//     → HTTP 400 на ВАЛИДНОМ запросе, то есть сессию нельзя было продолжить
//     вообще (ни одного нового сообщения).
//
// Ровно тот же дефект был на OpenAI-поверхности: cppworker САМ отдаёт
// `reasoning_content` в ответе (handlers_openai.go:630-690), а клиент возвращает
// его в истории → 400 `unknown field "reasoning_content"`.
//
// Тесты ниже фиксируют обе половины контракта:
//   - history-echo рассуждений ПРИНИМАЕТСЯ (не 400);
//   - он НЕ влияет на промпт (принимаем и игнорируем — как `reasoning`);
//   - строгий декодер при этом НЕ отключён: реальная опечатка по-прежнему 400.
package main

import (
	"net/http"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// clineContinueOldSessionBody — тело, которое Cline (провайдер "ollama")
// отправляет при продолжении сохранённой сессии: в истории есть сообщение
// ассистента с `thinking` — ровно то, что мы сами ему отдали в прошлом ответе.
const clineContinueOldSessionBody = `{
	"model":"qwen3.8-27b",
	"messages":[
		{"role":"system","content":"You are Cline, a highly skilled software engineer."},
		{"role":"user","content":"Сколько будет 17*23?"},
		{"role":"assistant","content":"391","thinking":"17*23 = 17*20 + 17*3 = 340 + 51 = 391"},
		{"role":"user","content":"а 18*23?"}
	],
	"options":{"num_ctx":32768,"temperature":0},
	"tool_choice":"auto",
	"stream":false
}`

// openAIEchoHistoryBody — то же самое на OpenAI-поверхности: клиент вернул
// `reasoning_content`, который cppworker сам положил в предыдущий ответ.
const openAIEchoHistoryBody = `{
	"model":"qwen3.8-27b",
	"messages":[
		{"role":"user","content":"Сколько будет 17*23?"},
		{"role":"assistant","content":"391","reasoning_content":"17*23 = 340 + 51 = 391"},
		{"role":"user","content":"а 18*23?"}
	],
	"stream":false,
	"max_tokens":2048
}`

// TestR106_OllamaChat_AcceptsSessionHistoryWithThinking — HTTP-уровень: тело
// продолжения сессии из Cline не должно получать 400 от строгого декодера.
func TestR106_OllamaChat_AcceptsSessionHistoryWithThinking(t *testing.T) {
	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	status, body := postRawBody(t, srv.URL, "/api/chat", clineContinueOldSessionBody)
	assertNotUnknownField400(t, "Cline (ollama provider, continuation of an old session)", "/api/chat", status, body)
	t.Logf("Cline continuation → HTTP %d (%s)", status, strings.TrimSpace(body))
}

// TestR106_OpenAIChat_AcceptsSessionHistoryWithReasoningContent — то же для
// OpenAI-соверхности.
func TestR106_OpenAIChat_AcceptsSessionHistoryWithReasoningContent(t *testing.T) {
	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	status, body := postRawBody(t, srv.URL, "/v1/chat/completions", openAIEchoHistoryBody)
	assertNotUnknownField400(t, "client echo of reasoning_content", "/v1/chat/completions", status, body)
	t.Logf("reasoning_content echo → HTTP %d (%s)", status, strings.TrimSpace(body))
}

// TestR106_OllamaChat_ThinkingAcceptedButNotInPrompt — главный смысловой тест:
// поле принимается, но НЕ меняет промпт. Сравниваем промпт для одной и той же
// истории с `thinking` и без него: он обязан быть байт-в-байт одинаковым.
//
// Так регрессия «поле молча поехало в промпт» (или «поле снова 400») ловится
// без зависимости от того, что именно вернёт stub-шаблон.
func TestR106_OllamaChat_ThinkingAcceptedButNotInPrompt(t *testing.T) {
	withThinking := `{"model":"qwen3.8-27b","stream":false,"messages":[
		{"role":"user","content":"Сколько будет 17*23?"},
		{"role":"assistant","content":"391","thinking":"marker-17x23-рассуждение"},
		{"role":"user","content":"а 18*23?"}]}`
	withoutThinking := `{"model":"qwen3.8-27b","stream":false,"messages":[
		{"role":"user","content":"Сколько будет 17*23?"},
		{"role":"assistant","content":"391"},
		{"role":"user","content":"а 18*23?"}]}`

	var reqWith, reqWithout chatRequest
	if err := types.DecodeJSONRequest(strings.NewReader(withThinking), types.MaxStreamingBodyBytes, &reqWith); err != nil {
		t.Fatalf("строгий декодер отверг историю с `thinking`: %v "+
			"(это ровно та ошибка, с которой Cline не мог продолжить сессию)", err)
	}
	if err := types.DecodeJSONRequest(strings.NewReader(withoutThinking), types.MaxStreamingBodyBytes, &reqWithout); err != nil {
		t.Fatalf("decode без `thinking`: %v", err)
	}
	if got := reqWith.Messages[1].Thinking; got != "marker-17x23-рассуждение" {
		t.Fatalf("`thinking` не дочитан в chatMessage: got %q", got)
	}

	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackend()

	const model = "qwen3.8-27b"
	if err := backend.InjectLoadedModelForTest(model); err != nil {
		t.Fatalf("InjectLoadedModelForTest: %v", err)
	}

	promptWith, err := buildChatPromptWithOptions(reqWith.Messages, model, chatPromptOptions{})
	if err != nil {
		t.Fatalf("buildChatPromptWithOptions (с thinking): %v", err)
	}
	promptWithout, err := buildChatPromptWithOptions(reqWithout.Messages, model, chatPromptOptions{})
	if err != nil {
		t.Fatalf("buildChatPromptWithOptions (без thinking): %v", err)
	}

	if promptWith != promptWithout {
		t.Errorf("`thinking` из истории попал в промпт: промпты различаются.\n"+
			"с thinking:    %q\nбез thinking: %q", promptWith, promptWithout)
	}
	if strings.Contains(promptWith, "marker-17x23-рассуждение") {
		t.Errorf("текст предыдущих рассуждений оказался в промпте: %q", promptWith)
	}
}

// TestR106_OpenAIChat_ReasoningContentAcceptedButNotConverted — то же для
// OpenAI-пути: поле дочитывается в openAIChatMessage, но openAIToChatMessage
// (единственный мост к сборке промпта) его не переносит.
func TestR106_OpenAIChat_ReasoningContentAcceptedButNotConverted(t *testing.T) {
	var req openAIChatCompletionRequest
	if err := types.DecodeJSONRequest(strings.NewReader(openAIEchoHistoryBody),
		types.MaxStreamingBodyBytes, &req); err != nil {
		t.Fatalf("строгий декодер отверг историю с `reasoning_content`: %v", err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(req.Messages))
	}
	if got := req.Messages[1].ReasoningContent; got == "" {
		t.Fatal("`reasoning_content` не дочитан в openAIChatMessage")
	}

	converted := openAIToChatMessage(req.Messages)
	if len(converted) != 3 {
		t.Fatalf("openAIToChatMessage вернул %d сообщений, want 3", len(converted))
	}
	if converted[1].Content != "391" {
		t.Errorf("content ассистента потерян: %q", converted[1].Content)
	}
	if converted[1].Thinking != "" || converted[1].ReasoningContent != "" {
		t.Errorf("reasoning_content из истории просочился в промпт-сообщение: %+v", converted[1])
	}
}

// TestR106_ClientChatToleratesTypos_MgmtStaysStrict — R106c (2026-10-12):
// контракт изменился сознательно.
//
// Раньше этот тест требовал 400 на опечатку в поле верхнего уровня /api/chat.
// Живая практика показала, что строгость на КЛИЕНТСКОМ API вредна: она ломала
// OpenWebUI (options.use_mmap/num_gpu), Cline (tool_choice/max_output_tokens) и
// историю сессии (thinking). Настоящая Ollama неизвестные поля игнорирует.
//
// Теперь: на /api/chat опечатка/чужое поле НЕ приводит к отказу «unknown field»
// (запрос идёт дальше — в тесте упирается в отсутствие модели), а на
// УПРАВЛЯЮЩЕМ endpoint'е строгость сохранена (см.
// TestLoadWithParams_StrictDecode_RejectsUnknownField и
// TestR106c_LenientDecode_ReportsUnknownFields — там проверяется, что имена
// неизвестных полей возвращаются для лога).
func TestR106_ClientChatToleratesTypos_MgmtStaysStrict(t *testing.T) {
	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	typo := `{"model":"dummy","stream":false,"thinking_hint":"опечатка","messages":[{"role":"user","content":"hi"}]}`
	status, body := postRawBody(t, srv.URL, "/api/chat", typo)
	if status == http.StatusBadRequest && strings.Contains(body, "unknown field") {
		t.Fatalf("/api/chat отверг незнакомое поле (400 unknown field) — это ломает живых клиентов "+
			"и расходится с поведением Ollama. Статус=%d body=%q", status, body)
	}
	t.Logf("/api/chat с лишним полем → HTTP %d (тело: %s)", status, strings.TrimSpace(body))

	// Управляющий endpoint (клиент — балансер) остаётся строгим.
	mgmt := `{"name":"dummy","context_size":32768}`
	status2, body2 := postRawBody(t, srv.URL, "/api/models/load-with-params", mgmt)
	if status2 != http.StatusBadRequest || !strings.Contains(body2, "unknown field") {
		t.Fatalf("управляющий endpoint обязан остаться строгим: статус=%d body=%q", status2, body2)
	}
}
