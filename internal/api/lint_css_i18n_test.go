// Package api — linter regression test для webui style compliance.
//
// Phase 7 (2026-07-10): WebUI style compliance — ensures em-dash (U+2014) does
// not reappear in CSS comments or i18n file headers after Phase 7.1 cleanup.
//
// Что проверяет:
//   1. webui/css/*.css — em-dash внутри /* ... */ комментариев не допускается
//      (comment-only marker; считаются "плохой" typography в CSS-коде, т.к.
//      em-dash это литературный знак, а в коде ожидается ASCII).
//      В user-facing CSS rules em-dash отсутствует (никогда не было).
//   2. webui/js/i18n/en.js и ru.js — только первая строка (file header).
//      Внутри i18n-объекта em-dash ДОПУСТИМ: это легитимная русская/английская
//      типографика в user-facing copy ("P1 - Model Affinity" — это пунктуация).
//
// Что НЕ проверяет:
//   - User-facing copy внутри i18n-объектов (P1-P4, help text) — там em-dash
//     легитимен как типографика в русском/английском тексте.
//   - rgba() в CSS — Phase 7.2 показал pattern (color-mix() для accent-derived
//     variants), но 100% конверсия 120 rgba() оставлена для будущих итераций
//     (issue doc помечает как low-priority).
//
// Зачем: regression-guard. После Phase 7.1 9 em-dash заменены на `-`. Если
// кто-то вставит em-dash обратно (копи-паст из Word, генератор кода, и т.д.)
// — test упадёт с указанием file:line.
package api

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// emDash is the U+2014 character we want to keep out of CSS comments and
// i18n file headers.
const emDash = "—"

// findRepoRoot walks up from this test file to find the repo root
// (identified by go.mod at the root). Allows the test to be run from
// any working directory.
func findRepoRoot(t *testing.T) string {
	t.Helper()
	// The test is at internal/api/lint_css_i18n_test.go. Walk up 2 levels
	// to find the repo root.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	for i := 0; i < 5; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Skip("repo root with go.mod not found; skipping lint test (CI may run from subdir)")
	return ""
}

// TestLintCSSAndI18nNoEmDash is the regression test for Phase 7.1.
//
// Scans all .css files in webui/css/ for em-dash inside comment blocks, and
// the first line of webui/js/i18n/{en,ru}.js. Fails with a list of file:line
// locations for any violations.
func TestLintCSSAndI18nNoEmDash(t *testing.T) {
	repoRoot := findRepoRoot(t)
	webuiDir := filepath.Join(repoRoot, "webui")
	if _, err := os.Stat(webuiDir); os.IsNotExist(err) {
		t.Skipf("webui dir not found at %s; skipping (CI without webui checkout?)", webuiDir)
	}

	var violations []string

	// 1. CSS files: scan all .css under webui/css/ for em-dash inside /* ... */
	cssDir := filepath.Join(webuiDir, "css")
	cssEntries, err := os.ReadDir(cssDir)
	if err != nil {
		t.Skipf("cannot read %s: %v", cssDir, err)
	}
	for _, e := range cssEntries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".css") {
			continue
		}
		path := filepath.Join(cssDir, e.Name())
		checkCSSFileEmDash(path, &violations)
	}

	// 2. i18n file headers: only line 1 of en.js and ru.js.
	for _, name := range []string{"en.js", "ru.js"} {
		path := filepath.Join(webuiDir, "js", "i18n", name)
		checkI18nHeaderEmDash(path, &violations)
	}

	if len(violations) > 0 {
		t.Errorf(
			"Phase 7 lint: found %d em-dash violation(s) in webui/. "+
				"Use ASCII hyphen (-) instead. Locations:\n  %s\n"+
				"See docs/phase-7-style-compliance.md for the convention.",
			len(violations), strings.Join(violations, "\n  "),
		)
	}
}

// checkCSSFileEmDash reads a CSS file and reports em-dash inside /* ... */
// comment blocks via violations slice. Lines outside comments are ignored
// (CSS rules don't have em-dash, but we don't want false positives in case
// someone adds a URL or selector with em-dash).
func checkCSSFileEmDash(path string, violations *[]string) {
	content, err := os.ReadFile(path)
	if err != nil {
		return
	}
	rel, _ := filepath.Rel(filepath.Dir(path), path)
	_ = rel
	// Walk char-by-char tracking block comment state.
	inBlock := false
	lineNum := 1
	colNum := 0
	for i := 0; i < len(content); i++ {
		ch := content[i]
		colNum++
		if ch == '\n' {
			lineNum++
			colNum = 0
		}
		// Detect /* ... */ transitions
		if !inBlock && i+1 < len(content) && ch == '/' && content[i+1] == '*' {
			inBlock = true
			i++ // skip '*'
			colNum++
			continue
		}
		if inBlock && i+1 < len(content) && ch == '*' && content[i+1] == '/' {
			inBlock = false
			i++
			colNum++
			continue
		}
		// Inside block comment, look for em-dash
		if inBlock {
			// Compare bytes (em-dash is 3 bytes in UTF-8: 0xE2 0x80 0x94)
			if ch == 0xE2 && i+2 < len(content) &&
				content[i+1] == 0x80 && content[i+2] == 0x94 {
				// Build a relative-ish path for readability
				relPath := path
				if cwd, err := os.Getwd(); err == nil {
					if r, err := filepath.Rel(cwd, path); err == nil {
						relPath = r
					}
				}
				*violations = append(*violations,
					relPath+":"+lintItoa(lineNum)+":"+lintItoa(colNum))
				i += 2 // skip the remaining 2 bytes of em-dash
				colNum += 2
			}
		}
	}
}

