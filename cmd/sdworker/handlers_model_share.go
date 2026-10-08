// handlers_model_share.go — R89 (2026-10-08): перенос image-bundle МЕЖДУ воркерами.
//
// ЗАЧЕМ. Bundle — это КАТАЛОГ с несколькими файлами (diffusion + vae +
// text encoders + profile.json), суммарно 13-15 ГБ. Второй машине его до сих пор
// можно было получить только повторной загрузкой из HuggingFace: интернет,
// HF-токен, время и риск другой ревизии. Теперь источник отдаёт каталог как tar,
// приёмник распаковывает его и перерегистрирует модель — балансер стримит поток
// между ними (internal/balancer/model_share.go).
//
// КОНТРАКТ (симметричен паре в cppworker, см. cmd/cppworker/handlers_model_share.go):
//
//	GET  /api/image/models/export?name=<bundle>
//	     поток tar; заголовки X-Bundle-Name, X-Bundle-Bytes (сумма ПАЛИТРЫ файлов),
//	     X-Bundle-FileCount. Content-Length НЕ ставим: tar-поток формируется на
//	     ходу, а врать про длину нельзя (см. import: целостность проверяется по
//	     фактическим байтам файлов, а не по байтам tar).
//	POST /api/image/models/import?name=<bundle>[&bytes=<payload>][&files=<n>][&overwrite=1]
//	     тело — tar; распаковываем во временный каталог и переименовываем.
package main

import (
	"archive/tar"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"ollama-loadbalancer/pkg/logger"
)

// bundleEntry — файл bundle'а: имя, размер и путь на диске.
type bundleEntry struct {
	Name string
	Path string
	Size int64
}

// resolveExportableBundle — каталог bundle'а по имени модели.
//
// Имя приходит извне, поэтому путь строится ТОЛЬКО от каталога моделей и
// проверяется на выход за его пределы (никаких «..»).
func (a *App) resolveExportableBundle(name string) (string, []bundleEntry, int64, error) {
	modelsDir := strings.TrimSpace(a.svc.Config.ModelsDir)
	if modelsDir == "" {
		return "", nil, 0, fmt.Errorf("models directory is not configured")
	}
	clean := filepath.Base(strings.TrimSpace(name))
	if clean == "" || clean == "." || clean == string(filepath.Separator) {
		return "", nil, 0, fmt.Errorf("name is required")
	}
	dir := filepath.Join(modelsDir, clean)
	if !pathInsideDir(modelsDir, dir) {
		return "", nil, 0, fmt.Errorf("bundle name escapes the models directory")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil, 0, fmt.Errorf("bundle %q not found on this worker (known: %s)",
				clean, strings.Join(a.svc.Registry.Names(), ", "))
		}
		return "", nil, 0, fmt.Errorf("cannot read bundle %q: %v", clean, err)
	}
	var files []bundleEntry
	var total int64
	for _, e := range entries {
		if e.IsDir() {
			// Вложенные каталоги в bundle не поддерживаются: движку они не нужны,
			// а tar с ними усложнил бы проверку путей на приёмнике.
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, bundleEntry{
			Name: e.Name(),
			Path: filepath.Join(dir, e.Name()),
			Size: info.Size(),
		})
		total += info.Size()
	}
	if len(files) == 0 {
		return "", nil, 0, fmt.Errorf("bundle %q is empty (нет файлов для переноса)", clean)
	}
	return clean, files, total, nil
}

// handleExportBundle — GET/HEAD /api/image/models/export (tar-поток каталога bundle).
//
// HEAD поддержан специально для балансера: перед копированием он спрашивает
// размер и число файлов, не выкачивая 14 ГБ (probeShareSource в internal/api).
func (a *App) handleExportBundle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "use GET or HEAD")
		return
	}
	name, files, total, err := a.resolveExportableBundle(r.URL.Query().Get("name"))
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "bundle_not_found", err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("X-Bundle-Name", name)
	w.Header().Set("X-Bundle-Bytes", strconv.FormatInt(total, 10))
	w.Header().Set("X-Bundle-FileCount", strconv.Itoa(len(files)))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}

	tw := tar.NewWriter(w)
	for _, f := range files {
		src, err := os.Open(f.Path)
		if err != nil {
			// Заголовки уже отправлены — прерываем поток: приёмник увидит
			// неполный tar и отклонит импорт (проверка по bytes/files).
			logger.Get().Errorw("bundle export: cannot open file",
				"bundle", name, "file", f.Name, "error", err)
			return
		}
		hdr := &tar.Header{
			Name:     f.Name,
			Mode:     0o644,
			Size:     f.Size,
			Typeflag: tar.TypeReg,
			// USTAR: имена в bundle короткие, а предсказуемый заголовок без PAX
			// держит поток простым (и позволяет читать его любым tar-клиентом).
			Format: tar.FormatUSTAR,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			_ = src.Close()
			logger.Get().Errorw("bundle export: write header failed",
				"bundle", name, "file", f.Name, "error", err)
			return
		}
		if _, err := io.Copy(tw, src); err != nil {
			_ = src.Close()
			logger.Get().Errorw("bundle export: copy failed",
				"bundle", name, "file", f.Name, "error", err)
			return
		}
		_ = src.Close()
	}
	if err := tw.Close(); err != nil {
		logger.Get().Errorw("bundle export: close tar failed", "bundle", name, "error", err)
	}
}

