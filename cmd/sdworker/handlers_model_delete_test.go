// handlers_model_delete_test.go — R-Image Phase 9 (2026-10-03): удаление
// bundle'а с диска (POST /api/image/models/delete).
//
// ЗАЧЕМ РУЧКА. У текстового пула ручка удаления (/api/models/delete) была с
// самого начала, у image-воркера её не было: bundle'ы на 1.5–12 GB копились
// навсегда, и оператор чистил их руками. Страница «Image-модели» получает
// кнопку «Удалить с диска» — ровно как «GGUF модели».
//
// ЧТО ЗДЕСЬ ПРОВЕРЯЕТСЯ (границы безопасности, а не только happy path):
//   - успешное удаление: каталог исчез, реестр перечитан (модели больше нет в
//     /api/image/models), в ответе есть freed_bytes;
//   - ЗАГРУЖЕННЫЙ bundle удалить нельзя → 409 с подсказкой про unload (движок
//     держит веса, а супервизор считает модель рабочей);
//   - имя с разделителями/«..» → 400 (иначе ручка удаляла бы что угодно за
//     пределами каталога моделей);
//   - неизвестное имя → 404, обычный файл вместо каталога → 400.
package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// deleteBundle — вызов ручки удаления через реальный роутер.
func deleteBundle(t *testing.T, app *App, name string) (int, map[string]any) {
	t.Helper()
	router := app.setupRouter()
	rec := doJSON(t, router, http.MethodPost, "/api/image/models/delete", map[string]any{"name": name})
	var out map[string]any
	if body := strings.TrimSpace(rec.Body.String()); body != "" {
		_ = json.Unmarshal([]byte(body), &out)
	}
	return rec.Code, out
}

func TestDeleteModel_RemovesBundleAndReloadsRegistry(t *testing.T) {
	app, _ := newTestApp(t, map[string]types.ImageModelProfile{
		"sd15": {Files: []types.ImageModelFile{{Role: types.ImageFileRoleDiffusion, Filename: "model.gguf"}}},
	})
	modelsDir, err := app.svc.Config.ModelsDirAbs()
	if err != nil {
		t.Fatalf("models dir: %v", err)
	}
	// Кладём в bundle второй файл: freed_bytes должен считать ВСЁ содержимое.
	extra := filepath.Join(modelsDir, "sd15", "vae.safetensors")
	if err := os.WriteFile(extra, []byte(strings.Repeat("x", 2048)), 0o644); err != nil {
		t.Fatalf("write extra file: %v", err)
	}

	code, out := deleteBundle(t, app, "sd15")
	if code != http.StatusOK {
		t.Fatalf("delete = %d body=%v, want 200", code, out)
	}
	if _, err := os.Stat(filepath.Join(modelsDir, "sd15")); !os.IsNotExist(err) {
		t.Fatalf("каталог bundle'а не удалён (err=%v)", err)
	}
	freed, _ := out["freed_bytes"].(float64)
	if freed < 2048 {
		t.Errorf("freed_bytes = %v, want >= 2048 (размер содержимого)", out["freed_bytes"])
	}

	// Реестр перечитан: удалённой модели больше нет в списке.
	router := app.setupRouter()
	rec := doJSON(t, router, http.MethodGet, "/api/image/models", nil)
	var models struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &models); err != nil {
		t.Fatalf("json: %v", err)
	}
	for _, m := range models.Models {
		if m.Name == "sd15" {
			t.Fatalf("после удаления модель sd15 осталась в /api/image/models (реестр не перечитан)")
		}
	}
}

func TestDeleteModel_RefusesLoadedBundle(t *testing.T) {
	app, _ := newTestApp(t, map[string]types.ImageModelProfile{
		"sd15": {Files: []types.ImageModelFile{{Role: types.ImageFileRoleDiffusion, Filename: "model.gguf"}}},
	})
	router := app.setupRouter()

	if rec := doJSON(t, router, http.MethodPost, "/api/image/models/load", map[string]any{"name": "sd15"}); rec.Code != http.StatusAccepted {
		t.Fatalf("load = %d body=%s", rec.Code, rec.Body.String())
	}
	deadline := time.Now().Add(10 * time.Second)
	for app.svc.Sup.State() != "loaded" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if app.svc.Sup.State() != "loaded" {
		t.Fatalf("модель не загрузилась (state=%s) — тест про 409 на загруженный bundle", app.svc.Sup.State())
	}

	code, out := deleteBundle(t, app, "sd15")
	if code != http.StatusConflict {
		t.Fatalf("удаление загруженного bundle = %d body=%v, want 409", code, out)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "unload") {
		t.Errorf("в ошибке нет подсказки про unload: %v", out)
	}
	// Каталог на месте: отказ обязан быть ДО удаления, иначе движок остался бы
	// без весов на диске.
	modelsDir, _ := app.svc.Config.ModelsDirAbs()
	if _, err := os.Stat(filepath.Join(modelsDir, "sd15")); err != nil {
		t.Fatalf("каталог загруженного bundle тронут при отказе: %v", err)
	}
}

func TestDeleteModel_RejectsPathTraversal(t *testing.T) {
	app, _ := newTestApp(t, map[string]types.ImageModelProfile{
		"sd15": {Files: []types.ImageModelFile{{Role: types.ImageFileRoleDiffusion, Filename: "model.gguf"}}},
	})
	modelsDir, _ := app.svc.Config.ModelsDirAbs()
	// «Жертва» вне каталога моделей: если бы валидация имени пропустила «..»,
	// этот каталог был бы удалён.
	victim := filepath.Join(filepath.Dir(modelsDir), "victim")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatalf("mkdir victim: %v", err)
	}
	if err := os.WriteFile(filepath.Join(victim, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatalf("write victim: %v", err)
	}

	for _, name := range []string{"../victim", "..", ".", "sub/dir", `sub\dir`, ""} {
		code, out := deleteBundle(t, app, name)
		if code != http.StatusBadRequest {
			t.Errorf("delete(%q) = %d body=%v, want 400", name, code, out)
		}
	}
	if _, err := os.Stat(filepath.Join(victim, "keep.txt")); err != nil {
		t.Fatalf("каталог ВНЕ modelsDir удалён или испорчен: %v", err)
	}
}

func TestDeleteModel_UnknownAndNonDir(t *testing.T) {
	app, _ := newTestApp(t, map[string]types.ImageModelProfile{
		"sd15": {Files: []types.ImageModelFile{{Role: types.ImageFileRoleDiffusion, Filename: "model.gguf"}}},
	})
	modelsDir, _ := app.svc.Config.ModelsDirAbs()

	if code, _ := deleteBundle(t, app, "nope"); code != http.StatusNotFound {
		t.Errorf("удаление неизвестного bundle = %d, want 404", code)
	}

	// Обычный ФАЙЛ с именем bundle'а: удалять его как каталог нельзя.
	if err := os.WriteFile(filepath.Join(modelsDir, "notabundle"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if code, out := deleteBundle(t, app, "notabundle"); code != http.StatusBadRequest {
		t.Errorf("удаление файла-не-каталога = %d body=%v, want 400", code, out)
	}
	if _, err := os.Stat(filepath.Join(modelsDir, "notabundle")); err != nil {
		t.Errorf("файл удалён, хотя не является bundle'ом: %v", err)
	}
}
