// handlers_agents_dedup_test.go — tests for agent registration dedup logic.
//
// Bug context (2026-08-09):
//
//	В bundled-режиме (cppworker + agent в одном docker-compose) при первом
//	старте агент регистрировался как standalone бэкенд (id = agentId), потому
//	что dedup-логики ещё не было. Затем cppworker регистрировался отдельно,
//	и при следующей регистрации агента dedup attach'ил его к правильному
//	бэкенду, но stale standalone оставался в /api/v1/backends.
//
// Round 30 fix:
//
//	При успешном attach агента к existing backend — удаляем stale standalone
//	с тем же agentId.
//
// Покрывает:
//   - Dedup attaches agent to cppworker backend AND removes stale standalone
//   - Standalone-only mode (no cppworker) is still allowed (legacy)
//   - Update path (BackendExists by ID) doesn't trigger dedup cleanup
package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"ollama-loadbalancer/pkg/types"

	"github.com/stretchr/testify/assert"
)

// TestAgentRegister_DedupRemovesStaleStandalone —
// Scenario:
//  1. Cppworker backend already exists at (host=cppworker-gpu, port=18092)
//  2. Stale standalone backend with same agentId exists from previous run
//  3. Agent tries to register with cppWorkerPort=18092
//     → Must attach to cppworker AND remove the stale standalone
//     → After: only 1 backend remains (the cppworker with agent attached)
func TestAgentRegister_DedupRemovesStaleStandalone(t *testing.T) {
	server, _, proxy := createTestServer(t)
	defer server.Close()

	// Pre-populate state: cppworker backend + stale standalone with same agentId
	host := "cppworker-gpu"
	cppWorkerPort := 18092
	agentID := "cppworker-gpu-bundled-agent"

	// 1. Cppworker backend (proper one)
	cppworkerBackend := types.Backend{
		ID:            "cppworker-gpu-bundled",
		Name:          "CppWorker GPU",
		Host:          host,
		OllamaPort:    0,
		AgentPort:     18032,
		CppWorkerPort: cppWorkerPort,
		Weight:        1,
		Labels:        []string{"llamacpp", "gpu"},
		Status:        types.StatusHealthy,
		Type:          types.BackendTypeLlamaCpp,
	}
	assert.NoError(t, proxy.AddBackend(cppworkerBackend))

	// 2. Stale standalone backend (created at 2026-08-06 before dedup existed)
	staleStandalone := types.Backend{
		ID:            agentID, // SAME ID as agent — это ключевой момент
		Host:          host,
		OllamaPort:    11434,
		AgentPort:     0,
		CppWorkerPort: cppWorkerPort,
		Weight:        1,
		Status:        types.StatusHealthy,
		Type:          types.BackendTypeLlamaCpp,
	}
	assert.NoError(t, proxy.AddBackend(staleStandalone))

	// Sanity: cppworker + stale standalone exist
	assert.True(t, proxy.BackendExists("cppworker-gpu-bundled"),
		"precondition: cppworker-gpu-bundled should exist")
	assert.True(t, proxy.BackendExists(agentID),
		"precondition: stale standalone should exist by agentId")

	// 3. Agent registers again
	registerReq := map[string]interface{}{
		"agentId":       agentID,
		"host":          host,
		"ollamaPort":    11434,
		"agentPort":     18032,
		"cppWorkerPort": cppWorkerPort,
		"backendType":   "llama_cpp",
		"name":          "CppWorker Agent",
	}
	body, _ := json.Marshal(registerReq)
	resp, err := http.Post(server.URL+"/api/v1/agents/register", "application/json", bytes.NewBuffer(body))
	assert.NoError(t, err)
	defer resp.Body.Close()

	// 4. Verify response is "attached" (not "created")
	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"agent should attach to existing backend, not be created anew")

	var respBody map[string]interface{}
	assert.NoError(t, json.NewDecoder(resp.Body).Decode(&respBody))
	assert.Equal(t, "attached", respBody["action"],
		"action should be 'attached' (dedup hit)")
	assert.Equal(t, "cppworker-gpu-bundled", respBody["backendId"],
		"should attach to cppworker backend, not standalone")

	// 5. CRITICAL: stale standalone should be REMOVED
	assert.False(t, proxy.BackendExists(agentID),
		"stale standalone backend with same agentId should be REMOVED after dedup attach")

	// 6. cppworker backend should remain
	assert.True(t, proxy.BackendExists("cppworker-gpu-bundled"),
		"cppworker-gpu-bundled should remain after dedup")

	// 7. The cppworker backend should have agent attached
	allBackends := proxy.GetAllBackends()
	var remaining *types.Backend
	for i := range allBackends {
		if allBackends[i].ID == "cppworker-gpu-bundled" {
			remaining = &allBackends[i]
			break
		}
	}
	if assert.NotNil(t, remaining, "cppworker-gpu-bundled should be present after dedup") {
		assert.True(t, remaining.HasAgent, "cppworker backend should have agent attached")
		assert.Equal(t, agentID, remaining.AgentID, "agentId should match")
		assert.Equal(t, 18032, remaining.AgentPort, "agentPort should be set from register req")
	}
}

