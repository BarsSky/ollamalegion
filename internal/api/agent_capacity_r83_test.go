//go:build llama_stub

// agent_capacity_r83_test.go — R83 (2026-09-28).
//
// ДЕФЕКТ, КОТОРЫЙ ЗДЕСЬ ЗАФИКСИРОВАН. Регистрация агента создавала бэкенд с
// жёсткой вместимостью `MaxConcurrentReqs: 10` — для ЛЮБОГО типа бэкенда. Для
// llama.cpp это завышение: n_parallel в C-bridge равен 1, то есть cppworker
// физически обслуживает один запрос за раз. Балансер при этом показывал
// `maxConcurrentRequests=10`, admission-очередь считала доступными десять
// слотов, и клиент (Cline/OpenWebUI) получал отказы и зависания там, где нужно
// было просто подождать. Обход типо-зависимого дефолта в AddBackend
// (llama_cpp → 1) выглядел как безобидная константа, а на деле ломал связку.
//
// Проверяются четыре свойства:
//  1. агент не сообщил вместимость → дефолт по типу (llama_cpp = 1);
//  2. агент сообщил реальный n_parallel (>1) → уважается именно он;
//  3. ollama-бэкенд сохраняет прежний дефолт (10);
//  4. ПОВТОРНАЯ регистрация обновляет вместимость, а не сохраняет старую
//     (найдено на живой стойке: агент пересоздан с
//     AGENT_MAX_CONCURRENT_REQUESTS=1, балансер продолжал отдавать 10).
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"
)

// seedBackendMaxConcurrent — имитация унаследованной вместимости из state.json
// старой сборки.
//
// Пишем через Proxy.MutateBackend (под p.mu+state.mu), а НЕ присваиванием в
// указатель из GetBackend: незалоченная запись гоняет с фоновыми читателями
// (GetAllBackends из poller'а и каталога моделей) и роняет `go test -race`.
func seedBackendMaxConcurrent(t *testing.T, s *Server, backendID string, maxConcurrent int) {
	t.Helper()
	if err := s.proxy.MutateBackend(backendID, func(b *types.Backend) {
		b.MaxConcurrentReqs = maxConcurrent
	}); err != nil {
		t.Fatalf("seed maxConcurrentReqs for %s: %v", backendID, err)
	}
}

func newAgentCapacityServer(t *testing.T) *Server {
	t.Helper()
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "127.0.0.1", Port: 8080, APIPort: 8081},
	}
	p := balancer.NewProxy(cfg)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = p.Shutdown(ctx)
	})
	return NewServer(p, cfg, nil)
}

// registerAgentPayload собирает тело регистрации так, как это делает агент
// (internal/agent/collector_register.go).
func registerAgentPayload(agentID, backendType string, maxConcurrent int) []byte {
	return registerAgentPayloadWithToken(agentID, backendType, maxConcurrent, "")
}

// registerAgentPayloadWithToken — то же, но с токеном cppworker.
// R83 (2026-09-28): агент обязан сообщать балансеру токен, который ждёт
// cppworker; без него балансер удаляет заголовок авторизации при проксировании
// (R65d), и правка параметров модели из WebUI получает 401.
func registerAgentPayloadWithToken(agentID, backendType string, maxConcurrent int, cppToken string) []byte {
	body := map[string]interface{}{
		"agentId":  agentID,
		"hostname": "test-host",
		// host задан явно и ОДИНАКОВО для всех бэкендов: dedup-путь
		// «агент привязывается к уже зарегистрированному cppworker» ищет
		// пару (host, cppWorkerPort), и при пустом host он не срабатывает.
		"host":                  "cppworker-gpu",
		"ollamaPort":            0,
		"agentPort":             18032,
		"cppWorkerPort":         18092,
		"gpuCount":              1,
		"name":                  agentID,
		"labels":                []string{"linux"},
		"weight":                1,
		"backendType":           backendType,
		"nodeLabels":            "gpu,llamacpp",
		"maxConcurrentRequests": maxConcurrent,
	}
	if cppToken != "" {
		body["cppWorkerApiToken"] = cppToken
	}
	data, _ := json.Marshal(body)
	return data
}

func doRegisterAgent(t *testing.T, s *Server, payload []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.agentRegisterHandler(rec, req)
	return rec
}

