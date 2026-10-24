//go:build llama_stub

// auth_hardening_r225_test.go — R225 (2026-10-22): три дыры в аутентификации,
// найденные при ревью R222/R223.
//
//  1. /auth/me верил ЛЮБОМУ непустому X-API-Token: с неверным токеном панель
//     показывала админские действия (запросы при этом получали 401);
//  2. минимальная длина пароля проверялась только в браузере — через API можно
//     было поставить пароль из одного символа;
//  3. список пользователей (и роли!) читался ролью viewer, потому что
//     AuthMiddleware проверяет роль только на мутациях;
//  4. погасить чужой вход можно было только сменой пароля — оператор не видел,
//     кто и откуда вошёл.
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func r225Users(t *testing.T) *UserStore {
	t.Helper()
	return testUserStore(t)
}

// r225SessionReq — запрос от имени сессии (как его видит handler ПОСЛЕ
// AuthMiddleware: cookie + X-Authenticated-Token).
func r225SessionReq(sessions *SessionStore, user, role, method, path string) *http.Request {
	sess := sessions.Create(user, role)
	req := httptest.NewRequest(method, path, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sess.ID})
	req.Header.Set("X-Authenticated-Token", "session:"+user)
	return req
}

func TestR225_MeRejectsInvalidToken(t *testing.T) {
	users := r225Users(t)
	sessions := NewSessionStore()
	auth := NewTokenAuthenticator([]string{"good-token-r225"}, "X-API-Token", true)
	me := AuthMeHandler(users, sessions, auth)

	// Неверный токен: админских прав в UI быть не должно.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.Header.Set("X-API-Token", "wrong-token")
	rec := httptest.NewRecorder()
	me(rec, req)
	var bad map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &bad); err != nil {
		t.Fatalf("ответ не json: %v", err)
	}
	if bad["tokenAuth"] == true {
		t.Error("неверный токен признан админским доступом — панель покажет админские действия, а запросы будут 401")
	}

	// Верный токен: break-glass работает.
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req2.Header.Set("X-API-Token", "good-token-r225")
	rec2 := httptest.NewRecorder()
	me(rec2, req2)
	var good map[string]interface{}
	_ = json.Unmarshal(rec2.Body.Bytes(), &good)
	if good["tokenAuth"] != true {
		t.Errorf("верный токен не признан: %v", good)
	}
}

func TestR225_PasswordMinLengthOnServer(t *testing.T) {
	us := r225Users(t)

	if err := us.Upsert("shorty", "1234567", "viewer"); err == nil {
		t.Fatal("пароль из 7 символов принят сервером — политика только в UI")
	} else if !strings.Contains(err.Error(), "короче") {
		t.Errorf("непонятная ошибка: %v", err)
	}
	if err := us.Upsert("normal", "12345678", "viewer"); err != nil {
		t.Fatalf("пароль из 8 символов отвергнут: %v", err)
	}
	// Смена только роли (пустой пароль) по-прежнему работает и хеш не теряется.
	if err := us.Upsert("normal", "", "admin"); err != nil {
		t.Fatalf("смена роли без пароля сломалась: %v", err)
	}
	if _, ok := us.Verify("normal", "12345678"); !ok {
		t.Error("пароль потерялся при смене только роли")
	}
}

func TestR225_UsersEndpointIsAdminOnly(t *testing.T) {
	users := r225Users(t)
	sessions := NewSessionStore()
	h := AuthUsersHandler(users, sessions)

	// viewer получает 403 — даже на чтение.
	recViewer := httptest.NewRecorder()
	h(recViewer, r225SessionReq(sessions, "guest", "viewer", http.MethodGet, "/api/v1/auth/users"))
	if recViewer.Code != http.StatusForbidden {
		t.Errorf("viewer читает список пользователей: код %d", recViewer.Code)
	}

	// admin — 200.
	recAdmin := httptest.NewRecorder()
	h(recAdmin, r225SessionReq(sessions, "admin", "admin", http.MethodGet, "/api/v1/auth/users"))
	if recAdmin.Code != http.StatusOK {
		t.Errorf("admin не получил список: код %d (%s)", recAdmin.Code, recAdmin.Body.String())
	}

	// break-glass master token (AuthMiddleware кладёт его в X-Authenticated-Token) — 200.
	reqTok := httptest.NewRequest(http.MethodGet, "/api/v1/auth/users", nil)
	reqTok.Header.Set("X-Authenticated-Token", "master-token")
	recTok := httptest.NewRecorder()
	h(recTok, reqTok)
	if recTok.Code != http.StatusOK {
		t.Errorf("master token не считается администратором: код %d", recTok.Code)
	}
}

