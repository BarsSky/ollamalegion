package api

// auth_bootstrap_r270_test.go — R270 (2026-10-29): происхождение входа должно быть
// проверяемым, а не угадываемым.
//
// ЖИВАЯ ЖАЛОБА (оффлайн-стенд): оператор задал LB_ADMIN_USER/LB_ADMIN_PASSWORD в
// env, но войти не смог — и причину узнать было негде. На изолированной машине
// UI-мастера нет, сети нет, единственный вход — env, поэтому «молча не пускает»
// там означает полную потерю доступа.
//
// Тесты фиксируют три состояния, которые раньше были неотличимы:
//  1. пустое хранилище + env → админ создан из env и записан на диск;
//  2. файл с другим паролем + env → env-пароль применён, прочие пользователи целы;
//  3. файл недоступен на запись → админ только в памяти, и это видно (persisted=false).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestR270_EnvBootstrapCreatesAdminOnEmptyStore — env создаёт админа и пишет файл.
func TestR270_EnvBootstrapCreatesAdminOnEmptyStore(t *testing.T) {
	t.Setenv("LB_ADMIN_USER", "skyadmin")
	t.Setenv("LB_ADMIN_PASSWORD", "Ol-legion-34172hsb@qw")

	path := filepath.Join(t.TempDir(), "users.json")
	us := NewUserStore(path)

	if !us.Enabled() {
		t.Fatal("админ из env не создан — вход невозможен")
	}
	if _, ok := us.Verify("skyadmin", "Ol-legion-34172hsb@qw"); !ok {
		t.Fatal("пароль из env не подошёл к созданному админу")
	}
	info := us.BootstrapInfo()
	if info.Source != "env" {
		t.Errorf("source = %q, want env", info.Source)
	}
	if !info.EnvApplied {
		t.Error("envApplied = false, хотя админ создан из env")
	}
	if !info.Persisted {
		t.Errorf("persisted = false (reason=%q), хотя каталог доступен на запись", info.Reason)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("файл пользователей не записан: %v", err)
	}
}

// TestR270_EnvPasswordWinsOverExistingFile — явный env-пароль применяется, а
// существующие пользователи не теряются.
func TestR270_EnvPasswordWinsOverExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.json")
	old := NewUserStore(path)
	if err := old.Upsert("skyadmin", "old-password", "admin"); err != nil {
		t.Fatalf("подготовка файла: %v", err)
	}
	if err := old.Upsert("viewer", "viewer-pass", "viewer"); err != nil {
		t.Fatalf("подготовка второго пользователя: %v", err)
	}

	t.Setenv("LB_ADMIN_USER", "skyadmin")
	t.Setenv("LB_ADMIN_PASSWORD", "new-password")

	us := NewUserStore(path)
	if _, ok := us.Verify("skyadmin", "new-password"); !ok {
		t.Fatal("новый пароль из env не применён — оператор не войдёт")
	}
	if _, ok := us.Verify("skyadmin", "old-password"); ok {
		t.Error("старый пароль всё ещё принимается")
	}
	if _, ok := us.Verify("viewer", "viewer-pass"); !ok {
		t.Error("потерян другой пользователь из файла")
	}
	info := us.BootstrapInfo()
	if info.Source != "env+file" {
		t.Errorf("source = %q, want env+file", info.Source)
	}
	if !info.EnvApplied {
		t.Error("envApplied = false, хотя пароль из env применён")
	}
}

// TestR270_ReadOnlyStoreReportsMemoryOnly — файл недоступен на запись: админ
// действует в памяти, а BootstrapInfo честно говорит, что не переживёт рестарт.
func TestR270_ReadOnlyStoreReportsMemoryOnly(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("подготовка: %v", err)
	}
	path := filepath.Join(blocker, "users.json")

	t.Setenv("LB_ADMIN_USER", "skyadmin")
	t.Setenv("LB_ADMIN_PASSWORD", "pass")

	us := NewUserStore(path)
	if !us.Enabled() {
		t.Fatal("админ не действует даже в памяти")
	}
	info := us.BootstrapInfo()
	if info.Writable {
		t.Error("writable = true для недоступного пути")
	}
	if info.Persisted {
		t.Error("persisted = true, хотя записать файл невозможно")
	}
	if info.Reason == "" {
		t.Error("причина не записана — оператор снова будет гадать")
	}
}

// TestR270_StatusExposesBootstrap — /auth/me отдаёт происхождение и доступность,
// но НИКОГДА не отдаёт пароль/хэш.
func TestR270_StatusExposesBootstrap(t *testing.T) {
	t.Setenv("LB_ADMIN_USER", "skyadmin")
	t.Setenv("LB_ADMIN_PASSWORD", "secret-in-env")

	us := NewUserStore(filepath.Join(t.TempDir(), "users.json"))
	sessions := NewSessionStore()

	rec := httptest.NewRecorder()
	AuthMeHandler(us, sessions, NewTokenAuthenticator([]string{"tok"}, "X-API-Token", true))(
		rec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("ответ не JSON: %v", err)
	}
	raw, ok := body["bootstrap"]
	if !ok {
		t.Fatal("в /auth/me нет bootstrap — причину входа снова не узнать")
	}
	bs, ok := raw.(map[string]interface{})
	if !ok {
		t.Fatalf("bootstrap странного типа: %T", raw)
	}
	if bs["source"] != "env" {
		t.Errorf("bootstrap.source = %v, want env", bs["source"])
	}
	for _, k := range []string{"path", "writable", "persisted", "users"} {
		if _, ok := bs[k]; !ok {
			t.Errorf("в bootstrap нет поля %q", k)
		}
	}
	text := rec.Body.String()
	if strings.Contains(text, "secret-in-env") || strings.Contains(text, "password_hash") {
		t.Errorf("в ответе /auth/me обнаружен секрет: %s", text)
	}
}
