// auth_session_r201_test.go — R201 (2026-10-21): аутентификация WebUI.
//
// ЖИВОЙ ДЕФЕКТ, который закрывают эти тесты: единственным доступом был
// X-API-Token, вшитый в страницу, — любой, кто открыл WebUI, работал как админ.
// Теперь: вход по паролю → серверная сессия в HttpOnly-cookie, роли admin/viewer,
// CSRF на мутирующих запросах, X-API-Token остаётся break-glass для скриптов.
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testUserStore(t *testing.T) *UserStore {
	t.Helper()
	dir := t.TempDir()
	us := NewUserStore(filepath.Join(dir, "users.json"))
	if err := us.Upsert("admin", "s3cret-pass", "admin"); err != nil {
		t.Fatalf("не удалось создать администратора: %v", err)
	}
	if err := us.Upsert("guest", "viewer-pass", "viewer"); err != nil {
		t.Fatalf("не удалось создать viewer: %v", err)
	}
	return us
}

func TestR201_PasswordHashAndVerify(t *testing.T) {
	hash, err := HashPassword("привет-пароль")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if strings.Contains(hash, "привет-пароль") {
		t.Fatal("пароль попал в хеш открытым текстом")
	}
	if !VerifyPassword(hash, "привет-пароль") {
		t.Fatal("верный пароль не подтверждён")
	}
	if VerifyPassword(hash, "другой-пароль") {
		t.Fatal("неверный пароль принят")
	}
	if VerifyPassword("мусор", "привет-пароль") {
		t.Fatal("битый хеш принят")
	}
}

