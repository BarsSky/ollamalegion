// Package cppbackend — HuggingFace интеграция для загрузки GGUF моделей
//
// HuggingFaceDownloader отвечает за:
// - Поиск GGUF моделей на HuggingFace Hub
// - Загрузку .gguf файлов с HuggingFace
// - Поддержку HF_TOKEN для аутентификации
// - Поддержку HF_MIRROR для зеркал (hf-mirror.com)
// - Отслеживание прогресса загрузки
// - Управление очередью загрузок
package cppbackend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// ============================================================
// Константы
// ============================================================

const (
	// HFHubEndpoint — базовый endpoint HuggingFace Hub API
	HFHubEndpoint = "https://huggingface.co"

	// HFHubAPIPrefix — префикс для API запросов
	HFHubAPIPrefix = "/api"

	// DefaultDownloadTimeout — таймаут загрузки по умолчанию
	DefaultDownloadTimeout = 30 * time.Minute

	// MaxConcurrentDownloads — максимальное количество одновременных загрузок
	MaxConcurrentDownloads = 3

	// ChunkSize — размер чанка для отслеживания прогресса (1MB)
	ChunkSize = 1 * 1024 * 1024
)

// ============================================================
// Типы
// ============================================================

// HFModelRepo — информация о репозитории модели на HuggingFace
type HFModelRepo struct {
	ID          string `json:"id"`          // e.g. "TheBloke/Llama-2-7B-GGUF"
	Name        string `json:"name"`        // e.g. "Llama-2-7B-GGUF"
	Author      string `json:"author"`      // e.g. "TheBloke"
	LastUpdated string `json:"lastUpdated"` // ISO 8601
	Downloads   int    `json:"downloads"`
	Likes       int    `json:"likes"`
	PipelineTag string `json:"pipelineTag"` // e.g. "text-generation"
}

// HFFileInfo — информация о GGUF файле в репозитории
type HFFileInfo struct {
	Path         string `json:"path"`         // путь в репозитории
	SizeBytes    int64  `json:"sizeBytes"`    // размер файла
	IsGGUF       bool   `json:"isGGUF"`       // .gguf файл?
	Quantization string `json:"quantization"` // извлечённый тип квантизации
}

// HFDownloadProgress — прогресс загрузки
type HFDownloadProgress struct {
	ModelID      string  `json:"modelId"`      // e.g. "TheBloke/Llama-2-7B-GGUF"
	Filename     string  `json:"filename"`     // имя файла
	TotalBytes   int64   `json:"totalBytes"`   // общий размер
	Downloaded   int64   `json:"downloaded"`   // загружено байт
	ProgressPct  float64 `json:"progressPct"`  // процент
	SpeedBps     int64   `json:"speedBps"`     // скорость байт/сек
	Status       string  `json:"status"`       // "downloading", "completed", "failed", "cancelled"
	ErrorMessage string  `json:"errorMessage,omitempty"`
	StartedAt    string  `json:"startedAt"`    // ISO 8601
	CompletedAt  string  `json:"completedAt,omitempty"`
}

// HFDownloadRequest — запрос на загрузку модели
type HFDownloadRequest struct {
	ModelID      string `json:"modelId"`      // e.g. "TheBloke/Llama-2-7B-GGUF"
	Filename     string `json:"filename"`     // конкретный файл (опционально)
	Revision     string `json:"revision"`     // ветка/ревизия (опционально, default: "main")
	Quantization string `json:"quantization"` // фильтр по квантизации (опционально)
	AutoDetect   bool   `json:"autoDetect"`   // авто-выбор файла?
}

// ============================================================
// HuggingFaceDownloader
// ============================================================

// HuggingFaceDownloader — загрузчик моделей с HuggingFace
type HuggingFaceDownloader struct {
	mu              sync.RWMutex
	token           string
	mirror          string
	downloadsDir    string
	modelsDir       string
	httpClient      *http.Client
	activeDownloads map[string]*downloadTask
	downloadHistory []HFDownloadProgress
	maxConcurrent   int
	semaphore       chan struct{}
}

