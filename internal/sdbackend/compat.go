// compat.go — R696 (2026-11-23): «сможет ли imageworker загрузить этот bundle».
//
// ЗАЧЕМ. Живой дефект (локальный стенд, imageworker r261120-r648): оператор
// скачал на вкладке HuggingFace один файл `Qwen-Image-2.1-Turbo-Uncensored-AD-Q4_K.gguf`
// (репозиторий AtomicChat/Qwen-Image-2.1-Turbo-Uncensored-GGUF). Воркер
// синтезировал профиль по имени (роль diffusion, family=other), запустил
// sd-server как all-in-one, и оператор увидел в панели только «не загрузилась»:
//
//	[out] loading model from '.../Qwen-Image-2.1-Turbo-Uncensored-AD-Q4_K.gguf'
//	[err] [ERROR] diffusion_engine.cpp:992 - get sd version from file failed
//	[err] [ERROR] main.cpp:93 - new_sd_ctx_t failed
//
// Файл при этом исправен — это TEXT ENCODER Qwen3-VL-8B (general.architecture=
// "qwen3vl", size_label=8.2B), который движок подключает флагом --llm, а
// diffusion-модели в наборе нет вовсе. То есть «не грузится модель» = набор
// неполный и роли назначены по имени, а не по содержимому.
//
// ЧТО ДЕЛАЕТ ЭТОТ ФАЙЛ. Считает ЯВНЫЙ вердикт по составу bundle'а ДО загрузки:
//   - роль каждого файла по заголовку (cppbackend.ProbeLocalFile) против роли в
//     профиле;
//   - семейство и режим движка (--model против --diffusion-model);
//   - обязательные роли семейства и чего не хватает;
//   - готовую фразу «imageworker сможет загрузить: да/нет, потому что …».
//
// Вердикты:
//
//	ready       — набор полный, движок получит корректные флаги;
//	incomplete  — семейство известно, но нет обязательных ролей (VAE/TE);
//	unsupported — движок заведомо не поднимет набор (diffusion-роль занята
//	              текстовым энкодером/VAE, family не DiT у DiT-файла, MLX-веса);
//	unknown     — по заголовкам судить не о чем (файлы не читаются/не тот формат).
//
// ПОЧЕМУ НЕ БЛОКИРУЕМ НА incomplete: правило обязательных ролей — наше знание о
// движке, а не его ответ; блокировать загрузку набора, который мог бы подняться,
// мы не вправе. Блокируется только unsupported, и у API есть force=true.
package sdbackend

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/types"
)

// Вердикты совместимости bundle (см. шапку файла).
const (
	CompatReady       = "ready"
	CompatIncomplete  = "incomplete"
	CompatUnsupported = "unsupported"
	CompatUnknown     = "unknown"
)

// Режимы движка в вердикте.
const (
	CompatEngineModel     = "model"           // all-in-one чекпойнт: --model
	CompatEngineDiffusion = "diffusion-model" // DiT: --diffusion-model + VAE/TE
	CompatEngineUnknown   = "unknown"
)

// CompatFile — один файл набора: роль в профиле и роль, определённая по заголовку.
type CompatFile struct {
	Filename        string `json:"filename"`
	Role            string `json:"role"` // роль, с которой файл попадёт в argv
	DetectedRole    string `json:"detectedRole,omitempty"`
	DetectedFamily  string `json:"detectedFamily,omitempty"`
	DetectedVersion string `json:"detectedVersion,omitempty"`
	FamilySource    string `json:"familySource,omitempty"`
	Format          string `json:"format,omitempty"`
	Arch            string `json:"arch,omitempty"`
	Truncated       bool   `json:"truncated,omitempty"`
	Dit             bool   `json:"dit,omitempty"`
	Match           bool   `json:"match"` // роль профиля совпала с ролью по содержимому
	Reason          string `json:"reason,omitempty"`
	ProbeError      string `json:"probeError,omitempty"`
	SizeBytes       int64  `json:"sizeBytes,omitempty"`
}

