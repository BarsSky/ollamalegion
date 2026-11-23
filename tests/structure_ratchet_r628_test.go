//go:build llama_stub

// structure_ratchet_r628_test.go — R628 (2026-11-19), план
// docs/plans/2026-11-19-readability-refactor.md, шаги R3 и R6.
//
// ЗАЧЕМ СТРАЖ. Цель §5 плана — «файлов >= 1500 строк: <= 3». Число в документе ничего
// не останавливает: за R623–R628 гигантов стало меньше ровно потому, что перенос
// делали руками и пересчитывали скриптом. Стоило перестать считать — файл снова
// растёт (этот класс долга уже случался: `_rNNN`-имена выросли до 277 файлов, пока
// не появился страж R506). Здесь тот же приём для размера: список-база
// docs/architecture/size-baseline-r628.txt, он **только уменьшается**.
//
// ПРАВИЛА (оба — красный тест, не предупреждение):
//  1. новый боевой файл >= 1500 строк, которого нет в базе → красный:
//     такой файл надо резать сразу, а не «потом».
//  2. запись в базе исчезла или ушла ниже 1500 строк → красный, пока её не удалят:
//     иначе счётчик «сколько осталось» врёт в меньшую сторону, и цель §5 «достигнута»
//     без работы.
//
// МЕТРИКА. Ровно как в scripts/structure_report.ps1 — непустые строки
// (`Measure-Object -Line`), расширения .go/.c/.h/.js, без *_test.go / *.test.js,
// без вендорного llama.cpp, node_modules, dist, test-results. Одна метрика на страж и
// на отчёт — иначе они спорят друг с другом (этот спор уже был в R506 с путями).
//
// ПОВЕДЕНИЕ НЕ ЛЕЧИТСЯ ЭТИМ ТЕСТОМ: тест не режет файлы, он лишь не даёт незаметно
// добавить новый. Сама работа — вынос слоёв (R623–R628) — остаётся ручной.
package tests

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const (
	sizeBaselineR628 = "../docs/architecture/size-baseline-r628.txt"
	// sizeThresholdR628 — порог «гиганта» из §5 плана.
	sizeThresholdR628 = 1500
)

// sizeSkipDirsR628 — что не наш код (совпадает с structure_report.ps1).
var sizeSkipDirsR628 = map[string]bool{
	".git":         true,
	"node_modules": true,
	"dist":         true,
	"test-results": true,
	"llama.cpp":    true, // vendored submodule целиком
	// R647 (решение владельца, 2026-11-19, вариант A): критерий §5 считается ТОЛЬКО по
	// предмету работ — Go-код и WebUI-модули. Данные словарей (webui/js/i18n) и C-мост
	// (c/bridge: bridge.c и его cgo-биндинг) из метрики ИСКЛЮЧЕНЫ осознанно: словари
	// делить не надо (это данные), а деление C-кода на горячем пути llama.cpp —
	// отдельный риск, не про читаемость. Причина записана в docs/architecture/debts.ru.md §15c.
	"i18n":   true,
	"bridge": true,
	// R712 (2026-11-25): `_diag` — каталог диагностики, а не боевой код. Он в
	// .gitignore (`/_diag/`) и в CI его нет вовсе: там лежат дампы заголовков
	// llama.cpp для диффа (new-ggml.h 2426, old-ggml.h 2416 и т.п.). Считая их
	// «нашими файлами», страж краснел на чужих выгрузках и не говорил ничего о
	// коде. Исключение узкое — по имени каталога, как у i18n/bridge выше, и
	// повторено в scripts/structure_report.ps1, чтобы метрики не разъехались.
	"_diag": true,
}

// sizeExtsR628 — расширения боевого кода (совпадает с structure_report.ps1).
var sizeExtsR628 = map[string]bool{".go": true, ".c": true, ".h": true, ".js": true}

// trimBOMR628 — снять BOM, если файл сохранён «блокнотом» или PowerShell 5.1
// (`Set-Content -Encoding utf8` пишет BOM).
//
// ЗАЧЕМ. Найдено мутацией: правка списка-базы через PS 5.1 добавила BOM перед
// первым символом, и страж отрапортовал «в базе есть <BOM># size-baseline-r628.txt …».
// Сообщение верное по форме и бессмысленное по сути: путаница на 20 строк вместо
// ответа «поправьте путь». BOM не должен влиять на смысл файла.
func trimBOMR628(s string) string {
	return strings.TrimSpace(strings.TrimPrefix(s, "\ufeff"))
}

// loadSizeBaselineR628 — пути из списка-базы (без комментариев и пустых строк).
func loadSizeBaselineR628(t *testing.T) map[string]bool {
	t.Helper()
	f, err := os.Open(sizeBaselineR628)
	if err != nil {
		t.Fatalf("чтение списка-базы размеров: %v", err)
	}
	defer f.Close()
	base := make(map[string]bool)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := trimBOMR628(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		base[normalizeStructurePathR506(line)] = true
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("чтение списка-базы размеров: %v", err)
	}
	if len(base) == 0 {
		// R656–R658 (2026-11-20): пустой список — это ЦЕЛЬ, а не вырожденный страж.
		// Когда все гиганты разложены, база пустеет; правило 1 (ни одного НОВОГО файла
		// >= 1500 строк) на пустой базе как раз и означает «гигантов быть не должно»,
		// а правило 2 (устаревшие записи) на пустом списке проверять нечего.
		t.Logf("список-база размеров пуст: в предмете работ нет файлов >= %d строк", sizeThresholdR628)
	}
	return base
}

