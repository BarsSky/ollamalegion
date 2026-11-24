// roles.go — роль файла image-модели по ИМЕНИ (R696) и контрактные имена файлов
// bundle.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ (cgo-свободный) ПАКЕТ. Правила ролей нужны ДВУМ сторонам:
// imageworker (реестр моделей, синтез профиля, пред-проверка набора) и сервису
// ре-экспорта (`services/gguf-reexport`, цель image_bundle). Сервис не грузит
// llama.cpp вовсе и обязан собираться без cgo/сабмодуля — поэтому правила живут
// здесь, а `internal/sdbackend` реэкспортирует их прежними именами. Так правило
// остаётся ОДНО (эвристика не разъезжается), а сервис не тянет C-мост.
//
// Экспорт для сервиса ре-экспорта: ProfileFileName, ProbeLocalFile/ProbeHead,
// BuildBundleCompat, SuggestRole, IsImageWeightFile (см. probe*.go и compat.go).
package imagemodel

import (
	"path/filepath"
	"strings"

	"ollama-loadbalancer/pkg/types"
)

// ProfileFileName — имя файла описания bundle'а внутри каталога модели.
const ProfileFileName = "profile.json"

// Веса image-модели по расширению. Тот же фильтр применяют: синтез профиля,
// вердикт совместимости и раскладка набора по ролям.
var imageWeightExts = map[string]bool{
	".gguf": true, ".safetensors": true, ".ckpt": true, ".pt": true, ".sft": true,
}

// IsImageWeightFile — файл весов по расширению (экспорт фильтра синтеза: им же
// пользуется раскладка набора и HTTP-слой воркера).
func IsImageWeightFile(name string) bool {
	return imageWeightExts[strings.ToLower(filepath.Ext(name))]
}

// SuggestRole — предложенная роль файла по его ИМЕНИ (экспорт эвристики
// roleFromFilename).
//
// ЗАЧЕМ ЭКСПОРТ (R-Image Phase 9): страница «Image-модели» показывает роль
// рядом с каждым файлом в HF-поиске, чтобы оператор собирал bundle не вслепую
// (diffusion / vae / clip_l / clip_g / t5xxl / llm / taesd / lora / upscaler /
// controlnet / ip_adapter / clip_vision). Эвристика обязана жить в ОДНОМ месте:
// ту же функцию применяют при чтении готового bundle, и если UI начнёт угадывать
// роли сам, правила со временем разъедутся.
//
// Возвращает ImageFileRoleDiffusion, если ничего не подошло: для однофайловых
// SD1.5-репозиториев это ровно верный ответ, а в многофайловых (FLUX/SDXL)
// оператор роль поправит — но ПУСТОЙ роли не бывает, иначе UI показал бы
// «неизвестно» там, где движку нужна конкретная роль.
func SuggestRole(filename string) string {
	return roleFromFilename(filename)
}

// roleFromFilename — эвристика роли по имени файла.
//
// R-Image (2026-10-03): принимается ПУТЬ внутри репозитория, а не только имя
// файла. DiT-репозитории раскладывают компоненты по каталогам (`vae/...`,
// `text_encoders/...`, `split_files/...`), и по одному имени роль не угадать:
// `text_encoders/qwen3vl_8b_bf16.safetensors` — это LLM-энкодер Qwen-Image, а не
// diffusion-файл (раньше он и попадал в diffusion, из-за чего в bundle
// оказывалось два diffusion-файла и загрузка падала на дубликате роли).
func roleFromFilename(filename string) string {
	n := strings.ToLower(filename)
	// Каталог — сильный сигнал, но только когда он есть в пути.
	inTextEncoderDir := strings.Contains(n, "text_encoder") || strings.Contains(n, "text-encoder") ||
		strings.Contains(n, "/te/") || strings.HasPrefix(n, "te/")
	if inTextEncoderDir {
		switch {
		case strings.Contains(n, "clip_l") || strings.Contains(n, "clip-l"):
			return types.ImageFileRoleClipL
		case strings.Contains(n, "clip_g") || strings.Contains(n, "clip-g"):
			return types.ImageFileRoleClipG
		case strings.Contains(n, "t5"):
			return types.ImageFileRoleT5xxl
		case isLLMEncoderName(n):
			return types.ImageFileRoleLLM
		}
	}
	switch {
	case strings.Contains(n, "vae") || strings.Contains(n, "ae.safetensors"):
		return types.ImageFileRoleVae
	case strings.Contains(n, "clip_l") || strings.Contains(n, "clip-l"):
		return types.ImageFileRoleClipL
	case strings.Contains(n, "clip_g") || strings.Contains(n, "clip-g"):
		return types.ImageFileRoleClipG
	case strings.Contains(n, "t5"):
		return types.ImageFileRoleT5xxl
	case strings.Contains(n, "taesd"):
		return types.ImageFileRoleTaesd
	case strings.Contains(n, "clip_vision") || strings.Contains(n, "vision"):
		return types.ImageFileRoleClipVision
	case strings.Contains(n, "control"):
		return types.ImageFileRoleControlNet
	case strings.Contains(n, "ip-adapter") || strings.Contains(n, "ip_adapter"):
		return types.ImageFileRoleIPAdapter
	case strings.Contains(n, "lora"):
		return types.ImageFileRoleLora
	case strings.Contains(n, "esrgan") || strings.Contains(n, "upscal"):
		return types.ImageFileRoleUpscaler
	}
	return types.ImageFileRoleDiffusion
}

// isLLMEncoderName — имя похоже на LLM-энкодер (Qwen-Image/Z-Image/FLUX-LLM).
//
// ЗАЧЕМ ОТДЕЛЬНО И ТОЛЬКО В КАТАЛОГЕ text_encoders: «qwen» встречается и в имени
// самого diffusion-файла (qwen-image-2.1-UC-Q4_0.gguf), поэтому вне каталога
// энкодеров такое правило давало бы ложные срабатывания.
func isLLMEncoderName(n string) bool {
	for _, marker := range []string{"qwen", "gemma", "mistral", "llama", "deepseek", "phi", "llm", "3vl", "_vl_"} {
		if strings.Contains(n, marker) {
			return true
		}
	}
	return false
}