// CompatRole — комплектность одной роли.
type CompatRole struct {
	Role     string   `json:"role"`
	Label    string   `json:"label"`
	Required bool     `json:"required"`
	Found    bool     `json:"found"`
	Files    []string `json:"files,omitempty"`
	Hint     string   `json:"hint,omitempty"`
}

// BundleCompat — вердикт по набору файлов.
type BundleCompat struct {
	Name         string       `json:"name"`
	Verdict      string       `json:"verdict"`
	Runnable     bool         `json:"runnable"`
	EngineMode   string       `json:"engineMode"`
	Family       string       `json:"family"`
	VersionLabel string       `json:"versionLabel,omitempty"`
	Reason       string       `json:"reason"`
	Missing      []string     `json:"missing,omitempty"`
	Warnings     []string     `json:"warnings,omitempty"`
	Roles        []CompatRole `json:"roles,omitempty"`
	Files        []CompatFile `json:"files,omitempty"`
	CheckedAt    time.Time    `json:"checkedAt"`
}

// ---------- кэш заголовков ---------------------------------------------------
//
// Заголовок файла (первые 512 КБ) читается один раз на (path, size, mtime):
// GET /api/image/models пересобирает вердикт, а он же вызывается на каждый
// refresh реестра. Цена промаха — один read; цена вечного кэша — «вердикт не
// обновился после замены файла», поэтому ключ включает размер и mtime.

type probeCacheKey struct {
	path  string
	size  int64
	mtime int64
}

var localProbeCache = struct {
	mu sync.Mutex
	m  map[probeCacheKey]*cppbackend.HFProbeResult
}{m: map[probeCacheKey]*cppbackend.HFProbeResult{}}

// probeLocalCached — заголовок файла (роль/семейство) с кэшем по (path,size,mtime).
// Ошибка чтения не кэшируется.
func probeLocalCached(path string) (*cppbackend.HFProbeResult, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	key := probeCacheKey{path: path, size: info.Size(), mtime: info.ModTime().UnixNano()}
	localProbeCache.mu.Lock()
	if res, ok := localProbeCache.m[key]; ok {
		localProbeCache.mu.Unlock()
		return res, nil
	}
	localProbeCache.mu.Unlock()

	res, err := cppbackend.ProbeLocalFile(path, filepath.Base(path))
	if err != nil {
		return nil, err
	}
	localProbeCache.mu.Lock()
	// Не даём кэшу расти бесконечно (замены файлов меняют ключ): чистим по
	// достижении предела — модели на диске десятки, файлов сотни.
	if len(localProbeCache.m) > 512 {
		localProbeCache.m = map[probeCacheKey]*cppbackend.HFProbeResult{}
	}
	localProbeCache.m[key] = res
	localProbeCache.mu.Unlock()
	return res, nil
}

// ---------- таблица обязательных ролей --------------------------------------

// compatRoleSpec — обязательная роль семейства.
type compatRoleSpec struct {
	Role  string
	Label string
	Hint  string
}

