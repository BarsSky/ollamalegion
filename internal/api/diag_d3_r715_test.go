//go:build llama_stub

// diag_d3_r715_test.go — R715 (2026-11-24): дефект D3 плана
// docs/plans/2026-11-24-remaining-defects.md.
//
// ТРИ ЧАСТИ ОДНОГО ДЕФЕКТА, каждая проверяется здесь:
//  1. архив стенда для image-бэкенда содержит файлы ВОРКЕРА (логи, модели,
//     версию), а не только note.txt; недоступный воркер даёт ПРИЧИНУ;
//  2. summary.json содержит версии всех сервисов (services.webui и т.д.);
//  3. повторы логов схлопнуты (repeated/first/last), а старый формат ответа
//     не сломан.
//
// Тесты краснеют на состоянии 06168892: там для типа != llama_cpp писался
// только note.txt, секции services не было вовсе, а брокер не схлопывал
// повторы.
package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// ---------------------------------------------------------------- helpers ---

// d3ServerR715 — сервер диагностики с пустым прокси (без фоновых петель).
func d3ServerR715(t *testing.T) (*Server, *balancer.Proxy) {
	t.Helper()
	t.Setenv("LB_USERS_FILE", filepath.Join(t.TempDir(), "users.json"))
	t.Setenv("LB_SESSIONS_FILE", filepath.Join(t.TempDir(), "sessions.json"))
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "127.0.0.1", Port: 8080, APIPort: 8081},
	}
	p := balancer.NewProxy(cfg)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = p.Shutdown(ctx)
	})
	return NewServer(p, cfg, nil), p
}

