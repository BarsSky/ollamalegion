//go:build llama_stub

// backend_update_merge_r65d_test.go — R65d (2026-09-20): регрессия на
// PUT /api/v1/backends/{id}.
//
// Найденные дефекты (аудит 2026-09-20):
//
//  1. Структура бэкенда собиралась из запроса ЦЕЛИКОМ, поэтому любой частичный
//     PUT уничтожал не переданные поля. Практический сценарий — кнопка
//     «Auto-detect cppworker port» в WebUI (app-modals.js): она шлёт
//     {cppWorkerPort: <N>} без остальных полей, после чего бэкенд получал
//     host="" и становился нероутируемым.
//
//  2. cppWorkerApiToken присваивался напрямую из запроса, а WebUI его не
//     отправляет (оператор не должен видеть секрет). Любое сохранение формы
//     стирало токен → балансер→cppworker вызовы, требующие авторизации
//     (POST /api/models/reload при apply профиля), начинали получать 401.
//
//  3. Поле Engine в структуре запроса отсутствует, поэтому обнулялось.
package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// putBackend выполняет PUT /api/v1/backends/{id} и возвращает код + тело.
func putBackend(t *testing.T, s *Server, id string, body interface{}) (int, map[string]interface{}) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPut, "/api/v1/backends/"+id, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Token", "r65d-secret-token")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	var resp map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec.Code, resp
}

// backendForUpdate — тестовый бэкенд с заполненными полями конфигурации.
func backendForUpdate() types.Backend {
	return types.Backend{
		ID:                "llama-merge",
		Name:              "Merge Test",
		Host:              "10.1.2.3",
		OllamaPort:        11434,
		AgentPort:         18032,
		CppWorkerPort:     18092,
		Weight:            7,
		MaxConcurrentReqs: 5,
		MaxModels:         3,
		Labels:            []string{"gpu:nvidia"},
		Status:            types.StatusHealthy,
		CppWorkerApiToken: "server-side-token",
		Type:              types.BackendTypeLlamaCpp,
		Engine:            types.EngineLlamaCPP,
		ApiStyle:          types.APIStyleOpenAICompatible,
	}
}

// findBackend находит бэкенд в конфиге сервера.
func findBackend(t *testing.T, s *Server, id string) types.Backend {
	t.Helper()
	for _, b := range s.config.Backends {
		if b.ID == id {
			return b
		}
	}
	t.Fatalf("backend %q not found in config", id)
	return types.Backend{}
}

// TestR65d_BackendUpdate_PartialPutPreservesFields — частичный PUT (как из
// авто-детекта порта) не должен уничтожать host/name/labels/лимиты.
func TestR65d_BackendUpdate_PartialPutPreservesFields(t *testing.T) {
	s := authEnabledServer(t, backendForUpdate())
	before := findBackend(t, s, "llama-merge")

	// Ровно тот payload, который отправляет кнопка авто-детекта.
	code, _ := putBackend(t, s, "llama-merge", map[string]interface{}{
		"cppWorkerPort": 18093,
	})
	if code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200", code)
	}

	after := findBackend(t, s, "llama-merge")
	if after.Host != before.Host {
		t.Errorf("host = %q, want %q — частичный PUT стёр host, бэкенд стал "+
			"нероутируемым (сценарий авто-детекта порта)", after.Host, before.Host)
	}
	if after.Name != before.Name {
		t.Errorf("name = %q, want %q", after.Name, before.Name)
	}
	if after.Weight != before.Weight {
		t.Errorf("weight = %d, want %d", after.Weight, before.Weight)
	}
	if after.MaxConcurrentReqs != before.MaxConcurrentReqs {
		t.Errorf("maxConcurrentReqs = %d, want %d", after.MaxConcurrentReqs, before.MaxConcurrentReqs)
	}
	if after.MaxModels != before.MaxModels {
		t.Errorf("maxModels = %d, want %d", after.MaxModels, before.MaxModels)
	}
	if len(after.Labels) != len(before.Labels) {
		t.Errorf("labels = %v, want %v", after.Labels, before.Labels)
	}
	// А переданное поле — применилось.
	if after.CppWorkerPort != 18093 {
		t.Errorf("cppWorkerPort = %d, want 18093 (переданное поле должно применяться)", after.CppWorkerPort)
	}
}

// TestR65d_BackendUpdate_PreservesApiTokenWhenNotSent — токен не должен
// стираться, если поле не пришло (WebUI его не отправляет).
func TestR65d_BackendUpdate_PreservesApiTokenWhenNotSent(t *testing.T) {
	s := authEnabledServer(t, backendForUpdate())

	// Полная форма из WebUI: токена в payload НЕТ.
	// Имена полей соответствуют request-структуре handlers_backends.go
	// (maxConcurrentRequests, а не maxConcurrentReqs).
	code, _ := putBackend(t, s, "llama-merge", map[string]interface{}{
		"name":                  "Merge Test",
		"host":                  "10.1.2.3",
		"ollamaPort":            11434,
		"agentPort":             18032,
		"cppWorkerPort":         18092,
		"weight":                2,
		"maxConcurrentRequests": 9,
		"maxModels":             4,
		"labels":                []string{"gpu:nvidia"},
		"backendType":           "llama_cpp",
	})
	if code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200", code)
	}

	after := findBackend(t, s, "llama-merge")
	if after.CppWorkerApiToken != "server-side-token" {
		t.Errorf("cppWorkerApiToken = %q, want %q — сохранение формы стёрло токен, "+
			"последующие reload-запросы к cppworker получат 401",
			after.CppWorkerApiToken, "server-side-token")
	}
	// Переданные поля применились.
	if after.Weight != 2 || after.MaxConcurrentReqs != 9 || after.MaxModels != 4 {
		t.Errorf("переданные поля не применены: weight=%d maxConcurrent=%d maxModels=%d",
			after.Weight, after.MaxConcurrentReqs, after.MaxModels)
	}
}

