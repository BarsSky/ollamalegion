package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// profile_apply_reload_fallback_r517_test.go — R517 (2026-11-19): «применить
// профиль» на узле, где модель ЕЩЁ НЕ ЗАГРУЖЕНА, должно ГРУЗИТЬ её, а не
// отделываться «skipped».
//
// ЖИВОЙ СЦЕНАРИЙ (следствие R516: страница теперь разрешает выбрать узел, где
// файл модели лежит на диске). Балансер отправляет POST /api/models/reload, а
// перезагружать нечего — воркер отвечает 404 «model not currently loaded». До
// R517 это превращалось в results[status=skipped] («модели нет на этом бэкенде
// — не ошибка»), оператор видел «применено», и модель так и не поднималась.
//
// Правило взято не с потолка: тот же fallback уже есть в
// internal/balancer/nctx_reload.go:1379-1404 (reload 404 → /api/models/load).
// Здесь он доведён до пути применения профиля.
//
// Тест держит: (1) 404 «not currently loaded» → загрузка и статус loaded;
// (2) тело загрузки — ТО ЖЕ, что у reload, но без `force` (у /api/models/load
// такого поля нет, строгий декодер воркера ответил бы 400);
// (3) чужие 404 (например несуществующий маршрут) НЕ маскируются загрузкой;
// (4) 5xx по-прежнему ошибка.

// r517Server — сервер с одним llama.cpp-бэкендом и подставным воркером.
type r517Upstream struct {
	reloadStatus int
	reloadBody   string
	loadStatus   int
	loadBody     string
	reloadCalls  int
	loadCalls    int
	loadQuery    url.Values
	loadPayload  map[string]interface{}
}

func (u *r517Upstream) start(t *testing.T) types.Backend {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/reload":
			u.reloadCalls++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(u.reloadStatus)
			_, _ = w.Write([]byte(u.reloadBody))
		case "/api/models/load":
			u.loadCalls++
			u.loadQuery = r.URL.Query()
			raw, _ := io.ReadAll(r.Body)
			u.loadPayload = map[string]interface{}{}
			_ = json.Unmarshal(raw, &u.loadPayload)
			if u.loadStatus == 0 {
				u.loadStatus = http.StatusOK
			}
			if u.loadBody == "" {
				u.loadBody = `{"status":"loading"}`
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(u.loadStatus)
			_, _ = w.Write([]byte(u.loadBody))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	parsed, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("URL upstream %q: %v", srv.URL, err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatalf("порт upstream %q: %v", srv.URL, err)
	}
	return types.Backend{
		ID:            "cppworker-r517",
		Host:          "127.0.0.1",
		CppWorkerPort: port,
		Type:          types.BackendTypeLlamaCpp,
		Status:        types.StatusHealthy,
	}
}

func r517Profile() types.LlamaCppModelProfile {
	return types.LlamaCppModelProfile{ContextLength: 32768, BatchSize: 512, NumGPULayers: -1}
}

// TestR517_ReloadNotLoadedFallsBackToLoad — ровно живой случай.
func TestR517_ReloadNotLoadedFallsBackToLoad(t *testing.T) {
	s, _ := newLoadDefaultsServer(t)
	up := &r517Upstream{
		reloadStatus: http.StatusNotFound,
		reloadBody:   `{"error":"model not currently loaded"}`,
	}
	backend := up.start(t)

	status, err := s.reloadModelOnCppWorker(backend, "on-disk-only.gguf", r517Profile())
	if err != nil {
		t.Fatalf("применение профиля на узел с незагруженной моделью обязано грузить её, получена ошибка: %v", err)
	}
	if status != "loaded" {
		t.Fatalf("статус = %q, want \"loaded\" (иначе UI покажет «перезагружено», хотя была загрузка)", status)
	}
	if up.loadCalls != 1 {
		t.Fatalf("вызовов /api/models/load: %d, want 1", up.loadCalls)
	}
	if got := up.loadQuery.Get("wait"); got != "true" {
		t.Fatalf("/api/models/load вызван без wait=true (query=%q) — apply вернулся бы до загрузки", up.loadQuery.Encode())
	}
	// Тело: те же параметры, но без force — у /api/models/load такого поля нет.
	if _, has := up.loadPayload["force"]; has {
		t.Fatalf("в теле /api/models/load есть force — строгий декодер воркера ответит 400 unknown field")
	}
	if name, _ := up.loadPayload["name"].(string); name != "on-disk-only.gguf" {
		t.Fatalf("name в теле загрузки = %q", name)
	}
	if ctx, _ := up.loadPayload["contextSize"].(float64); int(ctx) != 32768 {
		t.Fatalf("contextSize в теле загрузки = %v, want 32768", up.loadPayload["contextSize"])
	}
}

// TestR517_Foreign404IsNotMasked — чужой 404 не повод грузить модель.
func TestR517_Foreign404IsNotMasked(t *testing.T) {
	s, _ := newLoadDefaultsServer(t)
	up := &r517Upstream{
		reloadStatus: http.StatusNotFound,
		reloadBody:   `{"error":"route not found"}`,
	}
	backend := up.start(t)

	if _, err := s.reloadModelOnCppWorker(backend, "m.gguf", r517Profile()); err == nil {
		t.Fatalf("404 с посторонним текстом обязан остаться ошибкой, а не запускать загрузку")
	}
	if up.loadCalls != 0 {
		t.Fatalf("загрузка запущена на постороннем 404: вызовов load %d", up.loadCalls)
	}
}

// TestR517_ServerErrorIsNotMasked — 5xx остаётся ошибкой.
func TestR517_ServerErrorIsNotMasked(t *testing.T) {
	s, _ := newLoadDefaultsServer(t)
	up := &r517Upstream{
		reloadStatus: http.StatusInternalServerError,
		reloadBody:   `{"error":"boom"}`,
	}
	backend := up.start(t)

	if _, err := s.reloadModelOnCppWorker(backend, "m.gguf", r517Profile()); err == nil {
		t.Fatalf("500 от воркера обязан остаться ошибкой применения")
	}
	if up.loadCalls != 0 {
		t.Fatalf("загрузка запущена после 500 (вызовов load %d)", up.loadCalls)
	}
}

// TestR517_LoadFailureIsReported — если и загрузка не удалась, ошибка честная.
func TestR517_LoadFailureIsReported(t *testing.T) {
	s, _ := newLoadDefaultsServer(t)
	up := &r517Upstream{
		reloadStatus: http.StatusNotFound,
		reloadBody:   `{"error":"model not currently loaded"}`,
		loadStatus:   http.StatusBadRequest,
		loadBody:     `{"error":"invalid JSON: json: unknown field \"foo\""}`,
	}
	backend := up.start(t)

	if _, err := s.reloadModelOnCppWorker(backend, "m.gguf", r517Profile()); err == nil {
		t.Fatalf("провал загрузки обязан вернуть ошибку, а не «применено»")
	}
}
