// identity_r486_test.go — R486 (2026-11-10): запечённая идентичность образа.
//
// Живой дефект: образ был собран и проштампован ПРЕДЫДУЩИМ коммитом (в
// deployments/.env лежал старый BALANCER_GIT_COMMIT), а pkg/version молча
// принимал «unknown». Оператор узнал об этом случайно, по /api/version. Тесты
// держат три правила: запечённое важнее env, «unknown» не считается значением,
// несовпадение бинаря и /app/identity.json — ошибка запуска.
package version

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeIdentityR486 кладёт временный identity.json и сбрасывает кэш.
func writeIdentityR486(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "identity.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("не записал identity.json: %v", err)
	}
	t.Setenv("IDENTITY_FILE", path)
	resetIdentityCacheR486()
	t.Cleanup(resetIdentityCacheR486)
}

// stubLdflagsR486 подменяет значения, которые обычно приходят из -ldflags.
func stubLdflagsR486(t *testing.T, service, tag, commit, buildDate string) {
	t.Helper()
	savedService, savedTag, savedCommit, savedDate := Service, ImageTag, Commit, BuildDate
	Service, ImageTag, Commit, BuildDate = service, tag, commit, buildDate
	t.Cleanup(func() { Service, ImageTag, Commit, BuildDate = savedService, savedTag, savedCommit, savedDate })
}

const identitySampleR486 = `{
  "service": "balancer",
  "version": "0.7.246",
  "image_tag": "r261110-1900",
  "commit": "abc1234",
  "commit_full": "abc1234def5678abc1234def5678abc1234def56",
  "commit_date": "2026-11-10T14:20:00Z",
  "build_date": "2026-11-10T14:40:00Z",
  "author": "Иван <ivan@example.com>",
  "llama_commit": "d5c9f2a",
  "llama_date": "2026-09-01",
  "rpc_proto": "6.0.0"
}`

// TestR486_BakedWinsOverEnv — образ сообщает СВОЮ идентичность, а не то, что
// ему подложили в env стенда.
func TestR486_BakedWinsOverEnv(t *testing.T) {
	writeIdentityR486(t, identitySampleR486)
	stubLdflagsR486(t, "", "", "", "")
	t.Setenv("APP_VERSION", "from-env")
	t.Setenv("GIT_COMMIT", "0000000")
	t.Setenv("BUILD_DATE", "1999-01-01")

	got := Get()
	if got.Commit != "abc1234" {
		t.Fatalf("Commit = %q, ожидался запечённый abc1234 (env перекрывать не должен)", got.Commit)
	}
	if got.Version != "0.7.246" {
		t.Fatalf("Version = %q", got.Version)
	}
	if got.BuildDate != "2026-11-10T14:40:00Z" {
		t.Fatalf("BuildDate = %q", got.BuildDate)
	}
	if got.LlamaCommit != "d5c9f2a" || got.RPCProto != "6.0.0" {
		t.Fatalf("llama/rpc не запечены: llama=%q rpc=%q", got.LlamaCommit, got.RPCProto)
	}
}

// TestR486_FieldsCarryNewKeys — новые поля публикуются, старые ключи не переименованы.
func TestR486_FieldsCarryNewKeys(t *testing.T) {
	writeIdentityR486(t, identitySampleR486)
	stubLdflagsR486(t, "", "", "", "")
	f := Get().Fields()
	for _, k := range []string{"build_commit", "build_date", "build_version", "build"} {
		if _, ok := f[k]; !ok {
			t.Errorf("потерян прежний ключ %q — контракт /api/version", k)
		}
	}
	for _, k := range []string{"build_commit_full", "build_commit_date", "build_author", "build_image_tag", "build_service", "llama_commit", "llama_date", "rpc_proto"} {
		if _, ok := f[k]; !ok {
			t.Errorf("нет нового ключа %q", k)
		}
	}
}

// TestR486_UnknownIsNotAValue — «unknown» из env не выдаётся за факт сборки.
func TestR486_UnknownIsNotAValue(t *testing.T) {
	writeIdentityR486(t, `{"service":"balancer"}`)
	stubLdflagsR486(t, "", "", "", "")
	t.Setenv("GIT_COMMIT", "unknown")
	t.Setenv("APP_VERSION", "unknown")
	if got := Get(); got.Commit != "" {
		t.Fatalf("Commit = %q, «unknown» не должен считаться значением", got.Commit)
	}
	if _, ok := Get().Fields()["build_commit"]; ok {
		t.Fatal("build_commit не должен публиковаться при unknown")
	}
}

// TestR486_VerifyNoImageIsOK — вне образа (go run) файла нет, ошибки нет.
func TestR486_VerifyNoImageIsOK(t *testing.T) {
	t.Setenv("IDENTITY_FILE", filepath.Join(t.TempDir(), "absent.json"))
	resetIdentityCacheR486()
	t.Cleanup(resetIdentityCacheR486)
	if err := VerifyR486("balancer"); err != nil {
		t.Fatalf("локальная сборка без identity.json не должна падать: %v", err)
	}
}

// TestR486_VerifyMatch — бинарь и образ совпадают по коммиту.
func TestR486_VerifyMatch(t *testing.T) {
	writeIdentityR486(t, identitySampleR486)
	stubLdflagsR486(t, "balancer", "r261110-1900", "abc1234", "2026-11-10T14:40:00Z")
	if err := VerifyR486("balancer"); err != nil {
		t.Fatalf("совпадающая идентичность не должна падать: %v", err)
	}
}

// TestR486_VerifyStaleStamp — главный случай: в образе чужой/устаревший штамп.
func TestR486_VerifyStaleStamp(t *testing.T) {
	writeIdentityR486(t, identitySampleR486)
	stubLdflagsR486(t, "balancer", "", "850923d7", "2026-11-10T14:40:00Z")
	err := VerifyR486("balancer")
	if err == nil {
		t.Fatal("расхождение коммита бинаря и образа обязано быть ошибкой")
	}
	if !strings.Contains(err.Error(), "850923d7") || !strings.Contains(err.Error(), "abc1234") {
		t.Fatalf("в ошибке нет обоих коммитов: %v", err)
	}
}

// TestR486_VerifyUnstampedBinary — образ есть, а бинарь собран без -ldflags:
// идентичность недоказуема, это ошибка, а не предупреждение.
func TestR486_VerifyUnstampedBinary(t *testing.T) {
	writeIdentityR486(t, identitySampleR486)
	stubLdflagsR486(t, "", "", "", "")
	err := VerifyR486("balancer")
	if err == nil || !strings.Contains(err.Error(), "без штампа") {
		t.Fatalf("ожидалась ошибка «собран без штампа», получено: %v", err)
	}
}

// TestR486_VerifyWrongService — бинарь воркера в образе балансера.
func TestR486_VerifyWrongService(t *testing.T) {
	writeIdentityR486(t, identitySampleR486)
	stubLdflagsR486(t, "cppworker", "", "abc1234", "2026-11-10T14:40:00Z")
	err := VerifyR486("cppworker")
	if err == nil || !strings.Contains(err.Error(), "сервис") {
		t.Fatalf("ожидалась ошибка про сервис, получено: %v", err)
	}
}

// TestR486_BakedIdentityMap — диагностика получает файл целиком, а не пересказ env.
func TestR486_BakedIdentityMap(t *testing.T) {
	writeIdentityR486(t, identitySampleR486)
	m := BakedIdentityR486()
	if m["commit"] != "abc1234" || m["rpc_proto"] != "6.0.0" || m["path"] == "" {
		t.Fatalf("неполная карта идентичности: %v", m)
	}
}
