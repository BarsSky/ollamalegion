// hf_bundle.go — R-Image (2026-09-27): bundle-загрузка с HuggingFace.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ФАЙЛ: одна диффузионная модель (stable-diffusion.cpp) —
// это НЕ один файл, а НАБОР: diffusion-модель (.gguf/.safetensors) + VAE
// (ae.safetensors) + text encoder'ы (clip_l.safetensors, t5xxl-*.gguf,
// Qwen3-4B-*.gguf). Одиночный StartDownload здесь неприменим по двум причинам:
//
//  1. Он регистрирует модель по факту появления ОДНОГО файла. Для bundle это
//     означает «модель есть», когда скачан только diffusion, а VAE/TE ещё нет —
//     sd-server падает при загрузке, а оператор видит «модель доступна».
//  2. У него ноль сведений о ролях файлов, поэтому общий прогресс («скачано
//     3.1 из 9.4 GB») и понятная ошибка («не скачался text encoder») невозможны.
//
// Поэтому здесь: N файлов качаются в <modelsDir>/<bundleID>/ (или в явный
// targetDir), прогресс — общий по bundle (сумма байт), а РЕГИСТРАЦИЯ —
// атомарная: манифест .ollamalegion-bundle.json появляется в каталоге ТОЛЬКО
// после успешной загрузки ВСЕХ файлов. Неполный bundle не считается моделью
// (см. IsBundleRegistered). Механика передачи (Range-resume, HF_TOKEN,
// HF_MIRROR, .download-темп, прогресс каждые 100 мс, история, cancel,
// orphan-файлы) — переиспользуется из hf_downloader.go через fetchFile.
//
// ВАЖНО ПРО СИНХРОННОСТЬ: StartBundleDownload синхронная и возвращается, когда
// все файлы скачаны/провалены. Так её проще использовать как «pull модели» и
// так детерминированно проверяется атомарность (негативный тест). Вызывающему,
// которому нужен HTTP-ответ сразу (воркер Phase 3), следует запустить её в
// отдельной горутине и опрашивать GetBundleProgress(bundleID).
package cppbackend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// ============================================================
// Константы и типы
// ============================================================

// BundleManifestName — имя файла-манифеста внутри каталога bundle.
//
// Семантика: манифест = «bundle зарегистрирован». Его НЕТ, пока не скачаны
// ВСЕ файлы; при повторной загрузке он удаляется в начале, чтобы старая
// регистрация не «подтверждала» наполовину перезаписанный каталог.
const BundleManifestName = ".ollamalegion-bundle.json"

// HFBundleManifest — содержимое манифеста зарегистрированного bundle.
type HFBundleManifest struct {
	BundleID   string   `json:"bundleId"`
	CreatedAt  string   `json:"createdAt"`
	Files      []string `json:"files"`      // basename'ы файлов в каталоге bundle
	TotalBytes int64    `json:"totalBytes"` // суммарный размер
}

// HFBundleFileStatus — статусы одного файла внутри bundle.
const (
	BundleFileStatusPending     = "pending"
	BundleFileStatusDownloading = "downloading"
	BundleFileStatusCompleted   = "completed"
	BundleFileStatusFailed      = "failed"
	BundleFileStatusCancelled   = "cancelled"
	BundleFileStatusInterrupted = "interrupted"
)

// HFBundleFileProgress — прогресс одного файла в bundle.
type HFBundleFileProgress struct {
	Role       string `json:"role,omitempty"` // diffusion|vae|clip_l|t5xxl|llm|...
	ModelID    string `json:"modelId"`
	Filename   string `json:"filename"`   // basename внутри каталога bundle
	SourcePath string `json:"sourcePath"` // путь в HF-репозитории
	FinalPath  string `json:"finalPath"`  // локальный путь после загрузки
	SizeBytes  int64  `json:"sizeBytes"`  // размер (ожидаемый или фактический)
	Downloaded int64  `json:"downloaded"` // байт на диске
	Resumable  bool   `json:"resumable"`  // остался .download для resume
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
}

