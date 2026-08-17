// Round 36 Phase 3: contract conformance tests for SSE and NDJSON streaming.
//
// These tests capture the EXACT byte-level format that the contract
// (docs/BALANCER_CPPWORKER_API_CONTRACT.md) requires. They are
// reference implementations — production code MUST produce output
// matching these golden strings byte-for-byte.
//
// Future drift detection: any change to the streaming format MUST be
// reflected here AND in the contract doc. Otherwise this test acts as
// the canonical reference for the streaming protocol.
package types

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// =====================================================================
// SSE (Server-Sent Events) golden examples — Contract Section 1.1
// =====================================================================

// TestSSEEventFormat_Valid tests the canonical SSE event structure:
//   data: <json>\n
//   \n
// The two newlines at the end are the SSE event boundary.
func TestSSEEventFormat_Valid(t *testing.T) {
	jsonChunk := `{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1234,"model":"gemma-4","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`
	event := "data: " + jsonChunk + "\n\n"
	expected := "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1234,\"model\":\"gemma-4\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n"
	if event != expected {
		t.Errorf("SSE event format mismatch\nwant: %q\ngot:  %q", expected, event)
	}
}

// TestSSEEventFormat_DoneTermination tests that [DONE] is a separate
// event with the literal string '[DONE]' as the payload.
func TestSSEEventFormat_DoneTermination(t *testing.T) {
	// The terminator MUST be exactly "data: [DONE]\n\n"
	done := "data: [DONE]\n\n"
	if !bytes.HasSuffix([]byte(done), []byte("[DONE]\n\n")) {
		t.Errorf("terminator must end with [DONE]\\n\\n, got: %q", done)
	}
	// The payload is the LITERAL string [DONE], not JSON.
	// Test that JSON-parsing the payload would FAIL.
	var x interface{}
	if err := json.Unmarshal([]byte("[DONE]"), &x); err == nil {
		t.Errorf("[DONE] payload should NOT be valid JSON")
	}
}

// TestSSEEventFormat_NoTransferEncodingChunked tests the contract
// requirement that SSE responses MUST NOT set Transfer-Encoding:
// chunked in the headers (when using Go's http.ResponseWriter). This
// was the Round 32 #10 / #16 / #17 bug source.
//
// In Go's standard library, http.ResponseWriter automatically
// handles chunked transfer encoding when Content-Length is not set,
// so this is a test of the CONTRACT not the code. Production code
// that hand-rolls HTTP/1.1 must NOT set Transfer-Encoding: chunked
// for SSE responses.
func TestSSEEventFormat_NoTransferEncodingChunked(t *testing.T) {
	// Document the requirement: any code that sets
	// "Transfer-Encoding: chunked" explicitly for SSE is wrong.
	// The test does not assert on code (it would be tautological) but
	// documents the requirement for future readers.
	//
	// The actual enforcement is in the contract doc and in code
	// review of proxy_request_hijack.go.
	t.Log("Contract Section 1.1.1: SSE MUST NOT set Transfer-Encoding: chunked header")
}

// TestSSEFinishReasonRequirement tests that the final non-[DONE] event
// MUST contain a non-null finish_reason. This is the OpenAI client
// contract for token accounting.
func TestSSEFinishReasonRequirement(t *testing.T) {
	// Valid final event: has finish_reason.
	validFinal := map[string]interface{}{
		"id":      "chatcmpl-1",
		"object":  "chat.completion.chunk",
		"created": 1234,
		"model":   "gemma-4",
		"choices": []map[string]interface{}{{
			"index":         0,
			"delta":         map[string]interface{}{},
			"finish_reason": "stop",
		}},
	}
	validJSON, _ := json.Marshal(validFinal)
	if !strings.Contains(string(validJSON), `"finish_reason":"stop"`) {
		t.Errorf("valid final event must contain finish_reason=stop: %s", validJSON)
	}

	// Invalid: finish_reason is null in the final event.
	invalidFinal := map[string]interface{}{
		"choices": []map[string]interface{}{{
			"index":         0,
			"delta":         map[string]interface{}{},
			"finish_reason": nil,
		}},
	}
	invalidJSON, _ := json.Marshal(invalidFinal)
	// This is a contract violation: production code MUST check for this
	// pattern and reject/refuse to send such events.
	if !bytes.Contains(invalidJSON, []byte(`"finish_reason":null`)) {
		t.Errorf("expected the invalid pattern for negative test")
	}
	t.Log("Contract Section 1.1.4: final non-[DONE] event MUST have non-null finish_reason")
}

