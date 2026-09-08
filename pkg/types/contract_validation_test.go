// Round 36 Phase 2: tests for contract validation helpers.
//
// These tests verify the strict JSON decoder behavior:
//   - DisallowUnknownFields rejects typos like "maxTokens" instead of "max_tokens"
//   - Body size limit enforced
//   - Empty body rejected
//   - Type mismatches reported cleanly
//   - Field name and header name constants match expected values
//
// R60.16 (2026-09-08): UTF-8 BOM (EF BB BF) is stripped before decoding.
// Pre-R60.16: PowerShell Out-File default adds BOM → 400 invalid character.
package types

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestDecodeJSONRequest_Valid(t *testing.T) {
	type req struct {
		Model     string  `json:"model"`
		MaxTokens *int    `json:"max_tokens,omitempty"`
		Stream    bool    `json:"stream"`
	}
	body := strings.NewReader(`{"model":"gemma-4","max_tokens":1024,"stream":true}`)
	var got req
	if err := DecodeJSONRequest(body, 4096, &got); err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if got.Model != "gemma-4" {
		t.Errorf("model: got %q, want %q", got.Model, "gemma-4")
	}
	if got.MaxTokens == nil || *got.MaxTokens != 1024 {
		t.Errorf("max_tokens: got %v, want 1024", got.MaxTokens)
	}
	if !got.Stream {
		t.Errorf("stream: got false, want true")
	}
}

func TestDecodeJSONRequest_UnknownField_Rejected(t *testing.T) {
	// This is the critical Phase 2 guarantee: client typos are caught.
	type req struct {
		MaxTokens int `json:"max_tokens"`
	}
	body := strings.NewReader(`{"maxTokens": 1024}`) // typo: camelCase
	var got req
	err := DecodeJSONRequest(body, 4096, &got)
	if err == nil {
		t.Fatal("expected error for unknown field maxTokens, got nil")
	}
	if !strings.Contains(err.Error(), "maxTokens") {
		t.Errorf("error should mention field name: %v", err)
	}
}

func TestDecodeJSONRequest_EmptyBody(t *testing.T) {
	body := strings.NewReader("")
	var got map[string]interface{}
	err := DecodeJSONRequest(body, 4096, &got)
	if !errors.Is(err, ErrBodyEmpty) {
		t.Errorf("got %v, want ErrBodyEmpty", err)
	}
}

func TestDecodeJSONRequest_BodyTooLarge(t *testing.T) {
	// 100 bytes of body, limit 50. Should fail.
	body := strings.NewReader(strings.Repeat("x", 100))
	var got map[string]interface{}
	err := DecodeJSONRequest(body, 50, &got)
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Errorf("got %v, want ErrBodyTooLarge", err)
	}
}

func TestDecodeJSONRequest_InvalidJSON(t *testing.T) {
	body := strings.NewReader(`{"model": }`) // syntax error
	var got map[string]interface{}
	err := DecodeJSONRequest(body, 4096, &got)
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
	if errors.Is(err, ErrBodyEmpty) || errors.Is(err, ErrBodyTooLarge) {
		t.Errorf("should be a JSON error, not a body-size error: %v", err)
	}
}

func TestDecodeJSONRequest_TypeMismatch(t *testing.T) {
	type req struct {
		MaxTokens int `json:"max_tokens"`
	}
	body := strings.NewReader(`{"max_tokens": "not a number"}`)
	var got req
	err := DecodeJSONRequest(body, 4096, &got)
	if err == nil {
		t.Fatal("expected type mismatch error, got nil")
	}
	if !IsJSONSyntaxError(err) {
		t.Errorf("IsJSONSyntaxError should report true: %v", err)
	}
}

func TestStrictDecoder_BodyBytes(t *testing.T) {
	data := []byte(`{"model":"gemma-4"}`)
	dec := NewStrictDecoderFromBytes(data)
	if dec.BodyBytes() != int64(len(data)) {
		t.Errorf("BodyBytes: got %d, want %d", dec.BodyBytes(), len(data))
	}
}

func TestNewStrictDecoder_NilReader(t *testing.T) {
	dec, err := NewStrictDecoder(nil, 4096)
	if err == nil {
		t.Fatal("expected error for nil reader")
	}
	if dec != nil {
		t.Errorf("dec should be nil on error, got %v", dec)
	}
	if !errors.Is(err, ErrBodyEmpty) {
		t.Errorf("got %v, want ErrBodyEmpty", err)
	}
}

