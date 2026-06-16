package balancer

// Этот файл содержит экспортированные обёртки для unit-тестов в internal/balancer/...
// и внешних тестах в tests/multiclient/... (Phase 9 regression для OpenWebUI бага).
//
// Сами функции остаются приватными, тестовые обёртки позволяют избежать
// экспорта внутренних API, при этом давая тестам доступ к нужным точкам.

// TranslateOpenAISSEDataToOllamaExportedForTest — публичная обёртка для тестов.
// Не использовать в production-коде.
func TranslateOpenAISSEDataToOllamaExportedForTest(ollamaPath string, sseData []byte, modelName string) []byte {
	return translateOpenAISSEDataToOllama(ollamaPath, sseData, modelName)
}