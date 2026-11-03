// tools_terminal_reason_r394_test.go — R394 (2026-11-04).
//
// ЖИВАЯ ЖАЛОБА (A10, Cline через балансер, /api/chat + tools): «флаг
// завершения проскакивает раньше, чем приходит настоящий ответ». Лог воркера
// показал: done_reason=stop при 456 байтах видимого ответа и raw_len=15812 при
// клиентском num_predict=4096, то есть ответ был ОБРЕЗАН по бюджету, а клиенту
// сообщили «модель закончила» — и он закрыл ход.
package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestR394_TerminalReasonContract — причина остановки по контракту Ollama.
func TestR394_TerminalReasonContract(t *testing.T) {
	cases := []struct {
		name      string
		output    string
		visible   string
		prompt    string
		toolCalls int
		nPredict  int
		generated int
		want      string
	}{
		{
			name:      "вызов инструмента — tool_calls",
			output:    `{"name":"read_file","arguments":{"path":"a"}}`,
			toolCalls: 1,
			nPredict:  4096,
			generated: 4096,
			want:      "tool_calls",
		},
		{
			name:      "законченный ответ — stop",
			output:    "<think>посчитаем</think>Всего 15 яблок.",
			visible:   "Всего 15 яблок.",
			nPredict:  4096,
			generated: 120,
			want:      "stop",
		},
		{
			// Тот самый живой случай: блок размышлений ЗАКРЫТ, видимый ответ
			// начался и оборвался на клиентском лимите. R320 давал здесь "stop"
			// (его правило ловит только незакрытый reasoning), из-за чего Cline
			// считал ход законченным и продолжения не было.
			name:      "бюджет n_predict исчерпан — length, а не stop",
			output:    "<think>длинные размышления</think>Начало ответа, который обрывается",
			visible:   "Начало ответа, который обрывается",
			nPredict:  4096,
			generated: 4096,
			want:      "length",
		},
		{
			name:      "незакрытый reasoning — length (правило R320)",
			output:    "<think>Сначала посчитаем: 3 * 4 = 12, потом",
			nPredict:  4096,
			generated: 900,
			want:      "length",
		},
		{
			// n_predict <= 0 = лимита нет: усечения по бюджету быть не может,
			// поэтому даже «много токенов» не повод объявлять length.
			name:      "лимита клиента нет — stop",
			output:    "Короткий, но законченный ответ.",
			visible:   "Короткий, но законченный ответ.",
			nPredict:  0,
			generated: 5000,
			want:      "stop",
		},
		{
			name:      "пустой вывод без размышлений — stop (решает ветка ошибки)",
			output:    "   ",
			prompt:    "user: задача",
			nPredict:  16,
			generated: 0,
			want:      "stop",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := toolsTerminalReasonR394(tc.output, tc.visible, tc.prompt, tc.toolCalls, tc.nPredict, tc.generated)
			if got != tc.want {
				t.Errorf("toolsTerminalReasonR394 = %q, want %q (контракт Ollama: stop|length|tool_calls)", got, tc.want)
			}
		})
	}
}

// TestR394_BudgetExhausted — граница детектора бюджета.
func TestR394_BudgetExhausted(t *testing.T) {
	if !toolsBudgetExhaustedR394(4096, 4096) {
		t.Error("сгенерировано ровно n_predict — бюджет исчерпан")
	}
	if !toolsBudgetExhaustedR394(4096, 4100) {
		t.Error("сгенерировано больше n_predict — бюджет тем более исчерпан")
	}
	if !toolsBudgetExhaustedR394(4096, 4095) {
		// R409: живой лог A10 (27B, Cline): n_predict=4096, generated_tokens=4095,
		// done_reason=stop на обрезанном ответе. Детокенизация теряет последний
		// (EOS/служебный) токен, поэтому допуск -1 обязателен.
		t.Error("на один токен меньше лимита при упоре в кап — это length (R409)")
	}
	if toolsBudgetExhaustedR394(4096, 4094) {
		t.Error("на два токена меньше лимита — законный stop, а не length")
	}
	if toolsBudgetExhaustedR394(0, 5000) {
		t.Error("n_predict=0 означает «лимита нет»: length объявлять нельзя")
	}
	if toolsBudgetExhaustedR394(-1, 5000) {
		t.Error("n_predict=-1 (Ollama: без лимита) не должен давать length")
	}
	if toolsBudgetExhaustedR394(4096, 0) {
		t.Error("ноль сгенерированных токенов не бывает усечением по бюджету")
	}
}