// d3WorkerZipR715 — тот самый архив, который отдаёт image-воркер (R715).
func d3WorkerZipR715(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		f, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := f.Write([]byte(body)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// d3FakeImageWorkerR715 — воркер с /api/info и архивом; bundleStatus позволяет
// сыграть «в образе нет эндпоинта» (404).
func d3FakeImageWorkerR715(t *testing.T, bundleStatus int, workerZip []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":"image-r715","pinned_sd_server_revision":"r715"}`))
	})
	mux.HandleFunc("/api/v1/sdworker/debug/bundle", func(w http.ResponseWriter, r *http.Request) {
		if bundleStatus != http.StatusOK {
			http.Error(w, "endpoint not found in this image", bundleStatus)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(workerZip)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// d3PortOf — порт httptest-сервера (для ImagePort бэкенда).
func d3PortOf(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	u := srv.URL[len("http://"):]
	_, portStr, err := net.SplitHostPort(u)
	if err != nil {
		t.Fatalf("split %q: %v", srv.URL, err)
	}
	port := 0
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		t.Fatalf("port %q: %v", portStr, err)
	}
	return port
}

// d3ImageBackendR715 — image-бэкенд, указывающий на локальный порт.
func d3ImageBackendR715(id string, port int) types.Backend {
	return types.Backend{
		ID: id, Name: id, Host: "127.0.0.1", ImagePort: port, OllamaPort: port,
		Type: types.BackendTypeImage, Status: types.StatusHealthy,
		WorkerVersion: "image-r715", WorkerCommit: "imgc0de", WorkerBuildDate: "2026-11-24",
	}
}

// d3BundleFilesR715 — прочитать архив из тела ответа.
func d3BundleFilesR715(t *testing.T, body []byte) (names []string, files map[string][]byte) {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("ответ не zip: %v", err)
	}
	files = map[string][]byte{}
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

func d3HasFileR715(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// d3CollectBundleR715 — собрать архив через настоящий хендлер.
func d3CollectBundleR715(t *testing.T, s *Server, url string) []byte {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleDiagBundle(rec, httptest.NewRequest(http.MethodGet, url, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("handleDiagBundle: HTTP %d", rec.Code)
	}
	return rec.Body.Bytes()
}

// ------------------------------------------- часть 1: архив image-воркера ---

// TestR715_BundleForImageBackendHasWorkerFiles — ГЛАВНЫЙ ТЕСТ ДЕФЕКТА:
// для image_cpp в архиве теперь файлы ВОРКЕРА (лог, модели, версия), а не
// только note.txt «бандл недоступен».
func TestR715_BundleForImageBackendHasWorkerFiles(t *testing.T) {
	workerZip := d3WorkerZipR715(t, map[string]string{
		"logs.txt":    "2026-11-24T10:00:00Z INFO imageworker модельный реестр загружен\n",
		"models.json": `{"models":[{"name":"sd15-q4"}],"state":"not_loaded"}`,
		"info.json":   `{"version":"image-r715","pinned_sd_server_revision":"r715"}`,
		"config.json": `{"port":18093,"max_concurrent":1}`,
	})
	worker := d3FakeImageWorkerR715(t, http.StatusOK, workerZip)

	s, p := d3ServerR715(t)
	if err := p.AddBackend(d3ImageBackendR715("img-1", d3PortOf(t, worker))); err != nil {
		t.Fatalf("AddBackend: %v", err)
	}

	names, files := d3BundleFilesR715(t, d3CollectBundleR715(t, s, "/api/v1/diag/bundle"))

	if !d3HasFileR715(names, "backends/img-1.bundle.zip") {
		t.Fatalf("в архиве нет backends/img-1.bundle.zip (image-воркер) — файлы: %v", names)
	}
	if d3HasFileR715(names, "backends/img-1.note.txt") {
		t.Fatalf("остался note.txt «бандл недоступен» — ровно дефект D3: %v", names)
	}
	if !d3HasFileR715(names, "backends/img-1.info.json") {
		t.Fatalf("в архиве нет /api/info image-воркера: %v", names)
	}

	nested, nestedFiles := d3BundleFilesR715(t, files["backends/img-1.bundle.zip"])
	for _, want := range []string{"logs.txt", "models.json", "info.json", "config.json"} {
		if !d3HasFileR715(nested, want) {
			t.Fatalf("в архиве воркера нет %s — файлы: %v", want, nested)
		}
	}
	if !strings.Contains(string(nestedFiles["logs.txt"]), "модельный реестр загружен") {
		t.Fatalf("логи воркера пусты или не те: %q", nestedFiles["logs.txt"])
	}
	if !strings.Contains(string(nestedFiles["models.json"]), "sd15-q4") {
		t.Fatalf("список моделей воркера не доехал: %q", nestedFiles["models.json"])
	}
}

// TestR715_BundleWorkerDownWritesReason — недоступный воркер: в архиве ПРИЧИНА
// (адрес, тип, таймаут/соединение), а не общий note.
func TestR715_BundleWorkerDownWritesReason(t *testing.T) {
	// Свободный порт, который никто не слушает: соединение будет отвергнуто.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	s, p := d3ServerR715(t)
	if err := p.AddBackend(d3ImageBackendR715("img-down", port)); err != nil {
		t.Fatalf("AddBackend: %v", err)
	}

	names, files := d3BundleFilesR715(t, d3CollectBundleR715(t, s, "/api/v1/diag/bundle"))
	errName := "backends/img-down.error.txt"
	if !d3HasFileR715(names, errName) {
		t.Fatalf("нет %s: причина недоступности воркера не записана (файлы: %v)", errName, names)
	}
	text := string(files[errName])
	for _, want := range []string{
		"/api/v1/sdworker/debug/bundle",
		"backend_type=image_cpp",
		"reason=connection",
		"hint=",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("в причине нет %q:\n%s", want, text)
		}
	}
	if d3HasFileR715(names, "backends/img-down.note.txt") {
		t.Fatalf("для image_cpp всё ещё пишется note.txt вместо причины: %v", names)
	}
}

// TestR715_BundleImageInfoUsesImagePath — у image-воркера /api/info называется
// /api/image/info; архив не должен писать «воркер не ответил» на живой воркер.
func TestR715_BundleImageInfoUsesImagePath(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/image/info", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":"image-r715","pinned_sd_server_revision":"r715"}`))
	})
	mux.HandleFunc("/api/v1/sdworker/debug/bundle", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(d3WorkerZipR715(t, map[string]string{"logs.txt": "x\n"}))
	})
	worker := httptest.NewServer(mux)
	defer worker.Close()

	s, p := d3ServerR715(t)
	if err := p.AddBackend(d3ImageBackendR715("img-info", d3PortOf(t, worker))); err != nil {
		t.Fatalf("AddBackend: %v", err)
	}
	names, files := d3BundleFilesR715(t, d3CollectBundleR715(t, s, "/api/v1/diag/bundle"))
	if !d3HasFileR715(names, "backends/img-info.info.json") {
		t.Fatalf("нет backends/img-info.info.json — путь /api/image/info не использован (файлы: %v)", names)
	}
	if !strings.Contains(string(files["backends/img-info.info.json"]), "image-r715") {
		t.Fatalf("/api/image/info не доехал: %s", files["backends/img-info.info.json"])
	}
}

// TestR715_BundleOldImage404Explained — в образе воркера нет эндпоинта: причина
// обязана называть 404 и подсказывать «образ старше R715», иначе оператор снова
// ищет дефект в модели.
func TestR715_BundleOldImage404Explained(t *testing.T) {
	worker := d3FakeImageWorkerR715(t, http.StatusNotFound, nil)
	s, p := d3ServerR715(t)
	if err := p.AddBackend(d3ImageBackendR715("img-old", d3PortOf(t, worker))); err != nil {
		t.Fatalf("AddBackend: %v", err)
	}
	names, files := d3BundleFilesR715(t, d3CollectBundleR715(t, s, "/api/v1/diag/bundle"))
	errName := "backends/img-old.error.txt"
	if !d3HasFileR715(names, errName) {
		t.Fatalf("нет %s (файлы: %v)", errName, names)
	}
	text := string(files[errName])
	if !strings.Contains(text, "reason=http_status") || !strings.Contains(text, "404") {
		t.Fatalf("причина не называет HTTP-код:\n%s", text)
	}
	if !strings.Contains(text, "R715") {
		t.Fatalf("нет подсказки «образ старше R715»:\n%s", text)
	}
}

// TestR715_BundleCppWorkerStillWorks — путь llama_cpp не сломан: архив
// забирается с прежнего адреса /api/v1/cppworker/debug/bundle.
func TestR715_BundleCppWorkerStillWorks(t *testing.T) {
	workerZip := d3WorkerZipR715(t, map[string]string{"logs.txt": "cppworker log\n", "info.json": `{"version":"gpu-r715"}`})
	mux := http.NewServeMux()
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":"gpu-r715"}`))
	})
	mux.HandleFunc("/api/v1/cppworker/debug/bundle", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(workerZip)
	})
	worker := httptest.NewServer(mux)
	defer worker.Close()

	s, p := d3ServerR715(t)
	b := types.Backend{
		ID: "cpp-1", Name: "cpp-1", Host: "127.0.0.1", CppWorkerPort: d3PortOf(t, worker),
		Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
	}
	if err := p.AddBackend(b); err != nil {
		t.Fatalf("AddBackend: %v", err)
	}
	names, files := d3BundleFilesR715(t, d3CollectBundleR715(t, s, "/api/v1/diag/bundle"))
	if !d3HasFileR715(names, "backends/cpp-1.bundle.zip") {
		t.Fatalf("пропал архив cppworker: %v", names)
	}
	nested, _ := d3BundleFilesR715(t, files["backends/cpp-1.bundle.zip"])
	if !d3HasFileR715(nested, "logs.txt") {
		t.Fatalf("в архиве cppworker нет logs.txt: %v", nested)
	}
}