// compatRequiredRoles — роли, без которых движок не поднимет набор этого
// семейства. Источник: docs/features/image-generation.ru.md §8.2 и §16.10
// (Qwen-Image 2.1: diffusion + VAE + LLM), сверено с pinned sd.cpp
// master-929-3f8527a: DiT-семейства подключаются --diffusion-model и требуют
// отдельные VAE/text encoder; all-in-one SD1.x/2.x/SDXL не требуют ничего.
var compatRequiredRoles = map[string][]compatRoleSpec{
	"qwen_image": {
		{Role: types.ImageFileRoleVae, Label: "VAE", Hint: "Файл VAE (например vae/qwen_image_2.1_vae_bf16.safetensors), флаг --vae."},
		{Role: types.ImageFileRoleLLM, Label: "text encoder (LLM)", Hint: "Текстовый энкодер (Qwen3-VL GGUF/safetensors), флаг --llm."},
	},
	"flux2": {
		{Role: types.ImageFileRoleVae, Label: "VAE", Hint: "Отдельный VAE (ae.safetensors), флаг --vae."},
		{Role: types.ImageFileRoleLLM, Label: "text encoder (LLM)", Hint: "Текстовый энкодер (Mistral Small 3.2), флаг --llm."},
	},
	"z_image": {
		{Role: types.ImageFileRoleVae, Label: "VAE", Hint: "Отдельный VAE, флаг --vae."},
		{Role: types.ImageFileRoleLLM, Label: "text encoder (LLM)", Hint: "Текстовый энкодер (Qwen3-4B), флаг --llm."},
	},
	"flux": {
		{Role: types.ImageFileRoleVae, Label: "VAE", Hint: "ae.safetensors из FLUX, флаг --vae."},
		{Role: types.ImageFileRoleClipL, Label: "CLIP-L", Hint: "clip_l.safetensors, флаг --clip_l."},
		{Role: types.ImageFileRoleT5xxl, Label: "T5-XXL", Hint: "Квантованный T5-XXL (t5xxl*.gguf), флаг --t5xxl."},
	},
	"sd3": {
		{Role: types.ImageFileRoleVae, Label: "VAE", Hint: "Отдельный VAE SD3, флаг --vae."},
		{Role: types.ImageFileRoleClipL, Label: "CLIP-L", Hint: "clip_l.safetensors, флаг --clip_l."},
		{Role: types.ImageFileRoleClipG, Label: "CLIP-G", Hint: "clip_g.safetensors, флаг --clip_g."},
		{Role: types.ImageFileRoleT5xxl, Label: "T5-XXL", Hint: "Квантованный T5-XXL, флаг --t5xxl."},
	},
	"chroma": {
		{Role: types.ImageFileRoleVae, Label: "VAE", Hint: "Отдельный VAE, флаг --vae."},
		{Role: types.ImageFileRoleT5xxl, Label: "T5-XXL", Hint: "Квантованный T5-XXL, флаг --t5xxl."},
	},
}

// CompatRoleLabel — человеческая подпись роли (для вердикта и UI).
func CompatRoleLabel(role string) string {
	switch role {
	case types.ImageFileRoleDiffusion:
		return "diffusion"
	case types.ImageFileRoleVae:
		return "VAE"
	case types.ImageFileRoleLLM:
		return "text encoder (LLM)"
	case types.ImageFileRoleClipL:
		return "CLIP-L"
	case types.ImageFileRoleClipG:
		return "CLIP-G"
	case types.ImageFileRoleT5xxl:
		return "T5-XXL"
	case types.ImageFileRoleClipVision:
		return "CLIP-vision"
	}
	return role
}

// BundleDir — каталог bundle'а по имени модели.
func BundleDir(modelsDir, name string) string {
	return filepath.Join(modelsDir, name)
}

// BuildBundleCompat — вердикт по набору файлов профиля (файлы читаются с диска).
//
// Читаются ТОЛЬКО заголовки (cppbackend.ProbeLocalFile): цена — 512 КБ на файл,
// поэтому вердикт считается при Load реестра, а не на каждый запрос.
func BuildBundleCompat(p *types.ImageModelProfile, bundleDir string) *BundleCompat {
	if p == nil {
		out := &BundleCompat{CheckedAt: time.Now().UTC(), Verdict: CompatUnknown}
		out.Reason = i18n.T("sdworker.compat.no_profile")
		return out
	}
	probes := make(map[string]*cppbackend.HFProbeResult, len(p.Files))
	probeErrors := make(map[string]string, len(p.Files))
	for _, f := range p.Files {
		path := f.LocalPath
		if path == "" && bundleDir != "" && f.Filename != "" {
			path = filepath.Join(bundleDir, f.Filename)
		}
		if path == "" || !isWeightFileName(f.Filename) {
			continue
		}
		res, err := probeLocalCached(path)
		if err != nil {
			probeErrors[f.Filename] = err.Error()
			continue
		}
		probes[f.Filename] = res
	}
	return VerifyBundleProbes(p, probes, probeErrors)
}

