//go:build llama_stub

// backend_gpuindex_test.go — R-Image follow-up (2026-10-02): gpuIndex в API
// бэкендов.
//
// Поле нужно политике сосуществования image/text: ключ лока GPU = host +
// индекс, когда индекс ИЗВЕСТЕН у ОБЕИХ сторон (internal/balancer/
// image_resources.go: locksConflict). Без прокидки в API оператор не мог бы
// индекс задать, а GET не показывал бы уже сохранённое значение — ровно тот
// дефект, что был найден живым E2E у imagePort.
//
// Второй пункт бэклога — СЕМАНТИКА (R-Image follow-up): индекс GPU 0 обязан быть
// объявляемым (0 — валидная первая карта, а не «не задано»), а индекс — сбрасываемым.
// Отсюда три исхода PUT: ключа нет → не менять; null → сбросить; 0 → явно GPU 0.
package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// listedGPUIndex — gpuIndex из GET /api/v1/backends: Present=false означает,
// что поля в JSON НЕТ (nil = «индекс неизвестен»). Именно так потребитель
// отличает «бэкенд объявил GPU 0» от «индекс не задан» — от этого зависит ключ
// лока сосуществования.
type listedGPUIndex struct {
	Present bool
	Value   int
}

// backendListGPUIndexes — gpuIndex по id из GET /api/v1/backends.
func backendListGPUIndexes(t *testing.T, s *Server) map[string]listedGPUIndex {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/backends", nil)
	req.Header.Set("X-API-Token", "r65d-secret-token")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/backends = %d, тело %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Backends []map[string]json.RawMessage `json:"backends"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal: %v (тело %s)", err, rec.Body.String())
	}
	out := make(map[string]listedGPUIndex, len(payload.Backends))
	for _, b := range payload.Backends {
		var id string
		if err := json.Unmarshal(b["id"], &id); err != nil {
			t.Fatalf("unmarshal id: %v (тело %s)", err, rec.Body.String())
		}
		raw, ok := b["gpuIndex"]
		if !ok {
			out[id] = listedGPUIndex{} // поля нет — индекс «неизвестен»
			continue
		}
		var idx int
		if err := json.Unmarshal(raw, &idx); err != nil {
			t.Fatalf("gpuIndex бэкенда %s не число: %s", id, raw)
		}
		out[id] = listedGPUIndex{Present: true, Value: idx}
	}
	return out
}

// GET отдаёт gpuIndex, частичный PUT его не стирает, PUT применяет новое
// значение, `0` — это ЯВНЫЙ GPU 0, а `null` — СБРОС.
func TestRImage_BackendGPUIndex_ListAndMerge(t *testing.T) {
	b := backendForUpdate()
	b.GPUIndex = types.GPUIndexPtr(1)
	s := authEnabledServer(t, b)

	if got := backendListGPUIndexes(t, s)["llama-merge"]; !got.Present || got.Value != 1 {
		t.Errorf("GET /api/v1/backends: gpuIndex = %+v, want {true 1} (поле обязано быть в JSON, "+
			"иначе потребитель не увидит настройку)", got)
	}

	// Частичный PUT (как из WebUI: только name) не должен обнулять индекс —
	// иначе лок сосуществования молча вернулся бы к хостовому.
	if code, _ := putBackend(t, s, "llama-merge", map[string]interface{}{"name": "Merge Test"}); code != http.StatusOK {
		t.Fatalf("частичный PUT = %d, want 200", code)
	}
	if got := findBackend(t, s, "llama-merge").GPUIndex; got == nil || *got != 1 {
		t.Errorf("после частичного PUT gpuIndex = %v, want 1 (merge-семантика)", got)
	}

	// Явный PUT применяет новое значение.
	if code, _ := putBackend(t, s, "llama-merge", map[string]interface{}{"gpuIndex": 2}); code != http.StatusOK {
		t.Fatalf("PUT gpuIndex = 2 → %d, want 200", code)
	}
	if got := findBackend(t, s, "llama-merge").GPUIndex; got == nil || *got != 2 {
		t.Errorf("после PUT gpuIndex = %v, want 2", got)
	}
	if got := backendListGPUIndexes(t, s)["llama-merge"]; !got.Present || got.Value != 2 {
		t.Errorf("GET после PUT: gpuIndex = %+v, want {true 2}", got)
	}

	// 0 — ЯВНЫЙ GPU 0 (первая карта): раньше трактовался как «не менять», из-за
	// чего объявить карту 0 было нельзя.
	if code, _ := putBackend(t, s, "llama-merge", map[string]interface{}{"gpuIndex": 0}); code != http.StatusOK {
		t.Fatalf("PUT gpuIndex=0 → %d, want 200", code)
	}
	got := findBackend(t, s, "llama-merge").GPUIndex
	if got == nil || *got != 0 {
		t.Fatalf("PUT gpuIndex=0 → %v, want явный 0", got)
	}
	afterZero := findBackend(t, s, "llama-merge")
	if idx, known := afterZero.EffectiveGPUIndex(); !known || idx != 0 {
		t.Errorf("EffectiveGPUIndex() = (%d, %v), want (0, true) — явный GPU 0 обязан сужать лок "+
			"до host#gpu0, иначе первая карта снова блокирует весь хост", idx, known)
	}
	if got := backendListGPUIndexes(t, s)["llama-merge"]; !got.Present || got.Value != 0 {
		t.Errorf("GET после PUT gpuIndex=0: %+v, want {true 0}", got)
	}

	// null — СБРОС: индекс снова неизвестен, и поля в ответе быть не должно.
	if code, _ := putBackend(t, s, "llama-merge", map[string]interface{}{"gpuIndex": nil}); code != http.StatusOK {
		t.Fatalf("PUT gpuIndex=null → %d, want 200", code)
	}
	if got := findBackend(t, s, "llama-merge").GPUIndex; got != nil {
		t.Errorf("после PUT gpuIndex=null gpuIndex = %d, want nil (сброс в «неизвестно»)", *got)
	}
	afterReset := findBackend(t, s, "llama-merge")
	if idx, known := afterReset.EffectiveGPUIndex(); known {
		t.Errorf("после сброса EffectiveGPUIndex() = (%d, true), want known=false — "+
			"лок сосуществования обязан вернуться к хостовому", idx)
	}
	if got := backendListGPUIndexes(t, s)["llama-merge"]; got.Present {
		t.Errorf("после сброса поля gpuIndex в ответе быть не должно, получено %d", got.Value)
	}

	// Отрицательный индекс — 400: «GPU -1» смысла не имеет, а тихо записанное
	// значение EffectiveGPUIndex() трактует как «неизвестно».
	if code, _ := putBackend(t, s, "llama-merge", map[string]interface{}{"gpuIndex": -1}); code != http.StatusBadRequest {
		t.Errorf("PUT gpuIndex=-1 → %d, want 400", code)
	}
}

// POST /api/v1/backends принимает gpuIndex (backendRequest): в том числе 0 —
// саморегистрация cppworker'а на первой карте обязана сузить лок, а не оставить
// его хостовым.
func TestRImage_BackendGPUIndex_PostStoresIndex(t *testing.T) {
	s := authEnabledServer(t, backendForUpdate())

	post := func(t *testing.T, id string, body map[string]interface{}) {
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
		if rec.Code != http.StatusCreated {
			t.Fatalf("POST /api/v1/backends (%s) = %d, тело %s", id, rec.Code, rec.Body.String())
		}
	}

	post(t, "gpu-add", map[string]interface{}{
		"id": "gpu-add", "name": "GPU add", "host": "10.9.9.9",
		"backendType": string(types.BackendTypeLlamaCpp),
		"gpuIndex":    3,
	})
	// Backend, созданный через AddBackend, живёт в runtime-состоянии прокси
	// (в p.config.Backends его кладёт LoadState, а не AddBackend) — читаем там.
	if added := s.proxy.GetBackend("gpu-add"); added == nil {
		t.Fatal("POST /api/v1/backends не создал бэкенд")
	} else if added.GPUIndex == nil || *added.GPUIndex != 3 {
		t.Errorf("gpuIndex = %v, want 3 (POST обязан принимать поле)", added.GPUIndex)
	}

	post(t, "gpu-zero", map[string]interface{}{
		"id": "gpu-zero", "name": "GPU zero", "host": "10.9.9.10",
		"backendType": string(types.BackendTypeLlamaCpp),
		"gpuIndex":    0,
	})
	zero := s.proxy.GetBackend("gpu-zero")
	if zero == nil {
		t.Fatal("POST /api/v1/backends не создал бэкенд gpu-zero")
	}
	if zero.GPUIndex == nil || *zero.GPUIndex != 0 {
		t.Errorf("gpuIndex = %v, want явный 0 (я в JSON обязан отличаться от отсутствия поля)", zero.GPUIndex)
	}
	if idx, known := zero.EffectiveGPUIndex(); !known || idx != 0 {
		t.Errorf("EffectiveGPUIndex() = (%d, %v), want (0, true)", idx, known)
	}

	// Ключа нет — индекс неизвестен (прежнее поведение: лок хостовый).
	post(t, "gpu-none", map[string]interface{}{
		"id": "gpu-none", "name": "GPU none", "host": "10.9.9.11",
		"backendType": string(types.BackendTypeLlamaCpp),
	})
	none := s.proxy.GetBackend("gpu-none")
	if none == nil {
		t.Fatal("POST /api/v1/backends не создал бэкенд gpu-none")
	}
	if none.GPUIndex != nil {
		t.Errorf("gpuIndex = %d, want nil (ключ не прислан = «неизвестно»)", *none.GPUIndex)
	}
	if _, known := none.EffectiveGPUIndex(); known {
		t.Error("EffectiveGPUIndex() known = true, want false (лок обязан остаться хостовым)")
	}
}
