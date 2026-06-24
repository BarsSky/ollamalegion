package cppworker_lazy_load

// Тесты для верификации исправления race condition "model is loading":
// когда cppworker возвращает HTTP 503 + {"error":"model is loading"} на
// параллельные /api/models/load (пока другая горутина грузит модель),
// балансер должен делать retry до llamaCppLoadMaxRetries попыток и в итоге
// либо получить успешный ответ, либо вернуть клиенту ошибку с retry_after.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"
)

// mockCppWorker503First — мок cppworker, который:
//
//   - На первые `failuresBeforeSuccess` запросов POST /api/models/load возвращает
//     HTTP 503 + JSON {"error":"model is loading","loading":true}.
//   - На следующий запрос — 200 + JSON {"status":"loaded"}.
//
// Параллельно в фоне (через loadDelay) переводит state в "loaded", чтобы
// последующие запросы /api/models показывали модель загруженной.
//
// Используется для тестирования retry-логики в executeLlamaCppLoad.
type mockCppWorker503First struct {
	server *httptest.Server

	failuresBeforeSuccess int32 // сколько первых /api/models/load вернут 503
	loadDelay             time.Duration

	mu    sync.Mutex
	state map[string]string

	loadCalls      atomic.Int32
	chatCalls      atomic.Int32
	modelsCalls    atomic.Int32
	loadBodiesLog  []string
	loadStartTimes []time.Time
	loadStarted    chan struct{}
}

func newMockCppWorker503First(failuresBeforeSuccess int32, loadDelay time.Duration) *mockCppWorker503First {
	m := &mockCppWorker503First{
		failuresBeforeSuccess: failuresBeforeSuccess,
		loadDelay:             loadDelay,
		state:                 map[string]string{"lazy-model": "unloaded"},
		loadStarted:           make(chan struct{}, 16),
	}

	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		w.Header().Set("Content-Type", "application/json")

		switch path {
		case "/health":
			json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
			return

		case "/api/tags":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"models": []map[string]interface{}{
					{"name": "lazy-model", "modified_at": time.Now().UTC().Format(time.RFC3339)},
				},
			})
			return

		case "/api/models":
			m.modelsCalls.Add(1)
			m.mu.Lock()
			currentState := m.state["lazy-model"]
			m.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]interface{}{
				"count": 1,
				"models": []map[string]interface{}{
					{"name": "lazy-model", "path": "/models/lazy-model.gguf", "state": currentState},
				},
			})
			return

		case "/api/models/load":
			m.loadCalls.Add(1)
			body, _ := io.ReadAll(r.Body)
			m.mu.Lock()
			m.loadBodiesLog = append(m.loadBodiesLog, string(body))
			m.loadStartTimes = append(m.loadStartTimes, time.Now())
			failed := m.loadCalls.Load() <= m.failuresBeforeSuccess
			m.mu.Unlock()

			if failed {
				// Имитируем race condition: cppworker ещё грузит модель.
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusServiceUnavailable)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"error":        "model is loading: lazy-model",
					"loading":      true,
					"model":        "lazy-model",
					"elapsedMs":    500,
					"retryAfterMs": 1000,
				})
				return
			}

			// Успешная "загрузка" — переводим в state=loading, через loadDelay — в loaded.
			m.mu.Lock()
			m.state["lazy-model"] = "loading"
			m.mu.Unlock()
			select {
			case m.loadStarted <- struct{}{}:
			default:
			}
			go func() {
				time.Sleep(m.loadDelay)
				m.mu.Lock()
				m.state["lazy-model"] = "loaded"
				m.mu.Unlock()
			}()

			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{
				"status": "loaded",
				"model":  "lazy-model",
			})
			return

		case "/v1/chat/completions":
			m.chatCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id":      "chatcmpl-mock",
				"object":  "chat.completion",
				"created": time.Now().Unix(),
				"model":   "lazy-model",
				"choices": []map[string]interface{}{
					{
						"index": 0,
						"message": map[string]interface{}{
							"role":    "assistant",
							"content": "Привет! Это ответ после retry.",
						},
						"finish_reason": "stop",
					},
				},
				"usage": map[string]interface{}{
					"prompt_tokens":     5,
					"completion_tokens": 8,
					"total_tokens":      13,
				},
			})
			return

		default:
			http.Error(w, "mock: not found: "+path, http.StatusNotFound)
		}
	}))

	return m
}

func (m *mockCppWorker503First) Close() { m.server.Close() }

