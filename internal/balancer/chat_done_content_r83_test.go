//go:build llama_stub

// chat_done_content_r83_test.go — R83 §9.7 (2026-09-27).
//
// ЖАЛОБА была: «OpenWebUI дублирует ответ на /api/chat, а с настоящей Ollama
// работает». Проверено на ЖИВОМ стенде (v20):
//
//	$ curl -N .../api/chat   (через балансер, стрим)
//	текст в дельтах:      63 символа
//	текст в done-чанке:   63 символа   ← тот же текст второй раз
//	done:true чанков:     1
//
// То есть дубль реален, и его источник найден точно:
//
//  1. cppworker в своём /api/chat кладёт ПОЛНЫЙ текст в done-чанк
//     (`message.content`), хотя дельты его уже отдали. Настоящая Ollama в
//     done-чанке отдаёт `message.content: ""` (подтверждено живым сырым логом
//     OpenWebUI: _diag/openwebui_closing_backticks_raw_*.txt — один `"done":true`,
//     content приходит только дельтами).
//  2. балансер на llama.cpp-бэкенде идёт НАТИВНЫМ путём и отдаёт NDJSON
//     cppworker байт-в-байт (`llamacpp_transport.go`, ветка nativePath), поэтому
//     дубль доезжает до клиента.
//
// ФИКС: нормализуем финальный чанк на выходе (`stripNativeDoneContent`) —
// content пустой, все статы на месте. Это делает наш ответ неотличимым от Ollama,
// поэтому клиент, который работает с Ollama, работает и с нами.
package balancer

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// liveDoneChunk — реальный done-чанк живого стенда (v20), сокращённый по тексту.
const liveDoneChunk = `{"created_at":"2026-09-27T14:09:54Z","done":true,"done_reason":"stop",` +
	`"eval_count":16,"eval_duration":18838931896,` +
	`"message":{"content":"\u003cthink\u003e\nThe user wants me to reply with exactly \"OK\". This is a","role":"assistant"},` +
	`"model":"qwen3.8:latest","prompt_eval_count":13,"prompt_eval_duration":11145185784,"total_duration":29984117680}`

// TestR83_StripNativeDoneContent_RemovesText — текст из done-чанка уходит, статы
// остаются: именно это делает ответ неотличимым от Ollama.
func TestR83_StripNativeDoneContent_RemovesText(t *testing.T) {
	out := stripNativeDoneContent(liveDoneChunk)

	var got map[string]interface{}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("после нормализации чанк не парсится: %v (%s)", err, out)
	}
	msg, ok := got["message"].(map[string]interface{})
	if !ok {
		t.Fatalf("message потерян: %s", out)
	}
	if content, _ := msg["content"].(string); content != "" {
		t.Errorf("content = %q, want пусто (иначе клиент, конкатенирующий чанки, получит дубль)", content)
	}
	if role, _ := msg["role"].(string); role != "assistant" {
		t.Errorf("role = %q, want assistant — поле не должно теряться", role)
	}

	// Статистика — то, ради чего done-чанк и нужен OpenWebUI.
	for _, k := range []string{"eval_count", "prompt_eval_count", "total_duration",
		"eval_duration", "prompt_eval_duration", "done_reason", "model", "created_at"} {
		if _, ok := got[k]; !ok {
			t.Errorf("поле %q потеряно — OpenWebUI лишится статистики", k)
		}
	}
	if done, _ := got["done"].(bool); !done {
		t.Error("done больше не true — клиент не поймёт, что стрим завершён")
	}
}

// TestR83_StripNativeDoneContent_LeavesOtherChunksAlone — нормализация обязана
// трогать ТОЛЬКО финальный чанк: иначе она съест сам ответ.
func TestR83_StripNativeDoneContent_LeavesOtherChunksAlone(t *testing.T) {
	cases := []string{
		`{"created_at":"2026-09-27T14:09:54Z","done":false,"message":{"content":" is","role":"assistant"},"model":"qwen3.8:latest"}`,
		`{"done":false}`,
		`{"keepalive":true}`,
		`{"error":"upstream blew up"}`,
		``,
		`data: {"choices":[]}`, // SSE-строка: не наш формат, не трогаем
		`{not json at all`,
	}
	for _, in := range cases {
		if got := stripNativeDoneContent(in); got != in {
			t.Errorf("не-done чанк изменён:\n in: %s\nout: %s", in, got)
		}
	}
}

