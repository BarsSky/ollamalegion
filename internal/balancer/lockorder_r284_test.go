package balancer

// lockorder_r284_test.go — R284 (2026-10-29): сторож рекурсивных захватов p.mu.
//
// ЗАЧЕМ. Живой дедлок (см. lock_deadlock_r284_test.go): под нагрузкой агентских
// метрик балансер вставал навсегда, оставаясь «healthy». Причина — рекурсивный
// RLock: селектор держал p.mu.RLock и вызывал метод, который берёт p.mu.RLock
// снова. Пока писателей нет, вложенный RLock выдаётся, поэтому дефект не виден ни
// на холостом ходу, ни в обычных тестах. Второй такой случай появится так же
// незаметно, если не поставить статический сторож.
//
// ЧТО ПРОВЕРЯЕТСЯ. Тест парсит исходники пакета (без _test.go) и ищет пары
// «функция ДЕРЖИТ p.mu в момент вызова → вызываемый метод берёт p.mu».
// Учитываются порядок операторов, `defer p.mu.Unlock()` (держит до конца
// функции), ветки if/for/switch/select, а тела горутин (`go func(){...}` и
// `go p.method()`) анализируются отдельно — горутина НЕ наследует блокировки
// вызывающего.
//
// Разрешены только методы с суффиксом `Locked`: контракт пакета — такой метод
// НЕ берёт p.mu и требует, чтобы вызывающий уже держал его.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

const (
	lockOwnerRecv  = "p"
	lockOwnerMutex = "mu"
)

// lockRecScanner — состояние сканера.
type lockRecScanner struct {
	fset     *token.FileSet
	lockers  map[string]bool // методы, берущие p.mu
	findings []string
	curFn    string
	curFile  string
	curLine  int
}

// lockState — сколько локов держится в текущей точке (по тексту функции).
type lockState struct {
	read  int
	write int
}

func (s lockState) held() bool { return s.read > 0 || s.write > 0 }

func mergeLock(a, b lockState) lockState {
	out := lockState{}
	if a.read > b.read {
		out.read = a.read
	} else {
		out.read = b.read
	}
	if a.write > b.write {
		out.write = a.write
	} else {
		out.write = b.write
	}
	return out
}

func isProxyMutexCall(call *ast.CallExpr, method string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != method {
		return false
	}
	inner, ok := sel.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := inner.X.(*ast.Ident)
	if !ok {
		return false
	}
	return id.Name == lockOwnerRecv && inner.Sel.Name == lockOwnerMutex
}

func proxyMethod(call *ast.CallExpr) (string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok || id.Name != lockOwnerRecv {
		return "", false
	}
	return sel.Sel.Name, true
}

// collectLockers — методы пакета, которые берут p.mu (Lock/RLock/TryLock/TryRLock).
func (s *lockRecScanner) collectLockers(f *ast.File) {
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil || fd.Recv == nil {
			continue
		}
		takes := false
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				for _, m := range []string{"Lock", "RLock", "TryLock", "TryRLock"} {
					if isProxyMutexCall(call, m) {
						takes = true
					}
				}
			}
			return true
		})
		if takes {
			s.lockers[fd.Name.Name] = true
		}
	}
}

func (s *lockRecScanner) walkBody(body *ast.BlockStmt, st lockState) lockState {
	if body == nil {
		return st
	}
	for _, stmt := range body.List {
		st = s.walkStmt(stmt, st)
	}
	return st
}