// TestR83_AgentRegister_LlamaCppGetsTypeDefault — без явной вместимости
// llama.cpp-бэкенд обязан получить дефолт по типу (1), а не 10.
func TestR83_AgentRegister_LlamaCppGetsTypeDefault(t *testing.T) {
	s := newAgentCapacityServer(t)
	const id = "r83-cap-default"

	// -1 повторяет реальный дефолт агента: «не задано».
	rec := doRegisterAgent(t, s, registerAgentPayload(id, string(types.BackendTypeLlamaCpp), -1))
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("регистрация вернула %d: %s", rec.Code, rec.Body.String())
	}

	backend := s.proxy.GetBackend(id)
	if backend == nil {
		t.Fatal("бэкенд не зарегистрирован")
	}
	if backend.MaxConcurrentReqs != 1 {
		t.Errorf("MaxConcurrentReqs = %d, want 1 (дефолт для llama_cpp: n_parallel=1). "+
			"10 здесь — тот самый дефект: балансер показывал десять свободных слотов на "+
			"однослотовый cppworker", backend.MaxConcurrentReqs)
	}
}

// TestR83_AgentRegister_ExplicitCapacityWins — если агент сообщил реальный
// n_parallel (>1), вместимость должна быть именно такой.
func TestR83_AgentRegister_ExplicitCapacityWins(t *testing.T) {
	s := newAgentCapacityServer(t)
	const id = "r83-cap-explicit"

	rec := doRegisterAgent(t, s, registerAgentPayload(id, string(types.BackendTypeLlamaCpp), 3))
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("регистрация вернула %d: %s", rec.Code, rec.Body.String())
	}
	backend := s.proxy.GetBackend(id)
	if backend == nil {
		t.Fatal("бэкенд не зарегистрирован")
	}
	if backend.MaxConcurrentReqs != 3 {
		t.Errorf("MaxConcurrentReqs = %d, want 3 (агент сообщил реальный n_parallel)", backend.MaxConcurrentReqs)
	}
}

// TestR83_AgentRegister_OllamaKeepsLegacyDefault — для Ollama-бэкенда дефолт
// остаётся прежним (10): параллелизм там обслуживает сам Ollama, и снижать его
// до единицы значило бы ломать многопоточные клиенты.
func TestR83_AgentRegister_OllamaKeepsLegacyDefault(t *testing.T) {
	s := newAgentCapacityServer(t)
	const id = "r83-cap-ollama"

	rec := doRegisterAgent(t, s, registerAgentPayload(id, string(types.BackendTypeOllama), -1))
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("регистрация вернула %d: %s", rec.Code, rec.Body.String())
	}
	backend := s.proxy.GetBackend(id)
	if backend == nil {
		t.Fatal("бэкенд не зарегистрирован")
	}
	if backend.MaxConcurrentReqs != 10 {
		t.Errorf("MaxConcurrentReqs = %d, want 10 (legacy-дефолт для ollama)", backend.MaxConcurrentReqs)
	}
}

// TestR83_AgentRegister_ReregisterUpdatesCapacity — повторная регистрация того
// же агента с новой вместимостью ОБЯЗАНА её применить.
//
// Живой сценарий (2026-09-28, A10): оператор пересоздал агент с
// AGENT_MAX_CONCURRENT_REQUESTS=1, но балансер продолжал показывать
// maxConcurrentRequests=10, потому что ветка «бэкенд уже существует» брала
// MaxConcurrentReqs из существующей записи и игнорировала payload регистрации.
// Хуже того, 10 успевало сохраниться в state.json и восстанавливалось при
// рестарте балансера — то есть дефект переживал и починку стека, и перезапуск.
//
// Завышенное значение выставляется напрямую: ровно так оно и попадает на
// стенд — из ранее сохранённого state.json, записанного сборкой, где создание
// бэкенда ещё использовало константу 10. Без этого тест «зеленел» бы на любом
// коде: при создании дефолт llama_cpp уже равен 1.
func TestR83_AgentRegister_ReregisterUpdatesCapacity(t *testing.T) {
	s := newAgentCapacityServer(t)
	const id = "r83-cap-reregister"

	rec := doRegisterAgent(t, s, registerAgentPayload(id, string(types.BackendTypeLlamaCpp), -1))
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("первая регистрация вернула %d: %s", rec.Code, rec.Body.String())
	}

	// Имитируем унаследованное завышенное значение (state.json старой сборки).
	// Пишем через MutateBackend: прямое присваивание в указатель из GetBackend —
	// незалоченная запись, которую ловит `go test -race` (см. MutateBackend).
	seedBackendMaxConcurrent(t, s, id, 10)

	// Агент пересоздан с реальным n_parallel=1 — регистрация обязана это донести.
	rec = doRegisterAgent(t, s, registerAgentPayload(id, string(types.BackendTypeLlamaCpp), 1))
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("повторная регистрация вернула %d: %s", rec.Code, rec.Body.String())
	}

	backend := s.proxy.GetBackend(id)
	if backend == nil {
		t.Fatal("бэкенд пропал после повторной регистрации")
	}
	if backend.MaxConcurrentReqs != 1 {
		t.Errorf("после повторной регистрации MaxConcurrentReqs = %d, want 1 "+
			"(агент сообщил n_parallel=1; сохранение старого значения — это дефект, "+
			"из-за которого балансер держал десять слотов на однослотовом cppworker)",
			backend.MaxConcurrentReqs)
	}
}

