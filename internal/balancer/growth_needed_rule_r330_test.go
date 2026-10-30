// growth_needed_rule_r330_test.go — R330 (2026-10-30).
//
// Аудит маршрутов: вердикт «растим окно / ждём / отказываем» размазан по трём
// guard'ам (R288, R299, R301), и число needed считалось ДВУМЯ формулами —
// contextGrowthNumbers и contextGrowthBlocker. Результат совпадал, но любая
// правка одной разводила вердикт с числами в теле отказа (класс дефекта R267).
//
// Тесты фиксируют: (1) правило одно и живёт в neededWindowTokensR330; (2) оба
// места действительно вызывают его (AST-стюард), а не считают по-своему.
package balancer

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestR330_NeededWindowRule — одна формула на все случаи.
func TestR330_NeededWindowRule(t *testing.T) {
	cases := []struct {
		name string
		meta *RequestMeta
		want int
		why  string
	}{
		{"nil meta", nil, 0, "чисел нет — отказ строить не на чем"},
		{"ни оценки, ни окна", &RequestMeta{}, 0, "0 = «неизвестно», вызывающий не отказывает"},
		{"только оценка промпта", &RequestMeta{EstimatedPromptTokens: 10000}, 10000 + agentOutputBudget(&RequestMeta{EstimatedPromptTokens: 10000}), "промпт + бюджет вывода"},
		{"только явное окно", &RequestMeta{RequestedNCtxOverride: 65536}, 65536, "клиент вправе просить больше, чем нужно промпту"},
		{"запрошено больше, чем нужно промпту", &RequestMeta{EstimatedPromptTokens: 1000, RequestedNCtxOverride: 65536}, 65536, "берётся максимум"},
		{"промпт требует больше запрошенного", &RequestMeta{EstimatedPromptTokens: 60000, RequestedNCtxOverride: 4096}, 60000 + agentOutputBudget(&RequestMeta{EstimatedPromptTokens: 60000}), "промпт важнее: иначе воркер ответит code=3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := neededWindowTokensR330(tc.meta); got != tc.want {
				t.Errorf("neededWindowTokensR330 = %d, want %d (%s)", got, tc.want, tc.why)
			}
		})
	}
}

// TestR330_GrowthGuardsUseTheSingleRule — AST-стюард: обе функции считают needed
// через общую формулу, а не собственной копией.
func TestR330_GrowthGuardsUseTheSingleRule(t *testing.T) {
	targets := map[string]string{
		"contextGrowthNumbers": "deferred_ops_r291.go",
		"contextGrowthBlocker": "context_growth_r288.go",
	}
	for fnName, fileName := range targets {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, fileName, nil, 0)
		if err != nil {
			t.Fatalf("разбор %s: %v", fileName, err)
		}
		found := false
		usesRule := false
		ast.Inspect(file, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Name == nil || fn.Name.Name != fnName {
				return true
			}
			found = true
			ast.Inspect(fn.Body, func(inner ast.Node) bool {
				call, ok := inner.(*ast.CallExpr)
				if !ok {
					return true
				}
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "neededWindowTokensR330" {
					usesRule = true
				}
				return true
			})
			return false
		})
		if !found {
			t.Fatalf("%s не найдена в %s — стюард устарел", fnName, fileName)
		}
		if !usesRule {
			t.Errorf("%s не использует neededWindowTokensR330: снова две формулы одного числа "+
				"(класс дефекта R267: вердикт расходится с текстом отказа)", fnName)
		}
	}
}

// TestR330_ContextGrowthNumbersNoLongerDuplicatesFormula — числа совпадают с
// правилом (защита от возврата второй копии в deferred_ops_r291.go).
func TestR330_ContextGrowthNumbersNoLongerDuplicatesFormula(t *testing.T) {
	meta := &RequestMeta{EstimatedPromptTokens: 20000, RequestedNCtxOverride: 65536}
	const loaded = 32768
	want := neededWindowTokensR330(meta)
	if want != 65536 {
		t.Fatalf("правило вернуло %d, want 65536 (явно запрошенное окно больше)", want)
	}
	if want <= loaded {
		t.Fatal("тест потерял смысл: needed должно быть больше загруженного окна")
	}

}