// checkI18nHeaderEmDash checks the first line of an i18n JS file for em-dash.
// The file header is the only place em-dash is forbidden (it's a // comment
// describing the file, not user-facing copy). Strings inside the I18N_XX
// object are allowed (Russian/English typography).
func checkI18nHeaderEmDash(path string, violations *[]string) {
	content, err := os.ReadFile(path)
	if err != nil {
		return
	}
	// Find first newline
	nlIdx := strings.Index(string(content), "\n")
	if nlIdx < 0 {
		nlIdx = len(content)
	}
	header := string(content[:nlIdx])
	if strings.ContainsRune(header, '—') {
		// em-dash is in header
		relPath := path
		if cwd, err := os.Getwd(); err == nil {
			if r, err := filepath.Rel(cwd, path); err == nil {
				relPath = r
			}
		}
		*violations = append(*violations, relPath+":1 (header line)")
	}
}

// itoa is a local int-to-string helper to avoid importing strconv in the
// hot path (compiler may inline it). Kept simple — no error handling.
func lintItoa(n int) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// TestI18nKeyParity_EN_RU — проверяет, что webui/js/i18n/en.js и ru.js
// содержат одинаковый набор ключей (1073+).
//
// Зачем: при добавлении нового ключа легко забыть синхронизировать оба файла.
// Этот тест гарантирует parity — иначе language switch показывает fallback.
//
// Phase 8 (2026-07-11): добавлен как часть i18n hardening.
func TestI18nKeyParity_EN_RU(t *testing.T) {
	repoRoot := findRepoRoot(t)
	enPath := filepath.Join(repoRoot, "webui", "js", "i18n", "en.js")
	ruPath := filepath.Join(repoRoot, "webui", "js", "i18n", "ru.js")

	enKeys := extractI18nKeys(t, enPath)
	ruKeys := extractI18nKeys(t, ruPath)

	if len(enKeys) != len(ruKeys) {
		t.Errorf("i18n key count mismatch: en.js=%d, ru.js=%d (expected equal)",
			len(enKeys), len(ruKeys))
	}

	// In en.js but missing from ru.js.
	missingInRu := diffKeys(enKeys, ruKeys)
	if len(missingInRu) > 0 {
		t.Errorf("keys in en.js but missing from ru.js (%d):\n  %s\n"+
			"Add these keys to ru.js (or remove from en.js if obsolete).",
			len(missingInRu), joinKeys(missingInRu, 20))
	}

	// In ru.js but missing from en.js.
	missingInEn := diffKeys(ruKeys, enKeys)
	if len(missingInEn) > 0 {
		t.Errorf("keys in ru.js but missing from en.js (%d):\n  %s\n"+
			"Add these keys to en.js (or remove from ru.js if obsolete).",
			len(missingInEn), joinKeys(missingInEn, 20))
	}
}

// extractI18nKeys парсит i18n JS-файл и возвращает sorted unique keys.
// Поддерживает формат "key": "value" (внутри window.I18N_EN = {...}).
func extractI18nKeys(t *testing.T, path string) []string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read %s: %v", path, err)
	}
	// Regex: "key": "value" — match quoted key followed by colon.
	re := regexp.MustCompile(`"([a-zA-Z][a-zA-Z0-9._]*)"\s*:\s*"`)
	matches := re.FindAllStringSubmatch(string(content), -1)
	keys := make(map[string]bool)
	for _, m := range matches {
		keys[m[1]] = true
	}
	result := make([]string, 0, len(keys))
	for k := range keys {
		result = append(result, k)
	}
	sort.Strings(result)
	return result
}

func diffKeys(a, b []string) []string {
	bSet := make(map[string]bool, len(b))
	for _, k := range b {
		bSet[k] = true
	}
	var diff []string
	for _, k := range a {
		if !bSet[k] {
			diff = append(diff, k)
		}
	}
	sort.Strings(diff)
	return diff
}

func joinKeys(keys []string, max int) string {
	if len(keys) <= max {
		return strings.Join(keys, "\n  ")
	}
	return strings.Join(keys[:max], "\n  ") + fmt.Sprintf("\n  ... and %d more", len(keys)-max)
}
