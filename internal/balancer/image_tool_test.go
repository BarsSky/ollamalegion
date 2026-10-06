// image_tool_test.go — R84 (2026-10-03): инструмент generate_image.
//
// ЧТО ФИКСИРУЕМ (и почему именно это):
//  1. ГЕЙТ «есть кому исполнять»: инструмент объявляется только когда image_cpp
//     здоров, снимок достоверен и модель ЗАГРУЖЕНА. Иначе модель получила бы
//     инструмент, а вызов упирался бы в «no image model is loaded».
//  2. НЕ НАВЯЗЫВАЕМСЯ: tool_choice=none и собственный generate_image клиента
//     инструмент не подменяют, тело запроса остаётся прежним.
//  3. ВЫЗОВ РАЗБИРАЕТСЯ и в tool_calls, и в content (у части моделей вызова
//     приходит JSON-ом в текст).
//  4. TOOL-LOOP: turn 1 видит вызов → генерация → turn 2 получает диалог с
//     assistant(tool_calls) + tool(результат) и БЕЗ нашего инструмента.
//  5. ОШИБКА ГЕНЕРАЦИИ не ломает ответ: она уезжает в tool-сообщение, чтобы
//     модель объяснила её пользователю.
//  6. ПУТЬ БЕЗ IMAGE-БЭКЕНДА не меняется: гейт отдаёт nil, инструмент не
//     добавляется.
package balancer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"ollama-loadbalancer/internal/imagetool"
	"ollama-loadbalancer/pkg/types"
)

// --- 1. Схема и инъекция ----------------------------------------------------

// singleImageTool — один инструмент generate_image в форме, которую принимает
// injectImageTool (R85: функция принимает НАБОР инструментов — generate_image
// и, при автозагрузке, list_image_models).
func singleImageTool() []map[string]interface{} {
	return []map[string]interface{}{imagetool.Build(imagetool.Defaults()).OpenAIFunction()}
}

