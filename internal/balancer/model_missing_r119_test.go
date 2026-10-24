//go:build llama_stub

// model_missing_r119_test.go — R119 (2026-10-15).
//
// ЖИВОЙ ДЕФЕКТ (стенд). Клиент запросил модель `llama-3-8b`, которой нет в
// каталоге:
//
//	preflightNCtxReload: detected n_ctx mismatch, scheduling async reload   (loaded_n_ctx=0)
//	preflightNCtxReload(async): reload returned non-OK status … status:404,
//	    body:"{\"available_models\":[…]}"                                    ← понятный отказ
//	proxyRequestLlamaCppNonStream: preflight n_ctx reload, returning 503
//	    … "model \"llama-3-8b\" is being reloaded to n_ctx=2048 … retry in 30s"
//
// То есть балансер ИГНОРИРОВАЛ терминальный 404 и на каждый следующий запрос
// снова планировал загрузку, а клиенту отдавал 503 «повтори». Замер: POST
// /api/generate через балансер для несуществующей модели не отвечал более 4 минут;
// клиент падал по своему таймауту → оператор видел «Internal Server Error».
//
// Тесты фиксируют новое поведение:
//  1. 404 (и 4xx с `model_file_not_found` / `available_models`) распознаётся как
//     «модели нет», а успешное тело со списком моделей — нет;
//  2. причина и список доступных моделей достаются из тела;
//  3. preflight для такой модели НЕ планирует перезагрузку и возвращает 404;
//  4. запись снимается: по TTL, при успешной загрузке (executeAsyncReload SUCCESS)
//     и явно (clearModelMissing);
//  5. клиентский ответ — 404 с кодом model_not_found и списком моделей.
package balancer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// живое тело cppworker на попытку загрузить несуществующую модель.
const missingModelBody = `{"error":"model file not found: models/llama-3-8b.gguf","code":"model_file_not_found","hint":"проверьте имя модели","available_models":["gemma-4-E4B-it-Q4_K_M","Qwen3.8-27B-UD-Q4_K_M"]}`

// newMissingModelTestProxy — прокси БЕЗ загруженных моделей: ровно то состояние,
// в котором preflight решает «надо загрузить» (loaded_n_ctx=0).
func newMissingModelTestProxy(t *testing.T) *Proxy {
	t.Helper()
	const backendID = "cpp-1"
	p := &Proxy{
		config:     &types.LoadBalancerConfig{},
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
	}
	p.backends[backendID] = &BackendState{Backend: &types.Backend{
		ID: backendID, Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
		Host: "127.0.0.1", CppWorkerPort: 1, MaxConcurrentReqs: 2,
	}}
	p.metricsMgr.llamaMetrics[backendID] = &types.LlamaCppMetrics{}
	p.nctxReload = NewNCtxReloadCoordinator(DefaultNCtxReloadConfig())
	p.client = &http.Client{}
	return p
}

// TestR119_MissingResponseRecognized — что считаем «модели нет».
func TestR119_MissingResponseRecognized(t *testing.T) {
	if !isModelMissingResponse(404, []byte(missingModelBody)) {
		t.Error("404 с телом про отсутствующую модель не распознан")
	}
	if !isModelMissingResponse(400, []byte(`{"code":"model_file_not_found"}`)) {
		t.Error("400 с кодом model_file_not_found не распознан")
	}
	if !isModelMissingResponse(500, []byte(`{"available_models":["a"]}`)) {
		t.Error("4xx/5xx с available_models не распознан (так отвечает cppworker на load)")
	}
	// ГОЛЫЙ 404 — НЕ признак: так отвечает отсутствующий эндпоинт (старая сборка
	// cppworker, мок). Первая версия проверки считала такой ответ «модели нет» и
	// ломала рост окна — это поймал TestR83_StaleCache_ResolverUsesLiveWindow.
	if isModelMissingResponse(404, []byte(`404 page not found`)) {
		t.Error("голый 404 принят за «модели нет»: так отвечает НЕсуществующий эндпоинт")
	}
	if isModelMissingResponse(404, []byte(`{}`)) {
		t.Error("пустой 404 принят за «модели нет»")
	}
	// Успешное тело со списком моделей НЕ должно считаться «модели нет».
	if isModelMissingResponse(200, []byte(`{"available_models":["a","b"]}`)) {
		t.Error("200 со списком моделей ошибочно принят за «модели нет»")
	}
	// Обычная поломка — не «модели нет».
	if isModelMissingResponse(500, []byte(`{"error":"cudaMalloc failed"}`)) {
		t.Error("500 без признаков отсутствия модели принят за «модели нет»")
	}
}