// downloadTask — внутренняя задача загрузки
type downloadTask struct {
	request   HFDownloadRequest
	progress  HFDownloadProgress
	cancel    context.CancelFunc
	completed chan struct{}
}

// NewHuggingFaceDownloader создаёт новый загрузчик
func NewHuggingFaceDownloader(token, mirror, downloadsDir, modelsDir string) *HuggingFaceDownloader {
	if downloadsDir == "" {
		downloadsDir = "./downloads"
	}

	return &HuggingFaceDownloader{
		token:           token,
		mirror:          mirror,
		downloadsDir:    downloadsDir,
		modelsDir:       modelsDir,
		httpClient: &http.Client{
			Timeout: DefaultDownloadTimeout,
			Transport: &http.Transport{
				MaxIdleConns:        10,
				IdleConnTimeout:     30 * time.Second,
				DisableCompression:  false,
			},
		},
		activeDownloads: make(map[string]*downloadTask),
		downloadHistory: make([]HFDownloadProgress, 0),
		maxConcurrent:   MaxConcurrentDownloads,
		semaphore:       make(chan struct{}, MaxConcurrentDownloads),
	}
}

// ============================================================
// HuggingFace Hub API
// ============================================================

// getBaseURL возвращает базовый URL для запросов (с учётом mirror)
func (d *HuggingFaceDownloader) getBaseURL() string {
	if d.mirror != "" {
		return strings.TrimRight(d.mirror, "/")
	}
	return HFHubEndpoint
}

// getAPIURL возвращает URL для API запроса с учётом mirror
func (d *HuggingFaceDownloader) getAPIURL(path string) string {
	base := d.getBaseURL()
	if d.mirror != "" {
		// Для зеркал используем прямой путь, не через /api
		return base + path
	}
	return base + HFHubAPIPrefix + path
}

// getResolveURL возвращает URL для resolve (скачивание файла)
func (d *HuggingFaceDownloader) getResolveURL(modelID, filename, revision string) string {
	base := d.getBaseURL()
	if revision == "" {
		revision = "main"
	}
	return fmt.Sprintf("%s/%s/resolve/%s/%s", base, modelID, revision, filename)
}

// newRequest создаёт HTTP запрос с заголовками
func (d *HuggingFaceDownloader) newRequest(method, urlStr string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, urlStr, body)
	if err != nil {
		return nil, err
	}

	req.Header.Set("User-Agent", "ollamalegion-cppworker/1.0")

	if d.token != "" {
		req.Header.Set("Authorization", "Bearer "+d.token)
	}

	return req, nil
}

// ============================================================
// Поиск моделей на HuggingFace Hub
// ============================================================

// SetToken устанавливает HF токен для аутентифицированных запросов.
// Используется для доступа к gated моделям (например, Llama-3).
func (d *HuggingFaceDownloader) SetToken(token string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.token = token
}

