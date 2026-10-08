// Package api — lint regression test для полноты локализации WebUI.
//
// ЗАЧЕМ. `webui/js/i18n/index.js` при отсутствии перевода пишет
// `[i18n] Missing translation key: <key>` в консоль И ВОЗВРАЩАЕТ САМ КЛЮЧ
// (index.js:94-102). Отсюда два следствия, оба наблюдались оператором на живой
// стойке:
//
//  1. шум в консоли на каждой отрисовке панели («Missing translation key:
//     clientAccess.internal_label») — при том что у вызова есть текстовый
//     fallback и интерфейс выглядит правильно;
//  2. там, где глобальный I18N.t вызывается БЕЗ обёртки с fallback
//     (например cppworker-params.js: `I18N.t(key, 'текст')` — второй аргумент у
//     глобальной функции это ПОДСТАНОВКИ, а не fallback), пользователь видел
//     сырой ключ вида `settings.profiles.step_busy` вместо текста.
//
// Тест требует, чтобы каждый литеральный ключ, который код передаёт в t()/tr()/_(),
// существовал и в ru.js, и в en.js, и чтобы наборы ключей в этих файлах совпадали.
//
// Ключи-ПРЕФИКСЫ (оканчиваются на `_` или `.`) пропускаются: они склеиваются с
// вычисляемым суффиксом (`t('imageModels.role_' + role)`), и полного ключа в
// исходнике нет по определению.
package api

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// i18nCallRe — литеральный ключ в вызове t('...'), _t('...'), _('...'), tr('...'),
// .t('...') или ._t('...').
//
// R90 (2026-10-08): `_t(` ДОБАВЛЕН, и это не придирка. Старая регулярка требовала
// перед именем функции символ не из [A-Za-z0-9_$.], а у `_t(` непосредственно
// перед `t` стоит `_` — то есть подчёркнутый алиас перевода линт не видел ВООБЩЕ.
// Следствие на живой панели: шесть ключей секции «Состояние бэкенда»
// (metrics.gpu_usage, metrics.vram_usage, metrics.ram_usage, metrics.cpu_usage,
// metrics.gpu_temp, metrics.cpu_temp) отсутствовали в ru.js/en.js, а `_t()` при
// отсутствии перевода возвращает сам ключ — оператор видел в панели
// «metrics.ram_usage» вместо «RAM». Линт обязан ловить и такой вызов.
var i18nCallRe = regexp.MustCompile(
	`(?:^|[^A-Za-z0-9_$])(?:_t|t|_|tr)\(\s*['"]([A-Za-z][A-Za-z0-9_.]*)['"]` +
		`|\._t\(\s*['"]([A-Za-z][A-Za-z0-9_.]*)['"]` +
		`|\.t\(\s*['"]([A-Za-z][A-Za-z0-9_.]*)['"]`)

// i18nEntryRe — ключ в файле переводов: "key": "value".
var i18nEntryRe = regexp.MustCompile(`"([A-Za-z][A-Za-z0-9_.]*)"\s*:`)

// collectI18nKeys — множество ключей из файла переводов.
func collectI18nKeys(path string) (map[string]bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	keys := map[string]bool{}
	for _, m := range i18nEntryRe.FindAllStringSubmatch(string(data), -1) {
		keys[m[1]] = true
	}
	return keys, nil
}

// collectUsedI18nKeys — литеральные ключи, которые передаёт код WebUI.
func collectUsedI18nKeys(t *testing.T, webuiDir string) map[string][]string {
	t.Helper()
	used := map[string][]string{}
	err := filepath.WalkDir(webuiDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "i18n" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".js") || strings.HasSuffix(d.Name(), ".test.js") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, m := range i18nCallRe.FindAllStringSubmatch(string(data), -1) {
			// Групп стало три (t/_t/_, ._t, .t) — берём первую непустую.
			key := ""
			for _, g := range m[1:] {
				if g != "" {
					key = g
					break
				}
			}
			// Префиксы для склейки: реального ключа в исходнике нет.
			if strings.HasSuffix(key, "_") || strings.HasSuffix(key, ".") {
				continue
			}
			used[key] = append(used[key], d.Name())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("обход %s: %v", webuiDir, err)
	}
	return used
}

