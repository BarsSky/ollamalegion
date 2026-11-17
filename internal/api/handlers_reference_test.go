// handlers_reference_test.go — тесты серверной части «Справка/рекомендации + шаблоны».
//
// Живой стенд не нужен: httptest + временный каталог (t.TempDir()). Проверяются
// ровно те свойства, которые нельзя увидеть чтением кода:
//
//	(1) отсутствие файла хранилища — не 404/500, а 200 с пустыми списками;
//	(2) PUT создаёт запись, повторный PUT растит revision, старый revision → 409;
//	(3) неизвестное имя параметра → 400 unknown_param со списком allowed;
//	(4) значение вне границ → 400 invalid_value;
//	(5) сид из доков — ТОЛЬКО при отсутствии файла, существующий не перезаписывается;
//	(6) apply с mode=profile+reload и пустым backendId → 400 backend_required;
//	(7) apply mode=profile-only не требует backendId и НЕ запускает reload;
//	(8) preview отдаёт diff и violations и НИЧЕГО не пишет на диск.
//
// Контракт: docs/plans/2026-11-17-reference-templates-design.md §3.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/internal/reference"
	"ollama-loadbalancer/pkg/types"
)

// referenceDefaultModel — имя модели, к которой применяются шаблоны в тестах.
const referenceDefaultModel = "Qwen3-4B-Instruct-2507-Q4_K_M"

// --- обвязка --------------------------------------------------------------

// newReferenceTestServer — сервер с хранилищем во временном каталоге.
//
// statePath ВНУТРИ t.TempDir(): иначе fallback-персист профилей
// (profiles_persistence.go:52-58) писал бы data/profiles.json прямо в каталог
// тестов, и прогон пачкал бы репозиторий.
//
// configSaver НЕ задан намеренно: bundled-стенд с read-only /app/config — это
// штатный сценарий, и путь профилей обязан работать именно так.
func newReferenceTestServer(t *testing.T) (*Server, *reference.Store, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:      "127.0.0.1",
			Port:      8080,
			APIPort:   8081,
			StatePath: filepath.Join(dir, "state.json"),
		},
	}
	proxy := balancer.NewProxy(cfg)
	srv := NewServer(proxy, cfg, nil)
	t.Cleanup(func() {
		srv.StopMetricsLoop()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = proxy.Shutdown(ctx)
	})
	storePath := filepath.Join(dir, reference.FileName)
	store := reference.NewStore(storePath)
	store.Load()
	srv.SetReferenceStore(store)
	return srv, store, storePath
}

// referenceDo — запрос через mux сервера + разобранный JSON-ответ.
//
// body: nil | map/срез (уйдёт как JSON) | string (уйдёт как есть).
func referenceDo(t *testing.T, s *Server, method, path string, body interface{}) (*httptest.ResponseRecorder, map[string]interface{}) {
	t.Helper()
	var payload []byte
	switch v := body.(type) {
	case nil:
		payload = nil
	case string:
		payload = []byte(v)
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal тела запроса: %v", err)
		}
		payload = raw
	}
	var reader *bytes.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	var req *http.Request
	if reader != nil {
		req = httptest.NewRequest(method, path, reader)
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	parsed := map[string]interface{}{}
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &parsed)
	}
	return rec, parsed
}

// referenceRecommendationBody — тело PUT записи-рекомендации.
func referenceRecommendationBody(title string, params []map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{"title": title, "params": params}
}

// referenceParam — один параметр записи.
func referenceParam(name string, value interface{}) map[string]interface{} {
	return map[string]interface{}{"name": name, "value": value}
}

// referenceTemplateBody — тело PUT шаблона.
func referenceTemplateBody(name string, params map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{"name": name, "version": 1, "params": params}
}

// referenceNumber — число из разобранного JSON (float64 → int).
func referenceNumber(t *testing.T, parsed map[string]interface{}, path ...string) int {
	t.Helper()
	var current interface{} = parsed
	for _, key := range path {
		obj, ok := current.(map[string]interface{})
		if !ok {
			t.Fatalf("ожидался объект по пути %v, получили %T", path, current)
		}
		current = obj[key]
	}
	number, ok := current.(float64)
	if !ok {
		t.Fatalf("ожидалось число по пути %v, получили %T (%v)", path, current, current)
	}
	return int(number)
}

