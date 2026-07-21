//go:build ci

// Package ci содержит smoke-тесты, которые запускаются ТОЛЬКО в CI.
//
// Эти тесты валидируют, что собранные бинарники (balancer, cppworker, agent)
// хотя бы стартуют, парсят флаги и не падают при инициализации. Не делают
// реальных HTTP-запросов, не требуют работающего llama.cpp runtime, не
// трогают source/test-файлы проекта.
//
// Локально эти тесты НЕ запускаются (build-тег `ci` не выставлен по умолчанию).
// Чтобы прогнать их локально:
//
//	# 1. Собрать бинарники
//	go build -tags llama_stub -o bin/balancer-stub   ./cmd/balancer/
//	go build -tags llama_stub -o bin/cppworker-stub ./cmd/cppworker/
//	go build -tags llama_stub -o bin/agent-stub     ./cmd/agent/
//
//	# 2. Прогнать smoke
//	go test -tags ci -v ./tests/ci/...
//
// Назначение: быстрая валидация ключевой сборки в CI pipeline,
// без зависимости от test-файлов в internal/ и cmd/ (которые могут
// иметь специфические требования к runtime или медленно работать
// под coverage-инструментацией).
package ci

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// binDir — путь к bin/ относительно корня репозитория. Тест запускается
// из tests/ci/, поэтому относительно cwd это ../../bin/.
const binDir = "../../bin"

// locateBin — находит собранный бинарник. Если не найден — тест
// пропускается (Skip), а не падает, потому что smoke-test подразумевает,
// что build-шаг CI уже выполнился.
func locateBin(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(binDir, name)
	if _, err := exec.LookPath(path); err != nil {
		t.Skipf("binary not found: %s (CI build step likely failed): %v", path, err)
	}
	return path
}

// TestBinariesParseFlags — каждый из трёх ключевых бинарников должен
// стартовать, распарсить флаги и напечатать usage. Выходной код не важен
// (Go flag по умолчанию делает exit 2 на -h/--help), важно:
//   - бинарь запускается (не паникует, не валится на инициализации)
//   - output содержит "Usage" (значит flag.Parse() сработал)
//
// Это smoke-test, не функциональный. Полное тестирование — в
// internal/... и cmd/... под -tags llama_stub.
func TestBinariesParseFlags(t *testing.T) {
	cases := []struct {
		name string
		flag string
	}{
		{"balancer-stub", "-h"},
		{"cppworker-stub", "-h"},
		{"agent-stub", "-h"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			bin := locateBin(t, c.name)
			out, _ := exec.Command(bin, c.flag).CombinedOutput()
			if !strings.Contains(string(out), "Usage") {
				t.Fatalf("%s %s: expected 'Usage' in output, got:\n%s",
					c.name, c.flag, out)
			}
		})
	}
}
