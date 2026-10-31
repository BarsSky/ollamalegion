// keepalive_after_answer_r347_test.go — R347 (2026-10-30).
//
// ЖИВОЙ ДЕФЕКТ, ПОЙМАННЫЙ НА СТЕНДЕ (проверка донора, RPC-раскладка из двух узлов).
// После КАЖДОГО ответа /api/generate cppworker выгружал модель из VRAM, даже если
// клиент просил keep_alive="30m". Следующий запрос платил полную загрузку:
//
//	замер gemma-4-E4B-it-Q4_K_M (ctx 32768), 8 ГБ, стенд:
//	  запрос «по горячему»: 2.7 с, 27.6 tok/s
//	  следующий запрос:     78-80 с, 3.2 tok/s  (загрузка 58.7 с)
//	  в RPC-раскладке:      ~100 с на запрос, 3.9 tok/s
//
// ПРИЧИНА. `runGenerateCore(w, r, req generateRequest)` принимает запрос ПО ЗНАЧЕНИЮ
// и вызывает `normalizeGenerateRequest(&req)` — то есть заполняет `_keepAliveDuration`
// у СВОЕЙ копии. Обработчик же регистрировал `defer applyKeepAlive(modelName,
// req._keepAliveDuration)` со своим `req`, у которого это поле оставалось нулём.
// А ноль для applyKeepAlive — это «клиент просил выгрузить».
//
// ПОЧЕМУ ТЕСТ-СТЮАРД, А НЕ ТОЛЬКО МОДУЛЬНЫЙ. Класс дефекта — «значение посчитано не
// в той копии структуры»: он не виден ни компилятору, ни ревью отдельной строки,
// а новая ручка генерации воспроизведёт его молча. Поэтому проверяем форму кода:
// в каждом обработчике `/api/generate` время жизни обязано вычисляться ИЗ ТОГО ЖЕ
// req, чей defer зовёт applyKeepAlive, и раньше вызова runGenerateCore.
package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"
)

// TestR347_KeepAliveOfRequest_Semantics — чистая семантика: отсутствие поля НЕ
// означает выгрузку. Ошибка именно в этом: ноль вместо дефолта.
func TestR347_KeepAliveOfRequest_Semantics(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want time.Duration
		why  string
	}{
		{"absent", "", defaultKeepAliveDuration, "поле не задано — дефолт 30 минут, а не выгрузка"},
		{"minus one", "-1", defaultKeepAliveDuration, "-1 у Ollama означает «бесконечно», у нас дефолт"},
		{"explicit zero", "0", 0, "явный 0 — единственный случай, когда модель выгружается"},
		{"explicit 0s", "0s", 0, "0s — то же, что 0"},
		{"duration", "5m", 5 * time.Minute, "duration парсится как есть"},
		{"garbage", "не дата", defaultKeepAliveDuration, "мусор не должен выгружать модель"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := keepAliveOfRequest(generateRequest{KeepAlive: tc.raw})
			if got != tc.want {
				t.Fatalf("keep_alive=%q → %v, want %v (%s)", tc.raw, got, tc.want, tc.why)
			}
		})
	}
}

// TestR347_KeepAliveComputedBeforeCore_STEWARD — форма обработчиков.
//
// Требования к КАЖДОМУ обработчику /api/generate:
//  1. есть присваивание `req._keepAliveDuration = keepAliveOfRequest(req)`;
//  2. оно стоит РАНЬШЕ вызова runGenerateCore (иначе действует прежний дефект);
//  3. defer зовёт applyKeepAlive с `req._keepAliveDuration` (не с нулём и не с
//     локальной копией, полученной из runGenerateCore).
func TestR347_KeepAliveComputedBeforeCore_STEWARD(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "handlers_generate.go", nil, 0)
	if err != nil {
		t.Fatalf("не удалось разобрать handlers_generate.go: %v", err)
	}

	// keepaliveAssignOffset — offset присваивания req._keepAliveDuration = keepAliveOfRequest(req).
	keepaliveAssignOffset := func(fn *ast.FuncDecl) int {
		offset := -1
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
				return true
			}
			sel, ok := as.Lhs[0].(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "_keepAliveDuration" {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "req" {
				return true
			}
			call, ok := as.Rhs[0].(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "keepAliveOfRequest" {
				return true
			}
			offset = fset.Position(as.Pos()).Offset
			return false
		})
		return offset
	}

	// coreCallOffset — offset вызова runGenerateCore в теле функции.
	coreCallOffset := func(fn *ast.FuncDecl) int {
		offset := -1
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if offset >= 0 {
				return false
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "runGenerateCore" {
				offset = fset.Position(call.Pos()).Offset
				return false
			}
			return true
		})
		return offset
	}

	// applyKeepAliveUsesReqField — defer зовёт applyKeepAlive(model, req._keepAliveDuration).
	applyKeepAliveUsesReqField := func(fn *ast.FuncDecl) bool {
		found := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "applyKeepAlive" {
				return true
			}
			sel, ok := call.Args[1].(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "_keepAliveDuration" {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "req" {
				return true
			}
			found = true
			return false
		})
		return found
	}

	targets := []string{"handleGenerate", "handleOllamaGenerate"}
	found := 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		wanted := false
		for _, want := range targets {
			if fn.Name.Name == want {
				wanted = true
				break
			}
		}
		if !wanted {
			continue
		}
		found++

		core := coreCallOffset(fn)
		if core < 0 {
			continue // функция может не звать core (например, отфильтрована) — не наше дело
		}
		assign := keepaliveAssignOffset(fn)
		if assign < 0 {
			t.Errorf("%s: нет присваивания `req._keepAliveDuration = keepAliveOfRequest(req)` — "+
				"без него _keepAliveDuration остаётся нулём, и модель выгружается после КАЖДОГО ответа "+
				"(R347: замер 2.7 с → 78-80 с на следующий запрос)", fn.Name.Name)
			continue
		}
		if assign > core {
			t.Errorf("%s: `req._keepAliveDuration` вычисляется ПОСЛЕ runGenerateCore — "+
				"именно этот порядок и был дефектом R347 (runGenerateCore нормализует копию)", fn.Name.Name)
		}
		if !applyKeepAliveUsesReqField(fn) {
			t.Errorf("%s: applyKeepAlive вызван не с `req._keepAliveDuration` — "+
				"время жизни модели должно браться из нормализованного запроса", fn.Name.Name)
		}
	}

	if found == 0 {
		t.Fatal("не найдено ни одного обработчика /api/generate (handleGenerate/handleOllamaGenerate) — " +
			"если они переименованы, обновите список targets в тесте, иначе стюард ничего не проверяет")
	}
}
