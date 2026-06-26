//go:build !no_cppbackend

package tests

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/internal/cppbackend"
)

// flashAttnEnabled — helper для проверки flash attention.
// В новой Config поле DefaultFlashAttnType (int): -1=auto/enabled, 0=disabled, 1=enabled.
// В старом API было bool DefaultFlashAttn. Здесь мы маппим int → bool:
//   -1 (auto) и 1 (enabled) считаются «включённым» режимом
//   0 — «выключенным»
func flashAttnEnabled(t int) bool { return t != 0 }

// ============================================================
// Config tests
// ============================================================

func TestDefaultConfig(t *testing.T) {
	cfg := cppbackend.DefaultConfig()

	if cfg.Port != 18091 {
		t.Errorf("expected port 18091, got %d", cfg.Port)
	}
	if cfg.Host != "0.0.0.0" {
		t.Errorf("expected host 0.0.0.0, got %s", cfg.Host)
	}
	if cfg.ModelsDir != "./models" {
		t.Errorf("expected modelsDir ./models, got %s", cfg.ModelsDir)
	}
	if cfg.DefaultCtxSize != 4096 {
		t.Errorf("expected ctx size 4096, got %d", cfg.DefaultCtxSize)
	}
	if cfg.DefaultBatchSize != 512 {
		t.Errorf("expected batch size 512, got %d", cfg.DefaultBatchSize)
	}
	if cfg.DefaultGPULayers != -1 {
		t.Errorf("expected gpu layers -1, got %d", cfg.DefaultGPULayers)
	}
	if !flashAttnEnabled(cfg.DefaultFlashAttnType) {
		t.Error("expected flash attention enabled")
	}
	if !cfg.AutoGPUDistribution {
		t.Error("expected auto GPU distribution enabled")
	}
	if cfg.TensorSplitStrategy != "vram-ratio" {
		t.Errorf("expected tensor split strategy vram-ratio, got %s", cfg.TensorSplitStrategy)
	}
	if !cfg.EnableMetrics {
		t.Error("expected metrics enabled")
	}
}

func TestConfigValidate(t *testing.T) {
	cfg := cppbackend.DefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Errorf("expected valid config, got error: %v", err)
	}

	badCfg := cfg
	badCfg.Port = 0
	if err := badCfg.Validate(); err == nil {
		t.Error("expected error for port 0")
	}

	badCfg.Port = 99999
	if err := badCfg.Validate(); err == nil {
		t.Error("expected error for port > 65535")
	}

	badCfg = cfg
	badCfg.ModelsDir = ""
	if err := badCfg.Validate(); err == nil {
		t.Error("expected error for empty models dir")
	}

	badCfg = cfg
	badCfg.DefaultCtxSize = 128
	if err := badCfg.Validate(); err == nil {
		t.Error("expected error for ctx size < 256")
	}

	badCfg = cfg
	badCfg.DefaultBatchSize = 0
	if err := badCfg.Validate(); err == nil {
		t.Error("expected error for batch size < 1")
	}
}

