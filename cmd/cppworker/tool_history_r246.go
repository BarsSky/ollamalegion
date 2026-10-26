// tool_history_r246.go — R246 (2026-10-26): tool-история доезжает до шаблона
// модели СТРУКТУРНО, а не JSON-текстом в content.
//
// ЖИВОЙ ДЕФЕКТ (DeepSeek Harness → балансер → cppworker → gemma-4,
// потоковый /v1/chat/completions с 25 инструментами).
//
// Первый ход: модель вызвала read, клиент выполнил, второй ход клиент прислал
// с assistant(tool_calls) + role:"tool"(результат). Промпт второго хода,
// снятый с /api/v1/cppworker/debug/last-prompt:
//
//	<|turn>system ...<turn|>
//	<|turn>user Read the file go.mod and name the module.<turn|>
//	<|turn>model [[{"id":"call_read",...,"arguments":{"file_path":"go.mod"}}]]<turn|>
//	<|turn>model
//
// Результата инструмента в промпте НЕТ ВООБЩЕ. Модель его и не видела:
// в reasoning она писала «output is not provided yet, I must wait». Агент
// останавливался с пустым ответом (finish_reason=stop, content=""), и клиент
// показывал ошибку/пустоту — «агент не может применить инструменты».
//
// Причина. Через C-мост уходили только role+content. Шаблон gemma-4:
//   - вызовы рисует ТОЛЬКО из структурного message.get('tool_calls')
//     (c/llama.cpp/common — msgs_to_json_oaicompat передаёт tool_calls и
//     tool_call_id в Jinja только при заполненных полях common_chat_msg);
//   - сообщения role="tool" в основном цикле пропускает
//     (`if message['role'] != 'tool'`) и подставляет их из tool_calls
//     ПРЕДЫДУЩЕГО model-сообщения.
//
// Без структурных полей и вызов, и результат исчезали: вызов уезжал текстом
// `[[{...}]]`, результат — в никуда.
//
// Правило. Форма tool-истории выбирается по ВОЗМОЖНОСТЯМ ШАБЛОНА модели:
//   - шаблон сам рисует role="tool" (Qwen3.8: `message.role == "tool"`) —
//     оставляем прежнее поведение (вызовы текстом, результат отдельным tool-ом);
//     Qwen такой шаблон ТРЕБУЕТ arguments объектом, JSON-строку он отвергает,
//     поэтому структурные вызовы ему передавать нельзя;
//   - шаблон НЕ рисует role="tool", но знает про tool_calls (gemma-4) —
//     передаём структурные вызовы и role="tool" с tool_call_id;
//   - шаблон про инструменты не знает вовсе — результат уходит текстом в user,
//     иначе модель его не увидит.
package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/pkg/logger"
)

// templateToolCaps — что шаблон модели умеет с tool-историей.
type templateToolCaps struct {
	// StructuredCalls — шаблон читает message.tool_calls.
	StructuredCalls bool
	// StandaloneTool — шаблон сам рисует сообщения role="tool".
	StandaloneTool bool
	// ConsumesToolResults — в шаблоне есть признаки отрисовки РЕЗУЛЬТАТА
	// инструмента. Без них role="tool" будет молча пропущен (как было у gemma-4).
	ConsumesToolResults bool
	// Known — шаблон удалось прочитать (иначе считаем возможностей нет).
	Known bool
}

var (
	templateCapsMu    sync.RWMutex
	templateCapsCache = map[string]templateToolCaps{}
)

// standaloneToolRoleRe — обращение к роли tool на РАВЕНСТВО в Jinja-шаблоне.
// Нерaвенство (`role != 'tool'`, как в gemma-4) намеренно не считается:
// такой шаблон пропускает tool-сообщения в основном цикле.
var standaloneToolRoleRe = regexp.MustCompile(`role['"]?\]?\s*==\s*['"]tool['"]`)

// toolRoleInListRe — роль tool объявлена СПИСКОМ ролей шаблона
// (`set message_roles = ['system','user','assistant','tool']`).
var toolRoleInListRe = regexp.MustCompile(`(message_roles|roles)\s*=\s*\[[^\]]*['"]tool['"]`)

// toolResultConsumptionRe — признаки того, что шаблон РИСУЕТ результат
// инструмента: role="tool" в цикле, tool_responses/tool_call_id, либо
// прямой проход вперёд по следующим tool-сообщениям.
var toolResultConsumptionRe = regexp.MustCompile(`tool_responses|tool_call_id|tool_response|loop_messages\[|loop\.nextitem|previtem\.role`)

