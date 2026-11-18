//go:build llama_stub

// structure_guard_r506_test.go — R506 (2026-11-19), план
// docs/plans/2026-11-19-readability-refactor.md, шаг R1.
//
// ЗАЧЕМ СТРАЖ, А НЕ ПРОСТО ПРАВИЛО В ДОКЕ. Долг «файлы с номером раунда в имени»
// вырос до 277 боевых файлов — потому что запрета не было, а имя `*_rNNN.go`
// отвечает на вопрос «когда» вместо «что здесь лежит». Останавливать рост надо
// механизмом, иначе следующая правка снова назовёт файл по раунду.
//
// ПРАВИЛО:
//   - новый боевой файл с `_rNNN` в имени → красный тест (список разрешённых —
//     docs/architecture/structure-baseline-r506.txt, он только уменьшается);
//   - файл из списка исчез (переименован или удалён, как требует шаг R5) → красный,
//     пока список не обновлён: иначе счётчик «сколько осталось» врал бы.
//
// Тесты (`*_test.go`, `*.test.js`) исключены осознанно: по их именам ищут
// регрессию конкретного раунда (см. §3 и R5 плана).
package tests

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const (
	structureBaselineR506 = "../docs/architecture/structure-baseline-r506.txt"
	// rnnnReR506 — имя ФАЙЛА с номером раунда (r + три цифры и больше).
	rnnnReR506 = `_r\d{3,}`
)

// structureSkipDirsR506 — что не наш код (submodule, зависимости, артефакты).
var structureSkipDirsR506 = map[string]bool{
	".git":         true,
	"node_modules": true,
	"dist":         true,
	"test-results": true,
	"llama.cpp":    true, // vendored submodule целиком
}

// structureProdExtR506 — расширения боевого кода.
var structureProdExtR506 = map[string]bool{".go": true, ".c": true, ".h": true, ".js": true}

// normalizeStructurePathR506 — прямой слэш НЕЗАВИСИМО от ОС.
//
// R506-fix: список-база записан через `/`, но первые версии файла содержали
// обратные слэши. На Windows `filepath.ToSlash` их меняет, на Linux — нет (это
// no-op), поэтому все записи выглядели «устаревшими», и CI-job
// «Test tests/... (integration, -short)» краснел на Linux при зелёном локальном
// прогоне. Нормализация должна быть одна и без участия ОС.
func normalizeStructurePathR506(p string) string {
	return strings.ReplaceAll(strings.TrimSpace(p), `\`, "/")
}

// loadStructureBaselineR506 — пути из файла-списка (без комментариев).
func loadStructureBaselineR506(t *testing.T) map[string]bool {
	t.Helper()
	f, err := os.Open(structureBaselineR506)
	if err != nil {
		t.Fatalf("чтение списка-базы: %v", err)
	}
	defer f.Close()
	base := make(map[string]bool)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		base[normalizeStructurePathR506(line)] = true
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("чтение списка-базы: %v", err)
	}
	if len(base) == 0 {
		t.Fatal("список-база пуст: страж вырожден")
	}
	return base
}

// TestR506_NormalizeStructurePathIsOSIndependent — нормализация не должна
// зависеть от ОС: `filepath.ToSlash` на Linux не меняет обратные слэши, из-за
// чего список-база «не находил» свои же файлы (CI краснел на Linux).
func TestR506_NormalizeStructurePathIsOSIndependent(t *testing.T) {
	cases := map[string]string{
		`internal\balancer\a_r100.go`: "internal/balancer/a_r100.go",
		"internal/balancer/b_r100.go": "internal/balancer/b_r100.go",
		`  pkg\logger\c_r489.go  `:    "pkg/logger/c_r489.go",
	}
	for in, want := range cases {
		if got := normalizeStructurePathR506(in); got != want {
			t.Errorf("normalizeStructurePathR506(%q) = %q, ожидалось %q", in, got, want)
		}
	}
}

// TestR506_NoNewRoundNamedProdFiles — новых боевых файлов с `_rNNN` быть не должно.
func TestR506_NoNewRoundNamedProdFiles(t *testing.T) {
	base := loadStructureBaselineR506(t)
	re := regexp.MustCompile(rnnnReR506)
	root := ".."

	var found []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if structureSkipDirsR506[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if strings.HasSuffix(name, "_test.go") || strings.HasSuffix(name, ".test.js") {
			return nil
		}
		if !structureProdExtR506[strings.ToLower(filepath.Ext(name))] {
			return nil
		}
		if !re.MatchString(name) {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		found = append(found, normalizeStructurePathR506(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("обход дерева: %v", err)
	}
	sort.Strings(found)

	for _, p := range found {
		if !base[p] {
			t.Errorf("новый боевой файл с номером раунда в имени: %s — назовите файл по теме "+
				"(шаг R5 плана), а не по номеру раунда", p)
		}
	}

	// Обратная проверка: список обязан уменьшаться вместе с переименованиями, а не
	// копиться «на всякий случай». Иначе через год он снова соврёт.
	present := make(map[string]bool, len(found))
	for _, p := range found {
		present[p] = true
	}
	var stale []string
	for p := range base {
		if !present[p] {
			stale = append(stale, p)
		}
	}
	sort.Strings(stale)
	for _, p := range stale {
		t.Errorf("в списке-базе есть %s, но такого файла нет: обновите "+
			"docs/architecture/structure-baseline-r506.txt (переименование — хорошо, "+
			"устаревшая запись — нет)", p)
	}

	t.Logf("боевых файлов с _rNNN сейчас: %d (в базе %d)", len(found), len(base))
}