// --- (1) отсутствие файла — не ошибка -------------------------------------

// TestReference_NoFileEmptyState — хранилища на диске нет: GET отдаёт 200 и
// ПУСТЫЕ списки, а не 404/500/503.
//
// Файла нет — это валидное состояние (§2.5): иначе UI выглядел бы сломанным до
// первой записи. Сид (решение №4) наполняет файл при старте, поэтому здесь
// проверяется именно хранилище БЕЗ чтения файла: «состояние отсутствия файла»
// само по себе обязано быть пустым и валидным, а не падать.
func TestReference_NoFileEmptyState(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, reference.FileName)
	if _, err := os.Stat(storePath); !os.IsNotExist(err) {
		t.Fatalf("предусловие: файла хранилища быть не должно (%s)", storePath)
	}

	srv, _, _ := newReferenceTestServer(t)
	// Подменяем хранилище на НЕ ЗАГРУЖЕННОЕ: файла нет, данных нет.
	srv.SetReferenceStore(reference.NewStore(storePath))

	rec, body := referenceDo(t, srv, http.MethodGet, referenceRecommendationsPath, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET рекомендаций без файла: HTTP %d, ожидали 200 (%s)", rec.Code, rec.Body.String())
	}
	items, ok := body["items"].([]interface{})
	if !ok {
		t.Fatalf("ответ без массива items: %s", rec.Body.String())
	}
	if len(items) != 0 {
		t.Errorf("ожидался пустой список рекомендаций, получили %d записей", len(items))
	}
	if total := referenceNumber(t, body, "total"); total != 0 {
		t.Errorf("total = %d, ожидали 0", total)
	}
	if version := referenceNumber(t, body, "schemaVersion"); version != reference.SchemaVersion {
		t.Errorf("schemaVersion = %d, ожидали %d", version, reference.SchemaVersion)
	}

	rec, body = referenceDo(t, srv, http.MethodGet, referenceTemplatesPath, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET шаблонов без файла: HTTP %d, ожидали 200 (%s)", rec.Code, rec.Body.String())
	}
	items, ok = body["items"].([]interface{})
	if !ok {
		t.Fatalf("ответ без массива items: %s", rec.Body.String())
	}
	if len(items) != 0 {
		t.Errorf("ожидался пустой список шаблонов, получили %d", len(items))
	}
}

// --- (2) revision растёт, старая ревизия → 409 ----------------------------

// TestReference_PutCreatesAndBumpsRevision — PUT создаёт запись, повторный PUT
// растит revision, PUT со старым revision → 409 revision_conflict.
func TestReference_PutCreatesAndBumpsRevision(t *testing.T) {
	srv, _, _ := newReferenceTestServer(t)
	path := referenceRecommendationsPath + "/agent-note"
	payload := referenceRecommendationBody("Заметка оператора",
		[]map[string]interface{}{referenceParam("contextLength", 32768)})

	rec, body := referenceDo(t, srv, http.MethodPut, path, payload)
	if rec.Code != http.StatusOK {
		t.Fatalf("создание: HTTP %d, ожидали 200 (%s)", rec.Code, rec.Body.String())
	}
	if revision := referenceNumber(t, body, "recommendation", "revision"); revision != 1 {
		t.Fatalf("revision после создания = %d, ожидали 1", revision)
	}

	rec, body = referenceDo(t, srv, http.MethodPut, path, payload)
	if rec.Code != http.StatusOK {
		t.Fatalf("обновление: HTTP %d, ожидали 200 (%s)", rec.Code, rec.Body.String())
	}
	if revision := referenceNumber(t, body, "recommendation", "revision"); revision != 2 {
		t.Fatalf("revision после второго PUT = %d, ожидали 2", revision)
	}

	stale := referenceRecommendationBody("Заметка оператора",
		[]map[string]interface{}{referenceParam("contextLength", 32768)})
	stale["revision"] = 1
	rec, body = referenceDo(t, srv, http.MethodPut, path, stale)
	if rec.Code != http.StatusConflict {
		t.Fatalf("PUT со старой ревизией: HTTP %d, ожидали 409 (%s)", rec.Code, rec.Body.String())
	}
	if code, _ := body["error"].(string); code != "revision_conflict" {
		t.Errorf("error = %q, ожидали revision_conflict", code)
	}
	if current := referenceNumber(t, body, "current"); current != 2 {
		t.Errorf("current = %d, ожидали 2", current)
	}
	if sent := referenceNumber(t, body, "sent"); sent != 1 {
		t.Errorf("sent = %d, ожидали 1", sent)
	}

	matching := referenceRecommendationBody("Заметка оператора",
		[]map[string]interface{}{referenceParam("contextLength", 65536)})
	matching["revision"] = 2
	rec, body = referenceDo(t, srv, http.MethodPut, path, matching)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT с актуальной ревизией: HTTP %d, ожидали 200 (%s)", rec.Code, rec.Body.String())
	}
	if revision := referenceNumber(t, body, "recommendation", "revision"); revision != 3 {
		t.Errorf("revision = %d, ожидали 3", revision)
	}
}

