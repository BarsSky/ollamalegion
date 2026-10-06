// image_tool_loop_ollama_test.go — R86 (2026-10-06): инструмент генерации
// изображений на поверхности OLLAMA /api/chat.
//
// ЖИВОЙ КОНТЕКСТ: Open WebUI по умолчанию подключается как Ollama-сервер и ходит
// на POST /api/chat. До R86 инструменты там не объявлялись вообще, поэтому модель
// их «не видела». Тесты фиксируют: (1) объявление в Ollama-теле с сохранением
// инструментов клиента, (2) уважение tool_choice="none", (3) цикл
// вызов → исполнение → финальный ответ в NDJSON, (4) отдачу стрима.
package balancer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"ollama-loadbalancer/internal/imagetool"
)

// --- 1. Инъекция -------------------------------------------------------------

func TestInjectImageToolOllama_AddsToolsAndKeepsClientTools(t *testing.T) {
	target := &imageToolTarget{Models: []string{"sd15-q4"}, Families: []string{"sd15"}, AllowLoad: true}
	body := []byte(`{"model":"qwen3","messages":[{"role":"user","content":"нарисуй кота"}],"tools":[{"type":"function","function":{"name":"web_search"}}],"stream":true}`)

	out, added := injectImageToolOllama(body, target)
	if len(added) != 2 || added[0] != imagetool.Name || added[1] != imagetool.ListName {
		t.Fatalf("добавленные инструменты = %v", added)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tools, _ := doc["tools"].([]interface{})
	if len(tools) != 3 {
		t.Fatalf("ожидали 3 инструмента (клиентский + наши два), получили %d: %s", len(tools), out)
	}
	if openAIToolName(tools[0]) != "web_search" {
		t.Errorf("инструмент клиента должен остаться первым: %s", out)
	}
	// Остальное тело не меняем.
	if doc["stream"] != true || doc["model"] != "qwen3" {
		t.Errorf("инъекция испортила тело: %s", out)
	}
	msgs, _ := doc["messages"].([]interface{})
	if len(msgs) != 1 {
		t.Errorf("messages изменились: %s", out)
	}
}

func TestInjectImageToolOllama_RespectsClientOwnTool(t *testing.T) {
	target := &imageToolTarget{Models: []string{"sd15-q4"}, AllowLoad: true}
	// Клиент сам объявил generate_image — исполняет его сторона, наш набор не
	// добавляем ЦЕЛИКОМ (смешанный набор размыл бы ответственность за исполнение).
	for _, own := range []string{imagetool.Name, imagetool.ListName} {
		body := []byte(`{"model":"m","messages":[],"tools":[{"type":"function","function":{"name":"` + own + `"}}]}`)
		out, added := injectImageToolOllama(body, target)
		if len(added) != 0 {
			t.Fatalf("нельзя навязывать инструменты, когда клиент объявил %q: %v", own, added)
		}
		if string(out) != string(body) {
			t.Errorf("тело изменилось при клиентском %q: %s", own, out)
		}
	}
}

func TestInjectImageToolOllama_ToolChoiceNoneWins(t *testing.T) {
	target := &imageToolTarget{Models: []string{"sd15-q4"}, AllowLoad: true}
	body := []byte(`{"model":"m","messages":[],"tool_choice":"none"}`)
	out, added := injectImageToolOllama(body, target)
	if len(added) != 0 {
		t.Fatalf(`tool_choice="none" обязан отменять инъекцию: %v`, added)
	}
	if string(out) != string(body) {
		t.Errorf("тело изменилось: %s", out)
	}
}

// При ALLOW_LOAD=off объявляется ровно один инструмент — прежнее поведение.
func TestInjectImageToolOllama_AllowLoadOffSingleTool(t *testing.T) {
	target := &imageToolTarget{Models: []string{"sd15-q4"}, AllowLoad: false}
	out, added := injectImageToolOllama([]byte(`{"model":"m","messages":[]}`), target)
	if len(added) != 1 || added[0] != imagetool.Name {
		t.Fatalf("при ALLOW_LOAD=off ожидали только generate_image: %v (%s)", added, out)
	}
	_ = out
}

// --- 2. Цикл -----------------------------------------------------------------

// fakeOllamaWorker — минимальный cppworker на /api/chat: первый вызов отдаёт
// NDJSON с tool_calls, второй — финальный текст. Тела запросов сохраняются.
type fakeOllamaWorker struct {
	mu       sync.Mutex
	calls    int
	bodies   []map[string]interface{}
	finalTxt string
	toolName string
	args     string
}

func (f *fakeOllamaWorker) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ollamaChatPath {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		var doc map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&doc)
		f.mu.Lock()
		f.calls++
		call := f.calls
		name := f.toolName
		args := f.args
		if name == "" {
			name = "generate_image"
		}
		if args == "" {
			args = `{"prompt":"a cat"}`
		}
		f.bodies = append(f.bodies, doc)
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/x-ndjson")
		if call == 1 {
			_, _ = fmt.Fprintf(w,
				`{"model":"qwen3","created_at":"2026-10-06T12:00:00Z","message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":%q,"arguments":%s}}]},"done":false}`+"\n",
				name, args)
			_, _ = fmt.Fprint(w, `{"model":"qwen3","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop"}`+"\n")
			return
		}
		_, _ = fmt.Fprintf(w,
			`{"model":"qwen3","created_at":"2026-10-06T12:00:01Z","message":{"role":"assistant","content":%q},"done":true,"done_reason":"stop","eval_count":42}`+"\n",
			f.finalTxt)
	}
}

