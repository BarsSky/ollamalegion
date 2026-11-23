// debug_bundle_r715_test.go — R715 (2026-11-24): диагностический архив
// image-воркера (дефект D3 плана docs/plans/2026-11-24-remaining-defects.md).
//
// ПРОВЕРЯЕТСЯ именно то, из-за чего шаг делался:
//   - эндпоинт отдаёт архив с логами, моделями и их состоянием, версией,
//     состоянием загрузок и конфигом (раньше балансер писал note.txt для ЛЮБОГО
//     не-cppworker бэкенда, и в архиве стенда не было ничего из этого);
//   - HF-токен в архив НЕ попадает;
//   - логи в архиве усечены ЯВНО и ограничены (2000 строк / 2 МБ);
//   - повторы логов схлопнуты (repeated=N).
package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// withAggregatingBrokerR715 — подменить глобальный брокер логов.
func withAggregatingBrokerR715(t *testing.T, size int) *logger.LogBroker {
	t.Helper()
	prev := logger.GetBroker()
	b := logger.NewAggregatingLogBroker(size)
	logger.SetBroker(b)
	t.Cleanup(func() { logger.SetBroker(prev) })
	return b
}

// bundleFilesR715 — запросить архив у воркера и разобрать его.
func bundleFilesR715(t *testing.T, app *App, url string) ([]string, map[string][]byte) {
	t.Helper()
	rec := httptest.NewRecorder()
	app.setupRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d, ожидался 200 (тело: %s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/zip") {
		t.Fatalf("Content-Type = %q, ожидался application/zip", ct)
	}
	body := rec.Body.Bytes()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("ответ не zip: %v", err)
	}
	files := map[string][]byte{}
	var names []string
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %s: %v", f.Name, err)
		}
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("read %s: %v", f.Name, err)
		}
		names = append(names, f.Name)
		files[f.Name] = data
	}
	return names, files
}