// templateToolCapsForModel — возможности шаблона модели (кэш на процесс).
// Ошибка чтения шаблона = «возможностей нет»: тогда работает текстовый фолбэк,
// который не может сломать шаблон.
func templateToolCapsForModel(modelName string) templateToolCaps {
	if modelName == "" || backend == nil {
		return templateToolCaps{}
	}
	templateCapsMu.RLock()
	caps, ok := templateCapsCache[modelName]
	templateCapsMu.RUnlock()
	if ok {
		return caps
	}

	src, err := backend.GetChatTemplate(modelName)
	caps = analyzeTemplateToolCaps(src)
	caps.Known = err == nil && src != ""

	templateCapsMu.Lock()
	templateCapsCache[modelName] = caps
	templateCapsMu.Unlock()

	if err != nil && logger.Get() != nil {
		logger.Get().Debugw("tool history: chat template unavailable, using text fallback",
			"model", modelName, "error", err)
	}
	return caps
}

// analyzeTemplateToolCaps — разбор исходника Jinja-шаблона.
func analyzeTemplateToolCaps(src string) templateToolCaps {
	if strings.TrimSpace(src) == "" {
		return templateToolCaps{}
	}
	standalone := standaloneToolRoleRe.MatchString(src) || toolRoleInListRe.MatchString(src)
	return templateToolCaps{
		StructuredCalls:     strings.Contains(src, "tool_calls"),
		StandaloneTool:      standalone,
		ConsumesToolResults: standalone || toolResultConsumptionRe.MatchString(src),
		Known:               true,
	}
}

// usesStructuredToolHistory — отдавать ли tool-историю СТРУКТУРНО
// (tool_calls + role="tool" с tool_call_id).
//
// Условие намеренно ТРОЙНОЕ: шаблон читает tool_calls, НЕ рисует role="tool"
// сам (иначе работает прежний путь) И вообще умеет отрисовать результат
// инструмента. Без последнего признака role="tool" уходит в шаблон, который
// его молча игнорирует, и результат снова теряется — ровно этот класс дефекта
// и поймали в R246 (проверено на 68 шаблонах из c/llama.cpp/models/templates:
// Seed-OSS и LFM2.5-8B-A1B читают tool_calls, но результатов не рисуют).
func usesStructuredToolHistory(caps templateToolCaps) bool {
	return caps.StructuredCalls && !caps.StandaloneTool && caps.ConsumesToolResults
}

// resetTemplateToolCapsCache — сброс кэша (нужен при смене модели/перезагрузке
// профиля в тестах и при горячей перезагрузке шаблона).
func resetTemplateToolCapsCache() {
	templateCapsMu.Lock()
	templateCapsCache = map[string]templateToolCaps{}
	templateCapsMu.Unlock()
}

// toolCallArgsString — аргументы вызова как JSON-СТРОКА (форма OpenAI).
// Клиент может прислать и объект (Ollama-форма) — тогда сериализуем.
func toolCallArgsString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if strings.TrimSpace(s) == "" {
			return "{}"
		}
		return s
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "{}"
	}
	return trimmed
}

// historyIDsForCalls — id вызовов ассистента; пустой id заменяется на
// детерминированный, иначе склейка с результатом инструмента невозможна
// (шаблон ищет tool_call_id == tool_calls[].id).
func historyIDsForCalls(calls []ollamaToolCall, msgIndex int) []bridge.ChatToolCall {
	out := make([]bridge.ChatToolCall, 0, len(calls))
	for i, call := range calls {
		id := strings.TrimSpace(call.ID)
		if id == "" {
			id = fmt.Sprintf("call_%d_%d", msgIndex, i)
		}
		out = append(out, bridge.ChatToolCall{
			ID:        id,
			Name:      call.Function.Name,
			Arguments: toolCallArgsString(call.Function.Arguments),
		})
	}
	return out
}

