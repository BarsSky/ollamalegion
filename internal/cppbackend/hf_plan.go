// hf_plan.go — «паспорт репозитория»: что скачать, в каком режиме запустится
// движок и какие шаги остались.
//
// ЗАЧЕМ (живой разбор 2026-10-06). Оператор скачал ОДИН файл Qwen-Image 2.1
// (diffusion) и получил:
//
//	Version: Qwen Image 2.1                       ← файл прочитан, семейство узнано
//	ERROR Conditioner model tensor 'text_encoders.llm.model.embed_tokens.weight' not in model metadata
//	ERROR VAE tensor 'first_stage_model.conv1.weight' not in model metadata
//	ERROR model metadata validation failed / new_sd_ctx_t failed
//
// То есть «пригодность файла» и «пригодность НАБОРА» — разные вещи, а поиск
// показывал только первое. Диагноз собирается здесь: по именам файлов и заголовку
// главного кандидата определяем семейство, режим подключения (--model против
// --diffusion-model), обязательные роли и то, чего не хватает. UI показывает это
// в карточке репозитория, а инструкции те же, что нужны модели, которая поднимает
// модель инструментом (см. docs/image-generation.md §16.9).
package cppbackend

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"
)

// HFPlanRole — одна роль в наборе: нужна ли, найден ли файл, чем закрывается.
type HFPlanRole struct {
	// Role — роль bundle (pkg/types.ImageFileRole*): diffusion/vae/clip_l/llm/...
	Role string `json:"role"`
	// Label — человеческое имя для UI («VAE», «text encoder (Qwen2.5-VL)»).
	Label string `json:"label"`
	// Required — без этой роли движок не поднимется для этого семейства.
	Required bool `json:"required"`
	// Found — файл-кандидат есть в репозитории.
	Found bool `json:"found"`
	// Files — найденные кандидаты (пути внутри репозитория).
	Files []string `json:"files,omitempty"`
	// Hint — что искать, если роли нет.
	Hint string `json:"hint,omitempty"`
}

// HFRepoPlan — паспорт репозитория для UI.
type HFRepoPlan struct {
	ModelID  string `json:"modelId"`
	Revision string `json:"revision"`
	// Family — семейство pkg/types.ImageModelFamilies, определённое по заголовку
	// главного файла ("" = по заголовку не определить).
	Family string `json:"family,omitempty"`
	// VersionLabel — как семейство называет сам движок («Qwen Image 2.1»).
	VersionLabel string `json:"versionLabel,omitempty"`
	// EngineMode: "diffusion-model" (DiT: --diffusion-model + отдельные VAE/TE) |
	// "model" (all-in-one чекпойнт) | "unknown".
	EngineMode string `json:"engineMode"`
	// MainFile — главный файл-кандидат (самый крупный diffusion), "" если нет.
	MainFile string `json:"mainFile,omitempty"`
	// Roles — комплектность набора.
	Roles []HFPlanRole `json:"roles"`
	// Missing — роли, которых не хватает (человеческие имена).
	Missing []string `json:"missing,omitempty"`
	// Usable — набор собран полностью и движок сможет его запустить.
	Usable bool `json:"usable"`
	// Verdict: "ready" | "incomplete" | "unsupported" | "unknown".
	Verdict string `json:"verdict"`
	// Summary — одна строка для карточки репозитория.
	Summary string `json:"summary"`
	// Steps — что делать оператору по шагам (в том числе порядок в WebUI).
	Steps []string `json:"steps,omitempty"`
	// ModelHint — что делать ТЕКСТОВОЙ модели, которая поднимает модель
	// инструментом (какое имя передавать, надо ли вызывать каталог).
	ModelHint string `json:"modelHint,omitempty"`
	// ProbeError — почему главный файл проверить не удалось (сеть/HF).
	ProbeError string `json:"probeError,omitempty"`
	// CheckedAt — когда считали.
	CheckedAt time.Time `json:"checkedAt"`
}

