package types

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// R-Image (2026-09-27): контракт image-моделей (движок stable-diffusion.cpp).
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ФАЙЛ: одна диффузионная модель — это НАБОР файлов
// (diffusion/UNet + VAE + text encoder'ы + опционально TAESD/ControlNet/IP-Adapter),
// а не один GGUF. Поэтому «модель» описывается профилем (bundle), который:
//   - знает состав файлов и их роли (роль → флаг sd-server);
//   - хранит дефолты генерации (steps/cfg/sampler/size/batch/negative);
//   - хранит runtime-плейсмент и offload (--backend / --params-backend /
//     --max-vram / --vae-tiling / --taesd / ...);
//   - умеет собрать argv для sd-server (ServerArgs) — этим пользуется
//     супервизор субпроцесса в cmd/sdworker.
//
// ФАЙЛ ЯВЛЯЕТСЯ ЗАМОРОЖЕННЫМ КОНТРАКТОМ: он общий для слоя моделей/HF
// (internal/api, internal/cppbackend) и для воркера (cmd/sdworker,
// internal/sdbackend). Менять его — только синхронно, с обеих сторон.
//
// Обоснование решений (флаги, лимиты, подводные камни) —
// docs/research-sdcpp-lowvram-integration.md, план —
// plans/2026-09-27-image-generation-backend-plan.md §5.4–5.6, §12.

// Роли файлов внутри bundle. Значение роли определяет флаг sd-server
// (см. (p *ImageModelProfile) ServerArgs) и требование обязательности.
const (
	ImageFileRoleDiffusion  = "diffusion"   // UNet/DiT, либо all-in-one чекпойнт
	ImageFileRoleVae        = "vae"         // VAE (для FLUX/SD3/Qwen/Z-Image — отдельно)
	ImageFileRoleClipL      = "clip_l"      // CLIP-L text encoder
	ImageFileRoleClipG      = "clip_g"      // CLIP-G text encoder (SDXL/SD3)
	ImageFileRoleT5xxl      = "t5xxl"       // T5-XXL (FLUX/SD3) — только квантованный
	ImageFileRoleLLM        = "llm"         // LLM-энкодер (Qwen-Image, Z-Image, FLUX.2)
	ImageFileRoleClipVision = "clip_vision" // CLIP-vision (IP-Adapter, PhotoMaker)
	ImageFileRoleTaesd      = "taesd"       // tiny autoencoder (быстрый decode, меньше VRAM)
	ImageFileRoleLora       = "lora"        // LoRA (применяется per-request как lora[])
	ImageFileRoleUpscaler   = "upscaler"    // ESRGAN-апскейлер
	ImageFileRoleControlNet = "controlnet"  // ControlNet (SD1.5)
	ImageFileRoleIPAdapter  = "ip_adapter"  // IP-Adapter
)

// ImageModelFamilies — поддерживаемые семейства (значения profile.Family).
// Влияет на дефолты генерации и на проверку обязательных ролей.
var ImageModelFamilies = []string{
	"sd15", "sd21", "sd_turbo", "sdxl", "sdxl_turbo", "sd3",
	"flux", "flux2", "chroma", "qwen_image", "z_image", "other",
}

// diTFamilies — семейства, у которых diffusion/VAE/text-encoder лежат
// ОТДЕЛЬНЫМИ файлами (в отличие от all-in-one чекпойнтов SD1.x/2.x/SDXL).
var diTFamilies = map[string]bool{
	"sd3": true, "flux": true, "flux2": true,
	"chroma": true, "qwen_image": true, "z_image": true,
}

// PinnedSDServerRevision — версия sd-server, под которую написан контракт.
//
// Релизы stable-diffusion.cpp выходят по нескольку раз в день, а имена флагов
// менялись (например --host/--port → --listen-ip/--listen-port после
// master-600). Пиновать обязательно; расхождение — повод для contract-теста,
// а не для «тихой» деградации.
const PinnedSDServerRevision = "master-929-3f8527a"