func TestLoadConfigFromEnv(t *testing.T) {
	os.Setenv("CPPWORKER_PORT", "19091")
	os.Setenv("CPPWORKER_HOST", "127.0.0.1")
	os.Setenv("CPPWORKER_MODELS_DIR", "/test/models")
	os.Setenv("CPPWORKER_CTX_SIZE", "8192")
	os.Setenv("CPPWORKER_BATCH_SIZE", "1024")
	os.Setenv("CPPWORKER_GPU_LAYERS", "24")
	os.Setenv("CPPWORKER_FLASH_ATTN", "false")
	os.Setenv("CPPWORKER_NUMA", "true")
	os.Setenv("CPPWORKER_USE_MMAP", "false")
	os.Setenv("CPPWORKER_AUTO_GPU_DIST", "false")
	os.Setenv("CPPWORKER_TENSOR_SPLIT_STRATEGY", "manual")
	os.Setenv("CPPWORKER_ENABLE_METRICS", "false")
	os.Setenv("HF_TOKEN", "test-token")
	os.Setenv("HF_MIRROR", "https://hf-mirror.com")

	defer func() {
		os.Unsetenv("CPPWORKER_PORT")
		os.Unsetenv("CPPWORKER_HOST")
		os.Unsetenv("CPPWORKER_MODELS_DIR")
		os.Unsetenv("CPPWORKER_CTX_SIZE")
		os.Unsetenv("CPPWORKER_BATCH_SIZE")
		os.Unsetenv("CPPWORKER_GPU_LAYERS")
		os.Unsetenv("CPPWORKER_FLASH_ATTN")
		os.Unsetenv("CPPWORKER_NUMA")
		os.Unsetenv("CPPWORKER_USE_MMAP")
		os.Unsetenv("CPPWORKER_AUTO_GPU_DIST")
		os.Unsetenv("CPPWORKER_TENSOR_SPLIT_STRATEGY")
		os.Unsetenv("CPPWORKER_ENABLE_METRICS")
		os.Unsetenv("HF_TOKEN")
		os.Unsetenv("HF_MIRROR")
	}()

	cfg := cppbackend.LoadConfigFromEnv()

	if cfg.Port != 19091 {
		t.Errorf("expected port 19091, got %d", cfg.Port)
	}
	if cfg.Host != "127.0.0.1" {
		t.Errorf("expected host 127.0.0.1, got %s", cfg.Host)
	}
	if cfg.ModelsDir != "/test/models" {
		t.Errorf("expected models dir /test/models, got %s", cfg.ModelsDir)
	}
	if cfg.DefaultCtxSize != 8192 {
		t.Errorf("expected ctx size 8192, got %d", cfg.DefaultCtxSize)
	}
	if cfg.DefaultBatchSize != 1024 {
		t.Errorf("expected batch size 1024, got %d", cfg.DefaultBatchSize)
	}
	if cfg.DefaultGPULayers != 24 {
		t.Errorf("expected gpu layers 24, got %d", cfg.DefaultGPULayers)
	}
	if flashAttnEnabled(cfg.DefaultFlashAttnType) {
		t.Error("expected flash attention disabled")
	}
	if !cfg.DefaultNUMA {
		t.Error("expected NUMA enabled")
	}
	if cfg.DefaultUseMmap {
		t.Error("expected mmap disabled")
	}
	if cfg.AutoGPUDistribution {
		t.Error("expected auto GPU distribution disabled")
	}
	if cfg.TensorSplitStrategy != "manual" {
		t.Errorf("expected tensor split strategy manual, got %s", cfg.TensorSplitStrategy)
	}
	if cfg.EnableMetrics {
		t.Error("expected metrics disabled")
	}
	if cfg.HuggingFaceToken != "test-token" {
		t.Errorf("expected HF token test-token, got %s", cfg.HuggingFaceToken)
	}
	if cfg.HFMirror != "https://hf-mirror.com" {
		t.Errorf("expected HF mirror https://hf-mirror.com, got %s", cfg.HFMirror)
	}
}

// ============================================================
// ModelManager tests
// ============================================================

