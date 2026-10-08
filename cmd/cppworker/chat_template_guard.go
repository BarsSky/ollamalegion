// chat_template_guard.go — R91 (2026-10-08): cgo SIGSEGV в chat template больше
// не убивает воркер и не повторяется на «отравленной» модели.
//
// ЖИВОЙ СЛУЧАЙ (нагрузочный прогон 2026-10-08, gemma-4-E4B-it-Q4_K_M,
// n_ctx=65536, n_parallel=2, запрос с tools[] от балансера):
//
//	SIGSEGV: segmentation violation
//	signal arrived during cgo execution
//	c/bridge.(*ModelHandle).ApplyChatTemplate → C.bridge_apply_chat_template
//	cmd/cppworker.buildChatPromptWithOptions (handlers_chat.go:721)
//	→ весь контейнер cppworker перезапускается (Docker RestartCount +1),
//	  клиент получает 503/обрыв, все остальные запросы на узле теряются.
//
// ДВЕ ПРИЧИНЫ, ПОЧЕМУ ЭТО БЫЛО ВОЗМОЖНО.
//
//  1. recover() стоял ТОЛЬКО вокруг ApplyChatTemplateWithThinking (native
//     C++-путь). Legacy-вызов bridge_apply_chat_template (LLM-шаблон из GGUF) и
//     повторный вызов после неудачи — БЕЗ защиты. Ровно на этот путь и упал
//     процесс: в логе нет ни одной строки «recovered», то есть защищённый вызов
//     до падения даже не доходил (CPPWORKER_ENABLE_REASONING выключен —
//     reasoning-ветка пропускается целиком).
//
//  2. После ПЕРВОГО пойманного SIGSEGV состояние C уже испорчено: повторный
//     вызов того же API добивает процесс. Поэтому мало обернуть вызовы в
//     recover() — нужно ПОМНИТЬ, что на этой модели шаблон сломан, и больше в C
//     не ходить. Ровно так и было задумано в комментарии handlers_chat.go
//     («C-state после SIGSEGV может быть corrupted»), но реализовано не было.
//
// ЧТО ДЕЛАЕМ. Все обращения к chat template идут через safeApplyChatTemplate*:
//   - если модель помечена «шаблон сломан» — в C не ходим вообще, сразу отдаём
//     ошибку, и вызывающий уходит на ручную сборку промпта (она для gemma и
//     предназначена: см. isGemmaModel + buildChatPromptFromMessages);
//   - любой panic/SIGSEGV из cgo ловится recover(), модель помечается, а воркер
//     остаётся жив;
//   - пометка снимается при явной (пере)загрузке модели: оператор может
//     перезапустить модель, и это его штатный способ вылечить узел.
//
// ЧЕГО ЭТО НЕ ЛЕЧИТ: если SIGSEGV случился, память C могла остаться
// повреждённой, и следующий вызов ЛЮБОГО другого C API (токенизация,
// инференс) теоретически может упасть. Поэтому в лог пишется ERROR с прямым
// советом перезапустить cppworker; но потерю всех запросов узла мы уже
// предотвратили.
package main

import (
	"fmt"
	"sync"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/pkg/logger"
)

var (
	chatTemplateBrokenMu sync.RWMutex
	// chatTemplateBroken: имя модели → причина, по которой в C ходить нельзя.
	chatTemplateBroken = map[string]string{}
)

// chatTemplateBrokenReason — помечена ли модель как «шаблон сломан».
func chatTemplateBrokenReason(modelName string) (string, bool) {
	chatTemplateBrokenMu.RLock()
	defer chatTemplateBrokenMu.RUnlock()
	reason, ok := chatTemplateBroken[modelName]
	return reason, ok
}

// markChatTemplateBroken — запомнить, что на этой модели chat template падает.
// Повторные вызовы не выполняются (см. safeApplyChatTemplate*).
func markChatTemplateBroken(modelName, reason string) {
	if modelName == "" {
		return
	}
	chatTemplateBrokenMu.Lock()
	_, already := chatTemplateBroken[modelName]
	chatTemplateBroken[modelName] = reason
	chatTemplateBrokenMu.Unlock()
	if !already {
		logger.Get().Errorw("chat template отключён для модели до перезапуска cppworker: "+
			"cgo SIGSEGV в chat template (повторные вызовы не выполняются, "+
			"промпт собирается вручную)",
			"model", modelName, "reason", reason,
			"hint", "проверьте n_ctx/template модели; для полного сброса состояния C перезапустите cppworker")
	}
}

// clearChatTemplateBroken — снять пометку. Вызывается при явной загрузке модели:
// оператор перезапускает модель, а не воркер, и это должно лечить узел.
func clearChatTemplateBroken(modelName string) {
	if modelName == "" {
		return
	}
	chatTemplateBrokenMu.Lock()
	_, had := chatTemplateBroken[modelName]
	delete(chatTemplateBroken, modelName)
	chatTemplateBrokenMu.Unlock()
	if had {
		logger.Get().Infow("chat template разблокирован для модели (загружена заново)",
			"model", modelName)
	}
}

// safeApplyChatTemplate — bridge.ApplyChatTemplate под recover() + защита от
// повторного вызова на уже упавшей модели.
func safeApplyChatTemplate(modelName, system string, msgs []bridge.ChatMessage, addAss bool) (prompt string, err error) {
	if reason, broken := chatTemplateBrokenReason(modelName); broken {
		return "", fmt.Errorf("chat template отключён после cgo SIGSEGV (%s); промпт собирается вручную", reason)
	}
	defer func() {
		if r := recover(); r != nil {
			reason := fmt.Sprintf("legacy ApplyChatTemplate: %v", r)
			markChatTemplateBroken(modelName, reason)
			prompt, err = "", fmt.Errorf("native cgo panic: %v", r)
		}
	}()
	return backend.ApplyChatTemplate(modelName, system, msgs, addAss)
}

// safeApplyChatTemplateWithThinking — то же для native-пути с enable_thinking.
// Раньше recover() был вписан прямо в handlers_chat.go; здесь он один на оба
// пути, чтобы вторая точка входа не осталась без защиты (именно так и возник
// живой дефект).
func safeApplyChatTemplateWithThinking(
	modelName, chatTemplateOverride string,
	msgs []bridge.ChatMessage,
	enableThinking, addGenerationPrompt bool,
) (prompt string, supportsThinking bool, err error) {
	if reason, broken := chatTemplateBrokenReason(modelName); broken {
		return "", false, fmt.Errorf("chat template отключён после cgo SIGSEGV (%s); промпт собирается вручную", reason)
	}
	defer func() {
		if r := recover(); r != nil {
			reason := fmt.Sprintf("ApplyChatTemplateWithThinking: %v", r)
			markChatTemplateBroken(modelName, reason)
			prompt, supportsThinking, err = "", false, fmt.Errorf("native cgo panic: %v", r)
		}
	}()
	return backend.ApplyChatTemplateWithThinking(modelName, chatTemplateOverride, msgs, enableThinking, addGenerationPrompt)
}
