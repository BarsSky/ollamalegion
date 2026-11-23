// hf_probe_role.go — R696 (2026-11-23): РОЛЬ файла модели по его СОДЕРЖИМОМУ.
//
// ЗАЧЕМ. Живой дефект на локальном стенде: оператор скачал на вкладке HuggingFace
// файл `Qwen-Image-2.1-Turbo-Uncensored-AD-Q4_K.gguf` из репозитория
// AtomicChat/Qwen-Image-2.1-Turbo-Uncensored-GGUF, воркер положил его в bundle,
// СИНТЕЗИРОВАЛ профиль по имени файла (роль diffusion, family=other) и запустил
// sd-server как all-in-one:
//
//	[out] loading model from '.../Qwen-Image-2.1-Turbo-Uncensored-AD-Q4_K.gguf'
//	[err] [ERROR] diffusion_engine.cpp:992 - get sd version from file failed
//	[err] [ERROR] main.cpp:93 - new_sd_ctx_t failed
//
// При этом сам файл ИСПРАВЕН: в его GGUF-заголовке `general.architecture=qwen3vl`,
// `general.size_label=8.2B`, теги image-text-to-text — это TEXT ENCODER Qwen-Image
// 2.1 (Qwen3-VL-8B), который движок подключает флагом `--llm`, а не
// `--diffusion-model`. То есть «не грузится модель» = в наборе нет diffusion-файла,
// а имя файла об этом не говорит.
//
// ЧТО ЗДЕСЬ: одна функция classifySpecialRole, которая по заголовку (arch + имена
// тензоров) отвечает «это VAE / LLM-энкодер / CLIP / T5 / CLIP-vision», и
// ProbeLocalFile — чтение того же заголовка с ЛОКАЛЬНОГО диска (для уже
// скачанных bundle'ов). Разбор байтов не дублируется: он живёт в hf_probe.go.
//
// ВАЖНО ПРО ДОВЕРИЕ. Роль энкодера определяется по arch и/или по НАБОРУ имён
// тензоров, и это сильное, но не абсолютное свидетельство: у all-in-one чекпойнта
// SDXL внутри тоже есть CLIP (`conditioner.embedders.0...`). Поэтому
// classifySpecialRole вызывается ТОЛЬКО когда семейство diffusion по тензорам не
// опознано (см. applySpecialRole в hf_probe.go) — тогда «внутренний CLIP» уже
// учтён версией модели.
package cppbackend

import (
	"os"
	"sort"
	"strings"

	"ollama-loadbalancer/internal/i18n"
)

// Роли файлов, которые отдаёт пред-проверка (значения совпадают с
// pkg/types.ImageFileRole*, чтобы sdworker мог сравнивать их напрямую).
const (
	ProbeRoleDiffusion  = "diffusion"
	ProbeRoleVae        = "vae"
	ProbeRoleLLM        = "llm"
	ProbeRoleClipL      = "clip_l"
	ProbeRoleClipG      = "clip_g"
	ProbeRoleT5xxl      = "t5xxl"
	ProbeRoleClipVision = "clip_vision"
)

// Источники семейства: движок версию по general.architecture НЕ читает (см.
// docs/features/image-generation.ru.md §8.2), поэтому семейство, выведенное из
// arch, помечается отдельно — UI/валидатор может сказать «это подсказка, а не
// факт».
const (
	ProbeFamilyFromTensors = "tensors"
	ProbeFamilyFromArch    = "arch"
)

// ProbeHead — разбор заголовка файла БЕЗ сети и без обвязки ProbeFile.
//
// Экспортируется для sdworker (локальные bundle'ы) и тестов: именно эта функция
// решает, какую роль играет файл, и она обязана проверяться на реальных байтах
// заголовков, а не через HTTP-мок.
func ProbeHead(head []byte, filename string) *HFProbeResult {
	return probeHeadInto(&HFProbeResult{}, head, filename)
}