// --- (3) неизвестное имя параметра ---------------------------------------

// TestReference_UnknownParamRejected — 400 unknown_param со списком допустимых.
func TestReference_UnknownParamRejected(t *testing.T) {
	srv, _, _ := newReferenceTestServer(t)
	payload := referenceRecommendationBody("Плохой параметр",
		[]map[string]interface{}{referenceParam("nCtx", 4096)})

	rec, body := referenceDo(t, srv, http.MethodPut, referenceRecommendationsPath+"/bad-param", payload)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("HTTP %d, ожидали 400 (%s)", rec.Code, rec.Body.String())
	}
	if code, _ := body["error"].(string); code != "unknown_param" {
		t.Fatalf("error = %q, ожидали unknown_param (%s)", code, rec.Body.String())
	}
	if field, _ := body["field"].(string); field != "nCtx" {
		t.Errorf("field = %q, ожидали nCtx", field)
	}
	allowed, ok := body["allowed"].([]interface{})
	if !ok || len(allowed) == 0 {
		t.Fatalf("в ответе нет списка allowed: %s", rec.Body.String())
	}
	found := false
	for _, item := range allowed {
		if name, _ := item.(string); name == "contextLength" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("в allowed нет contextLength: %s", rec.Body.String())
	}

	// Шаблон отвечает тем же кодом: имена проверяются одним валидатором.
	rec, body = referenceDo(t, srv, http.MethodPut, referenceTemplatesPath+"/bad-template",
		referenceTemplateBody("Плохой шаблон", map[string]interface{}{"nCtx": 4096}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("шаблон: HTTP %d, ожидали 400 (%s)", rec.Code, rec.Body.String())
	}
	if code, _ := body["error"].(string); code != "unknown_param" {
		t.Errorf("шаблон: error = %q, ожидали unknown_param", code)
	}
}

// --- (4) значение вне границ ---------------------------------------------

// TestReference_ValueOutOfBoundsRejected — 400 invalid_value с именем поля.
//
// Границы берутся из существующего validateModelProfile
// (handlers_cppworker_profiles.go:1211-1266): parallel ∈ [0, 8],
// contextLength ∈ [256, 262144].
func TestReference_ValueOutOfBoundsRejected(t *testing.T) {
	srv, _, _ := newReferenceTestServer(t)
	cases := []struct {
		name  string
		field string
		value interface{}
	}{
		{"parallel_too_big", "parallel", 12},
		{"context_too_big", "contextLength", 1310720},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := referenceRecommendationBody("Вне границ",
				[]map[string]interface{}{referenceParam(tc.field, tc.value)})
			rec, body := referenceDo(t, srv, http.MethodPut,
				referenceRecommendationsPath+"/out-of-bounds", payload)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("HTTP %d, ожидали 400 (%s)", rec.Code, rec.Body.String())
			}
			if code, _ := body["error"].(string); code != "invalid_value" {
				t.Fatalf("error = %q, ожидали invalid_value (%s)", code, rec.Body.String())
			}
			if field, _ := body["field"].(string); field != tc.field {
				t.Errorf("field = %q, ожидали %q", field, tc.field)
			}
		})
	}
}

// --- (5) сид только при отсутствии файла ---------------------------------

