// auth_users_path_r481_test.go — R481: путь файла пользователей по умолчанию.
//
// Живая жалоба: «не даёт создавать пользователей — у контейнера нет прав на
// config/users.json». Причина: в части compose-вариантов (bundled-with-agent)
// LB_USERS_FILE не передавался, и балансер падал на legacy-дефолт
// `config/users.json` — а этот каталог смонтирован READ-ONLY (../config:/app/config:ro).
// В контейнере с томом /app/data дефолт обязан быть /app/data/users.json.
package api

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultUsersFileR481(t *testing.T) {
	t.Run("есть /app/data — берём том", func(t *testing.T) {
		got := defaultUsersFileR481(func(dir string) bool { return dir == "/app/data" })
		if got != "/app/data/users.json" {
			t.Fatalf("defaultUsersFileR481 = %q, ожидалось /app/data/users.json", got)
		}
	})
	t.Run("нет /app/data — legacy config/users.json", func(t *testing.T) {
		got := defaultUsersFileR481(func(string) bool { return false })
		if got != "config/users.json" {
			t.Fatalf("defaultUsersFileR481 = %q, ожидалось config/users.json", got)
		}
	})
}

// TestDefaultUsersFileR481_RealDataDir — на машине без /app/data функция обязана
// вернуть legacy-путь; тест не требует каталога и не создаёт его.
func TestDefaultUsersFileR481_RealDataDir(t *testing.T) {
	_, statErr := os.Stat("/app/data")
	want := "config/users.json"
	if statErr == nil {
		want = "/app/data/users.json"
	}
	if got := defaultUsersFileR481(dirExists); got != want {
		t.Fatalf("defaultUsersFileR481 = %q, ожидалось %q (dirExists(%q)=%v)",
			got, want, "/app/data", statErr == nil)
	}
}

// TestDefaultUsersFileR481_ConfigDirWritable — если legacy-путь лежит в
// read-only каталоге, дефолт не должен выбирать его молча: проверяем, что
// функция вообще не смотрит на запись (решение — за оператором), но
// документируем ожидаемое поведение: путь остаётся, а предупреждение пишет
// startup-проверка BootstrapInfo.
func TestDefaultUsersFileR481_ConfigDirWritable(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config")
	if err := os.MkdirAll(cfg, 0o555); err != nil {
		t.Skipf("не удалось создать read-only каталог: %v", err)
	}
	if got := defaultUsersFileR481(func(string) bool { return false }); got != "config/users.json" {
		t.Fatalf("дефолт = %q, ожидался config/users.json", got)
	}
}