// HFBundleProgress — агрегированный прогресс bundle (сумма по файлам).
type HFBundleProgress struct {
	BundleID     string                 `json:"bundleId"`
	Status       string                 `json:"status"` // downloading|completed|failed|cancelled|interrupted
	TargetDir    string                 `json:"targetDir"`
	TotalBytes   int64                  `json:"totalBytes"`
	Downloaded   int64                  `json:"downloaded"`
	ProgressPct  float64                `json:"progressPct"`
	SpeedBps     int64                  `json:"speedBps"` // скорость ТЕКУЩЕГО файла
	CurrentFile  string                 `json:"currentFile,omitempty"`
	Files        []HFBundleFileProgress `json:"files"`
	StartedAt    string                 `json:"startedAt"`
	CompletedAt  string                 `json:"completedAt,omitempty"`
	ErrorMessage string                 `json:"errorMessage,omitempty"`
	// Registered — манифест записан: bundle доступен как модель. true ТОЛЬКО
	// после успешной загрузки всех файлов.
	Registered bool `json:"registered"`
}

// bundleTask — внутреннее состояние активной bundle-загрузки.
type bundleTask struct {
	progress  HFBundleProgress
	cancel    context.CancelFunc
	completed chan struct{}
}

// ============================================================
// Загрузка bundle
// ============================================================

