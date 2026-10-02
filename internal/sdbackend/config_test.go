package sdbackend

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================
// Тесты конфигурации
// ============================================================

func TestDefaultConfig_Validate(t *testing.T) {
	cfg := DefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("дефолтный конфиг невалиден: %v", err)
	}
	if cfg.Port != 18093 {
		t.Fatalf("port = %d, want 18093 (types.DefaultImageWorkerPort)", cfg.Port)
	}
	if cfg.Port == cfg.ServerPort {
		t.Fatal("воркер и sd-server не могут слушать один порт")
	}
	// 64 — как max_queue_size движка: больше нельзя (двойная очередь).
	if cfg.MaxConcurrent != 64 {
		t.Fatalf("maxConcurrent = %d, want 64", cfg.MaxConcurrent)
	}
}

func TestConfig_ValidateRejectsBadValues(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"port zero", func(c *Config) { c.Port = 0 }},
		{"port too big", func(c *Config) { c.Port = 70000 }},
		{"port clash", func(c *Config) { c.ServerPort = c.Port }},
		{"empty bin", func(c *Config) { c.SDServerBin = "  " }},
		{"empty models dir", func(c *Config) { c.ModelsDir = "" }},
		{"max concurrent", func(c *Config) { c.MaxConcurrent = 0 }},
		{"negative idle", func(c *Config) { c.IdleUnloadMinutes = -1 }},
	}
	for _, c := range cases {
		cfg := DefaultConfig()
		c.mutate(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: ожидалась ошибка валидации", c.name)
		}
	}
}

func TestConfig_EnvOverrides(t *testing.T) {
	t.Setenv("SDWORKER_PORT", "19093")
	t.Setenv("SDWORKER_SD_SERVER_PORT", "19094")
	t.Setenv("SDWORKER_IMAGE_MODELS_DIR", "/tmp/image-models")
	t.Setenv("SDWORKER_SD_SERVER_BIN", "/usr/local/bin/sd-server")
	t.Setenv("SDWORKER_IDLE_UNLOAD_MINUTES", "7")
	t.Setenv("SDWORKER_MAX_CONCURRENT", "4")
	t.Setenv("SDWORKER_BASE_URL", "http://10.0.0.5:18093")
	t.Setenv("SDWORKER_EXTRA_ARGS", "--vae-tiling --threads 8")

	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv: %v", err)
	}
	if cfg.Port != 19093 || cfg.ServerPort != 19094 {
		t.Fatalf("ports = %d/%d", cfg.Port, cfg.ServerPort)
	}
	if cfg.ModelsDir != "/tmp/image-models" {
		t.Fatalf("modelsDir = %q", cfg.ModelsDir)
	}
	if cfg.SDServerBin != "/usr/local/bin/sd-server" {
		t.Fatalf("sdServerBin = %q", cfg.SDServerBin)
	}
	if cfg.IdleUnloadMinutes != 7 || cfg.MaxConcurrent != 4 {
		t.Fatalf("idle=%d concurrent=%d", cfg.IdleUnloadMinutes, cfg.MaxConcurrent)
	}
	if cfg.BaseURL != "http://10.0.0.5:18093" {
		t.Fatalf("baseUrl = %q", cfg.BaseURL)
	}
	if strings.Join(cfg.ExtraArgs, " ") != "--vae-tiling --threads 8" {
		t.Fatalf("extraArgs = %v", cfg.ExtraArgs)
	}
}

func TestConfig_EnvInvalidIntIsError(t *testing.T) {
	t.Setenv("SDWORKER_PORT", "not-a-number")
	if _, err := LoadConfigFromEnv(); err == nil {
		t.Fatal("опечатка в SDWORKER_PORT не должна молча игнорироваться")
	}
}