// ProbeLocalFile — то же, что ProbeFile, но для файла на диске: читаем только
// начало (заголовок), веса не трогаем.
//
// ПОВЕДЕНИЕ НА БОЛЬШИХ ЗАГОЛОВКАХ: GGUF-файл с большим словарём (Qwen3-VL: 151k
// токенов) не помещает таблицу тензоров в 512 КБ, и первый разбор возвращает
// Truncated=true с пустыми именами. Тогда читаем больше (до 8 МБ): для
// локального файла это дешёво, а без тензоров нельзя ни подтвердить роль
// diffusion, ни отличить его от текстового энкодера. У HF-пред-проверки лимит
// прежний (один Range-запрос, 512 КБ) — там лишние мегабайты стоят трафика.
func ProbeLocalFile(path, filename string) (*HFProbeResult, error) {
	if filename == "" {
		filename = path
	}
	head, err := readHeadPrefix(path, HFProbeRangeBytes)
	if err != nil {
		return nil, err
	}
	res := ProbeHead(head, filename)
	// Заголовок safetensors может быть длиннее 512 КБ (много тензоров): длина
	// записана в первых 8 байтах, поэтому её видно ещё до полного чтения.
	if res.Format == "safetensors" && res.Verdict == HFProbeUnknown && len(head) >= 8 {
		if need := int(safetensorsHeaderLen(head)) + 8; need > len(head) && need <= 64<<20 {
			if bigger, rerr := readHeadPrefix(path, need); rerr == nil {
				head = bigger
				res = ProbeHead(head, filename)
			}
		}
	}
	// GGUF: тензорная таблица не поместилась — читаем больше (до 8 МБ).
	if res.Format == "gguf" && res.Truncated && res.Tensors > 0 && len(head) < 8<<20 {
		if bigger, rerr := readHeadPrefix(path, 8<<20); rerr == nil && len(bigger) > len(head) {
			if res2 := ProbeHead(bigger, filename); res2.Truncated == false || len(res2.Prefixes) > len(res.Prefixes) {
				res = res2
			}
		}
	}
	res.CheckedB = len(head)
	return res, nil
}

// readHeadPrefix читает не больше limit байт с начала файла.
func readHeadPrefix(path string, limit int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, limit)
	n, rerr := f.Read(buf)
	if rerr != nil && n == 0 {
		return nil, rerr
	}
	return buf[:n], nil
}

// safetensorsHeaderLen — длина JSON-заголовка из первых 8 байт (0, если не похоже).
func safetensorsHeaderLen(head []byte) uint64 {
	if len(head) < 8 {
		return 0
	}
	// little-endian uint64 без импорта binary в этом файле: 8 байт вручную.
	var n uint64
	for i := 7; i >= 0; i-- {
		n = n<<8 | uint64(head[i])
	}
	return n
}

// applySpecialRole — роль файла по заголовку, когда diffusion-семейство по
// тензорам НЕ опознано. Для распознанного diffusion-файла роль всегда diffusion:
// «внутренние» CLIP/VAE в all-in-one чекпойнтах — часть модели, а не отдельные роли.
func applySpecialRole(res *HFProbeResult, arch string, ts tensorSet) {
	if res == nil {
		return
	}
	if res.Version != "" {
		res.Role = ProbeRoleDiffusion
		res.FamilySource = ProbeFamilyFromTensors
		return
	}
	// Семейство по general.architecture (движок его не читает — это подсказка).
	if fam, ver, dit, ok := familyFromArch(arch); ok {
		res.Family = fam
		res.Version = ver
		res.DiT = dit
		res.Role = ProbeRoleDiffusion
		res.FamilySource = ProbeFamilyFromArch
		res.Reason = i18n.T("cppbackend.hf_probe.reason_arch_family", strings.TrimSpace(arch), ver)
		return
	}
	role, reason := classifySpecialRole(arch, ts)
	res.Role = role
	if role != "" && reason != "" {
		res.Reason = reason
	}
}

