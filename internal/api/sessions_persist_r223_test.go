//go:build llama_stub

// sessions_persist_r223_test.go — R223 (2026-10-22).
//
// ЖИВОЙ ДЕФЕКТ. R222 убрал вшитый токен: браузер работает только сессией. Но сессии
// лежали в памяти процесса, и рестарт балансера (обновление образа, `compose
// restart`) выбрасывал оператора на экран входа. Здесь проверяется, что вход
// переживает рестарт, но не превращает файл в вечный пропуск:
//  1. сессия восстанавливается из файла вместе с ролью и CSRF;
//  2. файл с секретами создаётся с правами 0600;
//  3. просроченные сессии при загрузке отбрасываются;
//  4. logout и смена пароля убирают сессии из файла;
//  5. путь по умолчанию — рядом с users.json (тот же том), без обязательных env.
package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func r223Store(t *testing.T) (*SessionStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sessions.json")
	return NewSessionStoreFromFile(path), path
}

func TestR223_SessionSurvivesRestart(t *testing.T) {
	store, path := r223Store(t)
	sess := store.Create("test-admin", "admin")
	if sess == nil {
		t.Fatal("сессия не создана")
	}

	restarted := NewSessionStoreFromFile(path)
	got, ok := restarted.Get(sess.ID)
	if !ok {
		t.Fatal("после рестарта сессия не найдена — оператор снова увидит экран входа")
	}
	if got.User != "test-admin" || got.Role != "admin" {
		t.Errorf("после рестарта неверные данные сессии: user=%q role=%q", got.User, got.Role)
	}
	if got.CSRF != sess.CSRF {
		t.Error("CSRF не пережил рестарт: браузер получит 403 на мутации после обновления образа")
	}
}

func TestR223_SessionFileIsPrivate(t *testing.T) {
	store, path := r223Store(t)
	sess := store.Create("admin", "admin")

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("файл сессий не создан: %v", err)
	}
	// На Windows chmod не отображается в правах (работает только read-only бит), а
	// прод обычно Linux. Проверяем там, где это что-то значит.
	if runtime.GOOS != "windows" {
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("права файла сессий %o, ожидались 600 (в файле cookie-id и CSRF)", mode)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("файл не читается: %v", err)
	}
	var file sessionFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("файл сессий не разобран: %v", err)
	}
	if len(file.Sessions) != 1 || file.Sessions[0].ID != sess.ID {
		t.Errorf("в файле не та сессия: %+v", file.Sessions)
	}
}

func TestR223_ExpiredSessionsDroppedOnLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	stale := &AuthSession{
		ID: "dead", User: "admin", Role: "admin", CSRF: "x",
		Created:  time.Now().Add(-24 * time.Hour),
		LastSeen: time.Now().Add(-13 * time.Hour), // больше TTL
	}
	fresh := &AuthSession{
		ID: "alive", User: "admin", Role: "admin", CSRF: "y",
		Created:  time.Now().Add(-1 * time.Hour),
		LastSeen: time.Now().Add(-2 * time.Minute),
	}
	raw, _ := json.Marshal(&sessionFile{SavedAt: time.Now(), Sessions: []*AuthSession{stale, fresh}})
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("не удалось подготовить файл: %v", err)
	}

	store := NewSessionStoreFromFile(path)
	if _, ok := store.Get("dead"); ok {
		t.Error("просроченная сессия принята после рестарта — файл стал вечным пропуском")
	}
	if _, ok := store.Get("alive"); !ok {
		t.Error("живая сессия потеряна при загрузке")
	}
}

func TestR223_LogoutAndPasswordChangeDropSessionsFromFile(t *testing.T) {
	store, path := r223Store(t)
	a := store.Create("alice", "admin")
	b := store.Create("alice", "admin")
	other := store.Create("bob", "viewer")

	store.Delete(a.ID)
	store.DeleteUser("alice")

	restarted := NewSessionStoreFromFile(path)
	if _, ok := restarted.Get(a.ID); ok {
		t.Error("logout не убрал сессию из файла")
	}
	if _, ok := restarted.Get(b.ID); ok {
		t.Error("смена пароля не погасила остальные сессии пользователя")
	}
	if _, ok := restarted.Get(other.ID); !ok {
		t.Error("погашены сессии другого пользователя")
	}
}

// Путь по умолчанию: рядом с users.json, без отдельных env. Compose задаёт
// LB_USERS_FILE=/app/data/users.json — значит сессии обязаны лежать в том же томе.
func TestR223_DefaultSessionsPathNearUsers(t *testing.T) {
	t.Setenv("LB_SESSIONS_FILE", "")
	users := filepath.Join("/app/data", "users.json")
	got := resolveSessionsPath(users)
	if filepath.Base(got) != "sessions.json" || filepath.Dir(got) != filepath.Dir(users) {
		t.Errorf("путь сессий %q: ожидался sessions.json в каталоге %q", got, filepath.Dir(users))
	}
	t.Setenv("LB_SESSIONS_FILE", "/tmp/custom.json")
	if got := resolveSessionsPath("/app/data/users.json"); got != "/tmp/custom.json" {
		t.Errorf("LB_SESSIONS_FILE не учтён: %q", got)
	}
	t.Setenv("LB_SESSIONS_FILE", "")
	if got := resolveSessionsPath(""); got != "" {
		t.Errorf("без пути users.json сессии должны жить в памяти, получено %q", got)
	}
}

func TestR223_InMemoryStoreKeepsNoFile(t *testing.T) {
	store := NewSessionStore()
	if store.Path() != "" {
		t.Errorf("у хранилища в памяти не должно быть пути: %q", store.Path())
	}
	sess := store.Create("admin", "admin")
	if _, ok := store.Get(sess.ID); !ok {
		t.Error("сессия не работает в памяти")
	}
	// Не должно появиться никаких файлов: старый режим поведения сохранён.
	entries, _ := os.ReadDir(".")
	for _, e := range entries {
		if e.Name() == "sessions.json" {
			t.Fatalf("хранилище в памяти создало файл %s", e.Name())
		}
	}
}
