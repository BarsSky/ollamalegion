//go:build llama_stub

// backend_enabled_r250_test.go — R250 (2026-10-28): «не использовать бэкенд»
// (POST/GET /api/v1/backends/{id}/enabled).
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА: нужен тумблер, который исключает узел из выбора
// балансером, НЕ останавливая контейнер. Остановленный узел исчезает из панели,
// снятая роль ломает раскладку — оба способа плохи.
//
// Тесты фиксируют:
//   - POST снимает/ставит флаг и отвечает сразу (никаких обращений к узлу);
//   - выключенный узел ОСТАЁТСЯ в списке бэкендов вместе с причиной (иначе
//     оператор его не найдёт и не включит обратно);
//   - пересборка записи (PUT /api/v1/backends/{id} из WebUI, heartbeat агента)
//     НЕ снимает флаг — это была бы «галочка, которая сама отключается»;
//   - решение уходит в persistent-override и возвращается в запись после того,
//     как её пересобрали без флага;
//   - ошибки внятные: нет `enabled` → 400, нет бэкенда → 404.
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/internal/runtimeoverrides"
	"ollama-loadbalancer/pkg/types"
)

// newEnabledToggleServer — сервер с двумя ручными (не auto-registered) узлами.
//
// Ручные специально: у auto-registered записей есть защита узла-владельца и
// дедупликация, и тест про флаг превращался бы в тест про них.
func newEnabledToggleServer(t *testing.T) (*Server, string, string) {
	t.Helper()
	s := newAgentCapacityServer(t)
	for _, b := range []types.Backend{
		{ID: "node-a", Name: "node-a", Host: "10.0.0.1", CppWorkerPort: 18092, MaxConcurrentReqs: 1, Weight: 1, Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp},
		{ID: "node-b", Name: "node-b", Host: "10.0.0.2", CppWorkerPort: 18092, MaxConcurrentReqs: 1, Weight: 1, Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp},
	} {
		if err := s.proxy.AddBackend(b); err != nil {
			t.Fatalf("AddBackend(%s): %v", b.ID, err)
		}
	}
	return s, "node-a", "node-b"
}

// toggleEnabled — POST /enabled с телом {"enabled":...,"reason":...}.
func toggleEnabled(t *testing.T, s *Server, backendID string, enabled bool, reason string) (*httptest.ResponseRecorder, map[string]interface{}) {
	t.Helper()
	body, _ := json.Marshal(map[string]interface{}{"enabled": enabled, "reason": reason})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/backends/"+backendID+"/enabled", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	s.backendEnabled(rec, req, backendID)
	if rec.Code != http.StatusOK {
		return rec, nil
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("ответ не JSON: %v (%s)", err, rec.Body.String())
	}
	return rec, resp
}

// TestR250_10_ToggleDisablesAndReports — главный сценарий тумблера.
func TestR250_10_ToggleDisablesAndReports(t *testing.T) {
	s, id, _ := newEnabledToggleServer(t)

	rec, resp := toggleEnabled(t, s, id, false, "плановое обслуживание")
	if resp == nil {
		t.Fatalf("POST /enabled: HTTP %d (%s)", rec.Code, rec.Body.String())
	}
	if enabled, _ := resp["enabled"].(bool); enabled {
		t.Errorf("ответ говорит enabled=true после выключения: %s", rec.Body.String())
	}
	if disabled, _ := resp["disabled"].(bool); !disabled {
		t.Errorf("ответ не подтверждает флаг disabled: %s", rec.Body.String())
	}
	if reason, _ := resp["reason"].(string); reason != "плановое обслуживание" {
		t.Errorf("причина в ответе = %q, want «плановое обслуживание»", reason)
	}
	if persisted, ok := resp["persisted"].(bool); !ok || !persisted {
		t.Errorf("в ответе нет подтверждения персиста: %s", rec.Body.String())
	}
	if _, ok := resp["loadedModels"]; !ok {
		t.Errorf("в ответе нет loadedModels (оператор не увидит, что модель на узле работает): %s", rec.Body.String())
	}

	disabled, reason, since, ok := s.proxy.BackendDisabledInfo(id)
	if !ok || !disabled {
		t.Fatalf("флаг не записан: disabled=%v ok=%v", disabled, ok)
	}
	if reason != "плановое обслуживание" {
		t.Errorf("причина = %q", reason)
	}
	if since == nil {
		t.Error("момент выключения не записан")
	}
}

