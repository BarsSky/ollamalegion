package balancer

// Этот файл содержит экспортированные обёртки для unit-тестов в internal/balancer/...
// и внешних тестах в tests/multiclient/... (Phase 9 regression для OpenWebUI бага).
//
// Сами функции остаются приватными, тестовые обёртки позволяют избежать
// экспорта внутренних API, при этом давая тестам доступ к нужным точкам.

// TranslateOpenAISSEDataToOllamaExportedForTest — публичная обёртка для тестов.
// Не использовать в production-коде.
func TranslateOpenAISSEDataToOllamaExportedForTest(ollamaPath string, sseData []byte, modelName string) []byte {
	return translateOpenAISSEDataToOllama(ollamaPath, sseData, modelName, nil)
}

// TranslateOpenAIResponseToOllamaExportedForTest — публичная обёртка для тестов.
// Преобразует OpenAI response body в Ollama-формат.
// Не использовать в production-коде.
func TranslateOpenAIResponseToOllamaExportedForTest(ollamaPath string, openaiBody []byte, modelName string) ([]byte, error) {
	return translateOpenAIResponseToOllama(ollamaPath, openaiBody, modelName)
}

// TranslateOllamaChatToOpenAIExportedForTest — публичная обёртка для тестов.
// Преобразует Ollama /api/chat запрос в OpenAI /v1/chat/completions.
// Не использовать в production-коде.
func TranslateOllamaChatToOpenAIExportedForTest(body []byte) ([]byte, error) {
	return translateOllamaChatToOpenAI(body)
}

// BuildErrorOllamaResponseExportedForTest — публичная обёртка для тестов.
// Строит корректный Ollama-ответ с ошибкой.
// Не использовать в production-коде.
func BuildErrorOllamaResponseExportedForTest(ollamaPath, modelName, errMsg string) []byte {
	return buildErrorOllamaResponse(ollamaPath, modelName, errMsg)
}

