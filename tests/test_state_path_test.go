// test_state_path_test.go — R70 (2026-09-24): тесты не должны пачкать рабочее
// дерево.
//
// ПРОБЛЕМА: интеграционные тесты пакета ./tests передавали в
// LoadBalancerSettings.StatePath путь "testdata/state.json" — ФАЙЛ, КОТОРЫЙ
// ТРЕКАЕТСЯ В GIT. Proxy пишет туда своё состояние (debounced autosave), поэтому
// `git status` после каждого прогона показывал изменённый
// tests/testdata/state.json (в диффе — только поле "updated"). Из-за этого файл
// приходилось вручную откатывать перед коммитами, а случайный `git commit -a`
// мог утащить timestamp в историю.
//
// РЕШЕНИЕ: testStatePath(t) копирует фикстуру во временный каталог теста
// (t.TempDir()) и возвращает путь к копии — рабочее дерево остаётся чистым,
// а фикстура по-прежнему читается (тесты, которые её парсят, используют тот же
// временный файл).
package tests

import (
	"os"
	"path/filepath"
	"testing"
)

// testStatePath — путь к копии фикстуры testdata/state.json во временном
// каталоге теста (см. комментарий к файлу).
func testStatePath(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("testdata/state.json")
	if err != nil {
		t.Fatalf("не удалось прочитать фикстуру testdata/state.json: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(dst, src, 0o600); err != nil {
		t.Fatalf("не удалось создать временный state.json: %v", err)
	}
	return dst
}
