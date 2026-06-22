// handlers_diagnostics.go — endpoint для runtime диагностики cppworker.
//
// Помогает диагностировать проблемы с загрузкой моделей без чтения runtime логов:
//   - Проверка файловой системы (модели, права доступа)
//   - Проверка VRAM (свободная память, лимиты)
//   - Проверка llama.cpp / CUDA инициализации
//   - Подробный статус загруженных моделей с метаданными
//   - Последние ошибки загрузки
//
// Использование:
//   GET /api/diagnostics         — полная диагностика
//   GET /api/diagnostics/models  — статус всех моделей с историей загрузок
//   GET /api/diagnostics/load?model=<name>  — попытка загрузить модель с подробным логом
//   POST /api/diagnostics/clear — очистить историю загрузок
package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/pkg/logger"
)

// loadAttemptLog — последние N попыток загрузки моделей с диагностикой.
const maxLoadAttempts = 50

var (
	loadAttemptsMu sync.RWMutex
	loadAttempts   = make([]LoadAttempt, 0, maxLoadAttempts)
)

// LoadAttempt — запись об одной попытке загрузки модели.
type LoadAttempt struct {
	Timestamp   time.Time              `json:"timestamp"`
	Model       string                 `json:"model"`
	Path        string                 `json:"path"`
	Success     bool                   `json:"success"`
	Error       string                 `json:"error,omitempty"`
	Stage       string                 `json:"stage"`
	DurationMs  int64                  `json:"durationMs"`
	Diagnostics map[string]interface{} `json:"diagnostics,omitempty"`
}

// RecordLoadAttempt записывает попытку загрузки в кольцевой буфер.
func RecordLoadAttempt(attempt LoadAttempt) {
	loadAttemptsMu.Lock()
	defer loadAttemptsMu.Unlock()
	loadAttempts = append(loadAttempts, attempt)
	if len(loadAttempts) > maxLoadAttempts {
		// Удаляем самые старые записи (FIFO).
		loadAttempts = loadAttempts[len(loadAttempts)-maxLoadAttempts:]
	}
}

// Diagnostics — полная диагностическая информация.
type Diagnostics struct {
	System      SystemInfo     `json:"system"`
	Process     ProcessInfo    `json:"process"`
	Filesystem  FilesystemInfo `json:"filesystem"`
	GPU         GPUInfo        `json:"gpu"`
	Models      []ModelStatus  `json:"models"`
	RecentLoads []LoadAttempt  `json:"recentLoads"`
	CommonIssues []CommonIssue `json:"commonIssues"`
}

// SystemInfo — информация о системе.
type SystemInfo struct {
	GOOS         string `json:"goos"`
	GOArch       string `json:"goarch"`
	NumCPU       int    `json:"numCPU"`
	NumGoroutine int    `json:"numGoroutine"`
	MemStats     string `json:"memStats"`
	Hostname     string `json:"hostname,omitempty"`
}

// ProcessInfo — информация о процессе cppworker.
type ProcessInfo struct {
	PID             int    `json:"pid"`
	UptimeSeconds   int64  `json:"uptimeSeconds"`
	Version         string `json:"version,omitempty"`
	BackendReady    bool   `json:"backendReady"`
	ModelsDir       string `json:"modelsDir"`
	GPULayers       int    `json:"gpuLayers"`
	CtxSize         int    `json:"ctxSize"`
	BatchSize       int    `json:"batchSize"`
	UseMmapEnabled  bool   `json:"useMmapEnabled"`
}

// FilesystemInfo — информация о файловой системе моделей.
type FilesystemInfo struct {
	ModelsDir     string   `json:"modelsDir"`
	ModelsDirOK   bool     `json:"modelsDirOK"`
	GGUFFileCount int      `json:"ggufFileCount"`
	GGUFFiles     []string `json:"ggufFiles,omitempty"`
	Writable      bool     `json:"writable"`
}