// VerifyBundleProbes — вердикт по УЖЕ снятым заголовкам файлов.
//
// Нужен для пред-проверки НАБОРА ДО СКАЧИВАНИЯ: UI присылает отмеченные файлы,
// воркер читает их заголовки Range-запросами к HF (файлов на диске ещё нет) и
// отвечает тем же вердиктом, что потом покажет вкладка «Модели». probes/errors
// ключуются Filename файла в профиле.
func VerifyBundleProbes(p *types.ImageModelProfile, probes map[string]*cppbackend.HFProbeResult, probeErrors map[string]string) *BundleCompat {
	out := &BundleCompat{CheckedAt: time.Now().UTC()}
	if p == nil {
		out.Verdict = CompatUnknown
		out.Reason = i18n.T("sdworker.compat.no_profile")
		return out
	}
	out.Name = p.Name
	out.Family = p.Family
	out.Roles = nil

	// 1) Заголовки всех файлов набора: роль/семейство по содержимому.
	detectedFamily, detectedVersion, familyFromArch := "", "", false
	for _, f := range p.Files {
		cf := CompatFile{Filename: f.Filename, Role: f.Role, SizeBytes: f.SizeBytes}
		cf.Match = true
		if !isWeightFileName(f.Filename) {
			out.Files = append(out.Files, cf)
			continue
		}
		res := probes[f.Filename]
		if res == nil {
			if msg := probeErrors[f.Filename]; msg != "" {
				cf.ProbeError = msg
			}
			out.Files = append(out.Files, cf)
			continue
		}
		cf.DetectedRole = res.Role
		cf.DetectedFamily = res.Family
		cf.DetectedVersion = res.Version
		cf.FamilySource = res.FamilySource
		cf.Format = res.Format
		cf.Arch = res.Arch
		cf.Truncated = res.Truncated
		cf.Dit = res.DiT
		cf.Reason = res.Reason
		if res.Role != "" {
			cf.Match = res.Role == f.Role
		}
		if res.Role == types.ImageFileRoleDiffusion && res.Family != "" &&
			res.Family != "other" && detectedFamily == "" {
			detectedFamily = res.Family
			detectedVersion = res.Version
			familyFromArch = res.FamilySource == cppbackend.ProbeFamilyFromArch
		}
		out.Files = append(out.Files, cf)
	}

	// 2) Семейство: профиль главнее («other» = не знаем, тогда верим содержимому).
	effectiveFamily := p.Family
	if effectiveFamily == "" || effectiveFamily == "other" {
		if detectedFamily != "" && !familyFromArch {
			effectiveFamily = detectedFamily
		}
	}
	out.Family = effectiveFamily
	if detectedVersion != "" {
		out.VersionLabel = detectedVersion
	}
	dit := types.IsDiTFamily(effectiveFamily)
	if dit || detectedFamily != "" && types.IsDiTFamily(detectedFamily) {
		out.EngineMode = CompatEngineDiffusion
	} else if effectiveFamily != "" && effectiveFamily != "other" {
		out.EngineMode = CompatEngineModel
	} else {
		out.EngineMode = CompatEngineUnknown
	}

	out.Warnings = append(out.Warnings, compatWarnings(p, out.Files, effectiveFamily, familyFromArch)...)

	// 3) Жёсткие отказы: движок заведомо не поднимет набор.
	if reason, code := compatHardFailures(p, out.Files, effectiveFamily, dit); code != "" {
		out.Verdict = CompatUnsupported
		out.Runnable = false
		out.Reason = reason
		out.Missing = compatMissingForHardFailure(out.Files, dit)
		out.Roles = compatRoleCompleteness(p, effectiveFamily, dit)
		return out
	}

	// 4) Комплектность ролей.
	out.Roles = compatRoleCompleteness(p, effectiveFamily, dit)
	for _, r := range out.Roles {
		if r.Required && !r.Found {
			out.Missing = append(out.Missing, r.Label)
		}
	}
	if len(out.Missing) > 0 {
		out.Verdict = CompatIncomplete
		out.Runnable = false
		out.Reason = i18n.T("sdworker.compat.incomplete", out.EngineModeLabel(), strings.Join(out.Missing, ", "))
		return out
	}

	// 5) Набор полный — остались только «чем доказано». Если по заголовку
	//    семейство не опознано (файлы усечены/формат не тот), честно пишем, что
	//    движок может ответить get sd version from file failed.
	if effectiveFamily == "" || effectiveFamily == "other" {
		out.Verdict = CompatUnknown
		out.Runnable = true
		out.Reason = i18n.T("sdworker.compat.unknown_family")
		return out
	}
	if familyFromArch {
		out.Verdict = CompatUnknown
		out.Runnable = true
		out.Reason = i18n.T("sdworker.compat.arch_only", detectedVersion)
		return out
	}
	out.Verdict = CompatReady
	out.Runnable = true
	if out.VersionLabel != "" {
		out.Reason = i18n.T("sdworker.compat.ready", out.VersionLabel, out.EngineModeLabel())
	} else {
		out.Reason = i18n.T("sdworker.compat.ready_no_version", out.EngineModeLabel())
	}
	return out
}

