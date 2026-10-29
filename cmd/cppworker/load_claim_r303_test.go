//go:build llama_stub

// load_claim_r303_test.go — R303: D3 «неснимаемая загрузка / мусор в in_flight».
//
// ЖИВОЙ ДЕФЕКТ (двухузловой отчёт, D3):
//
//	POST /api/models/load  висел 260 с без ответа;
//	GET  /api/info         → in_flight_requests {"gemma-4-E4B-it-Q4_K_M": 3};
//	POST /api/models/unload → 409 «model is currently loading», хотя загрузки нет;
//	лечилось только перезапуском контейнера.
//
// Тесты ниже фиксируют три обязательных свойства R303:
//  1. load-запрос НЕ висит: чужое ожидание ограничено и отдаёт типизированный
//     503 с возрастом заявки (status=load_in_progress | load_stuck);
//  2. мёртвая заявка (нет живого инстанса в состоянии loading) снимается сама —
//     и load, и unload больше не упираются в неё навсегда;
//  3. состояние наблюдаемо: заявка и возраст in-flight видны в /api/info и
//     /api/models, а не только как «1».
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
)

// newTestBackendR303 — backend с ВРЕМЕННЫМ каталогом моделей.
//
// Зачем не newTestBackend(): тот создаёт backend с ModelsDir="" — cppworker
// пишет туда .name_history.json (персистентная история имён, model_manager.go),
// и файл оказывается в рабочем каталоге пакета (мусор в git status). Для R303
// это не важно по сути теста, но важно по гигиене.
func newTestBackendR303(t *testing.T) *cppbackend.Backend {
	t.Helper()
	b := cppbackend.NewBackend(cppbackend.Config{
		Host:      "127.0.0.1",
		Port:      18091,
		ModelsDir: t.TempDir(),
	})
	if err := b.Init(); err != nil {
		_ = err
	}
	return b
}

// r303TestModelPath — настоящий файл: handleLoadModel проверяет os.Stat ДО
// взятия заявки, иначе тест не дойдёт до проверяемой ветки.
func r303TestModelPath(t *testing.T) (name, path string) {
	t.Helper()
	name = "r303-model"
	path = filepath.Join(t.TempDir(), name+".gguf")
	if err := os.WriteFile(path, []byte("stub-gguf"), 0o644); err != nil {
		t.Fatalf("не удалось создать файл модели: %v", err)
	}
	return name, path
}

// TestR303_StaleClaim_LoadDoesNotHang — заявка без живого инстанса снимается, и
// POST /api/models/load?wait=true доходит до реальной загрузки (а не висит).
//
// Это ровно то состояние, в котором живой стенд висел 260 с: заявка лежала в
// b.loading, а обработчик уходил в бесконечный WaitForLoad.
func TestR303_StaleClaim_LoadDoesNotHang(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackendR303(t)

	t.Setenv("CPPWORKER_LOAD_WAIT_MAX_SEC", "1")
	t.Setenv("CPPWORKER_LOAD_STALE_SEC", "0") // 0 = любой возраст считается мёртвым

	name, path := r303TestModelPath(t)
	if ok, err := backend.TryLockLoadR303(name, "test-stale"); !ok || err != nil {
		t.Fatalf("предусловие: TryLockLoadR303 = (%v, %v)", ok, err)
	}
	if !backend.IsLoading(name) {
		t.Fatal("предусловие: заявка должна быть видимой (IsLoading=false)")
	}

	body, _ := json.Marshal(map[string]interface{}{"name": name, "path": path})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/models/load?wait=true&waitTimeoutMs=2000", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	handleLoadModel(rec, req)
	elapsed := time.Since(start)

	// Главное: ответ пришёл, а не тишина. Раньше здесь был бесконечный WaitForLoad.
	if elapsed > 5*time.Second {
		t.Fatalf("load висел %s — ожидание чужой загрузки не ограничено", elapsed)
	}
	// Мёртвая заявка должна быть снята: новый загрузчик её переиспользовал/заменил,
	// а не «подождал и сдался».
	if strings.Contains(rec.Body.String(), "load_in_progress") ||
		strings.Contains(rec.Body.String(), "load_stuck") {
		t.Fatalf("мёртвая заявка не снята: получен типизированный отказ вместо продолжения загрузки: %s",
			rec.Body.String())
	}
	t.Logf("ответ на load после снятия мёртвой заявки: HTTP %d за %s: %s",
		rec.Code, elapsed, rec.Body.String())
}