func TestConfig_JSONFileAndEnvPriority(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sdworker.json")
	body := map[string]any{
		"port":          18193,
		"modelsDir":     "/from/json",
		"sdServerBin":   "/from/json/sd-server",
		"idleUnloadMinutes": 15,
	}
	data, _ := json.Marshal(body)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("SDWORKER_CONFIG", path)
	// env должен победить файл.
	t.Setenv("SDWORKER_SD_SERVER_BIN", "/from/env/sd-server")

	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Port != 18193 {
		t.Fatalf("port = %d, want 18193 (из JSON)", cfg.Port)
	}
	if cfg.ModelsDir != "/from/json" {
		t.Fatalf("modelsDir = %q", cfg.ModelsDir)
	}
	if cfg.SDServerBin != "/from/env/sd-server" {
		t.Fatalf("env должен побеждать файл: %q", cfg.SDServerBin)
	}
	if cfg.IdleUnloadMinutes != 15 {
		t.Fatalf("idleUnloadMinutes = %d", cfg.IdleUnloadMinutes)
	}
}

func TestConfig_MissingJSONFileIsNotFatal(t *testing.T) {
	cfg, err := LoadConfigFromFile(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("отсутствие файла не должно быть ошибкой: %v", err)
	}
	if cfg.Port == 0 {
		t.Fatal("должны примениться дефолты")
	}
}

func TestConfig_GenTimeoutAndImagesDir(t *testing.T) {
	cfg := DefaultConfig()
	cfg.GenerationTimeoutSec = 42
	if cfg.GenTimeout() != 42 {
		t.Fatalf("GenTimeout = %d", cfg.GenTimeout())
	}
	cfg.ImagesDir = ""
	abs, err := cfg.ImagesDirAbs()
	if err != nil {
		t.Fatalf("ImagesDirAbs: %v", err)
	}
	if !filepath.IsAbs(abs) {
		t.Fatalf("ImagesDirAbs = %q, want absolute", abs)
	}
}

// HF-настройки (Phase 4): каталоги резолвятся в абсолютные пути, а токен и
// зеркало читаются из ОБЩИХ переменных проекта (HF_TOKEN/HF_MIRROR) — как в
// cppworker, чтобы оператор не дублировал секреты в двух конфигах.
func TestConfig_HFSettings(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.DownloadsDir == "" {
		t.Fatal("downloadsDir должен иметь дефолт (иначе темп .download ляжет в cwd)")
	}
	if cfg.HFBundleAutoFit != "on" {
		t.Fatalf("hfBundleAutoFit по умолчанию = %q, want on (слабые GPU)", cfg.HFBundleAutoFit)
	}
	modelsAbs, err := cfg.ModelsDirAbs()
	if err != nil {
		t.Fatalf("ModelsDirAbs: %v", err)
	}
	downloadsAbs, err := cfg.DownloadsDirAbs()
	if err != nil {
		t.Fatalf("DownloadsDirAbs: %v", err)
	}
	if !filepath.IsAbs(modelsAbs) || !filepath.IsAbs(downloadsAbs) {
		t.Fatalf("каталоги не абсолютные: %q, %q", modelsAbs, downloadsAbs)
	}
	if modelsAbs == downloadsAbs {
		t.Fatal("темп и модели не должны совпадать (orphan-cleanup удалил бы модели)")
	}

	t.Setenv("HF_TOKEN", "hf_from_env")
	t.Setenv("HF_MIRROR", "https://hf-mirror.example")
	t.Setenv("SDWORKER_DOWNLOADS_DIR", filepath.Join(t.TempDir(), "dl"))
	t.Setenv("SDWORKER_HF_BUNDLE_AUTOFIT", "off")
	env, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv: %v", err)
	}
	if env.HFToken != "hf_from_env" {
		t.Fatalf("hfToken = %q, want из HF_TOKEN", env.HFToken)
	}
	if env.HFMirror != "https://hf-mirror.example" {
		t.Fatalf("hfMirror = %q", env.HFMirror)
	}
	if env.HFBundleAutoFit != "off" {
		t.Fatalf("hfBundleAutoFit = %q, want off", env.HFBundleAutoFit)
	}
	if !strings.HasSuffix(filepath.ToSlash(env.DownloadsDir), "/dl") {
		t.Fatalf("downloadsDir = %q", env.DownloadsDir)
	}
	// SDWORKER_HF_TOKEN имеет приоритет над общей HF_TOKEN.
	t.Setenv("SDWORKER_HF_TOKEN", "hf_sdworker")
	env2, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv: %v", err)
	}
	if env2.HFToken != "hf_sdworker" {
		t.Fatalf("hfToken = %q, want SDWORKER_HF_TOKEN", env2.HFToken)
	}
}

