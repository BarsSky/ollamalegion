package api

// R81: хвосты P4 на уровне HTTP-слоя.
//
//  1. Heartbeat агента НЕ владеет health-статусом бэкенда. Замер P4 (R80)
//     показал: cppworker убит, но агент (отдельный контейнер) продолжает слать
//     heartbeat и безусловный UpdateBackendStatus(..., Healthy) возвращал бэкенд
//     в пул — 1 запрос из 3 уходил на мёртвую копию и висел до клиентского
//     таймаута (120 с). Теперь статусом владеет health-check, а агент отмечает
//     только факт контакта (HasAgent/LastAgentContact).
//
//  2. Событие cppworker'а «модель загружена» подтверждает реплику немедленно
//     (LOADING→LOADED), не дожидаясь 30-секундного тика поллера метрик.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

func TestAgentHeartbeatDoesNotOwnHealthStatus_R81(t *testing.T) {
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{{
		ID: "r81-a", Host: "localhost", CppWorkerPort: 18092,
		Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
	}})
	defer cleanup()

	// Имитируем вердикт health-check: cppworker не отвечает.
	s.proxy.UpdateBackendStatus("r81-a", types.StatusUnhealthy)

	body := `{"weight":1,"maxConcurrentRequests":1,"maxModels":2}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/heartbeat", strings.NewReader(body))
	req.Header.Set("X-Agent-ID", "r81-a")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.agentHeartbeatHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat: HTTP %d (%s)", rec.Code, rec.Body.String())
	}

	if got := s.proxy.GetBackend("r81-a"); got == nil {
		t.Fatal("бэкенд пропал")
	} else if got.Status != types.StatusUnhealthy {
		t.Fatalf("heartbeat вернул бэкенд в статус %q — агент не должен владеть health-статусом",
			got.Status)
	}
	// Контакт агента при этом отмечен (иначе UI/health-check не увидят, что агент жив).
	b := s.proxy.GetBackend("r81-a")
	if !b.HasAgent {
		t.Error("HasAgent не выставлен heartbeat'ом")
	}
	if time.Since(b.LastAgentContact) > time.Minute {
		t.Errorf("LastAgentContact не обновлён: %s", b.LastAgentContact)
	}

	// Успешный health-check возвращает бэкенд в пул — владелец статуса именно он.
	s.proxy.UpdateBackendStatus("r81-a", types.StatusHealthy)
	if got := s.proxy.GetBackend("r81-a").Status; got != types.StatusHealthy {
		t.Fatalf("после health-check статус %q, ожидался healthy", got)
	}
}

func TestAgentHeartbeatByBackendIDDoesNotOwnHealthStatus_R81(t *testing.T) {
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{{
		ID: "r81-b", Host: "localhost", CppWorkerPort: 18092,
		Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy, HasAgent: true, AgentID: "r81-b",
	}})
	defer cleanup()

	s.proxy.UpdateBackendStatus("r81-b", types.StatusUnhealthy)

	body := `{"weight":1,"maxConcurrentRequests":1,"maxModels":2,"agentPort":18032}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/backends/r81-b/heartbeat", strings.NewReader(body))
	req.Header.Set("X-Agent-ID", "r81-b")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.agentBackendHeartbeatHandler(rec, req, "r81-b")
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat: HTTP %d (%s)", rec.Code, rec.Body.String())
	}
	if got := s.proxy.GetBackend("r81-b").Status; got != types.StatusUnhealthy {
		t.Fatalf("heartbeat по backendID вернул статус %q — должен остаться unhealthy", got)
	}
}

func TestLlamaModelLoadedConfirmsReplica_R81(t *testing.T) {
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{
		{ID: "r81-c1", Host: "localhost", CppWorkerPort: 18092,
			Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy},
		{ID: "r81-c2", Host: "localhost", CppWorkerPort: 18093,
			Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy},
	})
	defer cleanup()

	// Политика replicated → группа на две копии; инстансов пока нет (модель не
	// загружена: в тестовой среде метрик нет, warmup не проходит).
	s.proxy.SetPlacementSettings(types.PlacementSettings{
		Enabled: true,
		Models: []types.PlacementModelRule{{
			Model:    "r81-model",
			Strategy: string(types.PlacementReplicated),
		}},
	})

	event := `{"backendId":"r81-c1","model":"r81-model","contextSize":4096}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/llama-model-loaded",
		bytes.NewBufferString(event))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleLlamaModelLoaded(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("llama-model-loaded: HTTP %d (%s)", rec.Code, rec.Body.String())
	}

	// Реплика подтверждена событием — сразу, без опроса метрик.
	status := s.proxy.PlacementReplicationStatus()
	groups, ok := status["groups"].(map[string]interface{})
	if !ok {
		t.Fatalf("в статусе репликации нет групп: %v", status)
	}
	entry, ok := groups["r81-model"].(map[string]interface{})
	if !ok {
		t.Fatalf("нет группы r81-model: %v", groups)
	}
	cands, _ := entry["candidates"].([]string)
	if len(cands) != 1 || cands[0] != "r81-c1" {
		t.Fatalf("кандидаты %v, ожидалось [r81-c1] сразу после события", cands)
	}

	// Событие выгрузки убирает инстанс так же сразу.
	unload := `{"backendId":"r81-c1","model":"r81-model"}`
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/internal/llama-model-unloaded",
		bytes.NewBufferString(unload))
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	s.handleLlamaModelUnloaded(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("llama-model-unloaded: HTTP %d (%s)", rec2.Code, rec2.Body.String())
	}
	status = s.proxy.PlacementReplicationStatus()
	entry = status["groups"].(map[string]interface{})["r81-model"].(map[string]interface{})
	if cands, _ := entry["candidates"].([]string); len(cands) != 0 {
		t.Fatalf("после выгрузки кандидаты %v, ожидалось пусто", cands)
	}
}