// TestR83_AgentRegister_ReregisterKeepsCapacityWhenUnset — если агент
// вместимость НЕ сообщил (<=0), повторная регистрация не должна сбрасывать
// уже известное значение. Иначе старые сборки агента (без поля в payload)
// обнуляли бы вместимость при каждом своём рестарте.
func TestR83_AgentRegister_ReregisterKeepsCapacityWhenUnset(t *testing.T) {
	s := newAgentCapacityServer(t)
	const id = "r83-cap-reregister-unset"

	rec := doRegisterAgent(t, s, registerAgentPayload(id, string(types.BackendTypeLlamaCpp), 2))
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("первая регистрация вернула %d: %s", rec.Code, rec.Body.String())
	}

	rec = doRegisterAgent(t, s, registerAgentPayload(id, string(types.BackendTypeLlamaCpp), -1))
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("повторная регистрация вернула %d: %s", rec.Code, rec.Body.String())
	}

	backend := s.proxy.GetBackend(id)
	if backend == nil {
		t.Fatal("бэкенд пропал после повторной регистрации")
	}
	if backend.MaxConcurrentReqs != 2 {
		t.Errorf("MaxConcurrentReqs = %d, want 2 (агент не сообщил значение — "+
			"прежняя вместимость должна сохраниться)", backend.MaxConcurrentReqs)
	}
}

// TestR83_AgentRegister_AttachAdoptsCapacity — третий путь, которым агент
// попадает в балансер: привязка к УЖЕ зарегистрированному cppworker-бэкенду по
// (host, cppWorkerPort). Именно он используется в bundled-режиме, и именно он
// на живом стенде не приводил вместимость к n_parallel узла: завышенное
// значение из state.json старой сборки возвращалось в /api/v1/backends при
// каждом запуске, хотя агент сообщал 1.
func TestR83_AgentRegister_AttachAdoptsCapacity(t *testing.T) {
	s := newAgentCapacityServer(t)
	const cppID = "r83-attach-cppworker"
	const agentID = "r83-attach-agent"

	// 1. cppworker регистрируется сам (Go-side регистрация) — как в старых
	//    стендах, где отсюда и взялась вместимость 10.
	rec := doRegisterAgent(t, s, registerAgentPayload(cppID, string(types.BackendTypeLlamaCpp), -1))
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("регистрация cppworker вернула %d: %s", rec.Code, rec.Body.String())
	}
	stale := s.proxy.GetBackend(cppID)
	if stale == nil {
		t.Fatal("cppworker-бэкенд не зарегистрирован")
	}
	seedBackendMaxConcurrent(t, s, cppID, 10) // унаследованное завышенное значение

	// 2. Агент приходит на тот же (host, cppWorkerPort) с реальным n_parallel=1.
	rec = doRegisterAgent(t, s, registerAgentPayload(agentID, string(types.BackendTypeLlamaCpp), 1))
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("регистрация агента вернула %d: %s", rec.Code, rec.Body.String())
	}

	backend := s.proxy.GetBackend(cppID)
	if backend == nil {
		t.Fatal("cppworker-бэкенд пропал после attach")
	}
	if !backend.HasAgent {
		t.Error("агент не привязался к cppworker-бэкенду — тест проверяет не тот путь")
	}
	if backend.MaxConcurrentReqs != 1 {
		t.Errorf("MaxConcurrentReqs = %d, want 1 (агент сообщил n_parallel=1 при attach; "+
			"сохранённое 10 — дефект, из-за которого балансер держал десять слотов "+
			"на однослотовом cppworker даже после пересборки агента)",
			backend.MaxConcurrentReqs)
	}
}

