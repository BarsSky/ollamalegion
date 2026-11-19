//go:build llama_stub

// structure_guard_r506_test.go — R506 (2026-11-19), план
// docs/plans/2026-11-19-readability-refactor.md, шаги R1/R5.
//
// ЗАЧЕМ СТРАЖ, А НЕ ПРОСТО ПРАВИЛО В ДОКЕ. Долг «файлы с номером раунда в имени»
// вырос до 277 боевых файлов — потому что запрета не было, а имя `*_rNNN.go`
// отвечает на вопрос «когда» вместо «что здесь лежит». Останавливать рост надо
// механизмом, иначе следующая правка снова назовёт файл по раунду.
//
// ИСТОРИЯ (R628, 2026-11-19). Сначала у стража был список-исключение на 277 путей
// (docs/architecture/structure-baseline-r506.txt) и правило «новых сверх списка
// нельзя; исчезнувшая запись — красный». Шаг R5 переименовал все 277 файлов, а
// список остался прежним — и страж стал красным на 277 сообщений «в списке-базе есть
// X, но такого файла нет». Это заметили только сейчас, потому что прогоняли
// `./internal/...`, а пакет `./tests/` в тот круг не попал. Вывод: список-исключение
// — долг с отложенной оплатой; после достижения нуля его надо не «обновлять», а
// удалять.
//
// СЕЙЧАС ПРАВИЛО ПРОСТОЕ И БЕЗ СПИСКА: боевых файлов с `_rNNN` в имени — ноль,
// точка. Файл-список удалён в R628, исключений нет.
//
// Тесты (`*_test.go`, `*.test.js`) исключены осознанно: по их именам ищут
// регрессию конкретного раунда (см. §3 и R5 плана).
package tests

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// rnnnReR506 — имя ФАЙЛА с номером раунда (r + три цифры и больше).
const rnnnReR506 = `_r\d{3,}`

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
// R506-fix: пути сравнивались со списком, записанным через `/`, но первые версии
// кода передавали пути с обратными слэшами. На Windows `filepath.ToSlash` их меняет,
// на Linux — нет (это no-op), поэтому все записи выглядели «устаревшими», и CI-job
// «Test tests/... (integration, -short)» краснел на Linux при зелёном локальном
// прогоне. Нормализация должна быть одна и без участия ОС.
//
// R628: здесь же снимается BOM — путь может прийти из файла, пересохранённого
// редактором Windows (PS 5.1 `Set-Content -Encoding utf8` пишет BOM), и тогда
// сравнение путей врёт молча.
func normalizeStructurePathR506(p string) string {
	return strings.ReplaceAll(strings.TrimSpace(strings.TrimPrefix(p, "\ufeff")), `\`, "/")
}

// TestR506_NormalizeStructurePathIsOSIndependent — нормализация не должна
// зависеть от ОС: `filepath.ToSlash` на Linux не меняет обратные слэши, из-за
// чего путь «не находил» сам себя (CI краснел на Linux).
func TestR506_NormalizeStructurePathIsOSIndependent(t *testing.T) {
	cases := map[string]string{
		`internal\balancer\a_r100.go`:             "internal/balancer/a_r100.go",
		"internal/balancer/b_r100.go":             "internal/balancer/b_r100.go",
		`  pkg\logger\c_r489.go  `:                "pkg/logger/c_r489.go",
		"\ufeffinternal\\balancer\\d_r100.go":     "internal/balancer/d_r100.go",
		"\ufeff  webui\\js\\modules\\e_r489.js  ": "webui/js/modules/e_r489.js",
	}
	for in, want := range cases {
		if got := normalizeStructurePathR506(in); got != want {
			t.Errorf("normalizeStructurePathR506(%q) = %q, ожидалось %q", in, got, want)
		}
	}
}

// TestR506_NoRoundNamedProdFiles — боевых файлов с `_rNNN` в имени быть не должно.
//
// Исключений нет: шаг R5 плана завершён (R599–R621, 277 → 0). Красный тест означает,
// что работу R5 начали откатывать — назовите файл по теме, а не по раунду.
func TestR506_NoRoundNamedProdFiles(t *testing.T) {
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
		t.Errorf("боевой файл с номером раунда в имени: %s — назовите файл по теме "+
			"(шаг R5 плана), а не по номеру раунда", p)
	}

	t.Logf("боевых файлов с _rNNN: %d (цель и текущее состояние — 0)", len(found))
}
