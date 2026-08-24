//go:build llama_stub

package balancer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// makeMockCppWorkerWithReload — создаёт мок cppworker, который:
//  1. На /api/models возвращает модель с указанным currentNCtx
//  2. На /api/models/reload записывает новый n_ctx и возвращает success
//  3. На /v1/chat/completions возвращает фиксированный ответ
func makeMockCppWorkerWithReload(t *testing.T, modelName string, initialNCtx int) (*httptest.Server, *atomic.Int64, *[]string) {
	t.Helper()
	var currentNCtx atomic.Int64
	currentNCtx.Store(int64(initialNCtx))
	var muReloadCalls sync.Mutex
	var reloadCalls []string

	mux := http.NewServeMux()

	mux.HandleFunc("/api/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"models": []map[string]interface{}{},
			"data": []map[string]interface{}{},
			"loaded_models": []map[string]interface{}{
				{
					"id":            modelName,
					"name":          modelName,
					"state":         "loaded",
					"contextLength": int(currentNCtx.Load()),
				},
			},
		})
	})

	mux.HandleFunc("/api/models/reload", func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		newNCtx, _ := payload["contextSize"].(float64)
		currentNCtx.Store(int64(newNCtx))
		muReloadCalls.Lock()
		reloadCalls = append(reloadCalls, fmt.Sprintf("%v", payload))
		muReloadCalls.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status":      "reloaded",
			"context_size": int(newNCtx),
		})
	})

	// Round 35+ (2026-08-12): executeAsyncReload uses /api/models/load instead
	// of /api/models/reload (load works for both unloaded and loaded cases).
	// Mock handler для совместимости с новым кодом.
	mux.HandleFunc("/api/models/load", func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		newNCtx, _ := payload["contextSize"].(float64)
		if newNCtx > 0 {
			currentNCtx.Store(int64(newNCtx))
		}
		muReloadCalls.Lock()
		reloadCalls = append(reloadCalls, fmt.Sprintf("%v", payload))
		muReloadCalls.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status":      "loading",
			"context_size": int(newNCtx),
			"progress_url": "/api/models/load/progress?name=" + modelName,
		})
	})

	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		// Эмитим 2 чанка + [DONE].
		chunk := map[string]interface{}{
			"id":      "chatcmpl-test",
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   modelName,
			"choices": []map[string]interface{}{
				{"index": 0, "delta": map[string]string{"role": "assistant", "content": ""}, "finish_reason": nil},
			},
		}
		data, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", data)
		if flusher != nil {
			flusher.Flush()
		}
		// Final chunk
		final := map[string]interface{}{
			"id":      "chatcmpl-test",
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   modelName,
			"choices": []map[string]interface{}{
				{"index": 0, "delta": map[string]string{"role": "assistant", "content": "Hello from cppworker"}, "finish_reason": "stop"},
			},
		}
		data, _ = json.Marshal(final)
		fmt.Fprintf(w, "data: %s\n\n", data)
		fmt.Fprintf(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(func() {
		srv.Close()
	})
	return srv, &currentNCtx, &reloadCalls
}

// buildProxyWithCppWorkerBackend создаёт минимальный Proxy с одним cppworker backend.
func buildProxyWithCppWorkerBackend(t *testing.T, cppWorkerURL string, modelName string) *Proxy {
	t.Helper()
	cfg := &types.LoadBalancerConfig{}
	p := newProxyWithCleanup(t, cfg)
	// Используем internal API для добавления backend.
	hostPort := strings.TrimPrefix(cppWorkerURL, "http://")
	parts := strings.Split(hostPort, ":")
	host := parts[0]
	port := 80
	if len(parts) > 1 {
		fmt.Sscanf(parts[1], "%d", &port)
	}
	p.backends["test-backend"] = &BackendState{
		Backend: &types.Backend{
			ID:            "test-backend",
			Type:          types.BackendTypeLlamaCpp,
			Host:          host,
			CppWorkerPort: port,
			OllamaPort:    port,
		},
	}
	// Заполняем llamaMetrics чтобы preflight видел loaded n_ctx.
	// Конкретное значение ContextLength выставит buildProxyWithMockInitialCtx,
	// который использует начальное значение из makeMockCppWorkerWithReload.
	if p.metricsMgr == nil {
		p.metricsMgr = NewMetricsManager()
	}
	p.metricsMgr.mu.Lock()
	p.metricsMgr.llamaMetrics["test-backend"] = &types.LlamaCppMetrics{}
	p.metricsMgr.mu.Unlock()
	return p
}