// TestReference_SeedOnlyWhenFileAbsent — сид создаёт записи ТОЛЬКО когда файла
// нет; существующий файл сид не перезаписывает.
func TestReference_SeedOnlyWhenFileAbsent(t *testing.T) {
	t.Run("absent_file_is_seeded", func(t *testing.T) {
		srv, store, storePath := newReferenceTestServer(t)
		if _, err := os.Stat(storePath); err != nil {
			t.Fatalf("файл хранилища не создан при сиде: %v", err)
		}
		rec, body := referenceDo(t, srv, http.MethodGet, referenceRecommendationsPath, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("HTTP %d, ожидали 200 (%s)", rec.Code, rec.Body.String())
		}
		items, _ := body["items"].([]interface{})
		if len(items) == 0 {
			t.Fatalf("сид не создал ни одной записи: %s", rec.Body.String())
		}
		if !store.Seeded() {
			t.Errorf("хранилище не сообщает о выполненном сиде")
		}
		for _, item := range items {
			obj, _ := item.(map[string]interface{})
			if author, _ := obj["author"].(string); author != "docs-import" {
				t.Errorf("запись %v: author = %q, ожидали docs-import", obj["id"], author)
			}
			sources, _ := obj["sources"].([]interface{})
			if len(sources) == 0 {
				t.Errorf("запись %v: нет sources", obj["id"])
				continue
			}
			src, _ := sources[0].(map[string]interface{})
			if doc, _ := src["doc"].(string); doc != "docs/operations/model-tuning-reference.ru.md" {
				t.Errorf("запись %v: sources[0].doc = %q", obj["id"], doc)
			}
		}
	})

	t.Run("existing_file_not_overwritten", func(t *testing.T) {
		dir := t.TempDir()
		storePath := filepath.Join(dir, reference.FileName)
		existing := `{
  "version": 1,
  "recommendations": [
    {
      "id": "operator-note",
      "title": "Своя запись оператора",
      "params": [{"name": "contextLength", "value": 8192}],
      "author": "operator",
      "revision": 1
    }
  ],
  "templates": []
}`
		if err := os.WriteFile(storePath, []byte(existing), 0o644); err != nil {
			t.Fatalf("подготовка файла: %v", err)
		}

		store := reference.NewStore(storePath)
		if seeded := store.Load(); seeded {
			t.Fatalf("Load выполнил сид поверх существующего файла — это запрещено решением №4")
		}
		if after, err := os.ReadFile(storePath); err == nil && string(after) != existing {
			t.Fatalf("существующий файл перезаписан сидом")
		}

		srv, _, _ := newReferenceTestServer(t)
		srv.SetReferenceStore(store)
		rec, body := referenceDo(t, srv, http.MethodGet, referenceRecommendationsPath, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("HTTP %d, ожидали 200 (%s)", rec.Code, rec.Body.String())
		}
		items, _ := body["items"].([]interface{})
		if len(items) != 1 {
			t.Fatalf("ожидалась ровно одна запись из файла, получили %d (%s)", len(items), rec.Body.String())
		}
		obj, _ := items[0].(map[string]interface{})
		if id, _ := obj["id"].(string); id != "operator-note" {
			t.Fatalf("id = %q, ожидали operator-note: содержимое файла потеряно или перезаписано", id)
		}
	})
}

// --- обвязка для apply ----------------------------------------------------

