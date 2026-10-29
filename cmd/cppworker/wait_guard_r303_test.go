//go:build llama_stub

// wait_guard_r303_test.go — R303 steward: в cmd/cppworker не должно остаться
// НЕОГРАНИЧЕННЫХ ожиданий чужой загрузки.
//
// ПОЧЕМУ СТРАЖ, А НЕ ПРОСТО ФИКС. Живой дефект D3 (двухузловой отчёт) —
// `POST /api/models/load` висел 260 c без ответа и без строки в логе, потому что
// `backend.WaitForLoad(name)` ждёт канал заявки БЕЗ ТАЙМАУТА. Заявка осталась от
// умершей горутины, и узел выходил из состояния только перезапуском контейнера.
// Такой вызов легко вернуть обратно (он выглядит естественно), поэтому запрет
// проверяется автоматически: любое `WaitForLoad(` в не-тестовом коде
// cmd/cppworker обязано быть `WaitForLoadTimeoutR303(`.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestR303_NoUnboundedWaitForLoad(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	bad := make([]string, 0, 4)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(".", name))
		if readErr != nil {
			t.Fatalf("ReadFile %s: %v", name, readErr)
		}
		for i, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if !strings.Contains(line, "WaitForLoad(") {
				continue
			}
			bad = append(bad, name+":"+itoaR303(i+1)+": "+trimmed)
		}
	}
	if len(bad) > 0 {
		t.Fatalf("неограниченное ожидание загрузки (R303): используйте "+
			"backend.WaitForLoadTimeoutR303(name, loadWaitMaxR303()):\n%s",
			strings.Join(bad, "\n"))
	}
}

// itoaR303 — маленький помощник, чтобы не тянуть strconv ради одного места.
func itoaR303(n int) string {
	if n == 0 {
		return "0"
	}
	var digits [20]byte
	i := len(digits)
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	return string(digits[i:])
}