// handleImportBundle — POST /api/image/models/import (распаковка tar в каталог bundle).
func (a *App) handleImportBundle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	modelsDir := strings.TrimSpace(a.svc.Config.ModelsDir)
	if modelsDir == "" {
		writeError(w, http.StatusServiceUnavailable, "models directory is not configured")
		return
	}
	q := r.URL.Query()
	rawName := strings.TrimSpace(q.Get("name"))
	name := filepath.Base(rawName)
	if name == "" || name == "." || name == string(filepath.Separator) {
		writeJSONError(w, http.StatusBadRequest, "bundle_name_required", "name is required")
		return
	}
	// Имя bundle — всегда один сегмент каталога. Путь отклоняем явно: молча
	// превратить «sub/bundle» в «bundle» значило бы положить модель не под тем
	// именем, которого ждёт источник.
	if rawName != name || strings.ContainsAny(rawName, `/\`) {
		writeJSONError(w, http.StatusBadRequest, "invalid_bundle_name",
			"name must be a plain directory name (no path separators)")
		return
	}
	finalDir := filepath.Join(modelsDir, name)
	if !pathInsideDir(modelsDir, finalDir) {
		writeJSONError(w, http.StatusBadRequest, "invalid_bundle_name",
			"bundle name escapes the models directory")
		return
	}
	overwrite := q.Get("overwrite") == "1" || strings.EqualFold(q.Get("overwrite"), "true")
	if _, err := os.Stat(finalDir); err == nil && !overwrite {
		writeJSONError(w, http.StatusConflict, "bundle_exists",
			fmt.Sprintf("bundle %q already exists on this worker", name))
		return
	}
	if err := os.MkdirAll(modelsDir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot create models dir: "+err.Error())
		return
	}

	tmpDir := filepath.Join(modelsDir, ".importing-"+name)
	if err := os.RemoveAll(tmpDir); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot clean temp dir: "+err.Error())
		return
	}
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot create temp dir: "+err.Error())
		return
	}

	written, fileCount, err := extractBundleTar(r.Body, tmpDir)
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		writeJSONError(w, http.StatusBadRequest, "import_failed",
			fmt.Sprintf("распаковка прервана: %v (получено %d байт, файлов %d)", err, written, fileCount))
		return
	}

	if want := strings.TrimSpace(q.Get("bytes")); want != "" {
		if expected, perr := strconv.ParseInt(want, 10, 64); perr == nil && expected != written {
			_ = os.RemoveAll(tmpDir)
			writeJSONError(w, http.StatusBadRequest, "size_mismatch",
				fmt.Sprintf("перенос неполный: получено %d байт полезной нагрузки, ожидалось %d", written, expected))
			return
		}
	}
	if want := strings.TrimSpace(q.Get("files")); want != "" {
		if expected, perr := strconv.Atoi(want); perr == nil && expected != fileCount {
			_ = os.RemoveAll(tmpDir)
			writeJSONError(w, http.StatusBadRequest, "filecount_mismatch",
				fmt.Sprintf("получено файлов %d, ожидалось %d", fileCount, expected))
			return
		}
	}

	if overwrite {
		if err := os.RemoveAll(finalDir); err != nil {
			_ = os.RemoveAll(tmpDir)
			writeError(w, http.StatusInternalServerError, "cannot replace existing bundle: "+err.Error())
			return
		}
	}
	if err := os.Rename(tmpDir, finalDir); err != nil {
		_ = os.RemoveAll(tmpDir)
		writeError(w, http.StatusInternalServerError, "cannot move bundle into place: "+err.Error())
		return
	}

	// Без релоада импортированный bundle не появится в /api/image/models до
	// рестарта воркера: реестр читает каталоги профилей один раз при старте.
	if err := a.svc.ReloadRegistry(); err != nil {
		logger.Get().Warnw("bundle import: registry reload failed (files are on disk)",
			"bundle", name, "error", err)
	}
	logger.Get().Infow("image bundle imported from another backend",
		"bundle", name, "bytes", written, "files", fileCount)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"name":        name,
		"path":        finalDir,
		"bytes":       written,
		"fileCount":   fileCount,
		"registered":  true,
		"modelsCount": len(a.svc.Registry.Names()),
	})
}

// extractBundleTar — распаковать tar в dir, вернув сумму байтов файлов и их число.
//
// Принимаем ТОЛЬКО обычные файлы верхнего уровня: symlink/hardlink/устройства и
// любой путь с разделителем отклоняем — иначе чужой tar мог бы писать вне каталога
// (path traversal). Пустые каталоги игнорируем.
func extractBundleTar(r io.Reader, dir string) (int64, int, error) {
	if dir == "" {
		return 0, 0, fmt.Errorf("empty destination")
	}
	tr := tar.NewReader(r)
	var written int64
	var files int
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return written, files, err
		}
		name := hdr.Name
		if name == "" {
			continue
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			return written, files, fmt.Errorf("unsupported tar entry %q (type %c)", name, hdr.Typeflag)
		}
		if strings.ContainsAny(name, `/\`) || name == ".." || name == "." {
			return written, files, fmt.Errorf("unsafe tar entry %q", name)
		}
		clean := filepath.Base(name)
		if clean != name {
			return written, files, fmt.Errorf("unsafe tar entry %q", name)
		}
		dst := filepath.Join(dir, clean)
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			return written, files, err
		}
		n, err := io.Copy(out, tr)
		closeErr := out.Close()
		written += n
		if err != nil {
			return written, files, err
		}
		if closeErr != nil {
			return written, files, closeErr
		}
		files++
	}
	if files == 0 {
		return written, files, fmt.Errorf("tar содержит 0 файлов")
	}
	return written, files, nil
}

// pathInsideDir — true, если path лежит внутри dir (защита от «..» в имени).
func pathInsideDir(dir, path string) bool {
	if dir == "" || path == "" {
		return false
	}
	absDir, err1 := filepath.Abs(dir)
	absPath, err2 := filepath.Abs(path)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(absDir, absPath)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