func TestR225_SessionsListAndRevoke(t *testing.T) {
	users := r225Users(t)
	sessions := NewSessionStore()
	login := AuthLoginHandler(users, sessions)

	// Вход с IP/UA — эти данные нужны оператору в списке.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
		strings.NewReader(`{"name":"admin","password":"s3cret-pass"}`))
	req.RemoteAddr = "203.0.113.9:4444"
	req.Header.Set("X-Forwarded-For", "198.51.100.7, 10.0.0.1")
	req.Header.Set("User-Agent", "TestBrowser/1.0")
	rec := httptest.NewRecorder()
	login(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("логин: код %d (%s)", rec.Code, rec.Body.String())
	}
	var userCookie string
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			userCookie = c.Value
		}
	}
	if userCookie == "" {
		t.Fatal("cookie сессии не выставлена")
	}

	// Админ смотрит список.
	adminSess := sessions.Create("admin", "admin")
	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/sessions", nil)
	listReq.AddCookie(&http.Cookie{Name: sessionCookieName, Value: adminSess.ID})
	listReq.Header.Set("X-Authenticated-Token", "session:admin")
	listRec := httptest.NewRecorder()
	AuthSessionsHandler(sessions)(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("список сессий: код %d (%s)", listRec.Code, listRec.Body.String())
	}
	var payload struct {
		Sessions []struct {
			ID        string `json:"id"`
			User      string `json:"user"`
			Role      string `json:"role"`
			IP        string `json:"ip"`
			UserAgent string `json:"user_agent"`
			Current   bool   `json:"current"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("ответ не json: %v", err)
	}
	var target string
	for _, s := range payload.Sessions {
		if s.ID == "" || len(s.ID) != 8 {
			t.Errorf("id в списке должен быть коротким (8), получено %q", s.ID)
		}
		// «Текущая» — ровно та сессия, чьей cookie пришёл запрос.
		if wantCurrent := strings.HasPrefix(adminSess.ID, s.ID); s.Current != wantCurrent {
			t.Errorf("current=%v для %q, ожидалось %v", s.Current, s.User, wantCurrent)
		}
		if strings.HasPrefix(userCookie, s.ID) {
			target = s.ID
			if s.Current {
				t.Error("сессия пользователя помечена как текущая")
			}
			if s.IP != "198.51.100.7" {
				t.Errorf("IP сессии %q, ожидался 198.51.100.7", s.IP)
			}
			if s.UserAgent != "TestBrowser/1.0" {
				t.Errorf("User-Agent %q", s.UserAgent)
			}
		}
	}
	if target == "" {
		t.Fatalf("сессия пользователя не найдена в списке: %+v", payload.Sessions)
	}
	// Полный cookie-id наружу не отдаём.
	if strings.Contains(listRec.Body.String(), userCookie) {
		t.Error("в ответ попал полный cookie-id — это фактически пароль сессии")
	}

	// Отзыв по короткому id.
	delReq := httptest.NewRequest(http.MethodDelete, "/api/v1/auth/sessions?id="+target, nil)
	delReq.AddCookie(&http.Cookie{Name: sessionCookieName, Value: adminSess.ID})
	delReq.Header.Set("X-Authenticated-Token", "session:admin")
	delRec := httptest.NewRecorder()
	AuthSessionsHandler(sessions)(delRec, delReq)
	if delRec.Code != http.StatusOK {
		t.Fatalf("отзыв сессии: код %d (%s)", delRec.Code, delRec.Body.String())
	}
	if _, ok := sessions.Get(userCookie); ok {
		t.Error("после отзыва сессия всё ещё действительна")
	}
}

func TestR225_SessionsRejectsShortPrefix(t *testing.T) {
	sessions := NewSessionStore()
	_ = sessions.Create("admin", "admin")
	if sessions.DeleteByShortID("abc") {
		t.Error("короткий префикс принят — можно погасить не ту сессию")
	}
	if sessions.Count() != 1 {
		t.Errorf("сессии изменились: %d", sessions.Count())
	}
}

func TestR225_ViewerCannotListSessions(t *testing.T) {
	sessions := NewSessionStore()
	viewer := r225SessionReq(sessions, "guest", "viewer", http.MethodGet, "/api/v1/auth/sessions")
	rec := httptest.NewRecorder()
	AuthSessionsHandler(sessions)(rec, viewer)
	if rec.Code != http.StatusForbidden {
		t.Errorf("viewer видит список сессий: код %d", rec.Code)
	}
}
