// handlers_model_r60_3_test.go — R60.3 (2026-09-04) tests for /api/models/files
// returning full per-file meta (size alias + quantization parsed from filename).
//
// БЕЗ этих полей webui gguf-renderer-detail.js:232-234 показывает карточку
// модели с пустыми "Размер: -" и "Квантизация: -", хотя файл виден.
// (Это и был production bug 2026-09-04.)
//go:build llama_stub

package main

import (
	"reflect"
	"testing"
	"time"
)

// TestParseQuantization — главный тест для parseQuantization.
// Покрывает все основные naming conventions llama.cpp + custom compressed aliases.
func TestParseQuantization(t *testing.T) {
	cases := []struct {
		filename string
		want     string
		comment  string
	}{
		// === Production case: R60 verification model ===
		{"Qwen3-Instruct-2507-q4km.gguf", "Q4_K_M", "Qwen3-2507 compressed alias (q4km)"},
		{"Qwen3-Instruct-2507-Q4_K_M.gguf", "Q4_K_M", "Qwen3-2507 full form"},

		// === Standard Q-quants ===
		{"Llama-3-8B-Q4_0.gguf", "Q4_0", "Q4_0 standard"},
		{"Llama-3-8B-Q4_1.gguf", "Q4_1", "Q4_1 standard"},
		{"Llama-3-8B-Q5_0.gguf", "Q5_0", "Q5_0 standard"},
		{"Llama-3-8B-Q5_1.gguf", "Q5_1", "Q5_1 standard"},
		{"Llama-3-8B-Q8_0.gguf", "Q8_0", "Q8_0 standard"},
		{"Llama-3-8B-Q2_K.gguf", "Q2_K", "Q2_K (no S/M/L)"},
		{"Llama-3-8B-Q3_K_S.gguf", "Q3_K_S", "Q3_K_S"},
		{"Llama-3-8B-Q3_K_M.gguf", "Q3_K_M", "Q3_K_M"},
		{"Llama-3-8B-Q3_K_L.gguf", "Q3_K_L", "Q3_K_L"},
		{"Llama-3-8B-Q4_K_S.gguf", "Q4_K_S", "Q4_K_S"},
		{"Llama-3-8B-Q4_K_M.gguf", "Q4_K_M", "Q4_K_M"},
		{"Llama-3-8B-Q5_K_S.gguf", "Q5_K_S", "Q5_K_S"},
		{"Llama-3-8B-Q5_K_M.gguf", "Q5_K_M", "Q5_K_M"},
		{"Llama-3-8B-Q6_K.gguf", "Q6_K", "Q6_K"},

		// === Compressed aliases (no underscore) ===
		{"Llama-3-8B-q4km.gguf", "Q4_K_M", "compressed q4km"},
		{"Llama-3-8B-q4ks.gguf", "Q4_K_S", "compressed q4ks"},
		{"Llama-3-8B-q5km.gguf", "Q5_K_M", "compressed q5km"},
		{"Llama-3-8B-q8_0.gguf", "Q8_0", "compressed q8_0 (lowercase)"},

		// === F-quants (unquantized) ===
		{"Llama-3-8B-f16.gguf", "F16", "F16"},
		{"Llama-3-8B-F16.gguf", "F16", "F16 uppercase"},
		{"Llama-3-8B-f32.gguf", "F32", "F32"},
		{"Llama-3-8B-bf16.gguf", "BF16", "BF16"},
		{"Llama-3-8B-fp16.gguf", "F16", "fp16 alias"},

		// === I-quants (llama.cpp IQ series) ===
		{"Llama-3-8B-IQ1_S.gguf", "IQ1_S", "IQ1_S"},
		{"Llama-3-8B-IQ2_XXS.gguf", "IQ2_XXS", "IQ2_XXS"},
		{"Llama-3-8B-IQ2_XS.gguf", "IQ2_XS", "IQ2_XS"},
		{"Llama-3-8B-IQ3_S.gguf", "IQ3_S", "IQ3_S"},
		{"Llama-3-8B-IQ4_XS.gguf", "IQ4_XS", "IQ4_XS"},
		{"Llama-3-8B-iq2xxs.gguf", "IQ2_XXS", "compressed iq2xxs"},

		// === Edge cases ===
		{"random-model.gguf", "", "no recognizable quantization"},
		{"model-v1.0.gguf", "", "version-like suffix, not a quant"},
		{"", "", "empty filename"},
		{".gguf", "", "just extension"},

		// === Path-like filenames ===
		{"/var/models/Llama-3-8B-Q4_K_M.gguf", "Q4_K_M", "absolute path"},
		{`C:\models\Llama-3-8B-Q4_K_M.gguf`, "Q4_K_M", "Windows path"},
	}

	for _, c := range cases {
		t.Run(c.filename, func(t *testing.T) {
			got := parseQuantization(c.filename)
			if got != c.want {
				t.Errorf("parseQuantization(%q) = %q, want %q (%s)",
					c.filename, got, c.want, c.comment)
			}
		})
	}
}

// TestGgufFileMeta — R60.3: ggufFileMeta() возвращает size + sizeBytes + quantization.
func TestGgufFileMeta(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	meta := ggufFileMeta("Qwen3-Instruct-2507-q4km.gguf", 2497281120, now)

	// Required keys (для совместимости с webui gguf-renderer-detail.js:232-234)
	if _, ok := meta["name"]; !ok {
		t.Errorf("missing 'name' key")
	}
	if _, ok := meta["size"]; !ok {
		t.Errorf("missing 'size' key (webui reads m.size)")
	}
	if _, ok := meta["sizeBytes"]; !ok {
		t.Errorf("missing 'sizeBytes' key (canonical name)")
	}
	if _, ok := meta["quantization"]; !ok {
		t.Errorf("missing 'quantization' key (webui reads m.quantization)")
	}
	if _, ok := meta["modifiedAt"]; !ok {
		t.Errorf("missing 'modifiedAt' key")
	}

	// Values
	if meta["name"] != "Qwen3-Instruct-2507-q4km.gguf" {
		t.Errorf("name = %v, want Qwen3-Instruct-2507-q4km.gguf", meta["name"])
	}
	if size, ok := meta["size"].(int64); !ok || size != 2497281120 {
		t.Errorf("size = %v (%T), want int64(2497281120)", meta["size"], meta["size"])
	}
	if sizeBytes, ok := meta["sizeBytes"].(int64); !ok || sizeBytes != 2497281120 {
		t.Errorf("sizeBytes = %v (%T), want int64(2497281120)", meta["sizeBytes"], meta["sizeBytes"])
	}
	if meta["quantization"] != "Q4_K_M" {
		t.Errorf("quantization = %v, want Q4_K_M", meta["quantization"])
	}
}

// TestParseQuantization_Stable — regression guard: ensure regex changes
// don't break known good filenames.
func TestParseQuantization_Stable(t *testing.T) {
	stable := map[string]string{
		"q4km":   "Q4_K_M",
		"q4ks":   "Q4_K_S",
		"q5km":   "Q5_K_M",
		"q5ks":   "Q5_K_S",
		"q6k":    "Q6_K",
		"q8_0":   "Q8_0",
		"Q4_K_M": "Q4_K_M",
		"f16":    "F16",
		"F32":    "F32",
		"bf16":   "BF16",
		"IQ4_XS": "IQ4_XS",
	}
	for input, want := range stable {
		t.Run(input, func(t *testing.T) {
			got := parseQuantization(input + ".gguf")
			if !reflect.DeepEqual(got, want) {
				t.Errorf("parseQuantization(%q) = %q, want %q", input, got, want)
			}
		})
	}
}

