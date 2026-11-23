package balancer

// queue_attempts_d42_test.go — D4.2 (2026-11-25): попытки считаются, а не только
// успехи.
//
// ЖИВОЙ ЗАМЕР на стенде r727: запрос через балансер получил 503, при этом
// `cluster.totalRequests=0`, а `recentClients.requestCount=6`. Причина:
// `totalRequests` инкрементится ТОЛЬКО после успешного ответа апстрима
// (`proxy_request.go`, ветка `retrySucceeded`), поэтому отказы и ожидание в
// очереди в панели выглядели как «запросов не было».
//
// РЕШЕНИЕ: рядом публикуется `totalAttempts` — попытки инференс-запросов,
// включая отказы. Считается в ServeHTTP ДО диспетчеризации и только для
// инференс-путей (чтение и админ-ручки не считаем, иначе число перестанет
// означать нагрузку).

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

func TestD42_AttemptsWirePresent(t *testing.T) {
	fset := token.NewFileSet()

	// 1. Поле счётчика в Proxy.
	pf, err := parser.ParseFile(fset, "proxy.go", nil, 0)
	if err != nil {
		t.Fatalf("не разобрать proxy.go: %v", err)
	}
	hasField := false
	hasIncrement := false
	guarded := false
	ast.Inspect(pf, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.Field:
			for _, name := range v.Names {
				if name.Name == "totalAttempts" {
					hasField = true
				}
			}
		case *ast.CallExpr:
			// atomic.AddInt64(&p.totalAttempts, 1)
			if sel, ok := v.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "AddInt64" && len(v.Args) == 2 {
				if unary, ok := v.Args[0].(*ast.UnaryExpr); ok {
					if sx, ok := unary.X.(*ast.SelectorExpr); ok && sx.Sel.Name == "totalAttempts" {
						hasIncrement = true
					}
				}
			}
		case *ast.IfStmt:
			// if !isReadOnlyOrMgmtEndpoint(...) { atomic.AddInt64(&p.totalAttempts, 1) }
			expr := strings.TrimSpace(nodeString(v.Cond))
			if strings.Contains(expr, "isReadOnlyOrMgmtEndpoint") {
				ast.Inspect(v.Body, func(inner ast.Node) bool {
					if call, ok := inner.(*ast.CallExpr); ok {
						if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "AddInt64" &&
							len(call.Args) == 2 {
							if unary, ok := call.Args[0].(*ast.UnaryExpr); ok {
								if sx, ok := unary.X.(*ast.SelectorExpr); ok && sx.Sel.Name == "totalAttempts" {
									guarded = true
								}
							}
						}
					}
					return true
				})
			}
		}
		return true
	})
	if !hasField {
		t.Error("в Proxy нет поля totalAttempts — попытки некуда считать")
	}
	if !hasIncrement {
		t.Error("нет atomic.AddInt64(&p.totalAttempts, 1) — попытки не считаются")
	}
	if !guarded {
		t.Error("инкремент totalAttempts не ограничен инференс-путями (isReadOnlyOrMgmtEndpoint) — " +
			"админ-ручки и чтение начнут считаться нагрузкой")
	}

	// 2. Публикация в ClusterState.
	cf, err := parser.ParseFile(fset, "cluster_state.go", nil, 0)
	if err != nil {
		t.Fatalf("не разобрать cluster_state.go: %v", err)
	}
	assigned := false
	ast.Inspect(cf, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, lhs := range as.Lhs {
			if sx, ok := lhs.(*ast.SelectorExpr); ok && sx.Sel.Name == "TotalAttempts" {
				assigned = true
			}
		}
		return true
	})
	if !assigned {
		t.Error("ClusterState.TotalAttempts не заполняется в cluster_state.go — число посчитано, но наружу не выходит")
	}
}

// nodeString — грубое текстовое представление выражения для поиска имени
// функции в условии (полноценный printer здесь избыточен).
func nodeString(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.UnaryExpr:
		return v.Op.String() + nodeString(v.X)
	case *ast.CallExpr:
		return nodeString(v.Fun)
	case *ast.SelectorExpr:
		return nodeString(v.X) + "." + v.Sel.Name
	case *ast.Ident:
		return v.Name
	case *ast.ParenExpr:
		return nodeString(v.X)
	}
	return ""
}