// TestR250_10_DisabledStaysInBackendList — узел не исчезает из панели.
func TestR250_10_DisabledStaysInBackendList(t *testing.T) {
	s, id, other := newEnabledToggleServer(t)
	toggleEnabled(t, s, id, false, "греется")

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

	seen := 0
	for _, b := range body.Backends {
		switch b["id"] {
		case id:
			seen++
			if disabled, _ := b["disabled"].(bool); !disabled {
				t.Errorf("выключенный узел в списке выглядит включённым: %v", b["disabled"])
			}
			if reason, _ := b["disabledReason"].(string); reason != "греется" {
				t.Errorf("причина в списке = %q, want «греется»", reason)
			}
		case other:
			seen++
			if disabled, _ := b["disabled"].(bool); disabled {
				t.Error("флаг протёк на второй узел")
			}
		}
	}
	if seen != 2 {
		t.Fatalf("в списке не оба узла (seen=%d): %s", seen, rec.Body.String())
	}
}

// TestR250_10_EnableClearsFlagAndReason — «вернуть в работу» одним запросом.
func TestR250_10_EnableClearsFlagAndReason(t *testing.T) {
	s, id, _ := newEnabledToggleServer(t)
	toggleEnabled(t, s, id, false, "сеть")

	rec, resp := toggleEnabled(t, s, id, true, "старая причина")
	if resp == nil {
		t.Fatalf("POST /enabled (включение): HTTP %d (%s)", rec.Code, rec.Body.String())
	}
	if enabled, _ := resp["enabled"].(bool); !enabled {
		t.Errorf("ответ не подтверждает включение: %s", rec.Body.String())
	}

	disabled, reason, since, _ := s.proxy.BackendDisabledInfo(id)
	if disabled {
		t.Fatal("флаг не снят")
	}
	if reason != "" || since != nil {
		t.Errorf("у включённого узла остались причина/момент: reason=%q since=%v", reason, since)
	}

	// GET отдаёт текущее состояние без побочных эффектов.
	getRec := httptest.NewRecorder()
	s.backendEnabled(getRec, httptest.NewRequest(http.MethodGet, "/api/v1/backends/"+id+"/enabled", nil), id)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET /enabled: HTTP %d", getRec.Code)
	}
	var getResp map[string]interface{}
	_ = json.Unmarshal(getRec.Body.Bytes(), &getResp)
	if enabled, _ := getResp["enabled"].(bool); !enabled {
		t.Errorf("GET говорит enabled=false: %s", getRec.Body.String())
	}
}

// TestR250_10_BadRequestsAreExplicit — «ключа enabled нет» и «бэкенда нет»
// должны различаться: молча трактовать отсутствие ключа как «выключить» нельзя.
func TestR250_10_BadRequestsAreExplicit(t *testing.T) {
	s, id, _ := newEnabledToggleServer(t)

	rec := httptest.NewRecorder()
	s.backendEnabled(rec,
		httptest.NewRequest(http.MethodPost, "/api/v1/backends/"+id+"/enabled", strings.NewReader(`{"reason":"нет ключа"}`)),
		id)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST без enabled: HTTP %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if disabled, _, _, _ := s.proxy.BackendDisabledInfo(id); disabled {
		t.Fatal("запрос без ключа enabled выключил бэкенд")
	}

	rec = httptest.NewRecorder()
	s.backendEnabled(rec, httptest.NewRequest(http.MethodGet, "/api/v1/backends/nope/enabled", nil), "nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET для несуществующего бэкенда: HTTP %d, want 404", rec.Code)
	}

	rec = httptest.NewRecorder()
	s.backendEnabled(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/backends/"+id+"/enabled", nil), id)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE: HTTP %d, want 405", rec.Code)
	}
}

