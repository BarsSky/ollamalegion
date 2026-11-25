// handlers_model_share_bundle_r759_test.go — R759 (2026-11-25): перенос
// image-bundle ЦЕЛИКОМ — с файлами во вложенных каталогах.
//
// ЖИВАЯ ЖАЛОБА. «Не копируется полностью все файлы группы для модели image,
// когда делается перенос, так как теперь это бандл из нескольких файлов».
//
// ПРИЧИНА. Экспорт читал каталог bundle ОДНИМ уровнем: `os.ReadDir` +
// `if e.IsDir() { continue }`. Файлы во вложенных каталогах (типичная раскладка
// HF-репозитория: `split_files/…`, `text_encoders/…`) в tar не попадали вообще,
// а перенос отчитывался успехом — приёмник получал bundle без части файлов.
//
// ВТОРАЯ ПРИЧИНА (тихая). Приёмник умел сверять `bytes`/`files`, но балансер эти
// параметры не передавал, а поток, оборванный на границе файла, tar-ридер
// принимает как нормальный EOF. Поэтому неполный перенос не отличить от полного
// (сверку после импорта делает балансер, см. internal/api/model_share.go).
//
// ЧТО ПРОВЕРЯЕТСЯ ЗДЕСЬ:
//  1. HEAD считает файлы ВЛОЖЕННЫХ каталогов;
//  2. GET-поток содержит вложенные пути;
//  3. импорт воспроизводит раскладку каталогов;
//  4. служебные скрытые каталоги (.cache загрузчика) не переносятся — и это
//     определимо (счётчик их не включает);
//  5. враждебные имена в tar отклоняются (path traversal), а не пишутся вне bundle.
package main

