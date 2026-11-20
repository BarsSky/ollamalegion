// cold_load_wiring_r659_test.go — R659 (2026-11-20): стюард ПОДКЛЮЧЕНИЯ.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ТЕСТ. Функции R659 можно написать правильно и не позвать
// ниоткуда — ровно так «загрузка вместо очереди» R552 уже жила в 3b и не мешала
// заявке стоять 300 с: выбор узла срабатывал каждые 250 мс, а загрузку никто не
// начинал. Поэтому проверяется не поведение функции, а ФАКТ её вызова в двух
// местах, где заявка узнаёт, что слот не получен:
//
//  1. proxy.go — (*Proxy).ServeHTTP: ДО единой очереди
//     (waitForInferenceBackendWithMoveR480) и ПОСЛЕ цикла по прогретым копиям,
//     то есть в ветке «слот не взят»;
//  2. unified_queue_gate.go — (*Proxy).waitForInferenceBackendR480: в самой
//     очереди (пока ждём, узел мог освободиться) и ДО постановки в очередь.
package balancer

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// r659CallLinesIn — строки всех вызовов метода с именем name внутри функции fn
// файла filename (AST, а не поиск подстроки: строка в комментарии не считается).
func r659CallLinesIn(t *testing.T, filename, fnName, method string) []int {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, nil, 0)
	if err != nil {
		t.Fatalf("разбор %s: %v", filename, err)
	}
	var found bool
	var lines []int
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name == nil || fn.Name.Name != fnName || fn.Recv == nil || fn.Body == nil {
			return true
		}
		found = true
		ast.Inspect(fn.Body, func(inner ast.Node) bool {
			call, ok := inner.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == method {
				lines = append(lines, fset.Position(call.Pos()).Line)
			}
			return true
		})
		return false
	})
	if !found {
		t.Fatalf("%s не найдена в %s — стюард не может проверить путь, обновите тест", fnName, filename)
	}
	return lines
}

// r659FirstCallLineIn — строка первого вызова метода name в функции fn.
func r659FirstCallLineIn(t *testing.T, filename, fnName, method string) int {
	t.Helper()
	lines := r659CallLinesIn(t, filename, fnName, method)
	if len(lines) == 0 {
		return 0
	}
	return lines[0]
}

// TestR659_ServeHTTPStartsColdLoadBeforeQueue — заявка, у которой слот не взят,
// запускает загрузку ДО постановки в очередь.
func TestR659_ServeHTTPStartsColdLoadBeforeQueue(t *testing.T) {
	starts := r659CallLinesIn(t, "proxy.go", "ServeHTTP", "maybeStartColdLoadR659")
	if len(starts) == 0 {
		t.Fatal("(*Proxy).ServeHTTP не запускает «загрузку вместо ожидания»: заявка снова " +
			"будет ждать слот, пока свободные узлы простаивают (живой дефект 2026-11-20)")
	}
	queue := r659CallLinesIn(t, "proxy.go", "ServeHTTP", "waitForInferenceBackendWithMoveR480")
	if len(queue) == 0 {
		t.Fatal("в ServeHTTP не найден вызов единой очереди — разметка функции изменилась")
	}
	if starts[0] > queue[0] {
		t.Errorf("загрузка инициируется (строка %d) ПОСЛЕ постановки в очередь (строка %d): "+
			"пока ждём слот, ёмкость не создаётся", starts[0], queue[0])
	}
}

// TestR659_QueueStartsColdLoadWhileWaiting — очередь тоже обязана пробовать
// загрузку: между итерациями ожидания слот на свободном узле мог освободиться.
func TestR659_QueueStartsColdLoadWhileWaiting(t *testing.T) {
	starts := r659CallLinesIn(t, "unified_queue_gate.go", "waitForInferenceBackendR480", "maybeStartColdLoadR659")
	if len(starts) < 2 {
		t.Fatalf("в waitForInferenceBackendR480 вызовов «начать загрузку»: %d, ожидалось >= 2 "+
			"(до постановки в очередь и в цикле ожидания)", len(starts))
	}
}

// TestR659_ColdLoadKeepsWindowRule — R659 не имеет права трогать правило окна:
// загрузка идёт на «нужном окне» (need), а не на клиентском num_ctx и не на
// дефолте профиля. Иначе это было бы сжатие окна под живым клиентом (R294/R459).
func TestR659_ColdLoadKeepsWindowRule(t *testing.T) {
	lines := r659CallLinesIn(t, "cold_load_placement_r659.go", "startColdLoadR659", "warmupModel")
	if len(lines) != 1 {
		t.Fatalf("startColdLoadR659 обязан запускать загрузку ровно одним вызовом warmupModel, найдено %d", len(lines))
	}
	// И в теле старта окно обязано уезжать двумя полями: NumCtx (старые воркеры,
	// total) и ContextPerSeq (окно на клиента — R83).
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "cold_load_placement_r659.go", nil, 0)
	if err != nil {
		t.Fatalf("разбор cold_load_placement_r659.go: %v", err)
	}
	var fields []string
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name == nil || fn.Name.Name != "startColdLoadR659" {
			return true
		}
		ast.Inspect(fn.Body, func(inner ast.Node) bool {
			kv, ok := inner.(*ast.KeyValueExpr)
			if !ok {
				return true
			}
			if key, ok := kv.Key.(*ast.Ident); ok {
				fields = append(fields, key.Name)
			}
			return true
		})
		return false
	})
	has := map[string]bool{}
	for _, f := range fields {
		has[f] = true
	}
	if !has["NumCtx"] || !has["ContextPerSeq"] {
		t.Fatalf("загрузка обязана задавать окно явно (NumCtx + ContextPerSeq), найдено %v — "+
			"иначе воркер поднимет модель с дефолтным окном", fields)
	}
}
