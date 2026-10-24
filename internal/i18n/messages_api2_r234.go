package i18n

func init() { Register(messagesAPI2) }

// messagesAPI2 — последняя порция api-сообщений R234: профили моделей (планшет
// настроек cppworker) и подсказки политики сосуществования image-бэкендов.
// Эти места не попали в первый батч: часть строк — значения JSON-полей
// ("hint"/"message"), часть — склейка с runtime-значением.
var messagesAPI2 = map[string]map[Lang]string{
	"api.cppworker_profiles.context_from_loaded": {
		RU: "профиль модели: contextLength взят из загруженного инстанса",
		EN: "model profile: contextLength taken from the loaded instance",
	},
	"api.cppworker_profiles.context_from_defaults": {
		RU: "профиль модели: contextLength взят из умолчаний (модель не загружена)",
		EN: "model profile: contextLength taken from the defaults (model is not loaded)",
	},
	"api.cppworker_profiles.context_required_new": {
		RU: "contextLength обязателен для нового профиля (нет существующего профиля, который можно домержить)",
		EN: "contextLength is required for a new profile (no existing profile to merge into)",
	},
	"api.cppworker_profiles.upsert_ram_only": {
		RU: "upsertModelProfile: профиль влезает только с выгрузкой в RAM",
		EN: "upsertModelProfile: the profile only fits by offloading to RAM",
	},
	"api.cppworker_profiles.saved_with_defaults": {
		RU: "модель не загружена: contextLength взят из умолчаний (%s). Остальные параметры сохранены и применятся при загрузке.",
		EN: "the model is not loaded: contextLength was taken from the defaults (%s). Other parameters are saved and will apply at load time.",
	},
	"api.cppworker_profiles.apply_model_absent": {
		RU: "applyModelProfile: модели нет на бэкенде — пропуск",
		EN: "applyModelProfile: the model is absent on the backend - skipping",
	},
	"api.cppworker_profiles.not_loaded_on_backend": {
		RU: "модель не загружена на этом бэкенде",
		EN: "the model is not loaded on this backend",
	},
	"api.cppworker_profiles.already_loaded": {
		RU: "текущие параметры модели не изменились (см. runtime-параметры на вкладке загруженных моделей)",
		EN: "the current model parameters did not change (see runtime parameters on the loaded models tab)",
	},
	"api.cppworker_profiles.per_slot_window_reset": {
		RU: "mergeProfileUpdate: окно на слот сброшено — оператор задал суммарное окно и/или число слотов, значение пересчитает cppworker",
		EN: "mergeProfileUpdate: the per-slot window was reset - the operator set the total window and/or the slot count, cppworker will recompute it",
	},

	"api.image_resources.hint_exclusive": {
		RU: "во время генерации image-бэкенд владеет картой: текстовый трафик ждёт (безопасный дефолт)",
		EN: "during generation the image backend owns the card: text traffic waits (the safe default)",
	},
	"api.image_resources.hint_offload": {
		RU: "совместная работа разрешена: применяйте, только если image-воркер запущен с offload в RAM (--offload-to-cpu / --backend te=cpu), иначе рискуете OOM",
		EN: "coexistence allowed: apply only if the image worker runs with offload to RAM (--offload-to-cpu / --backend te=cpu), otherwise you risk OOM",
	},
	"api.image_resources.hint_dedicated": {
		RU: "под image-бэкенд выделена отдельная GPU: ограничения сосуществования не применяются",
		EN: "a dedicated GPU is allocated to the image backend: coexistence limits do not apply",
	},
	"api.image_resources.hint_allow_tool_load": {
		RU: "разрешить текстовой модели поднимать image-модель, которой нет в VRAM (инструмент generate_image + list_image_models); при выключении инструмент объявляется только для уже загруженной модели",
		EN: "allow a text model to load an image model that is not in VRAM (the generate_image tool + list_image_models); when disabled the tool is only advertised for an already loaded model",
	},
	"api.image_resources.hint_tool_load_timeout": {
		RU: "сколько секунд ждать загрузку image-модели, которую поднимает вызов инструмента (большие модели на медленном диске грузятся дольше); по истечении в tool-сообщение уходит причина",
		EN: "how many seconds to wait for loading an image model started by a tool call (large models on a slow disk take longer); after the timeout the reason is sent to the tool message",
	},
}
