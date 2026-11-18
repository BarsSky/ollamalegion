package main

import (
	"encoding/json"
	"ollama-loadbalancer/pkg/tokencount"
)

// openAITokenUsage — счётчики токенов для OpenAI usage.
type openAITokenUsage struct {
	PromptTokens     int
	CompletionTokens int
}

// writeOpenAIUsageChunk эмитит в поток SSE chunk с блоком `usage` если запрошено.
// Соответствует OpenAI streaming spec (https://platform.openai.com/docs/api-reference/chat-streaming).
//
// Использование:
//   - includeUsage == true → chunk с полем `usage` (только в финальном чанке).
//   - includeUsage == false → no-op (legacy-режим, не мешаем старым клиентам).
//
// chunkID, created, modelName — копируются из вызывающего запроса (или
// генерируются заново). Безопасно для буферизованной и реальной передачи.
func writeOpenAIUsageChunk(sw *safeStreamWriter, chunkID string, created int64, modelName string, prompt, output string, includeUsage bool) {
	if !includeUsage {
		return
	}
	usage := openAITokenUsage{
		PromptTokens:     countTokensSafe(modelName, prompt),
		CompletionTokens: countTokensSafe(modelName, output),
	}
	chunk := map[string]interface{}{
		"id":      chunkID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   modelName,
		"choices": []map[string]interface{}{},
		"usage": map[string]interface{}{
			"prompt_tokens":     usage.PromptTokens,
			"completion_tokens": usage.CompletionTokens,
			"total_tokens":      usage.PromptTokens + usage.CompletionTokens,
		},
	}
	jsonData, _ := json.Marshal(chunk)
	sw.Writef("data: %s\n\n", jsonData)
	sw.Flush()
}

// ============================================================
// Usage helpers
// ============================================================

// buildUsage возвращает OpenAI-совместимый блок `usage` для /v1/chat/completions
// и /v1/completions. Использует реальный токенайзер модели (backend.CountTokens);
// если он недоступен — fallback на ~4 символа/токен. Cline/Roo/OpenWebUI читают
// `total_tokens` для трекинга заполненности контекстного окна.
//
// Параметры:
//   - prompt     — входные сообщения (для chat) или prompt (для completions);
//   - output     — полный output модели (result.Output);
//   - modelName  — для выбора токенайзера.
//
// Возвращает map с ключами prompt_tokens, completion_tokens, total_tokens,
// совместимыми со спецификацией OpenAI.
func buildUsage(prompt, output, modelName string) map[string]interface{} {
	promptTokens := countTokensSafe(modelName, prompt)
	completionTokens := countTokensSafe(modelName, output)
	totalTokens := promptTokens + completionTokens
	return map[string]interface{}{
		"prompt_tokens":     promptTokens,
		"completion_tokens": completionTokens,
		"total_tokens":      totalTokens,
	}
}

// buildPromptTokens считает prompt_tokens для embeddings (только входной текст).
// Используется в /v1/embeddings.
func buildPromptTokens(modelName, input string) int {
	return countTokensSafe(modelName, input)
}

// countTokensSafe считает токены, безопасно обрабатывая backend==nil (в unit-тестах).
//
// R66: фолбэк — pkg/tokencount, а не len([]rune)/4. Прежняя формула занижала
// счёт на любом измеренном словаре (латиница 63-81%, кириллица до 304%) и
// возвращала 0 для текста короче 4 символов. См. pkg/tokencount.
func countTokensSafe(modelName, text string) int {
	if text == "" {
		return 0
	}
	if backend == nil {
		return tokencount.Estimate(text)
	}
	if _, err := backend.GetModel(modelName); err != nil {
		return tokencount.Estimate(text)
	}
	return backend.CountTokens(modelName, text)
}