// GPUInfo — информация о GPU.
type GPUInfo struct {
	Count       int         `json:"count"`
	VRAMTotalMB uint64      `json:"vramTotalMB"`
	VRAMFreeMB  uint64      `json:"vramFreeMB"`
	VRAMUsedMB  uint64      `json:"vramUsedMB"`
	Devices     []GPUDevice `json:"devices,omitempty"`
}

// GPUDevice — одно GPU устройство.
type GPUDevice struct {
	Index       int    `json:"index"`
	Name        string `json:"name"`
	VRAMTotalMB uint64 `json:"vramTotalMB"`
	VRAMFreeMB  uint64 `json:"vramFreeMB"`
	VRAMUsedMB  uint64 `json:"vramUsedMB"`
}

// ModelStatus — статус одной модели.
type ModelStatus struct {
	Name             string `json:"name"`
	State            string `json:"state"`
	Path             string `json:"path"`
	Architecture     string `json:"architecture,omitempty"`
	NLayers          int    `json:"nLayers"`
	ContextSize      int    `json:"contextSize"`
	GPULayers        int    `json:"gpuLayers"`
	LoadedAt         string `json:"loadedAt,omitempty"`
	LoadingMs        int64  `json:"loadingMs,omitempty"`
	LoadingSizeBytes int64  `json:"loadingSizeBytes,omitempty"`
}

