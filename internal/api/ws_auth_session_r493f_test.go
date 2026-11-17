//go:build llama_stub

// ws_auth_session_r493f_test.go — R493f (2026-11-17).
//
// ЗАЧЕМ. Живой дефект на стенде: вошедший оператор видел на странице «Логи»
// только
//
//	ERROR  Ошибка WebSocket
//	WARN   Переподключение... (1/10) через 5с
//	… и так по кругу до 10/10,
//
// а /ws/metrics не поднимался вовсе. Замер через headless-браузер (Edge,
// 2026-11-17) дал точную причину:
//
//	CONSOLE-ERR: WebSocket connection to 'ws://127.0.0.1:18083/ws/logs' failed:
//	             HTTP Authentication failed; no valid credentials available
//
// Код: оба WS-обработчика требовали `?token=` и сессию НЕ смотрели, хотя весь
// остальной API живёт на сессии WebUI (R201/R202), а токен в страницу больше не
// вшивается (R222). То есть live-tail логов и живой график были недоступны
// КАЖДОМУ вошедшему оператору, а не «иногда глючили».
//
// Контракт R493f: доступ к WS даёт либо сессия (cookie ol_session), либо валидный
// токен (заголовок или query — браузер не умеет задавать заголовки на WebSocket).
package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// wsAuthR493f — режущая сборка: enabled + хранилище сессий + известный токен.
func wsAuthR493f(t *testing.T) (*TokenAuthenticator, string) {
	t.Helper()
	a := NewTokenAuthenticator([]string{"secret-token-r493f"}, "X-API-Token", true)
	store := NewSessionStore()
	sess := store.Create("operator", "admin")
	a.SetSessions(store)
	return a, sess.ID
}

func reqWithCookieR493f(sessionID string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/ws/logs", nil)
	if sessionID != "" {
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessionID})
	}
	return r
}

// TestR493f_AuthorizeWS_AllowsSession — сессия WebUI открывает WS (главный кейс).
func TestR493f_AuthorizeWS_AllowsSession(t *testing.T) {
	a, sid := wsAuthR493f(t)
	if err := a.AuthorizeWS(reqWithCookieR493f(sid)); err != nil {
		t.Fatalf("AuthorizeWS с валидной сессией = %v, ожидалось nil: "+
			"оператор после входа не имеет токена в странице (R222), и без этой ветки "+
			"WS недоступен ему вообще (живой цикл «Ошибка WebSocket»)", err)
	}
}

// TestR493f_AuthorizeWS_AllowsQueryToken — токен в query (скрипты, break-glass).
func TestR493f_AuthorizeWS_AllowsQueryToken(t *testing.T) {
	a, _ := wsAuthR493f(t)
	r := httptest.NewRequest(http.MethodGet, "/ws/logs?token=secret-token-r493f", nil)
	if err := a.AuthorizeWS(r); err != nil {
		t.Fatalf("AuthorizeWS с валидным query-токеном = %v, ожидалось nil", err)
	}
}

// TestR493f_AuthorizeWS_AllowsHeaderToken — токен в заголовке (не-WS клиенты).
func TestR493f_AuthorizeWS_AllowsHeaderToken(t *testing.T) {
	a, _ := wsAuthR493f(t)
	r := httptest.NewRequest(http.MethodGet, "/ws/logs", nil)
	r.Header.Set("X-API-Token", "secret-token-r493f")
	if err := a.AuthorizeWS(r); err != nil {
		t.Fatalf("AuthorizeWS с валидным заголовком = %v, ожидалось nil", err)
	}
}

// TestR493f_AuthorizeWS_DeniesAnonymous — без сессии и без токена доступа нет.
// Положительные ветки без этой проверки ничего не доказывали бы.
func TestR493f_AuthorizeWS_DeniesAnonymous(t *testing.T) {
	a, _ := wsAuthR493f(t)
	if err := a.AuthorizeWS(reqWithCookieR493f("")); err != ErrWSUnauthorized {
		t.Fatalf("AuthorizeWS анонимно = %v, ожидалось ErrWSUnauthorized", err)
	}
	// Просроченная/чужая сессия — тоже отказ (не «любая cookie подходит»).
	if err := a.AuthorizeWS(reqWithCookieR493f("no-such-session")); err != ErrWSUnauthorized {
		t.Fatalf("AuthorizeWS с чужой сессией = %v, ожидалось ErrWSUnauthorized", err)
	}
	// Неверный токен — отказ.
	r := httptest.NewRequest(http.MethodGet, "/ws/logs?token=wrong", nil)
	if err := a.AuthorizeWS(r); err != ErrWSUnauthorized {
		t.Fatalf("AuthorizeWS с неверным токеном = %v, ожидалось ErrWSUnauthorized", err)
	}
}

// TestR493f_AuthorizeWS_DisabledAuthAllowsAll — при выключенной аутентификации
// WS работает как раньше (иначе правка сломала бы локальные сборки без auth).
func TestR493f_AuthorizeWS_DisabledAuthAllowsAll(t *testing.T) {
	a := NewTokenAuthenticator(nil, "X-API-Token", false)
	if err := a.AuthorizeWS(reqWithCookieR493f("")); err != nil {
		t.Fatalf("AuthorizeWS при выключенной auth = %v, ожидалось nil", err)
	}
}

// TestR493f_WSHandlers_AcceptSession — сквозная проверка на самих обработчиках:
// с сессией они НЕ отвечают 401 (до правки отвечали 401 всегда).
func TestR493f_WSHandlers_AcceptSession(t *testing.T) {
	a, sid := wsAuthR493f(t)
	s := &Server{authenticator: a}
	for name, h := range map[string]http.HandlerFunc{
		"/ws/logs":    s.wsLogsHandler,
		"/ws/metrics": s.wsMetricsHandler,
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, name, nil)
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
		h(w, r)
		if w.Code == http.StatusUnauthorized {
			t.Errorf("%s с валидной сессией вернул 401 — именно это видел оператор в UI", name)
		}
		// Аноним — обязан получить 401 (граница не размыта).
		w2 := httptest.NewRecorder()
		h(w2, httptest.NewRequest(http.MethodGet, name, nil))
		if w2.Code != http.StatusUnauthorized {
			t.Errorf("%s анонимно = %d, ожидалось 401", name, w2.Code)
		}
	}
}
