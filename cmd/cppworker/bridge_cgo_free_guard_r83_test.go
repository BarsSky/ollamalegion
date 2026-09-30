//go:build llama_stub

// bridge_cgo_free_guard_r83_test.go — R83-fix (2026-09-30): статическая защита
// от повторения double free в c/bridge.
//
// Живой дефект (лог локального стенда 2026-09-30 14:14:22, gemma-4):
//
//	double free or corruption (out)
//	SIGABRT: abort ... signal arrived during cgo execution
//	c/bridge.(*ModelHandle).ApplyChatTemplateWithThinking.func2.func7()
//	    c/bridge/bridge.go:1390   ← defer C.free(unsafe.Pointer(outBuf))
//	cmd/cppworker.buildChatPromptWithOptions → handleChat → /api/chat
//
// Причина: буфер растёт через C.realloc (ret == -4 — отрендеренный prompt не влез
// в 64 KB; обычный Cline/OpenWebUI-запрос с system-промптом и tools[]), а
// `defer C.free(unsafe.Pointer(outBuf))` запоминает ЗНАЧЕНИЕ указателя в момент
// defer. После переезда блока free освобождал уже освобождённую память, glibc
// звал abort(), и падал весь процесс cppworker (recover() в handlers_chat.go
// process-level SIGABRT не ловит). Клиент получал connection refused.
//
// Тест не требует llama.h (bridge.go с CGo собирается отдельно), поэтому
// проверяет исходник: если переменная переприсваивается из C.realloc, её НЕЛЬЗЯ
// освобождать через defer со значением — только через defer-замыкание, читающее
// актуальный указатель.
package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// reallocAssignRe — `X = C.realloc(unsafe.Pointer(X), ...)`.
var reallocAssignRe = regexp.MustCompile(`(\w+)\s*:?=\s*C\.realloc\(unsafe\.Pointer\((\w+)\)`)

// stripLineComments убирает строки-комментарии: в doc-комментариях эти же
// выражения упоминаются как «так делать нельзя», и наивная проверка по
// подстроке ловила бы сам комментарий.
func stripLineComments(text string) string {
	var b strings.Builder
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// TestR83Fix_NoDeferredFreeOfReallocedPointer — главная проверка.
func TestR83Fix_NoDeferredFreeOfReallocedPointer(t *testing.T) {
	path := filepath.Join("..", "..", "c", "bridge", "bridge.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("не удалось прочитать %s: %v", path, err)
	}
	text := stripLineComments(string(src))

	checked := 0
	for _, m := range reallocAssignRe.FindAllStringSubmatch(text, -1) {
		dst, srcVar := m[1], m[2]
		// Интересует только рост СВОЕГО буфера (realloc того же указателя):
		// это тот случай, когда старый блок уже освобождён realloc'ом.
		if dst != srcVar {
			continue
		}
		checked++
		bad := "defer C.free(unsafe.Pointer(" + dst + "))"
		if strings.Contains(text, bad) {
			t.Errorf("найден `%s` для указателя, который переприсваивается из C.realloc "+
				"(%s): после переезда блока этот free освободит уже освобождённую память — "+
				"glibc даст «double free or corruption» и SIGABRT всего cppworker. "+
				"Освобождайте актуальный указатель через defer-замыкание (см. "+
				"ApplyChatTemplateWithThinking).", bad, dst)
		}
	}
	if checked == 0 {
		t.Log("в c/bridge/bridge.go нет realloc'ов «в себя» — проверять нечего " +
			"(паттерн мог быть удалён целиком, это тоже корректно)")
	}
}

// TestR83Fix_ApplyChatTemplateWithThinkingFreeIsClosure — конкретная регрессия
// на место падения: освобождение буфера должно быть замыканием.
func TestR83Fix_ApplyChatTemplateWithThinkingFreeIsClosure(t *testing.T) {
	path := filepath.Join("..", "..", "c", "bridge", "bridge.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("не удалось прочитать %s: %v", path, err)
	}
	text := stripLineComments(string(src))

	idx := strings.Index(text, "func (m *ModelHandle) ApplyChatTemplateWithThinking(")
	if idx < 0 {
		t.Fatal("ApplyChatTemplateWithThinking не найдена — тест устарел, поправьте его")
	}
	body := text[idx:]
	if end := strings.Index(body[1:], "\nfunc "); end > 0 {
		body = body[:end+1]
	}

	if !strings.Contains(body, "defer func() {") {
		t.Error("в ApplyChatTemplateWithThinking нет defer-замыкания для free — " +
			"вернулся паттерн, на котором cppworker падал по double free")
	}
	if strings.Contains(body, "defer C.free(unsafe.Pointer(outBuf))") {
		t.Error("вернулся `defer C.free(unsafe.Pointer(outBuf))` — double free после realloc")
	}
}
