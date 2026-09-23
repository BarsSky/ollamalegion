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
	"strconv"
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
	ID          string       `json:"id"`          // e.g. "TheBloke/Llama-2-7B-GGUF"
	Name        string       `json:"name"`        // e.g. "Llama-2-7B-GGUF"
	Author      string       `json:"author"`      // e.g. "TheBloke"
	LastUpdated string       `json:"lastUpdated"` // ISO 8601
	Downloads   int          `json:"downloads"`
	Likes       int          `json:"likes"`
	PipelineTag string       `json:"pipelineTag"`     // e.g. "text-generation"
	Files       []HFFileInfo `json:"files,omitempty"` // Список .gguf файлов (заполняется в SearchModels)
	TotalSize   int64        `json:"totalSize"`       // Суммарный размер всех .gguf
	HasGGUF     bool         `json:"hasGguf"`         // true если в репо есть хотя бы один .gguf
	Recommended string       `json:"recommended"`     // Path рекомендованного .gguf (Q4_K_M если есть)
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
	ModelID      string  `json:"modelId"`     // e.g. "TheBloke/Llama-2-7B-GGUF"
	Filename     string  `json:"filename"`    // имя файла
	TotalBytes   int64   `json:"totalBytes"`  // общий размер
	Downloaded   int64   `json:"downloaded"`  // загружено байт
	ProgressPct  float64 `json:"progressPct"` // процент
	SpeedBps     int64   `json:"speedBps"`    // скорость байт/сек
	Status       string  `json:"status"`      // "downloading", "completed", "failed", "cancelled", "interrupted"
	ErrorMessage string  `json:"errorMessage,omitempty"`
	StartedAt    string  `json:"startedAt"` // ISO 8601
	CompletedAt  string  `json:"completedAt,omitempty"`
	// Round 17.3 (2026-08-03): пути и resume support.
	// TempPath = где сейчас лежит частично скачанный .download файл.
	// FinalPath = куда переедет файл после успешного завершения.
	// Resumable = true если есть .download файл с N байт и можно
	//   продолжить через HTTP Range request.
	TempPath    string `json:"tempPath,omitempty"`
	FinalPath   string `json:"finalPath,omitempty"`
	Resumable   bool   `json:"resumable"`
	ResumedFrom int64  `json:"resumedFrom,omitempty"` // байт с которого продолжили (0 если fresh)
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

	// onDownloadComplete — R66d (2026-09-23): вызывается после успешной
	// загрузки файла в modelsDir. Backend подписывается на него, чтобы
	// пересканировать каталог моделей: ListModels()/GetModelMeta() читают кэш
	// ggufFiles, который наполняется только в ScanModels(), поэтому без этого
	// вызова скачанная модель не появлялась ни в /api/models/files (вкладка GGUF
	// в WebUI), ни в FindModelByPath — «скачали, а модели нет» до рестарта
	// cppworker. Тот же класс бага, что чинили в R66c для вкладки GGUF.
	onDownloadComplete func(filename string)
}

// SetOnDownloadComplete — подписка на успешное завершение загрузки.
func (d *HuggingFaceDownloader) SetOnDownloadComplete(fn func(filename string)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onDownloadComplete = fn
}

