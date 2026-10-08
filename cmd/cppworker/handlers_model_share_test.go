// handlers_model_share_test.go — R89 (2026-10-08): перенос GGUF между бэкендами.
//
// ЖИВОЙ СЦЕНАРИЙ: файл модели есть только на одной машине; вторая не должна
// качать его из HuggingFace заново. Источник отдаёт файл как есть, приёмник
// сохраняет его атомарно и пересканирует инвентарь — иначе импортированная
// модель не появится в /api/models/files до рестарта.
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

// newShareBackend — тестовый воркер с каталогом моделей и (опционально) файлами.
//
// Тот же приём, что в handlers_alias_r66d_test.go: подменяем package-level
// `backend`, потому что хендлеры берут менеджер моделей из него.
func newShareBackend(t *testing.T, files map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	previous := backend
	t.Cleanup(func() { backend = previous })
	backend = cppbackend.NewBackend(cppbackend.Config{
		ModelsDir:        dir,
		DefaultCtxSize:   512,
		DefaultBatchSize: 64,
	})
	if _, err := backend.ModelManager().ScanModels(); err != nil {
		t.Fatalf("ScanModels: %v", err)
	}
	return dir
}

func shareDo(method, path string, body []byte) *httptest.ResponseRecorder {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	rec := httptest.NewRecorder()
	switch method {
	case http.MethodGet, http.MethodHead:
		handleExportModel(rec, req)
	case http.MethodPost:
		handleImportModel(rec, req)
	}
	return rec
}

// TestModelShare_HeadAndExportGiveExactSize — балансер спрашивает размер (HEAD),
// затем качает поток; Content-Length обязан быть точным, иначе прогресс врёт.
func TestModelShare_HeadAndExportGiveExactSize(t *testing.T) {
	payload := bytes.Repeat([]byte("GGUF"), 100_000) // 400 000 байт
	newShareBackend(t, map[string][]byte{"tiny-model.gguf": payload})

	head := shareDo(http.MethodHead, "/api/models/export?name=tiny-model.gguf", nil)
	if head.Code != http.StatusOK {
		t.Fatalf("HEAD: status=%d body=%s", head.Code, head.Body.String())
	}
	if got := head.Header().Get("Content-Length"); got != strconv.Itoa(len(payload)) {
		t.Errorf("Content-Length = %q, want %d", got, len(payload))
	}
	if got := head.Header().Get("X-Model-Filename"); got != "tiny-model.gguf" {
		t.Errorf("X-Model-Filename = %q", got)
	}
	if got := head.Header().Get("X-Model-SizeBytes"); got != strconv.Itoa(len(payload)) {
		t.Errorf("X-Model-SizeBytes = %q, want %d", got, len(payload))
	}

	get := shareDo(http.MethodGet, "/api/models/export?name=tiny-model.gguf", nil)
	if get.Code != http.StatusOK {
		t.Fatalf("GET: status=%d", get.Code)
	}
	if !bytes.Equal(get.Body.Bytes(), payload) {
		t.Errorf("тело экспорта отличается от файла: %d байт vs %d", get.Body.Len(), len(payload))
	}

	// Имя без расширения резолвится в тот же файл (оператор пишет «tiny-model»).
	byName := shareDo(http.MethodHead, "/api/models/export?name=tiny-model", nil)
	if byName.Code != http.StatusOK {
		t.Errorf("экспорт по имени без .gguf: status=%d", byName.Code)
	}
}

// TestModelShare_ImportRoundTripAndRescan — приёмник сохраняет файл, а инвентарь
// подхватывает его без рестарта.
func TestModelShare_ImportRoundTripAndRescan(t *testing.T) {
	payload := bytes.Repeat([]byte("Q4KM"), 50_000)
	newShareBackend(t, map[string][]byte{"tiny-model.gguf": payload})
	src := shareDo(http.MethodGet, "/api/models/export?name=tiny-model.gguf", nil).Body.Bytes()

	dstDir := newShareBackend(t, nil)
	rec := shareDo(http.MethodPost,
		"/api/models/import?filename=tiny-model.gguf&size="+strconv.Itoa(len(src)), src)
	if rec.Code != http.StatusOK {
		t.Fatalf("import: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["filename"] != "tiny-model.gguf" || body["sha256"] == "" {
		t.Errorf("ответ импорта без filename/sha256: %v", body)
	}

	onDisk, err := os.ReadFile(filepath.Join(dstDir, "tiny-model.gguf"))
	if err != nil {
		t.Fatalf("файл не появился: %v", err)
	}
	if !bytes.Equal(onDisk, payload) {
		t.Errorf("содержимое на приёмнике отличается")
	}
	// Инвентарь пересканирован (иначе модель не видна до рестарта).
	found := false
	for _, m := range backend.ModelManager().ListModels() {
		if m.Filename == "tiny-model.gguf" {
			found = true
		}
	}
	if !found {
		t.Errorf("после импорта модель не видна в инвентаре: %v", backend.ModelManager().ListModels())
	}
	// Временный файл убран.
	if _, err := os.Stat(filepath.Join(dstDir, "tiny-model.gguf.importing")); !os.IsNotExist(err) {
		t.Errorf("остался .importing-файл: %v", err)
	}
}

// TestModelShare_ImportRejectsSizeMismatch — главная обязательная проверка:
// неполный поток не должен превращаться в «битую модель» на приёмнике.
func TestModelShare_ImportRejectsSizeMismatch(t *testing.T) {
	payload := bytes.Repeat([]byte("X"), 10_000)
	dstDir := newShareBackend(t, nil)

	rec := shareDo(http.MethodPost, "/api/models/import?filename=broken.gguf&size=999999", payload)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(dstDir, "broken.gguf")); !os.IsNotExist(err) {
		t.Errorf("неполный файл всё равно сохранён как модель")
	}
	if _, err := os.Stat(filepath.Join(dstDir, "broken.gguf.importing")); !os.IsNotExist(err) {
		t.Errorf("временный файл не удалён: %v", err)
	}
}