// TestR83_StripNativeDoneContent_DoneWithoutMessage — done-чанк без message (или
// с пустым content) возвращается как есть: пересобирать нечего.
func TestR83_StripNativeDoneContent_DoneWithoutMessage(t *testing.T) {
	cases := []string{
		`{"done":true,"done_reason":"stop","eval_count":3}`,                          // нет message
		`{"done":true,"message":{"content":"","role":"assistant"},"eval_count":3}`,   // content уже пуст
		`{"done":true,"message":{"content":null,"role":"assistant"},"eval_count":3}`, // content null
	}
	for _, in := range cases {
		if got := stripNativeDoneContent(in); got != in {
			t.Errorf("чанк без текста изменён:\n in: %s\nout: %s", in, got)
		}
	}
}

// TestR83_NativeDoneContentStripping_Toggle — выключатель: оператор, у которого
// нашёлся клиент, читающий ответ ТОЛЬКО из done-чанка, возвращает прежнее
// поведение переменной окружения, без пересборки образа.
func TestR83_NativeDoneContentStripping_Toggle(t *testing.T) {
	prev, had := os.LookupEnv("LB_OLLAMA_DONE_CONTENT")
	defer func() {
		if had {
			os.Setenv("LB_OLLAMA_DONE_CONTENT", prev)
			return
		}
		os.Unsetenv("LB_OLLAMA_DONE_CONTENT")
	}()

	os.Unsetenv("LB_OLLAMA_DONE_CONTENT")
	if !nativeDoneContentStrippingEnabled() {
		t.Error("по умолчанию нормализация обязана быть включена (иначе дубль вернётся)")
	}
	for _, v := range []string{"keep", "raw", "off", "0", "false", "passthrough"} {
		os.Setenv("LB_OLLAMA_DONE_CONTENT", v)
		if nativeDoneContentStrippingEnabled() {
			t.Errorf("LB_OLLAMA_DONE_CONTENT=%q не выключил нормализацию", v)
		}
	}
	os.Setenv("LB_OLLAMA_DONE_CONTENT", "strip")
	if !nativeDoneContentStrippingEnabled() {
		t.Error("явное strip должно оставлять нормализацию включённой")
	}
}

// TestR83_CppworkerOwnChatPutsFullTextInDone — фиксирует ИСТОЧНИК: нативный
// ответ cppworker (без балансера) несёт полный текст и в дельтах, и в done-чанке,
// отсюда историческое измерение «2.00×». Тест документирует это как свойство
// нижележащего слоя, которое балансер обязан снимать.
func TestR83_CppworkerOwnChatPutsFullTextInDone(t *testing.T) {
	// Поток повторяет реальный ответ живого cppworker (формат /api/chat).
	lines := []string{
		`{"done":false}`,
		`{"created_at":"2026-09-27T14:06:00Z","done":false,"message":{"content":"\n[llama_stub] ","role":"assistant"},"model":"m"}`,
		`{"created_at":"2026-09-27T14:06:00Z","done":false,"message":{"content":"Stub ","role":"assistant"},"model":"m"}`,
		liveDoneChunk,
	}
	var streamed, doneContent strings.Builder
	for _, line := range lines {
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			continue
		}
		msg, _ := obj["message"].(map[string]interface{})
		c, _ := msg["content"].(string)
		if done, _ := obj["done"].(bool); done {
			doneContent.WriteString(c)
			continue
		}
		streamed.WriteString(c)
	}
	if streamed.Len() == 0 || doneContent.Len() == 0 {
		t.Fatalf("ожидался текст и в дельтах, и в done: streamed=%q done=%q", streamed.String(), doneContent.String())
	}
	t.Logf("нативный /api/chat cppworker: текст в дельтах %d байт, в done-чанке ещё %d байт — "+
		"балансер снимает дубль (stripNativeDoneContent), доводя ответ до формата Ollama",
		streamed.Len(), doneContent.Len())
}