// TestR715_BundleOllamaNoteIsSpecific — у типа без архива по контракту note
// остаётся, но называет контракт, а не «бандл недоступен».
func TestR715_BundleOllamaNoteIsSpecific(t *testing.T) {
	s, p := d3ServerR715(t)
	if err := p.AddBackend(types.Backend{
		ID: "ollama-1", Name: "ollama-1", Host: "127.0.0.1", OllamaPort: 11434,
		Type: types.BackendTypeOllama, Status: types.StatusHealthy,
	}); err != nil {
		t.Fatalf("AddBackend: %v", err)
	}
	names, files := d3BundleFilesR715(t, d3CollectBundleR715(t, s, "/api/v1/diag/bundle"))
	noteName := "backends/ollama-1.note.txt"
	if !d3HasFileR715(names, noteName) {
		t.Fatalf("нет %s (файлы: %v)", noteName, names)
	}
	text := string(files[noteName])
	if !strings.Contains(text, "контракту типа ollama") || !strings.Contains(text, "contract=") {
		t.Fatalf("note не называет причину/контракт:\n%s", text)
	}
}

// ------------------------------- часть 2: версии сервисов в summary.json ---

// TestR715_SummaryHasServicesVersions — summary.json показывает версии ВСЕХ
// сервисов: балансер, webui (по env-тегу), воркеры и агенты.
func TestR715_SummaryHasServicesVersions(t *testing.T) {
	t.Setenv("WEBUI_TAG", "r715-ui")
	t.Setenv("WEBUI_VERSION", "0.7.99")
	t.Setenv("WEBUI_COMMIT", "abc1234")

	s, p := d3ServerR715(t)
	if err := p.AddBackend(d3ImageBackendR715("img-1", 18093)); err != nil {
		t.Fatalf("AddBackend image: %v", err)
	}
	if err := p.AddBackend(types.Backend{
		ID: "cpp-1", Name: "cpp-1", Host: "127.0.0.1", CppWorkerPort: 18092,
		Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
		WorkerVersion: "gpu-r715", WorkerCommit: "c0ffee1", WorkerBuildDate: "2026-11-24",
		WorkerRPCProto: "1.2.3", HasAgent: true, AgentID: "agent-1",
		AgentVersion: "agent-r715", AgentCommit: "a9e9e9",
	}); err != nil {
		t.Fatalf("AddBackend cpp: %v", err)
	}

	payload := s.diagSummaryPayloadR476()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("summary не сериализуется: %v", err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("summary не разбирается обратно: %v", err)
	}
	services, ok := decoded["services"].(map[string]interface{})
	if !ok {
		t.Fatalf("в summary нет секции services: %v", decoded)
	}
	balancerSvc, ok := services["balancer"].(map[string]interface{})
	if !ok || balancerSvc["service"] == nil {
		t.Fatalf("services.balancer нет или пуст: %v", services["balancer"])
	}
	webui, ok := services["webui"].(map[string]interface{})
	if !ok {
		t.Fatalf("services.webui нет: %v", services)
	}
	if webui["tag"] != "r715-ui" {
		t.Errorf("services.webui.tag = %v, ожидался env-тег r715-ui", webui["tag"])
	}
	if webui["commit"] != "abc1234" {
		t.Errorf("services.webui.commit = %v, ожидался abc1234", webui["commit"])
	}
	if webui["source"] != "env" {
		t.Errorf("services.webui.source = %v, ожидался env", webui["source"])
	}

	imageWorkers, ok := services["imageworker"].([]interface{})
	if !ok || len(imageWorkers) != 1 {
		t.Fatalf("services.imageworker = %v, ожидался 1 воркер", services["imageworker"])
	}
	img := imageWorkers[0].(map[string]interface{})
	if img["version"] != "image-r715" || img["commit"] != "imgc0de" {
		t.Errorf("версия image-воркера потеряна: %v", img)
	}

	cppWorkers, ok := services["cppworker"].([]interface{})
	if !ok || len(cppWorkers) != 1 {
		t.Fatalf("services.cppworker = %v, ожидался 1 воркер", services["cppworker"])
	}
	cpp := cppWorkers[0].(map[string]interface{})
	if cpp["version"] != "gpu-r715" || cpp["rpc_proto"] != "1.2.3" {
		t.Errorf("версия cppworker потеряна: %v", cpp)
	}

	agents, ok := services["agent"].([]interface{})
	if !ok || len(agents) != 1 {
		t.Fatalf("services.agent = %v, ожидался 1 агент", services["agent"])
	}
	agent := agents[0].(map[string]interface{})
	if agent["version"] != "agent-r715" || agent["commit"] != "a9e9e9" {
		t.Errorf("версия агента потеряна: %v", agent)
	}
}