// TestModelShare_ImportChecksSha256 — если балансер передал хеш источника,
// побитый поток обязан быть отклонён.
func TestModelShare_ImportChecksSha256(t *testing.T) {
	dstDir := newShareBackend(t, nil)
	rec := shareDo(http.MethodPost,
		"/api/models/import?filename=x.gguf&sha256=0000000000000000000000000000000000000000000000000000000000000000",
		[]byte("data"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 (sha256 mismatch)", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "sha256_mismatch" {
		t.Errorf("код ошибки = %v, want sha256_mismatch", body["code"])
	}
	if _, err := os.Stat(filepath.Join(dstDir, "x.gguf")); !os.IsNotExist(err) {
		t.Errorf("файл с неверным хешем сохранён")
	}
}

// TestModelShare_ImportRefusesExistingWithoutOverwrite — 409 и подсказка про
// overwrite=1 (заменять чужой файл молча нельзя).
func TestModelShare_ImportRefusesExistingWithoutOverwrite(t *testing.T) {
	dstDir := newShareBackend(t, map[string][]byte{"dup.gguf": []byte("old")})
	rec := shareDo(http.MethodPost, "/api/models/import?filename=dup.gguf", []byte("new"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d, want 409", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "model_exists" || body["hint"] == "" {
		t.Errorf("нет кода/подсказки: %v", body)
	}
	if got, _ := os.ReadFile(filepath.Join(dstDir, "dup.gguf")); string(got) != "old" {
		t.Errorf("существующий файл изменён без overwrite: %q", got)
	}

	rec = shareDo(http.MethodPost, "/api/models/import?filename=dup.gguf&overwrite=1", []byte("new"))
	if rec.Code != http.StatusOK {
		t.Fatalf("overwrite=1: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got, _ := os.ReadFile(filepath.Join(dstDir, "dup.gguf")); string(got) != "new" {
		t.Errorf("файл не заменён при overwrite=1: %q", got)
	}
}

// TestModelShare_ImportValidatesFilename — импортировать можно только .gguf и
// только по basename (никаких подкаталогов).
func TestModelShare_ImportValidatesFilename(t *testing.T) {
	dstDir := newShareBackend(t, nil)
	for _, bad := range []string{"", "..", "model.bin", "sub/dir.gguf"} {
		rec := shareDo(http.MethodPost, "/api/models/import?filename="+bad, []byte("x"))
		if rec.Code == http.StatusOK {
			t.Errorf("filename=%q принят, хотя должен быть отклонён", bad)
		}
	}
	// Проверяем, что ничего лишнего не создано вне каталога моделей.
	entries, _ := os.ReadDir(dstDir)
	for _, e := range entries {
		if e.IsDir() {
			t.Errorf("создан каталог %q — импорт пишет только файлы", e.Name())
		}
	}
}

// TestModelShare_ExportUnknownModel — 404 с кодом и подсказкой (оператор должен
// видеть, какие имена вообще есть на этом воркере).
func TestModelShare_ExportUnknownModel(t *testing.T) {
	newShareBackend(t, map[string][]byte{"known.gguf": []byte("data")})
	rec := shareDo(http.MethodGet, "/api/models/export?name=unknown.gguf", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "model_not_found" {
		t.Errorf("код = %v", body["code"])
	}
	if hint, _ := body["hint"].(string); hint == "" {
		t.Errorf("нет подсказки")
	}
	if errText, _ := body["error"].(string); !bytes.Contains([]byte(errText), []byte("known.gguf")) {
		t.Errorf("в ошибке нет списка известных моделей: %q", errText)
	}
}

// TestPathInsideDir — защита от «..» в имени модели (иначе экспорт увёз бы на
// другой узел произвольный файл с диска воркера).
func TestPathInsideDir(t *testing.T) {
	dir := t.TempDir()
	if !pathInsideDir(dir, filepath.Join(dir, "model.gguf")) {
		t.Error("файл внутри каталога признан внешним")
	}
	if pathInsideDir(dir, filepath.Join(dir, "..", "secret.txt")) {
		t.Error("выход из каталога не отсечён")
	}
	if pathInsideDir("", filepath.Join(dir, "model.gguf")) || pathInsideDir(dir, "") {
		t.Error("пустые аргументы должны давать false")
	}
}