// PlanRepo собирает паспорт репозитория: список файлов + пред-проверка главного
// кандидата (один Range-запрос, 512 КБ).
func (d *HuggingFaceDownloader) PlanRepo(ctx context.Context, modelID, revision string, files []HFFileInfo) (*HFRepoPlan, error) {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil, fmt.Errorf("modelId is required")
	}
	if revision == "" {
		revision = "main"
	}
	plan := &HFRepoPlan{
		ModelID:   modelID,
		Revision:  revision,
		CheckedAt: time.Now().UTC(),
	}
	if files == nil {
		listed, err := d.ListModelFilesByFormat(ctx, modelID, revision, ModelWeightExtensions)
		if err != nil {
			return nil, err
		}
		files = listed
	}

	// Главный кандидат — самый крупный файл с ролью diffusion (или любой весовой).
	main := mainDiffusionCandidate(files)
	if main != "" {
		plan.MainFile = main
		if _, err := d.probeInto(ctx, modelID, main, revision, plan); err != nil {
			// Пред-проверка недоступна (сеть, приватный репо) — это не ошибка
			// паспорта: отдаём комплектность по именам и честно пишем причину.
			plan.ProbeError = err.Error()
		}
	}
	plan.Roles = planRoles(files, plan.Family, plan.EngineMode)
	for _, r := range plan.Roles {
		if r.Required && !r.Found {
			plan.Missing = append(plan.Missing, r.Label)
		}
	}
	plan.Usable = plan.Verdict != "unsupported" && len(plan.Missing) == 0 && plan.MainFile != ""
	plan.Verdict, plan.Summary = planVerdict(plan)
	plan.Steps = planSteps(plan)
	plan.ModelHint = planModelHint(plan)
	return plan, nil
}

// probeInto — пред-проверка главного файла с записью результата в паспорт.
func (d *HuggingFaceDownloader) probeInto(ctx context.Context, modelID, filename, revision string, plan *HFRepoPlan) (*HFProbeResult, error) {
	res, err := d.ProbeFile(ctx, modelID, filename, revision)
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, nil
	}
	switch res.Verdict {
	case HFProbeUnsupported:
		plan.Verdict = "unsupported"
		plan.EngineMode = "unknown"
		plan.Summary = res.Reason
		return res, nil
	case HFProbeSupported:
		plan.Family = res.Family
		plan.VersionLabel = res.Version
		if res.DiT {
			plan.EngineMode = "diffusion-model"
		} else {
			plan.EngineMode = "model"
		}
	}
	return res, nil
}

// mainDiffusionCandidate — самый крупный файл, который может быть diffusion.
func mainDiffusionCandidate(files []HFFileInfo) string {
	best := ""
	var bestSize int64 = -1
	for _, f := range files {
		p := strings.TrimSpace(f.Path)
		if p == "" || !isWeightPath(p) {
			continue
		}
		if roleFromFilenameLocal(path.Base(p)) != "diffusion" {
			continue
		}
		if f.SizeBytes > bestSize {
			best, bestSize = p, f.SizeBytes
		}
	}
	return best
}