// TestR715_SummaryWebUIMissingIsExplained — если версии webui взять неоткуда,
// это сказано (source=not_configured + note), а не пустой объект.
func TestR715_SummaryWebUIMissingIsExplained(t *testing.T) {
	for _, key := range webuiEnvKeysR715 {
		t.Setenv(key, "")
	}
	t.Setenv("WEBUI_IDENTITY_FILE", "")
	s, _ := d3ServerR715(t)
	services := s.diagServicesR715()
	webui, ok := services["webui"].(map[string]interface{})
	if !ok {
		t.Fatalf("services.webui нет: %v", services)
	}
	if webui["source"] != "not_configured" {
		t.Fatalf("source = %v, ожидался not_configured", webui["source"])
	}
	if note, _ := webui["note"].(string); !strings.Contains(note, "WEBUI") {
		t.Fatalf("нет объяснения, откуда взять версию webui: %v", webui)
	}
}

// TestR715_SummaryWebUIFromIdentityFile — файл идентичности образа webui
// авторитетнее env: это ФАКТ сборки.
func TestR715_SummaryWebUIFromIdentityFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "webui-identity.json")
	if err := os.WriteFile(path, []byte(`{"service":"webui","version":"0.7.99","image_tag":"r715-file","commit":"file111","build_date":"2026-11-24"}`), 0o644); err != nil {
		t.Fatalf("write identity: %v", err)
	}
	t.Setenv("WEBUI_IDENTITY_FILE", path)
	t.Setenv("WEBUI_TAG", "r715-env-should-lose")
	s, _ := d3ServerR715(t)
	webui := s.diagServicesR715()["webui"].(map[string]interface{})
	if webui["source"] != "identity_file" {
		t.Fatalf("source = %v, ожидался identity_file", webui["source"])
	}
	if webui["tag"] != "r715-file" || webui["commit"] != "file111" {
		t.Fatalf("версия из файла не взята: %v", webui)
	}
	if webui["env_hints"] == nil {
		t.Fatalf("env-подсказки не сохранены рядом: %v", webui)
	}
}

