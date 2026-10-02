//go:build llama_stub

// backend_gpuindex_test.go — R-Image follow-up (2026-10-02): gpuIndex в API
// бэкендов.
//
// Поле нужно политике сосуществования image/text: ключ лока GPU = host +
// gpuIndex, когда индекс задан у ОБЕИХ сторон (internal/balancer/
// image_resources.go: locksConflict). Без прокидки в API оператор не мог бы
// индекс задать, а GET не показывал бы уже сохранённое значение — ровно тот
// дефект, что был найден живым E2E у imagePort.
package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// backendListGPUIndexes — gpuIndex по id из GET /api/v1/backends.
func backendListGPUIndexes(t *testing.T, s *Server) map[string]int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/backends", nil)
	req.Header.Set("X-API-Token", "r65d-secret-token")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/backends = %d, тело %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Backends []struct {
			ID       string `json:"id"`
			GPUIndex int    `json:"gpuIndex"`
		} `json:"backends"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal: %v (тело %s)", err, rec.Body.String())
	}
	out := make(map[string]int, len(payload.Backends))
	for _, b := range payload.Backends {
		out[b.ID] = b.GPUIndex
	}
	return out
}

// GET отдаёт gpuIndex, частичный PUT его не стирает, явный PUT — применяет.
func TestRImage_BackendGPUIndex_ListAndMerge(t *testing.T) {
	b := backendForUpdate()
	b.GPUIndex = 1
	s := authEnabledServer(t, b)

	if got := backendListGPUIndexes(t, s)["llama-merge"]; got != 1 {
		t.Errorf("GET /api/v1/backends: gpuIndex = %d, want 1 (поле обязано быть в JSON, "+
			"иначе потребитель не увидит настройку)", got)
	}

	// Частичный PUT (как из WebUI: только name) не должен обнулять индекс —
	// иначе лок сосуществования молча вернулся бы к хостовому.
	if code, _ := putBackend(t, s, "llama-merge", map[string]interface{}{"name": "Merge Test"}); code != http.StatusOK {
		t.Fatalf("частичный PUT = %d, want 200", code)
	}
	if got := findBackend(t, s, "llama-merge").GPUIndex; got != 1 {
		t.Errorf("после частичного PUT gpuIndex = %d, want 1 (merge-семантика)", got)
	}

	// Явный PUT применяет новое значение.
	if code, _ := putBackend(t, s, "llama-merge", map[string]interface{}{"gpuIndex": 2}); code != http.StatusOK {
		t.Fatalf("PUT gpuIndex = %d, want 200", code)
	}
	if got := findBackend(t, s, "llama-merge").GPUIndex; got != 2 {
		t.Errorf("после PUT gpuIndex = %d, want 2", got)
	}
	if got := backendListGPUIndexes(t, s)["llama-merge"]; got != 2 {
		t.Errorf("GET после PUT: gpuIndex = %d, want 2", got)
	}
	// 0 = «не менять»: обнулить индекс этой ручкой нельзя (0 и «не прислано»
	// в int неразличимы) — фиксируем поведение, чтобы оно не «поехало» молча.
	if code, _ := putBackend(t, s, "llama-merge", map[string]interface{}{"gpuIndex": 0}); code != http.StatusOK {
		t.Fatalf("PUT gpuIndex=0 = %d, want 200", code)
	}
	if got := findBackend(t, s, "llama-merge").GPUIndex; got != 2 {
		t.Errorf("PUT gpuIndex=0 → %d, want 2 (0 = «не менять»)", got)
	}
}

// POST /api/v1/backends принимает gpuIndex (backendRequest).
func TestRImage_BackendGPUIndex_PostStoresIndex(t *testing.T) {
	s := authEnabledServer(t, backendForUpdate())

	body, err := json.Marshal(map[string]interface{}{
		"id": "gpu-add", "name": "GPU add", "host": "10.9.9.9",
		"backendType": string(types.BackendTypeLlamaCpp),
		"gpuIndex":    3,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/backends", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Token", "r65d-secret-token")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/v1/backends = %d, тело %s", rec.Code, rec.Body.String())
	}
	// Backend, созданный через AddBackend, живёт в runtime-состоянии прокси
	// (в p.config.Backends его кладёт LoadState, а не AddBackend) — читаем там.
	added := s.proxy.GetBackend("gpu-add")
	if added == nil {
		t.Fatal("POST /api/v1/backends не создал бэкенд")
	}
	if added.GPUIndex != 3 {
		t.Errorf("gpuIndex = %d, want 3 (POST обязан принимать поле)", added.GPUIndex)
	}
}