// planRoles — какие роли нужны для этого семейства и что уже есть.
func planRoles(files []HFFileInfo, family, engineMode string) []HFPlanRole {
	found := map[string][]string{}
	for _, f := range files {
		p := strings.TrimSpace(f.Path)
		if p == "" {
			continue
		}
		role := roleFromFilenameLocal(path.Base(p))
		if f.SuggestedRole != "" {
			role = f.SuggestedRole
		}
		if role == "" {
			continue
		}
		found[role] = append(found[role], p)
	}

	// Набор ролей по семейству. DiT-семействам (SDL3/FLUX/Qwen/Z-Image/Chroma)
	// нужны отдельные VAE и text encoder — это ровно то, чего не хватило на живом
	// примере Qwen-Image 2.1.
	type spec struct {
		role, label string
		required    bool
		hint        string
	}
	var specs []spec
	if engineMode == "model" {
		specs = []spec{
			{"diffusion", "diffusion (all-in-one)", true,
				"Возьмите all-in-one чекпойнт: у него VAE и CLIP внутри (--model)."},
			{"vae", "VAE", false, "Не нужен: у all-in-one чекпойнта VAE внутри."},
			{"lora", "LoRA", false, "Опционально: применяется per-request."},
		}
	} else {
		dit := IsDiTFamilyName(family) || engineMode == "diffusion-model"
		specs = []spec{
			{"diffusion", "diffusion (DiT)", true,
				"Главный файл модели (GGUF). Подключается как --diffusion-model."},
			{"vae", "VAE", dit,
				"Нужен отдельный VAE (например ae.safetensors / *_vae_*.safetensors): без него движок отвечает «VAE tensor ... not in model metadata»."},
			{"llm", "text encoder (LLM)", dit,
				"Нужен отдельный text encoder (GGUF, например Qwen2.5-VL): без него «Conditioner model tensor ... not in model metadata»."},
			{"clip_l", "CLIP-L", false, "Для FLUX/SD3 вместо LLM или вместе с ним, если так собран релиз."},
			{"t5xxl", "T5-XXL", false, "Для FLUX/SD3 (только квантованный)."},
			{"lora", "LoRA", false, "Опционально: применяется per-request."},
		}
	}

	out := make([]HFPlanRole, 0, len(specs))
	for _, sp := range specs {
		r := HFPlanRole{
			Role:     sp.role,
			Label:    sp.label,
			Required: sp.required,
			Files:    found[sp.role],
			Found:    len(found[sp.role]) > 0,
			Hint:     sp.hint,
		}
		// Для DiT text encoder может лежать как clip_l+t5xxl (FLUX) — тогда
		// отсутствие роли llm не делает набор неполным.
		if sp.role == "llm" && !r.Found && len(found["t5xxl"]) > 0 {
			r.Required = false
			r.Hint = "В этом репозитории text encoder собран как T5-XXL (+CLIP-L) — роль llm не нужна."
		}
		out = append(out, r)
	}
	return out
}

// planVerdict — короткий вердикт и одна строка для карточки.
func planVerdict(plan *HFRepoPlan) (string, string) {
	if plan.Verdict == "unsupported" {
		return "unsupported", plan.Summary
	}
	if plan.MainFile == "" {
		return "unknown", "В репозитории нет diffusion-файла: возьмите репозиторий с весами модели."
	}
	mode := "как all-in-one (--model)"
	if plan.EngineMode == "diffusion-model" {
		mode = "как DiT (--diffusion-model + отдельные VAE/text encoder)"
	}
	if len(plan.Missing) > 0 {
		return "incomplete", fmt.Sprintf(
			"Движок запустит %s, но не хватает: %s.", mode, strings.Join(plan.Missing, ", "))
	}
	if plan.Verdict == "unknown" || plan.VersionLabel == "" {
		return "unknown", fmt.Sprintf("Набор собран (%s), но семейство по заголовку не определяется — сверьте его в профиле bundle.", mode)
	}
	return "ready", fmt.Sprintf("Набор собран: движок узнаёт «%s» и запустит %s.", plan.VersionLabel, mode)
}

// planSteps — пошаговая инструкция для WebUI.
func planSteps(plan *HFRepoPlan) []string {
	if plan.MainFile == "" {
		return []string{
			"Найдите репозиторий с GGUF-весами модели (роль diffusion) — в этом файлов с весами нет.",
		}
	}
	steps := []string{
		"Отметьте в списке файлов: " + strings.Join(planFileList(plan), ", ") + ".",
		"Проверьте роли у каждого файла (diffusion / vae / llm) — они подставляются по именам, но их можно изменить.",
	}
	if plan.Family != "" {
		steps = append(steps, "В поле «Семейство» выберите "+plan.Family+" (для DiT это обязательно: иначе движок получит --model и ответит «get sd version from file failed»).")
	} else {
		steps = append(steps, "Семейство по заголовку не определено: выберите его вручную (иначе движок получит --model).")
	}
	steps = append(steps,
		"Нажмите «Скачать bundle» — файлы лягут в один каталог модели.",
		"После загрузки нажмите «Загрузить» у модели на вкладке «Модели на диске»: воркер поднимет sd-server с нужными флагами.",
		"Если загрузка упала, посмотрите текст ошибки в тосте: в нём движок перечисляет недостающие тензоры (VAE/conditioner) — значит, в наборе не хватает роли.",
	)
	if len(plan.Missing) > 0 {
		steps = append(steps, "Не хватает: "+strings.Join(plan.Missing, ", ")+". Скачайте эти файлы в тот же bundle.")
	}
	return steps
}