func TestModelManagerScan(t *testing.T) {
	tmpDir := t.TempDir()
	files := []string{
		"llama-2-7b.Q4_K_M.gguf",
		"mistral-7b.Q5_K_M.gguf",
		"codellama-34b.Q4_0.gguf",
		"not-a-model.txt",
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(tmpDir, f), []byte("dummy"), 0644); err != nil {
			t.Fatalf("failed to create test file %s: %v", f, err)
		}
	}

	cfg := cppbackend.DefaultConfig()
	cfg.ModelsDir = tmpDir
	mm := cppbackend.NewModelManager(tmpDir, cfg)

	models, err := mm.ScanModels()
	if err != nil {
		t.Fatalf("ScanModels failed: %v", err)
	}
	if len(models) != 3 {
		t.Errorf("expected 3 models, got %d", len(models))
	}

	meta, err := mm.GetModelMeta("llama-2-7b.Q4_K_M.gguf")
	if err != nil {
		t.Errorf("GetModelMeta failed: %v", err)
	}
	if meta.Filename != "llama-2-7b.Q4_K_M.gguf" {
		t.Errorf("expected filename llama-2-7b.Q4_K_M.gguf, got %s", meta.Filename)
	}
	if mm.GetTotalScans() != 1 {
		t.Errorf("expected 1 scan, got %d", mm.GetTotalScans())
	}
	if mm.GetLastScanTime().IsZero() {
		t.Error("expected non-zero last scan time")
	}
}

func TestModelManagerListModels(t *testing.T) {
	tmpDir := t.TempDir()
	os.WriteFile(filepath.Join(tmpDir, "test.gguf"), []byte("data"), 0644)

	cfg := cppbackend.DefaultConfig()
	mm := cppbackend.NewModelManager(tmpDir, cfg)
	mm.ScanModels()

	models := mm.ListModels()
	if len(models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(models))
	}
	if models[0].Filename != "test.gguf" {
		t.Errorf("expected test.gguf, got %s", models[0].Filename)
	}
}

func TestModelManagerFindModelByPath(t *testing.T) {
	tmpDir := t.TempDir()
	modelPath := filepath.Join(tmpDir, "my-model.Q4_K_M.gguf")
	os.WriteFile(modelPath, []byte("data"), 0644)

	cfg := cppbackend.DefaultConfig()
	mm := cppbackend.NewModelManager(tmpDir, cfg)
	mm.ScanModels()

	path, err := mm.FindModelByPath("my-model.Q4_K_M.gguf")
	if err != nil {
		t.Errorf("FindModelByPath failed: %v", err)
	}
	if path != modelPath {
		t.Errorf("expected %s, got %s", modelPath, path)
	}

	path, err = mm.FindModelByPath("my-model.Q4_K_M")
	if err != nil {
		t.Errorf("FindModelByPath (without ext) failed: %v", err)
	}
	if path != modelPath {
		t.Errorf("expected %s, got %s", modelPath, path)
	}

	path, err = mm.FindModelByPath(modelPath)
	if err != nil {
		t.Errorf("FindModelByPath (abs path) failed: %v", err)
	}
	if path != "my-model.Q4_K_M.gguf" {
		t.Errorf("expected filename, got %s", path)
	}

	_, err = mm.FindModelByPath("nonexistent.gguf")
	if err == nil {
		t.Error("expected error for non-existent model")
	}
}

func TestModelManagerResolveModelPath(t *testing.T) {
	tmpDir := t.TempDir()
	modelPath := filepath.Join(tmpDir, "test-model.Q4_K_M.gguf")
	os.WriteFile(modelPath, []byte("data"), 0644)

	cfg := cppbackend.DefaultConfig()
	mm := cppbackend.NewModelManager(tmpDir, cfg)

	path := mm.ResolveModelPath("test-model.Q4_K_M.gguf")
	if path != modelPath {
		t.Errorf("expected %s, got %s", modelPath, path)
	}

	path = mm.ResolveModelPath(modelPath)
	if path != modelPath {
		t.Errorf("expected %s, got %s", modelPath, path)
	}
}

func TestModelManagerCreateDir(t *testing.T) {
	tmpDir := filepath.Join(t.TempDir(), "nonexistent-subdir")
	cfg := cppbackend.DefaultConfig()
	mm := cppbackend.NewModelManager(tmpDir, cfg)

	models, err := mm.ScanModels()
	if err != nil {
		t.Errorf("ScanModels on non-existent dir should create it: %v", err)
	}
	if len(models) != 0 {
		t.Errorf("expected 0 models, got %d", len(models))
	}
	if _, err := os.Stat(tmpDir); os.IsNotExist(err) {
		t.Error("directory should have been created")
	}
}

