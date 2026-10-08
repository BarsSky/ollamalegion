// handlers_model_share_test.go — R89 (2026-10-08): перенос image-bundle между
// воркерами (export → tar → import).
//
// ЖИВОЙ СЦЕНАРИЙ: на второй машине нет ни одного bundle'а, а тянуть 14 ГБ из
// HuggingFace на неё — это интернет, токен и время. Здесь проверяем честное
// копирование: источник отдаёт каталог как есть, приёмник распаковывает и
// перерегистрирует модель, а неполный/подделанный поток отклоняется.
package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// writeBundleFile — положить файл в каталог bundle тестового воркера.
func writeBundleFile(t *testing.T, app *App, bundle, name string, size int, fill byte) []byte {
	t.Helper()
	data := bytes.Repeat([]byte{fill}, size)
	path := filepath.Join(app.svc.Config.ModelsDir, bundle, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return data
}

// readBundleFile — прочитать файл из каталога bundle.
func readBundleFile(t *testing.T, app *App, bundle, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(app.svc.Config.ModelsDir, bundle, name))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return data
}

func shareRequest(t *testing.T, h http.Handler, method, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestShareBundle_HeadReportsSizeAndFileCount — балансер спрашивает размер, не
// выкачивая гигабайты: HEAD обязан отдать X-Bundle-Bytes и X-Bundle-FileCount.
func TestShareBundle_HeadReportsSizeAndFileCount(t *testing.T) {
	app, _ := newTestApp(t, map[string]types.ImageModelProfile{
		"bundle-a": {Family: "sd15"},
	})
	diff := writeBundleFile(t, app, "bundle-a", "diffusion.gguf", 300<<10, 0xAB)
	vae := writeBundleFile(t, app, "bundle-a", "vae.safetensors", 64<<10, 0xCD)
	profile := readBundleFile(t, app, "bundle-a", "profile.json")

	rec := shareRequest(t, app.setupRouter(), http.MethodHead, "/api/image/models/export?name=bundle-a", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD export: status=%d body=%s", rec.Code, rec.Body.String())
	}
	wantBytes := int64(len(diff) + len(vae) + len(profile))
	if got := rec.Header().Get("X-Bundle-Bytes"); got != strconv.FormatInt(wantBytes, 10) {
		t.Errorf("X-Bundle-Bytes = %q, want %d (сумма файлов без tar-заголовков)", got, wantBytes)
	}
	if got := rec.Header().Get("X-Bundle-FileCount"); got != "3" {
		t.Errorf("X-Bundle-FileCount = %q, want 3", got)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD вернул %d байт тела — размер спрашивают именно чтобы НЕ качать", rec.Body.Len())
	}
}

// TestShareBundle_ExportImportRoundTrip — главный сценарий: каталог уехал целиком
// и распаковался в bundle на приёмнике, реестр увидел модель без рестарта.
func TestShareBundle_ExportImportRoundTrip(t *testing.T) {
	src, _ := newTestApp(t, map[string]types.ImageModelProfile{
		"bundle-a": {Family: "sd15"},
	})
	diff := writeBundleFile(t, src, "bundle-a", "diffusion.gguf", 256<<10, 0x11)
	vae := writeBundleFile(t, src, "bundle-a", "vae.safetensors", 32<<10, 0x22)
	srcRouter := src.setupRouter()

	head := shareRequest(t, srcRouter, http.MethodHead, "/api/image/models/export?name=bundle-a", nil)
	total, files := head.Header().Get("X-Bundle-Bytes"), head.Header().Get("X-Bundle-FileCount")

	// GET отдаёт tar: проверим и сам поток (его читает балансер как тело POST).
	rec := shareRequest(t, srcRouter, http.MethodGet, "/api/image/models/export?name=bundle-a", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET export: status=%d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-tar" {
		t.Errorf("Content-Type = %q, want application/x-tar", ct)
	}
	tarBytes := rec.Body.Bytes()
	if names := tarEntryNames(t, tarBytes); len(names) < 2 {
		t.Fatalf("в tar мало файлов: %v", names)
	}

	// Приёмник: чистый каталог моделей.
	dst, _ := newTestApp(t, nil)
	path := "/api/image/models/import?name=bundle-a&bytes=" + total + "&files=" + files
	importRec := shareRequest(t, dst.setupRouter(), http.MethodPost, path, tarBytes)
	if importRec.Code != http.StatusOK {
		t.Fatalf("import: status=%d body=%s", importRec.Code, importRec.Body.String())
	}

	// Файлы совпадают побайтово.
	if got := readBundleFile(t, dst, "bundle-a", "diffusion.gguf"); !bytes.Equal(got, diff) {
		t.Errorf("diffusion.gguf на приёмнике отличается (%d байт vs %d)", len(got), len(diff))
	}
	if got := readBundleFile(t, dst, "bundle-a", "vae.safetensors"); !bytes.Equal(got, vae) {
		t.Errorf("vae.safetensors на приёмнике отличается (%d байт vs %d)", len(got), len(vae))
	}
	// Реестр перечитан: модель видна воркеру без рестарта.
	if _, ok := dst.svc.Registry.Profile("bundle-a"); !ok {
		t.Errorf("после импорта реестр не знает bundle-a: %v", dst.svc.Registry.Names())
	}
	// Временный каталог убран (иначе следующий импорт споткнётся).
	if _, err := os.Stat(filepath.Join(dst.svc.Config.ModelsDir, ".importing-bundle-a")); !os.IsNotExist(err) {
		t.Errorf("остался временный каталог .importing-bundle-a: %v", err)
	}
}

// TestShareBundle_ImportRejectsIncompleteStream — обрыв потока не должен
// превращаться в «полу-bundle» на приёмнике.
func TestShareBundle_ImportRejectsIncompleteStream(t *testing.T) {
	src, _ := newTestApp(t, map[string]types.ImageModelProfile{"bundle-a": {Family: "sd15"}})
	writeBundleFile(t, src, "bundle-a", "diffusion.gguf", 128<<10, 0x33)
	full := shareRequest(t, src.setupRouter(), http.MethodGet, "/api/image/models/export?name=bundle-a", nil).Body.Bytes()

	dst, _ := newTestApp(t, nil)
	truncated := full[:len(full)/2]
	rec := shareRequest(t, dst.setupRouter(), http.MethodPost,
		"/api/image/models/import?name=bundle-a", truncated)
	if rec.Code == http.StatusOK {
		t.Fatalf("обрезанный tar принят как успешный импорт")
	}
	if _, err := os.Stat(filepath.Join(dst.svc.Config.ModelsDir, "bundle-a")); !os.IsNotExist(err) {
		t.Errorf("на приёмнике остался каталог из неполного потока: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst.svc.Config.ModelsDir, ".importing-bundle-a")); !os.IsNotExist(err) {
		t.Errorf("временный каталог не убран: %v", err)
	}
}

// TestShareBundle_ImportChecksDeclaredPayload — несовпадение суммы байтов
// (единственная обязательная проверка целостности) отклоняет импорт.
func TestShareBundle_ImportChecksDeclaredPayload(t *testing.T) {
	src, _ := newTestApp(t, map[string]types.ImageModelProfile{"bundle-a": {Family: "sd15"}})
	writeBundleFile(t, src, "bundle-a", "diffusion.gguf", 64<<10, 0x44)
	full := shareRequest(t, src.setupRouter(), http.MethodGet, "/api/image/models/export?name=bundle-a", nil).Body.Bytes()

	dst, _ := newTestApp(t, nil)
	rec := shareRequest(t, dst.setupRouter(), http.MethodPost,
		"/api/image/models/import?name=bundle-a&bytes=1&files=1", full)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 (неполный перенос)", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	errText := strings.ToLower(fmt.Sprint(body["error"]))
	if !strings.Contains(errText, "неполн") && !strings.Contains(errText, "перенос") {
		t.Errorf("в ошибке нет объяснения про неполный перенос: %v", body["error"])
	}
	if _, err := os.Stat(filepath.Join(dst.svc.Config.ModelsDir, "bundle-a")); !os.IsNotExist(err) {
		t.Errorf("bundle с неверным размером всё равно распакован")
	}
}

// TestShareBundle_ImportRejectsPathTraversal — чужой tar не имеет права писать
// вне каталога моделей.
func TestShareBundle_ImportRejectsPathTraversal(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	payload := []byte("pwned")
	if err := tw.WriteHeader(&tar.Header{Name: "../evil.txt", Mode: 0o644, Size: int64(len(payload)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}

	dst, _ := newTestApp(t, nil)
	rec := shareRequest(t, dst.setupRouter(), http.MethodPost,
		"/api/image/models/import?name=bundle-evil", buf.Bytes())
	if rec.Code == http.StatusOK {
		t.Fatalf("tar с «../» принят")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dst.svc.Config.ModelsDir), "evil.txt")); err == nil {
		t.Fatalf("файл записан ВНЕ каталога моделей")
	}
}

// TestShareBundle_ImportRefusesExistingWithoutOverwrite — 409, а не молчаливая
// замена уже лежащего bundle (у оператора может быть другой квант/версия).
func TestShareBundle_ImportRefusesExistingWithoutOverwrite(t *testing.T) {
	src, _ := newTestApp(t, map[string]types.ImageModelProfile{"bundle-a": {Family: "sd15"}})
	writeBundleFile(t, src, "bundle-a", "diffusion.gguf", 16<<10, 0x55)
	full := shareRequest(t, src.setupRouter(), http.MethodGet, "/api/image/models/export?name=bundle-a", nil).Body.Bytes()

	dst, _ := newTestApp(t, map[string]types.ImageModelProfile{"bundle-a": {Family: "sd15"}})
	rec := shareRequest(t, dst.setupRouter(), http.MethodPost, "/api/image/models/import?name=bundle-a", full)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d, want 409 (bundle уже есть)", rec.Code)
	}
	// С overwrite=1 — принимаем.
	rec = shareRequest(t, dst.setupRouter(), http.MethodPost, "/api/image/models/import?name=bundle-a&overwrite=1", full)
	if rec.Code != http.StatusOK {
		t.Fatalf("overwrite=1: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestShareBundle_ExportUnknownBundle — 404 с кодом и подсказкой.
func TestShareBundle_ExportUnknownBundle(t *testing.T) {
	app, _ := newTestApp(t, map[string]types.ImageModelProfile{"bundle-a": {Family: "sd15"}})
	rec := shareRequest(t, app.setupRouter(), http.MethodGet, "/api/image/models/export?name=нет-такого", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "bundle_not_found" || fmt.Sprint(body["hint"]) == "" {
		t.Errorf("нет кода/подсказки: %v", body)
	}
}

// tarEntryNames — имена записей tar (для проверки содержимого потока).
func tarEntryNames(t *testing.T, data []byte) []string {
	t.Helper()
	var names []string
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		names = append(names, hdr.Name)
	}
	return names
}
