package cppbackend

import (
	"testing"
)

// TestPlanRepo_QwenImageMissingCompanions — ЖИВОЙ СЛУЧАЙ (2026-10-06): оператор
// скачал только diffusion-файл Qwen-Image 2.1 и получил
//
//	ERROR Conditioner model tensor 'text_encoders.llm.model.embed_tokens.weight' not in model metadata
//	ERROR VAE tensor 'first_stage_model.conv1.weight' not in model metadata
//	ERROR model metadata validation failed / new_sd_ctx_t failed
//
// Паспорт репозитория обязан заранее сказать: режим --diffusion-model, нужны роли
// vae и llm, их нет → набор неполный, и перечислить шаги.
func TestPlanRepo_QwenImageMissingCompanions(t *testing.T) {
	files := []HFFileInfo{
		{Path: "qwen-image-2.1-UC-Q4_K_M.gguf", SizeBytes: 4604558112, SuggestedRole: "diffusion"},
	}
	plan := planFromFilesForTest(t, "abenzerps/Qwen-Image-2.1-Uncensored-GGUF", files, "qwen_image", "Qwen Image 2.1", true)

	if plan.EngineMode != "diffusion-model" {
		t.Fatalf("engineMode=%q, want diffusion-model", plan.EngineMode)
	}
	if plan.Usable {
		t.Fatal("набор без VAE и text encoder не может быть пригодным")
	}
	if plan.Verdict != "incomplete" {
		t.Fatalf("verdict=%q, want incomplete (%s)", plan.Verdict, plan.Summary)
	}
	if len(plan.Missing) != 2 {
		t.Fatalf("missing=%v, want VAE + text encoder", plan.Missing)
	}
	joined := plan.Summary
	for _, want := range []string{"--diffusion-model", "VAE", "text encoder"} {
		if !containsStr(joined, want) {
			t.Errorf("в вердикте нет %q: %s", want, joined)
		}
	}
	// Шаги: пометить файлы, роли, семейство, скачать, загрузить и что делать при
	// ошибке движка — это то, что оператор делает в WebUI.
	steps := joinStrings(plan.Steps, " | ")
	for _, want := range []string{"qwen-image-2.1-UC-Q4_K_M.gguf", "qwen_image", "Скачать bundle", "Загрузить"} {
		if !containsStr(steps, want) {
			t.Errorf("в шагах нет %q: %s", want, steps)
		}
	}
	// Пока набор неполный, подсказки модели по имени быть не должно.
	if plan.ModelHint != "" {
		t.Errorf("при неполном наборе подсказка модели не нужна: %s", plan.ModelHint)
	}
}

// TestPlanRepo_CompleteDiTSetIsReady — полный набор (diffusion + vae + llm):
// вердикт ready, режим --diffusion-model, есть подсказка модели с именем каталога.
func TestPlanRepo_CompleteDiTSetIsReady(t *testing.T) {
	files := []HFFileInfo{
		{Path: "qwen-image-2.1-UC-Q4_K_M.gguf", SizeBytes: 4604558112, SuggestedRole: "diffusion"},
		{Path: "text_encoders/qwen2.5-vl-7b-Q4_K_M.gguf", SizeBytes: 4700000000, SuggestedRole: "llm"},
		{Path: "qwen_image_2.1_vae_bf16.safetensors", SizeBytes: 675509688, SuggestedRole: "vae"},
	}
	plan := planFromFilesForTest(t, "leejet/Qwen-Image-2.1-GGUF", files, "qwen_image", "Qwen Image 2.1", true)

	if !plan.Usable || plan.Verdict != "ready" {
		t.Fatalf("verdict=%q usable=%v (%s), want ready", plan.Verdict, plan.Usable, plan.Summary)
	}
	if len(plan.Missing) != 0 {
		t.Fatalf("missing=%v, want пусто", plan.Missing)
	}
	if !containsStr(plan.Summary, "Qwen Image 2.1") || !containsStr(plan.Summary, "--diffusion-model") {
		t.Errorf("сводка должна называть семейство и режим: %s", plan.Summary)
	}
	if !containsStr(plan.ModelHint, "qwen-image-2.1-UC-Q4_K_M") {
		t.Errorf("подсказка модели должна называть имя каталога bundle: %s", plan.ModelHint)
	}
	if !containsStr(plan.ModelHint, "list_image_models") {
		t.Errorf("подсказка модели должна отсылать к каталогу инструмента: %s", plan.ModelHint)
	}
}