// EngineModeLabel — режим движка по-русски (для текста вердикта).
func (c *BundleCompat) EngineModeLabel() string {
	switch c.EngineMode {
	case CompatEngineModel:
		return "как all-in-one (--model)"
	case CompatEngineDiffusion:
		return "как DiT (--diffusion-model + отдельные VAE/text encoder)"
	}
	return "режим не определён"
}

// compatHardFailures — отказы, в которых мы уверены по содержимому файлов.
func compatHardFailures(p *types.ImageModelProfile, files []CompatFile, family string, dit bool) (string, string) {
	diffusionFiles := 0
	for _, f := range files {
		if f.Role == types.ImageFileRoleDiffusion {
			diffusionFiles++
		}
	}
	if diffusionFiles == 0 {
		return i18n.T("sdworker.compat.no_diffusion"), "no_diffusion"
	}
	for _, f := range files {
		if f.Role != types.ImageFileRoleDiffusion {
			continue
		}
		if f.DetectedRole != "" && f.DetectedRole != types.ImageFileRoleDiffusion {
			// ЖИВОЙ СЛУЧАЙ: единственный «diffusion»-файл — текстовый энкодер.
			return i18n.T("sdworker.compat.diffusion_is_encoder",
				f.Filename, CompatRoleLabel(f.DetectedRole), f.Arch), "diffusion_is_encoder"
		}
		if f.DetectedRole == types.ImageFileRoleDiffusion && !f.Dit && dit {
			return i18n.T("sdworker.compat.family_mismatch_dit",
				f.Filename, CompatRoleLabel(f.Role), f.DetectedVersion, p.Family), "family_mismatch"
		}
		if f.DetectedRole == types.ImageFileRoleDiffusion && f.DetectedVersion != "" &&
			!types.IsDiTFamily(f.DetectedFamily) && dit {
			return i18n.T("sdworker.compat.family_mismatch_dit",
				f.Filename, CompatRoleLabel(f.Role), f.DetectedVersion, p.Family), "family_mismatch"
		}
	}
	return "", ""
}

// compatMissingForHardFailure — чего не хватает в наборе, который уже отбракован
// (для жёсткого отказа нужен хотя бы diffusion-файл).
func compatMissingForHardFailure(files []CompatFile, dit bool) []string {
	has := map[string]bool{}
	for _, f := range files {
		if f.Role == "" {
			continue
		}
		// Роль, занятая файлом с другим содержимым, не считается найденной.
		if f.DetectedRole != "" && f.DetectedRole != f.Role {
			continue
		}
		has[f.Role] = true
	}
	out := make([]string, 0, 3)
	if !has[types.ImageFileRoleDiffusion] {
		out = append(out, "diffusion")
	}
	return out
}