func TestInjectImageTool_AddsTool(t *testing.T) {
	body := []byte(`{"model":"gemma-4","messages":[{"role":"user","content":"нарисуй кота"}],"stream":true}`)
	out, ok, err := injectImageTool(body, singleImageTool())
	if err != nil || !ok {
		t.Fatalf("инъекция не сработала: ok=%v err=%v", ok, err)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tools, _ := doc["tools"].([]interface{})
	if len(tools) != 1 || openAIToolName(tools[0]) != imagetool.Name {
		t.Fatalf("инструмент не добавлен: %s", out)
	}
	// Остальные поля запроса обязаны уцелеть: мы правим только tools.
	if doc["model"] != "gemma-4" || doc["stream"] != true {
		t.Errorf("инъекция испортила запрос: %s", out)
	}
	msgs, _ := doc["messages"].([]interface{})
	if len(msgs) != 1 {
		t.Errorf("messages изменились: %s", out)
	}
}

func TestInjectImageTool_KeepsExistingTools(t *testing.T) {
	body := []byte(`{"model":"m","messages":[],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]}`)
	out, ok, err := injectImageTool(body, singleImageTool())
	if err != nil || !ok {
		t.Fatalf("инъекция не сработала: ok=%v err=%v", ok, err)
	}
	var doc map[string]interface{}
	_ = json.Unmarshal(out, &doc)
	tools, _ := doc["tools"].([]interface{})
	if len(tools) != 2 {
		t.Fatalf("ожидали 2 инструмента (клиентский + наш), получили %d: %s", len(tools), out)
	}
	if openAIToolName(tools[0]) != "get_weather" {
		t.Errorf("клиентский инструмент должен остаться первым: %s", out)
	}
}

func TestInjectImageTool_SkipsWhenClientOwnsTool(t *testing.T) {
	body := []byte(`{"model":"m","messages":[],"tools":[{"type":"function","function":{"name":"generate_image","parameters":{"type":"object"}}}]}`)
	out, ok, err := injectImageTool(body, singleImageTool())
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if ok {
		t.Fatal("нельзя подменять инструмент, объявленный клиентом: он сам его исполняет")
	}
	if string(out) != string(body) {
		t.Errorf("тело изменилось: %s", out)
	}
}

func TestInjectImageTool_SkipsWhenToolChoiceNone(t *testing.T) {
	body := []byte(`{"model":"m","messages":[],"tool_choice":"none"}`)
	out, ok, _ := injectImageTool(body, singleImageTool())
	if ok || string(out) != string(body) {
		t.Fatalf("при tool_choice=none инструмент навязывать нельзя: ok=%v %s", ok, out)
	}
}

// --- 2. Разбор вызова -------------------------------------------------------

func TestExtractImageToolCalls_FromToolCalls(t *testing.T) {
	message := map[string]interface{}{
		"role":    "assistant",
		"content": "",
		"tool_calls": []interface{}{
			map[string]interface{}{
				"id":   "call_1",
				"type": "function",
				"function": map[string]interface{}{
					"name":      "generate_image",
					"arguments": `{"prompt":"a cat"}`,
				},
			},
			map[string]interface{}{
				"id":       "call_2",
				"type":     "function",
				"function": map[string]interface{}{"name": "other_tool", "arguments": "{}"},
			},
		},
	}
	calls := extractImageToolCalls(message)
	if len(calls) != 1 || calls[0].ID != "call_1" {
		t.Fatalf("чужой инструмент не должен попадать в исполнение: %+v", calls)
	}
	if calls[0].Arguments != `{"prompt":"a cat"}` {
		t.Errorf("arguments=%q", calls[0].Arguments)
	}
}

func TestExtractImageToolCalls_FromContent(t *testing.T) {
	message := map[string]interface{}{
		"role":    "assistant",
		"content": `<tool_call>{"name": "generate_image", "arguments": {"prompt": "a dog"}}</tool_call>`,
	}
	calls := extractImageToolCalls(message)
	if len(calls) != 1 {
		t.Fatalf("вызов в content не распознан: %+v", calls)
	}
	if !strings.Contains(calls[0].Arguments, "a dog") {
		t.Errorf("arguments=%q", calls[0].Arguments)
	}
}

func TestParseImageToolArgs(t *testing.T) {
	args, err := parseImageToolArgs(`{"prompt":"cat","negative_prompt":"blurry","width":512,"height":"768","steps":8,"cfg":1.5,"seed":42,"model":"sd15-q4"}`)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if args.Prompt != "cat" || args.NegativePrompt != "blurry" || args.Width != 512 || args.Height != 768 ||
		args.Steps != 8 || args.CFG != 1.5 || args.Model != "sd15-q4" {
		t.Fatalf("разобрано неверно: %+v", args)
	}
	if args.Seed == nil || *args.Seed != 42 {
		t.Fatalf("seed=%v", args.Seed)
	}
	// camelCase тоже принимаем: часть моделей копирует схему по-своему.
	if a, err := parseImageToolArgs(`{"prompt":"x","negativePrompt":"y"}`); err != nil || a.NegativePrompt != "y" {
		t.Fatalf("camelCase не поддержан: %+v err=%v", a, err)
	}
	if _, err := parseImageToolArgs(`{}`); err == nil {
		t.Fatal("без prompt вызов исполнять нельзя")
	}
	if _, err := parseImageToolArgs(`не json`); err == nil {
		t.Fatal("не-JSON аргументы должны давать ошибку")
	}
}

// TestParseImageToolArgs_EmptyAndNestedForms — ЖИВОЙ ДЕФЕКТ R85: Qwen3 вызывает
// инструмент без параметров и присылает arguments пустой строкой. Такая форма
// (а также "null" и JSON-строка с JSON внутри) обязана разбираться: инструмент
// list_image_models специально сделан без обязательных параметров.
func TestParseImageToolArgs_EmptyAndNestedForms(t *testing.T) {
	cases := []string{"", "   ", "null", "{}", `"{}"`, `"{\"family\":\"flux\"}"`, `{"family":"flux"}`}
	for _, raw := range cases {
		args, err := parseImageToolArgsFor(raw, true)
		if err != nil {
			t.Fatalf("list-инструмент с аргументами %q должен разбираться, got %v", raw, err)
		}
		if !args.List {
			t.Errorf("args.List=false для %q", raw)
		}
		if strings.Contains(raw, "flux") && args.Family != "flux" {
			t.Errorf("family из %q не разобран: %q", raw, args.Family)
		}
	}
	// У генерации пустые аргументы по-прежнему означают «нет prompt».
	if _, err := parseImageToolArgsFor("", false); err == nil {
		t.Error("generate_image без prompt обязан отклоняться")
	}
	// Каноничная форма OpenAI (JSON-строка внутри строки) работает и для генерации.
	args, err := parseImageToolArgsFor(`"{\"prompt\":\"a cat\",\"model\":\"sd15-q8-0\"}"`, false)
	if err != nil {
		t.Fatalf("вложенная JSON-строка не разобрана: %v", err)
	}
	if args.Prompt != "a cat" || args.Model != "sd15-q8-0" {
		t.Fatalf("поля из вложенной формы = %+v", args)
	}
}

// --- 3. Гейт доступности ----------------------------------------------------

func TestImageToolTargetFor_RequiresLoadedModel(t *testing.T) {
	ResetImageCatalogCache()
	t.Cleanup(ResetImageCatalogCache)

	// ALLOW_LOAD=on (по умолчанию): достаточно, что модель ЕСТЬ в снимке — её
	// поднимет сам вызов.
	t.Setenv("LB_IMAGE_TOOL_ALLOW_LOAD", "on")
	p, _ := newImgResProxy(t, toolTestImageSettings())
	target := p.imageToolTargetFor(context.Background())
	if target == nil {
		t.Fatal("с загруженной моделью инструмент должен объявляться")
	}
	if target.Model != "flux-test" || target.BackendID != "img-1" {
		t.Fatalf("target=%+v", target)
	}
	if len(target.Models) == 0 {
		t.Error("enum моделей пуст: модель не сможет выбрать модель")
	}
	if !target.AllowLoad {
		t.Error("ALLOW_LOAD=on должен разрешать загрузку")
	}

	// Модель есть на диске, но не загружена → при ALLOW_LOAD=on инструмент ВСЁ
	// РАВНО объявляем (в этом и смысл R85). Стенд отдельный: снимок кэшируется
	// (staleAfter), и подмена ответа ПОСЛЕ первого опроса ничего бы не доказала.
	ResetImageCatalogCache()
	p2, stub2 := newImgResProxy(t, toolTestImageSettings())
	stub2.setModels(`{"models":[{"name":"flux-test","state":"not_loaded"}],"state":"not_loaded","current_model":""}`)
	got := p2.imageToolTargetFor(context.Background())
	if got == nil {
		t.Fatal("ALLOW_LOAD=on: модель на диске должна объявляться — вызов поднимет её сам")
	}
	if got.Model != "flux-test" {
		t.Fatalf("target=%+v", got)
	}

	// ALLOW_LOAD=off → прежнее правило: без загруженной модели не объявляем.
	ResetImageCatalogCache()
	t.Setenv("LB_IMAGE_TOOL_ALLOW_LOAD", "off")
	p3, stub3 := newImgResProxy(t, toolTestImageSettings())
	stub3.setModels(`{"models":[{"name":"flux-test","state":"not_loaded"}],"state":"not_loaded","current_model":""}`)
	if bad := p3.imageToolTargetFor(context.Background()); bad != nil {
		t.Fatalf("ALLOW_LOAD=off: без загруженной модели инструмент объявлять нельзя: %+v", bad)
	}
}

func TestImageToolTargetFor_DisabledByEnv(t *testing.T) {
	t.Setenv("LB_IMAGE_TOOL", "off")
	p, _ := newImgResProxy(t, toolTestImageSettings())
	if got := p.imageToolTargetFor(context.Background()); got != nil {
		t.Fatalf("LB_IMAGE_TOOL=off должен отключать инструмент: %+v", got)
	}
}

func TestImageToolTargetFor_NoImageBackend(t *testing.T) {
	// Стенд без image-бэкендов: инструмент объявлять некому.
	p, _ := newLlamaOnlyProxy(t)
	if got := p.imageToolTargetFor(context.Background()); got != nil {
		t.Fatalf("без image_cpp инструмент объявлять нельзя: %+v", got)
	}
}

// --- 4. Исполнение вызова ---------------------------------------------------

func TestGenerateImageForTool_UsesURLModeAndMetrics(t *testing.T) {
	p, stub := newImgResProxy(t, toolTestImageSettings())
	stub.setGenBody(`{"created":1,"model":"flux-test","output_format":"png","seed":7,"width":512,"height":512,"steps":8,"duration_ms":1500,"data":[{"index":0,"url":"/images/img_1_ab.png"}]}`)
	target := p.imageToolTargetFor(context.Background())
	if target == nil {
		t.Fatal("нет target")
	}
	res, err := p.generateImageForTool(context.Background(), target, imageToolArgs{Prompt: "a cat", Width: 512, Height: 512})
	if err != nil {
		t.Fatalf("генерация: %v", err)
	}
	if res["status"] != "ok" || res["url"] != "/images/img_1_ab.png" {
		t.Fatalf("результат=%+v", res)
	}
	if strings.Contains(fmt.Sprint(res["markdown"]), "http") {
		t.Error("markdown должен собираться ПОЗЖЕ, из публичного URL (см. executeOneImageToolCall)")
	}
	// Генерация обязана быть видна в ленте image-запросов (Monitor).
	recent := p.imageResources().imageRequests().recentGlobal(5)
	found := false
	for _, r := range recent {
		if r.Surface == imageToolSurface {
			found = true
		}
	}
	if !found {
		t.Errorf("тул-генерации нет в ленте image-запросов: %+v", recent)
	}
}

func TestGenerateImageForTool_EngineError(t *testing.T) {
	p, stub := newImgResProxy(t, toolTestImageSettings())
	stub.setGenStatus(http.StatusInternalServerError)
	target := p.imageToolTargetFor(context.Background())
	// Промпт осмысленный: вырожденные отсекаются раньше (см.
	// TestValidateImageToolPrompt), и этот тест проверяет именно ошибку движка.
	if _, err := p.generateImageForTool(context.Background(), target, imageToolArgs{Prompt: "a cat"}); err == nil {
		t.Fatal("ошибка движка должна возвращаться как ошибка")
	}
}

// TestValidateImageToolPrompt — живой случай 2026-10-06: модель вызвала
// generate_image с prompt_len=6, и пользователь получил красный квадрат (SD на
// бессмысленном промпте заливает картинку одним цветом). Такие вызовы отсекаем с
// понятным текстом, чтобы модель исправилась, а не «успешно» нарисовала мусор.
func TestValidateImageToolPrompt(t *testing.T) {
	bad := []string{"", "   ", "\n\t", "x", "а", "..", "аааа", "........"}
	for _, p := range bad {
		if err := validateImageToolPrompt(p); err == nil {
			t.Errorf("вырожденный промпт %q должен отклоняться", p)
		}
	}
	good := []string{"a cat", "лес", "кот на подоконнике", "a red cat on a windowsill, cartoon style"}
	for _, p := range good {
		if err := validateImageToolPrompt(p); err != nil {
			t.Errorf("осмысленный промпт %q отклонён: %v", p, err)
		}
	}
}

// --- 5. Tool-loop -----------------------------------------------------------

// fakeChatWorker — минимальный cppworker: первый вызов отдаёт tool_call по
// generate_image, второй — финальный текст. Пишет полученные тела в bodies.
type fakeChatWorker struct {
	mu       sync.Mutex
	calls    int
	bodies   []map[string]interface{}
	finalTxt string
}

func (f *fakeChatWorker) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var doc map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&doc)
		f.mu.Lock()
		f.calls++
		call := f.calls
		f.bodies = append(f.bodies, doc)
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"generate_image","arguments":"{\"prompt\":\"a cat\"}"}}]},"finish_reason":"tool_calls"}]}`))
			return
		}
		_, _ = fmt.Fprintf(w, `{"id":"c2","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}]}`, f.finalTxt)
	}
}

