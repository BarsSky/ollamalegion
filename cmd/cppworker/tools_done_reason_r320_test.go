// tools_done_reason_r320_test.go — R320 (2026-10-30).
//
// ЖИВАЯ ЖАЛОБА (Cline через балансер, /api/chat + tools): «приходит завершение
// прямо посреди рассуждений модели, обрубая всё, что приходит после», а следующий
// запрос «продолжи» клиент помечает как ответ, хотя это размышление.
//
// ПРИЧИНА НА НАШЕЙ СТОРОНЕ. На tools-пути (`writeChatStreamResponseWithTools`,
// именно им ходит Cline: он всегда прикладывает tools[]) `done_reason`
// выставлялся ЖЁСТКО в "stop" — и в ветке tool_calls, и в ветке текстового
// ответа. Пока модель успевала закрыть блок размышлений, это было незаметно. Но
// когда бюджет вывода заканчивался ВНУТРИ размышлений, клиент получал
// done:true + done_reason:"stop" при пустом content: по контракту Ollama «stop»
// значит «модель закончила», поэтому клиент закрывал ход, а остаток ответа
// терялся.
//
// Эти тесты фиксируют правило причины остановки и то, что на tools-пути больше
// нет жёстко прошитого "stop" (иначе дефект вернётся молча).
package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestR320_ToolsStopReasonContract — значения done_reason по контракту Ollama.
func TestR320_ToolsStopReasonContract(t *testing.T) {
	cases := []struct {
		name      string
		output    string
		visible   string
		prompt    string
		toolCalls int
		want      string
	}{
		{
			name:      "вызов инструмента — tool_calls",
			output:    `{"name":"read_file","arguments":{"path":"a"}}`,
			toolCalls: 1,
			want:      "tool_calls",
		},
		{
			name:    "обычный завершённый ответ — stop",
			output:  "Всего 15 яблок. Ответ: 15.",
			visible: "Всего 15 яблок. Ответ: 15.",
			prompt:  "Сколько яблок?",
			want:    "stop",
		},
		{
			name:   "открывающий маркер в выводе не закрыт и ответа нет — length",
			output: "<think>Сначала посчитаем: 3 * 4 = 12, потом",
			want:   "length",
		},
		{
			name:    "маркер закрыт — stop",
			output:  "<think>посчитаем</think>Всего 15 яблок.",
			visible: "Всего 15 яблок.",
			want:    "stop",
		},
		{
			name:   "блок открыт ШАБЛОНОМ в промпте, ответа нет — length",
			output: "Сначала посчитаем: 3 * 4 = 12, потом",
			prompt: "user: задача<|channel>thought\n",
			want:   "length",
		},
		{
			name:    "блок открыт шаблоном и модель дала ответ — stop",
			output:  "посчитаем<channel|>Всего 15 яблок.",
			visible: "Всего 15 яблок.",
			prompt:  "user: задача<|channel>thought\n",
			want:    "stop",
		},
		{
			name:   "пустой вывод — не размышления, stop",
			output: "   ",
			prompt: "user: задача<|channel>thought\n",
			want:   "stop",
		},
		{
			// R320.1: ровно та регрессия, на которую указал аудит. Шаблон открыл
			// канал размышлений в промпте (gemma-4), модель ответила нормально и
			// закрывающего тега не эмитила — прежнее правило объявляло length
			// ЛЮБОМУ такому ответу, и клиент (Cline) не мог закрыть ход.
			name:    "ответ есть, но закрывающего тега нет — stop, а не length",
			output:  "Думаю так: 12*12=144. Ответ: 144.",
			visible: "Думаю так: 12*12=144. Ответ: 144.",
			prompt:  "user: задача<|channel>thought\n",
			want:    "stop",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := toolsStopReasonR320(tc.output, tc.visible, tc.prompt, tc.toolCalls); got != tc.want {
				t.Errorf("toolsStopReasonR320 = %q, want %q (контракт Ollama: stop|length|tool_calls)", got, tc.want)
			}
		})
	}
}