import (
	"archive/tar"
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// writeBundleFileAt — положить файл по относительному пути внутри bundle.
func writeBundleFileAt(t *testing.T, app *App, bundle, rel string, data []byte) {
	t.Helper()
	path := filepath.Join(app.svc.Config.ModelsDir, bundle, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// TestR759_ExportCountsNestedFiles — вложенные файлы входят в состав экспорта.
func TestR759_ExportCountsNestedFiles(t *testing.T) {
	app, _ := newTestApp(t, map[string]types.ImageModelProfile{
		"bundle-n": {Family: "qwen_image"},
	})
	// profile.json от newTestApp + два отдельных файла + два вложенных.
	writeBundleFileAt(t, app, "bundle-n", "diffusion.gguf", bytes.Repeat([]byte{1}, 64<<10))
	writeBundleFileAt(t, app, "bundle-n", "vae.safetensors", bytes.Repeat([]byte{2}, 32<<10))
	writeBundleFileAt(t, app, "bundle-n", "split_files/text_encoders/t5.gguf", bytes.Repeat([]byte{3}, 16<<10))
	writeBundleFileAt(t, app, "bundle-n", "audio/encoder.safetensors", bytes.Repeat([]byte{4}, 8<<10))

	rec := shareRequest(t, app.setupRouter(), http.MethodHead, "/api/image/models/export?name=bundle-n", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD export: status=%d body=%s", rec.Code, rec.Body.String())
	}
	// profile.json + 4 наших файла = 5. Если бы считались только файлы верхнего
	// уровня, было бы 3 — ровно тот дефект, из-за которого терялись вложенные.
	if got := rec.Header().Get("X-Bundle-FileCount"); got != "5" {
		t.Errorf("X-Bundle-FileCount = %q, want 5 (profile.json + 2 верхнего уровня + 2 вложенных)", got)
	}
	wantBytes := int64(64<<10 + 32<<10 + 16<<10 + 8<<10)
	if got := rec.Header().Get("X-Bundle-Bytes"); got != "" {
		if n, _ := strconv.ParseInt(got, 10, 64); n <= wantBytes {
			t.Errorf("X-Bundle-Bytes = %q, want больше %d (плюс profile.json)", got, wantBytes)
		}
	}
}

// TestR759_NestedFilesSurviveRoundTrip — раскладка каталогов воспроизводится на приёмнике.
func TestR759_NestedFilesSurviveRoundTrip(t *testing.T) {
	src, _ := newTestApp(t, map[string]types.ImageModelProfile{"bundle-n": {Family: "qwen_image"}})
	writeBundleFileAt(t, src, "bundle-n", "diffusion.gguf", bytes.Repeat([]byte{0xAA}, 32<<10))
	writeBundleFileAt(t, src, "bundle-n", "split_files/text_encoders/t5.gguf", bytes.Repeat([]byte{0xBB}, 12<<10))
	writeBundleFileAt(t, src, "bundle-n", "text_encoders/clip_l.safetensors", bytes.Repeat([]byte{0xCC}, 4<<10))

	router := src.setupRouter()
	head := shareRequest(t, router, http.MethodHead, "/api/image/models/export?name=bundle-n", nil)
	files, bytesHeader := head.Header().Get("X-Bundle-FileCount"), head.Header().Get("X-Bundle-Bytes")
	if files != "4" {
		t.Fatalf("источник считает %q файлов, want 4 (profile.json + 3)", files)
	}

	rec := shareRequest(t, router, http.MethodGet, "/api/image/models/export?name=bundle-n", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET export: status=%d", rec.Code)
	}
	names := tarEntryNames(t, rec.Body.Bytes())
	joined := ""
	for _, n := range names {
		joined += n + " "
	}
	for _, want := range []string{"split_files/text_encoders/t5.gguf", "text_encoders/clip_l.safetensors"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("в tar нет вложенного файла %q (есть: %s) — это и есть «скопировалось не всё»", want, joined)
		}
	}

	dst, _ := newTestApp(t, nil)
	// Приёмник получает те же числа, что отдал балансер (bytes/files) — ровно
	// так теперь формирует URL shareEndpointURLs.
	path := "/api/image/models/import?name=bundle-n&bytes=" + bytesHeader + "&files=" + files
	importRec := shareRequest(t, dst.setupRouter(), http.MethodPost, path, rec.Body.Bytes())
	if importRec.Code != http.StatusOK {
		t.Fatalf("import: status=%d body=%s", importRec.Code, importRec.Body.String())
	}

	for _, rel := range []string{
		"diffusion.gguf",
		"split_files/text_encoders/t5.gguf",
		"text_encoders/clip_l.safetensors",
	} {
		got, err := os.ReadFile(filepath.Join(dst.svc.Config.ModelsDir, "bundle-n", filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("после импорта нет %q: %v (вложенные файлы потерялись при переносе)", rel, err)
		}
		if len(got) == 0 {
			t.Fatalf("файл %q пустой после импорта", rel)
		}
	}
}

// TestR759_HiddenServiceDirsSkipped — служебный .cache не считается контентом bundle.
func TestR759_HiddenServiceDirsSkipped(t *testing.T) {
	app, _ := newTestApp(t, map[string]types.ImageModelProfile{"bundle-h": {Family: "sd15"}})
	writeBundleFileAt(t, app, "bundle-h", "diffusion.gguf", bytes.Repeat([]byte{7}, 1024))
	writeBundleFileAt(t, app, "bundle-h", ".cache/huggingface/lock", []byte("lock"))

	rec := shareRequest(t, app.setupRouter(), http.MethodGet, "/api/image/models/export?name=bundle-h", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET export: status=%d", rec.Code)
	}
	for _, n := range tarEntryNames(t, rec.Body.Bytes()) {
		if n == ".cache" || filepath.ToSlash(n) == ".cache/huggingface/lock" {
			t.Fatalf("служебный каталог .cache попал в tar (%q): он не контент bundle", n)
		}
	}
}

// TestR759_ImportRejectsTraversal — враждебные имена отклоняются целиком.
func TestR759_ImportRejectsTraversal(t *testing.T) {
	cases := []string{"../evil.gguf", "/abs/evil.gguf", "a/../../evil.gguf", `C:\evil.gguf`}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			dst, _ := newTestApp(t, nil)
			payload := tarWithEntry(t, name, []byte("x"))
			rec := shareRequest(t, dst.setupRouter(), http.MethodPost,
				"/api/image/models/import?name=bundle-t", payload)
			if rec.Code == http.StatusOK {
				t.Fatalf("импорт принял враждебное имя %q (ответ %d)", name, rec.Code)
			}
			if _, err := os.Stat(filepath.Join(dst.svc.Config.ModelsDir, "evil.gguf")); err == nil {
				t.Fatalf("файл записан ВНЕ каталога bundle по имени %q", name)
			}
		})
	}
}

// tarWithEntry — минимальный tar с одной записью (для проверки приёмника).
func tarWithEntry(t *testing.T, name string, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatalf("tar header %q: %v", name, err)
	}
	if _, err := io.Copy(tw, bytes.NewReader(data)); err != nil {
		t.Fatalf("tar body %q: %v", name, err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	return buf.Bytes()
}