// Некорректный hfBundleAutoFit — ошибка конфигурации, а не молчаливое
// игнорирование: иначе оператор не поймёт, почему флаг не применился.
func TestConfig_RejectsBadAutoFit(t *testing.T) {
	cfg := DefaultConfig()
	cfg.HFBundleAutoFit = "maybe"
	if err := cfg.Validate(); err == nil {
		t.Fatal("ожидалась ошибка валидации hfBundleAutoFit")
	}
	cfg.HFBundleAutoFit = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("пустое значение допустимо (флаг не передаём): %v", err)
	}
}

// listenIP — критичная защита: движок не должен торчать наружу, иначе клиенты
// обойдут нормализацию (и получат дефолтный seed 42 — «все картинки одинаковые»).
func TestListenIP_ForcesLoopback(t *testing.T) {
	for _, in := range []string{"", "0.0.0.0", "::", "[::]"} {
		if got := listenIP(in); got != "127.0.0.1" {
			t.Errorf("listenIP(%q) = %q, want 127.0.0.1", in, got)
		}
	}
	if got := listenIP("192.168.1.5"); got != "192.168.1.5" {
		t.Errorf("явный адрес должен сохраняться, got %q", got)
	}
}

func TestQueue_Semantics(t *testing.T) {
	q := NewQueue(2)
	if q.Capacity() != 2 {
		t.Fatalf("capacity = %d", q.Capacity())
	}
	r1, err := q.Acquire(nil)
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	r2, err := q.Acquire(nil)
	if err != nil {
		t.Fatalf("acquire 2: %v", err)
	}
	if q.InFlight() != 2 {
		t.Fatalf("inFlight = %d", q.InFlight())
	}
	if _, err := q.Acquire(nil); err != ErrQueueFull {
		t.Fatalf("третий acquire = %v, want ErrQueueFull", err)
	}
	if q.Rejected() != 1 {
		t.Fatalf("rejected = %d", q.Rejected())
	}
	r1()
	r1() // повторный release не должен паниковать
	if q.InFlight() != 1 {
		t.Fatalf("inFlight после release = %d", q.InFlight())
	}
	r2()
	if q.InFlight() != 0 {
		t.Fatalf("inFlight = %d", q.InFlight())
	}
}

func TestImageStore_SaveAndExt(t *testing.T) {
	dir := t.TempDir()
	store, err := NewImageStore(dir, "http://worker:18093")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	for format, ext := range map[string]string{"png": "png", "jpeg": "jpg", "webp": "webp", "": "png"} {
		url, err := store.Save(tinyPNG, format)
		if err != nil {
			t.Fatalf("save(%q): %v", format, err)
		}
		if !strings.HasPrefix(url, "http://worker:18093/images/") {
			t.Fatalf("url = %q", url)
		}
		if !strings.HasSuffix(url, "."+ext) {
			t.Fatalf("url = %q, want extension .%s", url, ext)
		}
	}
	if _, err := store.Save("", "png"); err == nil {
		t.Fatal("пустой payload должен быть ошибкой")
	}
	if _, err := store.Save("!!!not-base64!!!", "png"); err == nil {
		t.Fatal("невалидный base64 должен быть ошибкой")
	}
	// data URL тоже принимается.
	if _, err := store.Save("data:image/png;base64,"+tinyPNG, "png"); err != nil {
		t.Fatalf("data URL: %v", err)
	}
}