// notifyDownloadComplete — дергает подписчика (если он есть) вне лока.
func (d *HuggingFaceDownloader) notifyDownloadComplete(filename string) {
	d.mu.RLock()
	fn := d.onDownloadComplete
	d.mu.RUnlock()
	if fn != nil {
		fn(filename)
	}
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
		token:        token,
		mirror:       mirror,
		downloadsDir: downloadsDir,
		modelsDir:    modelsDir,
		httpClient: &http.Client{
			Timeout: DefaultDownloadTimeout,
			Transport: &http.Transport{
				MaxIdleConns:       10,
				IdleConnTimeout:    30 * time.Second,
				DisableCompression: false,
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

// popularQuantPriority — приоритет квантизаций для рекомендации «лучшего» файла
// и сортировки в UI. Меньшее значение = выше приоритет.
var popularQuantPriority = []string{
	"Q4_K_M", "Q5_K_M", "Q6_K", "Q8_0", "Q4_0", "Q4_K_S",
	"Q3_K_M", "Q2_K", "Q5_0", "Q5_1", "Q4_1",
	"Q3_K_S", "Q3_K_L", "Q2_K_S", "F16", "F32",
	"FP16", "FP32", "BF16",
}

// quantPriority возвращает приоритет квантизации (0 = самый популярный).
func quantPriority(quant string) int {
	q := strings.ToUpper(strings.TrimSpace(quant))
	for i, p := range popularQuantPriority {
		if strings.Contains(q, p) {
			return i
		}
	}
	// При сравнении файлов: если квантизация «unknown» или пуста — ставим в конец.
	return len(popularQuantPriority) + 1
}

// pickRecommended выбирает рекомендованный .gguf (Q4_K_M если есть, иначе по приоритету).
func pickRecommended(files []HFFileInfo) string {
	if len(files) == 0 {
		return ""
	}
	best := files[0]
	bestPrio := quantPriority(best.Quantization)
	for _, f := range files[1:] {
		prio := quantPriority(f.Quantization)
		if prio < bestPrio {
			best = f
			bestPrio = prio
		}
	}
	return best.Path
}

// sortFilesByPopularity сортирует .gguf файлы по популярности квантизации.
// Сначала Q4_K_M, потом Q5_K_M и т.д. Файлы с одинаковой квантизацией
// сортируются по размеру (сначала меньшие — обычно split-файлы одинаковой квантизации).
func sortFilesByPopularity(files []HFFileInfo) {
	sort.SliceStable(files, func(i, j int) bool {
		pi := quantPriority(files[i].Quantization)
		pj := quantPriority(files[j].Quantization)
		if pi != pj {
			return pi < pj
		}
		return files[i].SizeBytes < files[j].SizeBytes
	})
}

// SearchModels выполняет поиск моделей на HuggingFace Hub.
// Для каждого результата параллельно запрашивает список .gguf файлов,
// фильтрует репозитории без GGUF и сортирует файлы по популярности.
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

	// Параллельно подгружаем список .gguf файлов для каждого репозитория.
	// Используем семафор на 4 одновременных запроса + 10s таймаут на каждый.
	// Репозитории без GGUF исключаем из результата.
	filtered, err := d.enrichWithFiles(ctx, result)
	if err != nil {
		log.Warnw("enrichWithFiles returned error, returning partial results", "error", err)
		// Возвращаем что есть, даже если часть не обогатилась файлами
	}

	log.Infow("search completed", "results", len(filtered), "withFiles", countWithFiles(filtered))
	return filtered, nil
}

// enrichWithFiles параллельно запрашивает ListModelFiles для каждого репо и
// обогащает результат списком .gguf файлов. Возвращает только репо с GGUF.
func (d *HuggingFaceDownloader) enrichWithFiles(ctx context.Context, repos []HFModelRepo) ([]HFModelRepo, error) {
	log := logger.Get()

	type enriched struct {
		idx   int
		repo  HFModelRepo
		files []HFFileInfo
		err   error
	}

	const maxConcurrent = 4
	sem := make(chan struct{}, maxConcurrent)
	results := make([]enriched, len(repos))

	var wg sync.WaitGroup
	for i, repo := range repos {
		wg.Add(1)
		go func(idx int, r HFModelRepo) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// Индивидуальный таймаут для каждого запроса
			fileCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()

			files, err := d.ListModelFiles(fileCtx, r.ID, "main")
			results[idx] = enriched{idx: idx, repo: r, files: files, err: err}
		}(i, repo)
	}
	wg.Wait()

	// Собираем результаты, фильтруя репо без .gguf файлов
	out := make([]HFModelRepo, 0, len(repos))
	for _, e := range results {
		if e.err != nil {
			log.Debugw("failed to fetch files for repo", "repo", e.repo.ID, "error", e.err)
			// Если не удалось получить файлы — пропускаем репо
			// (нет GGUF ⇒ не показываем в UI)
			continue
		}
		if len(e.files) == 0 {
			log.Debugw("repo has no GGUF files, skipping", "repo", e.repo.ID)
			continue
		}

		// Сортируем по популярности квантизации
		sortFilesByPopularity(e.files)

		// Суммарный размер
		var total int64
		for _, f := range e.files {
			total += f.SizeBytes
		}

		e.repo.Files = e.files
		e.repo.TotalSize = total
		e.repo.HasGGUF = true
		e.repo.Recommended = pickRecommended(e.files)
		out = append(out, e.repo)
	}

	return out, nil
}

func countWithFiles(repos []HFModelRepo) int {
	n := 0
	for _, r := range repos {
		if r.HasGGUF {
			n++
		}
	}
	return n
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
	// Если уже идёт загрузка — возвращаем её текущий прогресс (HTTP 200/идемпотентно),
	// а не ошибку. Это позволяет WebUI обрабатывать повторные клики по «Download»
	// без ложных 500-х и сразу переключаться на вкладку Downloads.
	downloadKey := d.makeDownloadKey(req.ModelID, req.Filename)
	d.mu.RLock()
	existing, exists := d.activeDownloads[downloadKey]
	d.mu.RUnlock()
	if exists {
		// Возвращаем снимок текущего прогресса (без удержания блокировки)
		d.mu.RLock()
		snapshot := existing.progress
		d.mu.RUnlock()
		log.Infow("download already in progress, returning current progress",
			"modelID", req.ModelID, "filename", req.Filename,
			"status", snapshot.Status, "progressPct", snapshot.ProgressPct)
		return &snapshot, nil
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

	// Round 17.3 (2026-08-03): RESUME support.
	// Проверяем существует ли .download файл от предыдущей попытки.
	// Если да — пометить Resumable=true и ResumedFrom=size.
	tmpPath := destPath + ".download"
	var resumedFrom int64
	resumable := false
	if fi, err := os.Stat(tmpPath); err == nil && fi.Size() > 0 {
		resumedFrom = fi.Size()
		resumable = true
		log.Infow("resuming partial download",
			"modelID", req.ModelID,
			"filename", filename,
			"resumedFrom", resumedFrom,
			"tmpPath", tmpPath)
	}

	progress := HFDownloadProgress{
		ModelID:     req.ModelID,
		Filename:    filename,
		Status:      "downloading",
		StartedAt:   time.Now().UTC().Format(time.RFC3339),
		TempPath:    tmpPath,   // UI: "Currently at: /app/downloads/foo.gguf.download"
		FinalPath:   finalPath, // UI: "Will end up at: /app/models/foo.gguf"
		Resumable:   resumable,
		ResumedFrom: resumedFrom,
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

	// Round 17.3 (2026-08-03): RESUME support — HTTP Range request.
	// Если tmpPath уже существует с N байт (от предыдущей попытки), шлём
	// `Range: bytes=N-` чтобы получить только остаток файла. HuggingFace
	// (как и любой S3-совместимый storage) возвращает 206 Partial Content
	// с заголовком `Content-Range: bytes N-(total-1)/total`.
	existingSize := task.progress.ResumedFrom
	if existingSize > 0 {
		httpReq.Header.Set("Range", fmt.Sprintf("bytes=%d-", existingSize))
		log.Infow("sending HTTP Range request to resume",
			"modelID", req.ModelID,
			"filename", req.Filename,
			"fromByte", existingSize)
	}

	// Выполняем запрос
	resp, err := d.httpClient.Do(httpReq)
	if err != nil {
		d.updateTaskError(task, fmt.Sprintf("HTTP request: %v", err))
		return
	}
	defer resp.Body.Close()

	// Для resume: ожидаем 206 Partial Content, для fresh: 200 OK.
	// Если сервер не поддерживает Range (например, CDN) — 200 OK с полным файлом,
	// тогда нужно начать с нуля (удаляем tmp файл).
	// ПРИМЕЧАНИЕ: tmpPath ещё не определён ниже — определяем здесь явно для ранней
	// проверки (перед MkdirAll downloadsDir).
	tmpPathEarly := destPath + ".download"
	if existingSize > 0 && resp.StatusCode == http.StatusOK {
		log.Warnw("server doesn't support Range, restarting from 0",
			"modelID", req.ModelID, "filename", req.Filename)
		os.Remove(tmpPathEarly)
		existingSize = 0
		d.mu.Lock()
		task.progress.ResumedFrom = 0
		task.progress.Resumable = false
		d.mu.Unlock()
	} else if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		d.updateTaskError(task, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body)))
		return
	}

	// Получаем размер файла
	totalBytes := resp.ContentLength
	if totalSize := resp.Header.Get("Content-Range"); totalSize != "" {
		// Content-Range: bytes 0-99/12345 → totalBytes=12345
		if slash := strings.LastIndex(totalSize, "/"); slash >= 0 {
			if t, err := strconv.ParseInt(totalSize[slash+1:], 10, 64); err == nil && t > 0 {
				totalBytes = t
			}
		}
	}
	if totalBytes <= 0 {
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
	// Round 17.3: если resuming — открываем файл в append mode.
	// Если fresh (existingSize==0) — создаём новый (truncate если был garbage).
	// (tmpPathEarly использовался выше для early Range check, теперь tmpPath — основной)
	var tmpFile *os.File
	if existingSize > 0 {
		tmpFile, err = os.OpenFile(tmpPath, os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			// Файл мог быть удалён между StartDownload и downloadFile — fallback на fresh.
			log.Warnw("resumed file missing, starting fresh",
				"tmpPath", tmpPath, "error", err)
			existingSize = 0
			tmpFile, err = os.Create(tmpPath)
		}
	} else {
		tmpFile, err = os.Create(tmpPath)
	}
	if err != nil {
		d.updateTaskError(task, fmt.Sprintf("create/open temp file: %v", err))
		return
	}
	defer tmpFile.Close()

	// Копируем с отслеживанием прогресса
	buf := make([]byte, ChunkSize)
	// downloaded = сколько скачали в ЭТОМ запуске; existingSize = сколько уже было на диске.
	// Общий прогресс = existingSize + downloaded.
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

				totalDownloaded := existingSize + downloaded
				pct := 0.0
				if totalBytes > 0 {
					pct = float64(totalDownloaded) / float64(totalBytes) * 100
				}

				d.mu.Lock()
				task.progress.Downloaded = totalDownloaded
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
			// Round 17.3: НЕ удаляем partial file при network/IO ошибке —
			// оставляем для RESUME при следующем вызове StartDownload.
			// Удаляем только при явной отмене (ctx.Done()).
			tmpFile.Close()
			if ctx.Err() != nil {
				// User cancelled — cleanup
				os.Remove(tmpPath)
				d.updateTaskStatus(task, "cancelled", readErr.Error())
			} else {
				// Network/IO error — keep partial file
				totalDownloaded := existingSize + downloaded
				log.Warnw("download interrupted, partial file kept for resume",
					"modelID", req.ModelID,
					"filename", req.Filename,
					"partialBytes", totalDownloaded,
					"tmpPath", tmpPath,
					"readError", readErr.Error())
				d.updateTaskStatus(task, "interrupted", readErr.Error())
				// Resumable остаётся true — следующий StartDownload подхватит
				d.mu.Lock()
				task.progress.Downloaded = totalDownloaded
				d.mu.Unlock()
			}
			return
		}
	}

	// Закрываем временный файл
	tmpFile.Close()

	// Round 17.3: финальный размер = existingSize (от прошлой попытки) + downloaded (этот запуск).
	// Это для корректного ProgressPct=100% и метрик.
	totalDownloaded := existingSize + downloaded

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

	// R66.3 (2026-09-16): use copy+delete unconditionally instead of os.Rename.
	//
	// Pre-R66.3: cppworker пытался os.Rename(tmpPath, finalPath) первым.
	// Если tmpPath (downloads/) и finalPath (/app/models/) на РАЗНЫХ
	// файловых системах (типичный случай в WSL: /downloads на overlay ext4,
	// /app/models на 9p drvfs mount от Windows) — os.Rename падает с EXDEV.
	// Fallback на io.Copy работает, но МЕДЛЕННО: 9p write ~50 MB/s vs 1+ GB/s
	// для cross-directory rename в одной FS.
	//
	// Symptom: download 99.996% (progress показывает "100% / 10.8 MB/s"
	// downloading), но goroutine висит в io.Copy на 30+ минут пока
	// копирует через медленный 9p mount.
	//
	// Fix: сразу использовать io.Copy (быстрее fallback'а не нужно,
	// rename не даёт никаких преимуществ когда temp и final на разных FS).
	// Skip os.Rename полностью — никакого win в нём.
	//
	// Для случая когда temp/final на одной FS (например оба на 9p) —
	// io.Copy всё равно корректно копирует, просто чуть медленнее чем
	// rename (rename = атомарный metadata swap, copy = байт за байтом).
	// Это OK потому что мы только что скачали файл — основное время
	// заняло скачивание, не этот последний move.
	//
	// Также: добавляем periodic progress updates в io.Copy loop чтобы UI
	// видел реальный прогресс move→copy (а не висел на 100% с фейковым
	// "downloading" статусом).
	d.moveTempToFinalWithProgress(task, tmpPath, finalPath, totalDownloaded, startTime, destPath, downloaded, existingSize, req)
}