// ImageModelFile — один файл модели на HuggingFace.
type ImageModelFile struct {
	Role      string `json:"role"`                // см. ImageFileRole*
	Repo      string `json:"repo"`                // HF repo id, например leejet/Z-Image-Turbo-GGUF
	Filename  string `json:"filename"`            // имя файла внутри репозитория
	Revision  string `json:"revision,omitempty"`  // default: main
	SizeBytes int64  `json:"sizeBytes,omitempty"` // для прогресса/оценки VRAM (0 = неизвестно)
	// LocalPath — путь к скачанному файлу (заполняется после bundle-загрузки).
	LocalPath string `json:"localPath,omitempty"`
}

// ImageGenDefaults — дефолты генерации для модели (пользователь/пресет).
type ImageGenDefaults struct {
	Steps          int     `json:"steps"`
	CFGScale       float64 `json:"cfgScale"`
	Sampler        string  `json:"sampler"`   // euler, euler_a, dpm++2m, lcm, ddim_trailing ...
	Scheduler      string  `json:"scheduler"` // discrete, karras, exponential, ays, gits, smoothstep
	Width          int     `json:"width"`
	Height         int     `json:"height"`
	BatchCount     int     `json:"batchCount"`
	NegativePrompt string  `json:"negativePrompt,omitempty"`
	Seed           int64   `json:"seed"`     // -1 = случайный (движку НЕ передаём -1: см. Runtime.SeedMode)
	ClipSkip       int     `json:"clipSkip"` // 0/1 = не передавать флаг вовсе
}

// ImageRuntime — плейсмент и экономия памяти (флаги sd-server).
//
// Ключевые правила (из исследования, §5.6 плана):
//   - VaeTiling ВСЕГДА предпочтительнее VaeOnCPU: --vae-on-cpu даёт штраф ~5x
//     (RTX 3060: 29 с → 150 с). Поэтому отдельного поля VaeOnCPU здесь нет —
//     при необходимости выражается через Backend ("vae=cpu").
//   - VaeTileSize — В ПИКСЕЛЯХ ИЗОБРАЖЕНИЯ (не в латентах!).
//   - OffloadToCPU = шорткат "--params-backend '*=cpu'"; одновременно с
//     ParamsBackend не передаём (ParamsBackend имеет приоритет и отключает
//     --auto-fit).
type ImageRuntime struct {
	Backend       string `json:"backend,omitempty"`       // "te=cpu,vae=cuda0,diffusion=vulkan0"
	ParamsBackend string `json:"paramsBackend,omitempty"` // "cpu" | "disk" | "diffusion=disk"
	MaxVRAM       string `json:"maxVram,omitempty"`       // "6" | "-1" | "cuda0=6,vulkan0=2"
	OffloadToCPU  bool   `json:"offloadToCpu,omitempty"`
	AutoFit       string `json:"autoFit,omitempty"` // on|off
	DiffusionFA   bool   `json:"diffusionFa,omitempty"`
	VaeTiling     bool   `json:"vaeTiling,omitempty"`
	VaeTileSize   int    `json:"vaeTileSize,omitempty"` // пиксели изображения, 0 = дефолт движка
	VaeConvDirect bool   `json:"vaeConvDirect,omitempty"`
	Taesd         bool   `json:"taesd,omitempty"`
	SplitMode     string `json:"splitMode,omitempty"` // layer|row
	NGpuLayers    int    `json:"nGpuLayers,omitempty"`
	Threads       int    `json:"threads,omitempty"`
	// SeedMode: "random" (по умолчанию) — воркер резолвит seed в положительное
	// число на каждый запрос; "fixed" — использовать Defaults.Seed как есть.
	// ЗАЧЕМ: OpenAI-хендлер sd-server не читает поле seed вообще и берёт
	// default_gen_params.seed = 42, поэтому без нашей подстановки ВСЕ картинки
	// получаются одинаковыми (см. план §12.3, ловушка №1).
	SeedMode  string   `json:"seedMode,omitempty"`
	ExtraArgs []string `json:"extraArgs,omitempty"` // дополнительные флаги (оператор)
}

