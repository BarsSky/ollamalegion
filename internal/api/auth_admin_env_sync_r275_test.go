package api

// auth_admin_env_sync_r275_test.go — R275 (2026-10-29): env-учётка должна
// ПРИЗНАВАТЬСЯ применённой и тогда, когда она совпала с файлом.
//
// ЖИВОЙ ФАКТ со стенда r273 (факты, не догадка): в контейнере
// `LB_ADMIN_USER=skyadmin` и пароль присутствуют (`docker exec … env`), вход
// РАБОТАЕТ, а `/api/v1/auth/me` отдаёт
// `bootstrap{source:"file", envApplied:false, persisted:true, writable:true, users:1}`.
//
// Причина: ветка «пароль из env совпал с хешем в файле» выходила из
// syncAdminFromEnv ДО того, как выставить EnvApplied. То есть самый благополучный
// случай (пароль из .env действительно действует, перезаписывать нечего)
// репортbровался как «env не применён», и оператор на изолированной машине шёл
// искать причину, которой нет.
//
// Тесты фиксируют правило:
//   - env задан и совпал с файлом → envApplied=true, source=enemv+file, файл не переписывается;
//   - env задан и отличается → пароль обновляется (прежнее поведение R203);
//   - env задан, в файле ДРУГОЙ админ → добавляется администратор, прежние целы;
//   - LB_ADMIN_ENV_SYNC=0 при непустом файле → env не применяется, но причина названа;
//   - LB_ADMIN_ENV_SYNC=0 при ПУСТОМ файле → админ всё равно создаётся (иначе войти некому).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// prepStoreWithUser — подготовить файл пользователей напрямую, не через env.
func prepStoreWithUser(t *testing.T, path, name, pass, role string) {
	t.Helper()
	// Пустое окружение на время подготовки: иначе NewUserStore применит env-учётку
	// и тест будет проверять не то, что заявлено.
	t.Setenv("LB_ADMIN_USER", "")
	t.Setenv("LB_ADMIN_PASSWORD", "")
	store := NewUserStore(path)
	if err := store.Upsert(name, pass, role); err != nil {
		t.Fatalf("подготовка пользователя %s: %v", name, err)
	}
}

// TestR275_EnvMatchesFileIsRecognised — главный живой случай: env действует,
// и это должно быть видно как envApplied=true, а не как «файл».
func TestR275_EnvMatchesFileIsRecognised(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.json")
	prepStoreWithUser(t, path, "skyadmin", "Ol-legion-34172hsb@qw", "admin")

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("чтение файла до старта: %v", err)
	}

	t.Setenv("LB_ADMIN_USER", "skyadmin")
	t.Setenv("LB_ADMIN_PASSWORD", "Ol-legion-34172hsb@qw")

	us := NewUserStore(path)
	if _, ok := us.Verify("skyadmin", "Ol-legion-34172hsb@qw"); !ok {
		t.Fatal("вход паролем из env не работает")
	}
	info := us.BootstrapInfo()
	if !info.EnvApplied {
		t.Fatal("envApplied=false, хотя env-пароль и есть действующий пароль — " +
			"это ровно тот ответ, из-за которого оператор ищет несуществующую причину")
	}
	if info.Source != "env+file" {
		t.Errorf("source = %q, want env+file (учётка из env подтверждена файлом)", info.Source)
	}
	if !info.Persisted {
		t.Errorf("persisted = false (reason=%q), хотя запись лежит в файле", info.Reason)
	}
	if info.Users != 1 {
		t.Errorf("users = %d, want 1 (совпадение не должно добавлять записи)", info.Users)
	}
	if info.Reason == "" {
		t.Error("причина не записана: оператор не узнает, что env-учётка действует")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("чтение файла после старта: %v", err)
	}
	if string(before) != string(after) {
		// Совпадение пароля не повод переписывать файл: лишняя запись в том на
		// оффлайн-стенде — это лишний риск (и лишний mtime в аудите).
		t.Error("файл пользователей переписан, хотя менять было нечего")
	}
}

