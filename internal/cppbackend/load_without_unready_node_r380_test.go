// load_without_unready_node_r380_test.go — R380 (2026-11-01).
//
// Проверяем правило и то, что путь загрузки им РЕАЛЬНО пользуется: гейт
// готовности без этого был декоративным — адреса уходили в мост при любом
// исходе, и занятый узел ронял процесс в cgo (negotiate_hello → SIGSEGV).
package cppbackend

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestR380_UnreadyNodeIsNotHandedToBridge — правило.
func TestR380_UnreadyNodeIsNotHandedToBridge(t *testing.T) {
	servers := []string{"100.64.0.7:50052", "100.64.0.8:50052"}

	if got := bridgeServersForLoadR380(servers, true); len(got) != len(servers) {
		t.Fatalf("готовый узел обязан идти в мост: got %v", got)
	}
	if got := bridgeServersForLoadR380(servers, false); got != nil {
		t.Fatalf("непринятый узел НЕ должен уходить в мост (иначе SIGSEGV в negotiate_hello), got %v", got)
	}
	if got := bridgeServersForLoadR380(nil, true); got != nil {
		t.Fatalf("пустой список остаётся пустым, got %v", got)
	}
}

// TestR380STEWARD_GateResultFeedsBridgeServers — путь загрузки обязан передавать
// мосту результат гейта, а не исходный список.
//
// Проверка по исходнику, потому что цена ошибки — падение процесса, а не
// неверный ответ: воспроизвести SIGSEGV в обычном тесте нельзя (нужен занятый
// rpc-server и гонка в инициализации cgo).
func TestR380STEWARD_GateResultFeedsBridgeServers(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "backend.go", nil, 0)
	if err != nil {
		t.Fatalf("разбор backend.go: %v", err)
	}

	bridgeArg := ""
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "applyRPCServersR105" {
			return true
		}
		if len(call.Args) < 2 {
			return true
		}
		if arg, ok := call.Args[1].(*ast.Ident); ok {
			bridgeArg = arg.Name
		}
		return true
	})
	if bridgeArg == "" {
		t.Fatal("в backend.go не найден вызов applyRPCServersR105(&cfg, …) — стюард проверяет не то")
	}
	if bridgeArg == "loadRPCServers" {
		t.Fatalf("в мост уходит исходный список %q: гейт готовности игнорируется — "+
			"занятый узел снова уронит процесс (R380)", bridgeArg)
	}

	// Переменная, переданная в мост, обязана быть результатом bridgeServersForLoadR380.
	assignedFromGate := false
	ast.Inspect(file, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		id, ok := as.Lhs[0].(*ast.Ident)
		if !ok || id.Name != bridgeArg {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		if fn, ok := call.Fun.(*ast.Ident); ok && fn.Name == "bridgeServersForLoadR380" {
			assignedFromGate = true
		}
		return true
	})
	if !assignedFromGate {
		t.Fatalf("%q не вычисляется через bridgeServersForLoadR380 — результат гейта "+
			"не влияет на список серверов (R380)", bridgeArg)
	}
}