// nonBlankLinesR628 — число непустых строк, как `Get-Content | Measure-Object -Line`.
//
// Считаем по `\n`, а не через bufio.Scanner: файлы у нас LF, но если в файле окажется
// CRLF, Scanner тоже отдаст строки — разницы в ответе не будет. Пустая последняя
// строка (файл кончается переводом строки) счётчиком не считается — как и в PowerShell.
func nonBlankLinesR628(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n, nil
}

// scanSizeR628 — все боевые файлы >= порога: rel-путь (прямой слэш) → строк.
func scanSizeR628(t *testing.T) map[string]int {
	t.Helper()
	big := make(map[string]int)
	root := ".."
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if sizeSkipDirsR628[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if strings.HasSuffix(name, "_test.go") || strings.HasSuffix(name, ".test.js") {
			return nil
		}
		if !sizeExtsR628[strings.ToLower(filepath.Ext(name))] {
			return nil
		}
		n, cErr := nonBlankLinesR628(path)
		if cErr != nil {
			t.Fatalf("чтение %s: %v", path, cErr)
		}
		if n < sizeThresholdR628 {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		big[normalizeStructurePathR506(rel)] = n
		return nil
	})
	if err != nil {
		t.Fatalf("обход дерева: %v", err)
	}
	return big
}

// TestR628_NoNewGiantFiles — новых боевых файлов >= 1500 строк быть не должно.
func TestR628_NoNewGiantFiles(t *testing.T) {
	base := loadSizeBaselineR628(t)
	big := scanSizeR628(t)

	var newOnes []string
	for p, n := range big {
		if !base[p] {
			newOnes = append(newOnes, p)
			t.Errorf("новый боевой файл >= %d строк: %s (%d строк) — режьте сразу на слои "+
				"(шаги R3/R6 плана), а не растите дальше", sizeThresholdR628, p, n)
		}
	}

	// Обратная проверка: база обязана уменьшаться вместе с работой.
	var stale []string
	for p := range base {
		if _, ok := big[p]; !ok {
			stale = append(stale, p)
		}
	}
	sort.Strings(stale)
	for _, p := range stale {
		t.Errorf("в базе размеров есть %s, но файл не найден среди гигантов: удалите строку "+
			"из docs/architecture/size-baseline-r628.txt — файл ушёл ниже %d строк, это успех, "+
			"и счётчик должен уменьшиться", p, sizeThresholdR628)
	}

	t.Logf("боевых файлов >= %d строк: %d (в базе %d)", sizeThresholdR628, len(big), len(base))
}

// TestR628_NonBlankLinesMetricMatchesReport — метрика стража = метрика отчёта.
//
// Проверяем на реальном файле: пустые строки не считаются, строки только из пробелов —
// тоже (PowerShell Measure-Object -Line считает «непустые» именно так). Если метрики
// разъедутся, страж и structure_report.ps1 начнут давать разные числа, и разбираться
// в этом будет дороже, чем написать 20 строк теста.
func TestR628_NonBlankLinesMetricMatchesReport(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]struct {
		body string
		want int
	}{
		"без пустых":        {"a\nb\nc\n", 3},
		"с пустыми":         {"a\n\nb\n\n\nc\n", 3},
		"только пробелы":    {"a\n   \nb\n", 2},
		"без финального LF": {"a\nb", 2},
		"пустой файл":       {"", 0},
	}
	for name, tc := range cases {
		p := filepath.Join(dir, "f.txt")
		if err := os.WriteFile(p, []byte(tc.body), 0o644); err != nil {
			t.Fatalf("%s: запись: %v", name, err)
		}
		got, err := nonBlankLinesR628(p)
		if err != nil {
			t.Fatalf("%s: чтение: %v", name, err)
		}
		if got != tc.want {
			t.Errorf("%s: непустых строк %d, ожидалось %d", name, got, tc.want)
		}
	}
}

// TestR628_BaselineLineParsing — строки списка-базы читаются одинаково с BOM и без.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ТЕСТ. Мутация «сохранить список через PS 5.1» дала красный тест с
// бессмысленным сообщением про «# size-baseline-r628.txt» в роли пути. Ловить такое
// глазами в 20-строчном файле — долго; ловится здесь за миллисекунды.
func TestR628_BaselineLineParsing(t *testing.T) {
	cases := map[string]string{
		"обычная запись":       "internal/balancer/proxy.go",
		"с BOM":                "\ufeffinternal/balancer/proxy.go",
		"с BOM и пробелами":    "\ufeff  internal/balancer/proxy.go  ",
		"обратные слэши":       `internal\balancer\proxy.go`,
		"BOM + обратные слэши": "\ufeffinternal\\balancer\\proxy.go",
	}
	const want = "internal/balancer/proxy.go"
	for name, in := range cases {
		if got := normalizeStructurePathR506(trimBOMR628(in)); got != want {
			t.Errorf("%s: %q → %q, ожидалось %q", name, in, got, want)
		}
	}
}