// TestR275_EnvDiffersUpdatesPassword — прежнее поведение R203 не сломано.
func TestR275_EnvDiffersUpdatesPassword(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.json")
	prepStoreWithUser(t, path, "skyadmin", "old-password", "admin")

	t.Setenv("LB_ADMIN_USER", "skyadmin")
	t.Setenv("LB_ADMIN_PASSWORD", "new-password")

	us := NewUserStore(path)
	if _, ok := us.Verify("skyadmin", "new-password"); !ok {
		t.Fatal("новый пароль из env не применён")
	}
	if _, ok := us.Verify("skyadmin", "old-password"); ok {
		t.Error("старый пароль всё ещё принимается")
	}
	info := us.BootstrapInfo()
	if !info.EnvApplied || info.Source != "env+file" {
		t.Errorf("bootstrap = %+v, want envApplied=true, source=env+file", info)
	}
}

// TestR275_EnvDifferentAdminAddsAdmin — оператор сменил имя в .env: добавляется
// администратор, прежние пользователи не теряются.
func TestR275_EnvDifferentAdminAddsAdmin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.json")
	prepStoreWithUser(t, path, "oldadmin", "old-pass", "admin")

	t.Setenv("LB_ADMIN_USER", "newadmin")
	t.Setenv("LB_ADMIN_PASSWORD", "new-pass")

	us := NewUserStore(path)
	if _, ok := us.Verify("newadmin", "new-pass"); !ok {
		t.Fatal("админ из env не создан")
	}
	if _, ok := us.Verify("oldadmin", "old-pass"); !ok {
		t.Error("потерян прежний администратор из файла")
	}
	info := us.BootstrapInfo()
	if !info.EnvApplied {
		t.Error("envApplied=false, хотя админ из env создан")
	}
	if info.Users != 2 {
		t.Errorf("users = %d, want 2", info.Users)
	}
}

// TestR275_SyncDisabledKeepsFile — явный отказ от синка: записи не трогаем, но
// причину называем.
func TestR275_SyncDisabledKeepsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.json")
	prepStoreWithUser(t, path, "skyadmin", "file-password", "admin")

	t.Setenv("LB_ADMIN_USER", "skyadmin")
	t.Setenv("LB_ADMIN_PASSWORD", "env-password")
	t.Setenv("LB_ADMIN_ENV_SYNC", "0")

	us := NewUserStore(path)
	if _, ok := us.Verify("skyadmin", "file-password"); !ok {
		t.Error("пароль из файла перестал работать при LB_ADMIN_ENV_SYNC=0")
	}
	if _, ok := us.Verify("skyadmin", "env-password"); ok {
		t.Error("env-пароль применён, хотя синк выключен оператором")
	}
	info := us.BootstrapInfo()
	if info.EnvApplied {
		t.Error("envApplied=true при выключенном синке")
	}
	if !strings.Contains(strings.ToUpper(info.Reason), "LB_ADMIN_ENV_SYNC") {
		t.Errorf("причина не называет переключатель: %q", info.Reason)
	}
}

// TestR275_SyncDisabledStillBootstrapsEmptyStore — исключение: без файла админ
// всё равно создаётся, иначе на изолированной машине войти некому.
func TestR275_SyncDisabledStillBootstrapsEmptyStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.json")
	t.Setenv("LB_ADMIN_USER", "skyadmin")
	t.Setenv("LB_ADMIN_PASSWORD", "env-password")
	t.Setenv("LB_ADMIN_ENV_SYNC", "0")

	us := NewUserStore(path)
	if !us.Enabled() {
		t.Fatal("при пустом файле админ обязан создаваться даже с LB_ADMIN_ENV_SYNC=0")
	}
	if _, ok := us.Verify("skyadmin", "env-password"); !ok {
		t.Fatal("пароль из env не применён к созданному админу")
	}
	info := us.BootstrapInfo()
	if info.Source != "env" || !info.EnvApplied {
		t.Errorf("bootstrap = %+v, want source=env, envApplied=true", info)
	}
}