// buildProxyWithMockInitialCtx — обёртка над buildProxyWithCppWorkerBackend,
// которая устанавливает initial n_ctx (как у cppWorker) в llamaMetrics кэш.
func buildProxyWithMockInitialCtx(t *testing.T, cppWorkerURL, modelName string, initialNCtx int) *Proxy {
	p := buildProxyWithCppWorkerBackend(t, cppWorkerURL, modelName)
	p.metricsMgr.mu.Lock()
	p.metricsMgr.llamaMetrics["test-backend"] = &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{
			{Name: modelName, State: "loaded", ContextLength: initialNCtx},
		},
	}
	p.metricsMgr.mu.Unlock()
	return p
}

// TestPreflightNCtxReload_LoadedLessThanRequested — главный тест НОВОЙ асинхронной семантики:
// если клиент шлёт options.num_ctx=16384, а loaded=4096 —
// preflight АСИНХРОННО запускает reload на 16384 и СРАЗУ возвращает клиенту
// HTTP 503 + Retry-After: 5 (а не блокирует стрим на 30 секунд).
//
// ВАЖНО (2026-06-22): тест полностью переписан под новую неблокирующую логику.
// Старая проверяла синхронный reload (ok=true после успешного reload),
// новая — что клиент сразу получает 503 и reload уходит в фон.
func TestPreflightNCtxReload_LoadedLessThanRequested(t *testing.T) {
	modelName := "test-model"
	cppWorker, currentNCtx, reloadCalls := makeMockCppWorkerWithReload(t, modelName, 4096)
	p := buildProxyWithMockInitialCtx(t, cppWorker.URL, modelName, 4096)

	body := []byte(`{"model":"` + modelName + `","messages":[{"role":"user","content":"test"}],"options":{"num_ctx":16384}}`)

	newBody, ok, msg, status := p.preflightNCtxReloadIfNeeded(nil, "test-backend", modelName, body, "/api/chat")

	// Round 23 (2026-08-04) FIX: SMART-SKIP RELOAD.
	// Для prompt "test" (1 токен) + default n_predict=2048:
	//   required = 1 + 2048 + 1 + slack ≈ 2050
	//   loaded = 4096
	//   2050 < 4096 → smart-skip (NO reload, body patched down to 4096)
	if !ok {
		t.Fatalf("preflight SHOULD succeed (smart-skip reload: prompt fits in current n_ctx); msg=%q status=%d", msg, status)
	}
	if status != http.StatusOK {
		t.Errorf("preflight status=%d, want 200 (smart-skip)", status)
	}
	// Body должен быть patched: options.num_ctx=16384 → 4096 (downgrade).
	if bytes.Equal(newBody, body) {
		t.Errorf("smart-skip SHOULD modify body (num_ctx should be downgraded from 16384 to 4096)")
	}
	// Проверяем что num_ctx в patched body = 4096.
	if nctx := ExtractNumCtxFromBody(newBody); nctx != 4096 {
		t.Errorf("patched body num_ctx=%d, want 4096", nctx)
	}
	// Reload НЕ должен был вызваться.
	time.Sleep(200 * time.Millisecond)
	if got := currentNCtx.Load(); got != 4096 {
		t.Errorf("currentNCtx=%d, want 4096 (reload should NOT fire)", got)
	}
	if len(*reloadCalls) != 0 {
		t.Errorf("expected 0 reload calls (smart-skip), got %d", len(*reloadCalls))
	}

	// Round 23 (2026-08-04) FIX: SMART-SKIP — кэш НЕ обновляется (reload не было).
	// Раньше: ожидалось что cache обновляется до 16384 (чтобы предотвратить retry-цикл).
	// Теперь: smart-skip не трогает cache — модель остаётся на 4096, request проксируется
	// с patched body (num_ctx=4096). На следующем запросе с тем же num_ctx=16384 —
	// опять smart-skip. Никакого reload нет.
	p.metricsMgr.mu.RLock()
	lm, hasLm := p.metricsMgr.llamaMetrics["test-backend"]
	p.metricsMgr.mu.RUnlock()
	if !hasLm || lm == nil {
		t.Fatalf("llamaMetrics cache missing for test-backend")
	}
	for _, m := range lm.LoadedModels {
		if strings.Contains(m.Name, modelName) {
			if m.ContextLength != 4096 {
				t.Errorf("llamaMetrics LoadedModels ContextLength=%d, want 4096 (smart-skip — no reload)", m.ContextLength)
			}
		}
	}
}

