//go:build llama_stub

// model_name_match_single_source_r502_test.go — R502 (2026-11-19), аудит S4,
// пункт X-3.
//
// ДЕФЕКТ. Сравнение имён моделей существовало в двух видах: канонический
// `modelNameMatches` (нормализует путь, `.gguf`, тег Ollama, регистр) и ad-hoc
// `m.Name == model || strings.Contains(m.Name, model)` в 9 точках решений. Из-за
// этого клиент, назвавший модель так же, как она лежит на диске
// («Qwen3-8B-Q4_K_M.gguf»), не попадал в группу «модель уже загружена», хотя она
// загружена, — уезжал на свободный узел под загрузку или в очередь.
package balancer

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR502_GgufNameMatchesLoadedModel — X-3: имя с расширением .gguf находит
// загруженную модель (раньше — только точное совпадение строк).
func TestR502_GgufNameMatchesLoadedModel(t *testing.T) {
	p := availProxyR493(t, map[string]int{"bk": 4})
	setMetricsR493(p, "bk", []string{"Qwen3-8B-Q4_K_M"}, 20000)

	groups := p.expandCandidates("qwen3-8b-q4_k_m.gguf", []types.BackendType{types.BackendTypeLlamaCpp})
	for _, g := range groups {
		if g.Priority != 1 {
			continue
		}
		for _, id := range g.BackendIDs {
			if id == "bk" {
				return // модель опознана как загруженная — то, что и требовалось
			}
		}
	}
	t.Fatalf("модель с .gguf не попала в группу P1 (loaded): правило сравнения имён "+
		"снова разошлось (X-3), группы = %+v", groups)
}

// TestR502_ModelNameMatchIsSingleSource — X-3: в решениях не осталось ad-hoc
// сравнения имён (`strings.Contains(m.Name, …)`).
//
// Исключение одно и осознанное: `proxy_request.go` разрешает ИМЯ В ПУТЬ к файлу
// (`*.gguf`) многошаговым поиском по alias — это не решение «та же ли модель»,
// а поиск файла, и подмена правилом сломала бы разрешение alias'ов.
func TestR502_ModelNameMatchIsSingleSource(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("чтение каталога пакета: %v", err)
	}
	canonicalUses := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if name == "proxy_request.go" {
			continue // разрешение имени в путь к файлу — другой класс задачи
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("разбор %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "modelNameMatches" {
				canonicalUses++
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel == nil {
				return true
			}
			if sel.Sel.Name != "Contains" || len(call.Args) != 2 {
				return true
			}
			arg, ok := call.Args[0].(*ast.SelectorExpr)
			if !ok || arg.Sel == nil || arg.Sel.Name != "Name" {
				return true
			}
			t.Errorf("%s:%d: ad-hoc сравнение имён моделей (%s) — используйте "+
				"modelNameMatches (аудит X-3)", name, fset.Position(call.Pos()).Line,
				"strings.Contains(... .Name ...)")
			return true
		})
	}
	if canonicalUses < 5 {
		t.Fatalf("канонический modelNameMatches используется всего %d раз: "+
			"проверка выродилась — сверьте, что решения о модели идут через него", canonicalUses)
	}
}
