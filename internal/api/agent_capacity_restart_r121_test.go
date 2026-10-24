//go:build llama_stub

// agent_capacity_restart_r121_test.go — R121 (2026-10-15), пункт 1.
//
// ЖИВОЙ ДЕФЕКТ (воспроизведён на стенде, `_diag/probe-capacity.js`):
//
//	после рестарта cppworker:  effective=1 source=static
//	после рестарта балансера:  effective=2 source=static   ← заявка узла потеряна
//
// Заявка узла записывалась в СТАТИЧЕСКОЕ MaxConcurrentReqs и не помечалась как
// «от ноды», поэтому при загрузке состояния побеждала статика конфига (2).
// Агент заново не регистрируется — он регистрируется только при своём старте,
// поэтому значение оставалось 2 до ручного перезапуска воркера, а heartbeat всё
// это время нёс узлу 2 (петля «эхо» оживала).
//
// Здесь проверяются два независимых пути восстановления:
//  1. регистрация агента помечает заявку как «от ноды» (переживает рестарт);
//  2. heartbeat агента несёт СОБСТВЕННУЮ заявку узла в отдельном поле, и балансер
//     её принимает — то есть запись самовосстанавливается без рестарта агента.
package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// configSourcedBackend — привести запись к состоянию «значение из конфига»:
// статика 2, никакой заявки узла и владельца. Ровно то, что видно на стенде
// сразу после перезапуска балансера.
func configSourcedBackend(t *testing.T, s *Server, backendID string) {
	t.Helper()
	if err := s.proxy.MutateBackend(backendID, func(b *types.Backend) {
		b.MaxConcurrentReqs = 2
		b.NodeClaimedCapacity = 0
		b.RuntimeCapacityFromNode = false
		b.RuntimeMaxConcurrentRequests = 0
		b.RuntimeModelSlots = 0
	}); err != nil {
		t.Fatalf("MutateBackend(%s): %v", backendID, err)
	}
}

// heartbeatPayload собирает тело heartbeat так, как это делает агент.
func heartbeatPayload(payload map[string]interface{}) []byte {
	data, _ := json.Marshal(payload)
	return data
}

func doBackendHeartbeat(t *testing.T, s *Server, backendID, agentID string, payload []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/backends/"+backendID+"/agent/heartbeat", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agent-ID", agentID)
	rec := httptest.NewRecorder()
	s.agentBackendHeartbeatHandler(rec, req, backendID)
	return rec
}

// TestR121_AgentRegister_ClaimsNodeOwnership — регистрация агента обязана
// пометить заявку как «от ноды», иначе она живёт в статическом поле конфига и
// теряется при перезапуске балансера.
func TestR121_AgentRegister_ClaimsNodeOwnership(t *testing.T) {
	t.Setenv("LB_CAPACITY_FROM_MODEL_SLOTS", "false")
	s := newAgentCapacityServer(t)
	const id = "r121-claim-owner"

	// Первая регистрация создаёт запись, затем приводим её к «значению из конфига»
	// (как после перезапуска балансера: статика 2, владельца нет).
	rec := doRegisterAgent(t, s, registerAgentPayload(id, string(types.BackendTypeLlamaCpp), 1))
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("регистрация вернула %d: %s", rec.Code, rec.Body.String())
	}
	configSourcedBackend(t, s, id)

	// Агент регистрируется снова, заявляя реальные 1.
	rec = doRegisterAgent(t, s, registerAgentPayload(id, string(types.BackendTypeLlamaCpp), 1))
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("повторная регистрация вернула %d: %s", rec.Code, rec.Body.String())
	}

	got := s.proxy.GetBackend(id)
	if got == nil {
		t.Fatal("бэкенд пропал после повторной регистрации")
	}
	if got.NodeClaimedCapacity != 1 {
		t.Errorf("NodeClaimedCapacity = %d, want 1: заявка узла снова осела в статике "+
			"и потеряется при перезапуске балансера", got.NodeClaimedCapacity)
	}
	if !got.RuntimeCapacityFromNode {
		t.Error("RuntimeCapacityFromNode = false — запись считается «из конфига»")
	}
	if eff := got.EffectiveMaxConcurrentRequests(); eff != 1 {
		t.Errorf("EffectiveMaxConcurrentRequests = %d, want 1", eff)
	}
	if _, source := got.ResolveCapacity(); source != types.CapacitySourceNode {
		t.Errorf("source = %q, want %q (на стенде отчёт врал «static»)", source, types.CapacitySourceNode)
	}
}