// TestR83_AgentToken_AttachAdoptsCppWorkerToken — агент сообщает токен cppworker,
// и балансер ОБЯЗАН его сохранить, иначе не сможет авторизоваться на защищённых
// эндпоинтах cppworker.
//
// Живой сценарий (2026-09-28, A10): правка параметров модели в WebUI через агента
// падала с 401 «invalid or missing API token» при ОДИНАКОВОМ токене во всех .env.
// Причина: в bundled-стеке Go-side регистрация cppworker выключена, бэкенд создаёт
// агент, а токен умела отдавать только cppworker-регистрация. Backend оставался
// без токена, и proxyToCppWorker (R65d) при пустом токене УДАЛЯЕТ заголовок
// авторизации — «чтобы не отправлять чужой секрет».
func TestR83_AgentToken_AttachAdoptsCppWorkerToken(t *testing.T) {
	s := newAgentCapacityServer(t)
	const cppID = "r83-token-cppworker"
	const agentID = "r83-token-agent"
	const sharedToken = "shared-stack-token"

	rec := doRegisterAgent(t, s, registerAgentPayload(cppID, string(types.BackendTypeLlamaCpp), -1))
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("регистрация cppworker вернула %d: %s", rec.Code, rec.Body.String())
	}

	// Агент приходит вторым (dedup-attach) и приносит токен cppworker.
	rec = doRegisterAgent(t, s, registerAgentPayloadWithToken(
		agentID, string(types.BackendTypeLlamaCpp), 1, sharedToken))
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("регистрация агента вернула %d: %s", rec.Code, rec.Body.String())
	}

	backend := s.proxy.GetBackend(cppID)
	if backend == nil {
		t.Fatal("cppworker-бэкенд пропал после attach")
	}
	if backend.CppWorkerApiToken != sharedToken {
		t.Errorf("CppWorkerApiToken = %q, want %q — без него балансер удаляет заголовок "+
			"авторизации при проксировании на cppworker, и WebUI получает 401 "+
			"«invalid or missing API token» при одинаковом токене во всех .env",
			backend.CppWorkerApiToken, sharedToken)
	}
}

// TestR83_AgentToken_CreateAdoptsCppWorkerToken — тот же токен, но на пути
// СОЗДАНИЯ бэкенда (агент пришёл первым).
func TestR83_AgentToken_CreateAdoptsCppWorkerToken(t *testing.T) {
	s := newAgentCapacityServer(t)
	const id = "r83-token-create"
	const sharedToken = "shared-stack-token"

	rec := doRegisterAgent(t, s, registerAgentPayloadWithToken(
		id, string(types.BackendTypeLlamaCpp), 1, sharedToken))
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("регистрация вернула %d: %s", rec.Code, rec.Body.String())
	}

	backend := s.proxy.GetBackend(id)
	if backend == nil {
		t.Fatal("бэкенд не зарегистрирован")
	}
	if backend.CppWorkerApiToken != sharedToken {
		t.Errorf("CppWorkerApiToken = %q, want %q (создание бэкенда обязано принять токен агента)",
			backend.CppWorkerApiToken, sharedToken)
	}
}

// TestR83_AgentToken_RegistrationResponseHidesSecret — токен не должен утекать
// в тело ответа на регистрацию.
func TestR83_AgentToken_RegistrationResponseHidesSecret(t *testing.T) {
	s := newAgentCapacityServer(t)
	const id = "r83-token-response"
	const sharedToken = "super-secret-token"

	rec := doRegisterAgent(t, s, registerAgentPayloadWithToken(
		id, string(types.BackendTypeLlamaCpp), 1, sharedToken))
	if strings.Contains(rec.Body.String(), sharedToken) {
		t.Errorf("ответ регистрации содержит токен открытым текстом: %s", rec.Body.String())
	}

	// Повторная регистрация (ветка «updated») тоже отвечает телом с бэкендом.
	rec = doRegisterAgent(t, s, registerAgentPayloadWithToken(
		id, string(types.BackendTypeLlamaCpp), 1, sharedToken))
	if strings.Contains(rec.Body.String(), sharedToken) {
		t.Errorf("ответ повторной регистрации содержит токен открытым текстом: %s", rec.Body.String())
	}
}
