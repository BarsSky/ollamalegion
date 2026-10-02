// one_path_tools_r83_test.go — R83/v67 (2026-10-02).
//
// ПРИНЦИП: запрос с tools[] и без них идёт ОДНИМ путём. Никаких «аллергий на
// Cline», никаких отдельных веток по клиенту или по наличию инструментов.
//
// Почему это отдельный тест. История стенда — набор дефектов, где tools-запрос
// вёл себя иначе, чем обычный:
//   - tools-путь буферизовал ответ → «Model returned empty response» в Cline;
//   - tools-запросы отключали reload по умолчанию (tryRamFallbackReloadAllowTools);
//   - при включённом reasoning tools-запрос мог терять thinking-инструкцию
//     (tools-описание переписывало system-промпт).
//
// Инварианты ниже: tools добавляют ОПИСАНИЕ инструментов и ничего больше —
// параметры генерации не меняются, а инструкции (reasoning/JSON) выживают.
package main

import (
	"encoding/json"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

// toolsBodyR83 — минимальный OpenAI-совместимый tools-запрос.
const toolsBodyR83 = `{"model":"gemma-4-E4B-it-Q4_K_M",` +
	`"messages":[{"role":"user","content":"прочитай файл main.go"}],` +
	`"tools":[{"type":"function","function":{"name":"read_file","description":"Read file",` +
	`"parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}}],` +
	`"tool_choice":"auto","temperature":0.2,"max_tokens":300}`

// plainBodyR83 — то же тело без tools/tool_choice.
const plainBodyR83 = `{"model":"gemma-4-E4B-it-Q4_K_M",` +
	`"messages":[{"role":"user","content":"прочитай файл main.go"}],` +
	`"temperature":0.2,"max_tokens":300}`

func decodeChatR83(t *testing.T, body string) chatRequest {
	t.Helper()
	var req chatRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return req
}

// TestOnePath_ToolsDoNotChangeGenerationParams — параметры генерации совпадают:
// наличие tools не влияет ни на длину ответа, ни на сэмплинг.
func TestOnePath_ToolsDoNotChangeGenerationParams(t *testing.T) {
	genWith := buildGenerateRequestFromChat(decodeChatR83(t, toolsBodyR83), "PROMPT")
	genWithout := buildGenerateRequestFromChat(decodeChatR83(t, plainBodyR83), "PROMPT")

	paramsWith := buildGenerationParams(genWith)
	paramsWithout := buildGenerationParams(genWithout)

	if paramsWith.NPredict != paramsWithout.NPredict {
		t.Errorf("NPredict: tools=%d, без tools=%d — наличие tools не должно влиять на длину ответа",
			paramsWith.NPredict, paramsWithout.NPredict)
	}
	if paramsWith.Temperature != paramsWithout.Temperature {
		t.Errorf("Temperature: tools=%v, без tools=%v — наличие tools не должно менять сэмплинг",
			paramsWith.Temperature, paramsWithout.Temperature)
	}
	if paramsWith.TopP != paramsWithout.TopP {
		t.Errorf("TopP: tools=%v, без tools=%v", paramsWith.TopP, paramsWithout.TopP)
	}
}

// TestOnePath_ToolsDoNotChangePromptPolicy — всё, что влияет на сборку промпта
// (окно, длина, keep_alive, режим think), одинаково с tools и без них.
func TestOnePath_ToolsDoNotChangePromptPolicy(t *testing.T) {
	genWith := buildGenerateRequestFromChat(decodeChatR83(t, toolsBodyR83), "PROMPT")
	genWithout := buildGenerateRequestFromChat(decodeChatR83(t, plainBodyR83), "PROMPT")

	if genWith.MaxTokens != genWithout.MaxTokens {
		t.Errorf("MaxTokens: tools=%d, без tools=%d", genWith.MaxTokens, genWithout.MaxTokens)
	}
	if genWith.NumCtx != genWithout.NumCtx {
		t.Errorf("NumCtx: tools=%d, без tools=%d", genWith.NumCtx, genWithout.NumCtx)
	}
	if genWith._upperBoundNumPredict != genWithout._upperBoundNumPredict {
		t.Errorf("верхняя граница ответа: tools=%d, без tools=%d",
			genWith._upperBoundNumPredict, genWithout._upperBoundNumPredict)
	}
	if genWith.KeepAlive != genWithout.KeepAlive {
		t.Errorf("KeepAlive: tools=%q, без tools=%q", genWith.KeepAlive, genWithout.KeepAlive)
	}
}

// TestOnePath_ToolsAugmentKeepsThinkingInstruction — описание инструментов
// ДОПОЛНЯЕТ system-промпт, а не заменяет его: thinking-инструкция (и любая
// другая политика промпта) обязана выжить после augmentSystemWithTools.
func TestOnePath_ToolsAugmentKeepsThinkingInstruction(t *testing.T) {
	msgs := []chatMessage{{Role: "user", Content: "прочитай файл main.go"}}
	system := injectThinkingInstructionWithLang("", LangEN)
	msgs = upsertSystemMessage(msgs, system)
	if !containsThinkingInstruction(msgs[0].Content) {
		t.Fatal("тест сломан: thinking-инструкция не записалась в system")
	}

	withTools := augmentSystemWithTools(msgs, decodeChatR83(t, toolsBodyR83).Tools)
	if !containsThinkingInstruction(withTools[0].Content) {
		t.Fatalf("augmentSystemWithTools потерял thinking-инструкцию.\n"+
			"system после augment: %q\n"+
			"Это и есть «tools-запрос пошёл другим путём»: модель получает описание "+
			"инструментов, но теряет политику размышлений.", withTools[0].Content)
	}
}

// TestOnePath_ToolsDoNotDisablePerModelReasoning — сквозная проверка на живом
// состоянии: при включённой галочке WebUI промпт с tools получает ровно ту же
// thinking-политику, что и без tools.
func TestOnePath_ToolsDoNotDisablePerModelReasoning(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackend()

	const model = "gemma-4-E4B-it-Q4_K_M"
	if err := backend.InjectLoadedModelForTest(model); err != nil {
		t.Fatalf("InjectLoadedModelForTest: %v", err)
	}
	if err := backend.SetModelReasoningEnabledForTest(model, true); err != nil {
		t.Fatalf("SetModelReasoningEnabledForTest: %v", err)
	}

	prevCfg := currentConfig
	defer func() { currentConfig = prevCfg }()
	cfg := cppbackend.Config{}
	if prevCfg != nil {
		cfg = *prevCfg
	}
	cfg.EnableReasoning = false
	currentConfig = &cfg

	msgsTools := augmentSystemWithTools(
		[]chatMessage{{Role: "user", Content: "прочитай файл main.go"}},
		decodeChatR83(t, toolsBodyR83).Tools)
	msgsPlain := []chatMessage{{Role: "user", Content: "прочитай файл main.go"}}

	promptTools, err := buildChatPromptWithOptions(msgsTools, model, chatPromptOptions{})
	if err != nil {
		t.Fatalf("buildChatPromptWithOptions (tools): %v", err)
	}
	promptPlain, err := buildChatPromptWithOptions(msgsPlain, model, chatPromptOptions{})
	if err != nil {
		t.Fatalf("buildChatPromptWithOptions (plain): %v", err)
	}

	if !containsThinkingInstruction(promptTools) {
		t.Fatal("per-model reasoning включён, но tools-запрос не получил thinking-инструкцию")
	}
	if !containsThinkingInstruction(promptPlain) {
		t.Fatal("per-model reasoning включён, но обычный запрос не получил thinking-инструкцию")
	}
}
