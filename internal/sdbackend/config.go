// Package sdbackend — серверная часть image-воркера (cmd/sdworker):
// реестр image-моделей, супервизор субпроцесса sd-server (stable-diffusion.cpp)
// и прокси к его нативному async API (/sdcpp/v1/*).
//
// R-Image / Phase 3 (2026-09-28).
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ПАКЕТ, А НЕ cmd/sdworker: ровно как cppbackend для cppworker —
// вся логика (спавн процесса, readiness, очередь, нормализация запросов)
// тестируется без HTTP-сервера и без реального sd-server. cmd/sdworker
// остаётся тонким слоем: конфиг + маршруты + автрегистрация в балансере.
//
// КОНТРАКТЫ ЗАМОРОЖЕНЫ: pkg/types/image_model.go (Profile.ServerArgs,
// PinnedSDServerRevision, ValidateImageModelProfile). Здесь только то, что
// относится к процессу и HTTP, сборку флагов модели не дублируем.
package sdbackend

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"ollama-loadbalancer/pkg/types"
)

// DefaultPort — порт HTTP API воркера по умолчанию.
//
// 18093 — тот же дефолт, что types.DefaultImageWorkerPort: если воркер поднят
// «как в документации», а бэкенд зарегистрирован через WebUI без явного
// imagePort, они совпадут без правок конфига.
const DefaultPort = types.DefaultImageWorkerPort

// Config — конфигурация image-воркера: env (SDWORKER_*) → JSON-файл → дефолты.
//
// Приоритет (как у cppbackend.Config): DefaultConfig() → JSON (если задан
// SDWORKER_CONFIG) → env. CLI-флаги обрабатываются в cmd/sdworker уже поверх
// этого (флаг побеждает env).
type Config struct {
	// HTTP-сервер воркера.
	Host string `json:"host"`
	Port int    `json:"port"`

	// ModelsDir — каталог image-моделей. Схема: <ModelsDir>/<bundleName>/<файлы>,
	// см. types.ImageModelProfile.BundleDir.
	ModelsDir string `json:"modelsDir"`

	// DownloadsDir — каталог темповых .download-файлов ОДИНОЧНЫХ HF-загрузок
	// (bundle'ы кладут темп внутрь своего каталога моделей).
	//
	// ПОЧЕМУ ОТДЕЛЬНЫЙ ОТ cppworker КАТАЛОГ ПО УМОЛЧАНИЮ: .download диффузионной
	// модели — это 6-12 GB. Если он лежит в общем ./downloads, оператор не
	// понимает, какой воркер занимает место, а orphan-cleanup одного воркера
	// удаляет partial другого.
	DownloadsDir string `json:"downloadsDir"`

	// HFToken — токен HuggingFace (gated-модели). Приоритет у заголовка
	// X-HF-Token из запроса: UI хранит токен у себя и шлёт его именно заголовком
	// (webui/js/modules/image-page.js:656). Env: HF_TOKEN (совместимо с
	// cppworker/cppbackend), SDWORKER_HF_TOKEN — переопределение.
	HFToken string `json:"hfToken,omitempty"`

	// HFMirror — зеркало HF (например https://hf-mirror.com). Пусто =
	// huggingface.co. Env: HF_MIRROR (общая переменная проекта).
	HFMirror string `json:"hfMirror,omitempty"`

	// HFBundleAutoFit — runtime.autoFit для профилей, которые воркер
	// генерирует сам после HF-bundle-загрузки: on|off|"" (пусто = не передавать
	// флаг). Дефолт "on": движок сам ужимает плейсмент под доступную VRAM —
	// именно то, что нужно слабым GPU (см. docs/research-sdcpp-lowvram-integration.md).
	HFBundleAutoFit string `json:"hfBundleAutoFit,omitempty"`

	// SDServerBin — путь к бинарю sd-server (или имя в PATH).
	//
	// Почему «или имя»: в Docker бинарь лежит в /usr/local/bin и достаточно
	// "sd-server"; на Windows оператор чаще задаёт полный путь к .exe.
	SDServerBin string `json:"sdServerBin"`

	// HostPort — порт, который слушает сам sd-server (loopback-адрес:
	// движок не должен быть доступен извне воркера — наружу торчит только API
	// воркера, иначе клиенты обойдут нашу нормализацию запросов).
	ListenIP   string `json:"listenIp"`
	ServerPort int    `json:"serverPort"`

	// Каталоги движка. Пусто = флаг не передаём: LoRA/ESRGAN будут доступны
	// только те, что лежат внутри bundle-каталога модели.
	LoraModelDir     string `json:"loraModelDir,omitempty"`
	HiresUpscalersDir string `json:"hiresUpscalersDir,omitempty"`

	// Таймауты процесса и генерации.
	StartupTimeoutSec    int `json:"startupTimeoutSec"`
	GenerationTimeoutSec int `json:"generationTimeoutSec"`

	// IdleUnloadMinutes — выгрузка простаивающей модели (0 = не выгружать).
	// В image-воркере «выгрузка» = kill субпроцесса sd-server (hot-swap в
	// движке отсутствует).
	IdleUnloadMinutes int `json:"idleUnloadMinutes"`

	// MaxConcurrent — размер НАШЕЙ очереди генераций.
	//
	// Движок сериализует исполнение одним мьютексом, поэтому parallelism
	// бесполезен: очередь нужна лишь чтобы ограничить число принятых, но ещё
	// не выполненных запросов и честно отдавать 429/queue_position.
	MaxConcurrent int `json:"maxConcurrent"`

	// PreloadModel — загрузить эту модель при старте (пусто = не загружать).
	PreloadModel string `json:"preloadModel,omitempty"`

	// BaseURL — базовый http-URL воркера «как его видят клиенты»; нужен для
	// response_format:"url" (клиент получает абсолютную ссылку на PNG).
	// Пример: http://192.168.1.10:18093
	BaseURL string `json:"baseUrl,omitempty"`

	// ImageModelsDir — куда складывать PNG/JPEG/WebP для response_format:"url".
	ImagesDir string `json:"imagesDir"`

	// CORS allow-origin для прямых подключений клиентов (SillyTavern и др.).
	CORSOrigin string `json:"corsOrigin"`

	// ExtraArgs — дополнительные флаги sd-server, добавляются ПОСЛЕ argv
	// профиля (операторский «довесок», не заменяет ServerArgs).
	ExtraArgs []string `json:"extraArgs,omitempty"`
}

