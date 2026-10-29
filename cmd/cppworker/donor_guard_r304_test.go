// donor_guard_r304_test.go — R304 (2026-10-30): тесты HTTP-гарда «донор не
// поднимает своих копий моделей».
//
// ЧТО ЗДЕСЬ ПРОВЕРЯЕТСЯ, КРОМЕ КОДА ОТВЕТА. Что запрет стоит НА ВХОДЕ в каждую
// ручку, создающую копию, а не где-то в середине: поэтому таблица идёт по
// обработчикам, а не по гарду. Если кто-то переставит проверку ниже разбора тела
// (или уберёт из одной ручки), тест упадёт — и это ровно та регрессия, ради
// которой он написан.
package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

// withBackendR304 подменяет пакетный backend на время теста и возвращает его.
//
// ЗАЧЕМ ПОДМЕНА. Гард спрашивает роль у ЖИВОГО бэкенда (единый предикат), поэтому
// проверять его на «пустом» backend невозможно: он ответил бы «не донор» и тест
// прошёл бы, ничего не проверив.
func withBackendR304(t *testing.T, cfg cppbackend.Config) *cppbackend.Backend {
	t.Helper()
	prev := backend
	be := cppbackend.NewBackend(cfg)
	backend = be
	t.Cleanup(func() { backend = prev })
	return be
}

// decodeDonorBodyR304 — разобрать тело отказа в общий вид.
func decodeDonorBodyR304(t *testing.T, rec *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("ответ не JSON: %v (%s)", err, rec.Body.String())
	}
	return body
}

// TestR304GUARD_RefusesOnDonorRole — приказ балансера: 409 + машинный код +
// объяснение для оператора.
func TestR304GUARD_RefusesOnDonorRole(t *testing.T) {
	withBackendR304(t, cppbackend.Config{BackendRole: cppbackend.RoleDonor})

	rec := httptest.NewRecorder()
	if !donorLoadGuard(rec) {
		t.Fatal("donorLoadGuard на доноре вернул false — запрос прошёл бы к загрузке")
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (конфликт состояния, а не ошибка клиента)", rec.Code)
	}
	body := decodeDonorBodyR304(t, rec)
	if body["status"] != "error" {
		t.Errorf("status = %v, want error", body["status"])
	}
	if body["error"] != donorErrorCodeR304 {
		t.Errorf("error = %v, want %q (машинный код для балансера и панели)",
			body["error"], donorErrorCodeR304)
	}
	if body["role"] != cppbackend.RoleDonor {
		t.Errorf("role = %v, want %q", body["role"], cppbackend.RoleDonor)
	}
	if body["reason"] != cppbackend.DonorReasonRole {
		t.Errorf("reason = %v, want %q", body["reason"], cppbackend.DonorReasonRole)
	}
	// message/hint — то, что читает человек в чужом клиенте: без них оператор
	// видит только код и идёт читать исходники.
	if msg, _ := body["message"].(string); msg == "" {
		t.Error("в отказе нет message — клиент покажет пустое место")
	}
	if hint, _ := body["hint"].(string); hint == "" {
		t.Error("в отказе нет hint — оператор не знает, что делать")
	}
}

// TestR304GUARD_RefusesOnRPCServerNode — донор по ФАКТУ поднятого узла.
//
// Отдельный случай: роль при этом standalone (оператор включил узел руками), и
// именно здесь догадка «по роли» расходится с реальностью.
func TestR304GUARD_RefusesOnRPCServerNode(t *testing.T) {
	withBackendR304(t, cppbackend.Config{
		BackendRole:      cppbackend.RoleStandalone,
		RPCServerEnabled: true,
	})

	rec := httptest.NewRecorder()
	if !donorLoadGuard(rec) {
		t.Fatal("машина с поднятым узлом раскладки разрешила загрузку — память продаётся дважды")
	}
	body := decodeDonorBodyR304(t, rec)
	if body["reason"] != cppbackend.DonorReasonRPCServerEnabled {
		t.Errorf("reason = %v, want %q", body["reason"], cppbackend.DonorReasonRPCServerEnabled)
	}
}

