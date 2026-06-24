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
//   OpenWebUI с tools получал "Server Connection Error" после успешного
//   tool-call, потому что proxyRequestLlamaCpp не делал auto-reload при 413.
//
// Сценарий:
//   1. Mock cppworker возвращает HTTP 413 + bridge_info (code=3, current=8196,
//      required=16384, max_vram=21952).
//   2. proxyRequestLlamaCpp должен:
//      - распарсить ошибку через ParseCppWorkerError
//      - errors.Is(err, ErrPromptTooLong) == true
//      - вызвать handleNCtxReload → DecideReloadBackend → POST /api/models/reload
//      - после успешного reload повторить запрос к cppworker
//   3. Mock cppworker на второй запрос возвращает 200 OK с реальным стримом.
//   4. Клиент (OpenWebUI) получает нормальный стрим, а не "Server Connection Error".
func TestProxyRequestLlamaCpp_Streaming413_TriggersReload(t *testing.T) {
	mu := &sync.Mutex{}
	mockCppworker := makeUpstreamForStreaming413Test(t, mu)
	defer mockCppworker.Close()

	// ==== Поднимаем Proxy + LlamaCppRouter с одним бэкендом ====
	p, _, cleanup := makeTestLlamaProxy(t, mockCppworker.URL)
	defer cleanup()

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

	// ==== Запускаем прокси (через LlamaCppRouter.handleOpenAIChatCompletions,
	//      который внутри вызовет proxyRequestOpenAIStreaming / proxyRequestLlamaCpp) ====
	// Для простоты тестируем прямо proxyRequestLlamaCpp, т.к. router требует
	// гораздо больше setup (proxy.backends map).
	bodyBuf := bodyBytes
	err := p.proxyRequestLlamaCpp(rec, req, "llama_test", bodyBuf)
	if err != nil {
		t.Fatalf("proxyRequestLlamaCpp failed: %v", err)
	}

	// ==== Верификация ====
	respBody := rec.Body.String()

	// Главная проверка: balancer НЕ должен пробрасывать SSE error chunk с "upstream returned HTTP 413".
	if strings.Contains(respBody, `"error":"upstream returned HTTP 413"`) {
		t.Errorf("REGRESSION: balancer propagated SSE error instead of triggering auto-reload!\nbody: %s", respBody)
	}

	// Клиент должен получить успешный streaming-ответ после reload.
	if !strings.Contains(respBody, "Reloaded and OK!") {
		t.Errorf("expected successful streaming content after reload, got body: %s", respBody)
	}
	if !strings.Contains(respBody, "[DONE]") {
		t.Errorf("expected [DONE] marker in streaming response, got: %s", respBody)
	}
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

	// Sanity: DecideReloadBackend должен вернуть DecisionReload
	// (required=16384 < max_vram*0.85=18659).
	coord := NewNCtxReloadCoordinator(DefaultNCtxReloadConfig())
	plan := coord.DecideReloadBackend("cppworker-gpu", bi, 0)
	if plan == nil || plan.Decision != DecisionReload {
		t.Errorf("DecideReloadBackend should return DecisionReload, got plan=%+v", plan)
	}
	if plan.NewNCtx != 16384 {
		t.Errorf("NewNCtx = %d, want 16384", plan.NewNCtx)
	}
	t.Logf("OK: BridgeInfo parsed correctly → DecideReloadBackend → Reload to n_ctx=%d", plan.NewNCtx)
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