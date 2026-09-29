// backend_reachable_r83_test.go — R83 (2026-09-29).
//
// ДЕФЕКТ, КОТОРЫЙ ЗДЕСЬ ЗАФИКСИРОВАН (воспроизведён на живом стенде).
//
// При capacity=3 и трёх параллельных запросах через балансер первый запрос
// обслуживался нормально, а второй и третий получали:
//
//	503 {"error":"model 'Qwen3-Instruct-2507-q4km' is not loaded and auto-load
//	     failed: backend unreachable: cppworker-gpu-bundled-agent
//	     (cppworker did not answer /api/models)"}
//
// хотя модель была загружена и первый запрос по ней работал.
//
// ПРИЧИНА. backendReachable проверял живость cppworker через GET /api/models с
// таймаутом 2 c. А /api/models во время генерации ЗАВИСАЕТ: он читает
// memfit-бюджет, который звал bridge.GetGPUInfo → cudaMemGetInfo, а CUDA-вызовы
// сериализуются, пока идёт инференс. Замеры во время генерации (живой стенд):
//
//	/api/gpu                     200 за 4-7 мс
//	/api/models/active-queries   200 за ~4 мс
//	/api/version                 200 за ~4 мс
//	/api/models, /api/info       не отвечали 8+ секунд
//
// Поэтому проверка живости врала «узел мёртв» → ensureModelLoadedOnBackend
// отказывался доверять снапшоту метрик → клиент получал 503 вместо очереди.
//
// ИСПРАВЛЕНИЕ: живость проверяется по /api/version (не блокируется инференсом),
// с откатом на /api/models только если /api/version недоступен вовсе (старые
// сборки). Плюс memfit-бюджет больше не зовёт CUDA из горячего пути
// (см. internal/cppbackend/memfit_adapter.go).
package balancer

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// backendReachableHarness — Proxy + LlamaCppRouter с одним бэкендом, указывающим
// на тестовый сервер. Отдельный хелпер, потому что проверки живости не нужен
// весь autoload-стенд (и его env-настройки).
func backendReachableHarness(t *testing.T, serverURL string) *LlamaCppRouter {
	t.Helper()
	host, port := splitHostPort(t, serverURL)

	mm := NewModelManager(nil)
	p := &Proxy{
		config:       &types.LoadBalancerConfig{},
		backends:     map[string]*BackendState{},
		metricsMgr:   NewMetricsManager(),
		modelManager: mm,
	}
	mm.proxy = p
	p.backends["test-bk"] = &BackendState{
		Backend: &types.Backend{
			ID:            "test-bk",
			Host:          host,
			Status:        types.StatusHealthy,
			Type:          types.BackendTypeLlamaCpp,
			CppWorkerPort: port,
		},
	}
	return NewLlamaCppRouter(p)
}

// livenessAwareHandler оборачивает тестовый обработчик cppworker так, чтобы он
// отвечал на /api/version — иначе проверка живости (которая теперь использует
// этот путь) получала бы 404 и все сценарии ломались бы на ровном месте.
func livenessAwareHandler(inner http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/version" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"version":"test"}`))
			return
		}
		inner(w, r)
	}
}

// TestR83_BackendReachable_UsesNonBlockingEndpoint — живость должна определяться
// эндпоинтом, который НЕ блокируется инференсом.
//
// Тест воспроизводит ситуацию «/api/models висит, /api/version отвечает»:
// мок отвечает на /api/version и НЕ отвечает на /api/models. Раньше
// backendReachable возвращал false (таймаут) и балансер отвечал 503 «backend
// unreachable»; теперь обязан вернуть true.
func TestR83_BackendReachable_UsesNonBlockingEndpoint(t *testing.T) {
	var modelsCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/version":
			_, _ = w.Write([]byte(`{"version":"test"}`))
		case "/api/models":
			// Имитируем «висит на инференсе»: не отвечаем, но отмечаем вызов.
			modelsCalled = true
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	lr := backendReachableHarness(t, srv.URL)

	if !lr.backendReachable("test-bk") {
		t.Fatal("backendReachable вернул false, хотя /api/version отвечает: " +
			"живость снова определяется блокирующимся /api/models — при параллельных " +
			"запросах клиент получит 503 «backend unreachable» на живой модели")
	}
	if modelsCalled {
		t.Error("backendReachable обратился к /api/models — этот путь блокируется " +
			"во время генерации и не годится для проверки живости")
	}
}

// TestR83_BackendReachable_FallsBackForLegacyCppworker — старые сборки без
// /api/version должны по-прежнему определяться как живые через /api/models.
func TestR83_BackendReachable_FallsBackForLegacyCppworker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/version":
			http.NotFound(w, r) // legacy-сборка: пути нет
		case "/api/models":
			_, _ = w.Write([]byte(`{"count":0,"models":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	lr := backendReachableHarness(t, srv.URL)

	if !lr.backendReachable("test-bk") {
		t.Fatal("для legacy-cppworker без /api/version живость должна проверяться " +
			"через /api/models (обратная совместимость)")
	}
}

// TestR83_BackendReachable_DeadBackendIsFalse — мёртвый узел по-прежнему должен
// определяться как недоступный, иначе failover перестанет работать.
func TestR83_BackendReachable_DeadBackendIsFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	lr := backendReachableHarness(t, srv.URL)

	if lr.backendReachable("test-bk") {
		t.Fatal("backendReachable вернул true для узла, отвечающего 500: сломается failover")
	}
}

// TestR83_LivenessAwareHandler_Sanity — вспомогательный хендлер действительно
// подменяет только /api/version и не мешает остальным путям.
func TestR83_LivenessAwareHandler_Sanity(t *testing.T) {
	called := ""
	h := livenessAwareHandler(func(w http.ResponseWriter, r *http.Request) {
		called = r.URL.Path
		_, _ = w.Write([]byte("ok"))
	})

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/api/version", nil))
	if !strings.Contains(rec.Body.String(), "version") {
		t.Errorf("/api/version должен отдаваться обёрткой, получено %q", rec.Body.String())
	}

	rec2 := httptest.NewRecorder()
	h(rec2, httptest.NewRequest(http.MethodGet, "/api/models", nil))
	if called != "/api/models" {
		t.Errorf("обёртка перехватила чужой путь: inner вызван с %q", called)
	}
}