func TestRunImageToolLoop_ExecutesAndFollowsUp(t *testing.T) {
	p, stub := newImgResProxy(t, toolTestImageSettings())
	stub.setGenBody(`{"created":1,"model":"flux-test","seed":7,"width":512,"height":512,"steps":8,"duration_ms":1200,"data":[{"url":"/images/img_9_ff.png"}]}`)
	target := p.imageToolTargetFor(context.Background())
	if target == nil {
		t.Fatal("нет target: стенд должен иметь загруженную модель")
	}

	fake := &fakeChatWorker{finalTxt: "Готово: котик нарисован."}
	chatSrv := httptest.NewServer(fake.handler())
	defer chatSrv.Close()

	router := NewLlamaCppRouter(p)
	body := []byte(`{"model":"gemma-4","messages":[{"role":"user","content":"нарисуй кота"}],"tools":[{"type":"function","function":{"name":"generate_image","parameters":{"type":"object"}}}],"stream":false}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	req.Host = "balancer.test:18079"

	handled := router.runImageToolLoop(rec, req, imageToolLoopArgs{
		bodyBuf:   body,
		model:     "gemma-4",
		backendID: "llm-1",
		targetURL: chatSrv.URL + "/v1/chat/completions",
		target:    target,
	})
	if !handled {
		t.Fatal("цикл должен был обработать запрос")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("клиент получил не JSON: %s", rec.Body.String())
	}
	choices, _ := out["choices"].([]interface{})
	if len(choices) != 1 {
		t.Fatalf("нет choices: %s", rec.Body.String())
	}
	msg, _ := choices[0].(map[string]interface{})["message"].(map[string]interface{})
	if content, _ := msg["content"].(string); !strings.Contains(content, "котик") {
		t.Errorf("клиент не получил финальный ответ модели: %v", msg["content"])
	}
	// Второй вызов к модели обязан содержать результат инструмента и НЕ содержать
	// наш инструмент (иначе модель зациклится на генерации).
	if fake.calls != 2 {
		t.Fatalf("ожидали два обращения к модели, получили %d", fake.calls)
	}
	second := fake.bodies[1]
	if tools, ok := second["tools"].([]interface{}); ok && len(tools) > 0 {
		t.Errorf("на втором turn наш инструмент должен быть убран: %+v", tools)
	}
	msgs, _ := second["messages"].([]interface{})
	if len(msgs) != 3 {
		t.Fatalf("ожидали 3 сообщения (user + assistant + tool), получили %d: %+v", len(msgs), msgs)
	}
	assistant, _ := msgs[1].(map[string]interface{})
	if assistant["role"] != "assistant" || assistant["tool_calls"] == nil {
		t.Errorf("нет assistant(tool_calls): %+v", assistant)
	}
	toolMsg, _ := msgs[2].(map[string]interface{})
	content, _ := toolMsg["content"].(string)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "call_1" {
		t.Errorf("tool-сообщение собрано неверно: %+v", toolMsg)
	}
	if !strings.Contains(content, "/v1/images/files/img_9_ff.png") {
		t.Errorf("в результате инструмента нет публичной ссылки: %s", content)
	}
	if !strings.Contains(content, "balancer.test:18079") {
		t.Errorf("ссылка должна быть абсолютной (Host клиента): %s", content)
	}
	if !strings.Contains(content, "markdown") {
		t.Errorf("в результате нет markdown для вставки картинки: %s", content)
	}
}

func TestRunImageToolLoop_NoToolCallPassesThrough(t *testing.T) {
	p, _ := newImgResProxy(t, toolTestImageSettings())
	target := p.imageToolTargetFor(context.Background())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"просто текст"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	router := NewLlamaCppRouter(p)
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"привет"}],"stream":false}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))

	if !router.runImageToolLoop(rec, req, imageToolLoopArgs{bodyBuf: body, model: "m", backendID: "llm-1", targetURL: srv.URL, target: target}) {
		t.Fatal("цикл должен обработать запрос")
	}
	if !strings.Contains(rec.Body.String(), "просто текст") {
		t.Fatalf("ответ модели должен уйти клиенту как есть: %s", rec.Body.String())
	}
}

func TestRunImageToolLoop_GenerationErrorGoesToToolMessage(t *testing.T) {
	p, stub := newImgResProxy(t, toolTestImageSettings())
	stub.setGenStatus(http.StatusInternalServerError)
	target := p.imageToolTargetFor(context.Background())

	fake := &fakeChatWorker{finalTxt: "не получилось"}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	router := NewLlamaCppRouter(p)
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"нарисуй"}],"stream":false}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	if !router.runImageToolLoop(rec, req, imageToolLoopArgs{bodyBuf: body, model: "m", backendID: "llm-1", targetURL: srv.URL, target: target}) {
		t.Fatal("цикл должен обработать запрос")
	}
	if fake.calls != 2 {
		t.Fatalf("даже при ошибке генерации модель должна получить tool-результат (вызовов: %d)", fake.calls)
	}
	msgs, _ := fake.bodies[1]["messages"].([]interface{})
	toolMsg, _ := msgs[len(msgs)-1].(map[string]interface{})
	content, _ := toolMsg["content"].(string)
	if !strings.Contains(content, `"status":"error"`) {
		t.Fatalf("в tool-сообщении должна быть ошибка: %s", content)
	}
}

// --- 6. Синтез SSE и отдача файлов -----------------------------------------

func TestSynthesizeChatSSE(t *testing.T) {
	raw := []byte(`{"id":"c1","model":"m","created":1700000000,"choices":[{"index":0,"message":{"role":"assistant","content":"текст ответа"},"finish_reason":"stop"}]}`)
	rec := httptest.NewRecorder()
	synthesizeChatSSE(rec, raw, "m", http.StatusOK)
	body := rec.Body.String()
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type=%q", ct)
	}
	for _, want := range []string{`"role":"assistant"`, "текст ответа", `"finish_reason":"stop"`, "data: [DONE]"} {
		if !strings.Contains(body, want) {
			t.Errorf("в SSE нет %q:\n%s", want, body)
		}
	}
}

func TestImageFileNameFromPath(t *testing.T) {
	if name, ok := imageFileNameFromPath("/v1/images/files/img_1_ab.png"); !ok || name != "img_1_ab.png" {
		t.Fatalf("нормальное имя не распознано: %q ok=%v", name, ok)
	}
	for _, bad := range []string{
		"/v1/images/files/../../etc/passwd",
		"/v1/images/files/a/b.png",
		"/v1/images/files/",
		"/v1/images/generations",
		"/v1/images/files/img.png?x=1",
	} {
		if _, ok := imageFileNameFromPath(bad); ok {
			t.Errorf("опасный/чужой путь принят: %q", bad)
		}
	}
}

func TestImageToolPublicURL(t *testing.T) {
	t.Setenv("LB_IMAGE_TOOL_BASE_URL", "")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Host = "lb.example:18079"
	if got := imageToolPublicURL(req, "/images/img_1.png"); got != "http://lb.example:18079/v1/images/files/img_1.png" {
		t.Errorf("got %q", got)
	}
	// Оператор за TLS-прокси задаёт внешний адрес явно.
	t.Setenv("LB_IMAGE_TOOL_BASE_URL", "https://ai.example.com/")
	if got := imageToolPublicURL(req, "/images/img_1.png"); got != "https://ai.example.com/v1/images/files/img_1.png" {
		t.Errorf("base URL из окружения не учтён: %q", got)
	}

	// ЖИВОЙ СЛУЧАЙ: воркер отдаёт АБСОЛЮТНУЮ ссылку (у него задан BASE_URL).
	// Первая версия отрезала только префикс «/images/» и склеивала мусор:
	// http://балансер/v1/images/files/http://localhost:18093/images/x.png
	t.Setenv("LB_IMAGE_TOOL_BASE_URL", "")
	if got := imageToolPublicURL(req, "http://localhost:18093/images/img_7_ab.png"); got != "http://lb.example:18079/v1/images/files/img_7_ab.png" {
		t.Errorf("абсолютная ссылка воркера разобрана неверно: %q", got)
	}
	if name := imageFileNameFromURL("http://worker:18093/images/img_7.png?x=1#f"); name != "img_7.png" {
		t.Errorf("query/fragment не отрезаны: %q", name)
	}
	for _, bad := range []string{"", "/images/", "http://w/images/../../etc/passwd", "http://w/images/a/b.png"} {
		if name := imageFileNameFromURL(bad); name != "" {
			t.Errorf("опасная ссылка %q дала имя %q", bad, name)
		}
	}
	// Неразобранную ссылку возвращаем как есть (лучше рабочая ссылка воркера,
	// чем склеенный мусор).
	if got := imageToolPublicURL(req, "ftp://weird"); got != "ftp://weird" {
		t.Errorf("неразобранная ссылка изменена: %q", got)
	}
}

func TestBuildFollowUpBody_KeepsClientTools(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"generate_image","parameters":{"type":"object"}}},{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}],"tool_choice":"auto"}`)
	assistant := map[string]interface{}{"role": "assistant", "content": ""}
	out, err := buildFollowUpBody(body, assistant, []imageToolCall{{ID: "call_1", Arguments: `{"prompt":"x"}`}}, []map[string]interface{}{{"status": "ok", "url": "u"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	var doc map[string]interface{}
	_ = json.Unmarshal(out, &doc)
	tools, _ := doc["tools"].([]interface{})
	if len(tools) != 1 || openAIToolName(tools[0]) != "get_weather" {
		t.Fatalf("должен остаться только клиентский инструмент: %s", out)
	}
	if doc["tool_choice"] != "auto" {
		t.Errorf("tool_choice клиента потерян: %s", out)
	}
}

func TestSetStreamFlag(t *testing.T) {
	out, err := setStreamFlag([]byte(`{"model":"m","stream":true}`), false)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	var doc map[string]interface{}
	_ = json.Unmarshal(out, &doc)
	if doc["stream"] != false {
		t.Fatalf("stream не сброшен: %s", out)
	}
}

// --- хелперы стенда ---------------------------------------------------------

// toolTestImageSettings — гейт выключен: тесты инструмента проверяют не VRAM,
// а объявление/исполнение (VRAM-гейт покрыт своими тестами image_resources).
func toolTestImageSettings() types.ImageResourceSettings {
	return types.ImageResourceSettings{GateDisabled: true}
}

// newLlamaOnlyProxy — кластер без image-бэкендов: инструмент объявлять некому.
func newLlamaOnlyProxy(t *testing.T) (*Proxy, *imgResStub) {
	t.Helper()
	stub := newImgResStub(t)
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:       "127.0.0.1",
			Port:       18080,
			APIPort:    18081,
			OpenAIPort: 18079,
			StatePath:  filepath.Join(t.TempDir(), "state.json"),
		},
		Backends: []types.Backend{{
			ID:                "llm-1",
			Name:              "llama.cpp worker",
			Host:              "127.0.0.1",
			CppWorkerPort:     18092,
			Type:              types.BackendTypeLlamaCpp,
			Status:            types.StatusHealthy,
			MaxConcurrentReqs: 4,
		}},
		Balancing: types.BalancingSettings{
			Algorithm:      types.AlgorithmResourceAware,
			RequestTimeout: 10,
			QueueTimeout:   5,
			QueueMaxSize:   50,
			QueueWorkers:   2,
		},
	}
	return newProxyWithCleanup(t, cfg), stub
}

// TestEnsureImageMarkdown — ссылка на картинку попадает в ответ, даже если модель
// её не вставила (живой случай: gemma-4 ответила текстом без markdown).
func TestEnsureImageMarkdown(t *testing.T) {
	results := []map[string]interface{}{{
		"status":   "ok",
		"url":      "http://lb:18079/v1/images/files/img_1.png",
		"markdown": "![кот](http://lb:18079/v1/images/files/img_1.png)",
	}}

	raw := []byte(`{"id":"c","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"Вот рыжий кот."},"finish_reason":"stop"}]}`)
	out := ensureImageMarkdown(raw, results)
	var doc map[string]interface{}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	choices, _ := doc["choices"].([]interface{})
	msg, _ := choices[0].(map[string]interface{})["message"].(map[string]interface{})
	content, _ := msg["content"].(string)
	if !strings.Contains(content, "Вот рыжий кот.") || !strings.Contains(content, "![кот](http://lb:18079/v1/images/files/img_1.png)") {
		t.Fatalf("markdown не добавлен к тексту: %q", content)
	}

	// Если модель УЖЕ вставила ссылку — второй раз не дублируем.
	already := []byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"Смотри: ![кот](http://lb:18079/v1/images/files/img_1.png)"},"finish_reason":"stop"}]}`)
	out2 := ensureImageMarkdown(already, results)
	if strings.Count(string(out2), "img_1.png") != 1 {
		t.Fatalf("ссылка задвоена: %s", out2)
	}

	// Ошибка генерации (нет url/markdown) — ответ не трогаем.
	errOnly := []byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"Не вышло"},"finish_reason":"stop"}]}`)
	out3 := ensureImageMarkdown(errOnly, []map[string]interface{}{{"status": "error", "error": "boom"}})
	if string(out3) != string(errOnly) {
		t.Fatalf("ответ изменён без картинки: %s", out3)
	}
}