// TestR715_SummaryOldFieldsNotBroken — секция services ДОБАВЛЕНА: прежние поля
// на месте (обратная совместимость /api/v1/diag/summary).
func TestR715_SummaryOldFieldsNotBroken(t *testing.T) {
	s, _ := d3ServerR715(t)
	payload := s.diagSummaryPayloadR476()
	for _, key := range []string{
		"generated_at", "version", "version_text", "flags",
		"nctx_reload", "nctx_reload_effective", "nctx_reload_note",
		"operating_mode", "cluster", "queue_stats", "nctx_ceiling",
		"model_profiles", "recent_failures", "services",
	} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("в сводке пропало прежнее поле %q", key)
		}
	}
	if _, err := json.Marshal(payload); err != nil {
		t.Fatalf("сводка не сериализуется: %v", err)
	}
}

// ----------------------------- часть 3: схлопывание повторов в архиве ------

// d3WithAggregatingBrokerR715 — подменить глобальный брокер агрегирующим.
func d3WithAggregatingBrokerR715(t *testing.T, size int) *logger.LogBroker {
	t.Helper()
	prev := logger.GetBroker()
	b := logger.NewAggregatingLogBroker(size)
	logger.SetBroker(b)
	t.Cleanup(func() { logger.SetBroker(prev) })
	return b
}