// StartBundleDownload скачивает набор файлов одной модели (bundle).
//
// files — состав bundle; порядок сохраняется (прогресс/ответ детерминированы).
// bundleID — имя каталога и ключ прогресса (например "z-image-turbo-q3k").
// targetDir — явный каталог; "" = <modelsDir>/<bundleID>.
//
// АТОМАРНОСТЬ: по завершении всех файлов в targetDir пишется манифест
// (BundleManifestName). Если хотя бы один файл не скачался — манифеста нет,
// IsBundleRegistered() == false, а StartBundleDownload возвращает ошибку со
// списком проблемных файлов. Частично скачанные файлы НЕ удаляются: их
// подхватит Range-resume при повторном вызове.
//
// Файлы, уже лежащие на диске с ожидаемым размером (req.SizeBytes), не
// перекачиваются — повторный pull после сбоя не тянет гигабайты заново.
func (d *HuggingFaceDownloader) StartBundleDownload(ctx context.Context, files []HFDownloadRequest, bundleID, targetDir string) (*HFBundleProgress, error) {
	if err := validateBundleRequest(files, bundleID); err != nil {
		return nil, err
	}
	if targetDir == "" {
		targetDir = filepath.Join(d.modelsDir, bundleID)
	}

	// Один активный bundle на bundleID: параллельная загрузка двух наборов в
	// ОДИН каталог даёт перемешанные .download-файлы и «плавающий» манифест.
	d.mu.Lock()
	if _, exists := d.activeBundles[bundleID]; exists {
		snapshot := d.activeBundles[bundleID].progress
		d.mu.Unlock()
		logger.Get().Infow("bundle download already in progress, returning current progress",
			"bundleId", bundleID, "status", snapshot.Status, "progressPct", snapshot.ProgressPct)
		return &snapshot, nil
	}

	bundleCtx, cancel := context.WithCancel(ctx)
	task := &bundleTask{
		progress: HFBundleProgress{
			BundleID:  bundleID,
			Status:    "downloading",
			TargetDir: targetDir,
			StartedAt: time.Now().UTC().Format(time.RFC3339),
			Files:     make([]HFBundleFileProgress, 0, len(files)),
		},
		cancel:    cancel,
		completed: make(chan struct{}),
	}
	for _, f := range files {
		base := filepath.Base(f.Filename)
		task.progress.Files = append(task.progress.Files, HFBundleFileProgress{
			Role:       f.Role,
			ModelID:    f.ModelID,
			Filename:   base,
			SourcePath: f.Filename,
			FinalPath:  filepath.Join(targetDir, base),
			SizeBytes:  f.SizeBytes,
			Status:     BundleFileStatusPending,
		})
	}
	recomputeBundleProgress(&task.progress)
	d.activeBundles[bundleID] = task
	d.mu.Unlock()

	defer func() {
		close(task.completed)
		d.mu.Lock()
		delete(d.activeBundles, bundleID)
		d.mu.Unlock()
	}()

	log := logger.Get()
	log.Infow("bundle download started",
		"bundleId", bundleID, "targetDir", targetDir, "files", len(files))

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		d.failBundle(task, fmt.Sprintf("create bundle dir: %v", err))
		return d.snapshotBundle(bundleID), fmt.Errorf("create bundle dir %s: %w", targetDir, err)
	}

	// Инвалидируем предыдущую регистрацию ДО начала работы: пока файлы
	// перезаписываются, каталог не должен выглядеть готовой моделью.
	if err := os.Remove(filepath.Join(targetDir, BundleManifestName)); err != nil && !os.IsNotExist(err) {
		log.Warnw("bundle: failed to remove stale manifest",
			"bundleId", bundleID, "error", err)
	}

	completedAll := true
	var failedFiles []string

	for i := range files {
		if err := bundleCtx.Err(); err != nil {
			d.setBundleFileStatus(task, i, BundleFileStatusCancelled, "cancelled by caller")
			completedAll = false
			failedFiles = append(failedFiles, task.progress.Files[i].Filename)
			break
		}
		if err := d.downloadBundleFile(bundleCtx, task, i, files[i]); err != nil {
			completedAll = false
			failedFiles = append(failedFiles, fmt.Sprintf("%s (%s)", task.progress.Files[i].Filename, err))
		}
	}

	if !completedAll {
		msg := "bundle not registered: failed files: " + strings.Join(failedFiles, "; ")
		status := "failed"
		if bundleCtx.Err() != nil {
			status = "cancelled"
		}
		d.mu.Lock()
		task.progress.Status = status
		task.progress.ErrorMessage = msg
		task.progress.Registered = false
		task.progress.CompletedAt = time.Now().UTC().Format(time.RFC3339)
		recomputeBundleProgress(&task.progress)
		d.bundleHistory = append(d.bundleHistory, task.progress)
		d.bundleHistory = trimBundleHistory(d.bundleHistory)
		d.mu.Unlock()

		// Диагностика «несуществующий репо даёт понятную ошибку, а не 401-маску»:
		// ошибка возвращается как есть по каждому файлу и попадает в ErrorMessage.
		log.Errorw("bundle download failed", "bundleId", bundleID, "error", msg)
		return d.snapshotBundle(bundleID), fmt.Errorf("%s", msg)
	}

	// Все файлы на месте → атомарная регистрация через манифест.
	total, err := writeBundleManifest(targetDir, bundleID, task.progress.Files)
	if err != nil {
		d.failBundle(task, fmt.Sprintf("write bundle manifest: %v", err))
		return d.snapshotBundle(bundleID), fmt.Errorf("write bundle manifest: %w", err)
	}

	d.mu.Lock()
	task.progress.Status = "completed"
	task.progress.Registered = true
	task.progress.TotalBytes = total
	task.progress.Downloaded = total
	task.progress.ProgressPct = 100.0
	task.progress.SpeedBps = 0
	task.progress.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	d.bundleHistory = append(d.bundleHistory, task.progress)
	d.bundleHistory = trimBundleHistory(d.bundleHistory)
	d.mu.Unlock()

	log.Infow("bundle download completed",
		"bundleId", bundleID, "targetDir", targetDir, "files", len(files), "totalBytes", total)

	// Тот же смысл, что и в одиночной загрузке: сообщить Backend'у, что каталог
	// моделей изменился (иначе инвентарь не обновится до рестарта).
	d.notifyDownloadComplete(bundleID)

	return d.snapshotBundle(bundleID), nil
}