// ollamaTestBody — типичное тело, которое присылает Open WebUI (stream + свои tools).
func ollamaTestBody(stream bool) []byte {
	streamFlag := "false"
	if stream {
		streamFlag = "true"
	}
	return []byte(`{"model":"qwen3","messages":[{"role":"user","content":"нарисуй кота"}],` +
		`"tools":[{"type":"function","function":{"name":"web_search"}},` +
		`{"type":"function","function":{"name":"generate_image"}},` +
		`{"type":"function","function":{"name":"list_image_models"}}],"stream":` + streamFlag + `}`)
}

// newOllamaLoopTest — стенд: прокси с image-бэкендом + фейковый cppworker.
func newOllamaLoopTest(t *testing.T) (*LlamaCppRouter, *imageToolTarget, *fakeOllamaWorker, *httptest.Server) {
	t.Helper()
	t.Setenv("LB_IMAGE_TOOL_ALLOW_LOAD", "on")
	ResetImageCatalogCache()
	t.Cleanup(ResetImageCatalogCache)

	p, stub := newImgResProxy(t, toolTestImageSettings())
	stub.setGenBody(`{"created":1,"model":"sd15-q4","output_format":"png","seed":7,"width":512,"height":512,"steps":8,"duration_ms":1500,"data":[{"url":"/images/img_1_cat.png"}]}`)
	target := p.imageToolTargetFor(context.Background())
	if target == nil {
		t.Fatal("нет target: стенд обязан иметь image-модель")
	}

	fake := &fakeOllamaWorker{finalTxt: "Готово: котик нарисован, смотри картинку."}
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	return NewLlamaCppRouter(p), target, fake, srv
}

func TestRunImageToolLoopOllama_ExecutesAndFollowsUp(t *testing.T) {
	router, target, fake, srv := newOllamaLoopTest(t)

	body := ollamaTestBody(false)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(string(body)))
	req.Host = "balancer.test:18080"

	handled := router.runImageToolLoopOllama(rec, req, imageToolLoopArgsOllama{
		bodyBuf:    body,
		model:      "qwen3",
		backendID:  "llm-1",
		backendURL: srv.URL,
		target:     target,
	})
	if !handled {
		t.Fatal("цикл должен был обработать запрос")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Нестриминг: один JSON-объект Ollama-формы с финальным текстом и done=true.
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("клиент получил не JSON: %s", rec.Body.String())
	}
	msg, _ := out["message"].(map[string]interface{})
	if content, _ := msg["content"].(string); !strings.Contains(content, "котик") {
		t.Errorf("клиент не получил финальный ответ модели: %v", msg["content"])
	}
	if out["done"] != true {
		t.Errorf("в ответе нет done=true: %s", rec.Body.String())
	}
	// Ссылку добавляем сами, если модель её потеряла.
	if !strings.Contains(fmt.Sprint(msg["content"]), "img_1_cat.png") {
		t.Errorf("в ответе нет markdown-картинки: %v", msg["content"])
	}

	// Второй вызов: есть assistant(tool_calls) и tool-результат, наших инструментов нет.
	if fake.calls != 2 {
		t.Fatalf("ожидали два обращения к модели, получили %d", fake.calls)
	}
	second := fake.bodies[1]
	msgs, _ := second["messages"].([]interface{})
	if len(msgs) != 3 {
		t.Fatalf("ожидали 3 сообщения (user + assistant + tool), получили %d: %+v", len(msgs), msgs)
	}
	assistant, _ := msgs[1].(map[string]interface{})
	if assistant["role"] != "assistant" || assistant["tool_calls"] == nil {
		t.Errorf("нет assistant(tool_calls): %+v", assistant)
	}
	toolMsg, _ := msgs[2].(map[string]interface{})
	if toolMsg["role"] != "tool" {
		t.Errorf("tool-сообщение собрано неверно: %+v", toolMsg)
	}
	content, _ := toolMsg["content"].(string)
	if !strings.Contains(content, "/v1/images/files/img_1_cat.png") {
		t.Errorf("в результате инструмента нет публичной ссылки: %s", content)
	}
	if !strings.Contains(content, "balancer.test:18080") {
		t.Errorf("ссылка должна быть абсолютной (Host клиента): %s", content)
	}
	tools, _ := second["tools"].([]interface{})
	if len(tools) != 1 || openAIToolName(tools[0]) != "web_search" {
		t.Errorf("на втором turn должны остаться только инструменты клиента: %+v", tools)
	}
}

