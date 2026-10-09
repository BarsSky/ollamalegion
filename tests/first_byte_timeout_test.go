package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"
)

// TestOpenAIChat_SlowFirstToken_HoldsConnection проверяет, что /v1/chat/completions
// через балансер держит соединение, если upstream (cppworker) задерживает первый
// токен, но сразу отправляет HTTP-заголовки и SSE keepalive.
//
// Это воспроизводит проблему Roo Code: "timeout awaiting response headers" / EOF,
// когда модель на partial GPU offload/RAM fallback долго готовит первый токен.
func TestOpenAIChat_SlowFirstToken_HoldsConnection(t *testing.T) {
	// upstream, который сразу шлёт заголовки + keepalive, но первый data-чанк
	// приходит только через 50 секунд. Старый ResponseHeaderTimeout=45s ломался
	// бы на этом тесте; новый FirstByteTimeout=120s должен выдержать.
	upstream := startSlowSSEServer(50*time.Second, []string{"hello", " world", "[DONE]"})
	defer func() {
		// slow SSE-сервер ждёт 50s перед отправкой первого токена;
		// httptest.Server.Close() зависнет на WaitGroup, ожидая handler'ов.
		// Принудительно закрываем listener и активные соединения (тот же
		// паттерн что и в TestOpenAIChat_HeaderTimeout_StillWorks).
		upstream.Listener.Close()
		upstream.CloseClientConnections()
	}()

	host, port := hostPort(upstream.URL)
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host: "0.0.0.0", Port: 18080, APIPort: 18081,
		},
		Backends: []types.Backend{
			{
				ID: "cpp-gpu", Host: host,
				CppWorkerPort: port, Weight: 1,
				MaxConcurrentReqs: 10,
				Status:            types.StatusHealthy,
				Type:              types.BackendTypeLlamaCpp,
			},
		},
		Balancing: types.BalancingSettings{
			Algorithm:            types.AlgorithmResourceAware,
			ModelAffinity:        true,
			SessionStickiness:    true,
			FirstByteTimeout:     120,
			StreamingIdleTimeout: 120,
			RequestTimeout:       120,
			QueueTimeout:         300,
			QueueMaxSize:         100,
			QueueWorkers:         4,
			SessionTTL:           900,
			OperatingMode:        "llama_cpp",
		},
		Resources: types.ResourceLimits{
			GPU:    types.GPULimits{MaxUsagePercent: 90, MaxVRAMUsagePercent: 85},
			CPU:    types.CPULimits{MaxUsagePercent: 80},
			Memory: types.MemoryLimits{MaxUsagePercent: 85},
		},
	}

	proxy := balancer.NewProxy(cfg)
	defer proxy.Shutdown(testCtx(t))

	// Помечаем модель загруженной в метриках, чтобы ensureModelLoadedOnBackend
	// не ходила в /api/models upstream (а то тест зависнет на auto-load).
	proxy.UpdateMetrics("cpp-gpu", &types.BackendMetrics{
		ID:     "cpp-gpu",
		Status: types.StatusHealthy,
		LlamaCpp: types.LlamaCppMetrics{
			LoadedModels: []types.LlamaCppModel{
				{Name: "gemma-4-E4B-it-Q4_K_M", State: "loaded"},
			},
		},
	})

	// Убеждаемся, что ResponseHeaderTimeout действительно взят из FirstByteTimeout.
	// Мы не можем достучаться до unexported полей, но можем проверить поведение:
	// с FirstByteTimeout=120 соединение должно выдержать 50s задержку.

	body, _ := json.Marshal(map[string]interface{}{
		"model":    "gemma-4-E4B-it-Q4_K_M",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
		"stream":   true,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.100:54321"

	rec := httptest.NewRecorder()
	done := make(chan bool, 1)
	go func() { proxy.ServeHTTP(rec, req); done <- true }()

	select {
	case <-done:
		// Ожидаем 200 и SSE-ответ с "hello world".
		if rec.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
		}
		bodyStr := rec.Body.String()
		if !strings.Contains(bodyStr, "hello") || !strings.Contains(bodyStr, "world") {
			t.Fatalf("expected SSE response with 'hello world', got: %s", bodyStr)
		}
		if !strings.Contains(bodyStr, ": keepalive") {
			t.Log("no keepalive comment in response (upstream may not send it)")
		}
		t.Log("PASS ✅ slow first token held")
	case <-time.After(60 * time.Second):
		t.Fatal("request did not complete within 60s")
	}
}