// planFileList — файлы набора: главный + по одному кандидату на обязательную роль.
func planFileList(plan *HFRepoPlan) []string {
	out := []string{plan.MainFile}
	for _, r := range plan.Roles {
		if !r.Required || r.Role == "diffusion" || len(r.Files) == 0 {
			continue
		}
		out = append(out, r.Files[0])
	}
	return out
}

// planModelHint — что нужно ТЕКСТОВОЙ модели, которая поднимает модель инструментом.
func planModelHint(plan *HFRepoPlan) string {
	if !plan.Usable {
		return ""
	}
	model := strings.TrimSuffix(path.Base(plan.MainFile), path.Ext(plan.MainFile))
	return fmt.Sprintf(
		"Инструменту generate_image передавайте model=%q (имя каталога bundle), а не «stable-diffusion.cpp» и не имя репозитория: имена моделей на воркере можно посмотреть вызовом list_image_models.",
		model)
}

// IsDiTFamilyName — DiT-семейство по имени (дубль pkg/types.IsDiTFamily, чтобы
// пакет не тянул types ради одной проверки; список совпадает с ImageModelFamilies).
func IsDiTFamilyName(family string) bool {
	switch family {
	case "sd3", "flux", "flux2", "chroma", "qwen_image", "z_image":
		return true
	}
	return false
}

// isWeightPath — файл весов по расширению.
func isWeightPath(p string) bool {
	lower := strings.ToLower(p)
	for _, ext := range ModelWeightExtensions {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// roleFromFilenameLocal — роль файла по имени (зеркало sdbackend.roleFromFilename).
//
// ПОЧЕМУ ЛОКАЛЬНАЯ КОПИЯ: пакет cppbackend — нижний слой (его использует и
// cppworker, и sdbackend), тянуть из него sdbackend нельзя (цикл). Правила держим
// в одном смысловом виде; расхождение ловится тестом hf_plan_test.go, где те же
// имена файлов проверяются на живой раскладке Qwen-Image 2.1.
func roleFromFilenameLocal(filename string) string {
	n := strings.ToLower(filename)
	inTE := strings.Contains(n, "text_encoder") || strings.Contains(n, "text-encoder") ||
		strings.Contains(n, "/te/") || strings.HasPrefix(n, "te/")
	if inTE {
		switch {
		case strings.Contains(n, "clip_l") || strings.Contains(n, "clip-l"):
			return "clip_l"
		case strings.Contains(n, "clip_g") || strings.Contains(n, "clip-g"):
			return "clip_g"
		case strings.Contains(n, "t5"):
			return "t5xxl"
		case strings.Contains(n, "qwen") || strings.Contains(n, "llm") || strings.Contains(n, "gemma"):
			return "llm"
		}
	}
	switch {
	case strings.Contains(n, "vae") || strings.HasSuffix(n, "ae.safetensors"):
		return "vae"
	case strings.Contains(n, "clip_l") || strings.Contains(n, "clip-l"):
		return "clip_l"
	case strings.Contains(n, "clip_g") || strings.Contains(n, "clip-g"):
		return "clip_g"
	case strings.Contains(n, "t5"):
		return "t5xxl"
	case strings.Contains(n, "taesd"):
		return "taesd"
	case strings.Contains(n, "clip_vision") || strings.Contains(n, "vision"):
		return "clip_vision"
	case strings.Contains(n, "control"):
		return "controlnet"
	case strings.Contains(n, "ip-adapter") || strings.Contains(n, "ip_adapter"):
		return "ip_adapter"
	case strings.Contains(n, "lora"):
		return "lora"
	case strings.Contains(n, "esrgan") || strings.Contains(n, "upscal"):
		return "upscaler"
	case strings.Contains(n, "qwen") && strings.Contains(n, "vl"):
		// Qwen2.5-VL в имени файла — это text encoder, а не diffusion.
		return "llm"
	}
	return "diffusion"
}
