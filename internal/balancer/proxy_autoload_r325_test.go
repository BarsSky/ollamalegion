// proxy_autoload_r325_test.go — R325 (2026-10-30).
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА: «запрос потерялся — крутится, на мониторе его нет;
// потом всё как раньше». Воспроизведено на стенде: холодный старт (модель
// выгружена) → generic-прокси путь отправлял запрос как есть → воркер грузил
// модель ВНУТРИ запроса и ~250 с не отдавал заголовки → балансер рвал соединение
// по first-byte (180 с) → клиент получал 503 «all backends failed», хотя загрузка
// шла нормально. Гейт автозагрузки (ensureModelLoadedOnBackend) был только в
// роутерных обработчиках handleChat/handleGenerate.
//
// Тест-стюард: generic-путь обязан содержать гейт, и гейт обязан стоять ДО выбора
// транспорта (иначе транспорту снова достанется незагруженная модель).
package balancer

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestR325_GenericProxyWaitsForModelLoad — AST-стюард по (*Proxy).proxyRequest.
func TestR325_GenericProxyWaitsForModelLoad(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "proxy_request.go", nil, 0)
	if err != nil {
		t.Fatalf("разбор proxy_request.go: %v", err)
	}

	var (
		foundFn     bool
		gateLine    int
		transportLn int
	)
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name == nil || fn.Name.Name != "proxyRequest" || fn.Recv == nil {
			return true
		}
		foundFn = true
		ast.Inspect(fn.Body, func(inner ast.Node) bool {
			call, ok := inner.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch f := call.Fun.(type) {
			case *ast.SelectorExpr:
				if f.Sel.Name == "ensureModelLoadedOnBackend" && gateLine == 0 {
					gateLine = fset.Position(call.Pos()).Line
				}
				if f.Sel.Name == "proxyRequestLlamaCpp" && transportLn == 0 {
					transportLn = fset.Position(call.Pos()).Line
				}
			}
			return true
		})
		return false
	})

	if !foundFn {
		t.Fatal("(*Proxy).proxyRequest не найдена — стюард не может проверить путь, обновите тест")
	}
	if gateLine == 0 {
		t.Fatal("generic-прокси путь снова не ждёт загрузку модели: на холодном старте " +
			"воркер грузит её внутри запроса, балансер рвёт по first-byte, клиент получает 503 " +
			"(живая жалоба R325)")
	}
	if transportLn == 0 {
		t.Fatal("в proxyRequest не найден вызов proxyRequestLlamaCpp — разметка функции изменилась")
	}
	if gateLine > transportLn {
		t.Errorf("гейт автозагрузки (строка %d) стоит ПОСЛЕ выбора транспорта (строка %d): "+
			"незагруженная модель снова уйдёт в транспорт", gateLine, transportLn)
	}
}
