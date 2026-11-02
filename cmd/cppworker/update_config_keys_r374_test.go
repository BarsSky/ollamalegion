// update_config_keys_r374_test.go — R374 (2026-11-01): СТЮАРД «карта известных
// ключей не отстаёт от применения».
//
// ЗАЧЕМ. В PUT /api/v1/cppworker/config/update было ДВЕ правды об одном наборе
// ключей: вызовы apply*() применяли настройку, а карта «известных ключей» рядом
// решала, писать ли «unknown field». Списки разъехались: rpcServerEnabled и
// rpcServerPort применялись, но в карте отсутствовали — и на каждой смене роли
// донора воркер писал предупреждение ровно о том, что только что успешно
// применил (живой факт двухузлового стенда).
//
// ЧТО ДЕЛАЕТ ТЕСТ. Разбирает handlers_config.go и требует, чтобы КАЖДЫЙ ключ,
// который применяется вызовом apply*("<key>"), был объявлен в
// updateConfigKnownKeysR374. Новая настройка, добавленная только в применение,
// ломает тест — вместо тихого «unknown field» в логе оператора.
package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// appliedConfigKeysR374 — собрать ключи, которые обработчик ЧИТАЕТ и ПРИМЕНЯЕТ.
//
// R381 (2026-11-01): раньше сканер видел только `apply*("key", …)`, и ключи,
// которые читаются иначе — getStringSlice("rpcServers"), updates["rpcEnabled"] —
// оставались вне проверки. Именно в этой дыре жил дефект: обе настройки
// раскладки применялись и одновременно логировались как «unknown field».
// Теперь собираем все три формы.
func appliedConfigKeysR374(t *testing.T) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "handlers_config.go", nil, 0)
	if err != nil {
		t.Fatalf("разбор handlers_config.go: %v", err)
	}
	out := map[string]bool{}
	addLiteral := func(expr ast.Expr) {
		lit, ok := expr.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return
		}
		if key, err := strconv.Unquote(lit.Value); err == nil && key != "" {
			out[key] = true
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			ident, ok := node.Fun.(*ast.Ident)
			if !ok || len(node.Args) == 0 {
				return true
			}
			// apply*("key", …) — применение; get*("key") — чтение из тела запроса.
			if strings.HasPrefix(ident.Name, "apply") || strings.HasPrefix(ident.Name, "get") {
				addLiteral(node.Args[0])
			}
		case *ast.IndexExpr:
			// updates["key"] — прямое чтение из карты обновления.
			if id, ok := node.X.(*ast.Ident); ok && id.Name == "updates" {
				addLiteral(node.Index)
			}
		}
		return true
	})
	return out
}

// TestR374STEWARD_EveryAppliedKeyIsKnown — главная проверка.
func TestR374STEWARD_EveryAppliedKeyIsKnown(t *testing.T) {
	applied := appliedConfigKeysR374(t)
	if len(applied) < 30 {
		t.Fatalf("сканер увидел %d применяемых ключей — тест проверяет не то, что задумано", len(applied))
	}
	var missing []string
	for key := range applied {
		if !updateConfigKnownKeysR374[key] {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		t.Errorf("эти ключи ПРИМЕНЯЮТСЯ, но отсутствуют в updateConfigKnownKeysR374: %v\n"+
			"    Следствие — предупреждение «unknown field» ровно о том, что успешно применилось\n"+
			"    (живой случай R374: rpcServerEnabled/rpcServerPort на каждой смене роли донора).",
			missing)
	}
}

// TestR374_RPCNodeKeysAreKnown — частный случай, из которого правка выросла.
func TestR374_RPCNodeKeysAreKnown(t *testing.T) {
	for _, key := range []string{"rpcServerEnabled", "rpcServerPort"} {
		if !updateConfigKnownKeysR374[key] {
			t.Errorf("ключ %q не в карте: балансер шлёт его при смене роли, воркер применяет, а лог ругается «unknown field»", key)
		}
	}
}