// Стрим клиента: turn 1 всё равно нестриминговый, но ответ отдаём NDJSON-потоком
// с обязательным done=true (иначе Open WebUI ждёт завершения потока).
func TestRunImageToolLoopOllama_StreamsNDJSON(t *testing.T) {
	router, target, fake, srv := newOllamaLoopTest(t)

	body := ollamaTestBody(true)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(string(body)))
	req.Host = "balancer.test:18080"

	if !router.runImageToolLoopOllama(rec, req, imageToolLoopArgsOllama{
		bodyBuf:    body,
		model:      "qwen3",
		backendID:  "llm-1",
		backendURL: srv.URL,
		target:     target,
	}) {
		t.Fatal("цикл должен был обработать запрос")
	}
	if fake.calls != 2 {
		t.Fatalf("обращений к модели %d, want 2", fake.calls)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "ndjson") {
		t.Errorf("Content-Type=%q, want application/x-ndjson", ct)
	}

	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("ожидали минимум 2 NDJSON-строки, получили: %q", rec.Body.String())
	}
	sawDone, sawContent := false, false
	for _, line := range lines {
		var doc map[string]interface{}
		if err := json.Unmarshal([]byte(line), &doc); err != nil {
			t.Fatalf("строка не JSON: %q (%v)", line, err)
		}
		if doc["done"] == true {
			sawDone = true
		}
		if msg, ok := doc["message"].(map[string]interface{}); ok {
			if content, _ := msg["content"].(string); strings.Contains(content, "котик") {
				sawContent = true
			}
		}
	}
	if !sawDone {
		t.Errorf("в NDJSON-потоке нет завершающей строки done=true: %s", rec.Body.String())
	}
	if !sawContent {
		t.Errorf("в NDJSON-потоке нет содержимого ответа: %s", rec.Body.String())
	}
}

// Обычный ответ без вызова инструмента обязан уйти клиенту как есть.
func TestRunImageToolLoopOllama_NoToolCallPassesThrough(t *testing.T) {
	router, target, fake, srv := newOllamaLoopTest(t)
	fake.toolName = "web_search" // модель вызвала ЧУЖОЙ инструмент

	body := ollamaTestBody(false)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(string(body)))
	req.Host = "balancer.test:18080"

	if !router.runImageToolLoopOllama(rec, req, imageToolLoopArgsOllama{
		bodyBuf:    body,
		model:      "qwen3",
		backendID:  "llm-1",
		backendURL: srv.URL,
		target:     target,
	}) {
		t.Fatal("цикл должен был обработать запрос")
	}
	if fake.calls != 1 {
		t.Fatalf("чужой вызов не требует второго turn: обращений %d", fake.calls)
	}
	if !strings.Contains(rec.Body.String(), `"done":true`) {
		t.Errorf("ответ должен быть отдан как есть: %s", rec.Body.String())
	}
}