// ImageModelProfile — одна image-модель (bundle).
type ImageModelProfile struct {
	Name     string           `json:"name"`   // sdxl-turbo-q8
	Family   string           `json:"family"` // см. ImageModelFamilies
	Files    []ImageModelFile `json:"files"`
	Defaults ImageGenDefaults `json:"defaults"`
	Runtime  ImageRuntime     `json:"runtime"`
	// VramEstimateMB — оценка пиковой VRAM (0 = неизвестно). Используется
	// гейтом «не влезает» на балансере (Phase 6).
	VramEstimateMB int `json:"vramEstimateMb,omitempty"`
	// TimeoutSec — профильный таймаут генерации (0 = дефолт по семейству).
	TimeoutSec int `json:"timeoutSec,omitempty"`
	// IdleUnloadMinutes — выгрузка простаивающей модели (0 = не выгружать).
	// Для image-воркера «выгрузка» = остановка субпроцесса sd-server.
	IdleUnloadMinutes int    `json:"idleUnloadMinutes,omitempty"`
	Disabled          bool   `json:"disabled,omitempty"`
	Notes             string `json:"notes,omitempty"`
}

// IsValidImageFamily — известное ли семейство (неизвестное → "other").
func IsValidImageFamily(family string) bool {
	for _, f := range ImageModelFamilies {
		if f == family {
			return true
		}
	}
	return false
}

// IsDiTFamily — true, если модель требует отдельных файлов VAE/text-encoder.
func IsDiTFamily(family string) bool {
	return diTFamilies[family]
}

// DefaultImageGenDefaults — рекомендованные дефолты по семейству.
//
// Значения из docs/research-sdcpp-lowvram-integration.md §1.2 (рекомендации
// проекта sd.cpp) и §6 плана. Для distilled-моделей cfg = 1.0 обязательно.
func DefaultImageGenDefaults(family string) ImageGenDefaults {
	base := ImageGenDefaults{
		Steps:      20,
		CFGScale:   7.0,
		Sampler:    "euler_a",
		Scheduler:  "discrete",
		Width:      512,
		Height:     512,
		BatchCount: 1,
		Seed:       -1,
		ClipSkip:   0,
	}
	switch family {
	case "sd_turbo", "sdxl_turbo":
		base.Steps = 4
		base.CFGScale = 1.0
		base.Sampler = "euler"
		if family == "sdxl_turbo" {
			base.Width, base.Height = 512, 512
		}
	case "sd15", "sd21":
		base.Steps = 25
	case "sdxl":
		base.Steps = 25
		base.Width, base.Height = 1024, 1024
	case "sd3":
		base.Steps = 28
		base.CFGScale = 4.5
		base.Sampler = "euler"
		base.Width, base.Height = 1024, 1024
	case "flux", "flux2":
		base.Steps = 4
		base.CFGScale = 1.0
		base.Sampler = "euler"
		base.Width, base.Height = 1024, 1024
	case "chroma":
		base.CFGScale = 4.0
		base.Sampler = "euler"
		base.Width, base.Height = 1024, 1024
	case "qwen_image":
		base.Steps = 25
		base.CFGScale = 2.5
		base.Sampler = "euler"
		base.Width, base.Height = 1024, 1024
	case "z_image":
		// Z-Image-Turbo: 8 шагов, cfg 1.0 (рецепт для 4 GB — с offload).
		base.Steps = 8
		base.CFGScale = 1.0
		base.Sampler = "euler"
		base.Scheduler = "smoothstep"
		base.Width, base.Height = 512, 1024
	}
	return base
}

