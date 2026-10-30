// routes_auth_matrix_r339_test.go — R339 (2026-10-30), ЭТАП 4 глобальной проверки
// API: матрица админских ручек.
//
// ЗАЧЕМ. Аудит маршрутов (docs/plans/2026-10-30-route-map.md, §12 п.10) отметил:
// админские ручки перечислены, но ПОВЕДЕНИЕ каждой не разобрано — тела, коды,
// точная авторизация. Худший дефект этого класса — открытая ручка управления:
// она не видна ни в логах, ни в мониторе, просто принимает команды у кого угодно.
//
// ЧТО ПРОВЕРЯЕТ ЭТОТ НАБОР (по каждой зарегистрированной админ-ручке):
//   - ручка, обёрнутая AuthMiddleware, БЕЗ токена отвечает 401/403 (а не 200/400/500);
//   - публичные ручки (перечислены явно, с причиной) без токена НЕ отвечают 401/403.
//
// ЧЕГО ЗДЕСЬ СОЗНАТЕЛЬНО НЕТ: проверки «с токеном ручка работает». Большинство
// админ-ручек ходит в воркеры по сети, и в тесте без стенда такой вызов либо
// висит, либо зависит от внешнего состояния — проверка превратилась бы в источник
// флаки. Положительный путь этих ручек покрыт их собственными тестами
// (internal/api/*_test.go) и живой матрицей (tests/api_contract).
//
// СПИСКИ. Protected-ручки читаются ИЗ ИСХОДНИКА routes.go: всё, что обёрнуто
// `AuthMiddleware(`, обязано попасть в проверку — если кто-то добавит ручку и
// забудет, стюард это поймает. Публичные перечислены руками: «публичная ручка» —
// это осознанное решение, а не забытая обёртка.
package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"testing"
)

// publicAdminRoutesR339 — ручки, которые НАМЕРЕННО без токена.
//
// Каждая — с причиной: без этого список превращается в свалку забытых обёрток.
var publicAdminRoutesR339 = map[string]string{
	"/api/v1/health":         "health-check для балансировщика и монитора",
	"/api/v1/gguf/backends":  "публичный список GGUF-бэкендов для страницы WebUI (R65d)",
	"/api/v1/backends/types": "справочник типов бэкендов, не состояние кластера",
	"/api/v1/ratelimit/status": "статус лимитера (диагностика, без управления)",
	"/api/v1/version":        "версия балансера",
	"/api/v1/auth/login":     "вход (пароль проверяется внутри обработчика)",
	"/api/v1/auth/logout":    "выход (сессия проверяется внутри)",
	"/api/v1/auth/me":        "текущий пользователь (сессия проверяется внутри)",
}

// protectedRoutesFromSourceR339 — вытащить из routes.go пути, обёрнутые AuthMiddleware.
func protectedRoutesFromSourceR339(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("routes.go")
	if err != nil {
		t.Fatalf("чтение routes.go: %v", err)
	}
	src := string(raw)
	// s.mux.Handle("/api/v1/...", AuthMiddleware(...)) и
	// s.mux.Handle("/path", AuthMiddleware(...)) — берём первый строковый литерал.
	re := regexp.MustCompile(`(?m)s\.mux\.Handle(?:Func)?\("([^"]+)"[^\n]*AuthMiddleware\(`)
	seen := map[string]bool{}
	var out []string
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		path := m[1]
		if seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

// adminR339Token — токен тестового админ-сервера (см. authEnabledServer).
const adminR339Token = "r65d-secret-token"

// doAdminR339 — запрос к админ-серверу с токеном или без.
//
// ВАЖНО: сервер создаётся С включённой аутентификацией (authEnabledServer):
// только тогда AuthMiddleware реально режет запрос ДО обработчика. На сервере без
// токенов ручки исполняются, и матрица не про авторизацию, а про сетевые вызовы
// (часть ручек ходит в воркеры — это и подвесило первый прогон).
func doAdminR339(t *testing.T, s *Server, method, path string, withToken bool) int {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if withToken {
		req.Header.Set("X-API-Token", adminR339Token)
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec.Code
}

// TestR339_AdminRoutesAuthMatrix — главный стюард: авторизация по каждой ручке.
func TestR339_AdminRoutesAuthMatrix(t *testing.T) {
	s := authEnabledServer(t)
	protected := protectedRoutesFromSourceR339(t)
	if len(protected) == 0 {
		t.Fatal("в routes.go не найдено ни одной ручки под AuthMiddleware — стюард сломался (изменился стиль регистрации)")
	}

	for _, path := range protected {
		if reason, isPublic := publicAdminRoutesR339[path]; isPublic {
			t.Errorf("ручка %q под AuthMiddleware, но объявлена публичной (%s) — одно из двух неверно", path, reason)
			continue
		}
		code := doAdminR339(t, s, http.MethodGet, path, false)
		if code == http.StatusUnauthorized || code == http.StatusForbidden {
			// Ожидаемое поведение защищённой ручки без токена.
			continue
		}
		if code >= 500 {
			t.Errorf("%s без токена: HTTP %d — ручка упала до проверки авторизации (клиент увидит 500 вместо 401)", path, code)
		}
		// 200/400/405 без токена означают, что защита не сработала.
		t.Errorf("%s без токена: HTTP %d, want 401/403 — админ-ручка доступна без авторизации", path, code)
	}
}

// TestR339_PublicRoutesStayPublic — публичные ручки не отвечают 401 без токена.
//
// Смысл: «публичность» должна быть решением, а не случайностью. Если ручку
// случайно завернут в AuthMiddleware, WebUI/монитор потеряют источник данных —
// и это заметят уже пользователи.
func TestR339_PublicRoutesStayPublic(t *testing.T) {
	s := authEnabledServer(t)
	for path, reason := range publicAdminRoutesR339 {
		code := doAdminR339(t, s, http.MethodGet, path, false)
		if code == http.StatusUnauthorized || code == http.StatusForbidden {
			t.Errorf("%s объявлена публичной (%s), но отвечает %d без токена", path, reason, code)
		}
	}
}