// CommonIssue — типичная проблема с рекомендацией.
type CommonIssue struct {
	ID          string `json:"id"`
	Severity    string `json:"severity"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Action      string `json:"action,omitempty"`
}

// handleDiagnostics — GET /api/diagnostics — полная runtime диагностика.
//
// Возвращает JSON со следующими секциями:
//   - system: Go runtime, ОС, hostname, текущее время
//   - process: PID, uptime, версия, состояние backend
//   - filesystem: размер modelsDir, кол-во .gguf файлов, права доступа
//   - gpu: кол-во GPU, свободная/общая VRAM
//   - models: статус каждой загруженной модели
//   - recent_loads: последние 20 попыток загрузки
//   - common_issues: автоматические рекомендации по частым проблемам
func handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	log := logger.Get()
	log.Infow("handleDiagnostics: collecting runtime info", "remote", r.RemoteAddr)
	diag := collectDiagnostics()
	writeJSON(w, http.StatusOK, diag)
}

func collectDiagnostics() Diagnostics {
	var d Diagnostics

	// System
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	d.System = SystemInfo{
		GOOS:         runtime.GOOS,
		GOArch:       runtime.GOARCH,
		NumCPU:       runtime.NumCPU(),
		NumGoroutine: runtime.NumGoroutine(),
		MemStats:     fmt.Sprintf("Alloc=%dMB Sys=%dMB NumGC=%d", m.Alloc/1024/1024, m.Sys/1024/1024, m.NumGC),
	}
	if h, err := os.Hostname(); err == nil {
		d.System.Hostname = h
	}

	// Process
	d.Process = ProcessInfo{
		PID:            os.Getpid(),
		UptimeSeconds:  int64(time.Since(startupTime).Seconds()),
		ModelsDir:      derefString(modelsDir),
		GPULayers:      derefInt(gpuLayers),
		CtxSize:        derefInt(ctxSize),
		BatchSize:      derefInt(batchSize),
		UseMmapEnabled: !derefBool(noMmap),
		BackendReady:   backend != nil,
	}
	if backend != nil {
		d.Process.Version = backend.Version()
	}

	// Filesystem
	d.Filesystem = inspectFilesystem(d.Process.ModelsDir)

	// GPU
	d.GPU = inspectGPU()

	// Models
	d.Models = inspectModels()

	// Recent loads
	loadAttemptsMu.RLock()
	n := len(loadAttempts)
	startIdx := 0
	if n > 20 {
		startIdx = n - 20
	}
	if n > 0 {
		d.RecentLoads = make([]LoadAttempt, n-startIdx)
		copy(d.RecentLoads, loadAttempts[startIdx:])
	}
	loadAttemptsMu.RUnlock()

	// Common issues
	d.CommonIssues = detectCommonIssues(d)

	return d
}

func inspectFilesystem(modelsDir string) FilesystemInfo {
	info := FilesystemInfo{ModelsDir: modelsDir}

	if modelsDir == "" {
		return info
	}

	// Проверяем существование директории
	fi, err := os.Stat(modelsDir)
	if err != nil {
		info.ModelsDirOK = false
		return info
	}
	if !fi.IsDir() {
		info.ModelsDirOK = false
		return info
	}
	info.ModelsDirOK = true

	// Проверяем права на запись
	testFile := filepath.Join(modelsDir, ".cppworker_diagnostics_test")
	if f, err := os.Create(testFile); err == nil {
		f.Close()
		os.Remove(testFile)
		info.Writable = true
	}

	// Сканируем .gguf файлы
	entries, err := os.ReadDir(modelsDir)
	if err != nil {
		return info
	}

	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".gguf") {
			info.GGUFFileCount++
			if len(info.GGUFFiles) < 50 {
				full := filepath.Join(modelsDir, e.Name())
				size := int64(-1)
				if fi, err := os.Stat(full); err == nil {
					size = fi.Size()
				}
				if size >= 0 {
					info.GGUFFiles = append(info.GGUFFiles, fmt.Sprintf("%s (%d MB)", e.Name(), size/1024/1024))
				} else {
					info.GGUFFiles = append(info.GGUFFiles, e.Name())
				}
			}
		}
	}

	return info
}

func inspectGPU() GPUInfo {
	if backend == nil {
		return GPUInfo{}
	}

	devices := backend.GetGPUDevices()

	info := GPUInfo{
		Count:   len(devices),
		Devices: make([]GPUDevice, 0, len(devices)),
	}

	for _, dev := range devices {
		gd := GPUDevice{
			Index:       dev.Index,
			Name:        dev.Name,
			VRAMTotalMB: dev.VRAMTotalMB,
			VRAMFreeMB:  dev.VRAMFreeMB,
		}
		if dev.VRAMTotalMB > dev.VRAMFreeMB {
			gd.VRAMUsedMB = dev.VRAMTotalMB - dev.VRAMFreeMB
			info.VRAMUsedMB += gd.VRAMUsedMB
		}
		info.VRAMTotalMB += dev.VRAMTotalMB
		info.VRAMFreeMB += dev.VRAMFreeMB
		info.Devices = append(info.Devices, gd)
	}

	return info
}

func inspectModels() []ModelStatus {
	if backend == nil {
		return nil
	}

	models := backend.ListModels()
	statuses := make([]ModelStatus, 0, len(models))
	for _, m := range models {
		ms := ModelStatus{
			Name:             m.Name,
			Path:             m.Path,
			Architecture:     m.Architecture,
			NLayers:          m.NLayers,
			ContextSize:      m.ContextSize,
			GPULayers:        m.GPULayers,
			LoadingSizeBytes: m.LoadingSizeBytes,
		}
		switch m.State {
		case cppbackend.StateLoaded:
			ms.State = "loaded"
			ms.LoadedAt = m.LoadedAt.Format(time.RFC3339)
		case cppbackend.StateLoading:
			ms.State = "loading"
			if !m.LoadingStartedAt.IsZero() {
				ms.LoadingMs = time.Since(m.LoadingStartedAt).Milliseconds()
			}
		case cppbackend.StateUnloaded:
			ms.State = "unloaded"
		default:
			ms.State = "unknown"
		}
		statuses = append(statuses, ms)
	}
	return statuses
}

// detectCommonIssues анализирует диагностику и возвращает список типичных проблем.
func detectCommonIssues(d Diagnostics) []CommonIssue {
	var issues []CommonIssue

	// 1. Backend не инициализирован
	if !d.Process.BackendReady {
		issues = append(issues, CommonIssue{
			ID:          "backend_not_ready",
			Severity:    "critical",
			Title:       "Backend (llama.cpp) не инициализирован",
			Description: "C++ модуль llama.cpp не загрузился. Проверьте логи startup.",
			Action:      "Проверьте что llama.cpp собран и все библиотеки доступны",
		})
	}

	// 2. ModelsDir не существует
	if !d.Filesystem.ModelsDirOK {
		issues = append(issues, CommonIssue{
			ID:          "models_dir_missing",
			Severity:    "critical",
			Title:       fmt.Sprintf("Директория моделей не найдена: %s", d.Filesystem.ModelsDir),
			Description: "CppWorker не может найти директорию с .gguf моделями.",
			Action:      "Создайте директорию и загрузите модели через /api/hf/download",
		})
	}

	// 3. Нет GPU, но есть запросы
	if d.GPU.Count == 0 && d.Process.GPULayers != 0 {
		issues = append(issues, CommonIssue{
			ID:          "gpu_unavailable_with_layers",
			Severity:    "high",
			Title:       "GPU недоступна, но настройки требуют GPU-слои",
			Description: fmt.Sprintf("Запрошены GPU-слои=%d, но GPU не обнаружена", d.Process.GPULayers),
			Action:      "Установите gpuLayers=0 для CPU-only или установите CUDA/ROCm драйверы",
		})
	}

	// 4. Мало VRAM
	if d.GPU.Count > 0 && d.GPU.VRAMFreeMB < 2048 {
		issues = append(issues, CommonIssue{
			ID:          "low_vram",
			Severity:    "high",
			Title:       fmt.Sprintf("Мало свободной VRAM: %d MB", d.GPU.VRAMFreeMB),
			Description: "Менее 2 GB свободной VRAM — модели не смогут загрузиться.",
			Action:      "Выгрузите неиспользуемые модели или уменьшите gpuLayers",
		})
	}

	// 5. Недавние ошибки загрузки
	failCount := 0
	for _, la := range d.RecentLoads {
		if !la.Success && time.Since(la.Timestamp) < 10*time.Minute {
			failCount++
		}
	}
	if failCount > 0 {
		issues = append(issues, CommonIssue{
			ID:          "recent_load_failures",
			Severity:    "high",
			Title:       fmt.Sprintf("Недавние ошибки загрузки: %d за последние 10 минут", failCount),
			Description: "Загрузка модели завершилась с ошибкой. См. секцию recentLoads.",
			Action:      "Проверьте recentLoads[i].error и recentLoads[i].diagnostics",
		})
	}

	// 6. Директория не записываемая
	if d.Filesystem.ModelsDirOK && !d.Filesystem.Writable {
		issues = append(issues, CommonIssue{
			ID:          "models_dir_not_writable",
			Severity:    "medium",
			Title:       "Директория моделей доступна только на чтение",
			Description: fmt.Sprintf("Не удалось создать тестовый файл в %s.", d.Filesystem.ModelsDir),
			Action:      "Проверьте права пользователя на запись",
		})
	}

	return issues
}

// handleDiagnosticsModels — GET /api/diagnostics/models — список моделей с подробной информацией.
func handleDiagnosticsModels(w http.ResponseWriter, r *http.Request) {
	diag := collectDiagnostics()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"models":       diag.Models,
		"recentLoads":  diag.RecentLoads,
		"gpu":          diag.GPU,
		"commonIssues": diag.CommonIssues,
	})
}

// handleDiagnosticsLoad — POST /api/diagnostics/load?model=<name>
// Попытка загрузить модель с записью подробной диагностики в recentLoads.
func handleDiagnosticsLoad(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	modelName := r.URL.Query().Get("model")
	if modelName == "" {
		http.Error(w, "model query parameter required", http.StatusBadRequest)
		return
	}

	log := logger.Get()
	startTime := time.Now()
	attempt := LoadAttempt{
		Timestamp: startTime,
		Model:     modelName,
		Stage:     "starting",
	}

	log.Infow("handleDiagnosticsLoad: attempt to load model",
		"model", modelName, "remote", r.RemoteAddr)

	// 1. Проверяем путь
	diagMap := map[string]interface{}{}
	if modelsDir == nil || *modelsDir == "" {
		diagMap["step1_modelsDir"] = "FAIL: --models-dir not set"
		attempt.Stage = "models_dir_check"
		attempt.Success = false
		attempt.Error = "--models-dir not set or empty"
		attempt.DurationMs = time.Since(startTime).Milliseconds()
		RecordLoadAttempt(attempt)
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"stage":   attempt.Stage,
			"error":   attempt.Error,
		})
		return
	}
	diagMap["step1_modelsDir"] = *modelsDir

	modelPath := filepath.Join(*modelsDir, modelName)
	if !strings.HasSuffix(modelPath, ".gguf") {
		matches, _ := filepath.Glob(modelPath + "*.gguf")
		if len(matches) > 0 {
			modelPath = matches[0]
		} else {
			modelPath += ".gguf"
		}
	}
	attempt.Path = modelPath

	// 2. Проверяем существование файла
	fi, err := os.Stat(modelPath)
	if err != nil {
		diagMap["step2_fileStat"] = fmt.Sprintf("FAIL: %v", err)
		attempt.Stage = "file_stat"
		attempt.Success = false
		attempt.Error = err.Error()
		attempt.DurationMs = time.Since(startTime).Milliseconds()
		attempt.Diagnostics = diagMap
		RecordLoadAttempt(attempt)
		writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"success":     false,
			"stage":       attempt.Stage,
			"error":       attempt.Error,
			"diagnostics": diagMap,
		})
		return
	}
	diagMap["step2_fileStat"] = fmt.Sprintf("OK: size=%d MB", fi.Size()/1024/1024)

	// 3. Проверяем загружена ли уже
	if _, err := backend.GetModel(modelName); err == nil {
		diagMap["step3_alreadyLoaded"] = "OK"
		attempt.Stage = "already_loaded"
		attempt.Success = true
		attempt.DurationMs = time.Since(startTime).Milliseconds()
		RecordLoadAttempt(attempt)
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"success":     true,
			"stage":       attempt.Stage,
			"message":     "model already loaded",
			"diagnostics": diagMap,
		})
		return
	}
	diagMap["step3_alreadyLoaded"] = "not_loaded"

	// 4. Проверяем VRAM
	var freeVRAM uint64
	for _, dev := range backend.GetGPUDevices() {
		freeVRAM += dev.VRAMFreeMB
	}
	diagMap["step4_freeVRAM_MB"] = freeVRAM

	// 5. Загружаем через ensureModelLoaded
	if err := ensureModelLoaded(modelName); err != nil {
		diagMap["step5_load"] = fmt.Sprintf("FAIL: %v", err)
		attempt.Stage = "ensure_model_loaded"
		attempt.Success = false
		attempt.Error = err.Error()
		attempt.DurationMs = time.Since(startTime).Milliseconds()
		attempt.Diagnostics = diagMap
		RecordLoadAttempt(attempt)
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success":     false,
			"stage":       attempt.Stage,
			"error":       attempt.Error,
			"diagnostics": diagMap,
		})
		return
	}
	diagMap["step5_load"] = "OK"

	// 6. Получаем метаданные
	if info, err := backend.GetModel(modelName); err == nil {
		diagMap["step6_metadata"] = map[string]interface{}{
			"architecture": info.Architecture,
			"nLayers":      info.NLayers,
			"ctxSize":      info.ContextSize,
			"gpuLayers":    info.GPULayers,
		}
	}

	attempt.Stage = "complete"
	attempt.Success = true
	attempt.DurationMs = time.Since(startTime).Milliseconds()
	attempt.Diagnostics = diagMap
	RecordLoadAttempt(attempt)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":     true,
		"durationMs":  attempt.DurationMs,
		"diagnostics": diagMap,
	})
}

// handleDiagnosticsClear — POST /api/diagnostics/clear
// Очищает лог попыток загрузки.
func handleDiagnosticsClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	loadAttemptsMu.Lock()
	loadAttempts = loadAttempts[:0]
	loadAttemptsMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"status": "cleared"})
}

// ============================================================
// Helpers
// ============================================================

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func derefBool(p *bool) bool {
	if p == nil {
		return false
	}
	return *p
}
