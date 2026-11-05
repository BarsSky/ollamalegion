// version_fields_r448_test.go — R448 (2026-11-05): сведения о сборке должны
// читаться по HTTP, а не только через `docker inspect`.
//
// ЖИВОЙ ФАКТ. После раскатки стенда образ r261105-2215 (коммит 556b7e63)
// проверить, что в контейнере именно этот код, можно было ТОЛЬКО тегом/Id образа:
// `/api/version` отдавал жёстко зашитую строку, `/api/info` — версию моста.
// Коммит печётся в бинарник с R83, но наружу не выводился — то есть у оператора
// с WebUI или удалённым доступом (без docker) снова не было способа отличить
// «правка не сработала» от «в контейнере старый образ».
package version

import "testing"

// TestR448_FieldsExposeBuild — заполненные сведения печатаются отдельными полями.
func TestR448_FieldsExposeBuild(t *testing.T) {
	about := About{Version: "r261105-2215", Commit: "556b7e63", BuildDate: "2026-11-05T19:15:00Z"}
	fields := about.Fields()

	if got := fields["build_commit"]; got != "556b7e63" {
		t.Fatalf("build_commit = %v, want 556b7e63", got)
	}
	if got := fields["build_date"]; got != "2026-11-05T19:15:00Z" {
		t.Fatalf("build_date = %v", got)
	}
	if got := fields["build_version"]; got != "r261105-2215" {
		t.Fatalf("build_version = %v", got)
	}
	// build — та же строка, что в шапке логов, и в ней обязан быть коммит.
	build, _ := fields["build"].(string)
	if build == "" || !contains(build, "556b7e63") {
		t.Fatalf("build = %q, want строку с коммитом", build)
	}
}

// TestR448_FieldsSkipUnknown — неизвестное не публикуется.
//
// Поле со значением "unknown" хуже отсутствующего: клиент покажет его как факт.
func TestR448_FieldsSkipUnknown(t *testing.T) {
	fields := About{}.Fields()
	if len(fields) != 0 {
		t.Fatalf("пустые сведения не должны давать полей: %v", fields)
	}

	onlyVersion := About{Version: "r1"}.Fields()
	if _, ok := onlyVersion["build_commit"]; ok {
		t.Fatalf("нет коммита — поля build_commit быть не должно: %v", onlyVersion)
	}
	if onlyVersion["build_version"] != "r1" {
		t.Fatalf("build_version = %v", onlyVersion["build_version"])
	}
	if onlyVersion["build"] != "r1" {
		t.Fatalf("build = %v, want r1 (без выдуманных частей)", onlyVersion["build"])
	}
}

// TestR448_EnvFallback — сведения берутся из окружения, когда ldflags их не задал.
//
// Именно так compose передаёт GIT_COMMIT/BUILD_DATE в контейнер.
func TestR448_EnvFallback(t *testing.T) {
	t.Setenv("GIT_COMMIT", "deadbeef")
	t.Setenv("BUILD_DATE", "2026-11-05T00:00:00Z")

	fields := Get().Fields()
	if fields["build_commit"] != "deadbeef" {
		t.Fatalf("build_commit = %v, want deadbeef (из GIT_COMMIT)", fields["build_commit"])
	}
	if fields["build_date"] != "2026-11-05T00:00:00Z" {
		t.Fatalf("build_date = %v", fields["build_date"])
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
