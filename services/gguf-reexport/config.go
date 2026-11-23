// config.go — конфигурация сервиса ре-экспорта из окружения.
//
// ПОЧЕМУ ВСЁ ЧЕРЕЗ ENV, А НЕ ФЛАГИ. Сервис поднимается compose-профилем, и
// единственное место, где задаётся его поведение в стенде, — `environment:` в
// deployments/docker-compose.stack.yml (значения — из deployments/.env). Флаги
// пришлось бы дублировать в `command:` и в документации; env читается и локальным
// запуском (`go run ./services/gguf-reexport`), и контейнером одинаково.
package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// config — параметры запуска. Значения по умолчанию рассчитаны на контейнер:
// каталог моделей смонтирован в /app/models, каталог задач — том /app/jobs,
// драйвер конвертера лежит в /app/py/reexport_job.py (см. docker/gguf-reexport/Dockerfile).
type config struct {
	Host string
	Port string

	// ModelsDir — корень моделей (тот же том, что у cppworker и imageworker).
	ModelsDir string
	// OutDir — второй корень для результата. Пусто = писать рядом с источником
	// внутри ModelsDir (обычный случай: воркер видит новый файл там же).
	//
	// ЗАЧЕМ ВТОРОЙ КОРЕНЬ. Тома моделей бывают только для чтения (`:ro` — так
	// стенд защищает веса от случайной записи), а результат 16 ГиБ не всегда
	// влезает на тот же диск. Тогда оператор монтирует writable-том и задаёт
	// REEXPORT_OUT_DIR: сервис проверяет, что итоговый путь лежит ВНУТРИ одного
	// из двух корней, и отказывает всему остальному.
	OutDir string
	// JobsDir — состояние задач (JSON-файл на задачу, без БД).
	JobsDir string

	// APIToken — необязательный токен на изменяющие запросы. Пусто = сервис
	// доступен только изнутри сети проекта (порт не публикуется, а проксирует
	// его nginx WebUI), и это осознанный режим по умолчанию: см.
	// docs/operations/gguf-reexport.ru.md.
	APIToken string

	PythonBin  string
	DriverPath string
	// ConverterPath — convert_hf_to_gguf.py из сабмодуля llama.cpp. Может
	// отсутствовать (образ собран без конвертера) — тогда цель hf_to_gguf
	// объявляется недоступной с причиной, а не падает в середине работы.
	ConverterPath string

	ScanDepth    int
	ScanMaxFiles int

	// InfoTTL — как часто перезапрашивать сведения о python/torch у драйвера.
	InfoTTL time.Duration
}

func loadConfig() (config, error) {
	cfg := config{
		Host:          envOr("REEXPORT_HOST", "0.0.0.0"),
		Port:          envOr("REEXPORT_PORT", "18095"),
		ModelsDir:     envOr("REEXPORT_MODELS_DIR", "/app/models"),
		OutDir:        strings.TrimSpace(os.Getenv("REEXPORT_OUT_DIR")),
		JobsDir:       envOr("REEXPORT_JOBS_DIR", "/app/jobs"),
		APIToken:      strings.TrimSpace(os.Getenv("REEXPORT_API_TOKEN")),
		PythonBin:     envOr("REEXPORT_PYTHON", "python3"),
		DriverPath:    envOr("REEXPORT_DRIVER", "/app/py/reexport_job.py"),
		ConverterPath: strings.TrimSpace(os.Getenv("REEXPORT_LLAMA_CONVERTER")),
		ScanDepth:     3,
		ScanMaxFiles:  500,
		InfoTTL:       60 * time.Second,
	}
	if v := strings.TrimSpace(os.Getenv("REEXPORT_SCAN_DEPTH")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 8 {
			return cfg, fmt.Errorf("REEXPORT_SCAN_DEPTH=%q: ожидается 1..8", v)
		}
		cfg.ScanDepth = n
	}
	if v := strings.TrimSpace(os.Getenv("REEXPORT_SCAN_MAX_FILES")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100000 {
			return cfg, fmt.Errorf("REEXPORT_SCAN_MAX_FILES=%q: ожидается 1..100000", v)
		}
		cfg.ScanMaxFiles = n
	}
	if strings.TrimSpace(cfg.ModelsDir) == "" {
		return cfg, fmt.Errorf("REEXPORT_MODELS_DIR пуст: нечего ре-экспортировать")
	}
	if cfg.OutDir != "" {
		abs, err := filepath.Abs(cfg.OutDir)
		if err != nil {
			return cfg, fmt.Errorf("REEXPORT_OUT_DIR=%q: %w", cfg.OutDir, err)
		}
		cfg.OutDir = abs
	}
	if cfg.ConverterPath == "" {
		// Рядом с драйвером: Dockerfile кладёт конвертер llama.cpp в /app/llama/.
		cfg.ConverterPath = filepath.Join(filepath.Dir(cfg.DriverPath), "..", "llama", "convert_hf_to_gguf.py")
	}
	return cfg, nil
}

// Addr — адрес прослушивания.
func (c config) Addr() string { return net.JoinHostPort(c.Host, c.Port) }

// outRoot — куда по умолчанию писать результат.
func (c config) outRoot() string {
	if c.OutDir != "" {
		return c.OutDir
	}
	return c.ModelsDir
}

func envOr(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

// fileExists — проверка «есть ли файл» без паники на каталоге.
func fileExists(path string) bool {
	if path == "" {
		return false
	}
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}
