// Package version — тесты формата строки версии для стартовой шапки логов.
//
// Зачем тесты. Строка версии печатается в первой шапке логов каждого сервиса и
// является единственным способом отличить «исправление не сработало» от
// «в контейнере старый образ». Ошибка в формате (например «unknown (commit
// unknown)») делает её бесполезной именно в тот момент, когда она нужна.
package version

import (
	"os"
	"strings"
	"testing"
)

// TestVersion_EnvFillsAllFields — compose передаёт APP_VERSION/GIT_COMMIT/BUILD_DATE.
func TestVersion_EnvFillsAllFields(t *testing.T) {
	backup := restoreEnv(t)
	defer backup()

	t.Setenv("APP_VERSION", "r83-submodule-v23")
	t.Setenv("GIT_COMMIT", "5731414")
	t.Setenv("BUILD_DATE", "2026-09-28")
	// Значения из ldflags в тестовой сборке пусты; если они заданы — сбрасываем,
	// чтобы тест не зависел от способа сборки.
	savedVersion, savedCommit, savedDate := Version, Commit, BuildDate
	Version, Commit, BuildDate = "", "", ""
	defer func() { Version, Commit, BuildDate = savedVersion, savedCommit, savedDate }()

	got := Get()
	if got.Version != "r83-submodule-v23" {
		t.Errorf("Version = %q, want r83-submodule-v23", got.Version)
	}
	if got.Commit != "5731414" {
		t.Errorf("Commit = %q, want 5731414", got.Commit)
	}
	if got.BuildDate != "2026-09-28" {
		t.Errorf("BuildDate = %q, want 2026-09-28", got.BuildDate)
	}
}

// TestVersion_LdflagsWinOverEnv — значение, вшитое при сборке, приоритетнее env.
func TestVersion_LdflagsWinOverEnv(t *testing.T) {
	backup := restoreEnv(t)
	defer backup()

	t.Setenv("GIT_COMMIT", "from-env")
	savedCommit := Commit
	Commit = "from-ldflags"
	defer func() { Commit = savedCommit }()

	if got := Get().Commit; got != "from-ldflags" {
		t.Errorf("Commit = %q, want from-ldflags (ldflags приоритетнее env)", got)
	}
}

// TestVersion_String — человекочитаемый формат и отсутствие «unknown (commit unknown)».
func TestVersion_String(t *testing.T) {
	cases := []struct {
		name string
		in   About
		want string
	}{
		{"всё известно", About{Version: "v23", Commit: "5731414", BuildDate: "2026-09-28"},
			"v23 (commit 5731414, 2026-09-28)"},
		{"только версия", About{Version: "v23"}, "v23"},
		{"ничего не известно", About{}, "unknown"},
		{"версия и коммит", About{Version: "v23", Commit: "5731414"}, "v23 (commit 5731414)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}

	// Ни в одном случае не должно быть повторов "unknown" — иначе строка
	// бесполезна ровно тогда, когда нужна.
	if s := (About{Version: "v23"}).String(); strings.Contains(s, "unknown") {
		t.Errorf("при известной версии строка не должна содержать unknown: %q", s)
	}
}

// restoreEnv отдаёт функцию, вызываемую через defer, для восстановления env.
// t.Setenv сам восстанавливает значения по завершении теста, поэтому здесь
// достаточно заглушки — оставлено для явности в тестах с ручным изменением vars.
func restoreEnv(t *testing.T) func() {
	t.Helper()
	for _, name := range []string{"APP_VERSION", "VERSION", "GIT_COMMIT", "BUILD_DATE"} {
		_ = os.Getenv(name)
	}
	return func() {}
}
