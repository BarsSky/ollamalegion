package types

import (
	"encoding/json"
	"testing"
)

// TestAPIStyleConstants — sanity-check для констант APIStyle.
// Если кто-то случайно изменит значение константы, тест зажжётся — это breaking
// change для state.json и config.json (round-trip ломается).
func TestAPIStyleConstants(t *testing.T) {
	tests := []struct {
		name string
		got  APIStyle
		want string
	}{
		{"ollama-native", APIStyleOllamaNative, "ollama-native"},
		{"openai-compatible", APIStyleOpenAICompatible, "openai-compatible"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if string(tt.got) != tt.want {
				t.Errorf("APIStyle constant value changed: got %q, want %q", string(tt.got), tt.want)
			}
		})
	}
}

// TestAPIStyle_IsValidAPIStyle — проверка валидатора стиля.
func TestAPIStyle_IsValidAPIStyle(t *testing.T) {
	tests := []struct {
		name  string
		style APIStyle
		want  bool
	}{
		{"ollama-native valid", APIStyleOllamaNative, true},
		{"openai-compatible valid", APIStyleOpenAICompatible, true},
		{"empty invalid", APIStyle(""), false},
		{"unknown invalid", APIStyle("openai"), false},
		{"case-sensitive: OpenAI-Compatible invalid", APIStyle("OpenAI-Compatible"), false},
		{"garbage invalid", APIStyle("foo-bar"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.style.IsValidAPIStyle(); got != tt.want {
				t.Errorf("APIStyle(%q).IsValidAPIStyle() = %v, want %v", string(tt.style), got, tt.want)
			}
		})
	}
}

// TestBackend_EffectiveAPIStyle — табличный тест главной логики helper'а.
// Round 51.2 (2026-08-20): priority order — explicit field > inference from Type > safe default.
func TestBackend_EffectiveAPIStyle(t *testing.T) {
	tests := []struct {
		name string
		be   *Backend
		want APIStyle
	}{
		{
			name: "nil receiver safe default",
			be:   nil,
			want: APIStyleOllamaNative,
		},
		{
			name: "explicit ollama-native overrides Type=llama_cpp",
			be:   &Backend{Type: BackendTypeLlamaCpp, ApiStyle: APIStyleOllamaNative},
			want: APIStyleOllamaNative,
		},
		{
			name: "explicit openai-compatible overrides Type=ollama",
			be:   &Backend{Type: BackendTypeOllama, ApiStyle: APIStyleOpenAICompatible},
			want: APIStyleOpenAICompatible,
		},
		{
			name: "empty ApiStyle + Type=llama_cpp → openai-compatible (R50 default)",
			be:   &Backend{Type: BackendTypeLlamaCpp, ApiStyle: ""},
			want: APIStyleOpenAICompatible,
		},
		{
			name: "empty ApiStyle + Type=ollama → ollama-native (R50 default)",
			be:   &Backend{Type: BackendTypeOllama, ApiStyle: ""},
			want: APIStyleOllamaNative,
		},
		{
			name: "empty ApiStyle + empty Type → ollama-native (safe default)",
			be:   &Backend{Type: "", ApiStyle: ""},
			want: APIStyleOllamaNative,
		},
		{
			name: "invalid ApiStyle (typo) + Type=llama_cpp → fallback to inference",
			be:   &Backend{Type: BackendTypeLlamaCpp, ApiStyle: APIStyle("openai")},
			want: APIStyleOpenAICompatible,
		},
		{
			name: "invalid ApiStyle (typo) + Type=ollama → fallback to ollama-native",
			be:   &Backend{Type: BackendTypeOllama, ApiStyle: APIStyle("olama")},
			want: APIStyleOllamaNative,
		},
		{
			name: "unknown Type + explicit ollama-native → ollama-native (explicit wins)",
			be:   &Backend{Type: BackendType("vllm"), ApiStyle: APIStyleOllamaNative},
			want: APIStyleOllamaNative,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.be.EffectiveAPIStyle(); got != tt.want {
				t.Errorf("Backend.EffectiveAPIStyle() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestBackend_ApiStyleJSONRoundTrip — round-trip через encoding/json.
// State.json и config.json используют json.Marshal/Unmarshal для Backend;
// изменение JSON-тега (apiStyle vs ApiStyle) — breaking change для всех
// существующих state.json на диске.
func TestBackend_ApiStyleJSONRoundTrip(t *testing.T) {
	original := &Backend{
		ID:       "test-1",
		Type:     BackendTypeLlamaCpp,
		ApiStyle: APIStyleOllamaNative,
	}

	// Marshal
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// Должно содержать "apiStyle" в camelCase
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	got, ok := m["apiStyle"]
	if !ok {
		t.Errorf("expected key 'apiStyle' in JSON, got keys: %v", keys(m))
	}
	if got != "ollama-native" {
		t.Errorf("apiStyle = %v, want 'ollama-native'", got)
	}

	// Round-trip
	var decoded Backend
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.ApiStyle != original.ApiStyle {
		t.Errorf("round-trip ApiStyle = %q, want %q", decoded.ApiStyle, original.ApiStyle)
	}
	if decoded.Type != original.Type {
		t.Errorf("round-trip Type = %q, want %q", decoded.Type, original.Type)
	}
}

// TestBackend_ApiStyleJSONOmitEmpty — пустой ApiStyle не должен попадать в JSON.
// Backend(ApiStyle="") → JSON без ключа "apiStyle" (omitempty).
// Это важно: state.json обратно совместим с R50 (где этого поля не было).
func TestBackend_ApiStyleJSONOmitEmpty(t *testing.T) {
	original := &Backend{
		ID:   "test-2",
		Type: BackendTypeOllama,
		// ApiStyle не задан
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	if _, ok := m["apiStyle"]; ok {
		t.Errorf("expected key 'apiStyle' to be omitted when empty, got: %v", keys(m))
	}
}

// keys — helper для печати ключей map в сообщениях об ошибке.
func keys(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