// referenceFakeCppWorker — минимальный «cppworker» рядом: считает reload'ы.
//
// Так проверяется именно то, что просили: profile-only НЕ перезагружает модель,
// profile+reload — перезагружает (единственный признак — реальный запрос
// POST /api/models/reload).
func referenceFakeCppWorker(t *testing.T) (port int, reloads *int64) {
	t.Helper()
	var counter int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/api/models/reload"):
			atomic.AddInt64(&counter, 1)
			_, _ = w.Write([]byte(`{"status":"reloaded"}`))
		case strings.HasSuffix(r.URL.Path, "/api/models/effective"):
			_, _ = w.Write([]byte(`{"memory":{"feasible":true,"stage":"vram"}}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	addr, ok := srv.Listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("не удалось получить адрес тестового сервера")
	}
	return addr.Port, &counter
}

// referenceAddFakeBackend — регистрирует llama.cpp-бэкенд на тестовом сервере.
func referenceAddFakeBackend(t *testing.T, srv *Server, id string, port int) {
	t.Helper()
	if err := srv.proxy.AddBackend(types.Backend{
		ID:            id,
		Name:          id,
		Host:          "127.0.0.1",
		Type:          types.BackendTypeLlamaCpp,
		CppWorkerPort: port,
		Status:        types.StatusHealthy,
	}); err != nil {
		t.Fatalf("AddBackend(%s): %v", id, err)
	}
}

// referenceCreateTemplate — создать шаблон через API и вернуть его id.
func referenceCreateTemplate(t *testing.T, srv *Server, id string, params map[string]interface{}) {
	t.Helper()
	rec, body := referenceDo(t, srv, http.MethodPut, referenceTemplatesPath+"/"+id,
		referenceTemplateBody("Шаблон "+id, params))
	if rec.Code != http.StatusOK {
		t.Fatalf("создание шаблона %s: HTTP %d (%s)", id, rec.Code, rec.Body.String())
	}
	_ = body
}

// --- (6) backend_required ------------------------------------------------

// TestReference_ApplyRequiresExplicitBackend — mode=profile+reload с пустым
// backendId → 400 backend_required (решение человека №3).
func TestReference_ApplyRequiresExplicitBackend(t *testing.T) {
	srv, _, _ := newReferenceTestServer(t)
	referenceCreateTemplate(t, srv, "agent-8gb", map[string]interface{}{
		"contextLength": 32768,
		"kvCacheType":   "q8_0",
	})

	rec, body := referenceDo(t, srv, http.MethodPost,
		referenceTemplatesPath+"/agent-8gb/apply",
		map[string]interface{}{"model": referenceDefaultModel, "mode": "profile+reload"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("HTTP %d, ожидали 400 (%s)", rec.Code, rec.Body.String())
	}
	if code, _ := body["error"].(string); code != "backend_required" {
		t.Fatalf("error = %q, ожидали backend_required (%s)", code, rec.Body.String())
	}
	message, _ := body["message"].(string)
	if !strings.Contains(message, "задевает чужие") {
		t.Errorf("в сообщении нет объяснения про чужие узлы: %q", message)
	}
	// Ничего не сохранено: профиль не появился.
	if profile, exists := srv.proxy.GetModelProfile(referenceDefaultModel); exists {
		t.Errorf("профиль сохранён, хотя применение отклонено: %+v", profile)
	}
}

// --- (7) profile-only не перезагружает модель ----------------------------

// TestReference_ApplyProfileOnlyDoesNotReload — profile-only не требует
// backendId и НЕ вызывает reload; profile+reload с явным backendId — вызывает.
func TestReference_ApplyProfileOnlyDoesNotReload(t *testing.T) {
	srv, _, _ := newReferenceTestServer(t)
	port, reloads := referenceFakeCppWorker(t)
	referenceAddFakeBackend(t, srv, "fake-gpu", port)
	referenceCreateTemplate(t, srv, "agent-8gb", map[string]interface{}{
		"contextLength": 65536,
		"kvCacheType":   "q8_0",
		"parallel":      1,
	})

	rec, body := referenceDo(t, srv, http.MethodPost,
		referenceTemplatesPath+"/agent-8gb/apply",
		map[string]interface{}{"model": referenceDefaultModel, "mode": "profile-only"})
	if rec.Code != http.StatusOK {
		t.Fatalf("profile-only: HTTP %d, ожидали 200 (%s)", rec.Code, rec.Body.String())
	}
	if status, _ := body["status"].(string); status != "applied" {
		t.Errorf("status = %q, ожидали applied", status)
	}
	if reloaded := atomic.LoadInt64(reloads); reloaded != 0 {
		t.Errorf("profile-only вызвал reload %d раз(а) — этого быть не должно", reloaded)
	}
	profile, exists := srv.proxy.GetModelProfile(referenceDefaultModel)
	if !exists {
		t.Fatalf("profile-only не сохранил профиль")
	}
	if profile.ContextLength != 65536 {
		t.Errorf("contextLength в профиле = %d, ожидали 65536", profile.ContextLength)
	}
	if profile.KVCacheType != "q8_0" {
		t.Errorf("kvCacheType в профиле = %q, ожидали q8_0", profile.KVCacheType)
	}

	rec, body = referenceDo(t, srv, http.MethodPost,
		referenceTemplatesPath+"/agent-8gb/apply",
		map[string]interface{}{
			"model":     referenceDefaultModel,
			"mode":      "profile+reload",
			"backendId": "fake-gpu",
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("profile+reload: HTTP %d, ожидали 200 (%s)", rec.Code, rec.Body.String())
	}
	if reloaded := atomic.LoadInt64(reloads); reloaded == 0 {
		t.Errorf("profile+reload не вызвал reload — шаблон не применён к загруженной модели")
	}
	backends, _ := body["backends"].([]interface{})
	if len(backends) != 1 {
		t.Fatalf("ожидался один бэкенд в ответе, получили %d (%s)", len(backends), rec.Body.String())
	}
	first, _ := backends[0].(map[string]interface{})
	if id, _ := first["backendId"].(string); id != "fake-gpu" {
		t.Errorf("backendId в ответе = %q, ожидали fake-gpu", id)
	}
}

// --- (8) preview ничего не пишет -----------------------------------------

// TestReference_ApplyBusyBackendForwardsAsync — занятый бэкенд: ответ 202 из
// существующего async-пути проксируется КАК ЕСТЬ (§3.4, §7 п.11: третьего
// механизма прогресса не заводим).
func TestReference_ApplyBusyBackendForwardsAsync(t *testing.T) {
	const backendID = "fake-busy"
	// R493 (2026-11-17): «занят» держится, пока не НАЧАЛАСЬ перезагрузка, а не
	// «ровно на первом вызове active-queries».
	//
	// Живой флейк (CI 44100630001, шаг «Generate coverage (race + atomic)»):
	// тест упал как HTTP 200, ожидали 202 — busy-сигнал достался ДРУГОМУ
	// вызову того же endpoint (ватчдог/поллер/повтор busy-проверки), apply увидел
	// «свободен» и ушёл синхронным путём. Привязка к факту «перезагрузка
	// началась» убирает зависимость от количества и порядка вызовов.
	var reloadStarted int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/api/models/active-queries"):
			if atomic.LoadInt32(&reloadStarted) == 0 {
				_, _ = w.Write([]byte(`{"activeQueries":2}`))
				return
			}
			_, _ = w.Write([]byte(`{"activeQueries":0}`))
		case strings.HasSuffix(r.URL.Path, "/api/models/reload"):
			atomic.StoreInt32(&reloadStarted, 1)
			_, _ = w.Write([]byte(`{"status":"reloaded"}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(fake.Close)
	addr, ok := fake.Listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("не удалось получить адрес тестового сервера")
	}

	srv, _, _ := newReferenceTestServer(t)
	referenceAddFakeBackend(t, srv, backendID, addr.Port)
	// Помечаем модель загруженной на этом бэкенде: без этого busy-проверка
	// пропускает бэкенд и apply идёт синхронным путём.
	srv.proxy.SetBackendMetrics(backendID, &types.BackendMetrics{
		LlamaCpp: types.LlamaCppMetrics{
			LoadedModels: []types.LlamaCppModel{{Name: referenceDefaultModel}},
		},
	})
	referenceCreateTemplate(t, srv, "busy-template", map[string]interface{}{
		"contextLength": 32768,
	})

	rec, body := referenceDo(t, srv, http.MethodPost,
		referenceTemplatesPath+"/busy-template/apply",
		map[string]interface{}{
			"model":     referenceDefaultModel,
			"mode":      "profile+reload",
			"backendId": backendID,
		})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("HTTP %d, ожидали 202 — занятый бэкенд должен идти async (%s)", rec.Code, rec.Body.String())
	}
	if applyID, _ := body["applyId"].(string); applyID == "" {
		t.Errorf("в ответе нет applyId: %s", rec.Body.String())
	}
	progressURL, _ := body["progressUrl"].(string)
	if !strings.Contains(progressURL, "/api/v1/cppworker/model-profiles/") ||
		!strings.Contains(progressURL, "apply/progress") {
		t.Errorf("progressUrl = %q: ожидался существующий endpoint прогресса профиля", progressURL)
	}
}