// ValidateImageModelProfile — границы, которые ДВИЖОК НЕ ПРОВЕРЯЕТ.
//
// Критично: sd-server на HTTP-уровне не валидирует width/height вообще
// (validate() не смотрит на них), batch_count и steps клампятся МОЛЧА
// (до 8 и до 100). Поэтому проверяем на нашей стороне, до отправки в движок.
func ValidateImageModelProfile(p *ImageModelProfile) error {
	if p == nil {
		return fmt.Errorf("profile is nil")
	}
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if p.Family != "" && !IsValidImageFamily(p.Family) {
		return fmt.Errorf("unknown family %q (allowed: %s)", p.Family, strings.Join(ImageModelFamilies, ", "))
	}
	if len(p.Files) == 0 {
		return fmt.Errorf("files is required: a diffusion bundle must contain at least one file")
	}

	roles := make(map[string]bool, len(p.Files))
	for i, f := range p.Files {
		if strings.TrimSpace(f.Role) == "" {
			return fmt.Errorf("files[%d]: role is required", i)
		}
		if !isValidImageFileRole(f.Role) {
			return fmt.Errorf("files[%d]: unknown role %q", i, f.Role)
		}
		if strings.TrimSpace(f.Repo) == "" || strings.TrimSpace(f.Filename) == "" {
			return fmt.Errorf("files[%d] (%s): repo and filename are required", i, f.Role)
		}
		// Одна роль — один файл (кроме LoRA, которых может быть много).
		if roles[f.Role] && f.Role != ImageFileRoleLora {
			return fmt.Errorf("files[%d]: duplicate role %q", i, f.Role)
		}
		roles[f.Role] = true
	}
	if !roles[ImageFileRoleDiffusion] {
		return fmt.Errorf("bundle must contain a %q file", ImageFileRoleDiffusion)
	}
	// Для DiT-семейств VAE отдельным файлом обязателен.
	if IsDiTFamily(p.Family) && !roles[ImageFileRoleVae] {
		return fmt.Errorf("family %q requires a separate %q file", p.Family, ImageFileRoleVae)
	}

	d := p.Defaults
	if d.Steps < 1 || d.Steps > 100 {
		return fmt.Errorf("defaults.steps must be in [1,100], got %d", d.Steps)
	}
	if d.CFGScale < 0 || d.CFGScale > 30 {
		return fmt.Errorf("defaults.cfgScale must be in [0,30], got %v", d.CFGScale)
	}
	for _, wh := range []struct {
		name string
		v    int
	}{{"width", d.Width}, {"height", d.Height}} {
		if wh.v < 64 || wh.v > 4096 {
			return fmt.Errorf("defaults.%s must be in [64,4096], got %d", wh.name, wh.v)
		}
		if wh.v%64 != 0 {
			return fmt.Errorf("defaults.%s must be a multiple of 64, got %d", wh.name, wh.v)
		}
	}
	if d.BatchCount < 1 || d.BatchCount > 8 {
		return fmt.Errorf("defaults.batchCount must be in [1,8], got %d", d.BatchCount)
	}
	if d.Seed < -1 {
		return fmt.Errorf("defaults.seed must be >= -1, got %d", d.Seed)
	}

	r := p.Runtime
	if r.NGpuLayers < -1 || r.NGpuLayers > 999 {
		return fmt.Errorf("runtime.nGpuLayers must be in [-1,999], got %d", r.NGpuLayers)
	}
	if r.AutoFit != "" && r.AutoFit != "on" && r.AutoFit != "off" {
		return fmt.Errorf("runtime.autoFit must be on|off, got %q", r.AutoFit)
	}
	if r.SplitMode != "" && r.SplitMode != "layer" && r.SplitMode != "row" {
		return fmt.Errorf("runtime.splitMode must be layer|row, got %q", r.SplitMode)
	}
	if r.VaeTileSize < 0 {
		return fmt.Errorf("runtime.vaeTileSize must be >= 0 (pixels), got %d", r.VaeTileSize)
	}
	if r.SeedMode != "" && r.SeedMode != "random" && r.SeedMode != "fixed" {
		return fmt.Errorf("runtime.seedMode must be random|fixed, got %q", r.SeedMode)
	}
	// OffloadToCPU и ParamsBackend — взаимоисключающие (ParamsBackend отключает
	// --auto-fit, а OffloadToCPU — его шорткат '*=cpu').
	if r.OffloadToCPU && r.ParamsBackend != "" {
		return fmt.Errorf("runtime: offloadToCpu and paramsBackend are mutually exclusive")
	}
	if p.TimeoutSec < 0 {
		return fmt.Errorf("timeoutSec must be >= 0, got %d", p.TimeoutSec)
	}
	if p.IdleUnloadMinutes < 0 {
		return fmt.Errorf("idleUnloadMinutes must be >= 0, got %d", p.IdleUnloadMinutes)
	}
	return nil
}

