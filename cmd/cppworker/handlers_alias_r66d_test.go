// handlers_alias_r66d_test.go — R66d (2026-09-23).
//
// РЕГРЕСС. Ollama-совместимые алиасы моделей (POST /api/create →
// <name>.gguf.json) были нерабочими:
//   - resolveModelPath не резолвил алиас → загрузка падала
//     («failed to load model from models/<alias>.gguf»);
//   - /api/tags не показывал алиас → клиент не видел созданную модель;
//   - delete удалял бы не то (или ничего).
//
// Тесты проверяют все три пути на временном models-dir.

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

// setupAliasTestBackend — backend + глобальный modelsDir на временном каталоге,
// с одним .gguf-файлом (size>0) и алиасом на него.
func setupAliasTestBackend(t *testing.T) (dir string, aliasName, sourceFile string) {
	t.Helper()

	dir = t.TempDir()
	oldDir := *modelsDir
	*modelsDir = dir
	t.Cleanup(func() { *modelsDir = oldDir })

	oldBackend := backend
	t.Cleanup(func() { backend = oldBackend })

	sourceFile = "gemma-4-E4B-it-qat-UD-Q4_K_XL.gguf"
	if err := os.WriteFile(filepath.Join(dir, sourceFile), make([]byte, 8192), 0o600); err != nil {
		t.Fatalf("write gguf: %v", err)
	}
	// Второй .gguf обязателен: иначе сработал бы fallback resolveModelPath
	// «в каталоге ровно один .gguf — берём его» и тест проверял бы не алиас.
	if err := os.WriteFile(filepath.Join(dir, "Qwen3-Instruct-2507-q4km.gguf"), make([]byte, 4096), 0o600); err != nil {
		t.Fatalf("write second gguf: %v", err)
	}
	aliasName = "gemma-4-E4B-it-Q4_K_M"
	rec := map[string]interface{}{
		"name":       aliasName,
		"source":     sourceFile,
		"created_at": "2026-09-23T06:00:00Z",
		"modelfile":  "FROM " + sourceFile,
	}
	raw, _ := json.MarshalIndent(rec, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, aliasName+".gguf.json"), raw, 0o600); err != nil {
		t.Fatalf("write alias: %v", err)
	}

	b := cppbackend.NewBackend(cppbackend.Config{ModelsDir: dir, DefaultCtxSize: 512, DefaultBatchSize: 64})
	if _, err := b.ModelManager().ScanModels(); err != nil {
		t.Fatalf("ScanModels: %v", err)
	}
	backend = b
	return dir, aliasName, sourceFile
}

func TestResolveModelPath_Alias_R66d(t *testing.T) {
	dir, aliasName, sourceFile := setupAliasTestBackend(t)

	got := resolveModelPath(aliasName)
	if filepath.Base(got) != sourceFile {
		t.Fatalf("resolveModelPath(%q) = %q, want путь к %q", aliasName, got, sourceFile)
	}
	if !strings.HasPrefix(got, dir) {
		t.Errorf("путь %q должен быть внутри models-dir %q", got, dir)
	}
}

func TestResolveModelPath_AliasWithGGUFSuffix_R66d(t *testing.T) {
	_, aliasName, sourceFile := setupAliasTestBackend(t)

	got := resolveModelPath(aliasName + ".gguf")
	if filepath.Base(got) != sourceFile {
		t.Fatalf("resolveModelPath(%q.gguf) = %q, want путь к %q", aliasName, got, sourceFile)
	}
}

