//go:build llama_stub

// chat_done_content_r83_test.go — R83 §9.7 + план A10 блок 3 (D6), 2026-09-27.
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
//
// D6 (план A10, «зафиксировать контракт под версию OpenWebUI из деплоя»).
// ВАЖНОЕ УТОЧНЕНИЕ, добытое чтением кода клиента: OpenWebUI в деплой НЕ ВХОДИТ
// (в compose есть только наш дашборд `ollama-legion/webui`, версия клиента
// оператором не пинится), поэтому «версия из деплоя» — это версия, которую
// оператор запускает снаружи. Отсюда контракт зафиксирован по КОДУ клиента:
//
//   - 0.9.x, `backend/open_webui/utils/response.py`:
//     `message_content if not done else None` — на done-чанке content обнулялся
//     самим клиентом;
//   - main (и 0.10/0.11), там же: content больше НЕ обнуляется
//     (`openai_chat_chunk_message_template(model, message_content, …)`), зато
//     фронтенд `src/lib/apis/streaming/index.ts` эмитит ровно
//     `parsedData.choices?.[0]?.delta?.content ?? ”`, а потребитель
//     конкатенирует эти дельты.
//
// Вывод, который и защищают тесты ниже: в ЛЮБОЙ из этих версий полный текст в
// done-чанке приводит к дублю (0.9.x — через `message.content` без обнуления в
// не-стриме/иных путях, main — через дельту), а пустой content на done не ломает
// ничего: в main он даёт `delta.content = null`, во фронтенде — `”`.
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
	// Текст, который уже уехал клиенту дельтами: cppworker кладёт в done-чанк тот
	// же текст целиком (измерено на живом стенде — 63 символа в дельтах и те же
	// 63 в done-чанке), поэтому дельты содержат текст done-чанка.
	streamed := "<think>\nThe user wants me to reply with exactly \"OK\". This is a"
	out := stripNativeDoneContent(liveDoneChunk, streamed)

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
		if got := stripNativeDoneContent(in, "текст уже отдан дельтами"); got != in {
			t.Errorf("не-done чанк изменён:\n in: %s\nout: %s", in, got)
		}
	}
}

// TestR91_StripNativeDoneContent_KeepsOnlyCarrierOfTheAnswer — R91 (2026-10-09):
// если дельтами ничего не отдано, текст done-чанка — ЕДИНСТВЕННЫЙ носитель
// ответа, и вычищать его нельзя.
//
// Живой дефект (CI: tests/llamacpp_proxy TestChatStreaming,
// TestResponseNotMarkdownBold/streaming; tests TestProxyOllama_*): апстрим отдал
// весь ответ одним финальным чанком — балансер вычищал content безусловно, и
// клиент получал `done:true` с пустым message.content, то есть пустой ответ.
// Так же выглядит наш собственный фолбэк cppworker, когда модель ушла в
// незакрытый reasoning и видимый текст удаётся отдать только в финале.
func TestR91_StripNativeDoneContent_KeepsOnlyCarrierOfTheAnswer(t *testing.T) {
	in := `{"created_at":"2026-10-09T10:30:00Z","done":true,"done_reason":"stop",` +
		`"message":{"content":"Привет! Я работающая модель llama.cpp.","role":"assistant"},` +
		`"model":"test-model"}`
	if got := stripNativeDoneContent(in, ""); got != in {
		t.Errorf("ответ потерян: единственный носитель текста вычищен\n in: %s\nout: %s", in, got)
	}
	// И то же самое, когда дельты были, но без текста (role-only чанки).
	if got := stripNativeDoneContent(in, "   "); got != in {
		t.Errorf("пробельные дельты не должны считаться «текст уже отдан»:\n%s", got)
	}
	// А когда текст уже уехал дельтами — снимаем дубль (поведение R83 §9.7).
	streamed := "Привет! Я работающая модель llama.cpp."
	out := stripNativeDoneContent(in, streamed)
	var chunk map[string]interface{}
	if err := json.Unmarshal([]byte(out), &chunk); err != nil {
		t.Fatalf("чанк не парсится: %v (%s)", err, out)
	}
	msg, _ := chunk["message"].(map[string]interface{})
	if content, _ := msg["content"].(string); content != "" {
		t.Errorf("дубль не снят: content = %q", content)
	}
	if done, _ := chunk["done"].(bool); !done {
		t.Error("done потерян при снятии дубля")
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
		if got := stripNativeDoneContent(in, "текст уже отдан дельтами"); got != in {
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

// TestD6_OpenAIStreamDoneFrameCarriesNoDelta — D6, вторая ветка: SSE-клиент
// (OpenAI-совместимый путь) читает ТОЛЬКО `choices[0].delta.content`.
//
// Почему это часть контракта: фронтенд OpenWebUI (`src/lib/apis/streaming/index.ts`)
// делает ровно
//
//	yield { done: false, value: parsedData.choices?.[0]?.delta?.content ?? '' }
//
// и конкатенирует `value` к сообщению. Значит любой текст в delta финального
// кадра будет приклеен ВТОРЫМ экземпляром ответа — независимо от того, что
// бэкенд OpenWebUI (в 0.9.x) обнулял content на done, а в main уже нет.
func TestD6_OpenAIStreamDoneFrameCarriesNoDelta(t *testing.T) {
	stream := []byte(
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hello \",\"role\":\"assistant\"},\"finish_reason\":null,\"index\":0}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"content\":\"world\",\"role\":\"assistant\"},\"finish_reason\":null,\"index\":0}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"content\":null,\"role\":\"assistant\"},\"finish_reason\":\"stop\",\"index\":0}]}\n\n" +
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n" +
			"data: [DONE]\n\n")

	var streamed strings.Builder
	var doneDeltas []string
	for _, raw := range strings.Split(string(stream), "\n\n") {
		raw = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "data:"))
		if raw == "" || raw == "[DONE]" {
			continue
		}
		var frame map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &frame); err != nil {
			t.Fatalf("кадр не разобран: %v (%s)", err, raw)
		}
		choices, _ := frame["choices"].([]interface{})
		if len(choices) == 0 {
			continue // usage-only кадр
		}
		choice, _ := choices[0].(map[string]interface{})
		delta, _ := choice["delta"].(map[string]interface{})
		content, _ := delta["content"].(string)
		fr, _ := choice["finish_reason"].(string)
		if fr != "" {
			// Финальный кадр: клиент возьмёт delta.content и приклеит его.
			doneDeltas = append(doneDeltas, content)
			continue
		}
		streamed.WriteString(content)
	}

	if len(doneDeltas) != 1 {
		t.Fatalf("кадров с finish_reason: %d, want ровно 1", len(doneDeltas))
	}
	if doneDeltas[0] != "" {
		t.Errorf("в финальном кадре delta.content = %q — клиент приклеит полный ответ вторым экземпляром", doneDeltas[0])
	}
	if got := streamed.String(); got != "Hello world" {
		t.Errorf("текст в дельтах = %q, want %q", got, "Hello world")
	}
}