// TestR121_Heartbeat_AdoptsNodeClaim — самолечение: узел сообщает свою заявку в
// heartbeat, и балансер восстанавливает вместимость БЕЗ перезапуска агента.
func TestR121_Heartbeat_AdoptsNodeClaim(t *testing.T) {
	t.Setenv("LB_CAPACITY_FROM_MODEL_SLOTS", "false")
	s := newAgentCapacityServer(t)
	const id = "r121-heartbeat-claim"

	rec := doRegisterAgent(t, s, registerAgentPayload(id, string(types.BackendTypeLlamaCpp), 1))
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("регистрация вернула %d: %s", rec.Code, rec.Body.String())
	}
	// Перезапуск балансера: значение откатилось к статике конфига.
	configSourcedBackend(t, s, id)
	if eff := s.proxy.GetBackend(id).EffectiveMaxConcurrentRequests(); eff != 2 {
		t.Fatalf("подготовка теста: effective = %d, want 2 (значение из конфига)", eff)
	}

	rec = doBackendHeartbeat(t, s, id, id, heartbeatPayload(map[string]interface{}{
		"agentId": id,
		// Собственная заявка узла — отдельное поле, а НЕ то, что балансер прислал
		// в ответе (иначе это «эхо» собственного ответа).
		"nodeMaxConcurrentRequests": 1,
		// Балансер в это же время мог прислать своё (неверное) 2 — оно не должно
		// приниматься за заявку узла.
		"maxConcurrentRequests": 2,
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat вернул %d: %s", rec.Code, rec.Body.String())
	}

	got := s.proxy.GetBackend(id)
	if got.NodeClaimedCapacity != 1 {
		t.Errorf("NodeClaimedCapacity = %d, want 1: heartbeat узла не восстановил заявку — "+
			"значит после перезапуска балансера узел снова живёт с чужим лимитом", got.NodeClaimedCapacity)
	}
	if eff := got.EffectiveMaxConcurrentRequests(); eff != 1 {
		t.Errorf("EffectiveMaxConcurrentRequests = %d, want 1 после heartbeat узла", eff)
	}
}

// TestR121_Heartbeat_IgnoresEchoedCapacity — защита от возврата петли «эхо»:
// поле maxConcurrentRequests в heartbeat — это то, что балансер САМ прислал узлу,
// и принимать его за заявку узла нельзя.
func TestR121_Heartbeat_IgnoresEchoedCapacity(t *testing.T) {
	t.Setenv("LB_CAPACITY_FROM_MODEL_SLOTS", "false")
	s := newAgentCapacityServer(t)
	const id = "r121-heartbeat-echo"

	rec := doRegisterAgent(t, s, registerAgentPayload(id, string(types.BackendTypeLlamaCpp), 1))
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("регистрация вернула %d: %s", rec.Code, rec.Body.String())
	}
	configSourcedBackend(t, s, id)

	rec = doBackendHeartbeat(t, s, id, id, heartbeatPayload(map[string]interface{}{
		"agentId":               id,
		"maxConcurrentRequests": 2, // «эхо»: ровно то, что балансер прислал узлу
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat вернул %d: %s", rec.Code, rec.Body.String())
	}

	got := s.proxy.GetBackend(id)
	if got.NodeClaimedCapacity != 0 {
		t.Errorf("NodeClaimedCapacity = %d, want 0: заявкой узла принято «эхо» "+
			"собственного ответа балансера — петля вернулась", got.NodeClaimedCapacity)
	}
}