// SearchModels выполняет поиск моделей на HuggingFace Hub
// Возвращает список репозиториев, содержащих GGUF файлы
func (d *HuggingFaceDownloader) SearchModels(ctx context.Context, query string, limit int) ([]HFModelRepo, error) {
	log := logger.Get()
	log.Infow("searching HuggingFace models", "query", query, "limit", limit)

	if limit <= 0 || limit > 100 {
		limit = 20
	}

	// HuggingFace API для поиска: GET /api/models?search=...&task=text-generation&sort=downloads
	apiURL := d.getAPIURL("/models")
	parsedURL, err := url.Parse(apiURL)
	if err != nil {
		return nil, fmt.Errorf("parse API URL: %w", err)
	}

	params := url.Values{}
	params.Set("search", query)
	params.Set("sort", "downloads")
	params.Set("direction", "-1")
	params.Set("limit", fmt.Sprintf("%d", limit))
	// Ищем модели, которые поддерживают GGUF
	params.Set("library", "gguf")
	parsedURL.RawQuery = params.Encode()

	req, err := d.newRequest("GET", parsedURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req = req.WithContext(ctx)

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}

	// Парсим ответ HF API
	var hfModels []struct {
		ID          string `json:"id"`
		LastUpdated string `json:"lastModified"`
		Downloads   int    `json:"downloads"`
		Likes       int    `json:"likes"`
		PipelineTag string `json:"pipeline_tag"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&hfModels); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	// Конвертируем в наш тип
	result := make([]HFModelRepo, 0, len(hfModels))
	for _, m := range hfModels {
		parts := strings.SplitN(m.ID, "/", 2)
		author := ""
		name := m.ID
		if len(parts) == 2 {
			author = parts[0]
			name = parts[1]
		}

		repo := HFModelRepo{
			ID:          m.ID,
			Name:        name,
			Author:      author,
			LastUpdated: m.LastUpdated,
			Downloads:   m.Downloads,
			Likes:       m.Likes,
			PipelineTag: m.PipelineTag,
		}
		result = append(result, repo)
	}

	log.Infow("search completed", "results", len(result))
	return result, nil
}

// ListModelFiles получает список GGUF файлов в репозитории
func (d *HuggingFaceDownloader) ListModelFiles(ctx context.Context, modelID, revision string) ([]HFFileInfo, error) {
	log := logger.Get()
	log.Infow("listing model files", "modelID", modelID, "revision", revision)

	if revision == "" {
		revision = "main"
	}

	// Используем HF API: GET /api/models/{modelID}/tree/{revision}
	apiURL := d.getAPIURL(fmt.Sprintf("/models/%s/tree/%s", modelID, revision))

	req, err := d.newRequest("GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req = req.WithContext(ctx)

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}

	// Парсим ответ
	var hfFiles []struct {
		Path string `json:"path"`
		Size int64  `json:"size"`
		Type string `json:"type"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&hfFiles); err != nil {
		// Если не удалось распарсить как массив, пробуем другой формат
		log.Warnw("failed to parse file list as array, trying alternative format", "error", err)
		return d.listModelFilesAlternative(ctx, modelID, revision)
	}

	result := make([]HFFileInfo, 0, len(hfFiles))
	for _, f := range hfFiles {
		if f.Type == "file" && strings.HasSuffix(strings.ToLower(f.Path), ".gguf") {
			info := HFFileInfo{
				Path:         f.Path,
				SizeBytes:    f.Size,
				IsGGUF:       true,
				Quantization: GetFileTypeFromName(f.Path),
			}
			result = append(result, info)
		}
	}

	// Сортируем по размеру (сначала маленькие)
	sort.Slice(result, func(i, j int) bool {
		return result[i].SizeBytes < result[j].SizeBytes
	})

	log.Infow("files listed", "modelID", modelID, "ggufFiles", len(result))
	return result, nil
}