// TestR715_BundleLogsCollapseRepeats — 1565 одинаковых WARN в архиве — одна
// строка с repeated=N и границами времени (плюс прежние поля строки).
func TestR715_WithAggregatingLogsCollapseRepeats(t *testing.T) {
	b := d3WithAggregatingBrokerR715(t, 5000)
	base := time.Now().Add(-10 * time.Minute)
	for i := 0; i < 1565; i++ {
		b.Publish(logger.LogEntry{
			Time: base.Add(time.Duration(i) * 100 * time.Millisecond), Level: "warn",
			Message: "backend disk space critical, blocking",
			Fields: map[string]interface{}{
				"backend": "A10-126", "disk_free_mb": 9480 + i%9, "min_required_mb": 10240,
			},
		})
	}
	b.Publish(logger.LogEntry{Time: time.Now(), Level: "info", Message: "request trace",
		Fields: map[string]interface{}{"request_id": "req-1", "path": "/api/chat"}})

	snap := diagBundleBalancerLogsSnapshotR715(time.Time{})
	text := snap.Text
	if !strings.Contains(text, "repeated=1565") {
		t.Fatalf("в тексте логов нет repeated=1565:\n%s", text)
	}
	if strings.Count(text, "backend disk space critical") != 1 {
		t.Fatalf("повторы не схлопнуты: строка WARN встречается %d раз",
			strings.Count(text, "backend disk space critical"))
	}
	if !strings.Contains(text, "first=") || !strings.Contains(text, "last=") {
		t.Fatalf("нет границ времени повтора:\n%s", text)
	}
	// Прежние поля строки не тронуты.
	if !strings.Contains(text, "WARN") || !strings.Contains(text, "backend=A10-126") {
		t.Fatalf("прежние поля строки потеряны:\n%s", text)
	}

	// JSON: массив того же буфера, новые поля внутри элементов.
	var entries []logger.LogEntry
	if err := json.Unmarshal([]byte(snap.JSONText()), &entries); err != nil {
		t.Fatalf("balancer-logs.json не массив: %v\n%s", err, snap.JSONText())
	}
	if len(entries) != 2 {
		t.Fatalf("записей в JSON %d, ожидалось 2 (WARN + request trace)", len(entries))
	}
	var agg *logger.LogEntry
	for i := range entries {
		if entries[i].Message == "backend disk space critical, blocking" {
			agg = &entries[i]
		}
	}
	if agg == nil {
		t.Fatalf("схлопнутой записи нет в JSON: %+v", entries)
	}
	if agg.Repeated != 1565 || agg.FirstTime == nil || agg.LastTime == nil {
		t.Fatalf("в JSON нет repeated/first_time/last_time: %+v", *agg)
	}
	if agg.Level != "warn" || agg.Time.IsZero() {
		t.Fatalf("прежние поля JSON (level/time) потеряны: %+v", *agg)
	}
}

// TestR715_BundleLogsMetaShowsWindow — logs-meta.json называет окно буфера и
// счётчики схлопывания: «13 минут» больше не узнаётся постфактум.
func TestR715_BundleLogsMetaShowsWindow(t *testing.T) {
	b := d3WithAggregatingBrokerR715(t, 5000)
	base := time.Now().Add(-90 * time.Minute)
	for i := 0; i < 200; i++ {
		b.Publish(logger.LogEntry{Time: base.Add(time.Duration(i) * 30 * time.Second),
			Level: "info", Message: "poll", Fields: map[string]interface{}{"request_id": fmt.Sprintf("p-%d", i)}})
	}
	snap := diagBundleBalancerLogsSnapshotR715(time.Time{})
	meta := snap.Meta
	if meta["aggregating"] != true {
		t.Fatalf("meta.aggregating = %v, ожидалось true", meta["aggregating"])
	}
	window, _ := meta["window_seconds"].(int64)
	if window < 60*60 {
		t.Fatalf("window_seconds = %v, ожидалось ≥ 3600 (окно часов, а не 13 минут)", meta["window_seconds"])
	}
	if meta["published_total"] == nil || meta["entries_in_buffer"] == nil {
		t.Fatalf("в meta нет счётчиков: %v", meta)
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("meta не сериализуется: %v", err)
	}
	for _, want := range []string{"window_seconds", "published_total", "collapsed_total", "entries_in_buffer"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("в logs-meta.json нет %q: %s", want, raw)
		}
	}
}

// TestR715_BundleFilesIncludeLogsArtifacts — имена файлов архива: прежние на
// месте, новые добавлены.
func TestR715_BundleFilesIncludeLogsArtifacts(t *testing.T) {
	d3WithAggregatingBrokerR715(t, 100)
	logger.GetBroker().Publish(logger.LogEntry{Time: time.Now(), Level: "info", Message: "hello"})
	s, _ := d3ServerR715(t)
	names, _ := d3BundleFilesR715(t, d3CollectBundleR715(t, s, "/api/v1/diag/bundle"))
	for _, want := range []string{
		"summary.json", "flags.txt", "balancer-logs.txt", "failures.json", "README.txt",
		"balancer-logs.json", "logs-meta.json",
	} {
		if !d3HasFileR715(names, want) {
			t.Fatalf("в архиве нет %q — файлы: %v", want, names)
		}
	}
}

