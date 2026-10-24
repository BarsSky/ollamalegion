// llamacpp_router_openai_model_byid_test.go — H7 (остаток аудита):
// GET /v1/models/{id} не обслуживался балансером.
//
// В internal/balancer/llamacpp_router.go в switch был только точный
// case "/v1/models", поэтому /v1/models/<id> не матчился: Route возвращал
// false, запрос уходил в общий proxy-flow (тело воркера), а на Ollama-only
// кластере мог встать в admission-очередь.
//
// Контракт, который держат эти тесты (OpenAI /v1/models/{id}):
//   - известная модель → 200 {"id","object":"model","created","owned_by":"ollamalegion"};
//   - неизвестная      → 404 {"error":{"message",...,"type":"invalid_request_error",
//     "code":"model_not_found"}};
//   - вложенный путь /v1/models/a/b → 400 (это не id модели);
//   - путь read-only: isReadOnlyOrMgmtEndpoint("/v1/models/x") == true,
//     иначе он снова займёт inference-слот.
package balancer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestH7_OpenAIModelByID_KnownModelReturns200 — /v1/models/model-A обязан
// обслуживаться самим балансером (Route → true) и отдавать OpenAI-объект модели.
func TestH7_OpenAIModelByID_KnownModelReturns200(t *testing.T) {
	proxy, _ := buildProxyWithStub(t,
		[]stubFileEntry{
			{Name: "model-A.gguf", SizeBytes: 1_000_000_000, ModifiedAt: "2026-08-01T10:00:00Z"},
		},
		nil,
	)

	req := httptest.NewRequest(http.MethodGet, "/v1/models/model-A", nil)
	w := httptest.NewRecorder()

	if handled := proxy.llamaCppRouter.Route(w, req); !handled {
		t.Fatalf("Route(GET /v1/models/model-A) = false, want true: путь уходит в "+
			"proxy-flow вместо handler'а (H7); body=%s", w.Body.String())
	}
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v1/models/model-A → HTTP %d, want 200; body=%s", w.Code, w.Body.String())
	}

	var resp struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("GET /v1/models/model-A: bad JSON: %v; body=%s", err, w.Body.String())
	}
	if resp.ID != "model-A" {
		t.Errorf("id = %q, want %q", resp.ID, "model-A")
	}
	if resp.Object != "model" {
		t.Errorf("object = %q, want %q", resp.Object, "model")
	}
	if resp.OwnedBy != "ollamalegion" {
		t.Errorf("owned_by = %q, want %q", resp.OwnedBy, "ollamalegion")
	}
	if resp.Created <= 0 {
		t.Errorf("created = %d, want unix timestamp > 0", resp.Created)
	}
}

// TestH7_OpenAIModelByID_UnknownModelReturns404OpenAIError — неизвестный id:
// 404 в форме OpenAI (клиенты читают error.code == "model_not_found").
func TestH7_OpenAIModelByID_UnknownModelReturns404OpenAIError(t *testing.T) {
	proxy, _ := buildProxyWithStub(t,
		[]stubFileEntry{
			{Name: "model-A.gguf", SizeBytes: 1_000_000_000, ModifiedAt: "2026-08-01T10:00:00Z"},
		},
		nil,
	)

	req := httptest.NewRequest(http.MethodGet, "/v1/models/no-such-model", nil)
	w := httptest.NewRecorder()

	if handled := proxy.llamaCppRouter.Route(w, req); !handled {
		t.Fatalf("Route(GET /v1/models/no-such-model) = false, want true (H7)")
	}
	if w.Code != http.StatusNotFound {
		t.Fatalf("GET /v1/models/no-such-model → HTTP %d, want 404; body=%s", w.Code, w.Body.String())
	}

	var resp struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("404 body не JSON-объект OpenAI: %v; body=%s", err, w.Body.String())
	}
	if resp.Error.Code != "model_not_found" {
		t.Errorf("error.code = %q, want %q; body=%s", resp.Error.Code, "model_not_found", w.Body.String())
	}
	if resp.Error.Type != "invalid_request_error" {
		t.Errorf("error.type = %q, want %q", resp.Error.Type, "invalid_request_error")
	}
	if resp.Error.Message == "" {
		t.Errorf("error.message пуст; body=%s", w.Body.String())
	}
}

// TestH7_OpenAIModelByID_NestedPathReturns400 — /v1/models/a/b это НЕ id модели
// (id не содержит '/'), отвечаем 400, а не 404 и не проксируем.
func TestH7_OpenAIModelByID_NestedPathReturns400(t *testing.T) {
	proxy, _ := buildProxyWithStub(t, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/models/a/b", nil)
	w := httptest.NewRecorder()

	if handled := proxy.llamaCppRouter.Route(w, req); !handled {
		t.Fatalf("Route(GET /v1/models/a/b) = false, want true (H7)")
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("GET /v1/models/a/b → HTTP %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestH7_OpenAIModelByID_IsReadOnly — путь by-id read-only: не должен занимать
// inference-слот (иначе снова уедет в admission-очередь как в H7).
func TestH7_OpenAIModelByID_IsReadOnly(t *testing.T) {
	if !isReadOnlyOrMgmtEndpoint("/v1/models/x") {
		t.Errorf("isReadOnlyOrMgmtEndpoint(%q) = false, want true: путь by-id "+
			"займёт inference-слот вместо мгновенного ответа", "/v1/models/x")
	}
	if !isReadOnlyOrMgmtEndpoint("/v1/models/") {
		t.Errorf("isReadOnlyOrMgmtEndpoint(%q) = false, want true", "/v1/models/")
	}
	// Граница: inference-пути остаются inference.
	if isReadOnlyOrMgmtEndpoint("/v1/chat/completions") {
		t.Errorf("isReadOnlyOrMgmtEndpoint(%q) = true, want false", "/v1/chat/completions")
	}
}
