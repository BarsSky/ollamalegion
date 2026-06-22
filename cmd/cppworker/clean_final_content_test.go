package main

import (
	"strings"
	"testing"
)

// TestCleanFinalContent_EmptyInput — пустой вход → пустой выход.
func TestCleanFinalContent_EmptyInput(t *testing.T) {
	if got := cleanFinalContent(""); got != "" {
		t.Errorf("cleanFinalContent(\"\") = %q, want \"\"", got)
	}
}

// TestCleanFinalContent_PlainText — обычный текст без служебных токенов
// должен пройти без изменений (с trim whitespace по краям).
func TestCleanFinalContent_PlainText(t *testing.T) {
	input := "This is a normal text response from the model to a user question."
	want := input
	if got := cleanFinalContent(input); got != want {
		t.Errorf("cleanFinalContent plain text:\n got  %q\n want %q", got, want)
	}
}

// TestCleanFinalContent_TrimWhitespace — пробелы и переносы строк по краям
// должны удаляться.
func TestCleanFinalContent_TrimWhitespace(t *testing.T) {
	input := "\n\n  Hello world  \n\n"
	want := "Hello world"
	if got := cleanFinalContent(input); got != want {
		t.Errorf("cleanFinalContent trim whitespace:\n got  %q\n want %q", got, want)
	}
}

// TestCleanFinalContent_RemoveTrailingToolCall — модель иногда эмитит
// </tool_call> в конце plain-text ответа (артефакт из-за tool definitions
// в system prompt). Должно быть удалено.
func TestCleanFinalContent_RemoveTrailingToolCall(t *testing.T) {
	input := "Я нашёл информацию по вашему вопросу.</tool_call>"
	want := "Я нашёл информацию по вашему вопросу."
	if got := cleanFinalContent(input); got != want {
		t.Errorf("cleanFinalContent remove </tool_call>:\n got  %q\n want %q", got, want)
	}
}

// TestCleanFinalContent_RemoveTrailingPythonTag — Llama-3 формат
// <|python_tag|> в конце ответа.
func TestCleanFinalContent_RemoveTrailingPythonTag(t *testing.T) {
	input := "Ответ на запрос пользователя.<|python_tag|>"
	want := "Ответ на запрос пользователя."
	if got := cleanFinalContent(input); got != want {
		t.Errorf("cleanFinalContent remove <|python_tag|>:\n got  %q\n want %q", got, want)
	}
}

// TestCleanFinalContent_RemoveTrailingMistralMarker — Mistral Nemo формат
// [TOOL_CALLS] в конце ответа.
func TestCleanFinalContent_RemoveTrailingMistralMarker(t *testing.T) {
	input := "Some text.[TOOL_CALLS]"
	want := "Some text."
	if got := cleanFinalContent(input); got != want {
		t.Errorf("cleanFinalContent remove [TOOL_CALLS]:\n got  %q\n want %q", got, want)
	}
}

// TestCleanFinalContent_RemoveTrailingGemmaMarkers — Gemma формат
// <start_of_turn>model в конце ответа (без закрывающего, что тоже артефакт).
func TestCleanFinalContent_RemoveTrailingGemmaMarkers(t *testing.T) {
	input := "Final answer text.<start_of_turn>model"
	want := "Final answer text."
	if got := cleanFinalContent(input); got != want {
		t.Errorf("cleanFinalContent remove gemma marker:\n got  %q\n want %q", got, want)
	}
}

// TestCleanFinalContent_RemoveMultipleTrailingTokens — несколько токенов подряд
// должны удаляться все (цикл).
func TestCleanFinalContent_RemoveMultipleTrailingTokens(t *testing.T) {
	input := "Real answer.</tool_call><|python_tag|>[TOOL_CALLS]"
	want := "Real answer."
	if got := cleanFinalContent(input); got != want {
		t.Errorf("cleanFinalContent remove multiple tokens:\n got  %q\n want %q", got, want)
	}
}

// TestCleanFinalContent_PreservesInternalToolMarkers — служебные токены
// В СЕРЕДИНЕ текста не должны удаляться (только хвостовые).
func TestCleanFinalContent_PreservesInternalToolMarkers(t *testing.T) {
	input := "First part.</tool_call> Second part."
	// Внутренний </tool_call> остаётся — мы чистим только хвостовые.
	want := "First part.</tool_call> Second part."
	if got := cleanFinalContent(input); got != want {
		t.Errorf("cleanFinalContent preserve internal tokens:\n got  %q\n want %q", got, want)
	}
}

// TestCleanFinalContent_RealisticOpenWebUIResponse — реалистичный ответ
// модели, которая сначала "думала" про tools, но не стала их вызывать.
// Используем ASCII чтобы избежать проблем с кодировкой тест-файла.
func TestCleanFinalContent_RealisticOpenWebUIResponse(t *testing.T) {
	input := "Based on the search results, weather in Moscow is +15C with partly cloudy skies. Wind SW at 5 m/s.</tool_call>"
	want := "Based on the search results, weather in Moscow is +15C with partly cloudy skies. Wind SW at 5 m/s."
	got := cleanFinalContent(input)
	t.Logf("input length=%d, got length=%d", len(input), len(got))
	t.Logf("input last 30 bytes: %q", input[len(input)-30:])
	t.Logf("got last 30 bytes: %q", got[len(got)-30:])
	if got != want {
		t.Errorf("cleanFinalContent realistic case:\n got  %q\n want %q", got, want)
	}
}

// TestCleanFinalContent_DoesNotModifyNonTrailingBrace — скобки в середине
// текста не должны затрагиваться.
func TestCleanFinalContent_DoesNotModifyNonTrailingBrace(t *testing.T) {
	input := "Array example: [1, 2, 3]"
	want := "Array example: [1, 2, 3]"
	if got := cleanFinalContent(input); got != want {
		t.Errorf("cleanFinalContent preserve internal braces:\n got  %q\n want %q", got, want)
	}
}

// Benchmark для проверки производительности на реалистичном ответе.
func BenchmarkCleanFinalContent_Realistic(b *testing.B) {
	input := "Это ответ модели с длинным текстом, который занимает несколько сотен токенов.</tool_call>"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = cleanFinalContent(input)
	}
}

// TestTrailingToolTokens_AreTrimmedFromSuffix — все токены из списка должны
// успешно удаляться с суффикса (это unit test для самого списка).
func TestTrailingToolTokens_AreTrimmedFromSuffix(t *testing.T) {
	for _, tok := range trailingToolTokens {
		t.Run(strings.ReplaceAll(strings.ReplaceAll(tok, "<", "_"), ">", "_"), func(t *testing.T) {
			input := "Some content" + tok
			got := cleanFinalContent(input)
			if strings.HasSuffix(got, strings.TrimSpace(tok)) && tok != "" {
				t.Errorf("token %q not removed: result has suffix %q", tok, got)
			}
		})
	}
}