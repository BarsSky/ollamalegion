// llamacpp_translate_resp_strip_streaming_test.go — tests for stateful strip
// в streaming path (Round 31 #4).
//
// cppworker (gemma-4) после SplitReasoningContent эмитит "\n" перед первым
// content токеном. Translator должен TrimLeft content когда reasoning уже был
// в этом стриме (через pointer state).
package balancer

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestTranslateSSEChatToOllama_StripLeadingNewlineAfterReasoning — главный кейс #4.
// Reasoning chunks → content chunk с leading "\n" → после translate content = "4"
func TestTranslateSSEChatToOllama_StripLeadingNewlineAfterReasoning(t *testing.T) {
	seenReasoning := false
	// 1. Reasoning chunk
	reasoningChunk := `{"choices":[{"delta":{"reasoning_content":"The user asks 2+2."}}]}`
	r1 := translateOpenAISSEDataToOllama("/api/chat", []byte(reasoningChunk), "gemma-4", &seenReasoning)
	if r1 == nil {
		t.Fatal("reasoning chunk returned nil")
	}
	// 2. Content chunk с leading "\n" (cppworker pattern после SplitReasoningContent)
	contentChunk := `{"choices":[{"delta":{"content":"\n4"}}]}`
	r2 := translateOpenAISSEDataToOllama("/api/chat", []byte(contentChunk), "gemma-4", &seenReasoning)
	if r2 == nil {
		t.Fatal("content chunk returned nil")
	}
	if !seenReasoning {
		t.Error("expected seenReasoning=true after reasoning chunk")
	}
	// Verify content trimmed
	var parsed map[string]interface{}
	if err := json.Unmarshal(r2, &parsed); err != nil {
		t.Fatalf("unmarshal: %v, raw=%s", err, r2)
	}
	msg, ok := parsed["message"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected message object, got %T: %s", parsed["message"], r2)
	}
	content, _ := msg["content"].(string)
	if content != "4" {
		t.Errorf("expected content='4' (trimmed leading \\n), got %q (raw: %s)", content, r2)
	}
}