// TestR715_BundleLogsTruncationIsExplicit — усечение логов в архиве называется,
// а не выглядит как «логов больше не было».
func TestR715_BundleLogsTruncationIsExplicit(t *testing.T) {
	b := d3WithAggregatingBrokerR715(t, 7000)
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 6500; i++ {
		b.Publish(logger.LogEntry{Time: base.Add(time.Duration(i) * time.Millisecond),
			Level: "info", Message: "line", Fields: map[string]interface{}{"request_id": fmt.Sprintf("r-%d", i)}})
	}
	snap := diagBundleBalancerLogsSnapshotR715(time.Time{})
	if !strings.Contains(snap.Text, "ОБРЕЗАНО") {
		t.Fatalf("нет пометки усечения при 6500 записях (предел %d):\n%s",
			diagBundleLogsMaxEntriesR715, snap.Text)
	}
	if len(snap.Entries) > diagBundleLogsMaxEntriesR715 {
		t.Fatalf("в JSON %d записей, предел %d", len(snap.Entries), diagBundleLogsMaxEntriesR715)
	}
	if len(snap.Text) > diagBundleLogsMaxBytesR715+4096 {
		t.Fatalf("текст логов %d байт превышает предел %d", len(snap.Text), diagBundleLogsMaxBytesR715)
	}
}

// TestR715_ExportTextShowsRepeats — выгрузка логов (/api/v1/logs/export)
// показывает счётчик повтора и не теряет прежние поля строки.
func TestR715_ExportTextShowsRepeats(t *testing.T) {
	b := d3WithAggregatingBrokerR715(t, 100)
	base := time.Now().Add(-time.Minute)
	for i := 0; i < 7; i++ {
		b.Publish(logger.LogEntry{Time: base.Add(time.Duration(i) * time.Second),
			Level: "warn", Message: "backend disk space critical, blocking",
			Fields: map[string]interface{}{"backend": "A10-126"}})
	}
	srv := &Server{}
	rec := httptest.NewRecorder()
	srv.handleLogsExport(rec, httptest.NewRequest(http.MethodGet, "/api/v1/logs/export", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "repeated=7") {
		t.Fatalf("в выгрузке нет repeated=7:\n%s", body)
	}
	if !strings.Contains(body, "WARN") || !strings.Contains(body, "backend=A10-126") {
		t.Fatalf("прежние поля строки потеряны:\n%s", body)
	}
	if got := rec.Header().Get("X-Logs-Exported"); got != "1" {
		t.Fatalf("X-Logs-Exported = %q, ожидалось 1 (одна схлопнутая запись)", got)
	}
}

// TestR715_ExportJSONKeepsOldShape — JSON-выгрузка остаётся МАССИВОМ записей с
// прежними ключами; новые ключи появляются только у схлопнутых записей.
func TestR715_ExportJSONKeepsOldShape(t *testing.T) {
	b := d3WithAggregatingBrokerR715(t, 100)
	base := time.Now().Add(-time.Minute)
	for i := 0; i < 3; i++ {
		b.Publish(logger.LogEntry{Time: base.Add(time.Duration(i) * time.Second),
			Level: "error", Message: "upstream error", Fields: map[string]interface{}{"path": "/api/chat"}})
	}
	b.Publish(logger.LogEntry{Time: time.Now(), Level: "info", Message: "single event"})

	srv := &Server{}
	rec := httptest.NewRecorder()
	srv.handleLogsExport(rec, httptest.NewRequest(http.MethodGet, "/api/v1/logs/export?format=json", nil))

	var raw []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("тело не JSON-массив: %v\n%s", err, rec.Body.String())
	}
	if len(raw) != 2 {
		t.Fatalf("записей %d, ожидалось 2", len(raw))
	}
	for _, key := range []string{"time", "level", "message", "fields"} {
		if _, ok := raw[0][key]; !ok {
			t.Fatalf("в JSON-записи пропало прежнее поле %q: %v", key, raw[0])
		}
	}
	if raw[0]["repeated"] != float64(3) {
		t.Fatalf("repeated = %v, ожидалось 3", raw[0]["repeated"])
	}
	if _, ok := raw[0]["first_time"]; !ok {
		t.Fatalf("нет first_time: %v", raw[0])
	}
	if _, ok := raw[0]["last_time"]; !ok {
		t.Fatalf("нет last_time: %v", raw[0])
	}
	// Одиночная запись не приобретает полей повтора (совместимость).
	if _, ok := raw[1]["repeated"]; ok {
		t.Fatalf("у одиночной записи появился repeated: %v", raw[1])
	}
}