// TestPreflightNCtxReload_LoadedAlreadyEnough — если loaded >= requested,
// reload НЕ должен вызываться.
func TestPreflightNCtxReload_LoadedAlreadyEnough(t *testing.T) {
	modelName := "test-model"
	cppWorker, currentNCtx, reloadCalls := makeMockCppWorkerWithReload(t, modelName, 16384)
	// loaded n_ctx = 16384 (как у mock-cppworker), requested = 8192 — reload не нужен.
	p := buildProxyWithMockInitialCtx(t, cppWorker.URL, modelName, 16384)

	body := []byte(`{"model":"` + modelName + `","options":{"num_ctx":8192}}`)
	_, ok, msg, _ := p.preflightNCtxReloadIfNeeded(nil, "test-backend", modelName, body, "/api/chat")

	if !ok {
		t.Fatalf("preflight should succeed (no reload needed); msg=%q", msg)
	}
	if got := currentNCtx.Load(); got != 16384 {
		t.Errorf("currentNCtx should not change; got %d, want 16384", got)
	}
	if len(*reloadCalls) != 0 {
		t.Errorf("expected 0 reload calls, got %d", len(*reloadCalls))
	}
}

// TestPreflightNCtxReload_NoNumCtxInBody — если клиент не задал num_ctx,
// preflight должен быть no-op.
func TestPreflightNCtxReload_NoNumCtxInBody(t *testing.T) {
	modelName := "test-model"
	cppWorker, currentNCtx, reloadCalls := makeMockCppWorkerWithReload(t, modelName, 4096)
	p := buildProxyWithMockInitialCtx(t, cppWorker.URL, modelName, 4096)

	body := []byte(`{"model":"` + modelName + `","messages":[{"role":"user","content":"test"}]}`)
	_, ok, msg, _ := p.preflightNCtxReloadIfNeeded(nil, "test-backend", modelName, body, "/api/chat")

	if !ok {
		t.Fatalf("preflight should succeed (no num_ctx in body); msg=%q", msg)
	}
	if got := currentNCtx.Load(); got != 4096 {
		t.Errorf("currentNCtx should not change; got %d, want 4096", got)
	}
	if len(*reloadCalls) != 0 {
		t.Errorf("expected 0 reload calls, got %d", len(*reloadCalls))
	}
}

// TestPreflightNCtxReload_ReloadFailureFallsBackToProxyAsIs — если reload endpoint
// возвращает 500, preflight ЗАПУСКАЕТ async reload в фоне (а не проксирует as-is,
// потому что cppworker всё равно вернёт 400 при n_ctx overflow).
// Клиент получает 503 + Retry-After, как и при успешном async reload.
//
// ВАЖНО (2026-06-22): тест обновлён под новую асинхронную логику. Раньше
// при failed reload preflight возвращал ok=true (proxy-as-is fallback), что приводило
// к 400-ошибке от cppworker и partial response. Теперь при loaded < requested мы
// ВСЕГДА сигнализируем клиенту о reload (503), даже если reload упал.
func TestPreflightNCtxReload_ReloadFailureFallsBackToProxyAsIs(t *testing.T) {
	modelName := "test-model"
	mux := http.NewServeMux()
	mux.HandleFunc("/api/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"loaded_models":[{"name":"` + modelName + `","state":"loaded","contextLength":4096}]}`))
	})
	mux.HandleFunc("/api/models/reload", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"reload failed"}`))
	})
	cppWorker := httptest.NewServer(mux)
	defer cppWorker.Close()

	p := buildProxyWithMockInitialCtx(t, cppWorker.URL, modelName, 4096)
	// Round 23 (2026-08-04): чтобы сработал reload (а не smart-skip), body должен
	// содержать prompt который НЕ влезает в 4096. Используем большой prompt +
	// num_predict=4096 → required ~ 2048+4096+slack > 4096.
	bigPrompt := strings.Repeat("a", 8192) // 8KB → ~2048 tokens
	body := []byte(`{"model":"` + modelName + `","messages":[{"role":"user","content":"` + bigPrompt + `"}],"options":{"num_ctx":16384,"num_predict":4096}}`)

	newBody, ok, _, _ := p.preflightNCtxReloadIfNeeded(nil, "test-backend", modelName, body, "/api/chat")

	// При failed reload мы всё равно хотим, чтобы клиент повторил через 30 секунд —
	// иначе cppworker вернёт 400 и клиент получит partial response.
	if ok {
		t.Fatalf("preflight should signal reload needed even on reload failure (5xx); 503 for client retry")
	}
	if !bytes.Equal(newBody, body) {
		t.Errorf("preflight should NOT modify body even on reload failure (async retry path)")
	}

	// Даём фоновой горутине время упасть с 500 и залогировать.
	time.Sleep(500 * time.Millisecond)
}