// TestI18nKeys_AllUsedKeysTranslated — каждый ключ из кода есть в обоих языках.
func TestI18nKeys_AllUsedKeysTranslated(t *testing.T) {
	repoRoot := findRepoRoot(t)
	webuiDir := filepath.Join(repoRoot, "webui", "js")
	if _, err := os.Stat(webuiDir); err != nil {
		t.Skip("webui/js не найден; пропускаем lint")
	}

	ruKeys, err := collectI18nKeys(filepath.Join(webuiDir, "i18n", "ru.js"))
	if err != nil {
		t.Fatalf("чтение ru.js: %v", err)
	}
	enKeys, err := collectI18nKeys(filepath.Join(webuiDir, "i18n", "en.js"))
	if err != nil {
		t.Fatalf("чтение en.js: %v", err)
	}

	used := collectUsedI18nKeys(t, webuiDir)

	var missingRu, missingEn []string
	for key, files := range used {
		sort.Strings(files)
		if !ruKeys[key] {
			missingRu = append(missingRu, key+"  <- "+strings.Join(uniq(files), ", "))
		}
		if !enKeys[key] {
			missingEn = append(missingEn, key+"  <- "+strings.Join(uniq(files), ", "))
		}
	}
	sort.Strings(missingRu)
	sort.Strings(missingEn)

	if len(missingRu) > 0 || len(missingEn) > 0 {
		t.Fatalf("ключи, используемые кодом, но отсутствующие в переводах:\n"+
			"  ru.js (%d):\n    %s\n  en.js (%d):\n    %s\n\n"+
			"Следствие: i18n/index.js пишет warning и возвращает САМ КЛЮЧ, поэтому в консоли шум,\n"+
			"а при вызове глобального I18N.t без обёртки с fallback пользователь видит ключ вместо текста.",
			len(missingRu), strings.Join(missingRu, "\n    "),
			len(missingEn), strings.Join(missingEn, "\n    "))
	}
}

// TestI18nKeys_RuEnParity — наборы ключей в ru.js и en.js совпадают.
//
// Равенство важно не «для красоты»: при отсутствии ключа в текущем языке
// index.js падает обратно на en, поэтому расхождение выглядит как случайный
// английский текст в русском интерфейсе.
func TestI18nKeys_RuEnParity(t *testing.T) {
	repoRoot := findRepoRoot(t)
	i18nDir := filepath.Join(repoRoot, "webui", "js", "i18n")
	if _, err := os.Stat(i18nDir); err != nil {
		t.Skip("webui/js/i18n не найден; пропускаем lint")
	}

	ruKeys, err := collectI18nKeys(filepath.Join(i18nDir, "ru.js"))
	if err != nil {
		t.Fatalf("чтение ru.js: %v", err)
	}
	enKeys, err := collectI18nKeys(filepath.Join(i18nDir, "en.js"))
	if err != nil {
		t.Fatalf("чтение en.js: %v", err)
	}

	var onlyRu, onlyEn []string
	for k := range ruKeys {
		if !enKeys[k] {
			onlyRu = append(onlyRu, k)
		}
	}
	for k := range enKeys {
		if !ruKeys[k] {
			onlyEn = append(onlyEn, k)
		}
	}
	sort.Strings(onlyRu)
	sort.Strings(onlyEn)
	if len(onlyRu) > 0 || len(onlyEn) > 0 {
		t.Fatalf("наборы ключей ru.js и en.js разошлись:\n  только ru (%d): %v\n  только en (%d): %v",
			len(onlyRu), onlyRu, len(onlyEn), onlyEn)
	}
}

// uniq — уникальные элементы с сохранением порядка.
func uniq(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
