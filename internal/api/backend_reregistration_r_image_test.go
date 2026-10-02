//go:build llama_stub

// backend_reregistration_r_image_test.go — R-Image Phase 8 (2026-10-03):
// идемпотентная ПОВТОРНАЯ саморегистрация бэкенда с изменившимся адресом.
//
// ЗАЧЕМ. Пересозданный контейнер (новый host или порт — обычное дело после
// `docker compose --profile full up -d --build`) получал 409 НАВСЕГДА:
// isSameBackendRegistration требует совпадения host+порта, а запись в балансере
// оставалась со старым адресом. Для оператора это выглядело как «image-бэкенд
// в докере не активен»: воркер жив, а балансер зовёт несуществующий адрес.
//
// Проверяем ровно границу безопасности: авто-бэкенд обновляется, а запись,
// созданная РУКАМИ (без метки auto-registered), по-прежнему защищена 409.
package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// autoImageBackend — запись, как её создаёт саморегистрация sdworker
// (cmd/sdworker/balancer_register.go: labels sdworker/auto-registered/image).
func autoImageBackend(id, host string, imagePort int) types.Backend {
	return types.Backend{
		ID:                id,
		Name:              id,
		Host:              host,
		ImagePort:         imagePort,
		Type:              types.BackendTypeImage,
		Status:            types.StatusHealthy,
		MaxConcurrentReqs: 8,
		MaxModels:         1,
		Labels:            []string{"sdworker", "auto-registered", "image"},
	}
}

// postBackend выполняет POST /api/v1/backends и возвращает код + тело.
func postBackend(t *testing.T, s *Server, body map[string]interface{}) (int, string) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/backends", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Token", "r65d-secret-token")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// Повторная саморегистрация с ДРУГИМ host/портом обязана обновить запись, а не
// залипнуть в 409 (единый docker-стенд: контейнер пересоздали).
func TestRImage_AutoBackend_ReRegistrationUpdatesAddress(t *testing.T) {
	s := authEnabledServer(t, autoImageBackend("imageworker", "imageworker-old", 18093))

	code, body := postBackend(t, s, map[string]interface{}{
		"id": "imageworker", "name": "imageworker", "host": "imageworker-new",
		"imagePort": 19093, "backendType": string(types.BackendTypeImage),
		"maxConcurrentRequests": 64, "maxModels": 1,
		"labels": []string{"sdworker", "auto-registered", "image"},
	})
	if code != http.StatusOK {
		t.Fatalf("повторная саморегистрация с новым адресом = %d, want 200 (иначе запись залипает навсегда); тело %s", code, body)
	}

	got := findBackend(t, s, "imageworker")
	if got.Host != "imageworker-new" {
		t.Errorf("host = %q, want imageworker-new (адрес обязан обновиться)", got.Host)
	}
	if got.ImagePort != 19093 {
		t.Errorf("imagePort = %d, want 19093", got.ImagePort)
	}
	if got.MaxConcurrentReqs != 64 {
		t.Errorf("maxConcurrentRequests = %d, want 64 (вместимость ноды тоже обновляется)", got.MaxConcurrentReqs)
	}
	if got.Type != types.BackendTypeImage {
		t.Errorf("type = %q, want image_cpp", got.Type)
	}
}

// Тот же адрес — прежний путь R70 (refresh вместимости) не сломан.
func TestRImage_AutoBackend_SameAddressRefreshesCapacity(t *testing.T) {
	s := authEnabledServer(t, autoImageBackend("imageworker", "imageworker", 18093))

	code, body := postBackend(t, s, map[string]interface{}{
		"id": "imageworker", "name": "imageworker", "host": "imageworker",
		"imagePort": 18093, "backendType": string(types.BackendTypeImage),
		"maxConcurrentRequests": 64,
		"labels":                []string{"sdworker", "auto-registered", "image"},
	})
	if code != http.StatusOK {
		t.Fatalf("саморегистрация на тот же адрес = %d, want 200; тело %s", code, body)
	}
	if got := findBackend(t, s, "imageworker"); got.MaxConcurrentReqs != 64 {
		t.Errorf("maxConcurrentRequests = %d, want 64", got.MaxConcurrentReqs)
	}
}

// Запись, созданная РУКАМИ (без auto-registered), не перезаписывается: смена
// адреса такому бэкенду не «прощается» — иначе чужой POST менял бы адрес
// операторского бэкенда по совпадению ID.
func TestRImage_ManualBackend_ReRegistrationStillConflicts(t *testing.T) {
	manual := autoImageBackend("image-manual", "10.0.0.5", 18093)
	manual.Labels = []string{"operator"}

	s := authEnabledServer(t, manual)

	code, body := postBackend(t, s, map[string]interface{}{
		"id": "image-manual", "name": "image-manual", "host": "10.0.0.9",
		"imagePort": 19093, "backendType": string(types.BackendTypeImage),
		"labels": []string{"sdworker", "auto-registered", "image"},
	})
	if code != http.StatusConflict {
		t.Fatalf("POST на ручную запись = %d, want 409 (защита операторской записи); тело %s", code, body)
	}
	if got := findBackend(t, s, "image-manual"); got.Host != "10.0.0.5" || got.ImagePort != 18093 {
		t.Errorf("ручная запись изменена: host=%q imagePort=%d", got.Host, got.ImagePort)
	}
}

// Смена ТИПА по совпадению ID — тоже конфликт: саморегистрация image-воркера не
// имеет права превратить текстовый бэкенд в image_cpp (и наоборот).
func TestRImage_ReRegistrationWithDifferentTypeConflicts(t *testing.T) {
	s := authEnabledServer(t, autoImageBackend("node-1", "10.0.0.7", 18093))

	code, _ := postBackend(t, s, map[string]interface{}{
		"id": "node-1", "name": "node-1", "host": "10.0.0.8",
		"cppWorkerPort": 18092, "backendType": string(types.BackendTypeLlamaCpp),
		"labels": []string{"cppworker", "auto-registered"},
	})
	if code != http.StatusConflict {
		t.Fatalf("смена типа бэкенда по ID = %d, want 409", code)
	}
	if got := findBackend(t, s, "node-1"); got.Type != types.BackendTypeImage {
		t.Errorf("type = %q, want прежний image_cpp", got.Type)
	}
}