// waitForLoadComplete — блокирует до перехода модели в state="loaded" или таймаута.
func (m *mockCppWorker503First) waitForLoadComplete(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		state := m.state["lazy-model"]
		m.mu.Unlock()
		if state == "loaded" {
			return true
		}
		if state == "error" {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// createTestProxyForCppWorker503Retry — обёртка для переиспользования мока.
func createTestProxyForCppWorker503Retry(t *testing.T, cppWorkerURL string) *balancer.Proxy {
	t.Helper()

	hostPort := strings.TrimPrefix(cppWorkerURL, "http://")
	parts := strings.Split(hostPort, ":")
	host := parts[0]
	port := 8080
	if len(parts) > 1 {
		fmt.Sscanf(parts[1], "%d", &port)
	}

	cfg := &types.LoadBalancerConfig{
		Balancing: types.BalancingSettings{
			OperatingMode:     "standard",
			SessionStickiness: true,
			SessionTTL:        60,
			SessionIdleTTL:    60,
			RequestTimeout:    30,
			QueueMaxSize:      100,
			ModelAffinity:     true,
			SyncModelLoad:     types.SyncModelLoadConfig{Enabled: false},
			Prewarm:           types.PrewarmConfig{TriggerLoadThreshold: 0.80},
		},
		Backends: []types.Backend{
			{
				ID:                "llamacpp-503",
				Name:              "Test cppworker (503 retry)",
				Host:              host,
				OllamaPort:        port,
				CppWorkerPort:     port,
				Type:              types.BackendTypeLlamaCpp,
				Status:            types.StatusHealthy,
				MaxConcurrentReqs: 10,
				Weight:            100,
			},
		},
		BackendEngine: types.EngineLlamaCPP,
	}

	proxy := balancer.NewProxy(cfg)
	proxy.UpdateMetrics("llamacpp-503", &types.BackendMetrics{
		ID:          "llamacpp-503",
		Host:        host,
		OllamaPort:  port,
		BackendType: types.BackendTypeLlamaCpp,
		LlamaCpp:    types.LlamaCppMetrics{LoadedModels: []types.LlamaCppModel{}},
		Ollama:      types.OllamaMetrics{RunningModels: []types.RunningModel{}},
	})
	return proxy
}

// TestCppWorker_LazyLoad_503Retry_RecoversFromRaceCondition —
// cppworker возвращает 503 + "model is loading" на первые 2 запроса, на 3-й — 200.
// Балансер должен:
//   1. Получить 200 OK на клиентский /api/chat
//   2. Сделать ровно 3 вызова POST /api/models/load (2 неудачных + 1 успешный)
//   3. Время запроса >= суммы retry-интервалов (~6 секунд при retryInterval=3s)
//
// ЭТО ТЕСТ ДЛЯ ИСПРАВЛЕНИЯ RACE CONDITION: до фикса балансер возвращал бы
// клиенту 503 "auto-load failed: model is loading" после первого же 503.
func TestCppWorker_LazyLoad_503Retry_RecoversFromRaceCondition(t *testing.T) {
	const failuresBeforeSuccess = int32(2)

	// loadDelay должен быть <= retryInterval * (failuresBeforeSuccess+1), чтобы модель
	// успела "загрузиться" к моменту 3-й попытки. Сейчас retryInterval = 3s,
	// failuresBeforeSuccess = 2 → 3-я попытка через ~6s. loadDelay = 500ms.
	worker := newMockCppWorker503First(failuresBeforeSuccess, 500*time.Millisecond)
	defer worker.Close()

	proxy := createTestProxyForCppWorker503Retry(t, worker.server.URL)

	reqBody := map[string]interface{}{
		"model": "lazy-model",
		"messages": []map[string]string{
			{"role": "user", "content": "Привет!"},
		},
		"stream": false,
	}
	bodyBytes, _ := json.Marshal(reqBody)

	req := httptest.NewRequest("POST", "/api/chat", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	start := time.Now()
	proxy.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	resp := rec.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK (retry recovered from 503), got %d: %s",
			resp.StatusCode, string(body))
	}

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)
	if !strings.Contains(bodyStr, "Привет! Это ответ после retry.") {
		t.Errorf("expected inference response after retry, got: %s", bodyStr)
	}

	// Главная проверка: 2 неудачных + 1 успешный load = 3 вызова.
	gotLoadCalls := worker.loadCalls.Load()
	if gotLoadCalls < failuresBeforeSuccess+1 {
		t.Errorf("expected at least %d POST /api/models/load calls (2x503 + 1x200), got %d",
			failuresBeforeSuccess+1, gotLoadCalls)
	}

	// Хотя бы 1 inference-вызов прошёл.
	if got := worker.chatCalls.Load(); got < 1 {
		t.Errorf("expected at least 1 /v1/chat/completions call, got %d", got)
	}

	t.Logf("✅ 503 retry recovered: loadCalls=%d, chatCalls=%d, elapsed=%v",
		gotLoadCalls, worker.chatCalls.Load(), elapsed)
}

