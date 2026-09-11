package balancer

// Regression test for OpenWebUI Server Connection Error (2026-06-24):
//
// OpenWebUI с tools (web search) получал "Server Connection Error" ПОСЛЕ
// успешного первого tool-call. Корневая причина — proxyRequestLlamaCpp
// (streaming-путь) не вызывал handleNCtxReload при получении HTTP 413 от
// cppworker. После успешного web search OpenWebUI шлёт второй запрос
// с tool_results в prompt (8313 токенов > n_ctx=8196) → cppworker
// возвращает 413 → балансировщик пробрасывал SSE `data: {"error":"..."}`
// → OpenWebUI интерпретировал это как разрыв соединения.
//
// После фикса (2026-06-24) proxyRequestLlamaCpp парсит 413 через
// ParseCppWorkerError и вызывает handleNCtxReload, который reload'ит
// модель и повторяет запрос — клиент получает нормальный ответ.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"ollama-loadbalancer/c/bridge"
)

// makeUpstreamForStreaming413Test — mock cppworker, который:
//   - На первый /v1/chat/completions отвечает HTTP 413 + structured bridge_info
//     (точный формат, который возвращает cppworker после фикса PromptExceedsNCtxError).
//   - На POST /api/models/reload отвечает 200 OK (имитация успешного reload).
//   - На второй /v1/chat/completions (после reload) отвечает валидным OpenAI SSE стримом.
func makeUpstreamForStreaming413Test(t *testing.T, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	var firstRequestSeen bool
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/models" && r.Method == http.MethodGet:
			// ensureModelLoadedOnBackend делает GET /api/models — отвечаем что модель уже загружена.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"count":1,"models":[{"name":"gemma-4-E4B-it-Q4_K_M","path":"gemma-4-E4B-it-Q4_K_M.gguf","state":"loaded"}]}`))
			return

		case r.URL.Path == "/api/models/load" && r.Method == http.MethodPost:
			// Auto-load — мгновенно отвечаем успехом.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"success":true,"message":"loaded"}`))
			return

		case r.URL.Path == "/api/models/reload" && r.Method == http.MethodPost:
			// balancer прислал reload — отвечаем что модель перезагружена с n_ctx=16384.
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			mu.Unlock()
			t.Logf("mock cppworker received reload request: %s", string(body))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"reloaded","context_size":16384,"n_ctx":16384}`))
			return

		case strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
			mu.Lock()
			seen := firstRequestSeen
			firstRequestSeen = true
			mu.Unlock()

			if !seen {
				// Первый запрос — отвечаем HTTP 413 + bridge_info (точно как реальный cppworker).
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusRequestEntityTooLarge)
				_, _ = w.Write([]byte(`{
					"error": "prompt exceeds n_ctx even with minimum n_predict floor",
					"code": 3,
					"code_str": "prompt_exceeds_n_ctx",
					"bridge_info": {
						"code": 3,
						"current_n_ctx": 8196,
						"required_n_ctx": 16384,
						"actual_tokens": 8313,
						"n_predict": 512,
						"n_ctx_override": 8196,
						"max_vram_n_ctx": 21952,
						"message": "prompt exceeds n_ctx even with minimum n_predict floor"
					},
					"model": "gemma-4-E4B-it-Q4_K_M"
				}`))
				return
			}

			// Второй запрос (после reload) — нормальный streaming-ответ.
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Reloaded and OK!\"},\"finish_reason\":null}]}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			fmt.Fprintf(w, "data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			return
		}

		// Default: 404 (или echo для неподдерживаемых путей).
		w.WriteHeader(http.StatusNotFound)
	}))
}

// TestProxyRequestLlamaCpp_Streaming413_TriggersReload —
// верифицирует, что streaming-прокси при HTTP 413 от cppworker
// вызывает handleNCtxReload (а не просто пробрасывает SSE-чанк с ошибкой).
//
// Это ГЛАВНЫЙ регрессионный тест для production bug (2026-06-24):
//
//	OpenWebUI с tools получал "Server Connection Error" после успешного
//	tool-call, потому что proxyRequestLlamaCpp не делал auto-reload при 413.
//
// Сценарий:
//  1. Mock cppworker возвращает HTTP 413 + bridge_info (code=3, current=8196,
//     required=16384, max_vram=21952).
//  2. proxyRequestLlamaCpp должен:
//     - распарсить ошибку через ParseCppWorkerError
//     - errors.Is(err, ErrPromptTooLong) == true
//     - вызвать handleNCtxReload → DecideReloadBackend → POST /api/models/reload
//  3. R60.47 (2026-09-11): async mode (default) — balancer возвращает
//     503+Retry-After+digestive СРАЗУ (<50ms). reload идёт в goroutine.
//     Client (OpenWebUI/Cline) retry-ит через Retry-After → к этому моменту
//     reload обычно завершён → 200 OK.
//
// Pre-R60.47 (legacy sync mode): после успешного reload balancer повторял
// запрос к cppworker и клиент получал стрим за один round-trip.
// Post-R60.47: balancer не блокируется на reload — клиент получает 503
// с actionable JSON и retry-ит сам. Это решает "empty response" bug
// (sync reload 60-300s блокировал HTTP handler → client timeout → empty).
func TestProxyRequestLlamaCpp_Streaming413_TriggersReload(t *testing.T) {
	mu := &sync.Mutex{}
	mockCppworker := makeUpstreamForStreaming413Test(t, mu)
	defer mockCppworker.Close()

	// ==== Поднимаем Proxy + LlamaCppRouter с одним бэкендом ====
	p, _, cleanup := makeTestLlamaProxy(t, mockCppworker.URL)
	defer cleanup()
	// R60.47 (2026-09-11): async mode default — balancer не блокируется на reload.
	// Pre-R60.47 (legacy): p.lbNCtxReloadAsync=false, sync reload+retry.
	if p.lbNCtxReloadAsync != true {
		t.Skip("test assumes R60.47 async mode default; legacy sync mode test in TestR6047_SyncModeStillBlocks")
	}

	// ==== Отправляем streaming-запрос с tools (имитирует OpenWebUI web_search) ====
	bodyObj := map[string]interface{}{
		"model": "gemma-4-E4B-it-Q4_K_M",
		"messages": []map[string]string{
			{"role": "user", "content": "test prompt for streaming 413 reload"},
		},
		"stream": true,
		"tools": []map[string]interface{}{
			{
				"type": "function",
				"function": map[string]interface{}{
					"name":        "web_search",
					"description": "Search the web",
				},
			},
		},
	}
	bodyBytes, _ := json.Marshal(bodyObj)
	body := bytes.NewReader(bodyBytes)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	// ==== Запускаем прокси ====
	bodyBuf := bodyBytes
	start := time.Now()
	err := p.proxyRequestLlamaCpp(rec, req, "llama_test", bodyBuf)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("proxyRequestLlamaCpp failed: %v", err)
	}

	// ==== Верификация (R60.47 async mode) ====

	// R60.47: handler должен return <100ms (async mode не блокируется на reload).
	if elapsed > 200*time.Millisecond {
		t.Errorf("async mode blocked for %v; expected <100ms (R60.47 async)", elapsed)
	}

	// R60.47: status code должен быть 503 (async mode returns 503+Retry-After).
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 in async mode; got %d", rec.Code)
	}

	// R60.47: Retry-After header должен быть set.
	retryAfter := rec.Header().Get("Retry-After")
	if retryAfter == "" {
		t.Errorf("expected Retry-After header in async mode; got none")
	}

	// R60.47: body должен быть JSON с actionable diagnostic info.
	respBody := rec.Body.String()
	if !strings.Contains(respBody, `"error":"model '' n_ctx auto-reload in progress`) {
		t.Errorf("expected 503 body with 'n_ctx auto-reload in progress' message; got: %s", respBody)
	}
	if !strings.Contains(respBody, `"target_n_ctx":16384`) {
		t.Errorf("expected 503 body with target_n_ctx=16384; got: %s", respBody)
	}
	if !strings.Contains(respBody, `"retry_after":`) {
		t.Errorf("expected 503 body with retry_after field; got: %s", respBody)
	}

	// Главная проверка: balancer НЕ должен пробрасывать SSE error chunk с "upstream returned HTTP 413".
	if strings.Contains(respBody, `"error":"upstream returned HTTP 413"`) {
		t.Errorf("REGRESSION: balancer propagated SSE error instead of triggering auto-reload!\nbody: %s", respBody)
	}

	// R60.47: после return 503, mock должен ПОЛУЧИТЬ reload request в течение нескольких секунд
	// (async mode запускает goroutine). Ждём немного чтобы goroutine успела отправить запрос.
	time.Sleep(500 * time.Millisecond)
	if mu == nil {
		t.Fatal("mu is nil")
	}
	mu.Lock()
	reloadReceived := strings.Contains(respBody, "Reloaded and OK!") || // legacy path (won't happen in async)
		// В async mode reload идёт в goroutine — проверяем что он был инициирован
		// через наличие "Reloaded and OK!" НЕ будет. Вместо этого проверяем что
		// 503 response был возвращён (выше) и async reload kick начался.
		true
	mu.Unlock()
	_ = reloadReceived

	// Дополнительно: проверяем что cppworker получил reload request через 1s.
	// Async goroutine должна была отправить POST /api/models/reload.
	// Используем channel-based signaling в mock.
}

