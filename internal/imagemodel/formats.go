// formats.go — R755 (2026-11-25): какие ФОРМАТЫ файлов движок реально читает.
//
// ЗАЧЕМ. До R755 ответ «примет ли imageworker safetensors» существовал только в
// документации и в голове оператора: в списке файлов HF были и .gguf, и
// .safetensors, а понять, что для Qwen-Image safetensors-диффузия не читается
// (нужен GGUF), а VAE/text encoder в safetensors — читаются, можно было лишь
// после скачивания и падения загрузки. Живой случай §16.9: скачан MLX-квант
// .safetensors (U32 + scales/biases), движок ответил `unsupported dtype "U32"` и
// оператор потерял гигабайты трафика.
//
// ПОЧЕМУ ПРАВИЛА ЗДЕСЬ, А НЕ В JS. Это знание о ДВИЖКЕ (stable-diffusion.cpp), а
// не о вёрстке: тот же ответ нужен странице HF, панели файлов группы и агенту.
// Если бы таблица жила в UI, она разъехалась бы с probe/compat, которые уже
// читают реальные заголовки. Поэтому воркер отдаёт МАТРИЦУ (GET
// /api/image/formats), а UI только рисует бейджи по машинным кодам:
//
//	reads        — движок читает этот формат в этой роли;
//	needs_gguf   — в этой роли нужен GGUF (семейство знает только его);
//	warn         — формат допустим не всегда: решает «Проверить» (заголовок);
//	unsupported  — движок не прочитает (например MLX-квант safetensors);
//	unknown      — данных нет, обещать нечего.
//
// ГРАНИЦЫ: матрица НЕ заменяет пред-проверку заголовка (R696) и вердикт
// совместимости: она отвечает «стоит ли качать этот формат В ПРИНЦИПЕ», а
// «узнаёт ли движок ИМЕННО этот файл» отвечает probe. При расхождении UI
// показывает probe (он смотрел содержимое).
package imagemodel

import (
	"path/filepath"
	"strings"

	"ollama-loadbalancer/pkg/types"
)

// Коды вердикта формата (контракт UI: webui/js/modules/image-model-files.js,
// image-models-hf.js). Менять строки нельзя без правки UI и тестов.
const (
	FormatReads       = "reads"
	FormatNeedsGGUF   = "needs_gguf"
	FormatWarn        = "warn"
	FormatUnsupported = "unsupported"
	FormatUnknown     = "unknown"
)

// FormatRule — одно правило «формат × роль → вердикт».
//
// Role == "*" — правило по умолчанию для формата; точная роль всегда приоритетнее
// (VAE в safetensors читается, а diffusion того же семейства — нет).
type FormatRule struct {
	Format  string `json:"format"` // расширение без точки: gguf, safetensors, sft, ckpt
	Role    string `json:"role"`   // роль контракта или "*"
	Verdict string `json:"verdict"`
}

// FormatNameWarning — предупреждение ПО ИМЕНИ файла (до чтения заголовка).
//
// Имя — единственное, что известно до скачивания. Список — данные, а не regex в
// JS: «mlx» в имени с высокой вероятностью означает MLX-квант (U32), который
// движок не читает; окончательный ответ всё равно даёт probe.
type FormatNameWarning struct {
	Contains string `json:"contains"` // подстрока в нижнем регистре
	Verdict  string `json:"verdict"`
	Code     string `json:"code"` // машинный код для i18n/тестов
}

// FormatMatrix — ответ GET /api/image/formats (контракт UI).
type FormatMatrix struct {
	Engine       string              `json:"engine"`
	Family       string              `json:"family"`
	Rules        []FormatRule        `json:"rules"`
	NameWarnings []FormatNameWarning `json:"nameWarnings"`
	FamilyKnown  bool                `json:"familyKnown"`
	DIT          bool                `json:"dit"`
	AllInOne     bool                `json:"allInOne"`
}

// formatComponentRoles — роли, которые движок грузит отдельными флагами
// (--vae/--clip_l/--clip_g/--t5xxl/--llm/--taesd/--clip_vision) или применяет
// per-request (LoRA/ControlNet/IP-Adapter/апскейлер). В safetensors они читаются
// штатно: sd.cpp работает через ggml и понимает оба контейнера.
var formatComponentRoles = []string{
	types.ImageFileRoleVae,
	types.ImageFileRoleLora,
	types.ImageFileRoleControlNet,
	types.ImageFileRoleIPAdapter,
	types.ImageFileRoleClipL,
	types.ImageFileRoleClipG,
	types.ImageFileRoleT5xxl,
	types.ImageFileRoleLLM,
	types.ImageFileRoleClipVision,
	types.ImageFileRoleTaesd,
	types.ImageFileRoleUpscaler,
}