// list_image_models не тратит GPU и не требует второго turn с генерацией.
func TestRunImageToolLoopOllama_ListCallDoesNotGenerate(t *testing.T) {
	router, target, fake, srv := newOllamaLoopTest(t)
	fake.toolName = imagetool.ListName
	fake.args = `{}`

	body := ollamaTestBody(false)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(string(body)))
	req.Host = "balancer.test:18080"

	if !router.runImageToolLoopOllama(rec, req, imageToolLoopArgsOllama{
		bodyBuf:    body,
		model:      "qwen3",
		backendID:  "llm-1",
		backendURL: srv.URL,
		target:     target,
	}) {
		t.Fatal("цикл должен был обработать запрос")
	}
	if fake.calls != 2 {
		t.Fatalf("обращений к модели %d, want 2 (каталог + финальный ответ)", fake.calls)
	}
	second := fake.bodies[1]
	msgs, _ := second["messages"].([]interface{})
	toolMsg, _ := msgs[len(msgs)-1].(map[string]interface{})
	content, _ := toolMsg["content"].(string)
	if !strings.Contains(content, "Доступные image-модели") {
		t.Errorf("в tool-сообщении нет каталога: %s", content)
	}
}

// TestStripEmptyToolCallArtifact — живой артефакт Qwen3: инструменты объявлены,
// модель их не вызвала, и в content попал пустой массив вызовов («[]»). Клиент не
// должен видеть эту строку вместо ответа.
func TestStripEmptyToolCallArtifact(t *testing.T) {
	in := []byte("{\"model\":\"qwen3\",\"message\":{\"role\":\"assistant\",\"content\":\"[]\"},\"done\":true}\n")
	out := stripEmptyToolCallArtifact(in)
	if strings.Contains(string(out), `"[]"`) {
		t.Fatalf("артефакт не убран: %s", out)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &doc); err != nil {
		t.Fatalf("ответ стал не-JSON: %s", out)
	}
	msg, _ := doc["message"].(map[string]interface{})
	if content, _ := msg["content"].(string); content != "" {
		t.Errorf("content=%q, want пусто", content)
	}
	if doc["done"] != true {
		t.Errorf("done потерян: %s", out)
	}

	// Маркер [TOOL_CALLS] + пустой массив — тот же артефакт.
	in2 := []byte("{\"message\":{\"role\":\"assistant\",\"content\":\"[TOOL_CALLS][]\"},\"done\":true}\n")
	out2 := stripEmptyToolCallArtifact(in2)
	if strings.Contains(string(out2), "TOOL_CALLS") {
		t.Fatalf("маркер не убран: %s", out2)
	}
	// Ещё один вариант со стенда: «[TOOL_CALLS]=[]».
	in3 := []byte("{\"message\":{\"role\":\"assistant\",\"content\":\"[TOOL_CALLS]=[]\"},\"done\":true}\n")
	out3 := stripEmptyToolCallArtifact(in3)
	if strings.Contains(string(out3), "TOOL_CALLS") || strings.Contains(string(out3), "[]") {
		t.Fatalf("вариант с '=' не убран: %s", out3)
	}

	// НОРМАЛЬНЫЙ ответ не трогаем (в том числе текст, где есть скобки).
	keep := []byte("{\"message\":{\"role\":\"assistant\",\"content\":\"Вот список: [] — пусто\"},\"done\":true}\n")
	if got := string(stripEmptyToolCallArtifact(keep)); got != string(keep) {
		t.Errorf("обычный ответ изменён: %s", got)
	}
	// Пустой ответ без артефакта тоже остаётся как есть.
	empty := []byte("{\"message\":{\"role\":\"assistant\",\"content\":\"\"},\"done\":true}\n")
	if got := string(stripEmptyToolCallArtifact(empty)); got != string(empty) {
		t.Errorf("пустой ответ изменён: %s", got)
	}
}