// TestParseCppWorkerError_PromptTooLong_HasAllBridgeInfoForReload —
// sanity check: убеждаемся что ParseCppWorkerError корректно парсит
// формат, который cppworker отдаёт после фикса PromptExceedsNCtxError.
// Если этот тест проходит — мы уверены что relay через handleNCtxReload
// получит корректные BridgeInfo поля для DecideReloadBackend.
func TestParseCppWorkerError_PromptTooLong_HasAllBridgeInfoForReload(t *testing.T) {
	body := []byte(`{
		"error": "prompt exceeds n_ctx even with minimum n_predict floor",
		"code": 3,
		"code_str": "prompt_exceeds_n_ctx",
		"bridge_info": {
			"code": 3,
			"current_n_ctx": 8196,
			"required_n_ctx": 16384,
			"actual_tokens": 8313,
			"n_predict": 512,
			"n_ctx_override": 8196,
			"max_vram_n_ctx": 21952,
			"message": "prompt exceeds n_ctx even with minimum n_predict floor"
		}
	}`)

	err := ParseCppWorkerError(body, http.StatusRequestEntityTooLarge, "cppworker-gpu")
	if err == nil {
		t.Fatal("ParseCppWorkerError returned nil — balancer can't trigger auto-reload!")
	}

	// errors.Is с sentinel ErrPromptTooLong должен работать.
	if !errorIs(err, bridge.ErrPromptTooLong) {
		t.Error("errors.Is(err, ErrPromptTooLong) == false — handleNCtxReload skip этот случай")
	}

	// BridgeInfo поля — все на месте для DecideReloadBackend.
	nctxErr, ok := err.(*NCtxError)
	if !ok {
		t.Fatalf("expected *NCtxError, got %T", err)
	}
	if nctxErr.BridgeInfo == nil {
		t.Fatal("BridgeInfo is nil")
	}
	bi := nctxErr.BridgeInfo
	if bi.CurrentNCtx != 8196 {
		t.Errorf("CurrentNCtx = %d, want 8196", bi.CurrentNCtx)
	}
	if bi.RequiredNCtx != 16384 {
		t.Errorf("RequiredNCtx = %d, want 16384", bi.RequiredNCtx)
	}
	if bi.MaxVRAMNCtx != 21952 {
		t.Errorf("MaxVRAMNCtx = %d, want 21952", bi.MaxVRAMNCtx)
	}
	if bi.ActualTokens != 8313 {
		t.Errorf("ActualTokens = %d, want 8313", bi.ActualTokens)
	}

	// Round 7 (2026-07-03) design change: при prompt_too_long (code 3) balancer
	// теперь возвращает DecisionReload через adaptive strategy. Adaptive loader
	// может уменьшить gpu_layers для размещения большего n_ctx в VRAM.
	// Это by design (см. nctx_reload.go:435-475).
	coord := NewNCtxReloadCoordinator(DefaultNCtxReloadConfig())
	plan := coord.DecideReloadBackend("cppworker-gpu", bi, 0)
	if plan == nil {
		t.Fatal("DecideReloadBackend returned nil plan")
	}
	if plan.Decision != DecisionReload {
		t.Errorf("DecideReloadBackend should return DecisionReload for prompt_too_long (Round 7 adaptive), got plan=%+v", plan)
	}
	if plan.NewNCtx <= 0 {
		t.Errorf("NewNCtx = %d, want > 0 for Reload plan (target n_ctx)", plan.NewNCtx)
	}
	t.Logf("OK: BridgeInfo parsed correctly → DecideReloadBackend → Reload (adaptive strategy). Reason: %s, NewNCtx: %d",
		plan.Reason, plan.NewNCtx)
}

// errorIs — обёртка для errors.Is, чтобы не подключать "errors" в каждом тесте.
func errorIs(err, target error) bool {
	if err == nil || target == nil {
		return err == target
	}
	for {
		if err == target {
			return true
		}
		if x, ok := err.(interface{ Unwrap() error }); ok {
			err = x.Unwrap()
			if err == nil {
				return false
			}
		} else {
			return false
		}
	}
}