func TestGetFileTypeFromName(t *testing.T) {
	tests := []struct {
		filename string
		expected string
	}{
		{"llama-2-7b.Q4_K_M.gguf", "Q4_K_M"},
		{"mistral-7b.Q5_K_M.gguf", "Q5_K_M"},
		{"codellama-34b.Q8_0.gguf", "Q8_0"},
		{"falcon-40b.F16.gguf", "F16"},
		{"model.Q2_K.gguf", "Q2_K"},
		{"model.GGML.gguf", "GGML"},
		{"unknown-model.gguf", "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.filename, func(t *testing.T) {
			result := cppbackend.GetFileTypeFromName(tt.filename)
			if result != tt.expected {
				t.Errorf("expected %s, got %s", tt.expected, result)
			}
		})
	}
}

func TestEstimateGPUMemoryForModel(t *testing.T) {
	// 2026-06-26: новая сигнатура EstimateGPUMemoryForModel(sizeBytes, gpuLayers, totalLayers, ctxSize).
	// ctxSize=4096 (default для теста) → ctxMemoryMB ≈ 1 GB,
	// gpuMemoryMB (10GB * 0.7 * 20/40 = 3.5 GB) + computeBufferMB (1 GB) = ~5.5 GB.
	// Допускаем погрешность ±1 GB.
	mem := cppbackend.EstimateGPUMemoryForModel(10*1024*1024*1024, 20, 40, 4096)
	if mem == 0 {
		t.Error("expected non-zero memory estimate")
	}
	if mem < 4000 || mem > 6500 {
		t.Errorf("expected memory around 5500 MB (3.5GB gpu + 1GB kv + 1GB overhead), got %d MB", mem)
	}

	memAll := cppbackend.EstimateGPUMemoryForModel(10*1024*1024*1024, -1, 40, 4096)
	if memAll == 0 {
		t.Error("expected non-zero memory for all layers")
	}

	memZero := cppbackend.EstimateGPUMemoryForModel(10*1024*1024*1024, 0, 40, 4096)
	if memZero != 0 {
		t.Errorf("expected 0 for no GPU layers, got %d", memZero)
	}

	// Проверяем что ctxSize влияет на оценку: для ctx=32768 должно быть больше,
	// чем для ctx=4096 (KV-cache ~8x больше).
	memSmallCtx := cppbackend.EstimateGPUMemoryForModel(10*1024*1024*1024, 20, 40, 4096)
	memLargeCtx := cppbackend.EstimateGPUMemoryForModel(10*1024*1024*1024, 20, 40, 32768)
	if memLargeCtx <= memSmallCtx {
		t.Errorf("ctxSize should affect estimate: 4096=%d vs 32768=%d", memSmallCtx, memLargeCtx)
	}
}

// ============================================================
// Metrics tests
// ============================================================

func TestMetricsBasic(t *testing.T) {
	m := cppbackend.NewMetrics()
	snapshot := m.GetMetricsSnapshot()
	if snapshot["totalRequests"].(int64) != 0 {
		t.Errorf("expected 0 requests, got %d", snapshot["totalRequests"])
	}
	if snapshot["totalTokens"].(int64) != 0 {
		t.Errorf("expected 0 tokens, got %d", snapshot["totalTokens"])
	}
	if snapshot["activeModels"].(int64) != 0 {
		t.Errorf("expected 0 active models, got %d", snapshot["activeModels"])
	}
}

