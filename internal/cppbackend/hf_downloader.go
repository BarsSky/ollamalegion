package cppbackend

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
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
// Расширения файлов моделей
// ============================================================

// ModelWeightExtensions — расширения, которые считаются файлами весов модели.
//
// R-Image (2026-09-27): до этого список был жёстко зашит как ".gguf" в двух
// местах (tree-API и fallback через siblings). Для диффузионных моделей
// (stable-diffusion.cpp) это неверно: diffusion/VAE/text-encoder'ы лежат в
// .safetensors/.sft, а старые all-in-one чекпойнты — в .ckpt. Один bundle =
// НЕСКОЛЬКО файлов разных форматов, поэтому фильтр должен быть один и
// параметризуемый (см. ListModelFilesByFormat).
var ModelWeightExtensions = []string{".gguf", ".safetensors", ".sft", ".ckpt"}

// HasModelWeightExtension — true, если имя файла имеет «весовое» расширение.
// Регистронезависимо: HF отдаёт имена как есть, встречается и ".GGUF".
func HasModelWeightExtension(name string) bool {
	lower := strings.ToLower(name)
	for _, ext := range ModelWeightExtensions {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// FileFormatFromPath — формат файла без точки ("gguf", "safetensors", "sft",
// "ckpt"); "" для не-весовых файлов. Используется вместо булева IsGGUF там,
// где важна разница между форматами (валидация bundle, UI).
func FileFormatFromPath(path string) string {
	lower := strings.ToLower(path)
	for _, ext := range ModelWeightExtensions {
		if strings.HasSuffix(lower, ext) {
			return strings.TrimPrefix(ext, ".")
		}
	}
	return ""
}

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

// HFFileInfo — информация о файле весов в репозитории
type HFFileInfo struct {
	Path         string `json:"path"`         // путь в репозитории
	SizeBytes    int64  `json:"sizeBytes"`    // размер файла
	IsGGUF       bool   `json:"isGGUF"`       // .gguf файл? (обратная совместимость UI)
	Quantization string `json:"quantization"` // извлечённый тип квантизации
	// Format — расширение без точки: gguf | safetensors | sft | ckpt.
	// R-Image: нужно, чтобы UI/каталог могли отличить файл весов диффузии от
	// VAE/text-encoder'а — для bundle-загрузки это разные роли, а не «GGUF или нет».
	Format string `json:"format,omitempty"`
	// SuggestedRole — R-Image Phase 9: ПРЕДЛОЖЕННАЯ роль файла в image-bundle
	// (diffusion|vae|clip_l|clip_g|t5xxl|llm|taesd|lora|upscaler|controlnet|
	// ip_adapter|clip_vision), выведенная из имени файла.
	//
	// Заполняет image-воркер (sdbackend.SuggestRole) в ответе GET /api/hf/files:
	// страница «Image-модели» рисует роль рядом с файлом и подставляет её в
	// строку bundle, чтобы оператор не выбирал роль вслепую. Для текстовых
	// GGUF-моделей поле пустое — там роль не нужна.
	SuggestedRole string `json:"suggestedRole,omitempty"`
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
	// Role — R-Image (2026-09-27): роль файла внутри bundle
	// (diffusion/vae/clip_l/t5xxl/llm/... — см. types.ImageFileRole*).
	// Для одиночной загрузки не используется; нужна для прогресса bundle и
	// для отчёта «какой компонент не скачался».
	Role string `json:"role,omitempty"`
	// SizeBytes — ожидаемый размер файла (0 = неизвестен). Для bundle-загрузки
	// позволяет (а) показать общий прогресс до прихода заголовков ответа и
	// (б) пропустить уже скачанный файл, если его размер совпал — повторный
	// pull не перекачивает гигабайты.
	SizeBytes int64 `json:"sizeBytes,omitempty"`
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

	// activeBundles/bundleHistory — R-Image (2026-09-27): bundle-загрузки
	// (несколько файлов = одна диффузионная модель). Отдельные карты, а не
	// общие с одиночными загрузками: ключ bundle — bundleID (каталог), а не
	// "repo/filename", и агрегировать их прогресс вместе нельзя.
	// См. hf_bundle.go.
	activeBundles map[string]*bundleTask
	bundleHistory []HFBundleProgress

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
		activeBundles:   make(map[string]*bundleTask),
		bundleHistory:   make([]HFBundleProgress, 0),
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
