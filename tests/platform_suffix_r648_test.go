//go:build llama_stub

// platform_suffix_r648_test.go — R648 (2026-11-20).
//
// ЖИВОЙ ОТКАЗ, из-за которого появился страж. R621 (закрытие R5) переименовал
// internal/balancer/stats_windows_r493.go → stats_windows.go «по теме». В Go суффикс
// `_windows` в ИМЕНИ файла — не слово в названии, а НЕЯВНОЕ ограничение GOOS: файл
// попадает в сборку только под Windows.
//
// Заметить это было нечем: тесты и CI идут под Windows (файл есть, всё зелено), а
// Linux-сборка впервые случилась при выпуске образов r261120 — балансер упал внутри
// docker build на `undefined: statsWindowsR493`. R599–R647 (54 коммита) жили с этим:
// ни один шаг не собирал под Linux, поэтому ни один страж не мог покраснеть.
//
// ПРАВИЛО. Если имя .go-файла оканчивается на платформенный токен (GOOS/GOARCH), в
// файле обязан быть ЯВНЫЙ //go:build. Тогда суффикс перестаёт быть секретом: видно,
// что файл платформенный, и случайное переименование «по теме» ловится стражем, а не
// выпуском образов. Настоящие платформенные файлы (internal/agent/nvml_unix.go,
// nvml_windows.go, system_windows.go) тег имеют — страж их не трогает.
package tests

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// platformTokensR648 — значения GOOS/GOARCH, которые Go распознаёт в имени файла
// (go/build: name_GOOS.go, name_GOARCH.go, name_GOOS_GOARCH.go).
var platformTokensR648 = map[string]bool{
	// GOOS
	"aix": true, "android": true, "darwin": true, "dragonfly": true, "freebsd": true,
	"hurd": true, "illumos": true, "ios": true, "js": true, "linux": true,
	"netbsd": true, "openbsd": true, "plan9": true, "solaris": true, "wasip1": true,
	"windows": true, "zos": true, "unix": true,
	// GOARCH
	"386": true, "amd64": true, "arm": true, "arm64": true, "loong64": true,
	"mips": true, "mips64": true, "mips64le": true, "mipsle": true,
	"ppc64": true, "ppc64le": true, "riscv64": true, "s390x": true, "wasm": true,
}

// platformSkipDirsR648 — что не наш код (совпадает с остальными сторожами tests/).
var platformSkipDirsR648 = map[string]bool{
	".git":         true,
	"node_modules": true,
	"dist":         true,
	"test-results": true,
	"llama.cpp":    true, // vendored submodule целиком
}

// platformSuffixOfR648 — платформенный токен в конце имени файла, если он там есть.
func platformSuffixOfR648(name string) string {
	base := strings.TrimSuffix(name, ".go")
	parts := strings.Split(base, "_")
	if len(parts) < 2 {
		return ""
	}
	last := parts[len(parts)-1]
	if platformTokensR648[last] {
		return last
	}
	return ""
}

// hasExplicitBuildTagR648 — есть ли в заголовке файла явный //go:build.
//
// Именно ЯВНЫЙ: суффикс имени тоже ограничивает сборку, но молча — а молчаливое
// ограничение и есть дефект, который сторожит этот тест.
func hasExplicitBuildTagR648(content []byte) bool {
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "\ufeff"))
		if strings.HasPrefix(line, "//go:build") {
			return true
		}
		if strings.HasPrefix(line, "package ") {
			return false // заголовок кончился, дальше тег действовать не может
		}
	}
	return false
}

func TestR648_PlatformSuffixedFilesHaveExplicitBuildTag(t *testing.T) {
	root := ".."
	var offenders []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if platformSkipDirsR648[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		token := platformSuffixOfR648(d.Name())
		if token == "" {
			return nil
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if !hasExplicitBuildTagR648(content) {
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				rel = path
			}
			offenders = append(offenders, strings.ReplaceAll(rel, "\\", "/")+" (суффикс _"+token+")")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("обход дерева: %v", err)
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("файлы с платформенным суффиксом в имени, но без явного //go:build:\n  %s\n\n"+
			"Суффикс _windows/_linux/_amd64 и т.п. в имени .go-файла — это НЕЯВНОЕ ограничение сборки "+
			"(файл выпадает из остальных платформ), а не часть названия.\n"+
			"R621 так переименовал internal/balancer/stats_windows_r493.go → stats_windows.go, и Linux-сборка "+
			"балансера 54 коммита падала с `undefined: statsWindowsR493` — поймал только выпуск образов, "+
			"потому что тесты и CI идут под Windows.\n"+
			"Лечение: переименовать файл так, чтобы платформенный токен не был последним "+
			"(stats_time_window.go), либо добавить ЯВНЫЙ //go:build, если файл действительно платформенный.\n"+
			"Подробности — docs/architecture/debts.ru.md, §15d.",
			strings.Join(offenders, "\n  "))
	}
}