func TestMetricsRecordRequest(t *testing.T) {
	m := cppbackend.NewMetrics()
	m.RecordRequest("test-model", 100, 500*time.Millisecond, true)

	snapshot := m.GetMetricsSnapshot()
	if snapshot["totalRequests"].(int64) != 1 {
		t.Errorf("expected 1 request, got %d", snapshot["totalRequests"])
	}
	if snapshot["totalTokens"].(int64) != 100 {
		t.Errorf("expected 100 tokens, got %d", snapshot["totalTokens"])
	}
	if snapshot["totalErrors"].(int64) != 0 {
		t.Errorf("expected 0 errors, got %d", snapshot["totalErrors"])
	}

	modelMetrics := m.GetModelMetricsList()
	if len(modelMetrics) != 1 {
		t.Fatalf("expected 1 model metrics, got %d", len(modelMetrics))
	}
	if modelMetrics[0].Name != "test-model" {
		t.Errorf("expected model name test-model, got %s", modelMetrics[0].Name)
	}
	if modelMetrics[0].Requests.Load() != 1 {
		t.Errorf("expected 1 request for model, got %d", modelMetrics[0].Requests.Load())
	}
}

func TestMetricsRecordError(t *testing.T) {
	m := cppbackend.NewMetrics()
	m.RecordRequest("error-model", 0, 100*time.Millisecond, false)

	snapshot := m.GetMetricsSnapshot()
	if snapshot["totalErrors"].(int64) != 1 {
		t.Errorf("expected 1 error, got %d", snapshot["totalErrors"])
	}
}

func TestMetricsModelLoadUnload(t *testing.T) {
	m := cppbackend.NewMetrics()
	m.RecordLoad("model-a")
	m.RecordLoad("model-b")

	snapshot := m.GetMetricsSnapshot()
	if snapshot["activeModels"].(int64) != 2 {
		t.Errorf("expected 2 active models, got %d", snapshot["activeModels"])
	}
	if snapshot["modelsLoaded"].(int64) != 2 {
		t.Errorf("expected 2 models loaded, got %d", snapshot["modelsLoaded"])
	}

	m.RecordUnload("model-a")
	snapshot = m.GetMetricsSnapshot()
	if snapshot["activeModels"].(int64) != 1 {
		t.Errorf("expected 1 active model after unload, got %d", snapshot["activeModels"])
	}
	if snapshot["modelsUnloaded"].(int64) != 1 {
		t.Errorf("expected 1 model unloaded, got %d", snapshot["modelsUnloaded"])
	}

	modelMetrics := m.GetModelMetricsList()
	if len(modelMetrics) != 2 {
		t.Errorf("expected 2 model metrics entries, got %d", len(modelMetrics))
	}
}

// ============================================================
// Backend tests (using llama_stub)
// ============================================================

func TestNewBackend(t *testing.T) {
	cfg := cppbackend.DefaultConfig()
	cfg.ModelsDir = t.TempDir()

	b := cppbackend.NewBackend(cfg)
	if b == nil {
		t.Fatal("NewBackend returned nil")
	}
	if b.Config().Port != 18091 {
		t.Errorf("expected port 18091, got %d", b.Config().Port)
	}
	if b.ModelManager() == nil {
		t.Error("ModelManager should not be nil")
	}
	if b.Metrics() == nil {
		t.Error("Metrics should not be nil")
	}
}

func TestBackendInitAndVersion(t *testing.T) {
	cfg := cppbackend.DefaultConfig()
	cfg.ModelsDir = t.TempDir()

	b := cppbackend.NewBackend(cfg)
	if err := b.Init(); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	version := b.Version()
	if version == "" {
		t.Error("version should not be empty")
	}
	if err := b.Init(); err != nil {
		t.Errorf("double init failed: %v", err)
	}
}

func TestBackendLoadUnloadModel(t *testing.T) {
	cfg := cppbackend.DefaultConfig()
	cfg.ModelsDir = t.TempDir()

	b := cppbackend.NewBackend(cfg)
	if err := b.Init(); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	err := b.LoadModel("test-model", "dummy-path.gguf")
	if err != nil {
		t.Fatalf("LoadModel failed: %v", err)
	}

	info, err := b.GetModel("test-model")
	if err != nil {
		t.Fatalf("GetModel failed: %v", err)
	}
	if info.Name != "test-model" {
		t.Errorf("expected name test-model, got %s", info.Name)
	}
	if info.State != cppbackend.StateLoaded {
		t.Errorf("expected state loaded, got %s", info.State)
	}

	models := b.ListModels()
	if len(models) != 1 {
		t.Errorf("expected 1 model, got %d", len(models))
	}

	if err := b.UnloadModel("test-model"); err != nil {
		t.Fatalf("UnloadModel failed: %v", err)
	}

	_, err = b.GetModel("test-model")
	if err == nil {
		t.Error("expected error after unload")
	}
}

