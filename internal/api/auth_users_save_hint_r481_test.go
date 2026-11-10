// auth_users_save_hint_r481_test.go — R481: ошибка записи users.json обязана
// быть действенной.
//
// Живая жалоба: «не даёт создавать пользователей — у контейнера нет прав на
// config/users.json». Причина была в ПУТИ по умолчанию (read-only ../config), но
// оператор видел только текст ошибки ОС и искал права в контейнере WebUI.
// Теперь ответ несёт путь записи и подсказку про том, а UI их показывает.
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestR481_UserSaveErrorCarriesPathAndHint — запись в недоступный путь даёт
// 400 с полями path и hint (а не только сырой текст ОС).
func TestR481_UserSaveErrorCarriesPathAndHint(t *testing.T) {
	dir := t.TempDir()
	// Делаем «каталогом» обычный файл: создание каталога/файла пользователей
	// внутри него гарантированно падает и на Windows, и на Linux.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("подготовка: %v", err)
	}
	brokenPath := filepath.Join(blocker, "users.json")
	users := NewUserStore(brokenPath)
	sessions := NewSessionStore()

	h := AuthUsersHandler(users, sessions)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/auth/users",
		strings.NewReader(`{"name":"newbie","password":"good-password","role":"viewer"}`))
	sess := sessions.Create("admin", "admin")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sess.ID})
	req.Header.Set("X-Authenticated-Token", "session:admin")
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("HTTP %d, ожидался 400 (тело: %s)", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("ответ не json: %v (%s)", err, rec.Body.String())
	}
	if resp["error"] == nil || resp["error"] == "" {
		t.Fatalf("нет поля error: %v", resp)
	}
	if got, _ := resp["path"].(string); got != brokenPath {
		t.Fatalf("path = %q, ожидался %q", got, brokenPath)
	}
	hint, _ := resp["hint"].(string)
	if !strings.Contains(hint, "/app/data/users.json") {
		t.Fatalf("hint не объясняет, куда должен идти файл: %q", hint)
	}
}