// TestAgentRegister_NoCppworker_StandaloneAllowed —
// Scenario: agent registers but no cppworker exists yet → standalone created
// (legacy mode for non-bundled deployments).
func TestAgentRegister_NoCppworker_StandaloneAllowed(t *testing.T) {
	server, _, proxy := createTestServer(t)
	defer server.Close()

	agentID := "solo-agent-1"
	host := "solo-host-1"
	cppWorkerPort := 18092

	// Register agent (no existing cppworker backend)
	registerReq := map[string]interface{}{
		"agentId":       agentID,
		"host":          host,
		"cppWorkerPort": cppWorkerPort,
		"backendType":   "llama_cpp",
	}
	body, _ := json.Marshal(registerReq)
	resp, err := http.Post(server.URL+"/api/v1/agents/register", "application/json", bytes.NewBuffer(body))
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusCreated, resp.StatusCode,
		"first register (no existing cppworker) should create standalone with 201")

	var respBody map[string]interface{}
	assert.NoError(t, json.NewDecoder(resp.Body).Decode(&respBody))
	assert.Equal(t, "created", respBody["action"])

	// Standalone exists
	assert.True(t, proxy.BackendExists(agentID))
}

// (additional tests below)

// TestAgentRegister_DedupByAgentId_StandaloneIsAttachedToItself —
// Scenario: agentId matches existing backend ID (no cppWorkerPort).
// Should UPDATE the existing backend, not create new one.
// (Legacy path — still works as before).
func TestAgentRegister_DedupByAgentId_StandaloneIsAttachedToItself(t *testing.T) {
	server, _, proxy := createTestServer(t)
	defer server.Close()

	agentID := "legacy-agent-1"

	// First register creates backend
	firstReq := map[string]interface{}{
		"agentId":     agentID,
		"host":        "legacy-host",
		"backendType": "ollama",
	}
	body, _ := json.Marshal(firstReq)
	resp1, err := http.Post(server.URL+"/api/v1/agents/register", "application/json", bytes.NewBuffer(body))
	assert.NoError(t, err)
	resp1.Body.Close()
	assert.Equal(t, http.StatusCreated, resp1.StatusCode)

	assert.True(t, proxy.BackendExists(agentID), "agentId should be registered")

	// Second register with same agentId → UPDATE (not create)
	secondReq := map[string]interface{}{
		"agentId":     agentID,
		"host":        "legacy-host-updated",
		"backendType": "ollama",
		"name":        "Updated Name",
	}
	body2, _ := json.Marshal(secondReq)
	resp2, err := http.Post(server.URL+"/api/v1/agents/register", "application/json", bytes.NewBuffer(body2))
	assert.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)

	var respBody map[string]interface{}
	assert.NoError(t, json.NewDecoder(resp2.Body).Decode(&respBody))
	assert.Equal(t, "updated", respBody["action"])

	// Still 1 backend with this agentId (no duplicate created)
	assert.True(t, proxy.BackendExists(agentID),
		"agentId should still exist (updated in-place)")
	// Count: only 1 backend with this ID (the update shouldn't create another)
	allBackends := proxy.GetAllBackends()
	countWithID := 0
	for _, b := range allBackends {
		if b.ID == agentID {
			countWithID++
		}
	}
	assert.Equal(t, 1, countWithID,
		"update path should not create a duplicate backend with same ID")
}