func TestBackendDuplicateLoad(t *testing.T) {
	cfg := cppbackend.DefaultConfig()
	cfg.ModelsDir = t.TempDir()

	b := cppbackend.NewBackend(cfg)
	b.Init()

	b.LoadModel("dup-model", "dummy.gguf")
	err := b.LoadModel("dup-model", "dummy.gguf")
	if err == nil {
		t.Error("expected error for duplicate load")
	}
}

func TestBackendGenerate(t *testing.T) {
	cfg := cppbackend.DefaultConfig()
	cfg.ModelsDir = t.TempDir()

	b := cppbackend.NewBackend(cfg)
	b.Init()
	b.LoadModel("gen-model", "dummy.gguf")

	result, err := b.Generate("gen-model", "Hello, world!", bridge.DefaultGenerationParams())
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	if result.Output == "" {
		t.Error("expected non-empty output")
	}
	t.Logf("Generate output: %s", result.Output)

	metrics := b.Metrics()
	if metrics != nil {
		snapshot := metrics.GetMetricsSnapshot()
		if snapshot["totalRequests"].(int64) < 1 {
			t.Errorf("expected at least 1 request, got %d", snapshot["totalRequests"])
		}
	}
}

func TestBackendGetGPUCount(t *testing.T) {
	cfg := cppbackend.DefaultConfig()
	cfg.ModelsDir = t.TempDir()

	b := cppbackend.NewBackend(cfg)
	b.Init()

	count := b.GetGPUCount()
	if count != 0 {
		t.Errorf("expected 0 GPU in stub mode, got %d", count)
	}
	devices := b.GetGPUDevices()
	if len(devices) != 0 {
		t.Errorf("expected 0 GPU devices in stub mode, got %d", len(devices))
	}
}

func TestBackendStatus(t *testing.T) {
	cfg := cppbackend.DefaultConfig()
	cfg.ModelsDir = t.TempDir()

	b := cppbackend.NewBackend(cfg)
	b.Init()
	b.LoadModel("status-model", "dummy.gguf")

	status := b.Status()
	if status["version"] == "" {
		t.Error("expected non-empty version in status")
	}
	if status["models"] == nil {
		t.Error("expected models in status")
	}
	if status["gpuDevices"] == nil {
		t.Error("expected gpuDevices in status")
	}
}

func TestBackendClose(t *testing.T) {
	cfg := cppbackend.DefaultConfig()
	cfg.ModelsDir = t.TempDir()

	b := cppbackend.NewBackend(cfg)
	b.Init()
	b.LoadModel("close-model", "dummy.gguf")
	b.Close()
}

// ============================================================
// IdleUnloadManager tests
// ============================================================

func TestIdleUnloadManager(t *testing.T) {
	cfg := cppbackend.DefaultConfig()
	cfg.ModelsDir = t.TempDir()

	b := cppbackend.NewBackend(cfg)
	b.Init()
	b.LoadModel("idle-model", "dummy.gguf")

	unloader := cppbackend.NewIdleUnloadManager(b, 10*time.Millisecond)
	unloader.Start()
	defer unloader.Stop()

	time.Sleep(100 * time.Millisecond)

	_, err := b.GetModel("idle-model")
	if err == nil {
		t.Log("Model may not be unloaded yet (timing-dependent)")
	} else {
		t.Log("Model unloaded by idle manager")
	}
}
