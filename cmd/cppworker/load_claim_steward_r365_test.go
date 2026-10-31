// load_claim_steward_r365_test.go — R365 (2026-10-31): СТЮАРД «заявка на
// загрузку не может утечь из sync-режима».
//
// ЗАЧЕМ. Заявка (`backend.TryLockLoadR303` → `b.loading[name]`) снимается
// ровно одним `UnlockLoad`. Если путь загрузки забывает её снять, следующая
// загрузка того же имени получает «model is already being loaded by another
// request; waiting», ждёт предел ожидания и возвращает 200 с параметрами
// ПРЕДЫДУЩЕЙ загрузки — то есть просьба «перезагрузи с другими параметрами»
// молча не выполняется.
//
// ЖИВОЙ ДЕФЕКТ (R365). В handleLoadModel снятие заявки стояло в ветке успеха
// `select`, а при таймауте (`wait=true`, дефолт 60 с, холодная загрузка gemma-4
// 60–100 с) эта ветка не выполнялась вовсе. Замер: ответ 202 на 61-й секунде,
// «model loaded» на 69-й, а следующая загрузка через 12 с всё ещё видела заявку
// и прождала 90001 мс. В handleLoadWithParams то же самое было исправлено ещё
// в Round 52 (`defer UnlockLoad` в горутине) — legacy-путь просто не поправили.
//
// ЧТО ДЕЛАЕТ ТЕСТ. Разбирает исходники пакета и для каждого обработчика,
// который грузит модель в СИНХРОННОМ режиме (создаёт `loadDone`-канал и
// запускает горутину с LoadModelWithOpts), требует:
//
//  1. `defer ...UnlockLoad(...)` ВНУТРИ горутины загрузки — единственное место,
//     которое выполняется при любом исходе select;
//  2. отсутствие `UnlockLoad` в самой ветке `select` — иначе снятие заявки
//     снова окажется там, где таймаут её обходит.
//
// ЧЕГО ТЕСТ НЕ ДЕЛАЕТ: не проверяет Lock/Unlock на уровне рантайма (это
// сделано живым замером в docs/plans/2026-10-31-load-claim-sync-timeout.md) и
// не трогает async-путь: там runAsyncLoad снимает заявку сам.
package main

import (
	"go/ast"
	"testing"
)

// syncLoadClaimHandlersR365 — обработчики, у которых есть sync-ветка с
// каналом loadDone. Список обязательный: если функция переименована, тест
// падает, а не молчит.
var syncLoadClaimHandlersR365 = []string{"handleLoadModel", "handleLoadWithParams"}

// TestR365STEWARD_SyncLoadReleasesClaimInGoroutine — проверки 1 и 2.
func TestR365STEWARD_SyncLoadReleasesClaimInGoroutine(t *testing.T) {
	files := parsePackageFilesR365(t)

	for _, want := range syncLoadClaimHandlersR365 {
		fn, pos := findFuncR365(t, files, want)

		goroutines := loadGoroutinesR365(fn)
		if len(goroutines) == 0 {
			t.Errorf("%s (%s): не найдена горутина загрузки с LoadModelWithOpts — "+
				"тест проверяет не то, что задумано (sync-ветку переписали?)", pos, want)
			continue
		}
		guarded := false
		for _, fnLit := range goroutines {
			if defersUnlockR365(fnLit) {
				guarded = true
			}
		}
		if !guarded {
			t.Errorf("%s (%s): горутина загрузки НЕ снимает заявку через defer UnlockLoad.\n"+
				"    При таймауте sync-ветки (wait=true, дефолт 60 с, холодная загрузка 60–100 с)\n"+
				"    заявка останется в b.loading, и следующая загрузка того же имени прождёт\n"+
				"    предел ожидания и вернёт 200 с ПАРАМЕТРАМИ ПРЕДЫДУЩЕЙ загрузки — молча.\n"+
				"    Ставь `defer backend.UnlockLoad(name)` ВНУТРЬ горутины (как в Round 52).", pos, want)
		}
		if unlockInSelectR365(fn) {
			t.Errorf("%s (%s): UnlockLoad вызывается в ветке select.\n"+
				"    Именно это и стреляло в R365: таймаут уходит мимо ветки успеха,\n"+
				"    поэтому заявку обязан снимать defer в горутине, а не select.", pos, want)
		}
	}
}

