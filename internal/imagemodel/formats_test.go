// formats_test.go — R755 (2026-11-25): матрица форматов «что движок читает».
//
// ЧТО ПРОВЕРЯЕТСЯ:
//   - GGUF читается в любой роли у любого семейства; .sft — тоже;
//   - safetensors-КОМПОНЕНТЫ (VAE, энкодеры, taesd, LoRA) читаются всегда;
//   - safetensors-DIFFUSION: SD1.5/SDXL (all-in-one) — читается, Qwen-Image —
//     нужен GGUF, прочие DiT — warn (решает заголовок), неизвестное семейство —
//     warn;
//   - имя с «mlx» даёт unsupported-предупреждение (MLX-квант U32), а не молчание;
//   - неизвестное расширение — unknown (не «читается»).
package imagemodel

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func TestBuildFormatMatrix_DiffusionByFamily_R755(t *testing.T) {
	cases := []struct {
		family  string
		verdict string
	}{
		{"sd15", FormatReads},
		{"sd21", FormatReads},
		{"sdxl", FormatReads},
		{"sdxl_turbo", FormatReads},
		{"qwen_image", FormatNeedsGGUF},
		{"flux", FormatWarn},
		{"flux2", FormatWarn},
		{"sd3", FormatWarn},
		{"z_image", FormatWarn},
		{"chroma", FormatWarn},
		{"other", FormatWarn},
		{"", FormatWarn},
		{"wan", FormatWarn},
	}
	for _, tc := range cases {
		m := BuildFormatMatrix(tc.family)
		if got := m.ResolveFormat("model.safetensors", types.ImageFileRoleDiffusion); got != tc.verdict {
			t.Errorf("family %q: safetensors diffusion = %q, want %q", tc.family, got, tc.verdict)
		}
		// GGUF-диффузия читается ВСЕГДА — включая qwen_image.
		if got := m.ResolveFormat("model.gguf", types.ImageFileRoleDiffusion); got != FormatReads {
			t.Errorf("family %q: gguf diffusion = %q, want reads", tc.family, got)
		}
	}
	// Флаги семейства: all-in-one ≠ DiT.
	if m := BuildFormatMatrix("sdxl"); !m.AllInOne || m.DIT || !m.FamilyKnown {
		t.Fatalf("sdxl: allInOne=%v dit=%v known=%v", m.AllInOne, m.DIT, m.FamilyKnown)
	}
	if m := BuildFormatMatrix("qwen_image"); m.AllInOne || !m.DIT || !m.FamilyKnown {
		t.Fatalf("qwen_image: allInOne=%v dit=%v known=%v", m.AllInOne, m.DIT, m.FamilyKnown)
	}
	if m := BuildFormatMatrix("other"); m.AllInOne || m.DIT || m.FamilyKnown {
		t.Fatalf("other: allInOne=%v dit=%v known=%v — обещать нечего", m.AllInOne, m.DIT, m.FamilyKnown)
	}
}

func TestBuildFormatMatrix_ComponentsAndContainers_R755(t *testing.T) {
	qwen := BuildFormatMatrix("qwen_image")
	components := []string{
		types.ImageFileRoleVae, types.ImageFileRoleLLM, types.ImageFileRoleClipL,
		types.ImageFileRoleClipG, types.ImageFileRoleT5xxl, types.ImageFileRoleClipVision,
		types.ImageFileRoleTaesd, types.ImageFileRoleLora, types.ImageFileRoleUpscaler,
		types.ImageFileRoleControlNet, types.ImageFileRoleIPAdapter,
	}
	for _, role := range components {
		for _, file := range []string{"x.safetensors", "x.gguf", "x.sft"} {
			if got := qwen.ResolveFormat(file, role); got != FormatReads {
				t.Errorf("qwen_image/%s/%s = %q, want reads", role, file, got)
			}
		}
	}
	// .sft — контейнер движка: читается и как diffusion.
	if got := qwen.ResolveFormat("model.sft", types.ImageFileRoleDiffusion); got != FormatReads {
		t.Fatalf(".sft diffusion = %q, want reads", got)
	}
	// Неизвестное расширение не обещает чтение.
	if got := qwen.ResolveFormat("model.bin", types.ImageFileRoleDiffusion); got != FormatUnknown {
		t.Fatalf("bin = %q, want unknown", got)
	}
	if got := qwen.ResolveFormat("noext", types.ImageFileRoleVae); got != FormatUnknown {
		t.Fatalf("no extension = %q, want unknown", got)
	}
	// Регистр расширения и пути не важен.
	if got := qwen.ResolveFormat("vae/AE.SafeTensors", types.ImageFileRoleVae); got != FormatReads {
		t.Fatalf("uppercase path = %q, want reads", got)
	}
}

func TestFormatMatrix_MLXNameWarning_R755(t *testing.T) {
	m := BuildFormatMatrix("qwen_image")
	w := m.NameWarningFor("qwen-image-2.1-UC-MLX-4bit.safetensors")
	if w == nil {
		t.Fatal("MLX-файл не получил предупреждение по имени")
	}
	if w.Verdict != FormatUnsupported || w.Code != "mlx_quantized" {
		t.Fatalf("warning = %+v, want unsupported/mlx_quantized", *w)
	}
	if w2 := m.NameWarningFor("qwen-image-Q4_K_M.gguf"); w2 != nil {
		t.Fatalf("обычный GGUF получил MLX-предупреждение: %+v", *w2)
	}
	if w3 := m.NameWarningFor("vae/ae.safetensors"); w3 != nil {
		t.Fatalf("VAE получил MLX-предупреждение: %+v", *w3)
	}
}