// TestR304GUARD_PassesNonDonor — поведение не-донора НЕ МЕНЯЕТСЯ.
//
// Условие «ничего не записали» важнее кода: гард не должен трогать заголовки и
// тело ответа, иначе успешная загрузка получила бы лишний Content-Type/код.
func TestR304GUARD_PassesNonDonor(t *testing.T) {
	cases := []struct {
		name string
		cfg  cppbackend.Config
	}{
		{"роль standalone", cppbackend.Config{BackendRole: cppbackend.RoleStandalone}},
		{"роль неизвестна", cppbackend.Config{}},
		{"роль main без узла", cppbackend.Config{BackendRole: cppbackend.RoleMain}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withBackendR304(t, c.cfg)
			rec := httptest.NewRecorder()
			if donorLoadGuard(rec) {
				t.Fatalf("не-донор (%s) получил отказ — сломано поведение обычной машины", c.name)
			}
			if rec.Body.Len() != 0 {
				t.Errorf("гард записал тело при пропуске: %q", rec.Body.String())
			}
			if len(rec.Header()) != 0 {
				t.Errorf("гард выставил заголовки при пропуске: %v", rec.Header())
			}
		})
	}
}

// TestR304GUARD_AllCopyDoorsRefuse — каждая ручка, создающая локальную копию,
// отвечает 409 до любого разбора тела.
//
// ПОЧЕМУ ТАБЛИЦА ПО ОБРАБОТЧИКАМ. Это единственная проверка, которая видит
// ПРОВОДКУ: гард может быть исправен, а ручка — его не вызывать (именно так в
// проекте появлялись «новые точки обхода общего пути»). Пустое тело запроса здесь
// не случайность: если отказ вернулся, значит проверка стоит РАНЬШЕ разбора JSON.
func TestR304GUARD_AllCopyDoorsRefuse(t *testing.T) {
	withBackendR304(t, cppbackend.Config{BackendRole: cppbackend.RoleDonor})

	handlers := []struct {
		name    string
		handler http.HandlerFunc
		method  string
		path    string
	}{
		{"handleLoadModel", handleLoadModel, http.MethodPost, "/api/models/load"},
		{"handleLoadWithParams", handleLoadWithParams, http.MethodPost, "/api/models/load-with-params"},
		{"handleReloadModel", handleReloadModel, http.MethodPost, "/api/models/reload"},
		{"handleImportModel", handleImportModel, http.MethodPost, "/api/models/import"},
	}
	for _, h := range handlers {
		t.Run(h.name, func(t *testing.T) {
			req := httptest.NewRequest(h.method, h.path, strings.NewReader(""))
			rec := httptest.NewRecorder()
			h.handler(rec, req)

			if rec.Code != http.StatusConflict {
				t.Fatalf("%s: status = %d, want 409 (body=%s)", h.name, rec.Code, rec.Body.String())
			}
			body := decodeDonorBodyR304(t, rec)
			if body["error"] != donorErrorCodeR304 {
				t.Fatalf("%s: error = %v, want %q", h.name, body["error"], donorErrorCodeR304)
			}
		})
	}
}

