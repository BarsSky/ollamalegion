// stage_steward_r477_test.go — T5 из плана «два мозга» (refactoring-plan.ru.md):
// предохранитель ДО рефакторинга, чтобы правки путей не теряли диагностику.
//
// ЗАЧЕМ. R477 добавил стадии (select_backend/admission/preflight/upstream/stream/
// client_write/done), и они уже ловили живые сбои. Но ничто не мешало:
//  1. объявить новую стадию и ни разу её не поставить (мёртвая константа, и в
//     логе снова не видно, где умер ход);
//  2. передать в SetStageR477 строковый литерал вместо константы — переименование
//     константы тогда не заметит компилятор, а лог получит чужое имя стадии;
//  3. удалить единственную точку постановки стадии «попутно» с рефакторингом.
//
// ДВЕ РОЛИ СТАДИЙ (первая версия теста этого не различала и падала на живой
// константе done):
//   - ставимые: select_backend, admission, preflight, upstream, stream,
//     client_write — их обязана ставить хотя бы одна точка SetStageR477;
//   - терминальная: done. Её НЕ ставят через SetStageR477 — по R479 она
//     вычисляется при печати трассы (stage_final = done, если ход дошёл до конца
//     без обрыва, request_trace_r476.go).
//
// Тест статический (по исходникам), поэтому не зависит от того, какой путь
// обслужил запрос, и не флакает.
package balancer

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// stageSetRequiredR477 — стадии, которые обязана ставить хотя бы одна
// продакшен-точка SetStageR477.
var stageSetRequiredR477 = []string{
	"stageSelectR477",
	"stageAdmissionR477",
	"stagePreflightR477",
	"stageUpstreamR477",
	"stageStreamR477",
	"stageClientR477",
}

// stageTerminalR477 — стадии, которые вычисляются, а не ставятся.
var stageTerminalR477 = []string{"stageDoneR477"}

// stageExpectedValuesR477 — имя константы → значение в логе. Имена стабильные:
// по ним ищут в логах и в архиве диагностики.
var stageExpectedValuesR477 = map[string]string{
	"stageSelectR477":    "select_backend",
	"stageAdmissionR477": "admission",
	"stagePreflightR477": "preflight",
	"stageUpstreamR477":  "upstream",
	"stageStreamR477":    "stream",
	"stageClientR477":    "client_write",
	"stageDoneR477":      "done",
}

// stageSetCallsR477 — точки постановки стадий в продакшен-коде пакета.
func stageSetCallsR477(t *testing.T) map[string][]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("не прочитать каталог пакета: %v", err)
	}
	fset := token.NewFileSet()
	calls := make(map[string][]string, len(stageExpectedValuesR477))
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("не разобрать %s: %v", name, err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			ident, ok := call.Fun.(*ast.Ident)
			if !ok || ident.Name != "SetStageR477" {
				return true
			}
			pos := fset.Position(call.Pos()).String()
			if len(call.Args) != 2 {
				t.Errorf("%s: SetStageR477 с %d аргументами — ожидалось 2", pos, len(call.Args))
				return true
			}
			arg, ok := call.Args[1].(*ast.Ident)
			if !ok {
				t.Errorf("%s: стадия передана не константой (%T) — используйте stage…R477; "+
					"иначе переименование константы не заметит компилятор", pos, call.Args[1])
				return true
			}
			calls[arg.Name] = append(calls[arg.Name], pos)
			return true
		})
	}
	return calls
}

// declaredStagesR477 — все константы stage…R477, объявленные в stage_r477.go.
// Читаем файл объявлений, чтобы «стадия, добавленная мимо списков» ловилась
// тестом, а не только глазами.
func declaredStagesR477(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, "stage_r477.go", nil, 0)
	if err != nil {
		t.Fatalf("не разобрать stage_r477.go: %v", err)
	}
	var names []string
	ast.Inspect(af, func(n ast.Node) bool {
		block, ok := n.(*ast.GenDecl)
		if !ok || block.Tok != token.CONST {
			return true
		}
		for _, spec := range block.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, id := range vs.Names {
				if strings.HasPrefix(id.Name, "stage") && strings.HasSuffix(id.Name, "R477") {
					names = append(names, id.Name)
				}
			}
		}
		return true
	})
	sort.Strings(names)
	return names
}