// TestR303_LiveClaim_SyncLoadReturnsTypedError — свежая заявка не снимается
// (это была бы вторая загрузка того же файла), но клиент получает ОТВЕТ:
// 503 + status/возраст/retryAfterMs, а не многоминутную тишину.
func TestR303_LiveClaim_SyncLoadReturnsTypedError(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackendR303(t)

	// Ожидание — 1 c, порог «мёртвой» заявки — час: заявка свежая и НЕ снимается.
	t.Setenv("CPPWORKER_LOAD_WAIT_MAX_SEC", "1")
	t.Setenv("CPPWORKER_LOAD_STALE_SEC", "3600")

	name, path := r303TestModelPath(t)
	if ok, err := backend.TryLockLoadR303(name, "test-live"); !ok || err != nil {
		t.Fatalf("предусловие: TryLockLoadR303 = (%v, %v)", ok, err)
	}
	defer backend.UnlockLoad(name)

	body, _ := json.Marshal(map[string]interface{}{"name": name, "path": path})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/models/load?wait=true&waitTimeoutMs=60000", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	handleLoadModel(rec, req)
	elapsed := time.Since(start)

	if elapsed > 10*time.Second {
		t.Fatalf("load висел %s — ожидание не ограничено CPPWORKER_LOAD_WAIT_MAX_SEC", elapsed)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("HTTP %d, ожидался 503 с типом загрузки; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("ответ не JSON: %v (%s)", err, rec.Body.String())
	}
	if got := resp["status"]; got != "load_in_progress" {
		t.Errorf("status=%v, ожидался load_in_progress (body=%s)", got, rec.Body.String())
	}
	for _, key := range []string{"claimAgeSec", "claimRequester", "retryAfterMs", "waitedMs", "hint"} {
		if _, ok := resp[key]; !ok {
			t.Errorf("в ответе нет %q — клиент не может отличить «грузится» от «сломалось»: %s",
				key, rec.Body.String())
		}
	}
	if _, ok := resp["loadingInstance"]; !ok {
		t.Errorf("в ответе нет loadingInstance — не отличить живую загрузку от мёртвой заявки: %s",
			rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("нет заголовка Retry-After")
	}
}

// TestR303_UnloadStaleClaim_Reclaimed — unload больше не отвечает вечным 409
// «model is currently loading» на мёртвую заявку.
func TestR303_UnloadStaleClaim_Reclaimed(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackendR303(t)

	t.Setenv("CPPWORKER_LOAD_STALE_SEC", "0")

	name := "r303-unload-stale"
	if ok, err := backend.TryLockLoadR303(name, "test-stale-unload"); !ok || err != nil {
		t.Fatalf("предусловие: TryLockLoadR303 = (%v, %v)", ok, err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/models/unload?name="+name, nil)
	handleUnloadModel(rec, req)

	if rec.Code == http.StatusConflict {
		t.Fatalf("мёртвая заявка всё ещё держит unload в 409: %s", rec.Body.String())
	}
	if backend.IsLoading(name) {
		t.Fatal("заявка не снята: IsLoading=true после unload")
	}
	t.Logf("unload мёртвой заявки: HTTP %d: %s", rec.Code, rec.Body.String())
}

// TestR303_UnloadLiveClaim_ConflictCarriesAge — живая заявка по-прежнему 409, но
// теперь в теле есть возраст и владелец (оператор видит, ЧТО держит модель).
func TestR303_UnloadLiveClaim_ConflictCarriesAge(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackendR303(t)

	t.Setenv("CPPWORKER_LOAD_STALE_SEC", "3600")

	name := "r303-unload-live"
	if ok, err := backend.TryLockLoadR303(name, "test-live-unload"); !ok || err != nil {
		t.Fatalf("предусловие: TryLockLoadR303 = (%v, %v)", ok, err)
	}
	defer backend.UnlockLoad(name)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/models/unload?name="+name, nil)
	handleUnloadModel(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("HTTP %d, ожидался 409 (живая заявка не должна сниматься); body=%s",
			rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("ответ не JSON: %v", err)
	}
	if _, ok := resp["claimAgeSec"]; !ok {
		t.Errorf("в 409 нет claimAgeSec — «model is loading» без возраста и есть та самая ложь: %s",
			rec.Body.String())
	}
	if got := resp["claimRequester"]; got != "test-live-unload" {
		t.Errorf("claimRequester=%v, ожидался владелец заявки", got)
	}
}

// TestR303_InfoExposesLoadClaimsAndInFlightAge — состояние наблюдаемо снаружи:
// /api/info отдаёт заявки с возрастом, пределы ожидания и возраст in-flight.
func TestR303_InfoExposesLoadClaimsAndInFlightAge(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackendR303(t)

	name := "r303-info-model"
	if ok, err := backend.TryLockLoadR303(name, "test-info"); !ok || err != nil {
		t.Fatalf("предусловие: TryLockLoadR303 = (%v, %v)", ok, err)
	}
	defer backend.UnlockLoad(name)

	backend.InFlight().Inc(name)
	defer backend.InFlight().Dec(name)

	time.Sleep(20 * time.Millisecond)

	rec := httptest.NewRecorder()
	handleInfo(rec, httptest.NewRequest(http.MethodGet, "/api/info", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d", rec.Code)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("ответ не JSON: %v", err)
	}
	claims, ok := resp["load_claims"].([]interface{})
	if !ok || len(claims) == 0 {
		t.Fatalf("в /api/info нет load_claims — оператор не видит, кто держит загрузку: %s",
			rec.Body.String())
	}
	first, _ := claims[0].(map[string]interface{})
	if first["model"] != name {
		t.Errorf("load_claims[0].model=%v, ожидалось %q", first["model"], name)
	}
	if _, ok := first["age_sec"]; !ok {
		t.Errorf("у заявки нет age_sec: %v", first)
	}
	if _, ok := resp["load_wait_limit_sec"]; !ok {
		t.Error("нет load_wait_limit_sec")
	}
	if _, ok := resp["in_flight_age_sec"]; !ok {
		t.Errorf("нет in_flight_age_sec — «мусор в in_flight» снова неотличим от генерации: %s",
			rec.Body.String())
	}
	oldest, ok := resp["in_flight_oldest_sec"].(float64)
	if !ok || oldest <= 0 {
		t.Errorf("in_flight_oldest_sec=%v, ожидалось > 0 (запрос висит 20+ мс)", resp["in_flight_oldest_sec"])
	}
}

// TestR303_ModelsCarryClaimAge — per-model диагностика в /api/models.
func TestR303_ModelsCarryClaimAge(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackendR303(t)

	name := "r303-models-claim"
	if ok, err := backend.TryLockLoadR303(name, "test-models"); !ok || err != nil {
		t.Fatalf("предусловие: TryLockLoadR303 = (%v, %v)", ok, err)
	}
	defer backend.UnlockLoad(name)

	// per-model заявка видна через тот же аксессор, который использует /api/models.
	claim, ok := backend.LoadClaimR303(name)
	if !ok {
		t.Fatal("LoadClaimR303 не видит заявку")
	}
	if claim.Requester != "test-models" {
		t.Errorf("Requester=%q", claim.Requester)
	}
	if claim.LoadingInstance {
		t.Errorf("LoadingInstance=true без инстанса в состоянии loading — по этому полю " +
			"unload решает, снимать ли заявку")
	}
}
