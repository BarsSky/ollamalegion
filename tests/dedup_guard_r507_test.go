//go:build llama_stub

// dedup_guard_r507_test.go — R507 (2026-11-19), шаг R7 плана
// docs/plans/2026-11-19-readability-refactor.md: дубли хелперов не возвращаются.
//
// ЗАЧЕМ СТРАЖ. Дубли возникают не злом, а привычкой: «рядом нужен
// firstNonEmpty — напишу свою». Четыре копии `firstNonEmpty` в четырёх пакетах
// разошлись обработкой пробелов (три разных поведения под одним именем), две
// копии `defaultIfZero` — семантикой нуля, `minInt`/`maxInt` — тремя пакетами.
// После R7 реализации ровно по одной (`pkg/strutil`, `pkg/numutil`, встроенные
// `min`/`max`), остальные места — алиасы. Тест закрывает путь назад.
package tests

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// dupHelpersR507 — имена, у которых должна остаться ОДНА реализация.
//
// `roundUpPow2`/`RoundUpPow2` здесь НЕ перечислены осознанно: это одна реализация
// (с полом 512 токенов) плюс тонкая обёртка для внешних вызовов — не дубль. Если
// перенести её в общий пакет под нейтральным именем, следующий читатель решит, что
// это чистая математика без пола, и сломает перезагрузку окна.
var dupHelpersR507 = []string{"firstNonEmpty", "minInt", "maxInt", "defaultIfZero"}

// TestR507_NoDuplicateHelperDefinitions — определений этих хелперов в боевом коде
// больше нет (остались только реализации в pkg/strutil, pkg/numutil и алиасы).
func TestR507_NoDuplicateHelperDefinitions(t *testing.T) {
	root := ".."
	skipDirs := map[string]bool{".git": true, "node_modules": true, "dist": true, "test-results": true, "llama.cpp": true}
	patterns := make(map[string]*regexp.Regexp, len(dupHelpersR507))
	for _, name := range dupHelpersR507 {
		patterns[name] = regexp.MustCompile(`func\s+` + regexp.QuoteMeta(name) + `\s*\(`)
	}

	var violations []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		text := string(src)
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		rel = normalizeStructurePathR506(rel)
		for helper, re := range patterns {
			if re.MatchString(text) {
				violations = append(violations, helper+" в "+rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("обход дерева: %v", err)
	}

	for _, v := range violations {
		t.Errorf("снова объявлен хелпер-дубль: %s — реализация одна: pkg/strutil (строки), "+
			"pkg/numutil (числа), встроенные min/max (Go 1.21); здесь должен быть алиас", v)
	}

	// Непустая проверка: реализации в pkg обязаны существовать, иначе «нет дублей»
	// означало бы «нет и функции».
	implHits := 0
	for _, f := range []string{"../pkg/strutil/strutil.go", "../pkg/numutil/numutil.go"} {
		if _, statErr := os.Stat(f); statErr == nil {
			implHits++
		}
	}
	if implHits != 2 {
		t.Fatalf("не найдены пакеты с единственной реализацией (%d из 2): проверка выродилась", implHits)
	}
}
