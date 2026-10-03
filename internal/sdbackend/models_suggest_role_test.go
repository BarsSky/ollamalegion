// models_suggest_role_test.go — R-Image Phase 9 (2026-10-03): экспорт эвристики
// ролей файлов для UI.
//
// ЗАЧЕМ. Страница «Image-модели» (в стиле «GGUF модели») показывает роль рядом с
// каждым файлом в HF-поиске: без неё оператор собирает bundle вслепую и путает
// веса диффузии с VAE/text-encoder'ом. Эвристика обязана жить в ОДНОМ месте —
// та же функция применяется при чтении готового bundle, поэтому UI получает её
// с сервера (поле suggestedRole в /api/hf/files), а не дублирует правила.
//
// Тест фиксирует и «пустой роли не бывает»: для неизвестного имени возвращается
// diffusion. Это осознанное решение: движку нужна конкретная роль, а «неизвестно»
// в UI выглядело бы как ошибка конфигурации.
package sdbackend

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func TestSuggestRole(t *testing.T) {
	cases := []struct {
		filename string
		want     string
	}{
		// Реальные имена из HF-репозиториев (FLUX/SDXL/SD1.5/Z-Image).
		{"flux1-schnell-Q4_0.gguf", types.ImageFileRoleDiffusion},
		{"z_image_turbo-Q3_K.gguf", types.ImageFileRoleDiffusion},
		{"stable-diffusion-v1-5-pruned-emaonly-Q4_0.gguf", types.ImageFileRoleDiffusion},
		{"vae.safetensors", types.ImageFileRoleVae},
		{"ae.safetensors", types.ImageFileRoleVae},
		{"sdxl_vae.safetensors", types.ImageFileRoleVae},
		{"clip_l.safetensors", types.ImageFileRoleClipL},
		{"clip-l.safetensors", types.ImageFileRoleClipL},
		{"clip_g.safetensors", types.ImageFileRoleClipG},
		{"t5xxl-Q4_K_M.gguf", types.ImageFileRoleT5xxl},
		{"taesd.safetensors", types.ImageFileRoleTaesd},
		{"clip_vision.safetensors", types.ImageFileRoleClipVision},
		{"controlnet-canny.safetensors", types.ImageFileRoleControlNet},
		{"ip-adapter-plus.safetensors", types.ImageFileRoleIPAdapter},
		{"my-lora.safetensors", types.ImageFileRoleLora},
		{"RealESRGAN_x4plus.pth", types.ImageFileRoleUpscaler},
		{"4x-UltraSharp-upscaler.pth", types.ImageFileRoleUpscaler},
		// Регистр не важен: HF-имена приходят и в верхнем регистре.
		{"VAE.safetensors", types.ImageFileRoleVae},
		{"CLIP_L.safetensors", types.ImageFileRoleClipL},
		// Пустое/неизвестное имя: роль всё равно конкретная (diffusion),
		// иначе UI показал бы «неизвестно» там, где движку нужна роль.
		{"", types.ImageFileRoleDiffusion},
		{"weights.bin", types.ImageFileRoleDiffusion},
	}
	for _, c := range cases {
		if got := SuggestRole(c.filename); got != c.want {
			t.Errorf("SuggestRole(%q) = %q, want %q", c.filename, got, c.want)
		}
	}
}
