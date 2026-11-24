// hf_probe_role_test.go — R696 (2026-11-23): роль файла по СОДЕРЖИМОМУ заголовка.
//
// ЖИВОЙ ДЕФЕКТ, который стерегут эти проверки: на локальном стенде оператор
// скачал на вкладке HuggingFace файл
// `Qwen-Image-2.1-Turbo-Uncensored-AD-Q4_K.gguf` (репозиторий
// AtomicChat/Qwen-Image-2.1-Turbo-Uncensored-GGUF). Имя выглядит как
// diffusion-модель, а в заголовке — general.architecture="qwen3vl",
// general.size_label="8.2B", теги image-text-to-text: это ТЕКСТОВЫЙ ЭНКОДЕР
// Qwen-Image 2.1. Воркер подключил его как --model, движок ответил
// «get sd version from file failed», и оператор увидел только «модель не
// загрузилась».
package imagemodel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProbeHead_TextEncoderGGUF_IsLLMRole — тот самый файл: arch qwen3vl, тензоры
// языковой модели. Вердикт «diffusion не опознан» остаётся честным (unknown), но
// роль обязана быть llm — по ней валидатор скажет «нужен diffusion-файл».
func TestProbeHead_TextEncoderGGUF_IsLLMRole(t *testing.T) {
	raw := buildGGUF(t, []ggufKV{
		{"general.architecture", "qwen3vl"},
		{"general.size_label", "8.2B"},
	}, names(
		"model.embed_tokens.weight",
		"model.layers.0.self_attn.q_proj.weight",
		"model.norm.weight",
	))
	res := probeHead(raw, "Qwen-Image-2.1-Turbo-Uncensored-AD-Q4_K.gguf")
	if res.Verdict != HFProbeUnknown {
		t.Fatalf("verdict=%q, want unknown (diffusion-якорей нет)", res.Verdict)
	}
	if res.Role != ProbeRoleLLM {
		t.Fatalf("role=%q, want llm (это текстовый энкодер, а не diffusion)", res.Role)
	}
	if !strings.Contains(res.Reason, "llm") || !strings.Contains(res.Reason, "энкодер") {
		t.Fatalf("причина должна говорить «текстовый энкодер» и роль llm: %s", res.Reason)
	}
	if res.Arch != "qwen3vl" {
		t.Fatalf("arch=%q, want qwen3vl", res.Arch)
	}
}

// TestProbeHead_TextEncoderByTensors — arch может отсутствовать (safetensors):
// роль определяется по именам тензоров (model.embed_tokens + model.layers).
func TestProbeHead_TextEncoderByTensors(t *testing.T) {
	raw := buildSafetensors(t, nil, []string{
		"model.embed_tokens.weight",
		"model.layers.0.self_attn.q_proj.weight",
		"model.layers.0.mlp.up_proj.weight",
	})
	res := probeHead(raw, "text_encoders/qwen3vl_8b_bf16.safetensors")
	if res.Role != ProbeRoleLLM {
		t.Fatalf("role=%q, want llm", res.Role)
	}
}

// TestProbeHead_VaeByTensors — VAE-файл (decoder/encoder/quant_conv) не должен
// считаться diffusion-моделью.
func TestProbeHead_VaeByTensors(t *testing.T) {
	raw := buildSafetensors(t, nil, []string{
		"decoder.conv_in.weight",
		"encoder.conv_in.weight",
		"quant_conv.weight",
		"post_quant_conv.weight",
	})
	res := probeHead(raw, "vae/qwen_image_2.1_vae_bf16.safetensors")
	if res.Role != ProbeRoleVae {
		t.Fatalf("role=%q, want vae", res.Role)
	}
}

// TestProbeHead_T5ByArch — текстовый энкодер T5 (arch "t5").
func TestProbeHead_T5ByArch(t *testing.T) {
	raw := buildGGUF(t, []ggufKV{{"general.architecture", "t5"}}, names(
		"encoder.block.0.layer.0.SelfAttention.q.weight",
	))
	res := probeHead(raw, "t5-v1_1-xxl-encoder-Q4_K_M.gguf")
	if res.Role != ProbeRoleT5xxl {
		t.Fatalf("role=%q, want t5xxl", res.Role)
	}
}

// TestProbeHead_DiffusionStaysDiffusion — настоящий diffusion-файл (якоря
// семейства) обязан остаться diffusion, даже если в имени есть «vae»/«text».
func TestProbeHead_DiffusionStaysDiffusion(t *testing.T) {
	raw := buildGGUF(t, nil, names(
		"img_in.weight",
		"txt_in.text_norm.weight",
		"transformer_blocks.0.attn.to_q.weight",
	))
	res := probeHead(raw, "qwen-image-2.1-UC-Q4_K_M.gguf")
	if res.Role != ProbeRoleDiffusion {
		t.Fatalf("role=%q, want diffusion (%s)", res.Role, res.Reason)
	}
	if res.Family != "qwen_image" || !res.DiT {
		t.Fatalf("family=%q dit=%v, want qwen_image/true", res.Family, res.DiT)
	}
	if res.FamilySource != ProbeFamilyFromTensors {
		t.Fatalf("familySource=%q, want tensors", res.FamilySource)
	}
}

// TestProbeHead_ArchFamilyIsHintOnly — семейство из general.architecture
// помечается как подсказка (движок её не читает, docs §8.2).
func TestProbeHead_ArchFamilyIsHintOnly(t *testing.T) {
	raw := buildGGUF(t, []ggufKV{{"general.architecture", "qwen_image21"}}, nil)
	res := probeHead(raw, "qwen-image-2.1-Q4_0.gguf")
	if res.Family != "qwen_image" || !res.DiT {
		t.Fatalf("family=%q dit=%v, want qwen_image/true", res.Family, res.DiT)
	}
	if res.FamilySource != ProbeFamilyFromArch {
		t.Fatalf("familySource=%q, want arch", res.FamilySource)
	}
}

// TestProbeLocalFile_ReadsDiskHeader — локальный файл читается только по
// заголовку (веса не нужны): 512 КБ + хвост-«веса».
func TestProbeLocalFile_ReadsDiskHeader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "te.gguf")
	raw := buildGGUF(t, []ggufKV{{"general.architecture", "qwen3vl"}}, names(
		"model.embed_tokens.weight",
		"model.layers.0.self_attn.q_proj.weight",
	))
	raw = append(raw, make([]byte, 4096)...) // «веса» — не читаются
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	res, err := ProbeLocalFile(path, "te.gguf")
	if err != nil {
		t.Fatalf("ProbeLocalFile: %v", err)
	}
	if res.Format != "gguf" || res.Role != ProbeRoleLLM {
		t.Fatalf("format=%q role=%q, want gguf/llm", res.Format, res.Role)
	}
	if res.CheckedB > HFProbeRangeBytes+1 {
		t.Fatalf("прочитано %d байт, want только заголовок (<= %d)", res.CheckedB, HFProbeRangeBytes)
	}
}