func TestIsJSONSyntaxError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"syntax", &json.SyntaxError{Offset: 5}, true},
		{"type", &json.UnmarshalTypeError{Field: "x"}, true},
		{"unexpected eof", io.ErrUnexpectedEOF, true},
		{"io.EOF", io.EOF, true},
		{"regular error", errors.New("oops"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := IsJSONSyntaxError(tc.err)
			if got != tc.want {
				t.Errorf("IsJSONSyntaxError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestFieldNameConstants_MatchContract(t *testing.T) {
	// The contract doc says these specific field names. If a constant
	// drifts, the test fails — preventing silent contract drift.
	tests := map[string]string{
		FieldModel:           "model",
		FieldMessages:        "messages",
		FieldStream:          "stream",
		FieldTools:           "tools",
		FieldContent:         "content",
		FieldRole:            "role",
		FieldReasoning:       "reasoning",
		FieldReasoningContent: "reasoning_content",
		FieldThinking:        "thinking",
		FieldTemperature:     "temperature",
		FieldTopP:            "top_p",
		FieldMaxTokens:       "max_tokens",
		FieldNumCtx:          "num_ctx",
		FieldContextSize:     "contextSize",
		FieldGpuLayers:       "gpuLayers",
		FieldKvCacheType:     "kvCacheType",
		FieldFlashAttn:       "flashAttn",
		FieldUseMmap:         "useMmap",
		FieldEnableReasoning: "enableReasoning",
		FieldKeepAlive:       "keep_alive",
		FieldStreamOptions:   "stream_options",
		FieldIncludeUsage:    "include_usage",
	}
	for got, want := range tests {
		if got != want {
			t.Errorf("FieldNameConstants drift: got %q, want %q", got, want)
		}
	}
}

func TestHeaderNameConstants_MatchContract(t *testing.T) {
	tests := map[string]string{
		HeaderContentType:      "Content-Type",
		HeaderAuthorization:    "Authorization",
		HeaderXAPIToken:        "X-API-Token",
		HeaderXRequestID:       "X-Request-Id",
		HeaderLocation:         "Location",
		HeaderCacheControl:     "Cache-Control",
		HeaderConnection:       "Connection",
		HeaderRetryAfter:       "Retry-After",
		HeaderTransferEncoding: "Transfer-Encoding",
		HeaderContentLength:    "Content-Length",
	}
	for got, want := range tests {
		if got != want {
			t.Errorf("HeaderNameConstants drift: got %q, want %q", got, want)
		}
	}
}

func TestContentTypeConstants(t *testing.T) {
	// Contract Section 1.1.1: SSE content type is exactly "text/event-stream".
	if ContentTypeSSE != "text/event-stream" {
		t.Errorf("ContentTypeSSE: got %q, want %q", ContentTypeSSE, "text/event-stream")
	}
	// Contract Section 1.2.1: NDJSON content type is "application/x-ndjson".
	if ContentTypeNDJSON != "application/x-ndjson" {
		t.Errorf("ContentTypeNDJSON: got %q, want %q", ContentTypeNDJSON, "application/x-ndjson")
	}
	if ContentTypeJSON != "application/json" {
		t.Errorf("ContentTypeJSON: got %q, want %q", ContentTypeJSON, "application/json")
	}
}

func TestBearerPrefix(t *testing.T) {
	if BearerPrefix != "Bearer " {
		t.Errorf("BearerPrefix: got %q, want %q (with trailing space)", BearerPrefix, "Bearer ")
	}
}

func TestMaxBytesConstants(t *testing.T) {
	// Round 36 Phase 2: defaults are 4MB streaming, 32MB regular.
	if MaxStreamingBodyBytes != 4*1024*1024 {
		t.Errorf("MaxStreamingBodyBytes: got %d, want 4194304", MaxStreamingBodyBytes)
	}
	if MaxRequestBodyBytes != 32*1024*1024 {
		t.Errorf("MaxRequestBodyBytes: got %d, want 33554432", MaxRequestBodyBytes)
	}
}

func TestDecodeJSONRequest_OpenAIChatExample(t *testing.T) {
	// Realistic OpenAI chat completion request body.
	body := `{
		"model": "gemma-4-E4B-it-Q4_K_M",
		"messages": [
			{"role": "system", "content": "You are a helpful assistant."},
			{"role": "user", "content": "Hello"}
		],
		"stream": true,
		"temperature": 0,
		"max_tokens": 2048
	}`
	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	type req struct {
		Model       string   `json:"model"`
		Messages    []msg    `json:"messages"`
		Stream      bool     `json:"stream"`
		Temperature *float64 `json:"temperature,omitempty"`
		MaxTokens   int      `json:"max_tokens,omitempty"`
	}
	var got req
	if err := DecodeJSONRequest(strings.NewReader(body), MaxStreamingBodyBytes, &got); err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if got.Model != "gemma-4-E4B-it-Q4_K_M" {
		t.Errorf("model: got %q", got.Model)
	}
	if len(got.Messages) != 2 {
		t.Errorf("messages: got %d, want 2", len(got.Messages))
	}
	if got.Temperature == nil || *got.Temperature != 0 {
		t.Errorf("temperature: got %v, want 0 (greedy)", got.Temperature)
	}
}

func TestDecodeJSONRequest_OllamaLoadExample(t *testing.T) {
	// Realistic Ollama load request body. Must accept camelCase fields
	// used by balancer profile system AND snake_case fields used by
	// direct Ollama clients.
	body := `{
		"name": "gemma-4-E4B-it-Q4_K_M",
		"contextSize": 32768,
		"gpuLayers": 42,
		"kvCacheType": "q4_0",
		"enableReasoning": true
	}`
	type req struct {
		Name            string `json:"name"`
		ContextSize     int    `json:"contextSize"`
		GpuLayers       int    `json:"gpuLayers"`
		KvCacheType     string `json:"kvCacheType"`
		EnableReasoning bool   `json:"enableReasoning"`
	}
	var got req
	if err := DecodeJSONRequest(strings.NewReader(body), MaxRequestBodyBytes, &got); err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if got.Name != "gemma-4-E4B-it-Q4_K_M" {
		t.Errorf("name: got %q", got.Name)
	}
	if got.ContextSize != 32768 {
		t.Errorf("contextSize: got %d", got.ContextSize)
	}
	if got.GpuLayers != 42 {
		t.Errorf("gpuLayers: got %d", got.GpuLayers)
	}
	if !got.EnableReasoning {
		t.Errorf("enableReasoning: got false")
	}
}

func TestDecodeJSONRequest_NilBody(t *testing.T) {
	// io.Reader that returns no data.
	body := bytes.NewReader(nil)
	var got map[string]interface{}
	err := DecodeJSONRequest(body, 4096, &got)
	if !errors.Is(err, ErrBodyEmpty) {
		t.Errorf("got %v, want ErrBodyEmpty", err)
	}
}

func TestDecodeJSONRequest_BodyAtExactLimit(t *testing.T) {
	// Body of exactly maxBytes — should succeed (limit is inclusive).
	payload := `{"model":"gemma-4"}` // 20 bytes
	body := strings.NewReader(payload)
	var got map[string]interface{}
	if err := DecodeJSONRequest(body, int64(len(payload)), &got); err != nil {
		t.Fatalf("body at exact limit should succeed: %v", err)
	}
}

func TestDecodeJSONRequest_BodyAtLimitPlus1(t *testing.T) {
	// Body of maxBytes+1 — should fail with ErrBodyTooLarge.
	payload := `{"model":"gemma-4"}` // 20 bytes
	body := strings.NewReader(payload)
	var got map[string]interface{}
	err := DecodeJSONRequest(body, int64(len(payload)-1), &got)
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Errorf("got %v, want ErrBodyTooLarge", err)
	}
}

// TestDecodeJSONRequest_StripsBOM — R60.16 (2026-09-08):
// UTF-8 BOM (EF BB BF) at the start of the body must be stripped
// before JSON decoding. Pre-R60.16: BOM caused 400 "invalid
// character ï looking for beginning of value" because
// json.NewDecoder sees EF BB BF as part of the JSON stream.
func TestDecodeJSONRequest_StripsBOM(t *testing.T) {
	bom := []byte{0xEF, 0xBB, 0xBF}
	body := append(bom, []byte(`{"model":"gemma-4","max_tokens":42}`)...)
	dec, err := NewStrictDecoder(bytes.NewReader(body), 4096)
	if err != nil {
		t.Fatalf("NewStrictDecoder: %v", err)
	}
	type req struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
	}
	var got req
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("decode with BOM: %v", err)
	}
	if got.Model != "gemma-4" {
		t.Errorf("model: got %q, want gemma-4 (BOM should be stripped)", got.Model)
	}
	if got.MaxTokens != 42 {
		t.Errorf("max_tokens: got %d, want 42 (BOM should be stripped)", got.MaxTokens)
	}
}

// TestDecodeJSONRequest_NoBOM_Regression — non-BOM body still works.
func TestDecodeJSONRequest_NoBOM_Regression(t *testing.T) {
	body := []byte(`{"model":"gemma-4","max_tokens":42}`)
	dec, err := NewStrictDecoder(bytes.NewReader(body), 4096)
	if err != nil {
		t.Fatalf("NewStrictDecoder: %v", err)
	}
	type req struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
	}
	var got req
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("decode without BOM: %v", err)
	}
	if got.Model != "gemma-4" {
		t.Errorf("model: got %q, want gemma-4", got.Model)
	}
}

// TestDecodeJSONRequest_OnlyBOM_Empty — body that is JUST the BOM
// (3 bytes) should be treated as empty (after strip → 0 bytes).
func TestDecodeJSONRequest_OnlyBOM_Empty(t *testing.T) {
	body := []byte{0xEF, 0xBB, 0xBF}
	var got map[string]interface{}
	err := DecodeJSONRequest(bytes.NewReader(body), 4096, &got)
	if !errors.Is(err, ErrBodyEmpty) {
		t.Errorf("got %v, want ErrBodyEmpty (BOM-only should be empty after strip)", err)
	}
}