// listModelFilesAlternative — альтернативный метод получения файлов
// Использует прямой HTML парсинг или API /api/models/{modelID}
func (d *HuggingFaceDownloader) listModelFilesAlternative(ctx context.Context, modelID, revision string) ([]HFFileInfo, error) {
	// Пробуем получить список через API модели
	apiURL := d.getAPIURL(fmt.Sprintf("/models/%s", modelID))

	req, err := d.newRequest("GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req = req.WithContext(ctx)

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned status %d for model info", resp.StatusCode)
	}

	// Парсим инфу о модели, ищем siblings (файлы)
	var modelInfo struct {
		Siblings []struct {
			Rfilename string `json:"rfilename"`
			Size      int64  `json:"size"`
		} `json:"siblings"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&modelInfo); err != nil {
		return nil, fmt.Errorf("decode model info: %w", err)
	}

	result := make([]HFFileInfo, 0, len(modelInfo.Siblings))
	for _, s := range modelInfo.Siblings {
		if strings.HasSuffix(strings.ToLower(s.Rfilename), ".gguf") {
			info := HFFileInfo{
				Path:         s.Rfilename,
				SizeBytes:    s.Size,
				IsGGUF:       true,
				Quantization: GetFileTypeFromName(s.Rfilename),
			}
			result = append(result, info)
		}
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].SizeBytes < result[j].SizeBytes
	})

	return result, nil
}

// ============================================================
// Загрузка моделей
// ============================================================

// StartDownload начинает загрузку модели из HuggingFace Hub
func (d *HuggingFaceDownloader) StartDownload(req HFDownloadRequest) (*HFDownloadProgress, error) {
	log := logger.Get()

	if req.ModelID == "" {
		return nil, fmt.Errorf("modelId is required")
	}
	if req.Revision == "" {
		req.Revision = "main"
	}

	// Проверяем, не загружается ли уже эта модель
	downloadKey := d.makeDownloadKey(req.ModelID, req.Filename)
	d.mu.RLock()
	_, exists := d.activeDownloads[downloadKey]
	d.mu.RUnlock()
	if exists {
		return nil, fmt.Errorf("download already in progress for %s", downloadKey)
	}

	// Если файл не указан, авто-выбираем
	if req.Filename == "" && req.AutoDetect {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		files, err := d.ListModelFiles(ctx, req.ModelID, req.Revision)
		if err != nil {
			return nil, fmt.Errorf("list model files: %w", err)
		}

		if len(files) == 0 {
			return nil, fmt.Errorf("no GGUF files found in %s", req.ModelID)
		}

		// Если указана квантизация, фильтруем
		if req.Quantization != "" {
			var filtered []HFFileInfo
			q := strings.ToLower(req.Quantization)
			for _, f := range files {
				if strings.Contains(strings.ToLower(f.Quantization), q) {
					filtered = append(filtered, f)
				}
			}
			if len(filtered) > 0 {
				files = filtered
			}
		}

		// Выбираем самый маленький файл (обычно это Q4_K_M или похожий оптимальный)
		req.Filename = files[0].Path
		log.Infow("auto-selected file", "modelID", req.ModelID, "filename", req.Filename)
	} else if req.Filename == "" {
		// Пытаемся найти GGUF файлы и взять первый
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		files, err := d.ListModelFiles(ctx, req.ModelID, req.Revision)
		if err == nil && len(files) > 0 {
			req.Filename = files[0].Path
			log.Infow("auto-selected first file", "modelID", req.ModelID, "filename", req.Filename)
		} else {
			return nil, fmt.Errorf("filename is required when autoDetect is false")
		}
	}

	// Проверяем, не загружен ли уже этот файл
	filename := filepath.Base(req.Filename)
	destPath := filepath.Join(d.downloadsDir, filename)
	finalPath := filepath.Join(d.modelsDir, filename)

	if _, err := os.Stat(finalPath); err == nil {
		return nil, fmt.Errorf("model file already exists: %s", finalPath)
	}

	// Создаём контекст с отменой
	ctx, cancel := context.WithCancel(context.Background())

	progress := HFDownloadProgress{
		ModelID:    req.ModelID,
		Filename:   filename,
		Status:     "downloading",
		StartedAt:  time.Now().UTC().Format(time.RFC3339),
	}

	task := &downloadTask{
		request:   req,
		progress:  progress,
		cancel:    cancel,
		completed: make(chan struct{}),
	}

	d.mu.Lock()
	d.activeDownloads[downloadKey] = task
	d.mu.Unlock()

	// Запускаем загрузку в фоне
	go d.downloadFile(ctx, task, req, destPath, finalPath)

	log.Infow("download started",
		"modelID", req.ModelID,
		"filename", req.Filename,
		"destPath", destPath)

	return &task.progress, nil
}

// downloadFile — внутренняя функция загрузки файла
func (d *HuggingFaceDownloader) downloadFile(ctx context.Context, task *downloadTask, req HFDownloadRequest, destPath, finalPath string) {
	defer close(task.completed)
	defer func() {
		d.mu.Lock()
		delete(d.activeDownloads, d.makeDownloadKey(req.ModelID, req.Filename))
		d.mu.Unlock()
	}()

	// Семафор для ограничения одновременных загрузок
	select {
	case d.semaphore <- struct{}{}:
		defer func() { <-d.semaphore }()
	case <-ctx.Done():
		d.updateTaskStatus(task, "cancelled", "")
		return
	}

	log := logger.Get()
	downloadURL := d.getResolveURL(req.ModelID, req.Filename, req.Revision)

	log.Infow("downloading file",
		"url", downloadURL,
		"dest", destPath,
		"mirror", d.mirror)

	// Создаём HTTP запрос
	httpReq, err := d.newRequest("GET", downloadURL, nil)
	if err != nil {
		d.updateTaskError(task, fmt.Sprintf("create request: %v", err))
		return
	}
	httpReq = httpReq.WithContext(ctx)

	// Выполняем запрос
	resp, err := d.httpClient.Do(httpReq)
	if err != nil {
		d.updateTaskError(task, fmt.Sprintf("HTTP request: %v", err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		d.updateTaskError(task, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body)))
		return
	}

	// Получаем размер файла
	totalBytes := resp.ContentLength
	if totalBytes <= 0 {
		// Пробуем из заголовка Content-Range или другого
		totalBytes = 0 // неизвестно
	}

	// Обновляем прогресс
	d.mu.Lock()
	task.progress.TotalBytes = totalBytes
	d.mu.Unlock()

	// Создаём временный файл в директории загрузок
	if err := os.MkdirAll(d.downloadsDir, 0755); err != nil {
		d.updateTaskError(task, fmt.Sprintf("create downloads dir: %v", err))
		return
	}

	tmpPath := destPath + ".download"
	tmpFile, err := os.Create(tmpPath)
	if err != nil {
		d.updateTaskError(task, fmt.Sprintf("create temp file: %v", err))
		return
	}
	defer tmpFile.Close()

	// Копируем с отслеживанием прогресса
	buf := make([]byte, ChunkSize)
	var downloaded int64
	var lastUpdate time.Time
	var lastBytes int64
	startTime := time.Now()

	for {
		select {
		case <-ctx.Done():
			tmpFile.Close()
			os.Remove(tmpPath)
			d.updateTaskStatus(task, "cancelled", "")
			return
		default:
		}

		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := tmpFile.Write(buf[:n]); writeErr != nil {
				tmpFile.Close()
				os.Remove(tmpPath)
				d.updateTaskError(task, fmt.Sprintf("write file: %v", writeErr))
				return
			}
			downloaded += int64(n)

			// Обновляем прогресс каждые 100ms
			now := time.Now()
			if now.Sub(lastUpdate) > 100*time.Millisecond {
				speed := int64(0)
				if !lastUpdate.IsZero() {
					dt := now.Sub(lastUpdate).Milliseconds()
					if dt > 0 {
						speed = (downloaded - lastBytes) * 1000 / dt
					}
				}

				pct := 0.0
				if totalBytes > 0 {
					pct = float64(downloaded) / float64(totalBytes) * 100
				}

				d.mu.Lock()
				task.progress.Downloaded = downloaded
				task.progress.ProgressPct = pct
				task.progress.SpeedBps = speed
				d.mu.Unlock()

				lastUpdate = now
				lastBytes = downloaded
			}
		}

		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			tmpFile.Close()
			os.Remove(tmpPath)
			d.updateTaskError(task, fmt.Sprintf("read response: %v", readErr))
			return
		}
	}

	// Закрываем временный файл
	tmpFile.Close()

	// Перемещаем в директорию моделей
	if err := os.MkdirAll(d.modelsDir, 0755); err != nil {
		os.Remove(tmpPath)
		d.updateTaskError(task, fmt.Sprintf("create models dir: %v", err))
		return
	}

	// Если файл уже существует в modelsDir, удаляем старый
	if _, err := os.Stat(finalPath); err == nil {
		os.Remove(finalPath)
	}

	if err := os.Rename(tmpPath, finalPath); err != nil {
		// Пробуем copy+delete
		srcFile, err := os.Open(tmpPath)
		if err != nil {
			os.Remove(tmpPath)
			d.updateTaskError(task, fmt.Sprintf("rename failed: %v", err))
			return
		}
		defer srcFile.Close()

		dstFile, err := os.Create(finalPath)
		if err != nil {
			os.Remove(tmpPath)
			d.updateTaskError(task, fmt.Sprintf("create final file: %v", err))
			return
		}
		defer dstFile.Close()

		if _, err := io.Copy(dstFile, srcFile); err != nil {
			os.Remove(tmpPath)
			os.Remove(finalPath)
			d.updateTaskError(task, fmt.Sprintf("copy to models: %v", err))
			return
		}
		os.Remove(tmpPath)
	}

	// Обновляем прогресс как завершённый
	duration := time.Since(startTime)
	d.mu.Lock()
	task.progress.Downloaded = downloaded
	task.progress.TotalBytes = downloaded
	task.progress.ProgressPct = 100.0
	task.progress.SpeedBps = int64(float64(downloaded) / duration.Seconds())
	task.progress.Status = "completed"
	task.progress.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	task.progress.Filename = filenameFromPath(destPath)
	d.downloadHistory = append(d.downloadHistory, task.progress)
	if len(d.downloadHistory) > 50 {
		d.downloadHistory = d.downloadHistory[len(d.downloadHistory)-50:]
	}
	d.mu.Unlock()

	log.Infow("download completed",
		"modelID", req.ModelID,
		"filename", filenameFromPath(destPath),
		"sizeBytes", downloaded,
		"duration", duration.String(),
		"speed", formatSpeed(downloaded, duration))
}

// ============================================================
// Управление загрузками
// ============================================================

// CancelDownload отменяет загрузку
func (d *HuggingFaceDownloader) CancelDownload(modelID, filename string) error {
	downloadKey := d.makeDownloadKey(modelID, filename)

	d.mu.RLock()
	task, exists := d.activeDownloads[downloadKey]
	d.mu.RUnlock()

	if !exists {
		return fmt.Errorf("no active download for %s", downloadKey)
	}

	task.cancel()
	<-task.completed

	return nil
}

// GetDownloadProgress возвращает прогресс загрузки
func (d *HuggingFaceDownloader) GetDownloadProgress(modelID, filename string) (*HFDownloadProgress, error) {
	downloadKey := d.makeDownloadKey(modelID, filename)

	d.mu.RLock()
	task, exists := d.activeDownloads[downloadKey]
	d.mu.RUnlock()

	if !exists {
		// Проверяем историю
		d.mu.RLock()
		for _, p := range d.downloadHistory {
			if p.ModelID == modelID && p.Filename == filename {
				d.mu.RUnlock()
				return &p, nil
			}
		}
		d.mu.RUnlock()
		return nil, fmt.Errorf("no download found for %s", downloadKey)
	}

	// Копируем прогресс
	d.mu.RLock()
	progress := task.progress
	d.mu.RUnlock()

	return &progress, nil
}

// ListActiveDownloads возвращает список активных загрузок
func (d *HuggingFaceDownloader) ListActiveDownloads() []HFDownloadProgress {
	d.mu.RLock()
	defer d.mu.RUnlock()

	result := make([]HFDownloadProgress, 0, len(d.activeDownloads))
	for _, task := range d.activeDownloads {
		result = append(result, task.progress)
	}
	return result
}

// ListDownloadHistory возвращает историю загрузок
func (d *HuggingFaceDownloader) ListDownloadHistory() []HFDownloadProgress {
	d.mu.RLock()
	defer d.mu.RUnlock()

	result := make([]HFDownloadProgress, len(d.downloadHistory))
	copy(result, d.downloadHistory)
	return result
}

// ============================================================
// Вспомогательные функции
// ============================================================

func (d *HuggingFaceDownloader) makeDownloadKey(modelID, filename string) string {
	return fmt.Sprintf("%s/%s", modelID, filename)
}

func (d *HuggingFaceDownloader) updateTaskStatus(task *downloadTask, status, errMsg string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	task.progress.Status = status
	task.progress.ErrorMessage = errMsg
	if status == "completed" || status == "failed" {
		task.progress.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if status == "completed" || status == "failed" || status == "cancelled" {
		d.downloadHistory = append(d.downloadHistory, task.progress)
		if len(d.downloadHistory) > 50 {
			d.downloadHistory = d.downloadHistory[len(d.downloadHistory)-50:]
		}
	}
}

func (d *HuggingFaceDownloader) updateTaskError(task *downloadTask, errMsg string) {
	d.updateTaskStatus(task, "failed", errMsg)
	logger.Get().Errorw("download failed",
		"modelID", task.request.ModelID,
		"filename", task.request.Filename,
		"error", errMsg)
}

func filenameFromPath(path string) string {
	return filepath.Base(path)
}

func formatSpeed(bytes int64, duration time.Duration) string {
	secs := duration.Seconds()
	if secs <= 0 {
		return "0 B/s"
	}
	bps := float64(bytes) / secs
	switch {
	case bps >= 1<<30:
		return fmt.Sprintf("%.2f GB/s", bps/(1<<30))
	case bps >= 1<<20:
		return fmt.Sprintf("%.2f MB/s", bps/(1<<20))
	case bps >= 1<<10:
		return fmt.Sprintf("%.2f KB/s", bps/(1<<10))
	default:
		return fmt.Sprintf("%.0f B/s", bps)
	}
}

// GetLocalPath возвращает локальный путь к скачанной HF-модели по идентификатору "repo/filename"
// Используется для резолвинга hf:-префиксных имён моделей
func (d *HuggingFaceDownloader) GetLocalPath(hfModelRef string) (string, error) {
	// hfModelRef может быть: "repo/filename.gguf" или "repo"
	parts := strings.SplitN(hfModelRef, "/", 2)
	filename := ""
	if len(parts) == 2 {
		filename = filepath.Base(parts[1])
	} else {
		filename = filepath.Base(hfModelRef)
	}

	// Ищем файл в modelsDir
	candidate := filepath.Join(d.modelsDir, filename)
	if _, err := os.Stat(candidate); err == nil {
		return candidate, nil
	}

	// Ищем по частичному совпадению в modelsDir
	entries, err := os.ReadDir(d.modelsDir)
	if err == nil {
		for _, entry := range entries {
			if !entry.IsDir() && strings.Contains(entry.Name(), filename) {
				return filepath.Join(d.modelsDir, entry.Name()), nil
			}
		}
	}

	// Ищем в downloadsDir
	candidate = filepath.Join(d.downloadsDir, filename)
	if _, err := os.Stat(candidate); err == nil {
		return candidate, nil
	}

	return "", fmt.Errorf("model file %s not found locally (searched in %s and %s)", filename, d.modelsDir, d.downloadsDir)
}

// InitializeDownloadDir создаёт необходимые директории
func (d *HuggingFaceDownloader) InitializeDownloadDir() error {
	for _, dir := range []string{d.downloadsDir, d.modelsDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("create directory %s: %w", dir, err)
		}
	}
	return nil
}

// Close завершает все активные загрузки
func (d *HuggingFaceDownloader) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()

	for key, task := range d.activeDownloads {
		task.cancel()
		delete(d.activeDownloads, key)
	}
}