// TestR477_EveryStageHasProductionCall — каждая ставимая стадия имеет prod-точку;
// каждая объявленная стадия классифицирована (ставимая или терминальная).
func TestR477_EveryStageHasProductionCall(t *testing.T) {
	calls := stageSetCallsR477(t)

	for _, name := range stageSetRequiredR477 {
		if len(calls[name]) == 0 {
			t.Errorf("стадия %s (%q) не ставится ни в одном продакшен-пути — "+
				"в логе снова не будет видно, где умер ход",
				name, stageExpectedValuesR477[name])
		}
	}

	classified := make(map[string]bool, len(stageSetRequiredR477)+len(stageTerminalR477))
	for _, n := range stageSetRequiredR477 {
		classified[n] = true
	}
	for _, n := range stageTerminalR477 {
		classified[n] = true
	}
	for _, n := range declaredStagesR477(t) {
		if !classified[n] {
			t.Errorf("стадия %s объявлена в stage_r477.go, но не классифицирована в тесте: "+
				"добавьте её в stageSetRequiredR477 (если её ставит SetStageR477) "+
				"или в stageTerminalR477 (если она вычисляется)", n)
		}
	}

	// Вызов с константой, которой нет среди объявленных, — опечатка или чужая стадия.
	declared := make(map[string]bool)
	for _, n := range declaredStagesR477(t) {
		declared[n] = true
	}
	for name, pos := range calls {
		if !declared[name] {
			t.Errorf("SetStageR477 вызывается с %s (%s), но такой стадии нет в stage_r477.go",
				name, pos[0])
		}
	}
}

// TestR477_TerminalStageIsComputed — терминальная стадия вычисляется в трассе, а
// не ставится: R479 специально различает «где ход сейчас/умер» (stage) и «дошёл
// ли до конца» (stage_final).
func TestR477_TerminalStageIsComputed(t *testing.T) {
	calls := stageSetCallsR477(t)
	for _, name := range stageTerminalR477 {
		if len(calls[name]) > 0 {
			t.Errorf("терминальная стадия %s ставится через SetStageR477 (%v): "+
				"она вычисляется при печати трассы (stage_final), см. R479", name, calls[name])
		}
	}

	src, err := os.ReadFile("request_trace_r476.go")
	if err != nil {
		t.Fatalf("не прочитать request_trace_r476.go: %v", err)
	}
	if !strings.Contains(string(src), "stageFinal = stageDoneR477") {
		t.Error("в request_trace_r476.go нет вычисления stage_final = stageDoneR477 — " +
			"успешный ход снова будет неотличим от оборванного")
	}
}

// TestR477_StageValuesAreStable — имена стадий в логе не меняются: по ним ищут в
// логах, в архиве и в заголовках.
func TestR477_StageValuesAreStable(t *testing.T) {
	actual := map[string]string{
		"stageSelectR477":    stageSelectR477,
		"stageAdmissionR477": stageAdmissionR477,
		"stagePreflightR477": stagePreflightR477,
		"stageUpstreamR477":  stageUpstreamR477,
		"stageStreamR477":    stageStreamR477,
		"stageClientR477":    stageClientR477,
		"stageDoneR477":      stageDoneR477,
	}
	for name, want := range stageExpectedValuesR477 {
		if got := actual[name]; got != want {
			t.Errorf("%s = %q, ожидалось %q (имена стадий стабильные: их ищут в логах)",
				name, got, want)
		}
	}
	if len(actual) != len(stageExpectedValuesR477) {
		t.Errorf("в тесте %d стадий, в ожидаемых значениях %d",
			len(actual), len(stageExpectedValuesR477))
	}
}