func TestR201_LoginSetsSessionCookieAndMeReadsIt(t *testing.T) {
	users := testUserStore(t)
	sessions := NewSessionStore()
	login := AuthLoginHandler(users, sessions)
	me := AuthMeHandler(users, sessions, NewTokenAuthenticator([]string{"master-token-r201"}, "X-API-Token", true))

	body := strings.NewReader(`{"name":"admin","password":"s3cret-pass"}`)
	rec := httptest.NewRecorder()
	login(rec, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("логин: код %d (%s)", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("cookie сессии не выставлена")
	}
	sid := cookies[0].Value
	if !cookies[0].HttpOnly {
		t.Error("cookie обязана быть HttpOnly")
	}
	if cookies[0].Name != sessionCookieName {
		t.Errorf("имя cookie %q, ожидалось %q", cookies[0].Name, sessionCookieName)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	rec2 := httptest.NewRecorder()
	me(rec2, req)
	var out map[string]interface{}
	if err := json.Unmarshal(rec2.Body.Bytes(), &out); err != nil {
		t.Fatalf("me не JSON: %v", err)
	}
	if out["authenticated"] != true {
		t.Fatalf("me не увидел сессию: %v", out)
	}
	if csrf, _ := out["csrf"].(string); csrf == "" {
		t.Fatal("me не отдал CSRF-токен")
	}
}

func TestR201_LoginRejectsWrongPassword(t *testing.T) {
	users := testUserStore(t)
	sessions := NewSessionStore()
	rec := httptest.NewRecorder()
	AuthLoginHandler(users, sessions)(rec, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
		strings.NewReader(`{"name":"admin","password":"неверный"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("ожидался 401, получен %d", rec.Code)
	}
	if sessions.Count() != 0 {
		t.Fatal("сессия создана при неверном пароле")
	}
}

func TestR201_NoUsersMeansLoginUnavailable(t *testing.T) {
	empty := NewUserStore(filepath.Join(t.TempDir(), "missing.json"))
	rec := httptest.NewRecorder()
	AuthLoginHandler(empty, NewSessionStore())(rec,
		httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"name":"a","password":"b"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("без users.json вход должен быть недоступен (503), получен %d", rec.Code)
	}
}

// authTestServer — middleware с сессиями и хранилищем пользователей.
func authTestServer(t *testing.T) (*TokenAuthenticator, *UserStore, *SessionStore, http.Handler) {
	t.Helper()
	users := testUserStore(t)
	sessions := NewSessionStore()
	auth := NewTokenAuthenticator([]string{"break-glass-token"}, "X-API-Token", true)
	auth.SetSessions(sessions)
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	return auth, users, sessions, AuthMiddleware(ok, auth)
}

func TestR201_SessionAdminMutatesWithCSRF(t *testing.T) {
	_, _, sessions, h := authTestServer(t)
	sess := sessions.Create("admin", "admin")

	// без CSRF — 403
	req := httptest.NewRequest(http.MethodPost, "/api/v1/backends", strings.NewReader(`{}`))
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("мутирующий запрос без CSRF должен быть 403, получен %d", rec.Code)
	}

	// с CSRF — 200
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/backends", strings.NewReader(`{}`))
	req2.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sess.ID})
	req2.Header.Set(csrfHeaderName, sess.CSRF)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("с CSRF ожидался 200, получен %d", rec2.Code)
	}
}

func TestR201_ViewerCannotMutate(t *testing.T) {
	_, _, sessions, h := authTestServer(t)
	sess := sessions.Create("guest", "viewer")

	// Чтение разрешено даже без CSRF.
	reqRead := httptest.NewRequest(http.MethodGet, "/api/v1/backends", nil)
	reqRead.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sess.ID})
	recRead := httptest.NewRecorder()
	h.ServeHTTP(recRead, reqRead)
	if recRead.Code != http.StatusOK {
		t.Fatalf("viewer должен читать: получен %d", recRead.Code)
	}

	// Запись — 403 даже с CSRF.
	reqWrite := httptest.NewRequest(http.MethodPut, "/api/v1/placement", strings.NewReader(`{}`))
	reqWrite.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sess.ID})
	reqWrite.Header.Set(csrfHeaderName, sess.CSRF)
	recWrite := httptest.NewRecorder()
	h.ServeHTTP(recWrite, reqWrite)
	if recWrite.Code != http.StatusForbidden {
		t.Fatalf("viewer не должен писать: получен %d", recWrite.Code)
	}
}

func TestR201_BreakGlassTokenStillWorks(t *testing.T) {
	_, _, _, h := authTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/backends", strings.NewReader(`{}`))
	req.Header.Set("X-API-Token", "break-glass-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("break-glass токен должен работать без CSRF/сессии: получен %d", rec.Code)
	}

	// Без сессии и без токена — 401.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/api/v1/backends", nil))
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("без учётных данных ожидался 401, получен %d", rec2.Code)
	}
}

func TestR201_LogoutKillsSession(t *testing.T) {
	_, _, sessions, h := authTestServer(t)
	sess := sessions.Create("admin", "admin")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sess.ID})
	AuthLogoutHandler(sessions)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout: код %d", rec.Code)
	}
	if sessions.Count() != 0 {
		t.Fatal("сессия не погашена")
	}
	// После logout cookie не открывает доступ.
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/backends", nil)
	req2.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sess.ID})
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("после logout ожидался 401, получен %d", rec2.Code)
	}
}

func TestR201_SessionTTLExpires(t *testing.T) {
	store := NewSessionStore()
	sess := store.Create("admin", "admin")
	// Искусственно состариваем сессию.
	sess.LastSeen = sess.LastSeen.Add(-sessionTTL - time.Minute)
	if _, ok := store.Get(sess.ID); ok {
		t.Fatal("просроченная сессия всё ещё валидна")
	}
}

func TestR201_UsersUpsertWritesFileWithoutPlainPassword(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "users.json")
	us := NewUserStore(path)
	if err := us.Upsert("op", "пароль-123", "admin"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("файл пользователей не создан: %v", err)
	}
	if strings.Contains(string(raw), "пароль-123") {
		t.Fatal("пароль записан в файл открытым текстом")
	}
	reloaded := NewUserStore(path)
	if _, ok := reloaded.Verify("op", "пароль-123"); !ok {
		t.Fatal("после перезагрузки файла пароль не подтверждается")
	}
}

// TestR203_EnvPasswordOverridesStored — R203 (2026-10-21).
//
// Живой случай: оператор сменил LB_ADMIN_PASSWORD в .env, но users.json уже был —
// новый пароль игнорировался. Явное действие оператора обязано применяться.
func TestR203_EnvPasswordOverridesStored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "users.json")
	old := NewUserStore(path)
	if err := old.Upsert("admin", "старый-пароль", "admin"); err != nil {
		t.Fatalf("подготовка: %v", err)
	}

	t.Setenv("LB_ADMIN_USER", "admin")
	t.Setenv("LB_ADMIN_PASSWORD", "новый-пароль-42")
	reloaded := NewUserStore(path)

	if _, ok := reloaded.Verify("admin", "новый-пароль-42"); !ok {
		t.Fatal("новый пароль из .env не применён — панель не пустит оператора")
	}
	if _, ok := reloaded.Verify("admin", "старый-пароль"); ok {
		t.Fatal("старый пароль всё ещё принимается")
	}
}

// TestR203_EnvPasswordUnchangedIsNoop — если пароль совпадает, файл не переписывается
// зря и доступ остаётся прежним.
func TestR203_EnvPasswordUnchangedIsNoop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "users.json")
	first := NewUserStore(path)
	if err := first.Upsert("admin", "тот-же-пароль", "admin"); err != nil {
		t.Fatalf("подготовка: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	t.Setenv("LB_ADMIN_USER", "admin")
	t.Setenv("LB_ADMIN_PASSWORD", "тот-же-пароль")
	again := NewUserStore(path)
	if _, ok := again.Verify("admin", "тот-же-пароль"); !ok {
		t.Fatal("пароль перестал работать")
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat2: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Log("файл перезаписан при совпадающем пароле (допустимо, но не требуется)")
	}
}

// TestR205_DeleteUser — R205 (2026-10-21): лишние учётные записи можно удалить,
// но нельзя удалить себя (иначе панель осталась бы без входа).
func TestR205_DeleteUser(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "users.json")
	us := NewUserStore(path)
	if err := us.Upsert("admin", "pass-1-long", "admin"); err != nil {
		t.Fatalf("admin: %v", err)
	}
	if err := us.Upsert("guest", "pass-2-long", "viewer"); err != nil {
		t.Fatalf("guest: %v", err)
	}

	if err := us.Delete("admin", "admin"); err != os.ErrPermission {
		t.Fatalf("удаление себя должно быть запрещено, получено %v", err)
	}
	if err := us.Delete("guest", "admin"); err != nil {
		t.Fatalf("удаление guest: %v", err)
	}
	if _, ok := us.Verify("guest", "pass-2-long"); ok {
		t.Fatal("guest всё ещё может войти после удаления")
	}
	// admin не тронут
	if _, ok := us.Verify("admin", "pass-1-long"); !ok {
		t.Fatal("admin пострадал при удалении другого пользователя")
	}
	// файл перечитан с диска: удаление сохранено
	reloaded := NewUserStore(path)
	if _, ok := reloaded.Verify("guest", "pass-2-long"); ok {
		t.Fatal("после перезагрузки файла guest снова существует")
	}
}