func TestHandleOllamaTags_IncludesAlias_R66d(t *testing.T) {
	_, aliasName, sourceFile := setupAliasTestBackend(t)

	req := httptest.NewRequest(http.MethodGet, "/api/tags", nil)
	rr := httptest.NewRecorder()
	handleOllamaTags(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var body struct {
		Models []struct {
			Name    string                 `json:"name"`
			Size    int64                  `json:"size"`
			Digest  string                 `json:"digest"`
			Details map[string]interface{} `json:"details"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("ответ не JSON: %v (%s)", err, rr.Body.String())
	}

	var found bool
	for _, m := range body.Models {
		if m.Name != aliasName {
			continue
		}
		found = true
		if m.Size != 8192 {
			t.Errorf("size алиаса = %d, want 8192 (размер источника)", m.Size)
		}
		if m.Digest == "" {
			t.Error("у алиаса нет digest — клиенты используют его как идентификатор")
		}
		if got, _ := m.Details["parent_model"].(string); got != strings.TrimSuffix(sourceFile, ".gguf") {
			t.Errorf("details.parent_model = %q, want %q", got, strings.TrimSuffix(sourceFile, ".gguf"))
		}
	}
	if !found {
		t.Errorf("в /api/tags нет алиаса %q (models=%+v)", aliasName, body.Models)
	}

	// Источник тоже остаётся видимым (алиас его не подменяет).
	var srcFound bool
	for _, m := range body.Models {
		if m.Name == strings.TrimSuffix(sourceFile, ".gguf") {
			srcFound = true
		}
	}
	if !srcFound {
		t.Error("файл-источник пропал из /api/tags после добавления алиаса")
	}
}

func TestHandleDeleteModel_AliasKeepsSource_R66d(t *testing.T) {
	dir, aliasName, sourceFile := setupAliasTestBackend(t)

	req := httptest.NewRequest(http.MethodPost, "/api/models/delete", strings.NewReader(`{"name":"`+aliasName+`"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handleDeleteModel(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s, want 200", rr.Code, rr.Body.String())
	}

	// Алиас удалён…
	if _, err := os.Stat(filepath.Join(dir, aliasName+".gguf.json")); !os.IsNotExist(err) {
		t.Errorf("файл алиаса должен быть удалён, stat err=%v", err)
	}
	// …а сам .gguf на месте (иначе удаление алиаса сносило бы модель).
	if _, err := os.Stat(filepath.Join(dir, sourceFile)); err != nil {
		t.Fatalf("файл-источник %q не должен удаляться: %v", sourceFile, err)
	}
	// И алиас больше не виден в реестре.
	if _, ok := backend.ModelManager().AliasByName(aliasName); ok {
		t.Error("после удаления алиас всё ещё в реестре — нужен rescan")
	}
}

func TestHandleDeleteModel_RegularModelStillWorks_R66d(t *testing.T) {
	dir, _, sourceFile := setupAliasTestBackend(t)

	req := httptest.NewRequest(http.MethodPost, "/api/models/delete", strings.NewReader(`{"name":"`+strings.TrimSuffix(sourceFile, ".gguf")+`"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handleDeleteModel(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s, want 200", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, sourceFile)); !os.IsNotExist(err) {
		t.Errorf("обычный .gguf должен удаляться, stat err=%v", err)
	}
}

// TestHandleOllamaCreate_ThenTags_R66d — полный цикл: создание алиаса через
// Ollama-совместимый POST /api/create должно сразу (без рестарта и без ручного
// скана) делать модель видимой в /api/tags. Раньше ScanModels после создания не
// вызывался, реестр оставался пустым, и «создали, а её нет».
func TestHandleOllamaCreate_ThenTags_R66d(t *testing.T) {
	dir := t.TempDir()
	oldDir := *modelsDir
	*modelsDir = dir
	t.Cleanup(func() { *modelsDir = oldDir })

	oldBackend := backend
	t.Cleanup(func() { backend = oldBackend })

	const source = "Qwen3-Instruct-2507-q4km.gguf"
	if err := os.WriteFile(filepath.Join(dir, source), make([]byte, 4096), 0o600); err != nil {
		t.Fatalf("write gguf: %v", err)
	}
	// Второй файл — чтобы fallback «единственный .gguf» не вмешивался.
	if err := os.WriteFile(filepath.Join(dir, "extra-model.gguf"), make([]byte, 2048), 0o600); err != nil {
		t.Fatalf("write second gguf: %v", err)
	}
	backend = cppbackend.NewBackend(cppbackend.Config{ModelsDir: dir, DefaultCtxSize: 512, DefaultBatchSize: 64})
	backend.ModelManager().ScanModels()

	createReq := httptest.NewRequest(http.MethodPost, "/api/create",
		strings.NewReader(`{"name":"my-short-name","from":"`+source+`"}`))
	createReq.Header.Set("Content-Type", "application/json")
	createRR := httptest.NewRecorder()
	handleOllamaCreate(createRR, createReq)
	if createRR.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", createRR.Code, createRR.Body.String())
	}

	tagsReq := httptest.NewRequest(http.MethodGet, "/api/tags", nil)
	tagsRR := httptest.NewRecorder()
	handleOllamaTags(tagsRR, tagsReq)

	var body struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(tagsRR.Body.Bytes(), &body); err != nil {
		t.Fatalf("tags не JSON: %v", err)
	}
	for _, m := range body.Models {
		if m.Name == "my-short-name" {
			return
		}
	}
	t.Fatalf("после /api/create алиас не виден в /api/tags: %+v", body.Models)
}