// TestR65d_BackendUpdate_ExplicitTokenUpdateAndClear — токен можно задать
// явно, а очистить — маркером "-" (пустая строка означает «не менять»).
func TestR65d_BackendUpdate_ExplicitTokenUpdateAndClear(t *testing.T) {
	s := authEnabledServer(t, backendForUpdate())

	// Явная установка нового токена.
	putBackend(t, s, "llama-merge", map[string]interface{}{
		"name": "Merge Test", "host": "10.1.2.3",
		"cppWorkerApiToken": "new-token",
	})
	if got := findBackend(t, s, "llama-merge").CppWorkerApiToken; got != "new-token" {
		t.Errorf("cppWorkerApiToken = %q, want new-token", got)
	}

	// Пустая строка = «не менять».
	putBackend(t, s, "llama-merge", map[string]interface{}{
		"name": "Merge Test", "host": "10.1.2.3",
		"cppWorkerApiToken": "",
	})
	if got := findBackend(t, s, "llama-merge").CppWorkerApiToken; got != "new-token" {
		t.Errorf("cppWorkerApiToken = %q, want new-token (пустая строка не должна стирать)", got)
	}

	// Явная очистка маркером "-".
	putBackend(t, s, "llama-merge", map[string]interface{}{
		"name": "Merge Test", "host": "10.1.2.3",
		"cppWorkerApiToken": "-",
	})
	if got := findBackend(t, s, "llama-merge").CppWorkerApiToken; got != "" {
		t.Errorf("cppWorkerApiToken = %q, want empty (маркер '-' должен очищать)", got)
	}
}

// TestR65d_BackendUpdate_PreservesEngine — Engine не отправляется из WebUI,
// поэтому должен сохраняться.
func TestR65d_BackendUpdate_PreservesEngine(t *testing.T) {
	s := authEnabledServer(t, backendForUpdate())

	putBackend(t, s, "llama-merge", map[string]interface{}{
		"name": "Merge Test", "host": "10.1.2.3", "cppWorkerPort": 18092,
	})

	if got := findBackend(t, s, "llama-merge").Engine; got != types.EngineLlamaCPP {
		t.Errorf("engine = %q, want %q (поле не приходит из WebUI и обнулялось)",
			got, types.EngineLlamaCPP)
	}
}

// TestR65e_GetBackend_DoesNotLeakCppWorkerToken — R65e (2026-09-20).
//
// Дефект: в fallback-ветке `getBackend` (когда метрик для бэкенда ещё нет)
// отдавался types.Backend ЦЕЛИКОМ — вместе с cppWorkerApiToken. Токен нужен
// балансеру для авторизации на cppworker; клиенту, который просто читает
// карточку бэкенда, он не нужен.
//
// Замечание о достижимости: `GetClusterState()` перечисляет все записи
// `p.backends`, а `RemoveBackend` удаляет бэкенд из обоих хранилищ — поэтому
// сегодня fallback практически недостижим, и утечка через живой HTTP-запрос
// не воспроизводится. Правка оставлена как защита от «оживления» ветки
// (например, если появится backend, зарегистрированный только в конфиге),
// и фиксируется этот тест: он проверяет наблюдаемый контракт — в ответе нет
// секрета, ответ не пустой, живой конфиг не мутирован.
func TestR65e_GetBackend_DoesNotLeakCppWorkerToken(t *testing.T) {
	s := authEnabledServer(t, backendForUpdate())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/backends/llama-merge", nil)
	req.Header.Set("X-API-Token", "r65d-secret-token")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/backends/llama-merge: код %d, тело %s", rec.Code, rec.Body.String())
	}

	raw := rec.Body.String()
	if strings.Contains(raw, "server-side-token") {
		t.Errorf("ответ содержит значение cppWorkerApiToken — утечка секрета: %s", raw)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (тело %s)", err, raw)
	}
	if tok, ok := resp["cppWorkerApiToken"]; ok && tok != "" {
		t.Errorf("cppWorkerApiToken = %v в ответе, ожидалось отсутствие/пусто", tok)
	}

	// Ответ не должен стать пустым: остальные поля нужны WebUI.
	// (Ветка с метриками отдаёт BackendMetrics — там есть id и host, но нет name.)
	if got, _ := resp["id"].(string); got != "llama-merge" {
		t.Errorf("id = %v, want llama-merge", resp["id"])
	}
	if got, _ := resp["host"].(string); got != "10.1.2.3" {
		t.Errorf("host = %v, want 10.1.2.3 — редактирование ответа сломало конфиг", resp["host"])
	}

	// Живой конфиг не мутирован.
	if got := findBackend(t, s, "llama-merge").CppWorkerApiToken; got != "server-side-token" {
		t.Errorf("живой CppWorkerApiToken = %q, want server-side-token — "+
			"редактирование копии затронуло оригинал, балансер потеряет авторизацию", got)
	}
}
