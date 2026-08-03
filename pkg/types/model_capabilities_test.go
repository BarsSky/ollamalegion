package types

import "testing"

func TestDefaultCapabilitiesForModelName(t *testing.T) {
	tests := []struct {
		name        string
		modelName   string
		wantReason  bool
		wantVision  bool
		wantTools   bool
		wantArch    string
	}{
		// Reasoning models
		{"qwen3-thinking", "Qwen3-4B-Thinking", true, false, true, "qwen"},
		{"qwen3-instruct (no reasoning)", "Qwen3-4B-Instruct-2507", false, false, true, "qwen"},
		{"qwen3.5 reasoning", "qwen3.5-7B", true, false, true, "qwen"},
		{"deepseek-r1", "deepseek-r1-distill-qwen-7B", true, false, true, "deepseek"},
		{"kimi-k2", "kimi-k2-7B", true, false, true, ""},
		{"gemma-4-it (instruct, no reasoning)", "gemma-4-E4B-it-Q4_K_M", false, false, true, "gemma"},
		{"gemma-4-thinking (reasoning)", "gemma-4-9B-thinking", true, false, true, "gemma"},
		// -r1 suffix
		{"llama-3.1-r1", "llama-3.1-8B-r1", true, false, true, "llama"},
		// Vision models
		{"llava", "llava-1.6-7b", false, true, true, "llava"},
		{"moondream", "moondream2", false, true, true, ""},
		{"qwen2.5-vl", "Qwen2.5-VL-7B-Instruct", false, true, true, "qwen"},
		// Non-vision qwen3
		{"qwen3 no vision", "qwen3-4b", true, false, true, "qwen"},
		// No tools
		{"bge embedding", "bge-large-en-v1.5", false, false, false, ""},
		{"gpt2 legacy", "gpt2", false, false, false, ""},
		// Plain instruct
		{"llama instruct", "llama-3.1-8B-Instruct", false, false, true, "llama"},
		// mistral
		{"mistral", "mistral-7B-Instruct", false, false, true, "mistral"},
		// Edge: empty
		{"empty name", "", false, false, true, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			caps := DefaultCapabilitiesForModelName(tt.modelName)
			if caps.Reasoning != tt.wantReason {
				t.Errorf("Reasoning: got %v, want %v (caps=%+v)", caps.Reasoning, tt.wantReason, caps)
			}
			if caps.Vision != tt.wantVision {
				t.Errorf("Vision: got %v, want %v (caps=%+v)", caps.Vision, tt.wantVision, caps)
			}
			if caps.Tools != tt.wantTools {
				t.Errorf("Tools: got %v, want %v (caps=%+v)", caps.Tools, tt.wantTools, caps)
			}
			if caps.Architecture != tt.wantArch {
				t.Errorf("Architecture: got %q, want %q (caps=%+v)", caps.Architecture, tt.wantArch, caps)
			}
			if caps.Source != "auto-detect" {
				t.Errorf("Source: got %q, want %q", caps.Source, "auto-detect")
			}
		})
	}
}

func TestCapabilitiesHeaders(t *testing.T) {
	caps := ModelCapabilities{
		Reasoning:    true,
		Tools:        true,
		Vision:       false,
		Embeddings:   true,
		MaxContext:   32768,
		Architecture: "qwen",
	}
	h := caps.Headers()
	if h["X-Model-Capabilities"] != "reasoning,tools,embeddings" {
		t.Errorf("X-Model-Capabilities: got %q", h["X-Model-Capabilities"])
	}
	if h["X-Model-Max-Context"] != "32768" {
		t.Errorf("X-Model-Max-Context: got %q", h["X-Model-Max-Context"])
	}
	if h["X-Model-Architecture"] != "qwen" {
		t.Errorf("X-Model-Architecture: got %q", h["X-Model-Architecture"])
	}
	if _, ok := h["X-Model-Vision"]; ok {
		t.Error("X-Model-Vision should not be set when Vision=false")
	}
}

func TestCapabilitiesHeadersEmpty(t *testing.T) {
	caps := ModelCapabilities{}
	h := caps.Headers()
	if h["X-Model-Capabilities"] != "none" {
		t.Errorf("X-Model-Capabilities: got %q, want %q", h["X-Model-Capabilities"], "none")
	}
	if _, ok := h["X-Model-Max-Context"]; ok {
		t.Error("X-Model-Max-Context should not be set when MaxContext=0")
	}
}

func TestCapabilitiesFromModelInfo(t *testing.T) {
	caps := CapabilitiesFromModelInfo(
		"Qwen3-4B-Instruct-2507",
		false, // reasoning: cppworker loaded it as instruct
		"qwen3",
		32768,
		262144, // gguf context length
	)
	if caps.Reasoning {
		t.Error("Reasoning should be false (instruct, no reasoning)")
	}
	if caps.MaxContext != 32768 {
		t.Errorf("MaxContext: got %d, want 32768", caps.MaxContext)
	}
	if caps.Architecture != "qwen3" {
		t.Errorf("Architecture: got %q, want %q", caps.Architecture, "qwen3")
	}
	if caps.Source != "cppworker-runtime" {
		t.Errorf("Source: got %q, want %q", caps.Source, "cppworker-runtime")
	}
}