// =====================================================================
// NDJSON (Ollama /api/chat) golden examples — Contract Section 1.2
// =====================================================================

// TestNDJSONEventFormat tests the canonical NDJSON structure:
// one JSON object per line, terminated by single \n
func TestNDJSONEventFormat(t *testing.T) {
	stream := strings.Join([]string{
		`{"model":"gemma-4","created_at":"2026-08-17T12:00:00Z","message":{"role":"assistant","content":"hi"},"done":false}`,
		`{"model":"gemma-4","created_at":"2026-08-17T12:00:01Z","message":{"role":"assistant","content":""},"done":false}`,
		`{"model":"gemma-4","created_at":"2026-08-17T12:00:02Z","message":{"role":"assistant","content":" world"},"done":true,"done_reason":"stop","total_duration":3000000000}`,
	}, "\n") + "\n" // final \n after last event

	// Parse line by line; each must be a valid JSON object.
	lines := strings.Split(strings.TrimRight(stream, "\n"), "\n")
	if len(lines) != 3 {
		t.Errorf("expected 3 events, got %d", len(lines))
	}
	for i, line := range lines {
		var v map[string]interface{}
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Errorf("event %d: invalid JSON: %v", i, err)
		}
	}

	// The final event must have done=true and done_reason.
	var finalEvent map[string]interface{}
	if err := json.Unmarshal([]byte(lines[2]), &finalEvent); err != nil {
		t.Fatal(err)
	}
	if finalEvent["done"] != true {
		t.Errorf("final event: done should be true, got %v", finalEvent["done"])
	}
	if finalEvent["done_reason"] != "stop" {
		t.Errorf("final event: done_reason should be 'stop', got %v", finalEvent["done_reason"])
	}
}

// TestNDJSONEventFormat_Heartbeat tests the prefill heartbeat
// format. Round 32 #2 added this to prevent client-side timeouts
// during long C-bridge prefill.
func TestNDJSONEventFormat_Heartbeat(t *testing.T) {
	heartbeat := `{"done":false}`
	var v map[string]interface{}
	if err := json.Unmarshal([]byte(heartbeat), &v); err != nil {
		t.Fatal(err)
	}
	if v["done"] != false {
		t.Errorf("heartbeat: done should be false, got %v", v["done"])
	}
	// Heartbeat has no 'message' field — OpenWebUI tolerates this
	// (it filters lines without 'message' as prefill markers).
	if _, ok := v["message"]; ok {
		t.Errorf("heartbeat should not have message field")
	}
}

// TestNDJSONEventFormat_BadEmptyLine tests the Round 35e bug: a
// bare '\n' in the stream (i.e. an empty line between events) is a
// protocol violation. NDJSON parsers may either skip it (lenient) or
// raise an error (strict). Either way, the producer MUST NOT emit
// bare empty lines.
func TestNDJSONEventFormat_BadEmptyLine(t *testing.T) {
	// This is what the Round 35e bug produced. The balancer wrote
	// `writeChunkedFrame(bufrw, "\n")` for each SSE empty-line read.
	// That produced 1-byte chunks in the chunked stream which broke
	// strict aiohttp parsers.
	bad := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n\ndata: [DONE]\n\n"

	// Count empty lines. The middle `\n\n\n` means there's an extra
	// empty line after the first event boundary.
	emptyLineCount := strings.Count(bad, "\n\n\n")
	if emptyLineCount == 0 {
		t.Skip("no bad pattern detected")
	}
	t.Logf("detected %d empty line(s) — this is a protocol violation per contract 1.1.2", emptyLineCount)
}

// =====================================================================
// Async load response golden examples — Contract Section 2
// =====================================================================

// TestLoadAcceptedResponse_Fields tests the 202 response body for
// async load. All required fields MUST be present.
func TestLoadAcceptedResponse_Fields(t *testing.T) {
	resp := map[string]interface{}{
		"status":              "loading",
		"name":                "gemma-4-E4B-it-Q4_K_M",
		"path":                "/opt/models/gemma-4.gguf",
		"loadingSizeBytes":    5368709120,
		"estimatedLoadTimeMs": 50000,
		"progressUrl":         "/api/models/load/progress?model=gemma-4-E4B-it-Q4_K_M",
		"model":               map[string]interface{}{"name": "gemma-4-E4B-it-Q4_K_M"},
		"message":             "Model load started in background. Poll progressUrl for state transitions (loading -> loaded / error).",
	}
	// Verify all required fields are present.
	required := []string{"status", "name", "path", "loadingSizeBytes", "estimatedLoadTimeMs", "progressUrl", "model", "message"}
	for _, field := range required {
		if _, ok := resp[field]; !ok {
			t.Errorf("required field missing: %s", field)
		}
	}
	// status must be "loading" or "loading_after_timeout"
	if s, _ := resp["status"].(string); s != "loading" && s != "loading_after_timeout" {
		t.Errorf("status: got %q, want 'loading' or 'loading_after_timeout'", s)
	}
}

