// nonstream_visible_r430_test.go — R430 (2026-11-04): НЕпотоковые поверхности
// (/api/chat stream:false и OpenAI) тоже не отдают разметку вызова в content.
//
// ЖИВОЙ ДЕФЕКТ (echo-проба через балансер, gemma-4): модель вернула в content
// `[[{\"function\":{\"name\":\"editor\",\"arguments\":…` — видимый ответ содержал
// переэкранированный JSON вызова, вызова при этом не было (tool_calls=0).
package main

import (
	"strings"
	"testing"
)

// TestR430_NonStreamContentCutsEscapedMarkup — content = проза + пояснение, без
// ключей вызова и без HTML.
func TestR430_NonStreamContentCutsEscapedMarkup(t *testing.T) {
	content, unparsed := nonStreamVisibleContentR430(fixtureEscapedCallR430)
	if !unparsed {
		t.Fatal("разметка не опознана — клиент получит её в content")
	}
	for _, leak := range []string{`\"function\"`, `\"name\"`, "DOCTYPE", "editor"} {
		if strings.Contains(content, leak) {
			t.Fatalf("в content осталась разметка вызова (%q): %q", leak, content)
		}
	}
	if !strings.Contains(content, "Starting with **index.html**") {
		t.Fatalf("проза перед вызовом потеряна: %q", content)
	}
	if !strings.Contains(content, unparsedToolCallNoticeR430) {
		t.Fatalf("нет пояснения «вызов не выполнен»: %q", content)
	}
}

// TestR430_NonStreamContentPlainText — обычный текстовый ответ не трогаем:
// ни реза, ни пояснения (иначе клиент получает ложное «вызов не выполнен»).
func TestR430_NonStreamContentPlainText(t *testing.T) {
	const plain = "Файлы созданы: index.html, styles.css.\r\n\r\nДальше проверю сборку."
	content, unparsed := nonStreamVisibleContentR430(plain)
	if unparsed {
		t.Fatal("обычный текст принят за разметку вызова")
	}
	if content != plain {
		t.Fatalf("content изменён: %q", content)
	}
}

// TestR430_NonStreamContentMarkupOnly — вывода, кроме разметки, нет: клиенту
// уходит одно пояснение (и никогда — сам вызов).
func TestR430_NonStreamContentMarkupOnly(t *testing.T) {
	only := "<|tool_call>call:editor(new_text: \"<html>\", path: \"a\")}]"
	content, unparsed := nonStreamVisibleContentR430(only)
	if !unparsed {
		t.Fatal("разметка не опознана")
	}
	if content != unparsedToolCallNoticeR430 {
		t.Fatalf("content = %q, ожидалось пояснение", content)
	}
}