// TestR394_NoticeWhenReasoningHidden — пояснение обязано появляться и при
// think=false, когда размышления клиенту не отдаются.
//
// R320 ставил пояснение только в ветке `reasoningOut != ""`. При think=false
// reasoningOut пуст, и клиент получал content:"" + done_reason:stop — «молчание»,
// от которого R314 защищает остальные поверхности.
func TestR394_NoticeWhenReasoningHidden(t *testing.T) {
	raw := "<think>долго думаем над задачей, бюджет кончается</think>"
	got := toolsTerminalContentR394("", raw)
	if got == "" {
		t.Fatal("видимого ответа нет, размышления были (скрыты think=false) — обязано быть пояснение")
	}
	if strings.Contains(got, "<think>") || strings.Contains(got, "долго думаем") {
		t.Errorf("пояснение не должно пересказывать скрытые размышления: %q", got)
	}

	if got := toolsTerminalContentR394("Всего 15 яблок.", raw); got != "Всего 15 яблок." {
		t.Errorf("непустой ответ менять нельзя, получено %q", got)
	}
	if got := toolsTerminalContentR394("", "просто текст без размышлений"); got != "" {
		t.Errorf("размышлений не было — подставлять пояснение нечего, получено %q", got)
	}
	if got := toolsTerminalContentR394("   ", ""); got != "   " {
		t.Errorf("пусто и без размышлений — оставляем как есть, получено %q", got)
	}
}

// TestR394_ReasoningCharsRaw — счётчик размышлений не зависит от политики
// отдачи (think=false не должен обнулять признак «размышления были»).
func TestR394_ReasoningCharsRaw(t *testing.T) {
	if got := reasoningCharsRawR394("<think>раз</think>два</think>"); got == 0 {
		t.Error("размышления есть — счётчик обязан быть > 0")
	}
	if got := reasoningCharsRawR394("просто ответ"); got != 0 {
		t.Errorf("размышлений нет — счётчик обязан быть 0, получено %d", got)
	}
	if got := reasoningCharsRawR394(""); got != 0 {
		t.Errorf("пустой вывод — 0, получено %d", got)
	}
}

// TestR394_ToolsPathUsesBudgetAwareReason — стюард: терминальные кадры
// tools-пути обязаны считаться через R394, иначе дефект вернётся молча.
//
// AST, а не поиск подстроки: `toolsStopReasonR320` упоминается в комментариях
// (там, где объясняется, почему хелпер заменён), и поиск по тексту давал бы
// ложные срабатывания. Проверяем ровно одно: внутри функции
// writeChatStreamResponseWithTools нет ВЫЗОВОВ toolsStopReasonR320, а вызовы
// toolsTerminalReasonR394/toolsTerminalContentR394 есть.
func TestR394_ToolsPathUsesBudgetAwareReason(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "handlers_chat.go", nil, 0)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skip("handlers_chat.go не найден в рабочем каталоге")
		}
		t.Fatalf("разбор handlers_chat.go: %v", err)
	}

	found := false
	reasonCalls, contentCalls := 0, 0
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name == nil || fn.Name.Name != "writeChatStreamResponseWithTools" {
			return true
		}
		found = true
		ast.Inspect(fn.Body, func(inner ast.Node) bool {
			call, ok := inner.(*ast.CallExpr)
			if !ok {
				return true
			}
			ident, ok := call.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			switch ident.Name {
			case "toolsStopReasonR320":
				t.Errorf("строка %d: прямой вызов toolsStopReasonR320 — причина остановки "+
					"не учитывает исчерпанный n_predict, клиент получит stop вместо length (R394)",
					fset.Position(call.Pos()).Line)
			case "toolsTerminalReasonR394":
				reasonCalls++
			case "toolsTerminalContentR394":
				contentCalls++
			case "terminalContentR417":
				// R417 (2026-11-03): единая обёртка, которая НЕ подставляет
				// пояснение, когда распознан tool_calls.
				contentCalls++
			}
			return true
		})
		return false
	})
	if !found {
		t.Fatal("writeChatStreamResponseWithTools не найдена — стюард не может проверить путь, обновите тест")
	}
	if reasonCalls < 3 {
		t.Errorf("вызовов toolsTerminalReasonR394 = %d, want >= 3 (нераспознанная разметка, пустой видимый вывод, основной финальный кадр)", reasonCalls)
	}
	if contentCalls < 1 {
		t.Error("нет вызова toolsTerminalContentR394: при think=false пояснение вместо пустого ответа не подставляется")
	}
}
