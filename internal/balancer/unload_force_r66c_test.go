//go:build llama_stub

// unload_force_r66c_test.go — R66c (2026-09-22): выгрузка ЗАНЯТОЙ модели.
//
// Проблема: cppworker отказывает в unload, если по модели есть активные
// inference-запросы — отвечает 409 и принимает ?force=true, чтобы отменить
// генерации и всё-таки выгрузить (cmd/cppworker/handlers_model.go:904-929).
// Балансер этот флаг не прокидывал, а в ModelOpRequest не было поля Force,
// поэтому из WebUI выгрузить занятую модель было НЕВОЗМОЖНО: пользователь
// видел "model is busy with active inference requests" и всё.
//
// Тесты проверяют обе половины контракта:
//  1. force=true доезжает до cppworker как query-параметр (и не появляется
//     без явного запроса);
//  2. 409 от cppworker превращается в структурный ответ Busy/RetryWithForce,
//     по которому UI предлагает повтор с force.

package balancer

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// newUnloadTestManager — минимальный ModelManager для вызова
// executeLlamaCppUnload (повторяет setup из unload_r6032_test.go).
func newUnloadTestManager() *ModelManager {
	mm := NewModelManager(nil)
	mm.proxy = &Proxy{
		config:     &types.LoadBalancerConfig{},
		metricsMgr: NewMetricsManager(),
		nctxReload: NewNCtxReloadCoordinator(NCtxReloadConfig{}),
	}
	return mm
}

// TestExecuteLlamaCppUnload_ForwardsForce — force доезжает до cppworker.
func TestExecuteLlamaCppUnload_ForwardsForce(t *testing.T) {
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models/unload" {
			gotQuery = r.URL.RawQuery
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"unloaded"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	host, port := parseTestHostPort(t, server.URL)

	mm := newUnloadTestManager()

	force := true
	res := mm.executeLlamaCppUnload(host, port, "b1", ModelOpRequest{
		Operation: "unload", ModelName: "m1", Force: &force,
	})
	if res == nil || !res.Success {
		t.Fatalf("force=true: ожидался успех, got %+v", res)
	}
	if gotQuery != "name=m1&force=true" {
		t.Errorf("force=true: query=%q, want %q", gotQuery, "name=m1&force=true")
	}

	// Без Force — прежнее безопасное поведение.
	gotQuery = ""
	res = mm.executeLlamaCppUnload(host, port, "b1", ModelOpRequest{
		Operation: "unload", ModelName: "m1",
	})
	if res == nil || !res.Success {
		t.Fatalf("без force: ожидался успех, got %+v", res)
	}
	if gotQuery != "name=m1" {
		t.Errorf("без force: query=%q, want %q", gotQuery, "name=m1")
	}

	// Явный false — тоже без force (не должно быть сюрпризов у клиентов,
	// которые присылают force:false явно).
	gotQuery = ""
	noForce := false
	res = mm.executeLlamaCppUnload(host, port, "b1", ModelOpRequest{
		Operation: "unload", ModelName: "m1", Force: &noForce,
	})
	if res == nil || !res.Success {
		t.Fatalf("force=false: ожидался успех, got %+v", res)
	}
	if gotQuery != "name=m1" {
		t.Errorf("force=false: query=%q, want %q", gotQuery, "name=m1")
	}
}

// TestExecuteLlamaCppUnload_BusySurfacesRetryWithForce — 409 от cppworker
// (модель занята) должен быть виден клиенту как Busy + RetryWithForce.
func TestExecuteLlamaCppUnload_BusySurfacesRetryWithForce(t *testing.T) {
	busyBody := `{"error":"model is busy with active inference requests",` +
		`"hint":"wait for in-flight requests to complete, or retry with ?force=true to cancel them"}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models/unload" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(busyBody))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	host, port := parseTestHostPort(t, server.URL)

	mm := newUnloadTestManager()
	res := mm.executeLlamaCppUnload(host, port, "b1", ModelOpRequest{
		Operation: "unload", ModelName: "m1",
	})

	if res == nil {
		t.Fatal("ожидался non-nil результат")
	}
	if res.Success {
		t.Fatal("409 должен быть неуспехом")
	}
	if !res.Busy {
		t.Error("Busy=false: UI не поймёт, что модель занята, а не сломана")
	}
	if !res.RetryWithForce {
		t.Error("RetryWithForce=false: UI не предложит повтор с force=true")
	}
	if res.Error == "" {
		t.Error("ожидалось человекочитаемое сообщение об ошибке")
	}
}
