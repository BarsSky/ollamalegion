package i18n

func init() { Register(messagesImageBundleR696) }

// messagesImageBundleR696 — R696 (2026-11-23): вердикт совместимости image-bundle.
//
// Живой дефект, ради которого появился каталог: на локальном стенде оператор
// скачал с HuggingFace ОДИН файл `Qwen-Image-2.1-Turbo-Uncensored-AD-Q4_K.gguf` и
// получил на загрузке «get sd version from file failed». Файл — текстовый
// энкодер Qwen3-VL (general.architecture="qwen3vl"), а воркер подключил его как
// --diffusion-model, потому что роль бралась из ИМЕНИ файла. Теперь роль и
// семейство определяются по заголовку, а вердикт «сможет ли imageworker
// загрузить набор» выводится ДО загрузки — эти строки его объясняют.
var messagesImageBundleR696 = map[string]map[Lang]string{
	// === internal/cppbackend/hf_probe_role.go: роль файла по заголовку ======
	"cppbackend.hf_probe.role_vae_arch": {
		RU: "в заголовке архитектура %q — это VAE, а не diffusion-модель (подключается флагом --vae)",
		EN: "the header architecture is %q — this is a VAE, not a diffusion model (attached with --vae)",
	},
	"cppbackend.hf_probe.role_clip_vision_arch": {
		RU: "в заголовке архитектура %q — это CLIP-vision (для IP-Adapter/PhotoMaker), а не diffusion-модель",
		EN: "the header architecture is %q — this is CLIP-vision (for IP-Adapter/PhotoMaker), not a diffusion model",
	},
	"cppbackend.hf_probe.role_t5_arch": {
		RU: "в заголовке архитектура %q — это текстовый энкодер T5, а не diffusion-модель (подключается флагом --t5xxl)",
		EN: "the header architecture is %q — this is a T5 text encoder, not a diffusion model (attached with --t5xxl)",
	},
	"cppbackend.hf_probe.role_clip_g_arch": {
		RU: "в заголовке архитектура %q и эмбеддинг 1280 — это CLIP-G текстовый энкодер (--clip_g), а не diffusion-модель",
		EN: "the header architecture is %q with a 1280 embedding — this is the CLIP-G text encoder (--clip_g), not a diffusion model",
	},
	"cppbackend.hf_probe.role_clip_l_arch": {
		RU: "в заголовке архитектура %q — это CLIP-L текстовый энкодер (--clip_l), а не diffusion-модель",
		EN: "the header architecture is %q — this is the CLIP-L text encoder (--clip_l), not a diffusion model",
	},
	"cppbackend.hf_probe.role_llm_arch": {
		RU: "в заголовке архитектура %q — это текстовый энкодер (LLM), а не diffusion-модель: в наборе он подключается флагом --llm",
		EN: "the header architecture is %q — this is a text encoder (LLM), not a diffusion model: in a bundle it is attached with --llm",
	},
	"cppbackend.hf_probe.role_llm_tensors": {
		RU: "по именам тензоров это текстовый энкодер (LLM), а не diffusion-модель: в наборе он подключается флагом --llm",
		EN: "the tensor names look like a text encoder (LLM), not a diffusion model: in a bundle it is attached with --llm",
	},
	"cppbackend.hf_probe.role_t5_tensors": {
		RU: "по именам тензоров это текстовый энкодер T5, а не diffusion-модель (флаг --t5xxl)",
		EN: "the tensor names look like a T5 text encoder, not a diffusion model (--t5xxl)",
	},
	"cppbackend.hf_probe.role_clip_tensors": {
		RU: "по именам тензоров это текстовый энкодер CLIP, а не diffusion-модель (флаги --clip_l/--clip_g)",
		EN: "the tensor names look like a CLIP text encoder, not a diffusion model (--clip_l/--clip_g)",
	},
	"cppbackend.hf_probe.role_vae_tensors": {
		RU: "по именам тензоров это VAE (encoder/decoder/quant_conv), а не diffusion-модель (флаг --vae)",
		EN: "the tensor names look like a VAE (encoder/decoder/quant_conv), not a diffusion model (--vae)",
	},
	"cppbackend.hf_probe.reason_arch_family": {
		RU: "семейство выведено из general.architecture=%q («%s»): движок версию по arch не читает — это подсказка, а не факт",
		EN: "the family is derived from general.architecture=%q (\"%s\"): the engine does not read the version from arch — this is a hint, not a fact",
	},

	// === internal/sdbackend/compat.go: вердикт по набору ====================
	"sdworker.compat.no_profile": {
		RU: "профиль модели не найден",
		EN: "the model profile was not found",
	},
	"sdworker.compat.no_diffusion": {
		RU: "в наборе нет diffusion-файла (роль diffusion): без него движку нечего загружать",
		EN: "the bundle has no diffusion file (role diffusion): the engine has nothing to load without it",
	},
	"sdworker.compat.diffusion_is_encoder": {
		RU: "файл %q — это %s (заголовок: %s), а не diffusion-модель: в наборе он подключается ролью llm (флаг --llm). Движок получит --model/--diffusion-model и ответит «get sd version from file failed» — в наборе не хватает собственно diffusion-файла",
		EN: "file %q is a %s (header: %s), not a diffusion model: in a bundle it is attached with role llm (--llm). The engine will get --model/--diffusion-model and answer \"get sd version from file failed\" — the bundle is missing the diffusion file itself",
	},
	"sdworker.compat.family_mismatch_dit": {
		RU: "файл %q — DiT-модель «%s», а в профиле семейство %q: движок получит --model и ответит «get sd version from file failed». Поставьте family=qwen_image/flux/sd3 (DiT-семейство)",
		EN: "file %q is the DiT model \"%s\", but the profile family is %q: the engine will get --model and answer \"get sd version from file failed\". Set family to a DiT family (qwen_image/flux/sd3)",
	},
	"sdworker.compat.incomplete": {
		RU: "движок запустит набор %s, но не хватает: %s",
		EN: "the engine will start the bundle %s, but these are missing: %s",
	},
	"sdworker.compat.unknown_family": {
		RU: "набор собран, но семейство по заголовкам не определяется: если это DiT-модель (FLUX/SD3/Qwen-Image), движок ответит «get sd version from file failed» — выберите семейство в профиле",
		EN: "the bundle is complete, but the family cannot be determined from the headers: if this is a DiT model (FLUX/SD3/Qwen-Image), the engine will answer \"get sd version from file failed\" — set the family in the profile",
	},
	"sdworker.compat.arch_only": {
		RU: "набор похож на «%s», но семейство выведено только из general.architecture — движок версию по нему не читает; проверьте загрузку",
		EN: "the bundle looks like \"%s\", but the family was derived only from general.architecture — the engine does not read the version from it; verify by loading",
	},
	"sdworker.compat.ready": {
		RU: "движок узнаёт «%s» и запустит набор %s",
		EN: "the engine recognises \"%s\" and will start the bundle %s",
	},
	"sdworker.compat.ready_no_version": {
		RU: "все обязательные роли на месте, движок запустит набор %s",
		EN: "all required roles are present, the engine will start the bundle %s",
	},
	"sdworker.compat.warn_family_from_arch": {
		RU: "семейство определено по general.architecture (движок читает только имена тензоров) — вердикт предварительный",
		EN: "the family was determined from general.architecture (the engine reads only tensor names) — the verdict is preliminary",
	},
	"sdworker.compat.warn_role_mismatch": {
		RU: "у файла %q роль %s, а по заголовку это %s",
		EN: "file %q has role %s, but its header says %s",
	},
	"sdworker.compat.warn_dit_without_vae": {
		RU: "семейство %q требует отдельного VAE, но роли vae в наборе нет",
		EN: "family %q requires a separate VAE, but the bundle has no vae role",
	},
	"sdworker.compat.line_yes": {
		RU: "imageworker сможет загрузить модель %q: %s",
		EN: "imageworker will be able to load model %q: %s",
	},
	"sdworker.compat.line_no": {
		RU: "imageworker НЕ сможет загрузить модель %q: %s",
		EN: "imageworker will NOT be able to load model %q: %s",
	},
	"sdworker.compat.load_blocked": {
		RU: "загрузка остановлена до запуска движка: %s",
		EN: "loading was stopped before starting the engine: %s",
	},
	"sdworker.compat.load_blocked_hint": {
		RU: "набор не совместим с imageworker: исправьте состав/роли файлов (вкладка «Модели»/«HuggingFace» показывает вердикт) либо повторите с force=true, если уверены в профиле",
		EN: "the bundle is not compatible with imageworker: fix the files/roles (the Models/HuggingFace tab shows the verdict) or retry with force=true if you trust the profile",
	},
	"sdworker.compat.plan_main_is_encoder": {
		RU: "главный файл репозитория %q — это %s (заголовок: %s), а не diffusion-модель: imageworker не сможет загрузить такой репозиторий как модель",
		EN: "the repository's main file %q is a %s (header: %s), not a diffusion model: imageworker will not be able to load this repository as a model",
	},
	"sdworker.compat.plan_need_diffusion_hint": {
		RU: "в репозитории должны быть diffusion-веса (GGUF для DiT: qwen_image/flux/sd3), а этот файл подходит только ролью llm/VAE",
		EN: "the repository must contain diffusion weights (GGUF for DiT: qwen_image/flux/sd3); this file only fits the llm/VAE role",
	},
}