// DefaultConfig — значения по умолчанию.
func DefaultConfig() Config {
	return Config{
		Host:              "0.0.0.0",
		Port:              DefaultPort,
		ModelsDir:         "./models/image",
		DownloadsDir:      "./downloads/image",
		SDServerBin:       "sd-server",
		ListenIP:          "127.0.0.1",
		ServerPort:        18094,
		StartupTimeoutSec: 180,
		// 600 с: пиковые замеры из исследования доходят до ~341 с
		// (GTX 1060, Z-Image 512x1024, 20 шагов). Меньший дефолт рвал бы
		// легитимные генерации на слабом железе.
		GenerationTimeoutSec: 600,
		IdleUnloadMinutes:    30,
		// 64 — как max_queue_size у самого sd-server (AsyncJobManager): если
		// наша очередь больше, движок начнёт отдавать 429 «job queue is full»
		// уже после того, как мы приняли запрос — двойная и несогласованная
		// очередь. Держим равной.
		MaxConcurrent: 64,
		ImagesDir:     "./data/images",
		CORSOrigin:    "*",
		// Автогенерируемые профили (после HF-bundle-загрузки) получают
		// --auto-fit on: движок сам решает, что держать в VRAM.
		HFBundleAutoFit: "on",
	}
}

// Validate — проверка конфигурации до старта сервера.
func (c *Config) Validate() error {
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("port must be in [1,65535], got %d", c.Port)
	}
	if c.ServerPort <= 0 || c.ServerPort > 65535 {
		return fmt.Errorf("serverPort (sd-server listen port) must be in [1,65535], got %d", c.ServerPort)
	}
	if c.Port == c.ServerPort {
		return fmt.Errorf("port and serverPort must differ: sd-server and the worker cannot share %d", c.Port)
	}
	if strings.TrimSpace(c.SDServerBin) == "" {
		return fmt.Errorf("sdServerBin must not be empty")
	}
	if strings.TrimSpace(c.ModelsDir) == "" {
		return fmt.Errorf("modelsDir must not be empty")
	}
	if c.MaxConcurrent < 1 {
		return fmt.Errorf("maxConcurrent must be >= 1, got %d", c.MaxConcurrent)
	}
	if c.IdleUnloadMinutes < 0 {
		return fmt.Errorf("idleUnloadMinutes must be >= 0, got %d", c.IdleUnloadMinutes)
	}
	if c.StartupTimeoutSec < 0 {
		return fmt.Errorf("startupTimeoutSec must be >= 0, got %d", c.StartupTimeoutSec)
	}
	if c.GenerationTimeoutSec < 0 {
		return fmt.Errorf("generationTimeoutSec must be >= 0, got %d", c.GenerationTimeoutSec)
	}
	if c.HFBundleAutoFit != "" && c.HFBundleAutoFit != "on" && c.HFBundleAutoFit != "off" {
		return fmt.Errorf("hfBundleAutoFit must be on|off|\"\", got %q", c.HFBundleAutoFit)
	}
	return nil
}

// LoadConfigFromFile — читает JSON-конфиг поверх дефолтов.
// Отсутствующий файл — не ошибка (дефолты + env).
func LoadConfigFromFile(path string) (Config, error) {
	cfg := DefaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("read config file %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config file %s: %w", path, err)
	}
	return cfg, nil
}