// TestR320_ToolsReasoningUnclosed — разбор «вывод кончился внутри размышлений».
func TestR320_ToolsReasoningUnclosed(t *testing.T) {
	if !toolsReasoningUnclosedR320("<think>начало", "", "") {
		t.Error("открытый <think> без закрытия должен считаться незакрытым reasoning")
	}
	if toolsReasoningUnclosedR320("<think>начало</think>ответ", "ответ", "") {
		t.Error("закрытый блок — не незакрытый reasoning")
	}
	if toolsReasoningUnclosedR320("", "", "prompt<|channel>thought\n") {
		t.Error("пустой вывод нельзя называть незакрытым reasoning: размышлений не было вовсе")
	}
}

// TestR320_EmptyAnswerNoticeStillApplies — пояснение вместо пустого content.
//
// На tools-пути размышления уходят ОДНИМ финальным чанком, то есть клиент до
// конца не видел ничего. Пустой content там читается как «модель ответила
// пустотой», поэтому подставляем тот же текст, что и на остальных поверхностях
// (R314).
func TestR320_EmptyAnswerNoticeStillApplies(t *testing.T) {
	if got := emptyAnswerNoticeForNativeR314("", 317); got == "" {
		t.Fatal("пустой content при непустых размышлениях обязан получить пояснение")
	}
	if got := emptyAnswerNoticeForNativeR314("Всего 15 яблок.", 317); got != "" {
		t.Errorf("непустой ответ менять нельзя, получено: %q", got)
	}
}

// TestR320_ToolsPathHasNoHardcodedStop — стюард: причина остановки на tools-пути
// больше не прошита константой.
//
// ЗАЧЕМ AST, А НЕ ТЕКСТ. Строка `"done_reason": "stop"` встречается в файле
// легитимно (например, в пояснениях и в других путях), поэтому поиск по
// подстроке даёт ложные срабатывания. Проверяем ровно одно: внутри функции
// writeChatStreamResponseWithTools нет присваивания done_reason литералу "stop" и
// нет составного литерала с ключом done_reason и значением "stop".
func TestR320_ToolsPathHasNoHardcodedStop(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "handlers_chat.go", nil, 0)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skip("handlers_chat.go не найден в рабочем каталоге")
		}
		t.Fatalf("разбор handlers_chat.go: %v", err)
	}

	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name == nil || fn.Name.Name != "writeChatStreamResponseWithTools" {
			return true
		}
		found = true
		ast.Inspect(fn.Body, func(inner ast.Node) bool {
			switch node := inner.(type) {
			case *ast.KeyValueExpr:
				key, ok := node.Key.(*ast.BasicLit)
				if !ok || key.Kind != token.STRING || strings.Trim(key.Value, `"`) != "done_reason" {
					return true
				}
				val, ok := node.Value.(*ast.BasicLit)
				if ok && val.Kind == token.STRING && strings.Trim(val.Value, `"`) == "stop" {
					t.Errorf("tools-путь снова прошивает done_reason=\"stop\" (строка %d): "+
						"при обрезанном reasoning контракт Ollama требует \"length\", иначе клиент "+
						"считает ход законченным (живая жалоба R320)",
						fset.Position(node.Pos()).Line)
				}
			case *ast.AssignStmt:
				for i, lhs := range node.Lhs {
					idx, ok := lhs.(*ast.IndexExpr)
					if !ok {
						continue
					}
					key, ok := idx.Index.(*ast.BasicLit)
					if !ok || strings.Trim(key.Value, `"`) != "done_reason" {
						continue
					}
					if i >= len(node.Rhs) {
						continue
					}
					if val, ok := node.Rhs[i].(*ast.BasicLit); ok &&
						val.Kind == token.STRING && strings.Trim(val.Value, `"`) == "stop" {
						t.Errorf("tools-путь снова присваивает done_reason=\"stop\" (строка %d)",
							fset.Position(node.Pos()).Line)
					}
				}
			}
			return true
		})
		return false
	})
	if !found {
		t.Fatal("writeChatStreamResponseWithTools не найдена — стюард не может проверить путь, обновите тест")
	}
}