// moveTempToFinalWithProgress — R66.3 helper. Копирует temp → final через io.Copy
// с periodic progress updates. UI видит реальный прогресс move→copy фазы
// (раньше status="downloading" на 100% с ProgressPct=99.996, теперь status="finalizing"
// с честным ProgressPct).
//
// Pre-R66.3: использовался os.Rename + io.Copy fallback. Когда temp и final
// на разных FS (типично для WSL: downloads на overlay, models на 9p mount),
// os.Rename падал с EXDEV и fallback io.Copy молча копировал без progress updates.
// UI думал что download ещё идёт.
func (d *HuggingFaceDownloader) moveTempToFinalWithProgress(
	task *downloadTask,
	tmpPath, finalPath string,
	totalDownloaded int64,
	startTime time.Time,
	destPath string,
	downloaded int64,
	existingSize int64,
	req HFDownloadRequest,
) {
	log := logger.Get()

	// Phase 1: open source
	srcFile, err := os.Open(tmpPath)
	if err != nil {
		os.Remove(tmpPath)
		d.updateTaskError(task, fmt.Sprintf("open temp for move: %v", err))
		return
	}
	defer srcFile.Close()

	// Phase 2: create destination
	dstFile, err := os.Create(finalPath)
	if err != nil {
		os.Remove(tmpPath)
		d.updateTaskError(task, fmt.Sprintf("create final file: %v", err))
		return
	}
	defer dstFile.Close()

	// Phase 3: copy with periodic progress
	// Wrap io.Copy в custom loop чтобы обновлять progress каждые 500ms.
	copyBuf := make([]byte, ChunkSize)
	var copied int64
	lastCopyUpdate := time.Now()

	for {
		n, readErr := srcFile.Read(copyBuf)
		if n > 0 {
			if _, writeErr := dstFile.Write(copyBuf[:n]); writeErr != nil {
				os.Remove(tmpPath)
				os.Remove(finalPath)
				d.updateTaskError(task, fmt.Sprintf("copy to models: %v", writeErr))
				return
			}
			copied += int64(n)
			if time.Since(lastCopyUpdate) > 500*time.Millisecond {
				// Update UI: switch status from "downloading" to "finalizing"
				d.mu.Lock()
				task.progress.Status = "finalizing"
				d.mu.Unlock()
				lastCopyUpdate = time.Now()
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			os.Remove(tmpPath)
			os.Remove(finalPath)
			d.updateTaskError(task, fmt.Sprintf("copy read: %v", readErr))
			return
		}
	}

	// Copy complete — close dst before removing tmp
	dstFile.Close()

	if err := os.Remove(tmpPath); err != nil {
		log.Warnw("failed to remove temp after copy", "tmpPath", tmpPath, "error", err)
	}

	// Обновляем прогресс как завершённый
	duration := time.Since(startTime)
	d.mu.Lock()
	task.progress.Downloaded = totalDownloaded
	task.progress.TotalBytes = totalDownloaded
	task.progress.ProgressPct = 100.0
	task.progress.SpeedBps = int64(float64(downloaded) / duration.Seconds())
	task.progress.Status = "completed"
	task.progress.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	task.progress.Filename = filenameFromPath(destPath)
	task.progress.Resumable = false // больше нечего resume'ить
	d.downloadHistory = append(d.downloadHistory, task.progress)
	if len(d.downloadHistory) > 50 {
		d.downloadHistory = d.downloadHistory[len(d.downloadHistory)-50:]
	}
	d.mu.Unlock()

	log.Infow("download completed",
		"modelID", req.ModelID,
		"filename", filenameFromPath(destPath),
		"sizeBytes", totalDownloaded,
		"resumedFrom", existingSize,
		"downloadedThisRun", downloaded,
		"duration", duration.String(),
		"speed", formatSpeed(downloaded, duration))

	// R66d: сообщаем Backend'у, что каталог моделей изменился — иначе свежая
	// модель не появится в /api/models/files (вкладка GGUF) до рестарта.
	d.notifyDownloadComplete(filenameFromPath(finalPath))
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

// DeleteDownload — Round 17.3 (2026-08-03): удаляет скачанный/частичный файл
// из контейнера, освобождая место на диске. Удаляет:
//   - .download файл (частичная загрузка, downloadsDir)
//   - final файл (полная загрузка, modelsDir)
//   - запись из downloadHistory (помечается как cleaned)
//
// Возвращает DeleteDownloadResult с информацией о том, что было удалено и
// сколько байт освобождено — для UI toast "Disk freed: 5.2 GB".
func (d *HuggingFaceDownloader) DeleteDownload(modelID, filename string) (*DeleteDownloadResult, error) {
	filename = filepath.Base(filename)
	result := &DeleteDownloadResult{
		ModelID:  modelID,
		Filename: filename,
	}

	// 1. Удаляем .download файл (если есть)
	tmpPath := filepath.Join(d.downloadsDir, filename+".download")
	if fi, err := os.Stat(tmpPath); err == nil {
		if err := os.Remove(tmpPath); err == nil {
			result.TempDeleted = true
			result.BytesFreed += fi.Size()
		}
	}

	// 2. Удаляем final файл (если есть)
	finalPath := filepath.Join(d.modelsDir, filename)
	if fi, err := os.Stat(finalPath); err == nil {
		if err := os.Remove(finalPath); err == nil {
			result.FinalDeleted = true
			result.BytesFreed += fi.Size()
		}
	}

	// 3. Чистим history — помечаем что файл удалён (Resumable=false).
	// Не удаляем запись — пользователь может видеть что скачивал.
	d.mu.Lock()
	for i := range d.downloadHistory {
		if d.downloadHistory[i].ModelID == modelID && d.downloadHistory[i].Filename == filename {
			d.downloadHistory[i].Resumable = false
			d.downloadHistory[i].TempPath = "" // пути уже неактуальны
			d.downloadHistory[i].FinalPath = ""
			d.downloadHistory[i].ErrorMessage = "deleted by user"
		}
	}
	d.mu.Unlock()

	if !result.TempDeleted && !result.FinalDeleted {
		return result, fmt.Errorf("no file found for %s/%s", modelID, filename)
	}

	return result, nil
}

// DeleteDownloadResult — результат DeleteDownload.
type DeleteDownloadResult struct {
	ModelID      string `json:"modelId"`
	Filename     string `json:"filename"`
	TempDeleted  bool   `json:"tempDeleted"`  // .download файл удалён
	FinalDeleted bool   `json:"finalDeleted"` // final файл удалён
	BytesFreed   int64  `json:"bytesFreed"`   // освобождено байт на диске
}

// ============================================================
// Вспомогательные функции
// ============================================================

// OrphanDownloadFile — R66.4 (2026-09-16): файл .download на диске, который
// НЕ привязан ни к одной активной загрузке. Возникает после прерывания
// (network timeout, container restart, crash) — cppworker сохраняет partial
// для возможного resume, но если загрузка больше не активна, файл просто
// "висит" и занимает место. ListOrphanDownloads сканирует downloadsDir
// и возвращает такие файлы, чтобы UI мог показать их с кнопкой "Delete".
//
// ModelID/Revision невозможно восстановить из имени файла (filename = gguf
// имя в репозитории, у разных репо могут быть одинаковые filenames).
// Оставляем пустыми — UI использует только Filename+Size для отображения.
type OrphanDownloadFile struct {
	Path     string `json:"path"`     // полный путь на диске (для UI tooltip)
	Filename string `json:"filename"` // имя файла БЕЗ .download суффикса (для cleanup endpoint)
	Size     int64  `json:"size"`     // текущий размер на диске (bytes)
	Modified int64  `json:"modified"` // mtime в unix seconds
}

// ListOrphanDownloads — R66.4 (2026-09-16): сканирует downloadsDir и возвращает
// список .download файлов, которые НЕ привязаны ни к одной активной загрузке.
// Это позволяет UI показать "residual files" — например, после сбоя сети или
// перезапуска cppworker, когда download прервался и файл остался на диске.
func (d *HuggingFaceDownloader) ListOrphanDownloads() []OrphanDownloadFile {
	d.mu.RLock()
	// Собираем множество ключей активных загрузок чтобы исключить их из orphans
	activeKeys := make(map[string]bool, len(d.activeDownloads))
	for _, task := range d.activeDownloads {
		// task.progress.Filename — это БЕЗ .download суффикса (логическое имя)
		activeKeys[task.progress.Filename] = true
	}
	d.mu.RUnlock()

	entries, err := os.ReadDir(d.downloadsDir)
	if err != nil {
		// Если директории нет — это нормально (никто ещё не качал).
		// Не логируем как ошибку, чтобы не спамить при cold start.
		return []OrphanDownloadFile{}
	}

	var orphans []OrphanDownloadFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".download") {
			continue
		}
		// Логическое имя файла (без .download) — то, что cleanup endpoint ожидает
		logicalName := strings.TrimSuffix(name, ".download")
		if activeKeys[logicalName] {
			continue // это активная загрузка, не orphan
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		orphans = append(orphans, OrphanDownloadFile{
			Path:     filepath.Join(d.downloadsDir, name),
			Filename: logicalName,
			Size:     info.Size(),
			Modified: info.ModTime().Unix(),
		})
	}
	return orphans
}

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