// LoadConfigFromEnv — накладывает переменные окружения SDWORKER_*.
//
// Имена переменных перечислены в отчёте Phase 3; всё, что не задано, остаётся
// из JSON/дефолтов. Некорректное значение НЕ роняет воркер молча: возвращаем
// ошибку, чтобы оператор увидел опечатку, а не «настройка не применилась».
func LoadConfigFromEnv() (Config, error) {
	cfg := DefaultConfig()
	if path := strings.TrimSpace(os.Getenv("SDWORKER_CONFIG")); path != "" {
		loaded, err := LoadConfigFromFile(path)
		if err != nil {
			return cfg, err
		}
		cfg = loaded
	}
	if err := cfg.ApplyEnv(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// ApplyEnv — применяет env поверх текущего конфига (идемпотентно; вызывается
// из LoadConfigFromEnv и из тестов).
func (c *Config) ApplyEnv() error {
	envStr := func(key string, dst *string) {
		if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
			*dst = strings.TrimSpace(v)
		}
	}
	envStr("SDWORKER_HOST", &c.Host)
	envStr("SDWORKER_MODELS_DIR", &c.ModelsDir)
	envStr("SDWORKER_IMAGE_MODELS_DIR", &c.ModelsDir)
	envStr("SDWORKER_DOWNLOADS_DIR", &c.DownloadsDir)
	envStr("SDWORKER_SD_SERVER_BIN", &c.SDServerBin)
	envStr("SDWORKER_SD_SERVER_LISTEN_IP", &c.ListenIP)
	envStr("SDWORKER_LORA_DIR", &c.LoraModelDir)
	envStr("SDWORKER_HIRES_UPSCALERS_DIR", &c.HiresUpscalersDir)
	envStr("SDWORKER_PRELOAD_MODEL", &c.PreloadModel)
	envStr("SDWORKER_BASE_URL", &c.BaseURL)
	envStr("SDWORKER_IMAGES_DIR", &c.ImagesDir)
	envStr("SDWORKER_CORS_ORIGIN", &c.CORSOrigin)
	envStr("SDWORKER_HF_BUNDLE_AUTOFIT", &c.HFBundleAutoFit)

	// HF-токен и зеркало читаем из ОБЩИХ переменных проекта (HF_TOKEN /
	// HF_MIRROR) — так конфиг image-воркера совпадает с cppworker, и оператору
	// не нужно дублировать секреты. SDWORKER_*-варианты имеют приоритет.
	envStr("HF_TOKEN", &c.HFToken)
	envStr("SDWORKER_HF_TOKEN", &c.HFToken)
	envStr("HF_MIRROR", &c.HFMirror)
	envStr("SDWORKER_HF_MIRROR", &c.HFMirror)

	// SDWORKER_EXTRA_ARGS берём НЕ через envStr с lookaside-полем: строку
	// разбираем сразу (в JSON это массив, в env — строка, разделённая пробелами).
	var rawExtraArgs string
	envStr("SDWORKER_EXTRA_ARGS", &rawExtraArgs)

	envInts := []struct {
		key string
		dst *int
	}{
		{"SDWORKER_PORT", &c.Port},
		{"SDWORKER_SD_SERVER_PORT", &c.ServerPort},
		{"SDWORKER_STARTUP_TIMEOUT_SEC", &c.StartupTimeoutSec},
		{"SDWORKER_GENERATION_TIMEOUT_SEC", &c.GenerationTimeoutSec},
		{"SDWORKER_IDLE_UNLOAD_MINUTES", &c.IdleUnloadMinutes},
		{"SDWORKER_MAX_CONCURRENT", &c.MaxConcurrent},
	}
	for _, e := range envInts {
		v := strings.TrimSpace(os.Getenv(e.key))
		if v == "" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("%s: invalid integer %q", e.key, v)
		}
		*e.dst = n
	}
	if rawExtraArgs != "" {
		c.ExtraArgs = append(c.ExtraArgs, strings.Fields(rawExtraArgs)...)
	}
	return nil
}

// GenTimeout — таймаут ожидания терминального статуса джобы.
func (c *Config) GenTimeout() int {
	return c.GenerationTimeoutSec
}

// ImagesDirAbs — абсолютный путь каталога раздачи картинок.
func (c *Config) ImagesDirAbs() (string, error) {
	if strings.TrimSpace(c.ImagesDir) == "" {
		c.ImagesDir = "./data/images"
	}
	return filepath.Abs(c.ImagesDir)
}

// ModelsDirAbs — абсолютный путь каталога image-моделей (bundle'ов).
//
// Абсолютный путь нужен HF-обёртке: загрузчик строит локальные пути файлов
// (LocalPath в profile.json), и относительный путь стал бы невалидным при
// запуске sd-server с другим рабочим каталогом.
func (c *Config) ModelsDirAbs() (string, error) {
	if strings.TrimSpace(c.ModelsDir) == "" {
		c.ModelsDir = "./models/image"
	}
	return filepath.Abs(c.ModelsDir)
}

// DownloadsDirAbs — абсолютный путь каталога темповых .download-файлов.
func (c *Config) DownloadsDirAbs() (string, error) {
	if strings.TrimSpace(c.DownloadsDir) == "" {
		// Темп рядом с моделями: на боевых хостах ./models часто лежит на
		// большом volume, а корень контейнера — нет.
		c.DownloadsDir = filepath.Join(filepath.Dir(c.ModelsDir), "downloads")
	}
	return filepath.Abs(c.DownloadsDir)
}
