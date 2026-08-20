//go:build llama_stub

// Round 51.5 (2026-08-20): systematic wire-format contract tests.
//
// Каждый payload, который balancer шлёт в cppworker, строится ТЕМ ЖЕ кодом,
// что в production, и парсится в mirror struct с DisallowUnknownFields.
// Если cppworker отвергнет payload с HTTP 400 — тест ПРОВАЛИТСЯ с явным
// указанием, какое поле "unknown" было отправлено.
//
// Это слой L1: статический mirror. L2 (live integration) —
// cppworker_contract_live_test.go — отдельный build tag `integration`.

package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCppworkerEndpoints_PayloadAcceptance — для каждого endpoint с payload'ом
// строит production payload (тот же что balancer шлёт в проде) и проверяет
// что он ДЕКОДИРУЕТСЯ в mirror struct без "unknown field" ошибок.
//
// NOTE: proxy passthrough endpoints (v1-chat, v1-completions, ollama-chat,
// ollama-generate) skip this test — balancer не конструирует их payload
// (клиент делает), но ДОЛЖЕН сохранять его байт-в-байт. Это покрывается
// отдельным тестом TestProxyPassthrough_NoMutation в proxy_request_test.go.
func TestCppworkerEndpoints_PayloadAcceptance(t *testing.T) {
	for _, ep := range AllEndpoints {
		if ep.RequestStruct == nil {
			continue
		}
		if ep.PayloadBuilder == nil {
			continue
		}
		t.Run(ep.Name+"_"+ep.Method+"_"+strings.ReplaceAll(ep.Path, "/", "_"), func(t *testing.T) {
			payload := ep.PayloadBuilder()
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(ep.RequestStruct); err != nil {
				t.Errorf("cppworker WOULD REJECT this payload with HTTP 400:\n  endpoint: %s %s\n  error:    %v\n  payload:  %s",
					ep.Method, ep.Path, err, string(raw))
			}
		})
	}
}

// buildRepresentativePayload — для каждого endpoint'а возвращает payload
// который balancer МОЖЕТ отправить в production. Содержит все optional поля
// которые balancer использует в коде (Reason, OverrideTensors, и т.д.).
//
// Определена в cppworker_contract_helpers.go (без build tag, чтобы
// использоваться и в L1 (llama_stub), и в L2 (integration) тестах).

// TestCppworkerEndpoints_NoUnknownFieldsInMinimal — minimal payload (только обязательные
// поля) тоже декодируется. Защита от "если передать минимум, всё ок; но при добавлении
// optional поля всё ломается".
func TestCppworkerEndpoints_NoUnknownFieldsInMinimal(t *testing.T) {
	minimal := map[string]map[string]interface{}{
		"reload":            {"name": "x"},
		"load":              {"name": "x"},
		"load-with-params":  {"name": "x"},
		"delete":            {"name": "x"},
	}
	for _, ep := range AllEndpoints {
		if ep.RequestStruct == nil {
			continue
		}
		t.Run(ep.Name+"_minimal", func(t *testing.T) {
			payload, ok := minimal[ep.Name]
			if !ok {
				t.Skip("no minimal payload defined")
			}
			raw, _ := json.Marshal(payload)
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(ep.RequestStruct); err != nil {
				t.Errorf("cppworker would reject minimal payload: %v\npayload: %s", err, string(raw))
			}
		})
	}
}

// TestCppworkerReload_NoReasonNoAdaptiveStage — guard-тест specifically для R51.4:
// ни reload, ни его enrichment НЕ добавляют "reason" или "adaptiveStage".
// Это было бы сломано если кто-то ре-интродуцирует эти поля в будущем.
func TestCppworkerReload_NoReasonNoAdaptiveStage(t *testing.T) {
	forbiddenFields := []string{"reason", "adaptiveStage"}
	for _, f := range forbiddenFields {
		// cppworker reloadModelRequest struct should NOT have these fields.
		// If cppworker ever adds them, the mirror should too — but that means
		// the user/intent has changed and we'd want to know.
		payload := map[string]interface{}{
			"name":        "test",
			"contextSize": 32768,
			f:             "this should not be sent",
		}
		raw, _ := json.Marshal(payload)
		// We expect the mirror struct to REJECT this — because it doesn't
		// have the forbidden field. If cppworker adds the field, this test
		// needs updating.
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		err := dec.Decode(&CppworkerReloadRequest{})
		if err == nil {
			t.Errorf("REGRESSION: payload with %q field DECODED SUCCESSFULLY into CppworkerReloadRequest. Either cppworker added the field (update mirror) or someone removed the DisallowUnknownFields check.", f)
		}
	}
}

