// donor_guard_steward_r304_test.go — R304 (2026-10-30): СТЮАРД «нет обхода».
//
// ЗАЧЕМ. Повторяющийся класс дефектов проекта — «новая точка обхода общего
// пути»: пока правило живёт в каждом обработчике отдельно, новая ручка загрузки
// появляется БЕЗ него, и запрет тихо перестаёт действовать не там, где его
// писали, а там, где про него забыли. Ровно так уже было с раскладкой
// (rpcEnabled), с окном ленивой загрузки и с очередью заявок.
//
// ЧТО ДЕЛАЕТ ТЕСТ. Разбирает исходники пакета и требует:
//
//  1. каждый HTTP-обработчик, который зовёт загрузку напрямую
//     (runAsyncLoad/runAsyncReload/LoadModelWithOpts), обязан звать
//     donorLoadGuard — иначе загрузка на доноре вернула бы 202, а не 409;
//  2. каждая функция-привратник, через которую копия создаётся БЕЗ HTTP
//     (ensureModelLoadedWithNCtx для запроса чата, autoLoadModels для старта,
//     runAsyncLoad/runAsyncReload для фона), обязана сама спрашивать роль
//     (donorOwnCopyRefusedR304) — donorLoadGuard там неуместен, у неё нет
//     http.ResponseWriter.
//
// ЧЕГО ТЕСТ НЕ ДЕЛАЕТ: не проверяет, что гард верный (это donor_guard_r304_test.go
// и internal/cppbackend/donor_role_r304_test.go). Его задача — чтобы новый путь
// загрузки нельзя было добавить молча.
package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// donorGuardFileR304 — сам гард: сканировать его нечего (обработчиков в нём нет).
const donorGuardFileR304 = "donor_guard_r304.go"

// directCopySinksR304 — вызовы, которые из обработчика НЕПОСРЕДСТВЕННО поднимают
// локальную копию модели.
//
// ensureModelLoaded здесь намеренно отсутствует: это привратник (см.
// gatedDoorFunctionsR304) — он сам проверяет роль, и требовать гард в каждом
// обработчике чата значило бы размазать запрет по пяти файлам вместо одного.
var directCopySinksR304 = []string{"runAsyncLoad", "runAsyncReload", "LoadModelWithOpts"}

// gatedDoorFunctionsR304 — привратники загрузки без HTTP: внутри каждого обязана
// стоять проверка роли.
var gatedDoorFunctionsR304 = []string{
	"ensureModelLoadedWithNCtx", // ленивая загрузка по запросу чата
	"autoLoadModels",            // «загрузить всё» при старте
	"runAsyncLoad",              // фоновая загрузка после 202
	"runAsyncReload",            // фоновый reload после 202
}

// parsedFileR304 — разобранный файл пакета вместе с набором токенов.
type parsedFileR304 struct {
	name string
	file *ast.File
	fset *token.FileSet
}

// TestR304STEWARD_NoLoadHandlerBypassesDonorGuard — проверка 1.
func TestR304STEWARD_NoLoadHandlerBypassesDonorGuard(t *testing.T) {
	files := parsePackageFilesR304(t)

	var seen []string
	for _, pf := range files {
		for _, fn := range funcsR304(pf.file) {
			if !isHTTPHandlerR304(fn) {
				continue
			}
			calls := callNamesR304(fn)
			var sinks []string
			for _, sink := range directCopySinksR304 {
				if calls[sink] {
					sinks = append(sinks, sink)
				}
			}
			if len(sinks) == 0 {
				continue
			}
			seen = append(seen, fn.Name.Name)
			if calls["donorLoadGuard"] {
				continue
			}
			t.Errorf("%s (%s): обработчик зовёт %s в ОБХОД donorLoadGuard.\n"+
				"    Донор при этом получит 202 «загрузка принята» вместо 409 — ровно тот дефект,\n"+
				"    от которого защищает R304. Поставь `if donorLoadGuard(w) { return }`\n"+
				"    первой содержательной проверкой обработчика.",
				pf.fset.Position(fn.Pos()), fn.Name.Name, strings.Join(sinks, ", "))
		}
	}

	// Защита от «зелёного» теста, который ничего не нашёл: если разбор сломался
	// или обработчики переименовали, обязательство должно упасть, а не замолчать.
	for _, want := range []string{"handleLoadModel", "handleLoadWithParams", "handleReloadModel"} {
		if !containsNameR304(seen, want) {
			t.Fatalf("сканер не увидел обработчик %s среди путей загрузки (найдено: %v) — "+
				"тест проверяет не то, что задумано", want, seen)
		}
	}
}

