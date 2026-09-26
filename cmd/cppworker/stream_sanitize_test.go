//go:build llama_stub

// stream_sanitize_test.go — R83 (2026-09-26).
//
// Живой инцидент: длинная сессия на 8 GB GPU, клиент показал
// «Invalid control character at: line 1 column 73 (char 72)». Симптом приходит
// от JSON-парсера клиента, значит в тексте, который он парсит, оказался сырой
// управляющий символ. Наш NDJSON его пропустить не может (json.Marshal
// экранирует), но текст модели клиент парсит и сам — артефакты, аргументы
// tool-call, «JSON mode». Поэтому чистим на выходе из cppworker.
package main

import (
	"strings"
	"testing"
)

// TestSanitizeStreamText_RemovesControlChars — C0-контролы и DEL вырезаются,
// структурные \n \r \t остаются.
func TestSanitizeStreamText_RemovesControlChars(t *testing.T) {
	in := "line1\x00line2\x07line3\x1b[0m\x7f"
	got := sanitizeStreamText(in)
	if strings.ContainsAny(got, "\x00\x07\x1b\x7f") {
		t.Errorf("управляющие символы остались: %q", got)
	}
	if got != "line1 line2 line3 [0m " {
		t.Errorf("got %q", got)
	}
}

// TestSanitizeStreamText_KeepsStructuralWhitespace — перевод строки, возврат
// каретки и табуляция не трогаются: их несут markdown и код модели.
func TestSanitizeStreamText_KeepsStructuralWhitespace(t *testing.T) {
	in := "int main() {\n\treturn 0;\r\n}"
	if got := sanitizeStreamText(in); got != in {
		t.Errorf("структурные пробелы испорчены: %q → %q", in, got)
	}
}

// TestSanitizeStreamText_FastPathNoAlloc — если контролов нет, строка
// возвращается тем же значением (быстрый путь без пересборки).
func TestSanitizeStreamText_FastPathNoAlloc(t *testing.T) {
	in := "обычный текст без контролов 👍"
	if got := sanitizeStreamText(in); got != in {
		t.Errorf("текст изменён: %q → %q", in, got)
	}
	if sanitizeStreamText("") != "" {
		t.Error("пустая строка должна остаться пустой")
	}
}

// TestSanitizeStreamText_DoesNotTouchUTF8 — многобайтные руны не разрезаются:
// чистим только однобайтовые C0/DEL, поэтому UTF-8 остаётся валидным.
func TestSanitizeStreamText_DoesNotTouchUTF8(t *testing.T) {
	in := "Рассуждаю\x01 14 минут → код"
	got := sanitizeStreamText(in)
	if !strings.HasPrefix(got, "Рассуждаю ") {
		t.Errorf("UTF-8 повреждён: %q", got)
	}
	if strings.Contains(got, "\x01") {
		t.Errorf("контрол остался: %q", got)
	}
	if !strings.Contains(got, "→") {
		t.Errorf("стрелка потерялась: %q", got)
	}
}

// TestSanitizeStreamText_RealWorldCodeBlock — содержимое блока кода из инцидента
// (то, что видно в скриншоте) не меняется: там нет управляющих символов.
func TestSanitizeStreamText_RealWorldCodeBlock(t *testing.T) {
	in := "#include <iostream>\n#include <vector>\n\nint main() {\n    std::vector<int> nums = {1, 2, 3, 4, 5};\n"
	if got := sanitizeStreamText(in); got != in {
		t.Errorf("блок кода изменён:\n%q\n→\n%q", in, got)
	}
}