// downloadBundleFile — скачивает ОДИН файл bundle и обновляет прогресс.
func (d *HuggingFaceDownloader) downloadBundleFile(ctx context.Context, task *bundleTask, idx int, req HFDownloadRequest) error {
	log := logger.Get()
	log.Infow("bundle file download", "bundleId", task.progress.BundleID,
		"file", task.progress.Files[idx].Filename, "modelId", req.ModelID, "role", req.Role)

	d.mu.Lock()
	fp := &task.progress.Files[idx]
	fp.Status = BundleFileStatusDownloading
	task.progress.CurrentFile = fp.Filename
	finalPath := fp.FinalPath
	expectedSize := fp.SizeBytes
	d.mu.Unlock()

	// Уже скачан (размер совпал) — не тянем заново. SizeBytes==0 означает
	// «размер неизвестен»: тогда доверяем факту наличия файла.
	if fi, err := os.Stat(finalPath); err == nil && !fi.IsDir() {
		if expectedSize == 0 || fi.Size() == expectedSize {
			d.mu.Lock()
			fp.Downloaded = fi.Size()
			fp.SizeBytes = fi.Size()
			fp.Status = BundleFileStatusCompleted
			fp.Resumable = false
			task.progress.CurrentFile = ""
			recomputeBundleProgress(&task.progress)
			d.mu.Unlock()
			log.Infow("bundle file already present, skipping", "file", fp.Filename, "sizeBytes", fi.Size())
			return nil
		}
		// Размер не совпал — файл битый/недокачанный: перекачиваем с нуля
		// (иначе sd-server молча получит обрезанный VAE/TE).
		log.Warnw("bundle file size mismatch, re-downloading",
			"file", fp.Filename, "have", fi.Size(), "want", expectedSize)
		if err := os.Remove(finalPath); err != nil {
			return fmt.Errorf("remove mismatched file: %w", err)
		}
	}

	// destPath передаём БЕЗ суффикса .download: fetchFile сам строит темповый
	// путь как destPath+".download" (единая конвенция с одиночной загрузкой).
	// Иначе получили бы foo.gguf.download.download.
	destPath := finalPath
	resumedFrom := int64(0)
	if fi, err := os.Stat(destPath + ".download"); err == nil && fi.Size() > 0 {
		resumedFrom = fi.Size()
	}

	cb := fileFetchCallbacks{
		OnTotal: func(total int64) {
			d.mu.Lock()
			if total > 0 {
				fp.SizeBytes = total
			}
			recomputeBundleProgress(&task.progress)
			d.mu.Unlock()
		},
		OnProgress: func(downloaded, _ int64, speed int64) {
			d.mu.Lock()
			fp.Downloaded = downloaded
			task.progress.SpeedBps = speed
			recomputeBundleProgress(&task.progress)
			d.mu.Unlock()
		},
	}

	res, err := d.fetchFile(ctx, req, destPath, finalPath, resumedFrom, cb)

	var interrupted *downloadInterruptedError
	d.mu.Lock()
	switch {
	case err == nil:
		fp.Downloaded = res.ResumeBase + res.Downloaded
		if fp.SizeBytes == 0 {
			fp.SizeBytes = fp.Downloaded
		}
		fp.Status = BundleFileStatusCompleted
		fp.Resumable = false
	case errors.Is(err, errDownloadCancelled):
		// Отмена: .download удалён fetchFile'ом — resume'ить нечего.
		fp.Status = BundleFileStatusCancelled
		fp.Error = "cancelled"
		fp.Resumable = false
	case errors.As(err, &interrupted):
		// Сетевой/IO сбой: .download оставлен, повторный pull продолжит.
		fp.Downloaded = res.ResumeBase + res.Downloaded
		fp.Status = BundleFileStatusInterrupted
		fp.Resumable = true
		fp.Error = err.Error()
	default:
		fp.Status = BundleFileStatusFailed
		fp.Error = err.Error()
	}
	task.progress.CurrentFile = ""
	recomputeBundleProgress(&task.progress)
	d.mu.Unlock()

	if err != nil {
		log.Warnw("bundle file failed",
			"bundleId", task.progress.BundleID, "file", fp.Filename,
			"status", fp.Status, "error", err.Error())
	}
	return err
}