// TestR304STEWARD_DoorFunctionsAskRoleThemselves — проверка 2.
func TestR304STEWARD_DoorFunctionsAskRoleThemselves(t *testing.T) {
	files := parsePackageFilesR304(t)

	for _, want := range gatedDoorFunctionsR304 {
		var found *ast.FuncDecl
		var pos token.Position
		for _, pf := range files {
			for _, fn := range funcsR304(pf.file) {
				if fn.Name.Name == want {
					found = fn
					pos = pf.fset.Position(fn.Pos())
					break
				}
			}
			if found != nil {
				break
			}
		}
		if found == nil {
			// Функцию переименовали/унесли — тест обязан об этом сказать, иначе его
			// список привратников молча устарел бы и «нет обхода» превратилось бы в
			// «нет проверки».
			t.Fatalf("функция-привратник %s не найдена — обнови gatedDoorFunctionsR304", want)
		}
		if !callNamesR304(found)["donorOwnCopyRefusedR304"] {
			t.Errorf("%s (%s): привратник не спрашивает роль у бэкенда "+
				"(нет вызова donorOwnCopyRefusedR304).\n"+
				"    Через него копия создаётся БЕЗ HTTP, поэтому donorLoadGuard здесь не сработает:\n"+
				"    донор снова начал бы поднимать модели — по запросу чата, при старте или в фоне.",
				pos, want)
		}
	}
}

// parsePackageFilesR304 — все НЕтестовые .go файлы пакета, кроме самого гарда.
func parsePackageFilesR304(t *testing.T) []parsedFileR304 {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir(.): %v", err)
	}
	var out []parsedFileR304
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		if strings.HasSuffix(name, "_test.go") || name == donorGuardFileR304 {
			continue
		}
		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, name, nil, 0)
		if parseErr != nil {
			t.Fatalf("разбор %s: %v", name, parseErr)
		}
		out = append(out, parsedFileR304{name: name, file: file, fset: fset})
	}
	if len(out) == 0 {
		t.Fatal("не разобрано ни одного файла — проверка бессмысленна")
	}
	return out
}

// funcsR304 — объявления функций файла (без методов-приёмников с пустым телом).
func funcsR304(file *ast.File) []*ast.FuncDecl {
	var out []*ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
			out = append(out, fn)
		}
	}
	return out
}

// isHTTPHandlerR304 — принимает ли функция *http.Request.
//
// Признак взят по типу параметра, а не по имени: имя обработчика ничем не
// закреплено, а «принимает запрос» — и есть определение HTTP-обработчика.
func isHTTPHandlerR304(fn *ast.FuncDecl) bool {
	if fn.Type == nil || fn.Type.Params == nil {
		return false
	}
	for _, p := range fn.Type.Params.List {
		star, ok := p.Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		sel, ok := star.X.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		pkg, ok := sel.X.(*ast.Ident)
		if ok && pkg.Name == "http" && sel.Sel.Name == "Request" {
			return true
		}
	}
	return false
}

// callNamesR304 — имена всех вызванных функций в теле (включая вложенные
// литералы и `go`/`defer`: фоновая горутина — тот же путь загрузки).
//
// Для вызовов вида `backend.LoadModelWithOpts(...)` берётся последний сегмент:
// проверяющему важно ЧТО зовут, а не через какую переменную.
func callNamesR304(fn *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch callee := call.Fun.(type) {
		case *ast.Ident:
			out[callee.Name] = true
		case *ast.SelectorExpr:
			out[callee.Sel.Name] = true
		}
		return true
	})
	return out
}

// containsNameR304 — есть ли имя в списке.
func containsNameR304(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
