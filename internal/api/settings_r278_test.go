package api

// settings_r278_test.go — R278: API реестра «горячих» настроек.
//
// Проверяем контракт, на который будет опираться WebUI (этап 3): список с
// источником значения, запись «горячей» настройки, отказ (400) для настройки,
// которая требует рестарта, и 404 для неизвестного имени. Отдельно — что ответ
// не содержит секретов: реестр вообще не хранит секретов, и тест это фиксирует.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/internal/settings"
)

// withSettingsRegistryAPI — глобальный реестр на время теста.
func withSettingsRegistryAPI(t *testing.T) *settings.Registry {
	t.Helper()
	reg := settings.Init("")
	t.Cleanup(func() {
		_ = reg.ResetAll()
		settings.Init("")
	})
	return reg
}

func TestR278_SettingsListAndSource(t *testing.T) {
	withSettingsRegistryAPI(t)
	s := newAgentCapacityServer(t)

	rec := httptest.NewRecorder()
	s.settingsHandler(rec, httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: HTTP %d (%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Settings []struct {
			Name      string `json:"name"`
			Type      string `json:"type"`
			HotReload bool   `json:"hotReload"`
			Default   int64  `json:"default"`
			Effective int64  `json:"effective"`
			Source    string `json:"source"`
		} `json:"settings"`
		Count int `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("не JSON: %v", err)
	}
	if body.Count == 0 || len(body.Settings) != body.Count {
		t.Fatalf("пустой список настроек: count=%d len=%d", body.Count, len(body.Settings))
	}
	found := false
	for _, e := range body.Settings {
		if e.Name == "LB_STREAM_MAX_LINE_BYTES" {
			found = true
			if !e.HotReload {
				t.Error("LB_STREAM_MAX_LINE_BYTES должна быть hot-reloadable")
			}
			if e.Default == 0 {
				t.Error("дефолт не отдан")
			}
			if e.Source != string(settings.SourceDefault) {
				t.Errorf("source = %q, want default (env не задан)", e.Source)
			}
		}
	}
	if !found {
		t.Fatal("в списке нет LB_STREAM_MAX_LINE_BYTES")
	}
	// Секретов быть не должно: реестр хранит только числовые лимиты, но если
	// кто-то добавит сюда строку с токеном, тест обязан это заметить.
	lower := strings.ToLower(rec.Body.String())
	for _, bad := range []string{"password", "token", "secret"} {
		if strings.Contains(lower, bad) {
			t.Errorf("в ответе есть поле, похожее на секрет: %q", bad)
		}
	}
}

func TestR278_SettingsPutAndReset(t *testing.T) {
	withSettingsRegistryAPI(t)
	s := newAgentCapacityServer(t)

	// Валидная запись горячей настройки.
	rec := httptest.NewRecorder()
	s.settingsHandler(rec, httptest.NewRequest(http.MethodPut, "/api/v1/settings",
		strings.NewReader(`{"name":"LB_RELOAD_DEFER_MAX_SEC","value":30}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: HTTP %d (%s)", rec.Code, rec.Body.String())
	}
	var put struct {
		Effective int64  `json:"effective"`
		Source    string `json:"source"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &put)
	if put.Effective != 30 || put.Source != string(settings.SourceOverride) {
		t.Fatalf("PUT: effective=%d source=%s", put.Effective, put.Source)
	}

	// Reset по имени возвращает env/дефолт.
	rec = httptest.NewRecorder()
	s.settingsResetHandler(rec, httptest.NewRequest(http.MethodPost, "/api/v1/settings/reset",
		strings.NewReader(`{"name":"LB_RELOAD_DEFER_MAX_SEC"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("reset: HTTP %d (%s)", rec.Code, rec.Body.String())
	}
	var after struct {
		Source string `json:"source"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &after)
	if after.Source != string(settings.SourceDefault) {
		t.Fatalf("после reset source = %q, want default", after.Source)
	}
}

func TestR278_SettingsRejectsNonHotAndUnknown(t *testing.T) {
	reg := withSettingsRegistryAPI(t)
	// Настройка, которая требует рестарта: оператор должен получить внятный
	// отказ, а не «сохранено» с применением «когда-нибудь».
	reg.Register(settings.Setting{
		Name:      "LB_TEST_ONLY_RESTART",
		Type:      settings.TypeInt,
		Default:   1,
		HotReload: false,
	})
	s := newAgentCapacityServer(t)

	rec := httptest.NewRecorder()
	s.settingsHandler(rec, httptest.NewRequest(http.MethodPut, "/api/v1/settings",
		strings.NewReader(`{"name":"LB_TEST_ONLY_RESTART","value":2}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("не-hot: HTTP %d, want 400 (%s)", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	s.settingsHandler(rec, httptest.NewRequest(http.MethodPut, "/api/v1/settings",
		strings.NewReader(`{"name":"LB_NO_SUCH_SETTING","value":1}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("неизвестная: HTTP %d, want 404 (%s)", rec.Code, rec.Body.String())
	}

	// Без value — 400, а не «записать ноль».
	rec = httptest.NewRecorder()
	s.settingsHandler(rec, httptest.NewRequest(http.MethodPut, "/api/v1/settings",
		strings.NewReader(`{"name":"LB_RELOAD_DEFER_MAX_SEC"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("без value: HTTP %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
}