// TestR250_10_PartialPutKeepsFlag — пересборка записи PUT'ом не снимает флаг.
//
// Живой сценарий: оператор выключил узел, потом поправил у него порт в WebUI
// (кнопка «Auto-detect cppworker port» шлёт частичный PUT). Раньше такой PUT
// собирал запись заново из полей — и флаг бы исчез, а узел молча вернулся в работу.
func TestR250_10_PartialPutKeepsFlag(t *testing.T) {
	s, id, _ := newEnabledToggleServer(t)
	toggleEnabled(t, s, id, false, "настраивают")

	rec := httptest.NewRecorder()
	s.updateBackend(rec,
		httptest.NewRequest(http.MethodPut, "/api/v1/backends/"+id, strings.NewReader(`{"cppWorkerPort":18093}`)),
		id)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: HTTP %d (%s)", rec.Code, rec.Body.String())
	}

	disabled, reason, _, ok := s.proxy.BackendDisabledInfo(id)
	if !ok {
		t.Fatal("бэкенд исчез после PUT")
	}
	if !disabled {
		t.Fatal("частичный PUT снял флаг «не использовать» — узел вернулся в работу молча")
	}
	if reason != "настраивают" {
		t.Errorf("причина потеряна: %q", reason)
	}
	if b := s.proxy.GetBackend(id); b != nil && b.CppWorkerPort != 18093 {
		t.Errorf("PUT не применился (cppWorkerPort=%d) — тест проверяет не то", b.CppWorkerPort)
	}
}

// TestR250_10_AgentHeartbeatKeepsFlag — heartbeat агента (пересборка записи
// каждые ~30 с) не снимает флаг.
func TestR250_10_AgentHeartbeatKeepsFlag(t *testing.T) {
	s, id, _ := newEnabledToggleServer(t)
	toggleEnabled(t, s, id, false, "не использовать")

	rec := doRegisterAgent(t, s, registerAgentPayload(id, string(types.BackendTypeLlamaCpp), 1))
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("регистрация агента: HTTP %d (%s)", rec.Code, rec.Body.String())
	}

	disabled, reason, _, ok := s.proxy.BackendDisabledInfo(id)
	if !ok {
		t.Fatal("бэкенд исчез после heartbeat")
	}
	if !disabled {
		t.Fatal("heartbeat агента снял флаг «не использовать»")
	}
	if reason != "не использовать" {
		t.Errorf("причина потеряна после heartbeat: %q", reason)
	}
}

// TestR250_10_DecisionSurvivesRecordRebuild — sidecar + возврат флага в запись.
//
// Записи бэкендов пересобираются НЕСКОЛЬКИМИ путями, и агент о решении оператора
// не знает. Поэтому решение хранится в persistent-override, а балансер
// периодически возвращает его в записи (тот же приём, что у ролей в R118).
func TestR250_10_DecisionSurvivesRecordRebuild(t *testing.T) {
	s, id, _ := newEnabledToggleServer(t)

	store := runtimeoverrides.New(t.TempDir())
	s.SetOverridesStore(store, nil)

	toggleEnabled(t, s, id, false, "не использовать")

	records, exists, err := store.LoadBackendDisabled()
	if err != nil {
		t.Fatalf("чтение sidecar: %v", err)
	}
	if !exists || len(records) == 0 {
		t.Fatal("решение не записано в sidecar — после рестарта флаг потеряется")
	}
	if rec, ok := records[id]; !ok || !rec.Disabled || rec.Reason != "не использовать" {
		t.Fatalf("запись sidecar неверна: %+v", records)
	}

	// Имитация пересборки записи путём, который про флаг не знает.
	if !s.proxy.SetBackendDisabled(id, false, "") {
		t.Fatal("не удалось сбросить флаг для имитации пересборки")
	}
	if restored := s.reapplyBackendDisabledOnce(); restored != 1 {
		t.Fatalf("возврат флага: restored=%d, want 1", restored)
	}
	disabled, reason, _, _ := s.proxy.BackendDisabledInfo(id)
	if !disabled || reason != "не использовать" {
		t.Fatalf("флаг не возвращён в запись: disabled=%v reason=%q", disabled, reason)
	}

	// Повторный проход идемпотентен: запись уже совпадает.
	if restored := s.reapplyBackendDisabledOnce(); restored != 0 {
		t.Fatalf("повторный проход вернул %d записей, want 0", restored)
	}

	// «Включить» тоже решение оператора: оно обязано перебить сохранённое «выкл».
	toggleEnabled(t, s, id, true, "")
	records, _, _ = store.LoadBackendDisabled()
	if rec, ok := records[id]; !ok || rec.Disabled {
		t.Fatalf("включение не сохранено в sidecar: %+v", records)
	}
}