func (s *lockRecScanner) walkStmt(stmt ast.Stmt, st lockState) lockState {
	switch x := stmt.(type) {
	case *ast.BlockStmt:
		return s.walkBody(x, st)
	case *ast.ExprStmt:
		return s.walkExpr(x.X, st)
	case *ast.AssignStmt:
		for _, e := range x.Rhs {
			st = s.walkExpr(e, st)
		}
		return st
	case *ast.DeferStmt:
		// defer p.mu.Unlock()/RUnlock() держит лок ДО КОНЦА функции, поэтому
		// разблокировку здесь НЕ применяем: иначе весь хвост функции ошибочно
		// считался бы «вне лока» и сторож пропускал бы реальные рекурсии
		// (именно так первый вариант сторожа не увидел regressive-вызов).
		for _, m := range []string{"Unlock", "RUnlock"} {
			if isProxyMutexCall(x.Call, m) {
				return st
			}
		}
		// Прочие defer-вызовы выполняются на выходе (лок к этому моменту обычно
		// уже отпущен), поэтому их тела не считаем «под локом» — анализируем
		// только аргументы.
		for _, arg := range x.Call.Args {
			st = s.walkExpr(arg, st)
		}
		return st
	case *ast.GoStmt:
		// Горутина не наследует блокировки вызывающего: тело анализируем с нуля,
		// а аргументы вызова (они вычисляются в текущей горутине) — как обычно.
		if lit, ok := x.Call.Fun.(*ast.FuncLit); ok {
			s.walkBody(lit.Body, lockState{})
			return st
		}
		for _, arg := range x.Call.Args {
			st = s.walkExpr(arg, st)
		}
		return st
	case *ast.IfStmt:
		if x.Init != nil {
			st = s.walkStmt(x.Init, st)
		}
		st = s.walkExpr(x.Cond, st)
		a := s.walkBody(x.Body, st)
		b := st
		if x.Else != nil {
			b = s.walkStmt(x.Else, st)
		}
		return mergeLock(a, b)
	case *ast.ForStmt:
		if x.Init != nil {
			st = s.walkStmt(x.Init, st)
		}
		if x.Cond != nil {
			st = s.walkExpr(x.Cond, st)
		}
		out := mergeLock(st, s.walkBody(x.Body, st))
		if x.Post != nil {
			out = s.walkStmt(x.Post, out)
		}
		return out
	case *ast.RangeStmt:
		st = s.walkExpr(x.X, st)
		return mergeLock(st, s.walkBody(x.Body, st))
	case *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt:
		// Для всех «ветвящихся» операторов достаточно консервативной оценки:
		// лок считается удерживаемым, если он удерживается хоть в одной ветке.
		out := st
		var body *ast.BlockStmt
		switch v := stmt.(type) {
		case *ast.SwitchStmt:
			if v.Tag != nil {
				st = s.walkExpr(v.Tag, st)
			}
			body = v.Body
		case *ast.TypeSwitchStmt:
			body = v.Body
		case *ast.SelectStmt:
			body = v.Body
		}
		if body == nil {
			return out
		}
		for _, c := range body.List {
			var inner *ast.BlockStmt
			switch cc := c.(type) {
			case *ast.CaseClause:
				inner = &ast.BlockStmt{List: cc.Body}
			case *ast.CommClause:
				inner = &ast.BlockStmt{List: cc.Body}
			}
			if inner != nil {
				out = mergeLock(out, s.walkBody(inner, st))
			}
		}
		return out
	case *ast.LabeledStmt:
		return s.walkStmt(x.Stmt, st)
	}
	return st
}

func (s *lockRecScanner) walkExpr(e ast.Expr, st lockState) lockState {
	if e == nil {
		return st
	}
	switch x := e.(type) {
	case *ast.CallExpr:
		for _, arg := range x.Args {
			st = s.walkExpr(arg, st)
		}
		if lit, ok := x.Fun.(*ast.FuncLit); ok {
			s.walkBody(lit.Body, lockState{})
			return st
		}
		switch {
		case isProxyMutexCall(x, "RLock"), isProxyMutexCall(x, "TryRLock"):
			st.read++
			return st
		case isProxyMutexCall(x, "RUnlock"):
			if st.read > 0 {
				st.read--
			}
			return st
		case isProxyMutexCall(x, "Lock"), isProxyMutexCall(x, "TryLock"):
			st.write++
			return st
		case isProxyMutexCall(x, "Unlock"):
			if st.write > 0 {
				st.write--
			}
			return st
		}
		if name, ok := proxyMethod(x); ok && st.held() && s.lockers[name] && name != s.curFn {
			if strings.HasSuffix(name, "Locked") {
				// Контракт: ...Locked не берёт p.mu. Если он его всё-таки взял —
				// это нарушение, о котором скажет проверка lockers ниже (метод с
				// суффиксом Locked не должен попадать в lockers).
				return st
			}
			hint := "RLock"
			if st.write > 0 {
				hint = "Lock"
			}
			s.findings = append(s.findings, s.curFn+" ("+s.curFile+":"+fmt.Sprintf("%d", s.curLine)+
				") держит p.mu."+hint+" и вызывает p."+name+
				", который берёт p.mu повторно (рекурсивный захват)")
		}
		return st
	case *ast.BinaryExpr:
		st = s.walkExpr(x.X, st)
		return s.walkExpr(x.Y, st)
	case *ast.UnaryExpr:
		return s.walkExpr(x.X, st)
	case *ast.StarExpr:
		return s.walkExpr(x.X, st)
	case *ast.ParenExpr:
		return s.walkExpr(x.X, st)
	case *ast.SelectorExpr:
		return s.walkExpr(x.X, st)
	case *ast.IndexExpr:
		st = s.walkExpr(x.X, st)
		return s.walkExpr(x.Index, st)
	case *ast.SliceExpr:
		st = s.walkExpr(x.X, st)
		st = s.walkExpr(x.Low, st)
		st = s.walkExpr(x.High, st)
		return s.walkExpr(x.Max, st)
	case *ast.KeyValueExpr:
		st = s.walkExpr(x.Key, st)
		return s.walkExpr(x.Value, st)
	case *ast.CompositeLit:
		for _, el := range x.Elts {
			st = s.walkExpr(el, st)
		}
		return st
	case *ast.FuncLit:
		s.walkBody(x.Body, lockState{})
		return st
	}
	return st
}