// TestBuildOllamaFollowUpBody_ArgumentsAreObjects — ЖИВОЙ ДЕФЕКТ (2026-10-06):
// на Ollama-поверхности второй turn должен получать tool_calls с arguments
// ОБЪЕКТОМ, а не строкой. cppworker (cmd/cppworker/handlers_chat.go:22-31) прямо
// предупреждает: нативные Ollama-клиенты «шлют и ждут arguments как JSON-ОБЪЕКТ»,
// со строкой агентский цикл ломается — именно это отличало /api/chat от
// /v1/chat/completions в живых проверках.
func TestBuildOllamaFollowUpBody_ArgumentsAreObjects(t *testing.T) {
	body := []byte(`{"model":"qwen3","messages":[{"role":"user","content":"нарисуй кота"}],"tools":[{"type":"function","function":{"name":"generate_image"}}],"stream":false}`)
	// Ответ движка приходит в OpenAI-форме: arguments — СТРОКА с JSON.
	assistant := map[string]interface{}{
		"role":    "assistant",
		"content": "",
		"tool_calls": []interface{}{map[string]interface{}{
			"id":   "call_1",
			"type": "function",
			"function": map[string]interface{}{
				"name":      "generate_image",
				"arguments": `{"prompt":"a cat","width":512}`,
			},
		}},
	}
	calls := []imageToolCall{{ID: "call_1", Name: "generate_image", Arguments: `{"prompt":"a cat","width":512}`}}
	results := []map[string]interface{}{{"status": "ok", "url": "http://lb/v1/images/files/x.png"}}

	out, err := buildOllamaFollowUpBody(body, assistant, calls, results)
	if err != nil {
		t.Fatalf("buildOllamaFollowUpBody: %v", err)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs, _ := doc["messages"].([]interface{})
	if len(msgs) != 3 {
		t.Fatalf("ожидали 3 сообщения, получили %d: %s", len(msgs), out)
	}
	asst, _ := msgs[1].(map[string]interface{})
	tcs, _ := asst["tool_calls"].([]interface{})
	if len(tcs) != 1 {
		t.Fatalf("нет tool_calls в истории: %s", out)
	}
	entry, _ := tcs[0].(map[string]interface{})
	fn, _ := entry["function"].(map[string]interface{})
	args := fn["arguments"]
	if _, isString := args.(string); isString {
		t.Fatalf("arguments остались СТРОКОЙ — Ollama-форма требует объект: %v", args)
	}
	obj, isObj := args.(map[string]interface{})
	if !isObj {
		t.Fatalf("arguments = %T, want объект", args)
	}
	if obj["prompt"] != "a cat" {
		t.Errorf("аргументы потерялись: %+v", obj)
	}
	if fn["name"] != "generate_image" {
		t.Errorf("имя функции потеряно: %v", fn["name"])
	}
	if entry["id"] != "call_1" {
		t.Errorf("id вызова потерян: %v", entry["id"])
	}
	// tool-сообщение — Ollama-форма (без tool_call_id, контент — JSON-строка).
	toolMsg, _ := msgs[2].(map[string]interface{})
	if toolMsg["role"] != "tool" {
		t.Errorf("tool-сообщение: %+v", toolMsg)
	}
	if _, hasID := toolMsg["tool_call_id"]; hasID {
		t.Errorf("в Ollama-форме у tool-сообщения нет tool_call_id: %+v", toolMsg)
	}
	if content, _ := toolMsg["content"].(string); !strings.Contains(content, "x.png") {
		t.Errorf("результат инструмента потерян: %q", content)
	}
}

// Диагностическая строка решения: причина отказа формулируется по-человечески.
func TestImageToolSkipReason_AllReasons(t *testing.T) {
	cases := map[string]string{
		imageToolSkipDisabled:     "LB_IMAGE_TOOL=off",
		imageToolSkipNoBackend:    "image_cpp",
		imageToolSkipNoModels:     "нет моделей",
		imageToolSkipAllowLoadOff: "ALLOW_LOAD=off",
		imageToolSkipClientNone:   "tool_choice",
		imageToolSkipClientOwns:   "клиент сам объявил",
	}
	for skip, want := range cases {
		got := imageToolSkipReason(skip)
		if !strings.Contains(got, want) {
			t.Errorf("imageToolSkipReason(%q) = %q, ожидали упоминание %q", skip, got, want)
		}
	}
}

// countClientTools — сколько инструментов прислал клиент (для лога решения).
func TestCountClientTools(t *testing.T) {
	if got := countClientTools(nil); got != 0 {
		t.Errorf("nil body: %d", got)
	}
	if got := countClientTools([]byte(`не json`)); got != 0 {
		t.Errorf("битый JSON: %d", got)
	}
	if got := countClientTools(ollamaTestBody(false)); got != 3 {
		t.Errorf("три инструмента клиента: %d", got)
	}
}

// NDJSON-хелперы: разбор ответа и защита от «нет done».
func TestOllamaChatMessage(t *testing.T) {
	raw := []byte("{\"model\":\"m\",\"message\":{\"role\":\"assistant\",\"content\":\"\"},\"done\":false}\n" +
		"{\"message\":{\"role\":\"assistant\",\"content\":\"ok\"},\"done\":true}\n")
	msg, done := ollamaChatMessage(raw)
	if msg == nil || done {
		t.Fatalf("ожидали первое сообщение и done=false: %v %v", msg, done)
	}
	if _, done := ollamaChatMessage([]byte(`{"error":"boom"}`)); done {
		t.Error("ответ без message не должен считаться завершённым")
	}
	if _, done := ollamaChatMessage(nil); done {
		t.Error("пустой ответ не должен считаться завершённым")
	}
	_ = io.Discard
}