// TestAgentRegister_ImageBackend_GetsHasAgent — R-Image (2026-10-07).
//
// ЖИВОЙ ДЕФЕКТ. Image-бэкенд (sdworker) регистрирует себя САМ и создаёт запись
// с hasAgent=false. Когда под тем же ID регистрировался агент, ветка «backend
// exists» собирала обновление как `AgentID/HasAgent: existing.*` — то есть
// повторная регистрация НИЧЕГО не могла изменить. Следствие на стенде:
// у imageworker в /api/v1/backends стояло hasAgent=false и agentPort=18032
// (порт ЧУЖОГО контейнера), метрики GPU/VRAM не приезжали вовсе, а страница
// модели в WebUI показывала нули.
//
// ЧТО ПРОВЕРЯЕМ. После регистрации агента под ID существующего image-бэкенда:
//   - hasAgent становится true;
//   - agentPort берётся из регистрации агента (не остаётся чужим);
//   - imagePort НЕ теряется (иначе EffectiveImagePort() ушёл бы в fallback);
//   - дубликата записи не появляется.
func TestAgentRegister_ImageBackend_GetsHasAgent(t *testing.T) {
	server, _, proxy := createTestServer(t)
	defer server.Close()

	const (
		backendID = "imageworker"
		imagePort = 18093
		agentPort = 18033
	)

	// 1. Так выглядит запись после саморегистрации sdworker.
	assert.NoError(t, proxy.AddBackend(types.Backend{
		ID:                backendID,
		Name:              backendID,
		Host:              "imageworker",
		ImagePort:         imagePort,
		Weight:            100,
		Labels:            []string{"sdworker", "auto-registered", "image"},
		Status:            types.StatusHealthy,
		Type:              types.BackendTypeImage,
		MaxConcurrentReqs: 64,
	}))

	// 2. Регистрируется встроенный агент — ПОД ТЕМ ЖЕ ID.
	req := map[string]interface{}{
		"agentId":               backendID,
		"host":                  "imageworker",
		"backendType":           string(types.BackendTypeImage),
		"agentPort":             agentPort,
		"imagePort":             imagePort,
		"maxConcurrentRequests": 64,
	}
	body, _ := json.Marshal(req)
	resp, err := http.Post(server.URL+"/api/v1/agents/register", "application/json", bytes.NewBuffer(body))
	assert.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode, "регистрация под существующим ID обновляет запись")

	updated := proxy.GetBackend(backendID)
	if updated == nil {
		t.Fatal("imageworker исчез из реестра")
	}
	assert.True(t, updated.HasAgent,
		"агент зарегистрировался — HasAgent обязан стать true, иначе метрики не поедут")
	assert.Equal(t, backendID, updated.AgentID)
	assert.Equal(t, agentPort, updated.AgentPort, "agentPort должен стать портом ВСТРОЕННОГО агента")
	assert.Equal(t, imagePort, updated.ImagePort, "imagePort не должен теряться при обновлении")
	assert.Equal(t, types.BackendTypeImage, updated.Type, "тип бэкенда не должен меняться")

	// 3. Дубликата быть не должно: запись одна.
	count := 0
	for _, b := range proxy.GetAllBackends() {
		if b.ID == backendID {
			count++
		}
	}
	assert.Equal(t, 1, count, "регистрация агента не должна создавать вторую запись")
}