// TestR119_MissingReasonAndAvailableParsed — причина и список моделей из тела.
func TestR119_MissingReasonAndAvailableParsed(t *testing.T) {
	reason, available := modelMissingFromBody([]byte(missingModelBody))
	if !strings.Contains(reason, "model file not found") || !strings.Contains(reason, "проверьте имя модели") {
		t.Errorf("reason = %q, want error + hint из тела", reason)
	}
	if len(available) != 2 || available[0] != "gemma-4-E4B-it-Q4_K_M" {
		t.Errorf("available = %v, want список моделей бэкенда", available)
	}
	// Мусор в теле не должен приводить к панике или пустому ответу.
	reason, available = modelMissingFromBody([]byte("not json"))
	if reason == "" || available != nil {
		t.Errorf("на не-JSON теле получили reason=%q available=%v", reason, available)
	}
}

// TestR119_PreflightReturns404InsteadOfReload — ГЛАВНОЕ: для отсутствующей модели
// перезагрузка НЕ планируется, клиент получает терминальный 404.
func TestR119_PreflightReturns404InsteadOfReload(t *testing.T) {
	p := newMissingModelTestProxy(t)
	p.recordModelMissing("cpp-1", "llama-3-8b", []byte(missingModelBody))

	body := []byte(`{"model":"llama-3-8b","messages":[{"role":"user","content":"hi"}]}`)
	_, needsProxy, msg, status := p.preflightNCtxReloadIfNeeded(
		context.Background(), "cpp-1", "llama-3-8b", body, "/api/chat")

	if p.nctxReload.IsReloadPending("cpp-1", "llama-3-8b") {
		t.Fatal("перезагрузка снова запланирована, хотя cppworker уже ответил 404 — " +
			"это и есть живой цикл «reload → 404 → 503 retry in 30s»")
	}
	if needsProxy {
		t.Fatalf("запрос нельзя проксировать: needsProxy=true status=%d", status)
	}
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (терминальный отказ, а не 503 «повтори»)", status)
	}
	if !strings.Contains(msg, "llama-3-8b") || !strings.Contains(msg, "not found") {
		t.Errorf("сообщение не объясняет, что модели нет: %q", msg)
	}
	if !strings.Contains(msg, "gemma-4-E4B-it-Q4_K_M") {
		t.Errorf("в сообщении нет списка доступных моделей: %q", msg)
	}
}

// TestR119_RecordClearedByTTL — модель могли скопировать на диск: запись живёт
// минуту, а не вечно.
func TestR119_RecordClearedByTTL(t *testing.T) {
	p := newMissingModelTestProxy(t)
	p.recordModelMissing("cpp-1", "m", []byte(missingModelBody))
	if _, _, ok := p.modelMissingFor("cpp-1", "m"); !ok {
		t.Fatal("запись не сохранена")
	}
	p.mu.Lock()
	rec := p.modelMissing[modelMissingKey("cpp-1", "m")]
	rec.At = time.Now().Add(-modelMissingTTL - time.Second)
	p.modelMissing[modelMissingKey("cpp-1", "m")] = rec
	p.mu.Unlock()
	if _, _, ok := p.modelMissingFor("cpp-1", "m"); ok {
		t.Error("просроченная запись должна игнорироваться (модель могли только что добавить)")
	}
}

// TestR119_ClearOnSuccess — успешная загрузка снимает запись.
func TestR119_ClearOnSuccess(t *testing.T) {
	p := newMissingModelTestProxy(t)
	p.recordModelMissing("cpp-1", "m", []byte(missingModelBody))
	p.clearModelMissing("cpp-1", "m")
	if _, _, ok := p.modelMissingFor("cpp-1", "m"); ok {
		t.Error("после успешной загрузки запись должна быть снята")
	}
	if snap := p.modelMissingSnapshot(); len(snap) != 0 {
		t.Errorf("запись осталась: %v", snap)
	}
}