// classifySpecialRole — роль не-diffusion файла: VAE / LLM-энкодер / CLIP / T5 /
// CLIP-vision. Пустая роль = по заголовку не определить (LoRA, незнакомый файл).
func classifySpecialRole(arch string, ts tensorSet) (role, reason string) {
	a := strings.ToLower(strings.TrimSpace(arch))
	switch {
	case a != "" && (strings.Contains(a, "vae") || strings.Contains(a, "autoencoder")):
		return ProbeRoleVae, i18n.T("cppbackend.hf_probe.role_vae_arch", arch)
	case a != "" && (strings.Contains(a, "clip_vision") || strings.Contains(a, "clip-vision")):
		return ProbeRoleClipVision, i18n.T("cppbackend.hf_probe.role_clip_vision_arch", arch)
	case a != "" && strings.Contains(a, "t5"):
		return ProbeRoleT5xxl, i18n.T("cppbackend.hf_probe.role_t5_arch", arch)
	case a != "" && strings.Contains(a, "clip"):
		if dim := ts.dim0("token_embedding.weight"); dim == 1280 {
			return ProbeRoleClipG, i18n.T("cppbackend.hf_probe.role_clip_g_arch", arch)
		}
		return ProbeRoleClipL, i18n.T("cppbackend.hf_probe.role_clip_l_arch", arch)
	case a != "" && isLLMArch(a):
		return ProbeRoleLLM, i18n.T("cppbackend.hf_probe.role_llm_arch", arch)
	}

	// Архитектуры нет (частый случай safetensors): решаем по именам тензоров.
	switch {
	case ts.has("text_encoders.llm.") ||
		(ts.has("model.embed_tokens.weight") && ts.has("model.layers.")):
		return ProbeRoleLLM, i18n.T("cppbackend.hf_probe.role_llm_tensors")
	case ts.has("encoder.block.") && (ts.has("shared.") || ts.has("lm_head.")):
		return ProbeRoleT5xxl, i18n.T("cppbackend.hf_probe.role_t5_tensors")
	case ts.has("text_model.encoder.layers."):
		if dim := ts.dim0("token_embedding.weight"); dim == 1280 {
			return ProbeRoleClipG, i18n.T("cppbackend.hf_probe.role_clip_tensors")
		}
		return ProbeRoleClipL, i18n.T("cppbackend.hf_probe.role_clip_tensors")
	case ts.has("quant_conv") || ts.has("post_quant_conv") ||
		(ts.has("decoder.conv_in") && ts.has("encoder.conv_in")):
		return ProbeRoleVae, i18n.T("cppbackend.hf_probe.role_vae_tensors")
	}
	return "", ""
}

// isLLMArch — arch похожа на языковую модель (text encoder семейств Qwen-Image /
// Z-Image / FLUX.2). Проверяются префиксы, а не подстроки «qwen»/«llama»: arch
// diffusion-модели qwen-image выглядит как `qwen_image21`, и подстрочный матч
// принял бы её за энкодер.
func isLLMArch(a string) bool {
	for _, prefix := range []string{
		"qwen3vl", "qwen2vl", "qwen2.5vl", "qwen2_5_vl", "qwen3_vl", "qwenvl",
		"llama", "mistral", "gemma", "deepseek", "phi", "internlm", "glm",
		"minicpm", "olmo", "smollm",
	} {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}

// familyFromArch — семейство и версия по `general.architecture` GGUF.
//
// ЭТО ПОДСКАЗКА, А НЕ ФАКТ: pinned sd.cpp версию модели по arch не определяет
// (docs/features/image-generation.ru.md §8.2), он смотрит имена тензоров. Но arch
// пишут конвертеры официальных сборок, и когда тензорная таблица не помещается в
// прочитанный заголовок, это единственный сигнал «файл — diffusion, а не энкодер».
func familyFromArch(arch string) (family, version string, dit, ok bool) {
	a := strings.ToLower(strings.TrimSpace(arch))
	if a == "" {
		return "", "", false, false
	}
	compact := strings.NewReplacer("-", "_", " ", "_", ".", "_").Replace(a)
	switch {
	case strings.HasPrefix(compact, "qwen_image"):
		if strings.Contains(compact, "21") || strings.Contains(compact, "2_1") {
			return "qwen_image", "Qwen Image 2.1", true, true
		}
		return "qwen_image", "Qwen Image", true, true
	case strings.HasPrefix(compact, "flux2") || strings.HasPrefix(compact, "flux_2"):
		return "flux2", "Flux 2", true, true
	case strings.HasPrefix(compact, "flux"):
		return "flux", "Flux", true, true
	case strings.HasPrefix(compact, "sd3") || strings.HasPrefix(compact, "stable_diffusion_3"):
		return "sd3", "SD3.x", true, true
	case strings.HasPrefix(compact, "z_image") || strings.HasPrefix(compact, "zimage"):
		return "z_image", "Z-Image", true, true
	case strings.HasPrefix(compact, "chroma"):
		return "chroma", "Chroma", true, true
	case strings.HasPrefix(compact, "stable_diffusion_xl") || strings.HasPrefix(compact, "sdxl"):
		return "sdxl", "SDXL", false, true
	case strings.HasPrefix(compact, "stable_diffusion_v1") || strings.HasPrefix(compact, "sd15") ||
		strings.HasPrefix(compact, "stable_diffusion_v2") || strings.HasPrefix(compact, "sd21"):
		fam := "sd15"
		if strings.Contains(compact, "v2") || strings.Contains(compact, "21") {
			fam = "sd21"
		}
		return fam, "SD1.x/2.x", false, true
	}
	return "", "", false, false
}

// SortedPrefixes — отсортированные имена-префиксы тензоров (для сообщений UI).
func SortedPrefixes(prefixes []string) []string {
	out := append([]string(nil), prefixes...)
	sort.Strings(out)
	return out
}