// TestR284_NoRecursiveProxyMutex — в пакете нет рекурсивных захватов p.mu.
func TestR284_NoRecursiveProxyMutex(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)

	s := &lockRecScanner{fset: token.NewFileSet(), lockers: map[string]bool{}}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	type parsedFile struct {
		f    *ast.File
		path string
	}
	var files []parsedFile
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		f, err := parser.ParseFile(s.fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, parsedFile{f: f, path: path})
	}
	if len(files) == 0 {
		t.Fatal("не найдено ни одного исходника пакета — сторож бесполезен")
	}

	for _, pf := range files {
		s.collectLockers(pf.f)
	}
	if len(s.lockers) == 0 {
		t.Fatal("не найдено ни одного метода, берущего p.mu — сломан разбор")
	}

	for _, pf := range files {
		for _, d := range pf.f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil || fd.Recv == nil {
				continue
			}
			s.curFn = fd.Name.Name
			s.curFile = filepath.Base(pf.path)
			s.curLine = s.fset.Position(fd.Pos()).Line
			s.walkBody(fd.Body, lockState{})
		}
	}

	// Дедупликация: одно и то же место может встретиться несколько раз
	// (вызов в ветках, повторный проход по вложенным выражениям).
	seen := map[string]bool{}
	var uniq []string
	for _, f := range s.findings {
		if seen[f] {
			continue
		}
		seen[f] = true
		uniq = append(uniq, f)
	}
	sort.Strings(uniq)

	if len(uniq) > 0 {
		t.Fatalf("найдены рекурсивные захваты p.mu (%d). RWMutex не рекурсивен: под "+
			"ожидающим писателем второй RLock не будет выдан никогда — балансер встанет "+
			"навсегда, оставаясь «healthy». Используйте ...Locked-вариант или снимок "+
			"данных под коротким локом:\n  %s",
			len(uniq), strings.Join(uniq, "\n  "))
	}
}

// TestR284_LockedVariantsDoNotTakeMutex — методы с суффиксом Locked НЕ должны
// брать p.mu: их контракт — «лок уже держит вызывающий». Иначе ...Locked-вариант
// вернёт ту же рекурсию, только незаметнее.
func TestR284_LockedVariantsDoNotTakeMutex(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	fset := token.NewFileSet()
	violations := []string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			continue
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil || fd.Recv == nil || !strings.HasSuffix(fd.Name.Name, "Locked") {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					for _, m := range []string{"Lock", "RLock"} {
						if isProxyMutexCall(call, m) {
							violations = append(violations, fmt.Sprintf("%s (%s:%d) берёт p.mu.%s, хотя обязан работать под локом вызывающего",
								fd.Name.Name, filepath.Base(name), fset.Position(fd.Pos()).Line, m))
						}
					}
				}
				return true
			})
		}
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("нарушен контракт ...Locked:\n  %s", strings.Join(violations, "\n  "))
	}
}