// ============================================================
// Прогресс / управление bundle-загрузками
// ============================================================

// recomputeBundleProgress — пересчёт агрегата (сумма по файлам).
// Вызывается ТОЛЬКО под d.mu.
func recomputeBundleProgress(p *HFBundleProgress) {
	var total, done int64
	for i := range p.Files {
		total += p.Files[i].SizeBytes
		done += p.Files[i].Downloaded
	}
	p.TotalBytes = total
	p.Downloaded = done
	if total > 0 {
		p.ProgressPct = float64(done) / float64(total) * 100
		if p.ProgressPct > 100 {
			p.ProgressPct = 100
		}
	} else {
		p.ProgressPct = 0
	}
}

// GetBundleProgress возвращает снимок прогресса bundle (активного или из истории).
func (d *HuggingFaceDownloader) GetBundleProgress(bundleID string) (*HFBundleProgress, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if task, ok := d.activeBundles[bundleID]; ok {
		snapshot := task.progress
		return &snapshot, nil
	}
	// История — от новых к старым: последняя попытка важнее.
	for i := len(d.bundleHistory) - 1; i >= 0; i-- {
		if d.bundleHistory[i].BundleID == bundleID {
			snapshot := d.bundleHistory[i]
			return &snapshot, nil
		}
	}
	return nil, fmt.Errorf("no bundle download found for %s", bundleID)
}

// ListActiveBundles возвращает активные bundle-загрузки.
func (d *HuggingFaceDownloader) ListActiveBundles() []HFBundleProgress {
	d.mu.RLock()
	defer d.mu.RUnlock()

	result := make([]HFBundleProgress, 0, len(d.activeBundles))
	for _, task := range d.activeBundles {
		result = append(result, task.progress)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].BundleID < result[j].BundleID })
	return result
}

// ListBundleHistory возвращает историю завершённых bundle-загрузок.
func (d *HuggingFaceDownloader) ListBundleHistory() []HFBundleProgress {
	d.mu.RLock()
	defer d.mu.RUnlock()

	result := make([]HFBundleProgress, len(d.bundleHistory))
	copy(result, d.bundleHistory)
	return result
}

// CancelBundleDownload отменяет активную bundle-загрузку и ждёт её завершения.
//
// ВНИМАНИЕ: как и CancelDownload, блокируется до остановки. Не вызывать из той
// же горутины, что выполняет StartBundleDownload (там отмена — через ctx).
func (d *HuggingFaceDownloader) CancelBundleDownload(bundleID string) error {
	d.mu.RLock()
	task, exists := d.activeBundles[bundleID]
	d.mu.RUnlock()
	if !exists {
		return fmt.Errorf("no active bundle download for %s", bundleID)
	}
	task.cancel()
	<-task.completed
	return nil
}

// snapshotBundle — снимок прогресса (для возврата из StartBundleDownload).
func (d *HuggingFaceDownloader) snapshotBundle(bundleID string) *HFBundleProgress {
	if p, err := d.GetBundleProgress(bundleID); err == nil {
		return p
	}
	return &HFBundleProgress{BundleID: bundleID, Status: "failed"}
}

