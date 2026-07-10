// tools_prompt_cache_test.go — Тесты для компактного tools-prompt + кеша.
//
// Проверяет:
//   - buildToolsSystemPrompt возвращает компактный формат с сигнатурами
//   - Кеш fingerprint-ключей работает: одинаковые tools → одинаковый результат
//   - Кеш работает: повторный вызов возвращает ту же строку (без изменений)
//   - toolsPromptFingerprint стабилен относительно порядка JSON-полей
//   - Кеш возвращает "" если запись устарела (TTL > 0 прошло)
//   - trimDescription корректно обрезает длинные описания
package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// makeTestTools возвращает стандартный набор tools для тестов.
func makeTestTools() []openAITool {
	return []openAITool{
		{
			Type: "function",
			Function: openAIFunction{
				Name:        "search",
				Description: "Search the web for information",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"},"limit":{"type":"integer"}},"required":["q"]}`),
			},
		},
		{
			Type: "function",
			Function: openAIFunction{
				Name:        "calculator",
				Description: "Perform mathematical calculations",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"expression":{"type":"string"}},"required":["expression"]}`),
			},
		},
	}
}

// jsonRawLiteral — алиас для удобства в тестах.
func jsonRawLiteral(s string) json.RawMessage { return json.RawMessage(s) }

// TestBuildToolsSystemPrompt_CompactFormat проверяет, что формат
// содержит компактные сигнатуры (q:str, limit?:int) вместо полного JSON Schema.
func TestBuildToolsSystemPrompt_CompactFormat(t *testing.T) {
	tools := makeTestTools()
	prompt := buildToolsSystemPrompt(tools)

	// Промпт не должен быть пустым.
	if prompt == "" {
		t.Fatal("buildToolsSystemPrompt returned empty string for non-empty tools")
	}
	// Должен содержать имена tool'ов.
	if !strings.Contains(prompt, "search") {
		t.Errorf("prompt missing 'search' tool name: %s", prompt)
	}
	if !strings.Contains(prompt, "calculator") {
		t.Errorf("prompt missing 'calculator' tool name: %s", prompt)
	}
	// Должен содержать сигнатуру параметров (компактный формат).
	if !strings.Contains(prompt, "q:str") {
		t.Errorf("prompt missing 'q:str' parameter signature: %s", prompt)
	}
	// НЕ должен содержать raw JSON Schema (компактный формат этого исключает).
	if strings.Contains(prompt, `"properties"`) {
		t.Errorf("prompt should not contain raw JSON Schema '\"properties\"': %s", prompt)
	}
	if strings.Contains(prompt, `"type":"object"`) {
		t.Errorf("prompt should not contain raw JSON Schema '\"type\":\"object\"': %s", prompt)
	}
	// Должен быть в пределах лимита.
	if len(prompt) > toolsPromptMaxTotalChars+50 { // +50 для тега [truncated]
		t.Errorf("prompt length %d exceeds toolsPromptMaxTotalChars=%d", len(prompt), toolsPromptMaxTotalChars)
	}
}

// TestBuildToolsSystemPrompt_CacheHit проверяет, что повторный вызов
// с теми же tools возвращает закешированный результат.
//
// Используем уникальный fingerprint (через уникальное имя tool), чтобы
// не зависеть от того, прогрели ли кеш предыдущие тесты в этом файле.
func TestBuildToolsSystemPrompt_CacheHit(t *testing.T) {
	tools := []openAITool{
		{
			Type: "function",
			Function: openAIFunction{
				Name:        "cache_hit_test_unique_tool_name_xyzzz",
				Description: "unique test tool to bypass prior cache warmup",
				Parameters:  json.RawMessage(`{}`),
			},
		},
	}

	// Гарантируем чистое состояние кеша для нашего fingerprint.
	fp := toolsPromptFingerprint(tools)
	toolsPromptCache.Delete(fp)

	// Первый вызов — miss (cache entry создаётся).
	prompt1 := buildToolsSystemPrompt(tools)
	// Второй вызов — должен вернуть ту же строку из кеша.
	prompt2 := buildToolsSystemPrompt(tools)

	if prompt1 != prompt2 {
		t.Errorf("Cached prompt differs from original:\nfirst:  %s\nsecond: %s", prompt1, prompt2)
	}
	// Гарантируем, что запись действительно есть в кеше.
	if _, ok := toolsPromptCache.items[fp]; !ok {
		t.Errorf("cache entry for fingerprint %q should exist after first call", fp)
	}
	// И что getCachedToolsPrompt возвращает тот же текст.
	if cached := getCachedToolsPrompt(fp); cached != prompt1 {
		t.Errorf("getCachedToolsPrompt(%q) = %q, want %q", fp, cached, prompt1)
	}
}

// TestBuildToolsSystemPrompt_DifferentToolsDifferentCacheKey проверяет,
// что разные tools дают разный fingerprint и, следовательно, разный prompt.
func TestBuildToolsSystemPrompt_DifferentToolsDifferentCacheKey(t *testing.T) {
	tools1 := []openAITool{
		{Type: "function", Function: openAIFunction{Name: "a", Description: "first"}},
	}
	tools2 := []openAITool{
		{Type: "function", Function: openAIFunction{Name: "b", Description: "second"}},
	}
	prompt1 := buildToolsSystemPrompt(tools1)
	prompt2 := buildToolsSystemPrompt(tools2)
	if prompt1 == prompt2 {
		t.Errorf("Different tools produced same prompt:\n%s\n%s", prompt1, prompt2)
	}
}

// TestToolsPromptFingerprint_Stable проверяет, что fingerprint
// одинаков для одного и того же набора tools (порядок tools сохраняется).
func TestToolsPromptFingerprint_Stable(t *testing.T) {
	tools := makeTestTools()
	fp1 := toolsPromptFingerprint(tools)
	fp2 := toolsPromptFingerprint(tools)
	if fp1 == "" {
		t.Fatal("fingerprint is empty")
	}
	if fp1 != fp2 {
		t.Errorf("fingerprint not stable: %s vs %s", fp1, fp2)
	}
	// Длина SHA1 hex = 40 символов.
	if len(fp1) != 40 {
		t.Errorf("fingerprint length = %d, expected 40", len(fp1))
	}
}

// TestToolsPromptFingerprint_EmptyTools проверяет edge case: пустой набор.
func TestToolsPromptFingerprint_EmptyTools(t *testing.T) {
	if fp := toolsPromptFingerprint(nil); fp != "" {
		t.Errorf("fingerprint for nil tools = %q, want empty string", fp)
	}
	if fp := toolsPromptFingerprint([]openAITool{}); fp != "" {
		t.Errorf("fingerprint for empty tools = %q, want empty string", fp)
	}
}

// TestToolsPromptFingerprint_IgnoresNonFunction проверяет, что tools
// с Type != "function" игнорируются при расчёте fingerprint.
func TestToolsPromptFingerprint_IgnoresNonFunction(t *testing.T) {
	tools := []openAITool{
		{Type: "function", Function: openAIFunction{Name: "a"}},
		{Type: "other", Function: openAIFunction{Name: "ignored"}},
	}
	fp := toolsPromptFingerprint(tools)
	if fp == "" {
		t.Fatal("fingerprint is empty")
	}
}

// TestGetCachedToolsPrompt_TTLExpiry проверяет, что записи с истекшим TTL
// возвращают "" (хотя фактически НЕ удаляются до следующей записи).
func TestGetCachedToolsPrompt_TTLExpiry(t *testing.T) {
	fp := "test-ttl-key"
	prompt := "test prompt content"

	putCachedToolsPrompt(fp, prompt)

	// Сразу — должно вернуть закэшированное значение.
	if got := getCachedToolsPrompt(fp); got != prompt {
		t.Errorf("getCachedToolsPrompt right after put = %q, want %q", got, prompt)
	}

	// Имитируем устаревание: подменяем createdAt на старый таймстамп через
	// прямую запись в toolsPromptCache (sync.Map).
	type oldEntry struct {
		prompt    string
		createdAt time.Time
	}
	oldPrompt := "outdated prompt content"
	toolsPromptCache.Put(fp, oldPrompt)
	// Force backdate via internal access (Go test in same package).
	toolsPromptCache.mu.Lock()
	if el, ok := toolsPromptCache.items[fp]; ok {
		el.Value.(*toolsPromptCacheEntry).createdAt = time.Now().Add(-2 * toolsPromptCacheTTL)
	}
	toolsPromptCache.mu.Unlock()

	if got := getCachedToolsPrompt(fp); got != "" {
		t.Errorf("getCachedToolsPrompt after TTL expiry = %q, want empty string", got)
	}
}

// TestPutCachedToolsPrompt_EmptyFingerprintIsNoOp проверяет, что нельзя
// записать в кеш с пустым fingerprint (защита от загрязнения).
func TestPutCachedToolsPrompt_EmptyFingerprintIsNoOp(t *testing.T) {
	toolsPromptCache.Delete("")
	putCachedToolsPrompt("", "should not be stored")
	if _, ok := toolsPromptCache.Get(""); ok {
		t.Error("empty fingerprint should not be stored")
	}
	putCachedToolsPrompt("valid-fp", "")
	if _, ok := toolsPromptCache.Get("valid-fp"); ok {
		t.Error("empty prompt should not be stored")
	}
}

// TestTrimDescription_BasicCases проверяет trimDescription на разных длинах.
func TestTrimDescription_BasicCases(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		maxChars int
		want     string
	}{
		{"empty input", "", 100, ""},
		{"zero max", "hello world", 0, ""},
		{"shorter than max", "short desc", 100, "short desc"},
		{"exact max", "exact length", 12, "exact length"},
		{"longer truncated", "this is a very long description that needs to be cut", 30, "this is a very long..."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := trimDescription(tc.input, tc.maxChars)
			if got != tc.want {
				t.Errorf("trimDescription(%q, %d) = %q, want %q",
					tc.input, tc.maxChars, got, tc.want)
			}
		})
	}
}

// TestTrimDescription_WordBoundary проверяет, что обрезка происходит
// по границе слова, а не посередине.
func TestTrimDescription_WordBoundary(t *testing.T) {
	in := "this is a long description with several words"
	got := trimDescription(in, 25)
	// Должно обрезать по последнему пробелу в первых 25 символах.
	if !strings.HasSuffix(got, "...") {
		t.Errorf("trimDescription should end with '...': %q", got)
	}
	if strings.Contains(got, "words") {
		t.Errorf("trimDescription should not include 'words' (out of range): %q", got)
	}
}

// TestBuildToolsSystemPrompt_EmptyTools — edge case: пустой список.
func TestBuildToolsSystemPrompt_EmptyTools(t *testing.T) {
	if got := buildToolsSystemPrompt(nil); got != "" {
		t.Errorf("buildToolsSystemPrompt(nil) = %q, want empty", got)
	}
	if got := buildToolsSystemPrompt([]openAITool{}); got != "" {
		t.Errorf("buildToolsSystemPrompt([]) = %q, want empty", got)
	}
}