// TestOpenAIChat_HeaderTimeout_StillWorks проверяет, что если upstream вообще не
// отправляет заголовки в течение FirstByteTimeout, балансер обрывает соединение
// и возвращает ошибку (а не ждёт бесконечно).
//
// R91 (2026-10-09): механизм проверяется с ЯВНЫМ opt-in — LB_ALLOW_PROFILE_TIMEOUTS=1.
// Профильные таймауты по умолчанию игнорируются (R83/v62: «таймаут — рубильник
// оператора, а не скрытая настройка профиля модели», см. profileTimeoutsAllowed в
// internal/balancer/proxy_timeout.go), поэтому без этой переменной тест ждал бы
// вечно и падал по своему же 10-секундному дедлайну. Тест фиксирует, что
// ResponseHeaderTimeout РАБОТАЕТ, когда оператор его разрешил; поведение по
// умолчанию (профиль игнорируется) проверяет
// TestOpenAIChat_ProfileTimeoutIgnoredByDefault ниже.
func TestOpenAIChat_HeaderTimeout_StillWorks(t *testing.T) {
	t.Setenv("LB_ALLOW_PROFILE_TIMEOUTS", "1")
	upstream := startHungServer()
	defer func() {
		// hung-сервер держит соединения открытыми; Close может зависнуть.
		// Принудительно закрываем listener, не дожидаясь завершения handler'ов.
		upstream.Listener.Close()
		upstream.CloseClientConnections()
	}()

	host, port := hostPort(upstream.URL)
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host: "0.0.0.0", Port: 18080, APIPort: 18081,
		},
		Backends: []types.Backend{
			{
				ID: "cpp-gpu", Host: host,
				CppWorkerPort: port, Weight: 1,
				MaxConcurrentReqs: 10,
				Status:            types.StatusHealthy,
				Type:              types.BackendTypeLlamaCpp,
			},
		},
		Balancing: types.BalancingSettings{
			Algorithm:            types.AlgorithmResourceAware,
			ModelAffinity:        true,
			SessionStickiness:    true,
			FirstByteTimeout:     2, // короткий таймаут (см. профиль ниже)
			StreamingIdleTimeout: 120,
			RequestTimeout:       120,
			QueueTimeout:         300,
			QueueMaxSize:         100,
			QueueWorkers:         4,
			SessionTTL:           900,
			OperatingMode:        "llama_cpp",
		},
		// R66c (2026-09-22): короткий first-byte timeout задаём ПРОФИЛЕМ модели.
		// По R42 для модели без профиля и без истории генерации действует
		// эвристика по размеру GGUF (EstimateFirstByteTimeoutFromModelSize:
		// >=300s, для неизвестного размера 900s), и она перекрывает глобальное
		// Balancing.FirstByteTimeout — так задумано, чтобы slow prefill не
		// обрывался. Поддерживаемый способ задать короткий таймаут — профиль
		// (Tier 1 в getModelFirstByteTimeout); тест проверяет именно механизм
		// ResponseHeaderTimeout, а не приоритет конфигов.
		LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
			"gemma-4": {FirstByteTimeoutSec: 2},
		},
		Resources: types.ResourceLimits{
			GPU:    types.GPULimits{MaxUsagePercent: 90},
			CPU:    types.CPULimits{MaxUsagePercent: 80},
			Memory: types.MemoryLimits{MaxUsagePercent: 85},
		},
	}

	proxy := balancer.NewProxy(cfg)
	defer proxy.Shutdown(testCtx(t))

	// Помечаем модель загруженной, чтобы избежать auto-load.
	proxy.UpdateMetrics("cpp-gpu", &types.BackendMetrics{
		ID:     "cpp-gpu",
		Status: types.StatusHealthy,
		LlamaCpp: types.LlamaCppMetrics{
			LoadedModels: []types.LlamaCppModel{
				{Name: "gemma-4", State: "loaded"},
			},
		},
	})

	body, _ := json.Marshal(map[string]interface{}{
		"model":    "gemma-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
		"stream":   true,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.100:54322"

	rec := httptest.NewRecorder()
	done := make(chan bool, 1)
	go func() { proxy.ServeHTTP(rec, req); done <- true }()

	select {
	case <-done:
		if rec.Code == http.StatusOK {
			t.Fatalf("expected error status, got 200: %s", rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "timeout") && !strings.Contains(rec.Body.String(), "awaiting") {
			t.Logf("got non-200 status %d: %s", rec.Code, rec.Body.String())
		}
		t.Log("PASS ✅ header timeout enforced")
	case <-time.After(10 * time.Second):
		t.Fatal("request did not fail within 10s despite 2s header timeout")
	}
}

// TestOpenAIChat_ProfileTimeoutIgnoredByDefault — R91 (2026-10-09): профильный
// FirstByteTimeoutSec сам по себе НЕ обрывает запрос.
//
// Доктрина R83/v62: таймаут — рубильник оператора (LB_ALLOW_PROFILE_TIMEOUTS=1,
// LB_REQUEST_TIMEOUT_SEC, LB_STREAMING_IDLE_TIMEOUT_SEC), а значение из профиля
// модели по умолчанию игнорируется — иначе оператор не понимает, кто оборвал
// генерацию (живой случай: запрос падал ровно на 300-й секунде из-за значения в
// профиле, которого в логах не было видно).
//
// Тест держит зависший upstream дольше профильного таймаута (2 с) и проверяет,
// что балансер по-прежнему ждёт — и завершается только по отмене клиента.
func TestOpenAIChat_ProfileTimeoutIgnoredByDefault(t *testing.T) {
	upstream := startHungServer()
	defer func() {
		upstream.Listener.Close()
		upstream.CloseClientConnections()
	}()

	host, port := hostPort(upstream.URL)
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host: "0.0.0.0", Port: 18080, APIPort: 18081,
		},
		Backends: []types.Backend{
			{
				ID: "cpp-gpu", Host: host,
				CppWorkerPort: port, Weight: 1,
				MaxConcurrentReqs: 10,
				Status:            types.StatusHealthy,
				Type:              types.BackendTypeLlamaCpp,
			},
		},
		Balancing: types.BalancingSettings{
			Algorithm:            types.AlgorithmResourceAware,
			ModelAffinity:        true,
			SessionStickiness:    true,
			FirstByteTimeout:     2,
			StreamingIdleTimeout: 120,
			RequestTimeout:       120,
			QueueTimeout:         300,
			QueueMaxSize:         100,
			QueueWorkers:         4,
			SessionTTL:           900,
			OperatingMode:        "llama_cpp",
		},
		LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
			"gemma-4": {FirstByteTimeoutSec: 2},
		},
		Resources: types.ResourceLimits{
			GPU:    types.GPULimits{MaxUsagePercent: 90},
			CPU:    types.CPULimits{MaxUsagePercent: 80},
			Memory: types.MemoryLimits{MaxUsagePercent: 85},
		},
	}

	proxy := balancer.NewProxy(cfg)
	defer proxy.Shutdown(testCtx(t))

	proxy.UpdateMetrics("cpp-gpu", &types.BackendMetrics{
		ID:     "cpp-gpu",
		Status: types.StatusHealthy,
		LlamaCpp: types.LlamaCppMetrics{
			LoadedModels: []types.LlamaCppModel{{Name: "gemma-4", State: "loaded"}},
		},
	})

	body, _ := json.Marshal(map[string]interface{}{
		"model":    "gemma-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
		"stream":   true,
	})
	ctx, cancel := context.WithCancel(testCtx(t))
	defer cancel()
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.101:54323"

	rec := httptest.NewRecorder()
	done := make(chan bool, 1)
	go func() { proxy.ServeHTTP(rec, req); done <- true }()

	// Профильный таймаут = 2 с; ждём заметно дольше и требуем, чтобы запрос
	// продолжал ждать (то есть НЕ был оборван профилем).
	select {
	case <-done:
		t.Fatalf("запрос завершился за 6 с при профильном таймауте 2 с — профиль "+
			"перестал игнорироваться по умолчанию (status=%d body=%s)", rec.Code, rec.Body.String())
	case <-time.After(6 * time.Second):
	}

	// Отмена КЛИЕНТА обязана завершить запрос: без этого «нет таймаута» = утечка.
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("запрос не завершился после отмены клиента")
	}
}

