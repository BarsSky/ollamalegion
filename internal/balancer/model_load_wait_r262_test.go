package balancer

// model_load_wait_r262_test.go — R262 (2026-10-28): таймаут ожидания загрузки —
// это «ещё грузится», а не провал.
//
// ЖИВОЙ ФАКТ (стенд 8 ГБ VRAM). Загрузка Qwen3.8-27B вернула HTTP 500
// («poll timeout») через ~380 с, при этом воркер модель догрузил и через
// несколько десятков секунд она была state=loaded. Оператор видел «ошибка
// загрузки» при фактически загруженной модели.
//
// Тесты фиксируют:
//   - модель ещё грузится к моменту таймаута → StillLoading (HTTP 202), а не ошибка;
//   - модель УСПЕЛА загрузиться к моменту таймаута → успех (финальная сверка);
//   - модель вообще не появилась у воркера → остаётся провалом;
//   - счётчик в логе поллера считает только state=="loaded".

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// r262LoadServer — воркер, который отдаёт 202 + заданную последовательность
// состояний /api/models. states[i] используется на i-й запрос; последний
// повторяется, когда запросы исчерпаны.
func r262LoadServer(t *testing.T, states ...string) *httptest.Server {
	t.Helper()
	var calls int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/load", "/api/models/load-with-params":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"status": "loading", "estimatedLoadTimeMs": 100,
			})
		case "/api/models":
			n := int(atomic.AddInt32(&calls, 1)) - 1
			state := states[len(states)-1]
			if n < len(states) {
				state = states[n]
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"models": []map[string]interface{}{{"name": "test-model", "state": state}},
			})
		case "/api/models/load/progress":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"state": "loading", "model": "test-model",
			})
		default:
			http.NotFound(w, r)
		}
	}))
}

// r262ShortWait — короткое окно ожидания, чтобы тест не шёл минутами.
func r262ShortWait(t *testing.T) {
	t.Helper()
	t.Setenv("LB_NCTX_PREFLIGHT_MAX_WAIT_SEC", "3")
	t.Setenv("LB_NCTX_PREFLIGHT_WAIT_BUFFER_SEC", "0")
	t.Setenv("LB_NCTX_PREFLIGHT_WAIT_MULTIPLIER", "1")
}

// TestR262_StillLoadingIsNotFailure — главный живой случай: модель грузится,
// окно ожидания истекло. Клиент обязан получить «ещё грузится», а не провал.
func TestR262_StillLoadingIsNotFailure(t *testing.T) {
	r262ShortWait(t)
	server := r262LoadServer(t, "loading") // всегда loading
	defer server.Close()
	host, port := parseTestHostPort(t, server.URL)

	mm := NewModelManager(nil)
	result := mm.executeLlamaCppLoad(host, port, "test-backend", ModelOpRequest{
		Operation: "load", ModelName: "test-model",
	})
	if result == nil {
		t.Fatal("результат nil: таймаут ожидания снова означает «ничего не знаем»")
	}
	if !result.StillLoading {
		t.Fatalf("StillLoading=false: живая загрузка снова выглядит провалом (error=%q)", result.Error)
	}
	if result.Error != "" {
		t.Errorf("при StillLoading ошибка должна быть пустой (это не провал), got %q", result.Error)
	}
	if result.Success {
		t.Error("Success не должен быть true: загрузка ещё не подтверждена")
	}
	if result.PollAfterSec <= 0 {
		t.Errorf("клиенту не сказано, когда опросить статус: pollAfterSec=%d", result.PollAfterSec)
	}
	if result.Message == "" {
		t.Error("нет человекочитаемого объяснения — панель покажет пустоту")
	}
}

// TestR262_LoadedAtDeadlineIsSuccess — модель успела догрузиться к моменту
// таймаута: финальная сверка обязана признать успех, а не «не дождались».
func TestR262_LoadedAtDeadlineIsSuccess(t *testing.T) {
	r262ShortWait(t)
	// Первый опрос — loading (окно ещё идёт), дальше loaded (финальная сверка).
	server := r262LoadServer(t, "loading", "loaded", "loaded")
	defer server.Close()
	host, port := parseTestHostPort(t, server.URL)

	mm := NewModelManager(nil)
	result := mm.executeLlamaCppLoad(host, port, "test-backend", ModelOpRequest{
		Operation: "load", ModelName: "test-model",
	})
	if result == nil {
		t.Fatal("результат nil")
	}
	if !result.Success {
		t.Fatalf("модель загружена к моменту таймаута, а результат — провал (stillLoading=%v error=%q)",
			result.StillLoading, result.Error)
	}
}

// TestR262_ModelNeverAppearedStaysFailure — если модели у воркера нет вовсе,
// это по-прежнему провал: «ещё грузится» не должно маскировать отсутствие.
func TestR262_ModelNeverAppearedStaysFailure(t *testing.T) {
	r262ShortWait(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/load", "/api/models/load-with-params":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "loading", "estimatedLoadTimeMs": 100})
		case "/api/models":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"models": []interface{}{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	host, port := parseTestHostPort(t, server.URL)

	mm := NewModelManager(nil)
	result := mm.executeLlamaCppLoad(host, port, "test-backend", ModelOpRequest{
		Operation: "load", ModelName: "test-model",
	})
	if result == nil {
		t.Fatal("результат nil")
	}
	if result.Success || result.StillLoading {
		t.Fatalf("модель не появилась у воркера, а результат = success=%v stillLoading=%v",
			result.Success, result.StillLoading)
	}
	if result.Error == "" {
		t.Error("провал без причины: оператор не поймёт, что произошло")
	}
}

// TestR262_LoadedCountIgnoresLoadingAndError — диагностика поллера обязана
// считать только реально загруженные модели (тот же критерий, что в R257).
func TestR262_LoadedCountIgnoresLoadingAndError(t *testing.T) {
	models := []types.LlamaCppModel{
		{Name: "a", State: "loaded"},
		{Name: "b", State: "loading"},
		{Name: "c", State: "error"},
		{Name: "d", State: "LOADED"}, // регистр не важен
	}
	if got := loadedModelsCountR262(models); got != 2 {
		t.Fatalf("loadedModelsCountR262 = %d, want 2 (loaded + LOADED), всего записей %d",
			got, len(models))
	}
	if got := loadedModelsCountR262(nil); got != 0 {
		t.Fatalf("nil-список: loadedModelsCountR262 = %d, want 0", got)
	}
}