// TestPlanRepo_FLUXWithT5DoesNotRequireLLM — у FLUX text encoder собран как T5+CLIP-L:
// отсутствие роли llm не делает набор неполным.
func TestPlanRepo_FLUXWithT5DoesNotRequireLLM(t *testing.T) {
	files := []HFFileInfo{
		{Path: "flux1-schnell-q4_k.gguf", SizeBytes: 4000000000, SuggestedRole: "diffusion"},
		{Path: "text_encoders/t5-v1_1-xxl-encoder-Q4_K_M.gguf", SizeBytes: 2896123072, SuggestedRole: "t5xxl"},
		{Path: "text_encoders/clip_l.safetensors", SizeBytes: 246144152, SuggestedRole: "clip_l"},
		{Path: "ae.safetensors", SizeBytes: 335304388, SuggestedRole: "vae"},
	}
	plan := planFromFilesForTest(t, "leejet/FLUX.1-schnell-gguf", files, "flux", "Flux", true)
	for _, r := range plan.Roles {
		if r.Role == "llm" && r.Required {
			t.Fatalf("для FLUX роль llm не обязательна: %+v", r)
		}
	}
	if len(plan.Missing) != 0 {
		t.Fatalf("missing=%v, want пусто", plan.Missing)
	}
}

// TestPlanRepo_AllInOneNeedsNothingElse — all-in-one (SD1.5/SDXL): режим --model,
// отдельные VAE/TE не требуются.
func TestPlanRepo_AllInOneNeedsNothingElse(t *testing.T) {
	files := []HFFileInfo{
		{Path: "stable-diffusion-v1-5-pruned-emaonly-Q4_0.gguf", SizeBytes: 1566768416, SuggestedRole: "diffusion"},
	}
	plan := planFromFilesForTest(t, "second-state/stable-diffusion-v1-5-GGUF", files, "sd15", "SD1.x", false)
	if plan.EngineMode != "model" {
		t.Fatalf("engineMode=%q, want model", plan.EngineMode)
	}
	if !plan.Usable || plan.Verdict != "ready" {
		t.Fatalf("all-in-one набор должен быть готов: %q %v (%s)", plan.Verdict, plan.Usable, plan.Summary)
	}
	if len(plan.Missing) != 0 {
		t.Fatalf("missing=%v, want пусто", plan.Missing)
	}
}

// TestPlanRepo_NoDiffusionFile — репозиторий без diffusion-файла: вердикт unknown
// с понятным текстом (не «ready»).
func TestPlanRepo_NoDiffusionFile(t *testing.T) {
	files := []HFFileInfo{{Path: "README.md", SizeBytes: 1000}}
	plan := planFromFilesForTest(t, "someone/empty", files, "", "", false)
	if plan.Verdict != "unknown" || plan.Usable {
		t.Fatalf("verdict=%q usable=%v (%s)", plan.Verdict, plan.Usable, plan.Summary)
	}
	if !containsStr(plan.Summary, "diffusion") {
		t.Errorf("сводка должна объяснять отсутствие весов: %s", plan.Summary)
	}
}

// --- helpers -----------------------------------------------------------------

// planFromFilesForTest — паспорт по ЗАДАННЫМ файлам и ЗАДАННОМУ вердикту
// пред-проверки (сеть не нужна: probeInto подменяем вручную).
func planFromFilesForTest(t *testing.T, modelID string, files []HFFileInfo, family, version string, dit bool) *HFRepoPlan {
	t.Helper()
	plan := &HFRepoPlan{ModelID: modelID, Revision: "main"}
	main := mainDiffusionCandidate(files)
	plan.MainFile = main
	if main != "" && family != "" {
		plan.Family = family
		plan.VersionLabel = version
		if dit {
			plan.EngineMode = "diffusion-model"
		} else {
			plan.EngineMode = "model"
		}
	}
	plan.Roles = planRoles(files, plan.Family, plan.EngineMode)
	for _, r := range plan.Roles {
		if r.Required && !r.Found {
			plan.Missing = append(plan.Missing, r.Label)
		}
	}
	if plan.EngineMode == "" {
		plan.EngineMode = "unknown"
	}
	plan.Usable = plan.Verdict != "unsupported" && len(plan.Missing) == 0 && plan.MainFile != ""
	plan.Verdict, plan.Summary = planVerdict(plan)
	plan.Steps = planSteps(plan)
	plan.ModelHint = planModelHint(plan)
	return plan
}

func joinStrings(in []string, sep string) string {
	out := ""
	for i, s := range in {
		if i > 0 {
			out += sep
		}
		out += s
	}
	return out
}

func containsStr(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && stringIndex(haystack, needle) >= 0
}

func stringIndex(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