// TestTranslateSSEChatToOllama_NoStripWithoutReasoning — content без reasoning
// в стриме не должен быть тронут (defensive — TrimLeft применяется только
// если reasoning был).
func TestTranslateSSEChatToOllama_NoStripWithoutReasoning(t *testing.T) {
	contentChunk := `{"choices":[{"delta":{"content":"hello world"}}]}`
	seenReasoning := false
	r := translateOpenAISSEDataToOllama("/api/chat", []byte(contentChunk), "test-model", &seenReasoning)
	if r == nil {
		t.Fatal("content chunk returned nil")
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(r, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msg, _ := parsed["message"].(map[string]interface{})
	content, _ := msg["content"].(string)
	if content != "hello world" {
		t.Errorf("expected content='hello world', got %q", content)
	}
	if seenReasoning {
		t.Error("seenReasoning should still be false (no reasoning chunk yet)")
	}
}

// TestTranslateSSEChatToOllama_StripMultiChunkContent — content приходит
// несколькими чанками: "\n" + "4" + " is the answer". После strip + accumulate
// должны получить "4 is the answer" (без leading \n).
func TestTranslateSSEChatToOllama_StripMultiChunkContent(t *testing.T) {
	seenReasoning := false
	// Reasoning first
	rc := `{"choices":[{"delta":{"reasoning_content":"thinking..."}}]}`
	_ = translateOpenAISSEDataToOllama("/api/chat", []byte(rc), "gemma-4", &seenReasoning)
	// Content chunk 1: just "\n"
	c1 := `{"choices":[{"delta":{"content":"\n"}}]}`
	r1 := translateOpenAISSEDataToOllama("/api/chat", []byte(c1), "gemma-4", &seenReasoning)
	if r1 == nil {
		t.Fatal("content chunk 1 returned nil")
	}
	var p1 map[string]interface{}
	_ = json.Unmarshal(r1, &p1)
	msg1, _ := p1["message"].(map[string]interface{})
	c1str, _ := msg1["content"].(string)
	if c1str != "" {
		t.Errorf("expected content='' (single \\n trimmed), got %q", c1str)
	}
	// Content chunk 2: "4"
	c2 := `{"choices":[{"delta":{"content":"4"}}]}`
	r2 := translateOpenAISSEDataToOllama("/api/chat", []byte(c2), "gemma-4", &seenReasoning)
	if r2 == nil {
		t.Fatal("content chunk 2 returned nil")
	}
	var p2 map[string]interface{}
	_ = json.Unmarshal(r2, &p2)
	msg2, _ := p2["message"].(map[string]interface{})
	c2str, _ := msg2["content"].(string)
	if c2str != "4" {
		t.Errorf("expected content='4', got %q", c2str)
	}
}

// TestTranslateSSEGenerateToOllama_StripAfterReasoning — /api/generate path.
func TestTranslateSSEGenerateToOllama_StripAfterReasoning(t *testing.T) {
	seenReasoning := false
	// Reasoning first
	rc := `{"choices":[{"reasoning_content":"thinking..."}]}`
	_ = translateOpenAISSEDataToOllama("/api/generate", []byte(rc), "gemma-4", &seenReasoning)
	// Content with leading \n
	cc := `{"choices":[{"text":"\nanswer"}]}`
	r := translateOpenAISSEDataToOllama("/api/generate", []byte(cc), "gemma-4", &seenReasoning)
	if r == nil {
		t.Fatal("content chunk returned nil")
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(r, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	response, _ := parsed["response"].(string)
	if response != "answer" {
		t.Errorf("expected response='answer' (trimmed leading \\n), got %q", response)
	}
}

// TestTranslateSSEChatToOllama_NilStateIsStateless — pointer nil = stateless
// (используется в legacy callers, не должно падать).
func TestTranslateSSEChatToOllama_NilStateIsStateless(t *testing.T) {
	// Без state, без reasoning — content не трогаем
	cc := `{"choices":[{"delta":{"content":"\nhello"}}]}`
	r := translateOpenAISSEDataToOllama("/api/chat", []byte(cc), "test-model", nil)
	if r == nil {
		t.Fatal("content chunk returned nil")
	}
	var parsed map[string]interface{}
	_ = json.Unmarshal(r, &parsed)
	msg, _ := parsed["message"].(map[string]interface{})
	content, _ := msg["content"].(string)
	// nil state = no strip applied, content remains "\nhello"
	if content != "\nhello" {
		t.Errorf("expected content='\\nhello' (stateless, no strip), got %q", content)
	}
}

// TestTranslateSSEChatToOllama_FullGemmaStreamSimulation — эмулируем полный стрим gemma-4.
// Round 31 #4 главный сценарий: SplitReasoningContent → thinking → "\n" → answer.
func TestTranslateSSEChatToOllama_FullGemmaStreamSimulation(t *testing.T) {
	seenReasoning := false
	chunks := []string{
		`{"choices":[{"delta":{"role":"assistant"}}]}`,                                     // 1: role
		`{"choices":[{"delta":{"reasoning_content":"The user asks 2+2=?"}}]}`,             // 2: reasoning
		`{"choices":[{"delta":{"reasoning_content":" It's simple math."}}]}`,              // 3: reasoning more
		`{"choices":[{"delta":{"content":"\n4"}}]}`,                                        // 4: content with leading \n
		`{"choices":[{"delta":{"content":" is the answer."}}]}`,                            // 5: content more
		`{"choices":[{"finish_reason":"stop"}]}`,                                            // 6: final
	}
	allContents := []string{}
	for i, raw := range chunks {
		r := translateOpenAISSEDataToOllama("/api/chat", []byte(raw), "gemma-4", &seenReasoning)
		if r == nil {
			t.Fatalf("chunk %d returned nil", i+1)
		}
		var parsed map[string]interface{}
		if err := json.Unmarshal(r, &parsed); err != nil {
			t.Fatalf("chunk %d unmarshal: %v, raw=%s", i+1, err, r)
		}
		// Skip role-only / final chunks
		if msg, ok := parsed["message"].(map[string]interface{}); ok {
			if c, ok := msg["content"].(string); ok && c != "" {
				allContents = append(allContents, c)
			}
		}
	}
	full := strings.Join(allContents, "")
	// Critical: NO leading "\n" in accumulated content
	if strings.HasPrefix(full, "\n") {
		t.Errorf("accumulated content has leading \\n: %q (Round 31 #4 leak!)", full)
	}
	// Should be: "4 is the answer." (2 content chunks joined: "4" + " is the answer.")
	if full != "4 is the answer." {
		t.Errorf("expected '4 is the answer.', got %q", full)
	}
	if !seenReasoning {
		t.Error("expected seenReasoning=true after reasoning chunks")
	}
}
