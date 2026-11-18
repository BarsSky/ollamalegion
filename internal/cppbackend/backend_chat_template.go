package cppbackend

import (
	"fmt"
	"ollama-loadbalancer/c/bridge"
	"sync"
)

// chatTemplateMu — сериализация вызовов chat template (R91, 2026-10-08).
//
// ЗАЧЕМ. llama_chat_apply_template / common_chat_templates_apply внутри держат
// СОБСТВЕННОЕ кэшированное состояние шаблона (minja-парсер) и не потокобезопасны.
// cppworker при этом обслуживает несколько запросов параллельно (n_parallel=2,
// плюс /api/chat и /v1/chat/completions), и каждый строит промпт на своём
// goroutine — то есть два вызова легко заходят в C одновременно.
//
// Живое следствие (нагрузочный прогон 2026-10-08, gemma-4, n_ctx=65536):
// SIGSEGV в bridge_apply_chat_template «signal arrived during cgo execution» —
// процесс падал целиком. Гонка такого класса воспроизводится не всегда, поэтому
// кроме recover() в cmd/cppworker (chat_template_guard.go) нужна и защита от
// одновременного входа.
//
// ЦЕНА: применение шаблона — короткая CPU-операция без GPU, поэтому сериализация
// почти не влияет на пропускную способность, а падение узла стоит куда дороже.
var chatTemplateMu sync.Mutex

// ApplyChatTemplate applies GGUF chat template to messages for a model.
// If no template is embedded in GGUF — returns bridge.ErrNoChatTemplate
// (caller can fallback to raw completion).
func (b *Backend) ApplyChatTemplate(modelName, system string, messages []bridge.ChatMessage, addAss bool) (string, error) {
	inst, err := b.getModelInstance(modelName)
	if err != nil {
		return "", err
	}
	if inst.handle == nil {
		return "", fmt.Errorf("model %s has no loaded handle", modelName)
	}
	chatTemplateMu.Lock()
	defer chatTemplateMu.Unlock()
	return inst.handle.ApplyChatTemplate(system, messages, addAss)
}

// ApplyChatTemplateWithThinking — Round 14b (2026-07-28): native enable_thinking
// через C++ API common_chat_templates_apply (см. c/bridge/csrc/chat_thinking.cpp).
//
// Применяет chat template с native enable_thinking параметром. Для моделей
// с Jinja enable_thinking variable (Qwen3-thinking, DeepSeek-R1) это даёт
// правильное thinking tag emission. Для моделей без такой variable
// (gemma-4-it, llama-3-it) — supportsThinking=false, caller решает что
// делать (наш fallback — soft prompt injection, см. handlers_chat.go).
//
// Параметры:
//
//	modelName         — загруженная модель
//	chatTemplateOverride — кастомный Jinja template ("" = use GGUF default)
//	messages          — список chat-сообщений (включая system если нужно)
//	enableThinking    — true = native thinking mode
//	addGenerationPrompt — true = добавить assistant turn tokens в конец
//
// Возвращает:
//
//	prompt            — formatted prompt
//	supportsThinking  — true если template поддерживает thinking
//	error             — nil / generic error
func (b *Backend) ApplyChatTemplateWithThinking(
	modelName, chatTemplateOverride string,
	messages []bridge.ChatMessage,
	enableThinking, addGenerationPrompt bool,
) (string, bool, error) {
	inst, err := b.getModelInstance(modelName)
	if err != nil {
		return "", false, err
	}
	if inst.handle == nil {
		return "", false, fmt.Errorf("model %s has no loaded handle", modelName)
	}
	// Тот же мьютекс, что и в ApplyChatTemplate: native-путь и legacy-путь ходят
	// в одно и то же не потокобезопасное состояние шаблона (см. chatTemplateMu).
	chatTemplateMu.Lock()
	defer chatTemplateMu.Unlock()
	return inst.handle.ApplyChatTemplateWithThinking(
		chatTemplateOverride, messages, enableThinking, addGenerationPrompt,
	)
}

// ApplyChatTemplateWithTools — R434 (2026-11-05): рендер шаблона ВМЕСТЕ с
// инструментами запроса.
//
// Возвращает:
//
//	prompt          — готовый промпт
//	supportsThinking — шаблон поддерживает native enable_thinking
//	toolsRendered   — шаблон НАРИСОВАЛ секцию инструментов (иначе вызывающий
//	                  обязан добавить текстовую преамбулу как fallback)
//	toolFormat      — common_chat_format, которым llama.cpp видит вызовы
//	                  (0 = CONTENT_ONLY)
func (b *Backend) ApplyChatTemplateWithTools(
	modelName, chatTemplateOverride string,
	messages []bridge.ChatMessage,
	toolsJSON string,
	enableThinking, addGenerationPrompt bool,
) (string, bool, bool, int, error) {
	inst, err := b.getModelInstance(modelName)
	if err != nil {
		return "", false, false, 0, err
	}
	if inst.handle == nil {
		return "", false, false, 0, fmt.Errorf("model %s has no loaded handle", modelName)
	}
	// Тот же мьютекс, что и в ApplyChatTemplate: native-путь ходит в одно и то же
	// не потокобезопасное состояние шаблона (см. chatTemplateMu).
	chatTemplateMu.Lock()
	defer chatTemplateMu.Unlock()
	return inst.handle.ApplyChatTemplateWithTools(
		chatTemplateOverride, messages, toolsJSON, enableThinking, addGenerationPrompt,
	)
}

// GetChatTemplate returns raw chat template from GGUF of a model.
func (b *Backend) GetChatTemplate(modelName string) (string, error) {
	inst, err := b.getModelInstance(modelName)
	if err != nil {
		return "", err
	}
	if inst.handle == nil {
		return "", fmt.Errorf("model %s has no loaded handle", modelName)
	}
	return inst.handle.GetChatTemplate()
}

// ============================================================
// GPU управление
// ============================================================
