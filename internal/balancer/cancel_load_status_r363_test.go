// cancel_load_status_r363_test.go — R363 (2026-10-31), аудит abort A4 (сторона балансера).
//
// РЕШЕНИЕ ОПЕРАТОРА: «Да загрузку прерывать при отмене».
//
// Что делает балансер:
//   - на КЛИЕНТСКУЮ отмену (/api/cancel) по умолчанию передаёт cancel_load=true
//     (клиент может явно отказаться, прислав cancel_load=false);
//   - сторожевые обрывы (таймаут стрима, тишина upstream) идут другим путём и загрузку НЕ
//     трогают — иначе таймаут одного клиента обрывал бы общую загрузку;
//   - адресатом отмены становится и бэкенд, у которого модель сейчас ГРУЗИТСЯ (раньше
//     смотрели только LoadedModels, и Stop во время загрузки уходил в пустоту);
//   - статусы воркера load_cancelled / load_abort_requested отражаются в верхнем статусе
//     честно: «прервана» и «флаг выставлен, ещё останавливается».
//
// Проверяется:
//  1. load_cancelled → верхний статус load_cancelled с пояснением;
//  2. load_abort_requested → отдельный статус (не «cancelled»: чтение весов ещё идёт);
//  3. загрузка в кэше поллера делает бэкенд адресатом отмены;
//  4. прежние статусы (not_cancelled/no_active_generation) не сломаны;
//  5. сторож: клиентская отмена ставит cancel_load по умолчанию, сторожевой путь — нет.
package balancer

import (
	"os"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR363_StatusLoadCancelled — загрузка прервана: это успех отмены.
func TestR363_StatusLoadCancelled(t *testing.T) {
	results := []map[string]interface{}{
		{"backend": "b1", "ok": true, "cancelled": float64(0), "status": "load_cancelled",
			"supported": true},
	}
	status, hint := cancelStatusR343(results, []string{"b1"}, 1)
	if status != "load_cancelled" {
		t.Fatalf("status = %q, want load_cancelled", status)
	}
	if !strings.Contains(hint, "прервана") {
		t.Errorf("подсказка %q не объясняет, что именно прервано", hint)
	}
}

// TestR363_StatusLoadAbortPending — флаг выставлен, но загрузка ещё идёт.
func TestR363_StatusLoadAbortPending(t *testing.T) {
	results := []map[string]interface{}{
		{"backend": "b1", "ok": true, "cancelled": float64(0), "status": "load_abort_requested"},
	}
	status, hint := cancelStatusR343(results, []string{"b1"}, 1)
	if status != "load_abort_requested" {
		t.Fatalf("status = %q, want load_abort_requested: выдавать «cancelled», пока "+
			"чтение весов идёт, значит врать клиенту", status)
	}
	if !strings.Contains(hint, "тензор") {
		t.Errorf("подсказка %q не объясняет задержку", hint)
	}
}

// TestR363_LoadingModelIsCancelTarget — Stop во время загрузки должен дойти до узла.
func TestR363_LoadingModelIsCancelTarget(t *testing.T) {
	p := &Proxy{
		config:     &types.LoadBalancerConfig{},
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
	}
	p.backends["b1"] = &BackendState{Backend: &types.Backend{
		ID: "b1", Host: "10.0.0.5", CppWorkerPort: 18092,
		Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp,
	}}
	p.metricsMgr.mu.Lock()
	p.metricsMgr.llamaMetrics["b1"] = &types.LlamaCppMetrics{
		LoadingModels: []types.LlamaCppModel{{Name: "gemma-4-E4B-it-Q4_K_M", State: "loading"}},
	}
	p.metricsMgr.mu.Unlock()

	targets := p.cancelTargets("gemma-4-E4B-it-Q4_K_M")
	found := false
	for _, id := range targets {
		if id == "b1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("адресаты %v не содержат b1, где модель ГРУЗИТСЯ: Stop во время загрузки "+
			"уходил бы в пустоту (смотрели только LoadedModels)", targets)
	}
}

// TestR363_PreviousStatusesIntact — прежние статусы R343 не сломаны.
func TestR363_PreviousStatusesIntact(t *testing.T) {
	accepted := []map[string]interface{}{{"backend": "b1", "ok": true, "cancelled": float64(0), "supported": true}}
	if got, _ := cancelStatusR343(accepted, []string{"b1"}, 1); got != "not_cancelled" {
		t.Errorf("приняли, но не отменили → %q, want not_cancelled", got)
	}
	if got, _ := cancelStatusR343(nil, nil, 0); got != "no_active_generation" {
		t.Errorf("нет целей → %q, want no_active_generation", got)
	}
}

// TestR363_ClientCancelDefaultsToLoadCancel_STEWARD — клиентская отмена ставит cancel_load,
// сторожевой путь — нет.
func TestR363_ClientCancelDefaultsToLoadCancel_STEWARD(t *testing.T) {
	read := func(name string) string {
		for _, p := range []string{name, "internal/balancer/" + name} {
			if b, err := os.ReadFile(p); err == nil {
				return string(b)
			}
		}
		return ""
	}

	client := read("cancel_client_r229.go")
	if client == "" {
		t.Fatal("не удалось прочитать cancel_client_r229.go")
	}
	if !strings.Contains(client, "req.CancelLoad = &allowLoadCancel") {
		t.Error("клиентская отмена не выставляет cancel_load по умолчанию: Stop во время " +
			"загрузки снова не будет её прерывать")
	}
	if !strings.Contains(client, `json:"cancel_load"`) {
		t.Error("clientCancelBody не принимает cancel_load — клиенту нечем отказаться")
	}

	// Сторожевые обрывы: в cancel_upstream_r184.go поля cancel_load быть НЕ должно.
	upstream := read("cancel_upstream_r184.go")
	if upstream == "" {
		t.Fatal("не удалось прочитать cancel_upstream_r184.go")
	}
	if strings.Contains(upstream, "cancel_load") {
		t.Error("сторожевой путь (таймаут/тишина) ставит cancel_load: таймаут одного клиента " +
			"оборвал бы загрузку, которую ждут другие")
	}
}