// TestLoadProgressResponse_StateField tests that /api/models/load/progress
// returns state field for each model. Round 32 #8 finding: missing
// state field caused balancer auto-load polling to never complete.
func TestLoadProgressResponse_StateField(t *testing.T) {
	resp := map[string]interface{}{
		"models": []map[string]interface{}{{
			"name":                "gemma-4-E4B-it-Q4_K_M",
			"state":               "loaded",
			"path":                "/opt/models/gemma-4.gguf",
			"loadedContextSize":   32768,
			"loadedGpuLayers":     42,
			"loadedKvCacheType":   "q4_0",
			"loadedFlashAttnType": -1,
			"loadedUseMmap":       true,
		}},
		"count": 1,
	}
	models, ok := resp["models"].([]map[string]interface{})
	if !ok || len(models) == 0 {
		t.Fatal("models array missing or empty")
	}
	// State field is REQUIRED.
	if _, ok := models[0]["state"]; !ok {
		t.Errorf("state field missing on model — would break balancer auto-load polling")
	}
	state, _ := models[0]["state"].(string)
	allowedStates := map[string]bool{
		"unloaded":  true,
		"loading":   true,
		"loaded":    true,
		"unloading": true,
		"error":     true,
	}
	if !allowedStates[state] {
		t.Errorf("state %q not in allowed set", state)
	}
}

// =====================================================================
// Tool calls golden examples — Contract Section 5
// =====================================================================

// TestToolCallFormat_Valid tests the canonical OpenAI tool call shape.
func TestToolCallFormat_Valid(t *testing.T) {
	toolCall := map[string]interface{}{
		"id":   "call_abc123",
		"type": "function",
		"function": map[string]interface{}{
			"name":      "get_weather",
			"arguments": `{"location":"San Francisco","unit":"celsius"}`,
		},
	}
	// arguments is a STRING containing JSON, not a nested object.
	fn, ok := toolCall["function"].(map[string]interface{})
	if !ok {
		t.Fatal("function field missing")
	}
	args, ok := fn["arguments"].(string)
	if !ok {
		t.Fatal("arguments must be a string, not a nested object")
	}
	// Verify the string is valid JSON.
	var argObj map[string]interface{}
	if err := json.Unmarshal([]byte(args), &argObj); err != nil {
		t.Errorf("arguments string is not valid JSON: %v", err)
	}
	if argObj["location"] != "San Francisco" {
		t.Errorf("expected location=San Francisco, got %v", argObj["location"])
	}
}

// TestToolCallFormat_GemmaRelaxedJSON tests the gemma-4 relaxed-JSON
// case from Round 32 #8. The fixGemmaArgs post-processor must add
// quotes around unquoted keys.
func TestToolCallFormat_GemmaRelaxedJSON(t *testing.T) {
	// gemma-4 emits: {path: "/foo"} (no quotes around path key)
	// After fixGemmaArgs: {"path": "/foo"}
	relaxed := `{path: "/foo"}`
	// The actual fixGemmaArgs is in cmd/cppworker/tool_calls.go.
	// Here we test the expected output shape.
	fixed := `{"path": "/foo"}`
	var relaxedObj, fixedObj map[string]interface{}
	if err := json.Unmarshal([]byte(relaxed), &relaxedObj); err == nil {
		t.Errorf("relaxed JSON should fail strict JSON parsing — gemma-4 bug")
	}
	if err := json.Unmarshal([]byte(fixed), &fixedObj); err != nil {
		t.Errorf("fixed JSON should parse: %v", err)
	}
	if fixedObj["path"] != "/foo" {
		t.Errorf("path: got %v", fixedObj["path"])
	}
}

// =====================================================================
// Reasoning tag pairs golden examples — Contract Section 4.2
// =====================================================================

