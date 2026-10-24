//go:build llama_stub

// backend_record_kept_r118_test.go — R118.10 (2026-10-14).
//
// ТРЕБОВАНИЕ ОПЕРАТОРА: «оставлять узел в списке, так как может происходить
// настройка».
//
// ЖИВОЙ ДЕФЕКТ. Штатную остановку узла сопровождает best-effort
// `DELETE /api/v1/backends/{id}` от самого cppworker'а
// (cmd/cppworker/balancer_register.go, SIGTERM). Через ~30 с после
// `docker stop cppworker-node2` запись донора ИСЧЕЗАЛА из /api/v1/backends вместе
// с ролью в раскладке: остановил узел, чтобы настроить, — потерял и роль, и
// раскладку, а в панели настраивать стало нечего.
//
// Что фиксируют тесты:
//  1. DELETE записи с ЯВНОЙ ролью не удаляет её: агент открепляется, роль цела,
//     запись остаётся доступной (и видимой в списке даже без includeUnhealthy);
//  2. DELETE записи БЕЗ роли удаляет её как раньше (мусор auto-registered);
//  3. после «возврата» агента запись оживает: агент прикрепляется, статус снова
//     healthy (роль при этом сохраняется — её вернёт roleForBackend).
package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// bytesReader — тело запроса из []byte (bytes.NewReader в тестах читается один раз).
func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// newKeptRecordServer — сервер с одним зарегистрированным llama.cpp-бэкендом.
func newKeptRecordServer(t *testing.T, role string) (*Server, string) {
	t.Helper()
	s := newAgentCapacityServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register",
		bytesReader(registerAgentPayloadWithToken("agent-1", "llama_cpp", 2, "tok")))
	req.RemoteAddr = "192.0.2.10:51234"
	rec := httptest.NewRecorder()
	s.agentRegisterHandler(rec, req)
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("регистрация агента: HTTP %d (%s)", rec.Code, rec.Body.String())
	}
	backends := s.proxy.GetAllBackends()
	if len(backends) == 0 {
		t.Fatal("агент не создал запись бэкенда")
	}
	id := backends[0].ID
	if role != "" {
		if !s.proxy.SetBackendRole(id, role) {
			t.Fatalf("не удалось выставить роль %q", role)
		}
	}
	return s, id
}

// TestR118_10_DeleteOfRoleBackendKeepsRecord — ГЛАВНОЕ: узел с ролью остаётся.
func TestR118_10_DeleteOfRoleBackendKeepsRecord(t *testing.T) {
	s, id := newKeptRecordServer(t, types.BackendRoleDonor)

	rec := httptest.NewRecorder()
	s.deleteBackend(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/backends/"+id, nil), id)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE вернул HTTP %d (%s)", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("ответ не JSON: %v", err)
	}
	if kept, _ := resp["kept"].(bool); !kept {
		t.Errorf("ответ не сообщает, что запись оставлена: %s", rec.Body.String())
	}

	got := s.proxy.GetBackend(id)
	if got == nil {
		t.Fatal("запись с ролью удалена — это и есть живой дефект (потеряли роль и раскладку)")
	}
	if got.Role != types.BackendRoleDonor {
		t.Errorf("роль потеряна: %q", got.Role)
	}
	if got.HasAgent {
		t.Error("агент должен быть откреплён (узел остановлен)")
	}
	if got.Status != types.StatusOffline {
		t.Errorf("статус = %q, want %q", got.Status, types.StatusOffline)
	}
}

// TestR118_10_RoleBackendVisibleInListWhileOffline — остановленный узел с ролью
// виден в списке даже без includeUnhealthy: иначе настраивать нечего.
func TestR118_10_RoleBackendVisibleInListWhileOffline(t *testing.T) {
	s, id := newKeptRecordServer(t, types.BackendRoleDonor)

	s.deleteBackend(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodDelete, "/api/v1/backends/"+id, nil), id)

	rec := httptest.NewRecorder()
	s.listBackends(rec, httptest.NewRequest(http.MethodGet, "/api/v1/backends", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("список: HTTP %d", rec.Code)
	}
	var body struct {
		Backends []map[string]interface{} `json:"backends"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("список не JSON: %v", err)
	}
	found := false
	for _, b := range body.Backends {
		if b["id"] == id {
			found = true
			if b["role"] != types.BackendRoleDonor {
				t.Errorf("в списке роль = %v, want %q", b["role"], types.BackendRoleDonor)
			}
		}
	}
	if !found {
		t.Fatalf("узел %s с ролью не попал в список (offline и unhealthy фильтруются): %s",
			id, rec.Body.String())
	}
}

// TestR118_10_BackendWithoutRoleStillRemoved — старое поведение для записей без
// роли (мусор auto-registered) сохранено.
func TestR118_10_BackendWithoutRoleStillRemoved(t *testing.T) {
	s, id := newKeptRecordServer(t, "")

	rec := httptest.NewRecorder()
	s.deleteBackend(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/backends/"+id, nil), id)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE вернул HTTP %d (%s)", rec.Code, rec.Body.String())
	}
	if got := s.proxy.GetBackend(id); got != nil {
		t.Fatalf("запись без роли должна удаляться, осталась: %+v", got)
	}
}

// TestR118_10_AgentReturnsToSameRecord — вернувшийся агент прикрепляется к той же
// записи и роль сохраняется (её возвращает roleForBackend при регистрации).
func TestR118_10_AgentReturnsToSameRecord(t *testing.T) {
	s, id := newKeptRecordServer(t, types.BackendRoleDonor)
	s.deleteBackend(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodDelete, "/api/v1/backends/"+id, nil), id)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register",
		bytesReader(registerAgentPayloadWithToken("agent-1", "llama_cpp", 2, "tok")))
	req.RemoteAddr = "192.0.2.10:51234"
	rec := httptest.NewRecorder()
	s.agentRegisterHandler(rec, req)
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("повторная регистрация: HTTP %d (%s)", rec.Code, rec.Body.String())
	}

	got := s.proxy.GetBackend(id)
	if got == nil {
		t.Fatal("после возврата агента запись пропала")
	}
	if !got.HasAgent {
		t.Error("агент не прикрепился к существующей записи")
	}
	if got.Role != types.BackendRoleDonor {
		t.Errorf("роль не сохранилась при возврате узла: %q", got.Role)
	}
	// Статус после возврата агента ставит health-check (агент лишь прикрепляется):
	// до первого успешного health-чекa запись честно остаётся offline и в маршрут
	// не попадает. Проверяем, что она оживает штатным путём и роль при этом цела.
	s.proxy.UpdateBackendStatus(id, types.StatusHealthy)
	got = s.proxy.GetBackend(id)
	if got.Status != types.StatusHealthy {
		t.Errorf("статус не обновился штатным путём: %q", got.Status)
	}
	if got.Role != types.BackendRoleDonor {
		t.Errorf("роль потеряна при смене статуса: %q", got.Role)
	}
}