// TestReference_PreviewWritesNothing — preview отдаёт diff и violations и не
// меняет ни файл хранилища, ни профиль модели.
func TestReference_PreviewWritesNothing(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, reference.FileName)
	// Файл написан руками: так проверяется и «битый набор в файле» (violations).
	existing := `{
  "version": 1,
  "recommendations": [],
  "templates": [
    {"id": "good-template", "name": "Хороший", "version": 2, "revision": 1,
     "params": {"contextLength": 65536, "kvCacheType": "q8_0"}},
    {"id": "bad-template", "name": "Плохой", "version": 1, "revision": 1,
     "params": {"contextLength": 32768, "parallel": 12}}
  ]
}`
	if err := os.WriteFile(storePath, []byte(existing), 0o644); err != nil {
		t.Fatalf("подготовка файла: %v", err)
	}
	before, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatalf("чтение файла до preview: %v", err)
	}

	srv, _, _ := newReferenceTestServer(t)
	store := reference.NewStore(storePath)
	store.Load()
	srv.SetReferenceStore(store)
	srv.proxy.SetModelProfile(referenceDefaultModel, types.LlamaCppModelProfile{
		ContextLength: 16384,
		KVCacheType:   "f16",
	})

	rec, body := referenceDo(t, srv, http.MethodPost,
		referenceTemplatesPath+"/good-template/preview",
		map[string]interface{}{"model": referenceDefaultModel})
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: HTTP %d, ожидали 200 (%s)", rec.Code, rec.Body.String())
	}
	if ok, _ := body["ok"].(bool); !ok {
		t.Errorf("ok = false для корректного шаблона: %s", rec.Body.String())
	}
	if version := referenceNumber(t, body, "templateVersion"); version != 2 {
		t.Errorf("templateVersion = %d, ожидали 2", version)
	}
	diff, _ := body["diff"].([]interface{})
	if len(diff) == 0 {
		t.Fatalf("diff пуст: %s", rec.Body.String())
	}
	diffFields := map[string]map[string]interface{}{}
	for _, item := range diff {
		entry, _ := item.(map[string]interface{})
		field, _ := entry["field"].(string)
		diffFields[field] = entry
	}
	contextEntry, ok := diffFields["contextLength"]
	if !ok {
		t.Fatalf("в diff нет contextLength: %s", rec.Body.String())
	}
	if current := referenceNumber(t, contextEntry, "current"); current != 16384 {
		t.Errorf("diff.contextLength.current = %d, ожидали 16384", current)
	}
	if next := referenceNumber(t, contextEntry, "next"); next != 65536 {
		t.Errorf("diff.contextLength.next = %d, ожидали 65536", next)
	}
	valid, _ := body["valid"].(map[string]interface{})
	if validOK, _ := valid["ok"].(bool); !validOK {
		t.Errorf("valid.ok = false для корректного шаблона: %s", rec.Body.String())
	}

	rec, body = referenceDo(t, srv, http.MethodPost,
		referenceTemplatesPath+"/bad-template/preview",
		map[string]interface{}{"model": referenceDefaultModel})
	if rec.Code != http.StatusOK {
		t.Fatalf("preview плохого шаблона: HTTP %d, ожидали 200 (%s)", rec.Code, rec.Body.String())
	}
	if ok, _ := body["ok"].(bool); ok {
		t.Errorf("ok = true для набора с parallel=12: %s", rec.Body.String())
	}
	valid, _ = body["valid"].(map[string]interface{})
	violations, _ := valid["violations"].([]interface{})
	if len(violations) == 0 {
		t.Fatalf("violations пусты: %s", rec.Body.String())
	}
	first, _ := violations[0].(map[string]interface{})
	if field, _ := first["field"].(string); field != "parallel" {
		t.Errorf("violations[0].field = %q, ожидали parallel", field)
	}

	// ГЛАВНОЕ: preview ничего не записал.
	after, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatalf("чтение файла после preview: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("preview изменил файл хранилища — этого быть не должно")
	}
	profile, _ := srv.proxy.GetModelProfile(referenceDefaultModel)
	if profile.ContextLength != 16384 {
		t.Errorf("preview изменил профиль модели: contextLength = %d, ожидали 16384", profile.ContextLength)
	}
}