// startSlowSSEServer запускает реальный TCP HTTP-сервер, который сразу шлёт
// заголовки SSE, затем через firstTokenDelay отправляет data-чанки.
// Также отвечает на /api/models, чтобы ensureModelLoadedOnBackend не завис.
func startSlowSSEServer(firstTokenDelay time.Duration, chunks []string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models" {
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"count": 1,
				"models": []map[string]interface{}{
					{
						"name":  "gemma-4-E4B-it-Q4_K_M",
						"state": "loaded",
					},
				},
			})
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		flusher.Flush()

		// keepalive, пока готовим первый токен
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		timer := time.NewTimer(firstTokenDelay)
		defer timer.Stop()

	keepalive:
		for {
			select {
			case <-timer.C:
				break keepalive
			case <-ticker.C:
				fmt.Fprintf(w, ": keepalive\n\n")
				flusher.Flush()
			case <-r.Context().Done():
				// клиент отвалился (defer закрыл listener/соединения) —
				// не ждём оставшиеся 50s firstTokenDelay, выходим сразу.
				return
			}
		}

		for _, chunk := range chunks {
			if chunk == "[DONE]" {
				fmt.Fprintf(w, "data: [DONE]\n\n")
			} else {
				payload, _ := json.Marshal(map[string]interface{}{
					"id":      "chatcmpl-test",
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   "gemma-4",
					"choices": []map[string]interface{}{
						{
							"index": 0,
							"delta": map[string]string{"role": "assistant", "content": chunk},
						},
					},
				})
				fmt.Fprintf(w, "data: %s\n\n", payload)
			}
			flusher.Flush()
		}
	}))
}

// writeJSON — helper для ответа JSON в тестовом сервере.
func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

// startHungServer не шлёт заголовки на /v1/chat/completions — проверяет
// ResponseHeaderTimeout. На /api/models отвечает, чтобы auto-load не завис.
func startHungServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models" {
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"count": 1,
				"models": []map[string]interface{}{
					{
						"name":  "gemma-4",
						"state": "loaded",
					},
				},
			})
			return
		}

		// Зависаем, ничего не отвечая.
		select {
		case <-r.Context().Done():
			return
		case <-time.After(300 * time.Second):
			return
		}
	}))
}

func hostPort(raw string) (string, int) {
	s := strings.TrimPrefix(strings.TrimPrefix(raw, "http://"), "https://")
	hp := strings.Split(s, ":")
	if len(hp) == 2 {
		var p int
		if _, err := fmt.Sscanf(hp[1], "%d", &p); err == nil {
			return hp[0], p
		}
	}
	return s, 0
}

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}
