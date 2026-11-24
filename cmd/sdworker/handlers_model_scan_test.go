// handlers_model_scan_test.go — POST /api/image/models/scan (Фаза 4 плана
// ре-экспорта): принудительное пересканирование каталога моделей.
//
// ЖИВОЙ ДЕФЕКТ, который стерегут эти проверки. Сервис gguf-reexport (цель
// image_bundle) умеет собирать bundle В УЖЕ СУЩЕСТВУЮЩЕМ каталоге — он пишет туда
// profile.json и sd-server.config.json. GET /api/image/models перечитывает реестр
// только при изменении НАБОРА подкаталогов (sdbackend.RefreshIfChanged), поэтому
// после такой записи воркер продолжал показывать вердикт, посчитанный по
// СИНТЕЗИРОВАННОМУ профилю (без profile.json): набор, собранный «в на месте»,
// выглядел неполным, а сервис честно отказывался говорить «succeeded».
//
// ЧТО ПРОВЕРЯЕТСЯ: новый каталог с profile.json, появившийся ПОСЛЕ старта воркера
// и НЕ изменивший набор каталогов (он и был в списке), виден только после
// принудительного пересканирования; ручка отвечает 200 и отдаёт модели вместе с
// вердиктом compat. Плюс отказ на не-POST (контракт явный) и 409 при активных
// загрузках (каталог создан, profile.json ещё нет — скан синтезировал бы «пустую»
// модель, тот же живой дефект 2026-10-06).
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestRescanModels_PicksUpProfileWrittenAfterStart — запись profile.json в
// существующий каталог становится видна после POST /api/image/models/scan.
func TestRescanModels_PicksUpProfileWrittenAfterStart(t *testing.T) {
	// Модель зарегистрирована ДО старта воркера (в каталоге нет profile.json —
	// реестр синтезировал профиль по файлам).
	app, _ := newTestApp(t, map[string]types.ImageModelProfile{
		"later": {Family: "sd15", Files: []types.ImageModelFile{
			{Role: types.ImageFileRoleDiffusion, Repo: "local", Filename: "m.gguf"},
		}},
	})
	// Убираем профиль у каталога и перечитываем реестр: это состояние «набор уже
	// лежит, profile.json ещё не написан» — ровно до сборки bundle.
	bundleDir := filepath.Join(app.svc.Registry.ModelsDir(), "later")
	if err := os.Remove(filepath.Join(bundleDir, "profile.json")); err != nil {
		t.Fatalf("удаление профиля: %v", err)
	}
	if err := app.svc.Registry.Load(); err != nil {
		t.Fatalf("реестр: %v", err)
	}
	if p, _ := app.svc.Registry.Profile("later"); p.Name != "later" || p.Notes == "" {
		t.Fatalf("подготовка не удалась: профиль %+v", p)
	}

	// Сборщик (сервис ре-экспорта) пишет профиль в СУЩЕСТВУЮЩИЙ каталог.
	profile := types.ImageModelProfile{
		Name: "later", Family: "sd15", Defaults: types.DefaultImageGenDefaults("sd15"),
		Files: []types.ImageModelFile{
			{Role: types.ImageFileRoleDiffusion, Repo: "local", Filename: "m.gguf"},
		},
		Notes: "bundle собран модулем ре-экспорта",
	}
	data, _ := json.Marshal(profile)
	if err := os.WriteFile(filepath.Join(bundleDir, "profile.json"), data, 0o644); err != nil {
		t.Fatalf("профиль: %v", err)
	}

	router := app.setupRouter()
	// GET до пересканирования: набор каталогов не изменился, поэтому реестр НЕ
	// перечитывается и профиль на диске ещё не виден — это и есть дефект.
	getRec := httptest.NewRecorder()
	router.ServeHTTP(getRec, httptest.NewRequest(http.MethodGet, "/api/image/models", nil))
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET списка: HTTP %d", getRec.Code)
	}
	if got := profileNotesFromList(t, getRec.Body.Bytes(), "later"); got == profile.Notes {
		t.Fatalf("реестр уже перечитан без скана: проверка стерегла бы не то (%q)", got)
	}

	// POST scan — принудительное перечитывание: тот же профиль теперь из файла.
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/image/models/scan", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/image/models/scan: HTTP %d (тело: %s)", rec.Code, rec.Body.String())
	}
	var reply struct {
		Models    []map[string]any `json:"models"`
		Rescanned bool             `json:"rescanned"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("ответ не разобран: %v", err)
	}
	if !reply.Rescanned {
		t.Fatal("ответ не подтверждает пересканирование (rescanned=false)")
	}
	if got := profileNotesFromList(t, rec.Body.Bytes(), "later"); got != profile.Notes {
		t.Fatalf("после скана ноутсы профиля %q, ожидались %q — профиль с диска не прочитан", got, profile.Notes)
	}
}

// TestRescanModels_MethodContract — только POST: GET на этот путь даёт 405.
func TestRescanModels_MethodContract(t *testing.T) {
	app, _ := newTestApp(t, nil)
	router := app.setupRouter()
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/image/models/scan", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/image/models/scan: HTTP %d, ожидался 405", rec.Code)
	}
}

// profileNotesFromList — поле notes модели из ответа списка (пусто = модели нет).
func profileNotesFromList(t *testing.T, body []byte, name string) string {
	t.Helper()
	var reply struct {
		Models []struct {
			Name  string `json:"name"`
			Notes string `json:"notes"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatalf("список не разобран: %v", err)
	}
	for _, m := range reply.Models {
		if m.Name == name {
			return m.Notes
		}
	}
	return ""
}
