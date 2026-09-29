// capacity_from_cppworker_r83_test.go — R83-fix (2026-09-29).
//
// ДЕФЕКТ. Вместимость бэкенда задавалась в ДВУХ местах: CPPWORKER_N_PARALLEL
// (сколько слотов реально поднимает cppworker) и AGENT_MAX_CONCURRENT_REQUESTS
// (сколько параллельных запросов агент сообщает балансеру). Рассинхрон давал
// ровно тот симптом, на который жаловался оператор: «первый кто отправил получил
// ответ, второй получил ошибку 503 и то что модель не загрузить» — агент сообщал
// 1, балансер ставил второй запрос в очередь, хотя воркер держал два слота.
//
// Теперь при незаданном AGENT_MAX_CONCURRENT_REQUESTS агент спрашивает у
// cppworker его defaultNParallel и сообщает балансеру именно это значение.
package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// TestR83_DetectCppWorkerParallelism_FromConfig — значение берётся из
// /api/v1/cppworker/config → config.defaultNParallel.
func TestR83_DetectCppWorkerParallelism_FromConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/cppworker/config" {
			t.Errorf("запрошен неожиданный путь: %s (агент обязан читать именно config)", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"config": map[string]interface{}{"defaultNParallel": 2},
		})
	}))
	defer srv.Close()

	a := &Agent{config: &types.AgentConfig{CppWorkerURL: srv.URL}, httpClient: srv.Client()}
	if got := a.detectCppWorkerParallelism(); got != 2 {
		t.Errorf("detectCppWorkerParallelism() = %d, want 2 — иначе балансер "+
			"пропустит только один параллельный запрос, хотя воркер держит два слота", got)
	}
}

// TestR83_DetectCppWorkerParallelism_SingleSlot — воркер с одним слотом
// (или со старым значением по умолчанию) даёт 0 = «не задано»: балансер
// применит свой дефолт для llama_cpp (1).
func TestR83_DetectCppWorkerParallelism_SingleSlot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"config": map[string]interface{}{"defaultNParallel": 0},
		})
	}))
	defer srv.Close()

	a := &Agent{config: &types.AgentConfig{CppWorkerURL: srv.URL}, httpClient: srv.Client()}
	if got := a.detectCppWorkerParallelism(); got != 0 {
		t.Errorf("detectCppWorkerParallelism() = %d, want 0 (defaultNParallel=0)", got)
	}
}

// TestR83_DetectCppWorkerParallelism_Unreachable — недоступный cppworker не
// должен ронять регистрацию: возвращаем 0 и оставляем решение балансеру.
func TestR83_DetectCppWorkerParallelism_Unreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // порт закрыт

	a := &Agent{config: &types.AgentConfig{CppWorkerURL: url}, httpClient: &http.Client{}}
	if got := a.detectCppWorkerParallelism(); got != 0 {
		t.Errorf("detectCppWorkerParallelism() = %d при недоступном cppworker, want 0", got)
	}
}

// TestR83_DetectCppWorkerParallelism_BadJSON — мусор в ответе не должен
// приводить к панике или к выдуманной вместимости.
func TestR83_DetectCppWorkerParallelism_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json at all"))
	}))
	defer srv.Close()

	a := &Agent{config: &types.AgentConfig{CppWorkerURL: srv.URL}, httpClient: srv.Client()}
	if got := a.detectCppWorkerParallelism(); got != 0 {
		t.Errorf("detectCppWorkerParallelism() = %d при невалидном JSON, want 0", got)
	}
}

// TestR83_DetectCppWorkerParallelism_TimeoutIsEnforced — недоступный (зависший)
// cppworker не должен вешать регистрацию агента: запрос обязан прерваться по
// таймауту и вернуть 0 («не задано»), а не ждать бесконечно.
func TestR83_DetectCppWorkerParallelism_TimeoutIsEnforced(t *testing.T) {
	old := cppWorkerProbeTimeout
	cppWorkerProbeTimeout = 150 * time.Millisecond
	defer func() { cppWorkerProbeTimeout = old }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done(): // клиент ушёл по таймауту — как и требуется
		case <-time.After(10 * time.Second):
			t.Error("сервер ждал 10 с: клиентский таймаут не сработал")
		}
	}))
	defer srv.Close()

	a := &Agent{config: &types.AgentConfig{CppWorkerURL: srv.URL}, httpClient: srv.Client()}
	start := time.Now()
	got := a.detectCppWorkerParallelism()
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("detectCppWorkerParallelism() ждал %v — таймаут не enforced", elapsed)
	}
	if got != 0 {
		t.Errorf("detectCppWorkerParallelism() = %d при таймауте, want 0", got)
	}
}