// loadGoroutinesR365 — функциональные литералы, запущенные через `go` и
// вызывающие LoadModelWithOpts.
func loadGoroutinesR365(fn *ast.FuncDecl) []*ast.FuncLit {
	var out []*ast.FuncLit
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		stmt, ok := n.(*ast.GoStmt)
		if !ok {
			return true
		}
		lit, ok := stmt.Call.Fun.(*ast.FuncLit)
		if !ok {
			return true
		}
		if !bodyCallsR365(lit.Body, "LoadModelWithOpts") {
			return true
		}
		out = append(out, lit)
		return true
	})
	return out
}

// bodyCallsR365 — есть ли в теле вызов функции с таким именем (последний
// сегмент селектора).
func bodyCallsR365(body *ast.BlockStmt, name string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch callee := call.Fun.(type) {
		case *ast.Ident:
			if callee.Name == name {
				found = true
			}
		case *ast.SelectorExpr:
			if callee.Sel.Name == name {
				found = true
			}
		}
		return true
	})
	return found
}

// defersUnlockR365 — есть ли внутри литерала `defer ...UnlockLoad(...)`.
func defersUnlockR365(lit *ast.FuncLit) bool {
	found := false
	ast.Inspect(lit.Body, func(n ast.Node) bool {
		def, ok := n.(*ast.DeferStmt)
		if !ok {
			return true
		}
		switch call := def.Call.Fun.(type) {
		case *ast.Ident:
			if call.Name == "UnlockLoad" {
				found = true
			}
		case *ast.SelectorExpr:
			if call.Sel.Name == "UnlockLoad" {
				found = true
			}
		case *ast.FuncLit:
			// `defer func() { backend.UnlockLoad(name) }()` — штатная форма из
			// Round 52 (нужна, чтобы заявку снимал сам defer, а не тело).
			if bodyCallsR365(call.Body, "UnlockLoad") {
				found = true
			}
		}
		return true
	})
	return found
}

// unlockInSelectR365 — вызывается ли UnlockLoad НЕ в горутине, то есть в самом
// обработчике (ветки select, defer функции и т.п.).
func unlockInSelectR365(fn *ast.FuncDecl) bool {
	inGoroutine := map[ast.Node]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		stmt, ok := n.(*ast.GoStmt)
		if !ok {
			return true
		}
		if lit, ok := stmt.Call.Fun.(*ast.FuncLit); ok {
			inGoroutine[lit] = true
		}
		return true
	})

	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var name string
		switch callee := call.Fun.(type) {
		case *ast.Ident:
			name = callee.Name
		case *ast.SelectorExpr:
			name = callee.Sel.Name
		}
		if name != "UnlockLoad" {
			return true
		}
		// Ищем, не находится ли вызов внутри функционального литерала,
		// запущенного через `go`.
		for lit := range inGoroutine {
			if inNode(lit, call) {
				return true
			}
		}
		found = true
		return true
	})
	return found
}

// inNode — находится ли node внутри root (сравнение по диапазону позиций).
func inNode(root, node ast.Node) bool {
	return node.Pos() >= root.Pos() && node.End() <= root.End()
}

// parsePackageFilesR365 — все НЕтестовые .go файлы пакета (разбор общий с
// R364-стюардом: сканер там уже проверен на «пустой разбор»).
func parsePackageFilesR365(t *testing.T) []parsedPackageFileR364 {
	t.Helper()
	return parsePackageFilesR364(t)
}

// findFuncR365 — объявление функции по имени; отсутствие — провал теста.
func findFuncR365(t *testing.T, files []parsedPackageFileR364, name string) (*ast.FuncDecl, string) {
	t.Helper()
	for _, pf := range files {
		for _, fn := range funcsR364(pf.file) {
			if fn.Name.Name == name {
				return fn, pf.fset.Position(fn.Pos()).String()
			}
		}
	}
	// Функцию переименовали/унесли — тест обязан об этом сказать, иначе его
	// список устарел бы и «нет утечки» превратилось бы в «нет проверки».
	t.Fatalf("обработчик %s не найден — обнови syncLoadClaimHandlersR365", name)
	return nil, ""
}