// failBundle — перевести bundle в failed c сообщением.
func (d *HuggingFaceDownloader) failBundle(task *bundleTask, msg string) {
	d.mu.Lock()
	task.progress.Status = "failed"
	task.progress.ErrorMessage = msg
	task.progress.Registered = false
	task.progress.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	d.bundleHistory = append(d.bundleHistory, task.progress)
	d.bundleHistory = trimBundleHistory(d.bundleHistory)
	d.mu.Unlock()
	logger.Get().Errorw("bundle download failed", "bundleId", task.progress.BundleID, "error", msg)
}

// setBundleFileStatus — пометить файл статусом (под локом).
func (d *HuggingFaceDownloader) setBundleFileStatus(task *bundleTask, idx int, status, errMsg string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if idx < 0 || idx >= len(task.progress.Files) {
		return
	}
	task.progress.Files[idx].Status = status
	task.progress.Files[idx].Error = errMsg
	recomputeBundleProgress(&task.progress)
}

// trimBundleHistory — держим последние 50 записей (как downloadHistory).
func trimBundleHistory(h []HFBundleProgress) []HFBundleProgress {
	const maxBundleHistory = 50
	if len(h) > maxBundleHistory {
		return h[len(h)-maxBundleHistory:]
	}
	return h
}

// ============================================================
// Регистрация bundle (манифест)
// ============================================================

// writeBundleManifest — атомарная запись манифеста: сначала .tmp, затем
// os.Rename. Без этого падение процесса в момент записи оставило бы
// ОБРЕЗАННЫЙ JSON, который читается как «bundle есть, но файлы неизвестны».
func writeBundleManifest(targetDir, bundleID string, files []HFBundleFileProgress) (int64, error) {
	manifest := HFBundleManifest{
		BundleID:  bundleID,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Files:     make([]string, 0, len(files)),
	}
	for _, f := range files {
		manifest.Files = append(manifest.Files, f.Filename)
		manifest.TotalBytes += f.SizeBytes
	}

	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("marshal manifest: %w", err)
	}
	tmp := filepath.Join(targetDir, BundleManifestName+".tmp")
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return 0, fmt.Errorf("write manifest temp: %w", err)
	}
	final := filepath.Join(targetDir, BundleManifestName)
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return 0, fmt.Errorf("rename manifest: %w", err)
	}
	return manifest.TotalBytes, nil
}

// LoadBundleManifest читает манифест каталога bundle.
// Возвращает (nil, os.ErrNotExist) если bundle НЕ зарегистрирован.
func LoadBundleManifest(targetDir string) (*HFBundleManifest, error) {
	data, err := os.ReadFile(filepath.Join(targetDir, BundleManifestName))
	if err != nil {
		return nil, err
	}
	var manifest HFBundleManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("parse bundle manifest in %s: %w", targetDir, err)
	}
	return &manifest, nil
}

// IsBundleRegistered — true, если каталог bundle зарегистрирован (манифест есть).
// Пакетная функция: работает и на балансере, и в воркере.
func IsBundleRegistered(targetDir string) bool {
	manifest, err := LoadBundleManifest(targetDir)
	return err == nil && manifest != nil && len(manifest.Files) > 0
}

// IsBundleRegistered — вариант от загрузчика: каталог <modelsDir>/<bundleID>.
func (d *HuggingFaceDownloader) IsBundleRegistered(bundleID string) bool {
	return IsBundleRegistered(filepath.Join(d.modelsDir, bundleID))
}

// BundleDir — каталог bundle внутри modelsDir загрузчика.
func (d *HuggingFaceDownloader) BundleDir(bundleID string) string {
	return filepath.Join(d.modelsDir, bundleID)
}

// ============================================================
// Валидация и вспомогательное
// ============================================================