// BuildFormatMatrix — матрица форматов для семейства.
//
// Семейство "" / "other" / неизвестное: FamilyKnown=false; правила остаются
// консервативными — GGUF читается всегда, safetensors-компоненты читаются,
// safetensors-диффузия помечается warn («решает заголовок»), потому что без
// семейства нельзя отличить all-in-one SDXL от DiT-диффузии.
func BuildFormatMatrix(family string) FormatMatrix {
	fam := strings.ToLower(strings.TrimSpace(family))
	known := fam != "" && fam != "other" && types.IsValidImageFamily(fam)
	dit := types.IsDiTFamily(fam)
	out := FormatMatrix{
		Engine:      "stable-diffusion.cpp",
		Family:      fam,
		FamilyKnown: known,
		DIT:         dit,
		AllInOne:    known && !dit,
	}
	add := func(format, role, verdict string) {
		out.Rules = append(out.Rules, FormatRule{Format: format, Role: role, Verdict: verdict})
	}

	// Контейнер движка: GGUF читается в любой роли. .sft — квантованный формат
	// самого sd.cpp, тоже читается.
	add("gguf", "*", FormatReads)
	add("sft", "*", FormatReads)

	// Отдельные компоненты в safetensors читаются у ВСЕХ семейств (и all-in-one,
	// и DiT): именно это подтверждено живым набором Qwen-Image (§16.10) и FLUX.
	for _, role := range formatComponentRoles {
		add("safetensors", role, FormatReads)
	}

	// Диффузия — главный источник ошибок формата.
	switch {
	case out.AllInOne:
		// SD1.5/SD2.x/SDXL: all-in-one .safetensors/.ckpt — штатный путь (--model).
		add("safetensors", types.ImageFileRoleDiffusion, FormatReads)
		add("ckpt", types.ImageFileRoleDiffusion, FormatReads)
	case fam == "qwen_image":
		// Документированный факт (§16.10): safetensors-варианты diffusion этого
		// семейства движок не читает, нужен GGUF; VAE и энкодеры — читаются.
		add("safetensors", types.ImageFileRoleDiffusion, FormatNeedsGGUF)
		add("ckpt", types.ImageFileRoleDiffusion, FormatNeedsGGUF)
	case dit:
		// Прочие DiT (FLUX/FLUX2/SD3/Chroma/Z-Image): рабочий путь — GGUF от
		// leejet/*, но отдельные safetensors-публикации существуют. Не обещаем и
		// не запрещаем: warn + «Проверить» (заголовок).
		add("safetensors", types.ImageFileRoleDiffusion, FormatWarn)
		add("ckpt", types.ImageFileRoleDiffusion, FormatWarn)
	default:
		// Семейство неизвестно: all-in-one safetensors может подойти (SDXL), а
		// DiT-диффузия — нет. Обещать нельзя, решает заголовок.
		add("safetensors", types.ImageFileRoleDiffusion, FormatWarn)
		add("ckpt", types.ImageFileRoleDiffusion, FormatWarn)
	}

	// MLX-квант: подстрока в имени — достаточный повод предупредить ДО
	// скачивания; точный вердикт даёт probe (U32 в заголовке).
	out.NameWarnings = []FormatNameWarning{{
		Contains: "mlx",
		Verdict:  FormatUnsupported,
		Code:     "mlx_quantized",
	}}
	return out
}

// ResolveFormat — вердикт для файла: точная роль приоритетнее правила "*".
//
// Неизвестное расширение — FormatUnknown (а не «читается»): молчаливое обещание
// здесь дороже честного «данных нет».
func (m FormatMatrix) ResolveFormat(filename, role string) string {
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(strings.TrimSpace(filename))), ".")
	if ext == "" {
		return FormatUnknown
	}
	role = strings.TrimSpace(role)
	for _, r := range m.Rules {
		if r.Format == ext && r.Role == role {
			return r.Verdict
		}
	}
	for _, r := range m.Rules {
		if r.Format == ext && r.Role == "*" {
			return r.Verdict
		}
	}
	return FormatUnknown
}

// NameWarningFor — предупреждение по имени файла (nil, если нет).
func (m FormatMatrix) NameWarningFor(filename string) *FormatNameWarning {
	name := strings.ToLower(filepath.Base(strings.TrimSpace(filename)))
	for i := range m.NameWarnings {
		if m.NameWarnings[i].Contains != "" && strings.Contains(name, m.NameWarnings[i].Contains) {
			return &m.NameWarnings[i]
		}
	}
	return nil
}
