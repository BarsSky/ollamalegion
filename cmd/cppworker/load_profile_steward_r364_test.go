// load_profile_steward_r364_test.go — R364 (2026-10-31): СТЮАРД «новый путь
// загрузки не может молча обойти профиль модели».
//
// ЗАЧЕМ. Повторяющийся класс дефектов проекта — «новая точка обхода общего
// пути»: правило живёт в каждом обработчике отдельно, новая ручка загрузки
// появляется БЕЗ него, и оно тихо перестаёт действовать не там, где его писали,
// а там, где про него забыли. Ровно это и случилось с профилем модели:
// handleLoadWithParams (R26) и handleReloadModel его применяли, ленивая загрузка
// — с R83/v52, а legacy POST /api/models/load — НЕ применял. Причём комментарий
// в lazyload.go утверждал, что применяет; расхождение прожило до живого замера
// (ctx 65536 вместо 32768, kv q8_0 вместо q4_0, 2.9 tok/s вместо 25.9).
//
// ЧТО ДЕЛАЕТ ТЕСТ. Разбирает исходники пакета и требует, чтобы каждый
// HTTP-обработчик, который доводит загрузку до llama.cpp
// (runAsyncLoad/runAsyncReload/LoadModelWithOpts), консультировался с профилем
// модели одной из штатных дверей:
//
//	readProfileForLoadR364          — legacy /api/models/load (R364)
//	applyProfileToLoadRequest       — load-with-params / reload (R26)
//	applySyncedProfileToLazyLoadOpts — ленивая загрузка по запросу чата (R83/v52)
//	profileSyncer.applyProfileOnLoad — прямой доступ к syncer-у
//
// ЧЕГО ТЕСТ НЕ ДЕЛАЕТ: не проверяет ни приоритет полей, ни содержимое seed —
// это load_profile_priority_r364_test.go. Его задача — чтобы новую ручку
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

// loadSinksR364 — вызовы, которые доводят загрузку до llama.cpp.
var loadSinksR364 = []string{"runAsyncLoad", "runAsyncReload", "LoadModelWithOpts"}

// profileDoorsR364 — штатные способы спросить «а что говорит профиль модели».
var profileDoorsR364 = []string{
	"readProfileForLoadR364",
	"applyProfileToLoadRequest",
	"applySyncedProfileToLazyLoadOpts",
	"applyProfileOnLoad",
}

// TestR364STEWARD_EveryLoadPathConsultsModelProfile — проверка.
func TestR364STEWARD_EveryLoadPathConsultsModelProfile(t *testing.T) {
	files := parsePackageFilesR364(t)

	var seen []string
	for _, pf := range files {
		for _, fn := range funcsR364(pf.file) {
			if !isHTTPHandlerR364(fn) {
				continue
			}
			calls := callNamesR364(fn)
			var sinks []string
			for _, sink := range loadSinksR364 {
				if calls[sink] {
					sinks = append(sinks, sink)
				}
			}
			if len(sinks) == 0 {
				continue
			}
			seen = append(seen, fn.Name.Name)
			if anyDoorR364(calls) {
				continue
			}
			t.Errorf("%s (%s): обработчик грузит модель (%s), но НЕ спрашивает профиль модели.\n"+
				"    Профиль оператора (окно, KV-cache, слои, parallelism, reasoning) при этом молча\n"+
				"    игнорируется — ровно дефект R364: модель поднималась с ctx 65536 вместо 32768\n"+
				"    и давала 2.9 tok/s вместо 25.9. Добавь одну из дверей: %s.",
				pf.fset.Position(fn.Pos()), fn.Name.Name,
				strings.Join(sinks, ", "), strings.Join(profileDoorsR364, ", "))
		}
	}

	// Защита от «зелёного» теста, который ничего не нашёл: если разбор сломался
	// или обработчики переименовали, обязательство должно упасть, а не замолчать.
	for _, want := range []string{"handleLoadModel", "handleLoadWithParams", "handleReloadModel"} {
		if !containsNameR364(seen, want) {
			t.Fatalf("сканер не увидел обработчик %s среди путей загрузки (найдено: %v) — "+
				"тест проверяет не то, что задумано", want, seen)
		}
	}
}

// TestR364STEWARD_LegacyLoadUsesResolver — legacy-путь обязан собирать параметры
// через resolveLoadParamsR364, а не через defaultIntPtr напрямую: иначе порядок
// «запрос > профиль > конфиг» снова разъедется по обработчикам.
func TestR364STEWARD_LegacyLoadUsesResolver(t *testing.T) {
	fn := findFuncR364(t, "handleLoadModel")
	calls := callNamesR364(fn)
	if !calls["resolveLoadParamsR364"] {
		t.Error("handleLoadModel не зовёт resolveLoadParamsR364 — приоритет «запрос > профиль > конфиг» " +
			"перестал быть общим правилом и снова размазан по обработчику")
	}
	if !calls["readProfileForLoadR364"] {
		t.Error("handleLoadModel не зовёт readProfileForLoadR364 — профиль модели снова не читается на legacy-пути")
	}
}

// parsePackageFilesR364 — все НЕтестовые .go файлы пакета.
func parsePackageFilesR364(t *testing.T) []parsedPackageFileR364 {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir(.): %v", err)
	}
	var out []parsedPackageFileR364
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, name, nil, 0)
		if parseErr != nil {
			t.Fatalf("разбор %s: %v", name, parseErr)
		}
		out = append(out, parsedPackageFileR364{name: name, file: file, fset: fset})
	}
	if len(out) == 0 {
		t.Fatal("не разобрано ни одного файла — проверка бессмысленна")
	}
	return out
}

// parsedPackageFileR364 — разобранный файл пакета вместе с набором токенов.
type parsedPackageFileR364 struct {
	name string
	file *ast.File
	fset *token.FileSet
}

// funcsR364 — объявления функций файла (без пустых тел: внешние/линкуемые).
func funcsR364(file *ast.File) []*ast.FuncDecl {
	var out []*ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
			out = append(out, fn)
		}
	}
	return out
}

// findFuncR364 — объявление функции по имени; отсутствие — провал теста.
func findFuncR364(t *testing.T, name string) *ast.FuncDecl {
	t.Helper()
	for _, pf := range parsePackageFilesR364(t) {
		for _, fn := range funcsR364(pf.file) {
			if fn.Name.Name == name {
				return fn
			}
		}
	}
	t.Fatalf("функция %s не найдена — тест проверяет не то, что задумано", name)
	return nil
}

// isHTTPHandlerR364 — принимает ли функция *http.Request.
func isHTTPHandlerR364(fn *ast.FuncDecl) bool {
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

// callNamesR364 — имена всех вызванных функций в теле (включая вложенные литералы
// и `go`/`defer`: фоновая горутина — тот же путь загрузки). Для вызовов вида
// `backend.LoadModelWithOpts(...)` берётся последний сегмент.
func callNamesR364(fn *ast.FuncDecl) map[string]bool {
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

// anyDoorR364 — зовёт ли обработчик одну из дверей профиля.
func anyDoorR364(calls map[string]bool) bool {
	for _, door := range profileDoorsR364 {
		if calls[door] {
			return true
		}
	}
	return false
}

// containsNameR364 — есть ли имя в списке.
func containsNameR364(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
