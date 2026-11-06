// client_tool_guidance_r455_test.go — R455/R456/R457 (2026-11-06).
//
// Живой прогон (Cline + Qwen3.8-9B, стенд):
//   - команда `Get-ChildItem ... && Get-Content .gitignore` → exit 1
//     («Cannot find path»), модель повторила её ещё раз;
//   - затем модель выдала HTML-страницу ТЕКСТОМ (8587 символов, tool_calls=0).
//
// Подсказка R445 на эти два класса не срабатывала: она ищет только отказы
// клиента (`old_text was omitted`, `already exists`, `editor input too large`).
// Тесты фиксируют новые детекторы и границы, чтобы не начать «лечить» нормальные
// ответы (короткий пример кода в ответе — это не файл текстом).
package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func toolMsgR455(content string) chatMessage {
	return chatMessage{Role: "tool", Content: content}
}

func assistantCallR456(name, args string) chatMessage {
	return chatMessage{
		Role: "assistant",
		ToolCalls: []ollamaToolCall{{
			Function: ollamaToolCallFunction{Name: name, Arguments: json.RawMessage(args)},
		}},
	}
}

// TestR455_CommandFailureDetection — что считаем падением команды.
func TestR455_CommandFailureDetection(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{"exit code 1", "[Command exited with code 1]", true},
		{"exit code 0 — успех", "[Command exited with code 0]", false},
		{"exit code 127", "exit code: 127", true},
		{"файла нет", "Get-Content : Cannot find path 'D:\\x\\.gitignore' because it does not exist.", true},
		{"команда не найдена", "tree : The term 'tree' is not recognized as the name of a cmdlet", true},
		{"подсказка про term", "CommandNotFoundException: не найден", true},
		{"unix no such file", "cat: .gitignore: No such file or directory", true},
		{"обычный вывод", "Каталог: D:\\cline-e2e-web\n-a---- index.html", false},
		{"пусто", "", false},
	}
	for _, tc := range cases {
		if got := commandFailedR455(tc.content); got != tc.want {
			t.Errorf("%s: commandFailedR455 = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestR455_HintOnFailingCommand — подсказка появляется и не мешает успеху.
func TestR455_HintOnFailingCommand(t *testing.T) {
	broken := []chatMessage{
		{Role: "user", Content: "изучи проект"},
		assistantCallR456("run_commands", `{"commands":["dir"]}`),
		toolMsgR455("Get-Content : Cannot find path 'D:\\cline-e2e-web\\.gitignore'\n[Command exited with code 1]"),
	}
	hint := commandFailureHintR455(broken)
	if hint == "" {
		t.Fatal("после падения команды подсказка обязана появиться — иначе модель снова повторит вызов")
	}
	if !strings.Contains(hint, "Не повторяй") {
		t.Fatalf("подсказка не говорит, что делать: %q", hint)
	}

	ok := []chatMessage{
		assistantCallR456("run_commands", `{"commands":["dir"]}`),
		toolMsgR455("-a---- index.html\n[Command exited with code 0]"),
	}
	if hint := commandFailureHintR455(ok); hint != "" {
		t.Fatalf("успешная команда не должна давать подсказку: %q", hint)
	}
}

// TestR456_RepeatedCallDetection — повтор того же вызова с теми же аргументами.
func TestR456_RepeatedCallDetection(t *testing.T) {
	same := []chatMessage{
		assistantCallR456("run_commands", `{"commands":["dir && Get-Content .gitignore"]}`),
		toolMsgR455("[Command exited with code 1]"),
		assistantCallR456("run_commands", `{"commands":["dir && Get-Content .gitignore"]}`),
	}
	hint := repeatedToolCallHintR456(same)
	if hint == "" {
		t.Fatal("два одинаковых вызова подряд — это цикл, подсказка обязана появиться")
	}
	if !strings.Contains(hint, "Не повторяй") {
		t.Fatalf("подсказка не говорит, что делать: %q", hint)
	}

	// Пробелы внутри JSON не делают вызов другим.
	spaced := []chatMessage{
		assistantCallR456("run_commands", `{"commands": ["dir"]}`),
		assistantCallR456("run_commands", "{\n  \"commands\":[\"dir\"]\n}"),
	}
	if hint := repeatedToolCallHintR456(spaced); hint == "" {
		t.Fatal("различие только в пробелах не делает вызов новым")
	}

	different := []chatMessage{
		assistantCallR456("run_commands", `{"commands":["dir"]}`),
		assistantCallR456("read_files", `{"files":["index.html"]}`),
	}
	if hint := repeatedToolCallHintR456(different); hint != "" {
		t.Fatalf("разные вызовы — не цикл: %q", hint)
	}
}

// TestR457_FileBodyDetection — «тело файла» против обычного ответа.
func TestR457_FileBodyDetection(t *testing.T) {
	longHTML := "<!DOCTYPE html>\n<html lang=\"ru\"><head><title>x</title></head><body>" +
		strings.Repeat("<p>контент</p>", 200) + "</body></html>"
	if !looksLikeFileBodyR457(longHTML) {
		t.Fatal("HTML-документ целиком обязан распознаваться")
	}

	fenced := "```html\n" + strings.Repeat("<div>x</div>\n", 200) + "```"
	if !looksLikeFileBodyR457(fenced) {
		t.Fatal("длинный код в ограде с начала ответа — тоже файл текстом")
	}

	short := "<!DOCTYPE html><html></html>"
	if looksLikeFileBodyR457(short) {
		t.Fatal("короткий пример не повод вмешиваться")
	}

	prose := strings.Repeat("Проект состоит из нескольких файлов. ", 100)
	if looksLikeFileBodyR457(prose) {
		t.Fatal("обычный текст не должен распознаваться как файл")
	}
}

// TestR457_HintOnFileBodyAnswer — подсказка на следующий ход.
func TestR457_HintOnFileBodyAnswer(t *testing.T) {
	longHTML := "<!DOCTYPE html>\n<html><body>" + strings.Repeat("<p>x</p>", 300) + "</body></html>"
	msgs := []chatMessage{
		{Role: "user", Content: "предложи оптимизации"},
		{Role: "assistant", Content: "Вот оптимизированная страница:\n\n" + longHTML},
	}
	hint := fileBodyAnswerHintR457(msgs)
	if hint == "" {
		t.Fatal("файл текстом в предыдущем ответе — нужна инструкция на следующий ход")
	}
	if !strings.Contains(hint, "инструментом") {
		t.Fatalf("подсказка не говорит, что делать: %q", hint)
	}

	// Если в ответе был вызов — это нормальный путь, подсказки нет.
	withCall := []chatMessage{
		{Role: "assistant", Content: "Создаю файл.", ToolCalls: []ollamaToolCall{{}}},
	}
	if hint := fileBodyAnswerHintR457(withCall); hint != "" {
		t.Fatalf("вызов инструмента — не повод для подсказки: %q", hint)
	}
}

// TestR455Guidance_Merged — один блок на запрос, все найденные причины видны.
func TestR455Guidance_Merged(t *testing.T) {
	longHTML := "<!DOCTYPE html>\n<html><body>" + strings.Repeat("<p>x</p>", 300) + "</body></html>"
	msgs := []chatMessage{
		{Role: "user", Content: "изучи проект"},
		assistantCallR456("run_commands", `{"commands":["dir"]}`),
		toolMsgR455("Cannot find path 'D:\\x\\.gitignore'\n[Command exited with code 1]"),
		{Role: "assistant", Content: longHTML},
	}
	hint := clientToolGuidanceHint(msgs)
	if hint == "" {
		t.Fatal("ожидался блок инструкций")
	}
	for _, want := range []string{"R455:", "R457:", "Инструкции по инструментам"} {
		if !strings.Contains(hint, want) {
			t.Fatalf("в блоке нет %q: %q", want, hint)
		}
	}
	if len(hint) > 1200 {
		t.Fatalf("блок слишком длинный (%d) — он уходит в каждый запрос: %q", len(hint), hint)
	}

	// Пустая история — пустой блок (никакого шума в промпте).
	if hint := clientToolGuidanceHint(nil); hint != "" {
		t.Fatalf("пустая история не должна давать инструкций: %q", hint)
	}
}