// validateBundleRequest — проверки ДО начала загрузки (fail fast, понятная ошибка).
func validateBundleRequest(files []HFDownloadRequest, bundleID string) error {
	if strings.TrimSpace(bundleID) == "" {
		return fmt.Errorf("bundleId is required")
	}
	if err := validateBundleID(bundleID); err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("bundle %q: files is required (a diffusion bundle must contain at least one file)", bundleID)
	}

	seen := make(map[string]string, len(files))
	for i, f := range files {
		if strings.TrimSpace(f.ModelID) == "" {
			return fmt.Errorf("files[%d]: modelId is required", i)
		}
		if strings.TrimSpace(f.Filename) == "" {
			return fmt.Errorf("files[%d] (%s): filename is required", i, f.ModelID)
		}
		if !HasModelWeightExtension(f.Filename) {
			return fmt.Errorf("files[%d] (%s): unsupported file type %q (allowed: %s)",
				i, f.ModelID, f.Filename, strings.Join(ModelWeightExtensions, ", "))
		}
		base := filepath.Base(f.Filename)
		if base == "." || base == ".." || base == string(filepath.Separator) {
			return fmt.Errorf("files[%d]: invalid filename %q", i, f.Filename)
		}
		// Коллизия basename'ов: в каталоге bundle один файл затрёт другой, и
		// sd-server получит не тот VAE/TE. Лучше явная ошибка сейчас.
		if prev, dup := seen[base]; dup {
			return fmt.Errorf("bundle %q: two files resolve to the same name %q (%s and %s); rename or use separate bundles",
				bundleID, base, prev, f.ModelID)
		}
		seen[base] = f.ModelID
	}
	return nil
}

// validateBundleID — bundleID становится именем каталога: запрещаем всё, что
// может выйти за пределы modelsDir (path traversal) или сломать FS-вызовы.
func validateBundleID(bundleID string) error {
	if strings.ContainsAny(bundleID, `/\`) {
		return fmt.Errorf("bundleId %q must not contain path separators", bundleID)
	}
	if bundleID == "." || bundleID == ".." || strings.HasPrefix(bundleID, ".") {
		return fmt.Errorf("bundleId %q must not start with '.' or be a relative path element", bundleID)
	}
	for _, r := range bundleID {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("bundleId %q contains control characters", bundleID)
		}
	}
	return nil
}

// ListOrphanBundleFiles — «осиротевшие» .download-файлы внутри каталогов bundle.
//
// Зачем отдельно от ListOrphanDownloads: одиночные загрузки кладут темп в
// downloadsDir, а bundle — в СВОЙ подкаталог modelsDir (иначе два bundle с
// одинаковым именем файла (ae.safetensors) затирали бы друг друга). Без этого
// метода прерванный pull диффузии (6+ GB) не виден в UI и не чистится.
//
// Filename — путь ОТНОСИТЕЛЬНО modelsDir (например
// "z-image-turbo-q3k/z_image_turbo-Q3_K.gguf"), чтобы cleanup-endpoint понимал,
// какой именно файл удалять; Path — абсолютный путь.
func (d *HuggingFaceDownloader) ListOrphanBundleFiles() []OrphanDownloadFile {
	d.mu.RLock()
	activeBundles := make(map[string]bool, len(d.activeBundles))
	for id := range d.activeBundles {
		activeBundles[id] = true
	}
	d.mu.RUnlock()

	entries, err := os.ReadDir(d.modelsDir)
	if err != nil {
		return []OrphanDownloadFile{}
	}

	var orphans []OrphanDownloadFile
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		bundleID := e.Name()
		if activeBundles[bundleID] {
			continue // идёт активная загрузка — это не orphan
		}
		bundleDir := filepath.Join(d.modelsDir, bundleID)
		files, err := os.ReadDir(bundleDir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".download") {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			rel := bundleID + "/" + f.Name()
			orphans = append(orphans, OrphanDownloadFile{
				Path:     filepath.Join(bundleDir, f.Name()),
				Filename: rel,
				Size:     info.Size(),
				Modified: info.ModTime().Unix(),
			})
		}
	}
	sort.Slice(orphans, func(i, j int) bool { return orphans[i].Filename < orphans[j].Filename })
	return orphans
}