// buildNativeToolHistory — сообщения для НАТИВНОГО пути (Jinja-шаблон).
//
// structured=true — вызовы отдаются полями tool_calls, content остаётся чистым
// (без JSON-дампа): так их видит шаблон gemma-4.
func buildNativeToolHistory(msgs []chatMessage, caps templateToolCaps) []bridge.ChatMessage {
	if !usesStructuredToolHistory(caps) {
		return buildLegacyToolHistory(msgs)
	}

	out := make([]bridge.ChatMessage, 0, len(msgs))
	var pendingIDs []string
	var pendingNames []string
	pendingUsed := 0

	for i, m := range msgs {
		switch m.Role {
		case "assistant":
			calls := historyIDsForCalls(m.ToolCalls, i)
			ids := make([]string, 0, len(calls))
			names := make([]string, 0, len(calls))
			for _, c := range calls {
				ids = append(ids, c.ID)
				names = append(names, c.Name)
			}
			pendingIDs, pendingNames, pendingUsed = ids, names, 0
			out = append(out, bridge.ChatMessage{
				Role:      m.Role,
				Content:   m.Content,
				ToolCalls: calls,
			})
		case "tool":
			id := strings.TrimSpace(m.ToolCallID)
			name := toolMessageName(m)
			if id == "" && pendingUsed < len(pendingIDs) {
				id = pendingIDs[pendingUsed]
				if name == "" && pendingUsed < len(pendingNames) {
					name = pendingNames[pendingUsed]
				}
				pendingUsed++
			}
			if id == "" {
				// Перед этим tool-сообщением нет model-сообщения с tool_calls
				// (история пришла от клиента без вызова). Шаблон gemma-4 такое
				// сообщение пропустит молча — отдаём текст в user, иначе
				// результат инструмента снова потеряется.
				out = append(out, bridge.ChatMessage{Role: "user", Content: toolResultContent(m.Content, name)})
				continue
			}
			// Content без префикса: имя инструмента шаблон подставит сам из
			// tool_calls предыдущего model-сообщения.
			out = append(out, bridge.ChatMessage{
				Role:       "tool",
				Content:    m.Content,
				ToolCallID: id,
			})
		default:
			out = append(out, bridge.ChatMessage{Role: m.Role, Content: m.Content})
		}
	}
	return out
}

// buildLegacyToolHistory — сообщения для НЕ-структурного пути: вызовы уходят
// JSON-текстом в content, результат инструмента — в user-сообщении.
//
// Этот же вид уходит в низкоуровневый llama_chat_apply_template, который про
// role="tool" ничего не знает.
func buildLegacyToolHistory(msgs []chatMessage) []bridge.ChatMessage {
	out := make([]bridge.ChatMessage, 0, len(msgs))
	var pendingNames []string
	pendingUsed := 0

	for _, m := range msgs {
		switch m.Role {
		case "assistant":
			content := m.Content
			if len(m.ToolCalls) > 0 {
				if block := legacyToolCallsText(m.ToolCalls); block != "" {
					if content != "" {
						content = content + "\n" + block
					} else {
						content = block
					}
				}
			}
			pendingNames = pendingNames[:0]
			for _, call := range m.ToolCalls {
				pendingNames = append(pendingNames, call.Function.Name)
			}
			pendingUsed = 0
			out = append(out, bridge.ChatMessage{Role: m.Role, Content: content})
		case "tool":
			name := toolMessageName(m)
			if name == "" && pendingUsed < len(pendingNames) {
				name = pendingNames[pendingUsed]
				pendingUsed++
			}
			out = append(out, bridge.ChatMessage{Role: "user", Content: toolResultContent(m.Content, name)})
		default:
			out = append(out, bridge.ChatMessage{Role: m.Role, Content: m.Content})
		}
	}
	return out
}

// legacyToolCallsText — текстовый дамп вызовов в прежней форме (`[[...]]`).
//
// Форма сохранена намеренно: на ней работают модели, которых устраивает
// JSON-текст (Qwen3.8 и др.), и её разбирает наш же парсер вызовов.
func legacyToolCallsText(calls []ollamaToolCall) string {
	if len(calls) == 0 {
		return ""
	}
	raw, err := json.Marshal(calls)
	if err != nil || len(raw) == 0 {
		return ""
	}
	return "[" + string(raw) + "]"
}

// toolMessageName — имя инструмента в tool-сообщении (OpenAI: name,
// Ollama: tool_name).
func toolMessageName(m chatMessage) string {
	if strings.TrimSpace(m.ToolName) != "" {
		return strings.TrimSpace(m.ToolName)
	}
	return strings.TrimSpace(m.Name)
}

// toolResultContent — тело результата инструмента для модели.
//
// Префикс с именем инструмента: шаблон, который не рисует role="tool", иначе
// отдаёт модели голый текст без указания, какой инструмент его вернул.
func toolResultContent(content, name string) string {
	if name == "" {
		return content
	}
	return "tool result (" + name + "): " + content
}