// compatRoleCompleteness — какие роли нужны семейству и что найдено.
func compatRoleCompleteness(p *types.ImageModelProfile, family string, dit bool) []CompatRole {
	found := map[string][]string{}
	for _, f := range p.Files {
		if f.Role == "" || f.Filename == "" {
			continue
		}
		if f.Role == types.ImageFileRoleLora {
			continue
		}
		found[f.Role] = append(found[f.Role], f.Filename)
	}
	for role := range found {
		sort.Strings(found[role])
	}

	specs := []compatRoleSpec{{Role: types.ImageFileRoleDiffusion, Label: "diffusion"}}
	if dit {
		specs = append(specs, compatRequiredRoles[family]...)
		if len(compatRequiredRoles[family]) == 0 {
			// DiT-семейства без своей записи всё равно требуют VAE: движок
			// ищет VAE-тензоры в отдельном файле.
			specs = append(specs, compatRoleSpec{
				Role: types.ImageFileRoleVae, Label: "VAE",
				Hint: "Отдельный VAE (fлаг --vae).",
			})
		}
	}
	out := make([]CompatRole, 0, len(specs))
	for _, sp := range specs {
		out = append(out, CompatRole{
			Role:     sp.Role,
			Label:    sp.Label,
			Required: true,
			Found:    len(found[sp.Role]) > 0,
			Files:    found[sp.Role],
			Hint:     sp.Hint,
		})
	}
	return out
}

// compatWarnings — подсказки, которые не меняют вердикт, но нужны оператору.
func compatWarnings(p *types.ImageModelProfile, files []CompatFile, family string, familyFromArch bool) []string {
	var out []string
	if familyFromArch {
		out = append(out, i18n.T("sdworker.compat.warn_family_from_arch"))
	}
	for _, f := range files {
		if f.DetectedRole == "" || f.Role == "" || f.DetectedRole == f.Role {
			continue
		}
		// Файл-энкодер назван ролью diffusion, но, кроме него, в наборе есть
		// настоящий diffusion-файл: роль будет исправлена при синтезе профиля,
		// здесь — предупреждение, чтобы оператор знал про переименование.
		out = append(out, i18n.T("sdworker.compat.warn_role_mismatch",
			f.Filename, CompatRoleLabel(f.Role), CompatRoleLabel(f.DetectedRole)))
	}
	if p != nil && p.Family != "" && p.Family != "other" && types.IsDiTFamily(p.Family) {
		hasVae := false
		for _, f := range p.Files {
			if f.Role == types.ImageFileRoleVae {
				hasVae = true
			}
		}
		if !hasVae {
			out = append(out, i18n.T("sdworker.compat.warn_dit_without_vae", p.Family))
		}
	}
	return out
}

// isWeightFileName — файл весов по расширению (тот же фильтр, что у синтеза).
func isWeightFileName(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".gguf" || ext == ".safetensors" || ext == ".ckpt" || ext == ".pt" || ext == ".sft"
}

// CompatReasonLine — одна строка «сможет/не сможет» для логов и ответа API.
func (c *BundleCompat) CompatReasonLine() string {
	if c == nil {
		return ""
	}
	if c.Runnable {
		return i18n.T("sdworker.compat.line_yes", c.Name, c.Reason)
	}
	return i18n.T("sdworker.compat.line_no", c.Name, c.Reason)
}

// LoadBlockedMessage — текст ошибки API, когда загрузка остановлена до запуска
// движка (вердикт unsupported).
func LoadBlockedMessage(c *BundleCompat) string {
	if c == nil {
		return ""
	}
	return i18n.T("sdworker.compat.load_blocked", c.Reason)
}

// LoadBlockedHint — что делать оператору с заблокированной загрузкой.
func LoadBlockedHint() string { return i18n.T("sdworker.compat.load_blocked_hint") }

// IsImageWeightFile — файл весов по расширению (экспорт фильтра синтеза: им же
// пользуется пред-проверка выбранного набора в HTTP-слое).
func IsImageWeightFile(name string) bool { return isWeightFileName(name) }