func isValidImageFileRole(role string) bool {
	switch role {
	case ImageFileRoleDiffusion, ImageFileRoleVae, ImageFileRoleClipL, ImageFileRoleClipG,
		ImageFileRoleT5xxl, ImageFileRoleLLM, ImageFileRoleClipVision, ImageFileRoleTaesd,
		ImageFileRoleLora, ImageFileRoleUpscaler, ImageFileRoleControlNet, ImageFileRoleIPAdapter:
		return true
	}
	return false
}

// BundleDir — каталог, куда складываются файлы этого bundle'а.
// Схема: <modelsDir>/<name>/ — один каталог на модель (атомарная регистрация:
// неполный bundle не должен выглядеть рабочей моделью).
func (p *ImageModelProfile) BundleDir(modelsDir string) string {
	if p == nil {
		return modelsDir
	}
	return filepath.Join(modelsDir, p.Name)
}

// SeedIsRandom — нужно ли резолвить seed на каждый запрос.
func (p *ImageModelProfile) SeedIsRandom() bool {
	if p == nil {
		return true
	}
	return p.Runtime.SeedMode != "fixed"
}

// ServerArgs собирает аргументы командной строки sd-server для этой модели.
//
// НЕ включает: --listen-ip/--listen-port (их добавляет супервизор процесса),
// --lora-model-dir/--hires-upscalers-dir (каталоги уровня воркера).
// Порядок аргументов детерминирован — на него опираются тесты и
// contract-smoke против зафиксированной версии движка.
func (p *ImageModelProfile) ServerArgs() []string {
	if p == nil {
		return nil
	}
	args := make([]string, 0, 24)

	// 1) Файлы модели. All-in-one (SD1.x/2.x/SD-Turbo/SDXL) грузится через
	//    --model; DiT-семейства — через --diffusion-model + отдельные VAE/TE.
	for _, role := range []string{
		ImageFileRoleDiffusion, ImageFileRoleVae, ImageFileRoleClipL, ImageFileRoleClipG,
		ImageFileRoleT5xxl, ImageFileRoleLLM, ImageFileRoleClipVision,
		ImageFileRoleControlNet, ImageFileRoleIPAdapter, ImageFileRoleUpscaler,
	} {
		path := p.filePathByRole(role)
		if path == "" {
			continue
		}
		switch role {
		case ImageFileRoleDiffusion:
			if IsDiTFamily(p.Family) {
				args = append(args, "--diffusion-model", path)
			} else {
				args = append(args, "--model", path)
			}
		case ImageFileRoleVae:
			args = append(args, "--vae", path)
		case ImageFileRoleClipL:
			args = append(args, "--clip_l", path)
		case ImageFileRoleClipG:
			args = append(args, "--clip_g", path)
		case ImageFileRoleT5xxl:
			args = append(args, "--t5xxl", path)
		case ImageFileRoleLLM:
			args = append(args, "--llm", path)
		case ImageFileRoleClipVision:
			args = append(args, "--clip_vision", path)
		case ImageFileRoleControlNet:
			args = append(args, "--control-net", path)
		case ImageFileRoleIPAdapter:
			args = append(args, "--ip-adapter", path)
		case ImageFileRoleUpscaler:
			args = append(args, "--upscale-model", path)
		}
	}

	// TAESD — отдельный булев флаг (файл берётся движком из своего каталога).
	if p.Runtime.Taesd || p.hasRole(ImageFileRoleTaesd) {
		args = append(args, "--taesd")
	}

	// 2) Плейсмент/память.
	r := p.Runtime
	if r.Backend != "" {
		args = append(args, "--backend", r.Backend)
	}
	if r.ParamsBackend != "" {
		args = append(args, "--params-backend", r.ParamsBackend)
	} else if r.OffloadToCPU {
		args = append(args, "--offload-to-cpu")
	}
	if r.MaxVRAM != "" {
		args = append(args, "--max-vram", r.MaxVRAM)
	}
	if r.AutoFit != "" {
		args = append(args, "--auto-fit", r.AutoFit)
	}
	if r.NGpuLayers != 0 {
		args = append(args, "--n-gpu-layers", fmt.Sprintf("%d", r.NGpuLayers))
	}
	if r.SplitMode != "" {
		args = append(args, "--split-mode", r.SplitMode)
	}
	if r.Threads > 0 {
		args = append(args, "--threads", fmt.Sprintf("%d", r.Threads))
	}

	// 3) Скорость/память внимания и VAE.
	if r.DiffusionFA {
		args = append(args, "--diffusion-fa")
	}
	if r.VaeTiling {
		args = append(args, "--vae-tiling")
		if r.VaeTileSize > 0 {
			args = append(args, "--vae-tile-size", fmt.Sprintf("%d", r.VaeTileSize))
		}
	}
	if r.VaeConvDirect {
		args = append(args, "--vae-conv-direct")
	}

	// 4) Дефолты сэмплирования (их можно переопределить в запросе).
	d := p.Defaults
	if d.Steps > 0 {
		args = append(args, "--steps", fmt.Sprintf("%d", d.Steps))
	}
	if d.CFGScale > 0 {
		args = append(args, "--cfg-scale", trimFloat(d.CFGScale))
	}
	if d.Sampler != "" {
		args = append(args, "--sampling-method", d.Sampler)
	}
	if d.Scheduler != "" {
		args = append(args, "--scheduler", d.Scheduler)
	}
	if d.Width > 0 {
		args = append(args, "-W", fmt.Sprintf("%d", d.Width))
	}
	if d.Height > 0 {
		args = append(args, "-H", fmt.Sprintf("%d", d.Height))
	}
	if d.BatchCount > 0 {
		args = append(args, "--batch-count", fmt.Sprintf("%d", d.BatchCount))
	}
	if d.ClipSkip > 1 {
		args = append(args, "--clip-skip", fmt.Sprintf("%d", d.ClipSkip))
	}

	// 5) Случайный seed по умолчанию: OpenAI-путь sd-server НЕ читает seed из
	//    запроса и берёт 42 → одинаковые картинки. Поэтому ставим -1 явно,
	//    если профиль не требует фиксированного seed.
	if p.SeedIsRandom() {
		args = append(args, "--seed", "-1")
	} else if d.Seed >= 0 {
		args = append(args, "--seed", fmt.Sprintf("%d", d.Seed))
	}

	// 6) Операторские дополнительные флаги — последними.
	if len(r.ExtraArgs) > 0 {
		args = append(args, r.ExtraArgs...)
	}
	return args
}

func (p *ImageModelProfile) hasRole(role string) bool {
	for _, f := range p.Files {
		if f.Role == role {
			return true
		}
	}
	return false
}

// filePathByRole — локальный путь файла роли (LocalPath, иначе Filename).
// Несколько LoRA одной роли не поддерживаются в ServerArgs (они применяются
// per-request как lora[] в теле запроса).
func (p *ImageModelProfile) filePathByRole(role string) string {
	for _, f := range p.Files {
		if f.Role != role {
			continue
		}
		if f.LocalPath != "" {
			return f.LocalPath
		}
		return f.Filename
	}
	return ""
}

// ImageProfileNames — отсортированные имена профилей (для стабильных ответов API).
func ImageProfileNames(profiles map[string]ImageModelProfile) []string {
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// trimFloat — компактная запись числа: 1.0 → "1", 4.5 → "4.5".
// Нужна для аргументов командной строки (движок парсит std::stof, но
// детерминированность строки важна для contract-тестов).
func trimFloat(v float64) string {
	s := fmt.Sprintf("%.4f", v)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	if s == "" || s == "-" {
		return "0"
	}
	return s
}