// TestR119_MissingDoesNotBlockOtherModels — запись про одну модель не мешает
// другой (ключ включает имя модели).
func TestR119_MissingDoesNotBlockOtherModels(t *testing.T) {
	p := newMissingModelTestProxy(t)
	p.recordModelMissing("cpp-1", "llama-3-8b", []byte(missingModelBody))
	if _, _, ok := p.modelMissingFor("cpp-1", "gemma-4-E4B-it-Q4_K_M"); ok {
		t.Error("запись про llama-3-8b не должна действовать на другую модель")
	}
}

// TestR119_ClientResponseIs404WithMachineReadableBody — что видит клиент.
func TestR119_ClientResponseIs404WithMachineReadableBody(t *testing.T) {
	rec := httptest.NewRecorder()
	_, available := modelMissingFromBody([]byte(missingModelBody))
	writeModelMissingResponse(rec, "llama-3-8b", "model file not found: models/llama-3-8b.gguf", available)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404: 503 заставлял клиента повторять запрос до таймаута", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("тело не JSON: %v (%s)", err, rec.Body.String())
	}
	if body["code"] != "model_not_found" {
		t.Errorf("code = %v, want model_not_found (машинный признак для инструментов)", body["code"])
	}
	if body["model"] != "llama-3-8b" {
		t.Errorf("model = %v, want llama-3-8b", body["model"])
	}
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, "llama-3-8b") || !strings.Contains(msg, "not found") {
		t.Errorf("error = %q: клиент должен понять, что модели нет", msg)
	}
	if list, ok := body["availableModels"].([]interface{}); !ok || len(list) != 2 {
		t.Errorf("availableModels = %v, want список доступных моделей", body["availableModels"])
	}
}

// TestR119_NilWriterIsSafe — nil ResponseWriter не паникует (preflight вызывают
// и без writer'а — например, в диагностике).
func TestR119_NilWriterIsSafe(t *testing.T) {
	writeModelMissingResponse(nil, "m", "reason", nil)
}

// TestR119_WaitAbortsWhenModelMissing — ожидание НЕ выжигает бюджет, если модель
// уже признана отсутствующей.
//
// Живой замер до правки: запрос к llama-3-8b через балансер не отвечал более
// 4 минут — фоновая загрузка получила терминальный 404, а цикл ожидания
// продолжал ждать появления модели до конца бюджета.
func TestR119_WaitAbortsWhenModelMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Бэкенд жив, но модели не знает: пустой список (ровно как cppworker для
		// несуществующего имени).
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[],"count":0}`))
	}))
	defer srv.Close()

	hostPort := strings.TrimPrefix(srv.URL, "http://")
	host := hostPort[:strings.LastIndex(hostPort, ":")]
	port := 0
	if _, err := fmt.Sscanf(hostPort[strings.LastIndex(hostPort, ":")+1:], "%d", &port); err != nil {
		t.Fatalf("parse port: %v", err)
	}

	p := newMissingModelTestProxy(t)
	p.backends["cpp-1"] = &BackendState{Backend: &types.Backend{
		ID: "cpp-1", Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
		Host: host, CppWorkerPort: port, MaxConcurrentReqs: 2,
	}}
	p.recordModelMissing("cpp-1", "llama-3-8b", []byte(missingModelBody))

	start := time.Now()
	ok := p.waitForBackendNCtx(context.Background(), "cpp-1", "llama-3-8b", 1, 60*time.Second)
	elapsed := time.Since(start)

	if ok {
		t.Fatal("waitForBackendNCtx вернул true для отсутствующей модели")
	}
	if elapsed > 10*time.Second {
		t.Errorf("ожидание длилось %v при бюджете 60 c: при известном «модели нет» "+
			"цикл обязан выйти сразу (живой замер до правки — более 4 минут)", elapsed)
	}
}

// TestR119_LongAvailableListIsTruncated — в сообщении не должно быть простыни из
// сотен имён: список моделей бывает длинным, а ошибку читает человек.
func TestR119_LongAvailableListIsTruncated(t *testing.T) {
	available := make([]string, 40)
	for i := range available {
		available[i] = "model-" + strings.Repeat("x", 3)
	}
	msg := formatModelMissingMessage("m", "reason", available)
	if len(msg) > 600 {
		t.Errorf("сообщение длиной %d символов не годится для UI: %q", len(msg), msg)
	}
	if !strings.Contains(msg, "…") {
		t.Errorf("длинный список должен быть усечён: %q", msg)
	}
}