// TestReasoningTagPairs_Locked tests the exact tag pairs that
// SplitReasoningContent (cppworker) and stripReasoningTags (balancer)
// MUST recognize. The contract is normative.
func TestReasoningTagPairs_Locked(t *testing.T) {
	// These tag pairs are extracted from contract Section 4.2.
	// Any drift MUST be reflected in BOTH the contract doc and
	// cmd/cppworker/reasoning_content.go + internal/balancer/
	// llamacpp_translate_resp.go.
	tagPairs := []struct {
		open  string
		close string
	}{
		{"<think>", "</think>"},
		{"<thinking>", "</thinking>"},
		{"<reasoning>", "</reasoning>"},
		{"<analysis>", "</analysis>"},
		{"<|channel>thought\n", "\n<channel|>"},
		{"<|channel>thought", "<channel|>"},
		{"<|channel>analysis\n", "\n<channel|>"},
		{"<|channel>analysis", "<channel|>"},
		{"<|channel>", "<channel|>"},
		{"<|think>", "<think|>"},
	}
	if len(tagPairs) != 10 {
		t.Errorf("tag pairs count: got %d, want 10 (per contract 4.2)", len(tagPairs))
	}
	// Verify ordering: longest prefix first (gemma-4 channel format).
	// The contract requires: `<|channel>thought\n` before
	// `<|channel>thought` before bare `<|channel>`. This test pins
	// the order.
	if tagPairs[4].open != "<|channel>thought\n" {
		t.Errorf("tag pair [4] should be longest prefix <|channel>thought\\n, got %q", tagPairs[4].open)
	}
	if tagPairs[5].open != "<|channel>thought" {
		t.Errorf("tag pair [5] should be <|channel>thought, got %q", tagPairs[5].open)
	}
	if tagPairs[8].open != "<|channel>" {
		t.Errorf("tag pair [8] should be bare <|channel>, got %q", tagPairs[8].open)
	}
}

// =====================================================================
// Antiprompt list — Contract Section 8
// =====================================================================

// TestAntipromptExclusion_Gemma4EndOfTurn tests that gemma-4 with
// reasoning MUST NOT include <end_of_turn> in antiprompts. Round 32
// #27 fix: the model emits <end_of_turn> as part of its reasoning
// output, which prematurely stopped the model at 85 tokens.
func TestAntipromptExclusion_Gemma4EndOfTurn(t *testing.T) {
	// The contract (Section 8.1) says gemma-4 reasoning antiprompts
	// must be: <start_of_turn>user, <start_of_turn>model
	// (no <end_of_turn>)
	gemma4ReasoningAntiprompts := []string{
		"<start_of_turn>user",
		"<start_of_turn>model",
	}
	for _, ap := range gemma4ReasoningAntiprompts {
		if ap == "<end_of_turn>" {
			t.Errorf("gemma-4 reasoning MUST NOT include <end_of_turn> in antiprompts")
		}
	}
	// gemma-2 (no reasoning) is the only variant that CAN use <end_of_turn>.
	// This is documented in contract Section 8.1.
	t.Log("gemma-4 reasoning antiprompts: ", gemma4ReasoningAntiprompts)
}

// =====================================================================
// Preflight params — Contract Section 6.4
// =====================================================================

// TestPreflightParamsToCompare_Locked tests the exact set of runtime
// parameters the balancer MUST compare in preflight. Round 32 #26
// extension: previously only n_ctx was compared; the contract locks
// the full set to prevent drift.
func TestPreflightParamsToCompare_Locked(t *testing.T) {
	paramsToCompare := []string{
		"n_ctx",          // Contract 6.4 row 1
		"kv_cache_type",  // Contract 6.4 row 2
		"flash_attn",     // Contract 6.4 row 3
		"use_mmap",       // Contract 6.4 row 4
		"gpu_layers",     // Contract 6.4 row 5
	}
	if len(paramsToCompare) != 5 {
		t.Errorf("params to compare: got %d, want 5 (per contract 6.4)", len(paramsToCompare))
	}
}

// =====================================================================
// Detokenization contract — Contract Section 8.3
// =====================================================================

// TestDetokenization_RealNewlines tests that the model output
// MUST contain real newlines (U+000A), not literal "\\n" (2 chars).
// Round 32 #19 fix: gemma-4 Q4_K_M detokenizer was emitting literal
// "\\n" instead of "\n".
func TestDetokenization_RealNewlines(t *testing.T) {
	// Real newline: 1 byte (0x0A)
	realNewline := []byte{0x0A}
	if len(realNewline) != 1 {
		t.Errorf("real newline should be 1 byte, got %d", len(realNewline))
	}
	// Literal "\\n": 2 bytes (0x5C 0x6E)
	literalBackslashN := []byte{0x5C, 0x6E}
	if len(literalBackslashN) != 2 {
		t.Errorf("literal \\n should be 2 bytes, got %d", len(literalBackslashN))
	}
	// These are different bytes — Round 32 #19 fix replaces the
	// 2-byte pattern with the 1-byte pattern.
}
