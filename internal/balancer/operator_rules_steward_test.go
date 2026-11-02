package balancer

// operator_rules_steward_test.go — R386 (2026-11-02): СТЮАРД «правило оператора
// без теста не живёт».
//
// ЗАЧЕМ. Живой случай R385: требование оператора «запрос с большим окном
// поднимает модель» существовало в продукте, но было заменено в R301 на
// противоположное правило, и ТЕСТЫ закрепили замену. Всё зелёное, требование не
// работает. Причина не в конкретной правке, а в отсутствии реестра: правило
// жило только в коде и в тесте, поэтому «обновить тест» выглядело как обычная
// правка.
//
// ЧТО ДЕЛАЕТ ЭТОТ ТЕСТ. Читает docs/architecture/operator-rules.ru.md и требует:
//  1. у КАЖДОГО правила OR-N есть ссылка хотя бы на один тест;
//  2. КАЖДЫЙ упомянутый тест реально существует в репозитории.
//
// Переименовали тест, удалили его или добавили правило без теста — падает здесь,
// а не через месяц в жалобе оператора.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// operatorRulesPathR386 — реестр правил относительно каталога пакета
// (тесты выполняются с cwd = internal/balancer).
const operatorRulesPathR386 = "../../docs/architecture/operator-rules.ru.md"

// testFuncReR386 — имя функции теста, упомянутое в документе.
var testFuncReR386 = regexp.MustCompile(`\bTest[A-Za-z0-9_]+\b`)

// ruleLineReR386 — строка таблицы правил (начинается с "| OR-").
var ruleLineReR386 = regexp.MustCompile(`^\|\s*(OR-\d+)\s*\|`)

// repoTestFuncsR386 — все имена тестовых функций в репозитории.
//
// Сканируем исходники, а не запускаем `go test -list`: стюард обязан работать и
// тогда, когда чужие пакеты не собираются в этой конфигурации (теги сборки), и
// не должен зависеть от окружения.
func repoTestFuncsR386(t *testing.T) map[string]bool {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("корень репозитория: %v", err)
	}
	out := map[string]bool{}
	funcRe := regexp.MustCompile(`(?m)^func\s+(Test[A-Za-z0-9_]+)\s*\(`)
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // нечитаемый каталог не повод валить стюард
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == "c" { // c/ — сабмодуль llama.cpp
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		for _, m := range funcRe.FindAllSubmatch(raw, -1) {
			out[string(m[1])] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("обход репозитория: %v", err)
	}
	if len(out) < 100 {
		t.Fatalf("найдено %d тестовых функций — стюард проверяет не то дерево", len(out))
	}
	return out
}

// TestR386_STEWARD_OperatorRulesHaveLivingTests — главная проверка.
func TestR386_STEWARD_OperatorRulesHaveLivingTests(t *testing.T) {
	raw, err := os.ReadFile(operatorRulesPathR386)
	if err != nil {
		t.Fatalf("не прочитать реестр правил: %v", err)
	}
	lines := strings.Split(string(raw), "\n")
	existing := repoTestFuncsR386(t)

	// 1) Каждое правило обязано ссылаться на живой тест.
	rules := 0
	for _, line := range lines {
		m := ruleLineReR386.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		rules++
		refs := testFuncReR386.FindAllString(line, -1)
		if len(refs) == 0 {
			t.Errorf("%s: в правиле нет ссылки на тест — правило без теста теряется при следующей правке", m[1])
			continue
		}
		for _, ref := range refs {
			if !existing[ref] {
				t.Errorf("%s: тест %q, на который ссылается правило, не существует "+
					"(переименован/удалён?) — обновите правило вместе с тестом", m[1], ref)
			}
		}
	}
	if rules == 0 {
		t.Fatal("в реестре не найдено ни одного правила OR-N — стюард проверяет не то")
	}

	// 2) Каждый упомянутый в документе тест существует.
	for _, ref := range testFuncReR386.FindAllString(string(raw), -1) {
		if !existing[ref] {
			t.Errorf("документ ссылается на несуществующий тест %q", ref)
		}
	}
}