func hasBundleFileR715(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// TestR715_SdworkerBundleHasDiagnosticFiles — архив воркера содержит логи,
// модели, версию/identity, конфиг и состояние загрузок.
func TestR715_SdworkerBundleHasDiagnosticFiles(t *testing.T) {
	logs := withAggregatingBrokerR715(t, 5000)
	logs.Publish(logger.LogEntry{Time: time.Now(), Level: "info", Message: "модельный реестр загружен"})

	app, _ := newTestApp(t, map[string]types.ImageModelProfile{
		"sd15-q4": newTestProfile("sd15-q4", "sd15"),
	})

	names, files := bundleFilesR715(t, app, "/api/v1/sdworker/debug/bundle")
	for _, want := range []string{
		"info.json", "models.json", "downloads.json", "load-progress.json",
		"config.json", "env.json", "logs.txt", "README.txt",
	} {
		if !hasBundleFileR715(names, want) {
			t.Fatalf("в архиве нет %q — файлы: %v", want, names)
		}
	}

	// Модели и их состояние — главное, чего не хватало в архиве стенда.
	var models map[string]any
	if err := json.Unmarshal(files["models.json"], &models); err != nil {
		t.Fatalf("models.json не JSON: %v", err)
	}
	rawModels, _ := models["models"].([]any)
	if len(rawModels) != 1 {
		t.Fatalf("в models.json %d моделей, ожидалась 1: %s", len(rawModels), files["models.json"])
	}
	model := rawModels[0].(map[string]any)
	if model["name"] != "sd15-q4" {
		t.Fatalf("в models.json нет имени модели: %v", model)
	}
	if model["state"] == nil {
		t.Fatalf("в models.json нет состояния модели: %v", model)
	}

	// Логи процесса.
	if !strings.Contains(string(files["logs.txt"]), "модельный реестр загружен") {
		t.Fatalf("logs.txt не содержит записей буфера:\n%s", files["logs.txt"])
	}

	// Версия/ревизия и конфиг.
	if !strings.Contains(string(files["info.json"]), "pinned_sd_server_revision") {
		t.Fatalf("info.json без ревизии контракта: %s", files["info.json"])
	}
	if !strings.Contains(string(files["config.json"]), "max_concurrent") {
		t.Fatalf("config.json без конфигурации: %s", files["config.json"])
	}

	// Загрузки: HF не сконфигурирован в тестовом приложении — это сказано явно.
	if !strings.Contains(string(files["downloads.json"]), `"available"`) {
		t.Fatalf("downloads.json не отвечает на вопрос о состоянии загрузок: %s", files["downloads.json"])
	}
}

// TestR715_SdworkerBundleAlias — короткий путь /api/debug/bundle работает так же.
func TestR715_SdworkerBundleAlias(t *testing.T) {
	withAggregatingBrokerR715(t, 100)
	app, _ := newTestApp(t, map[string]types.ImageModelProfile{"m": newTestProfile("m", "sd15")})
	names, _ := bundleFilesR715(t, app, "/api/debug/bundle")
	if !hasBundleFileR715(names, "models.json") {
		t.Fatalf("алиас не отдал архив: %v", names)
	}
}

// TestR715_SdworkerBundleNoHFToken — токен HF в архив не попадает, но факт его
// наличия виден (hf_token_set).
func TestR715_SdworkerBundleNoHFToken(t *testing.T) {
	t.Setenv("SDWORKER_HF_TOKEN", "hf_super_secret_value")
	t.Setenv("HF_TOKEN", "hf_super_secret_value")
	withAggregatingBrokerR715(t, 100)
	app, _ := newTestApp(t, map[string]types.ImageModelProfile{"m": newTestProfile("m", "sd15")})
	app.svc.Config.HFToken = "hf_super_secret_value"

	_, files := bundleFilesR715(t, app, "/api/v1/sdworker/debug/bundle")
	for name, data := range files {
		if strings.Contains(string(data), "hf_super_secret_value") {
			t.Fatalf("секрет HF попал в файл %s", name)
		}
	}
	var cfg map[string]any
	if err := json.Unmarshal(files["config.json"], &cfg); err != nil {
		t.Fatalf("config.json не JSON: %v", err)
	}
	if cfg["hf_token_set"] != true {
		t.Fatalf("hf_token_set = %v, ожидалось true (факт наличия токена)", cfg["hf_token_set"])
	}
}

// TestR715_SdworkerBundleLogsCollapseRepeats — повторы схлопнуты: одна строка
// с repeated=N вместо тысяч одинаковых.
func TestR715_SdworkerBundleLogsCollapseRepeats(t *testing.T) {
	logs := withAggregatingBrokerR715(t, 5000)
	base := time.Now().Add(-5 * time.Minute)
	for i := 0; i < 500; i++ {
		logs.Publish(logger.LogEntry{
			Time: base.Add(time.Duration(i) * time.Second), Level: "warn",
			Message: "image model not loaded", Fields: map[string]interface{}{"model": "sd15-q4"},
		})
	}
	app, _ := newTestApp(t, map[string]types.ImageModelProfile{"sd15-q4": newTestProfile("sd15-q4", "sd15")})
	_, files := bundleFilesR715(t, app, "/api/v1/sdworker/debug/bundle")
	text := string(files["logs.txt"])
	if !strings.Contains(text, "repeated=500") {
		t.Fatalf("повторы не схлопнуты (нет repeated=500):\n%s", text)
	}
	if strings.Count(text, "image model not loaded") != 1 {
		t.Fatalf("строка предупреждения встречается %d раз вместо 1",
			strings.Count(text, "image model not loaded"))
	}
	if !strings.Contains(text, "first=") || !strings.Contains(text, "last=") {
		t.Fatalf("нет границ времени повтора:\n%s", text)
	}
}

// TestR715_SdworkerBundleLogsLimitIsExplicit — логи в архиве ограничены
// 2000 строками, и усечение названо в файле.
func TestR715_SdworkerBundleLogsLimitIsExplicit(t *testing.T) {
	logs := withAggregatingBrokerR715(t, 6000)
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 2500; i++ {
		logs.Publish(logger.LogEntry{
			Time: base.Add(time.Duration(i) * time.Millisecond), Level: "info", Message: "tick",
			Fields: map[string]interface{}{"request_id": "req-" + itoaR715(i)},
		})
	}
	app, _ := newTestApp(t, map[string]types.ImageModelProfile{"m": newTestProfile("m", "sd15")})
	_, files := bundleFilesR715(t, app, "/api/v1/sdworker/debug/bundle")
	text := string(files["logs.txt"])
	if !strings.Contains(text, "ОБРЕЗАНО") {
		t.Fatalf("нет пометки усечения:\n%.500s", text)
	}
	if n := strings.Count(text, " INFO "); n > debugBundleMaxLogLinesR715 {
		t.Fatalf("строк логов %d > предела %d", n, debugBundleMaxLogLinesR715)
	}
	if len(text) > debugBundleMaxLogBytesR715+4096 {
		t.Fatalf("логи %d байт превышают предел %d", len(text), debugBundleMaxLogBytesR715)
	}
}

// TestR715_SdworkerBundleSinceFilters — ?since= отбрасывает старые записи.
func TestR715_SdworkerBundleSinceFilters(t *testing.T) {
	logs := withAggregatingBrokerR715(t, 5000)
	now := time.Now()
	logs.Publish(logger.LogEntry{Time: now.Add(-3 * time.Hour), Level: "info", Message: "OLD-EVENT-715"})
	logs.Publish(logger.LogEntry{Time: now, Level: "info", Message: "NEW-EVENT-715"})
	app, _ := newTestApp(t, map[string]types.ImageModelProfile{"m": newTestProfile("m", "sd15")})
	_, files := bundleFilesR715(t, app, "/api/v1/sdworker/debug/bundle?since=1h")
	text := string(files["logs.txt"])
	if strings.Contains(text, "OLD-EVENT-715") {
		t.Fatalf("since не отфильтровал старую запись:\n%s", text)
	}
	if !strings.Contains(text, "NEW-EVENT-715") {
		t.Fatalf("since выбросил свежую запись:\n%s", text)
	}
}

// TestR715_SdworkerBundleMethodNotAllowed — архив только на чтение.
func TestR715_SdworkerBundleMethodNotAllowed(t *testing.T) {
	withAggregatingBrokerR715(t, 100)
	app, _ := newTestApp(t, map[string]types.ImageModelProfile{"m": newTestProfile("m", "sd15")})
	rec := httptest.NewRecorder()
	app.setupRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/sdworker/debug/bundle", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("HTTP %d, ожидался 405", rec.Code)
	}
}

// TestR715_InfoEndpointKeepsFields — обратная совместимость /info: общий сборщик
// не потерял прежние поля (в том числе vram).
func TestR715_InfoEndpointKeepsFields(t *testing.T) {
	withAggregatingBrokerR715(t, 100)
	app, _ := newTestApp(t, map[string]types.ImageModelProfile{"m": newTestProfile("m", "sd15")})
	rec := httptest.NewRecorder()
	app.setupRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/info", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("/info не JSON: %v", err)
	}
	for _, key := range []string{
		"version", "pinned_sd_server_revision", "port", "sd_server_bin", "sd_server_port",
		"models_dir", "images_dir", "idle_unload_minutes", "max_concurrent",
		"generation_timeout_sec", "startup_timeout_sec", "base_url", "vram",
	} {
		if _, ok := out[key]; !ok {
			t.Fatalf("/info потерял прежнее поле %q: %v", key, out)
		}
	}
}

// itoaR715 — локальная замена strconv.Itoa (тест без лишних импортов).
func itoaR715(n int) string {
	if n == 0 {
		return "0"
	}
	digits := make([]byte, 0, 12)
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