// TestR304GUARD_DonorRoleSurvivesContainerRecreation — роль действует на ходу,
// уезжает в файл операторских переопределений и НЕ приходит из окружения.
//
// ЗАЧЕМ. Живой контейнер пересоздаётся независимо от балансера. Если роль жила бы
// только в памяти процесса, донор после recreate снова начал бы грузить модели —
// при том что мастер уже спланировал раскладку по его памяти. Отдельно
// проверяется, что роль НЕ читается из env: иначе «донора» можно было бы включить
// руками там, где им быть не должны.
func TestR304GUARD_DonorRoleSurvivesContainerRecreation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CPPWORKER_CONFIG_DIR", dir)

	prevCfg := currentConfig
	prevBackend := backend
	cfg := &cppbackend.Config{
		ModelsDir:        t.TempDir(),
		DefaultCtxSize:   8192,
		DefaultBatchSize: 512,
	}
	currentConfig = cfg
	backend = cppbackend.NewBackend(*cfg)
	t.Cleanup(func() {
		currentConfig = prevCfg
		backend = prevBackend
	})

	// Приказ балансера. Роль с заглавной буквы — нормализация обязана её принять.
	req := httptest.NewRequest(http.MethodPut, "/api/v1/cppworker/config/update",
		strings.NewReader(`{"backendRole":"Donor"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handleCppWorkerUpdateConfig(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	if cfg.BackendRole != cppbackend.RoleDonor {
		t.Fatalf("в конфиге роль = %q, want %q", cfg.BackendRole, cppbackend.RoleDonor)
	}
	// Живой бэкенд обязан узнать о роли сразу: это ОН решает, можно ли грузить.
	if donor, reason := backend.DonorOnly(); !donor || reason != cppbackend.DonorReasonRole {
		t.Fatalf("живой бэкенд не стал донором: DonorOnly() = (%v, %q)", donor, reason)
	}

	// «Пересоздание контейнера»: конфиг читается из окружения и файла
	// переопределений, памяти процесса уже нет.
	fresh := cppbackend.LoadConfigFromEnv()
	if fresh.BackendRole != "" {
		t.Fatalf("роль пришла из окружения (%q) — источником обязан быть только балансер",
			fresh.BackendRole)
	}
	applied := applyRuntimeOverrides(&fresh)
	if !appliedListContains(applied, "backendRole") {
		t.Fatalf("роль не применена из файла переопределений (applied=%v) — "+
			"после пересоздания контейнера донор снова начнёт грузить свои копии", applied)
	}
	if fresh.BackendRole != cppbackend.RoleDonor {
		t.Fatalf("после пересоздания роль = %q, want %q", fresh.BackendRole, cppbackend.RoleDonor)
	}
	if donor, _ := cppbackend.NewBackend(fresh).DonorOnly(); !donor {
		t.Fatal("свежий бэкенд из восстановленного конфига не считает себя донором")
	}
}

// TestR304GUARD_ModelLoadErrorMapsTo409 — внутренний путь (загрузка по запросу
// чата) тоже обязан давать 409, а не 500 «model load failed».
//
// ЗАЧЕМ. ensureModelLoaded не пишет HTTP сам: он возвращает ошибку, а её
// переводит writeModelLoadError. Без отдельной ветки отказ донора выглядел бы
// как поломка воркера — и балансер пометил бы донора «сломанным» вместо того,
// чтобы исключить его из загрузки по роли.
func TestR304GUARD_ModelLoadErrorMapsTo409(t *testing.T) {
	withBackendR304(t, cppbackend.Config{BackendRole: cppbackend.RoleDonor})

	err := donorRefusalErrorR304(cppbackend.DonorMark{
		Role: cppbackend.RoleDonor, Donor: true, Reason: cppbackend.DonorReasonRole,
	})
	if !errors.Is(err, errDonorCannotHostModelsR304) {
		t.Fatalf("ошибка отказа не опознаётся через errors.Is: %v", err)
	}

	rec := httptest.NewRecorder()
	if !writeModelLoadError(rec, "qwen3", err) {
		t.Fatal("writeModelLoadError вернул false — ответ не записан")
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if body := decodeDonorBodyR304(t, rec); body["error"] != donorErrorCodeR304 {
		t.Fatalf("error = %v, want %q", body["error"], donorErrorCodeR304)
	}

	// Тот же отказ, но поднявшийся из RAM-fallback во время генерации.
	rec2 := httptest.NewRecorder()
	if !handleInferenceError(rec2, err) {
		t.Fatal("handleInferenceError не распознал отказ донора — клиент получил бы 500")
	}
	if rec2.Code != http.StatusConflict {
		t.Fatalf("handleInferenceError: status = %d, want 409", rec2.Code)
	}
}