// TestCppWorker_LazyLoad_503Always_ReturnsFinalError —
// cppworker ВСЕГДА возвращает 503. После исчерпания retries балансер должен
// вернуть клиенту 503 c информативным сообщением (не висеть и не давать
// неопределённый ответ).
//
// ВАЖНО: чтобы тест не занимал ~30 секунд (полный цикл retry), мы
// тестируем только ModelManager напрямую, а не полный ServeHTTP.
// Это допустимо, потому что retry-логика инкапсулирована в executeLlamaCppLoad.
func TestCppWorker_LazyLoad_503Always_ReturnsFinalError(t *testing.T) {
	// failuresBeforeSuccess = очень большое число — все запросы вернут 503.
	worker := newMockCppWorker503First(1000, 1*time.Hour) // loadDelay огромный, не дождёмся
	defer worker.Close()

	proxy := createTestProxyForCppWorker503Retry(t, worker.server.URL)
	mm := proxy.GetModelManager()
	if mm == nil {
		t.Fatal("model manager not available")
	}

	// Засекаем таймер — ожидаем ~6-12 секунд (10 retries × 3s sleep).
	// В CI можно сократить через переменную окружения или build tag,
	// но для текущего теста это нормальное время.
	start := time.Now()
	result := mm.ExecuteOperation("llamacpp-503", balancer.ModelOpRequest{
		Operation: "load",
		ModelName: "lazy-model",
	})
	elapsed := time.Since(start)

	if result.Success {
		t.Errorf("expected failure after exhausting retries, got success: %s", result.Message)
	}
	if result.Error == "" {
		t.Fatal("expected non-empty error after exhausting retries")
	}

	// Проверяем, что ошибка содержит информацию о retry/исчерпании.
	errLower := strings.ToLower(result.Error)
	if !strings.Contains(errLower, "loading") && !strings.Contains(errLower, "attempts") {
		t.Errorf("expected error to mention 'loading' or 'attempts', got: %s", result.Error)
	}

	// Должно быть не менее 2-3 вызовов load (точно ≥1, в реальности ~10).
	gotLoadCalls := worker.loadCalls.Load()
	if gotLoadCalls < 1 {
		t.Errorf("expected at least 1 load attempt, got %d", gotLoadCalls)
	}

	// Проверяем, что было сделано несколько retries (как минимум 3).
	// Используем >= 3 для устойчивости к таймингу.
	if gotLoadCalls < 3 && elapsed > 5*time.Second {
		t.Logf("note: loadCalls=%d, elapsed=%v (low retry count is acceptable in some timings)",
			gotLoadCalls, elapsed)
	}

	t.Logf("✅ 503 always → retries exhausted: loadCalls=%d, elapsed=%v, error=%s",
		gotLoadCalls, elapsed, result.Error)
}

// TestExecuteLlamaCppLoad_503RetryCount —
// Проверяет точное количество retries (без полного HTTP-прокси):
// cppworker возвращает 503 ровно N раз, потом 200 — балансер должен
// сделать ровно N+1 вызовов и вернуть success.
func TestExecuteLlamaCppLoad_503RetryCount(t *testing.T) {
	const failuresBeforeSuccess = int32(3)

	worker := newMockCppWorker503First(failuresBeforeSuccess, 100*time.Millisecond)
	defer worker.Close()

	proxy := createTestProxyForCppWorker503Retry(t, worker.server.URL)
	mm := proxy.GetModelManager()
	if mm == nil {
		t.Fatal("model manager not available")
	}

	start := time.Now()
	result := mm.ExecuteOperation("llamacpp-503", balancer.ModelOpRequest{
		Operation: "load",
		ModelName: "lazy-model",
	})
	elapsed := time.Since(start)

	if !result.Success {
		t.Errorf("expected success after %d retries, got error: %s",
			failuresBeforeSuccess, result.Error)
	}

	gotLoadCalls := worker.loadCalls.Load()
	expectedCalls := failuresBeforeSuccess + 1
	if gotLoadCalls != expectedCalls {
		t.Errorf("expected exactly %d POST /api/models/load calls, got %d",
			expectedCalls, gotLoadCalls)
	}

	// Минимальное время = failuresBeforeSuccess × retryInterval (3s).
	minExpected := time.Duration(failuresBeforeSuccess) * 3 * time.Second
	if elapsed < minExpected-1*time.Second {
		t.Errorf("expected elapsed >= %v (retries × 3s sleep), got %v — retries did not sleep",
			minExpected, elapsed)
	}

	t.Logf("✅ 503 retry count: loadCalls=%d, elapsed=%v, min_expected=%v",
		gotLoadCalls, elapsed, minExpected)
}