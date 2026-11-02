package balancer

// feature_switches_steward_test.go — R387 (2026-11-02): стюард «переключатель
// без документа не живёт».
//
// ЗАЧЕМ. После R385 стало видно класс «потерянного функционала»: поведение есть,
// но спрятано за флагом/политикой, и оператор видит «не работает», а
// разработчик — «так и задумано». Пример: LB_AUTO_CONTINUE_ON_TRUNCATION и
// LB_OPENAI_AUTO_STREAM по умолчанию ВЫКЛЮЧЕНЫ; LB_OLLAMA_NATIVE_PATH включён и
// меняет форму ответа. Без реестра это выясняется чтением исходников.
//
// ЧТО ДЕЛАЕТ ТЕСТ. Сканирует `os.Getenv("LB_*"/"CPPWORKER_*")` в коде (без
// _test.go) и требует, чтобы КАЖДОЕ имя было в
// docs/architecture/feature-switches.ru.md. Новая переменная без записи в
// реестр — падение здесь, а не «почему у оператора не работает».

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const featureSwitchesDocR387 = "../../docs/architecture/feature-switches.ru.md"

// envSwitchReadReR387 — чтение переключателя из кода.
var envSwitchReadReR387 = regexp.MustCompile(`os\.Getenv\("((?:LB|CPPWORKER)_[A-Z0-9_]+)"\)`)

// TestR387_STEWARD_EverySwitchIsDocumented — главная проверка.
func TestR387_STEWARD_EverySwitchIsDocumented(t *testing.T) {
	rawDoc, err := os.ReadFile(featureSwitchesDocR387)
	if err != nil {
		t.Fatalf("не прочитать реестр переключателей: %v", err)
	}
	doc := string(rawDoc)

	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("корень репозитория: %v", err)
	}
	switches := map[string]string{} // имя → первый файл, где встретилось
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == "c" {
				return filepath.SkipDir
			}
			// Интересует только боевой код: доки/тесты/фронт не считаем.
			if name == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		for _, m := range envSwitchReadReR387.FindAllSubmatch(raw, -1) {
			name := string(m[1])
			if _, ok := switches[name]; !ok {
				rel, _ := filepath.Rel(root, path)
				switches[name] = rel
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("обход репозитория: %v", err)
	}
	if len(switches) < 50 {
		t.Fatalf("найдено %d переключателей — стюард проверяет не то дерево", len(switches))
	}

	var missing []string
	for name, file := range switches {
		if strings.Contains(doc, name) {
			continue
		}
		missing = append(missing, name+" ("+file+")")
	}
	if len(missing) > 0 {
		t.Errorf("эти переключатели читаются кодом, но НЕ описаны в %s:\n    %s\n"+
			"    Оператор не узнает, что фича выключена по умолчанию, пока не прочитает исходник.",
			featureSwitchesDocR387, strings.Join(missing, "\n    "))
	}

	// Обратная проверка: документ не должен ссылаться на несуществующие
	// переключатели (иначе реестр врёт про то, чего в коде нет).
	var stale []string
	for _, m := range envSwitchReadReR387.FindAllStringSubmatch(doc, -1) {
		if _, ok := switches[m[1]]; !ok {
			stale = append(stale, m[1])
		}
	}
	if len(stale) > 0 {
		t.Errorf("реестр описывает переключатели, которых нет в коде: %v", stale)
	}
}