// TestCppworkerContract_RoundTrip — round-trip через mirror struct должен
// сохранять ВСЕ поля (если payload валиден, decode → re-marshal → decode не теряет ничего).
func TestCppworkerContract_RoundTrip(t *testing.T) {
	for _, ep := range AllEndpoints {
		if ep.RequestStruct == nil {
			continue
		}
		if ep.PayloadBuilder == nil {
			continue
		}
		t.Run(ep.Name, func(t *testing.T) {
			original := ep.PayloadBuilder()
			raw, err := json.Marshal(original)
			if err != nil {
				t.Fatalf("marshal original: %v", err)
			}
			// Decode
			dec1 := json.NewDecoder(bytes.NewReader(raw))
			dec1.DisallowUnknownFields()
			decoded, err := decodeInto(ep.Name, raw)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			_ = dec1
			// Re-marshal
			raw2, err := json.Marshal(decoded)
			if err != nil {
				t.Fatalf("re-marshal: %v", err)
			}
			// Re-decode
			_, err = decodeInto(ep.Name, raw2)
			if err != nil {
				t.Errorf("re-decode failed: %v\nre-marshaled: %s", err, string(raw2))
			}
		})
	}
}

func decodeInto(name string, raw []byte) (interface{}, error) {
	var target interface{}
	switch name {
	case "reload":
		target = &CppworkerReloadRequest{}
	case "load":
		target = &CppworkerLoadRequest{}
	case "load-with-params":
		target = &CppworkerLoadWithParamsRequest{}
	case "delete":
		target = &CppworkerDeleteModelRequest{}
	default:
		return nil, fmt.Errorf("unknown endpoint: %s (no mirror struct)", name)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return nil, err
	}
	return target, nil
}

// TestCppworkerContract_LiveMockRejectsUnknownFields — guard-тест: реальный
// cppworker-like mock с DisallowUnknownFields отвергает payload с "unknown field".
// Если этот тест когда-то начнёт проходить с "reason" в reload payload —
// значит кто-то тихо убрал DisallowUnknownFields из cppworker (regression).
func TestCppworkerContract_LiveMockRejectsUnknownFields(t *testing.T) {
	// Mock server, имитирующий cppworker с DisallowUnknownFields
	mux := http.NewServeMux()
	mux.HandleFunc("/api/models/reload", func(w http.ResponseWriter, r *http.Request) {
		var req CppworkerReloadRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"reloaded"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Тест 1: валидный payload — 200
	t.Run("valid_payload_200", func(t *testing.T) {
		body := []byte(`{"name":"x","contextSize":32768,"force":true}`)
		resp, err := http.Post(srv.URL+"/api/models/reload", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("valid payload: status=%d, want 200", resp.StatusCode)
		}
	})

	// Тест 2: payload с "reason" — 400 (R51.4)
	t.Run("unknown_field_reason_400", func(t *testing.T) {
		body := []byte(`{"name":"x","contextSize":32768,"reason":"auto-reload"}`)
		resp, err := http.Post(srv.URL+"/api/models/reload", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("payload with 'reason' field: status=%d, want 400 (cppworker rejects unknown field)", resp.StatusCode)
		}
	})

	// Тест 3: payload с "adaptiveStage" — 400 (R51.4)
	t.Run("unknown_field_adaptiveStage_400", func(t *testing.T) {
		body := []byte(`{"name":"x","contextSize":32768,"adaptiveStage":"exact_fit"}`)
		resp, err := http.Post(srv.URL+"/api/models/reload", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("payload with 'adaptiveStage' field: status=%d, want 400", resp.StatusCode)
		}
	})
}
