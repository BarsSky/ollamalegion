package i18n

// messages_balancer2 — R234 (2026-10-24): сообщения балансера, батч r234 (2).
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ФАЙЛ. Каталог пополняется батчами по областям (см.
// register.go): параллельные правки одного messages.go конфликтуют. Здесь живут
// ключи файлов internal/balancer/{preflight_nctx,llamacpp_handlers_inference,
// nctx_reload_handlers,llamacpp_transport,unified_queue_r73,
// llamacpp_backend_helpers,nctx_reload,num_ctx_resolver,placement_replication_r74,
// proxy}.go.
//
// ПРАВИЛА. Ключ — "<компонент>.<файл>.<смысл>"; RU — исходный текст байт-в-байт
// (тесты и операторские инструкции ссылаются на него), EN — тот же смысл с тем же
// набором fmt-глаголов (TestCatalogVerbParity). Поля логов вроде reason/hint/note
// переведены так же, как сам лог: это диагностика оператора, а не данные модели.
var messagesBalancer2 = map[string]map[Lang]string{
	// === preflight_nctx.go: preflight-проверка n_ctx ==================
	"balancer.preflight_nctx.target_window_recalculated": {
		RU: "preflight: целевое окно пересчитано «на клиента → суммарно»",
		EN: "preflight: target window recalculated from per-client to total",
	},
	"balancer.preflight_nctx.strategy_cpu_only_warn": {
		RU: "preflight: стратегия предлагает cpu_only — окно растим, " +
			"раскладку решает cppworker (gpuLayers=0 для него = «из профиля»)",
		EN: "preflight: the strategy proposes cpu_only — the window is still grown, " +
			"the layout is decided by cppworker (gpuLayers=0 means \"from the profile\" for it)",
	},
	"balancer.preflight_nctx.growth_not_worth_reason": {
		RU: "стратегия даёт суммарное окно %d при текущем %d (на клиента %d): " +
			"reload не изменил бы окно, но выгрузил бы модель на 1–3 минуты",
		EN: "the strategy yields a total window of %d against the current %d (%d per client): " +
			"a reload would not change the window but would unload the model for 1–3 minutes",
	},
	"balancer.preflight_nctx.param_mismatch_reject_warn": {
		RU: "preflight: отказ — параметры загруженной модели не совпадают с требованием запроса",
		EN: "preflight: reject — the loaded model parameters do not match the request requirement",
	},
	"balancer.preflight_nctx.client_silent_no_reload_info": {
		RU: "preflight: клиент не указал num_ctx — модель не перезагружаем",
		EN: "preflight: the client did not specify num_ctx — the model is not reloaded",
	},
	"balancer.preflight_nctx.model_not_loaded_load_with_client_window_info": {
		RU: "preflight: модель не загружена — грузим окном клиента",
		EN: "preflight: the model is not loaded — loading it with the client window",
	},
	"balancer.preflight_nctx.loaded_window_covers_request_info": {
		RU: "preflight: загруженное окно не меньше запрошенного — reload не нужен",
		EN: "preflight: the loaded window is not smaller than the requested one — no reload needed",
	},
	"balancer.preflight_nctx.requested_window_above_physical_ceiling_warn": {
		RU: "preflight: запрошенное окно больше физического потолка — отказ",
		EN: "preflight: the requested window exceeds the physical ceiling — reject",
	},
	"balancer.preflight_nctx.model_supports_max_context": {
		RU: "модель поддерживает не более %d токенов контекста",
		EN: "the model supports at most %d tokens of context",
	},
	"balancer.preflight_nctx.requested_window_above_operator_ceiling_warn": {
		RU: "preflight: запрошенное окно больше операторского потолка — отказ",
		EN: "preflight: the requested window exceeds the operator ceiling — reject",
	},
	"balancer.preflight_nctx.auto_reload_ceiling_reason": {
		RU: "в настройках балансера задан потолок авто-перезагрузки %d токенов (LB_NCTX_RELOAD_MAX_N_CTX)",
		EN: "the balancer settings define an auto-reload ceiling of %d tokens (LB_NCTX_RELOAD_MAX_N_CTX)",
	},
	"balancer.preflight_nctx.reload_for_client_window_info": {
		RU: "preflight: загруженное окно меньше запрошенного — перезагружаем под клиента",
		EN: "preflight: the loaded window is smaller than the requested one — reloading for the client",
	},
	"balancer.preflight_nctx.reasoning_enabled": {
		RU: "включено",
		EN: "enabled",
	},
	"balancer.preflight_nctx.reasoning_disabled": {
		RU: "выключено",
		EN: "disabled",
	},
	"balancer.preflight_nctx.reasoning_mismatch_explain": {
		RU: "Запрос требует %s размышление (reasoning), а модель %s уже загружена с %s.",
		EN: "The request requires %s reasoning, while model %s is already loaded with %s.",
	},
	"balancer.preflight_nctx.param_diff_line": {
		RU: "%s: загружено «%s», запрос требует «%s»",
		EN: "%s: loaded \"%s\", the request requires \"%s\"",
	},
	"balancer.preflight_nctx.param_mismatch_error": {
		RU: "Модель %s загружена с другими параметрами, чем требует запрос: %s. " +
			"Перезагрузка под этот запрос не выполняется — измените параметр в клиенте " +
			"или перезагрузите модель с нужными настройками в WebUI.",
		EN: "Model %s is loaded with parameters different from what the request requires: %s. " +
			"No reload is performed for this request — change the parameter in the client " +
			"or reload the model with the required settings in the WebUI.",
	},
	"balancer.preflight_nctx.param_mismatch_what_to_do": {
		RU: "Измените параметр в клиенте (например, уберите think=true) — либо " +
			"перезагрузите модель с этим параметром: WebUI → Настройки → llama.cpp → профиль " +
			"модели → Enable reasoning → «Применить (reload)».",
		EN: "Change the parameter in the client (for example, remove think=true) — or " +
			"reload the model with this parameter: WebUI → Settings → llama.cpp → model " +
			"profile → Enable reasoning → \"Apply (reload)\".",
	},
	"balancer.preflight_nctx.window_too_big_error": {
		RU: "Запрошенное контекстное окно %d токенов на клиента не поместится: %s. " +
			"При %d параллельных слотах суммарное окно %d делится между клиентами, " +
			"поэтому одному клиенту доступно не больше %d токенов.",
		EN: "The requested context window of %d tokens per client does not fit: %s. " +
			"With %d parallel slots the total window of %d is split between clients, " +
			"so a single client gets no more than %d tokens.",
	},
	"balancer.preflight_nctx.window_too_big_reason": {
		RU: "требуется %d (суммарно %d), доступно не более %d на клиента",
		EN: "required %d (%d in total), at most %d available per client",
	},
	"balancer.preflight_nctx.window_too_big_what_to_do": {
		RU: "Уменьшите окно в клиенте (до %d или меньше), либо поставьте параллельные слоты в 1 " +
			"(тогда всё окно достанется одному клиенту), либо поднимите потолок " +
			"(профиль модели → contextLengthMax / LB_NCTX_RELOAD_MAX_N_CTX).",
		EN: "Reduce the window in the client (to %d or less), or set parallel slots to 1 " +
			"(then the whole window goes to a single client), or raise the ceiling " +
			"(model profile → contextLengthMax / LB_NCTX_RELOAD_MAX_N_CTX).",
	},
	"balancer.preflight_nctx.wait_env_unrecognized_warn": {
		RU: "LB_NCTX_PREFLIGHT_WAIT_SEC не распознан — ждём терминального состояния reload",
		EN: "LB_NCTX_PREFLIGHT_WAIT_SEC is not recognized — waiting for the terminal reload state",
	},
	"balancer.preflight_nctx.wait_cap_armed_warn": {
		RU: "armed preflight wait cap (operator opt-in): первый запрос клиента " +
			"получит 503, если reload не успеет за этот срок",
		EN: "armed preflight wait cap (operator opt-in): the first client request " +
			"gets 503 if the reload does not finish within this period",
	},
	"balancer.preflight_nctx.window_unreachable_reject_warn": {
		RU: "preflight: запрошенное окно недостижимо — отказ с объяснением",
		EN: "preflight: the requested window is unreachable — rejecting with an explanation",
	},
	"balancer.preflight_nctx.achievable_window_reason": {
		RU: "cppworker может дать не больше %d токенов на клиента при %d слотах",
		EN: "cppworker can give at most %d tokens per client with %d slots",
	},
	"balancer.preflight_nctx.growth_skipped_warn": {
		RU: "preflight: рост окна пропущен — reload не дал бы выигрыша",
		EN: "preflight: window growth skipped — a reload would bring no gain",
	},
	"balancer.preflight_nctx.growth_skipped_action": {
		RU: "запрос обслуживается в текущем окне; cppworker ответит честным " +
			"413, если prompt действительно не влезает",
		EN: "the request is served in the current window; cppworker will answer with an honest " +
			"413 if the prompt really does not fit",
	},

	// === llamacpp_handlers_inference.go: inference-обработчики ========
	"balancer.llamacpp_handlers_inference.image_tool_inject_failed_warn": {
		RU: "handleOpenAIChatCompletions: не удалось добавить image-инструмент",
		EN: "handleOpenAIChatCompletions: failed to add the image tool",
	},
	"balancer.llamacpp_handlers_inference.model_missing_catalog_warn": {
		RU: "handleChat: модели нет в каталоге бэкенда — терминальный 404",
		EN: "handleChat: the model is not in the backend catalog — terminal 404",
	},
	"balancer.llamacpp_handlers_inference.model_missing_catalog_llamacpp_warn": {
		RU: "llama.cpp: модели нет в каталоге бэкенда — терминальный 404",
		EN: "llama.cpp: the model is not in the backend catalog — terminal 404",
	},
	"balancer.llamacpp_handlers_inference.model_missing_reason": {
		RU: "модели нет в каталоге бэкенда",
		EN: "the model is not in the backend catalog",
	},
	"balancer.llamacpp_handlers_inference.r82_migrate_chat_completions_info": {
		RU: "handleOpenAIChatCompletions: R82 переезд на другую копию модели",
		EN: "handleOpenAIChatCompletions: R82 moving to another copy of the model",
	},
	"balancer.llamacpp_handlers_inference.r82_migrate_completion_info": {
		RU: "handleOpenAICompletion: R82 переезд на другую копию модели",
		EN: "handleOpenAICompletion: R82 moving to another copy of the model",
	},
	"balancer.llamacpp_handlers_inference.r82_migrate_embeddings_info": {
		RU: "handleOpenAIEmbeddings: R82 переезд на другую копию модели",
		EN: "handleOpenAIEmbeddings: R82 moving to another copy of the model",
	},
	"balancer.llamacpp_handlers_inference.r82_migrate_chat_info": {
		RU: "handleChat: R82 переезд на другую копию модели",
		EN: "handleChat: R82 moving to another copy of the model",
	},
	"balancer.llamacpp_handlers_inference.r82_migrate_generate_info": {
		RU: "handleGenerate: R82 переезд на другую копию модели",
		EN: "handleGenerate: R82 moving to another copy of the model",
	},
	"balancer.llamacpp_handlers_inference.image_tool_loop_fallback_warn": {
		RU: "handleChat: image tool loop не справился, отдаём обычным путём",
		EN: "handleChat: the image tool loop did not cope, serving through the regular path",
	},

	// === nctx_reload_handlers.go: обработчик n_ctx auto-reload ========
	"balancer.nctx_reload_handlers.window_growth_not_requested_warn": {
		RU: "nctx_reload: рост окна НЕ запрошен клиентом — reload отменён",
		EN: "nctx_reload: the client did NOT request window growth — reload cancelled",
	},
	"balancer.nctx_reload_handlers.client_window_too_small_error": {
		RU: "Промпт не помещается в контекстное окно, которое запросил клиент: " +
			"запрошено %d токенов, а промпту нужно %d. " +
			"Увеличьте num_ctx в настройках модели клиента (до %d или больше) либо сократите промпт/историю.",
		EN: "The prompt does not fit into the context window the client requested: " +
			"%d tokens were requested but the prompt needs %d. " +
			"Increase num_ctx in the client's model settings (to %d or more) or shorten the prompt/history.",
	},
	"balancer.nctx_reload_handlers.loaded_window_too_small_error": {
		RU: "Промпт не помещается в загруженное окно модели: промпту нужно %d токенов. " +
			"Клиент не указал num_ctx, поэтому балансер не перезагружает модель сам: " +
			"укажите num_ctx в клиенте (не меньше %d) или сократите промпт/историю.",
		EN: "The prompt does not fit into the loaded model window: the prompt needs %d tokens. " +
			"The client did not specify num_ctx, so the balancer does not reload the model itself: " +
			"set num_ctx in the client (at least %d) or shorten the prompt/history.",
	},
	"balancer.nctx_reload_handlers.window_not_requested_reason": {
		RU: "клиент не запросил большее окно — автоматическая перезагрузка модели не выполняется",
		EN: "the client did not request a larger window — no automatic model reload is performed",
	},
	"balancer.nctx_reload_handlers.window_too_small_what_to_do": {
		RU: "В клиенте (Cline: «Model Context Window», OpenWebUI: num_ctx) поставьте не меньше %d " +
			"и сократите историю/промпт. Балансер не меняет окно модели без явного запроса клиента.",
		EN: "In the client (Cline: \"Model Context Window\", OpenWebUI: num_ctx) set at least %d " +
			"and shorten the history/prompt. The balancer does not change the model window without an explicit client request.",
	},
	"balancer.nctx_reload_handlers.preflight_model_missing_404_warn": {
		RU: "preflightNCtxReloadIfNeeded: модели нет на бэкенде — отдаю 404, перезагрузку не планирую",
		EN: "preflightNCtxReloadIfNeeded: the model is not on the backend — returning 404, not scheduling a reload",
	},
	"balancer.nctx_reload_handlers.requested_above_limit_no_reload_warn": {
		RU: "preflightNCtxReload: запрошенный n_ctx выше предела — reload НЕ запускаю",
		EN: "preflightNCtxReload: the requested n_ctx is above the limit — NOT starting a reload",
	},
	"balancer.nctx_reload_handlers.requested_above_limit_hint": {
		RU: "поднимите LB_NCTX_RELOAD_MAX_N_CTX или загрузите модель с нужным contextSize явно",
		EN: "raise LB_NCTX_RELOAD_MAX_N_CTX or load the model explicitly with the needed contextSize",
	},
	"balancer.nctx_reload_handlers.model_loading_skip_second_load_info": {
		RU: "preflightNCtxReload: модель загружается на бэкенде — вторую загрузку не запускаю",
		EN: "preflightNCtxReload: the model is loading on the backend — not starting a second load",
	},
	"balancer.nctx_reload_handlers.model_loading_hint": {
		RU: "ждём завершения текущей загрузки: окно выставит она сама",
		EN: "waiting for the current load to finish: it will set the window itself",
	},
	"balancer.nctx_reload_handlers.model_state_unknown_no_reload_warn": {
		RU: "preflightNCtxReload: состояние модели неизвестно — reload не запускаю, запрос идёт как есть",
		EN: "preflightNCtxReload: the model state is unknown — not starting a reload, the request goes as is",
	},
	"balancer.nctx_reload_handlers.model_state_unknown_hint": {
		RU: "опрос /api/models не удался; выгружать подготовленную модель по недостоверным данным нельзя",
		EN: "the /api/models poll failed; the prepared model must not be unloaded on unreliable data",
	},
	"balancer.nctx_reload_handlers.window_total_vs_per_seq_info": {
		RU: "preflightNCtxReload: окно модели суммарное/на клиента",
		EN: "preflightNCtxReload: model window total/per client",
	},
	"balancer.nctx_reload_handlers.client_silent_loaded_no_reload_info": {
		RU: "preflightNCtxReload: клиент не задал num_ctx, модель уже загружена — reload не нужен",
		EN: "preflightNCtxReload: the client did not set num_ctx and the model is already loaded — no reload needed",
	},
	"balancer.nctx_reload_handlers.client_silent_loaded_note": {
		RU: "настройки загруженной модели не меняем; клиент, которому не хватит контекста, получит ошибку и сработает реактивный reload",
		EN: "the loaded model settings are not changed; a client that runs out of context gets an error and the reactive reload kicks in",
	},
	"balancer.nctx_reload_handlers.client_window_total_recalculated_info": {
		RU: "preflightNCtxReload: клиентское окно — на клиента, суммарное окно пересчитано",
		EN: "preflightNCtxReload: the client window is per client, the total window is recalculated",
	},
	"balancer.nctx_reload_handlers.window_infeasible_no_reload_warn": {
		RU: "preflightNCtxReload: окно физически недостижимо — reload не планирую",
		EN: "preflightNCtxReload: the window is physically unreachable — not scheduling a reload",
	},
	"balancer.nctx_reload_handlers.model_missing_404_warn": {
		RU: "preflightNCtxReload: модели нет на бэкенде — отдаю 404",
		EN: "preflightNCtxReload: the model is not on the backend — returning 404",
	},
	"balancer.nctx_reload_handlers.profile_conservative_recommendation": {
		RU: "Add contextLengthAuto:true к profile для auto-relax",
		EN: "Add contextLengthAuto:true to the profile for auto-relax",
	},

	// === llamacpp_transport.go: транспорт llama.cpp-стрима ===============
	"balancer.llamacpp_transport.window_infeasible_413_info": {
		RU: "proxyRequestLlamaCpp: окно недостижимо — отдаю 413 клиенту",
		EN: "proxyRequestLlamaCpp: the window is unreachable — returning 413 to the client",
	},
	"balancer.llamacpp_transport.model_missing_404_info": {
		RU: "proxyRequestLlamaCpp: модели нет на бэкенде — отдаю 404 клиенту",
		EN: "proxyRequestLlamaCpp: the model is not on the backend — returning 404 to the client",
	},
	"balancer.llamacpp_transport.done_chunk_nctx_error_reload_info": {
		RU: "proxyRequestLlamaCpp: done-chunk сообщил n_ctx-ошибку — " +
			"кэш n_ctx устарел, запускаю auto-reload вместо отдачи ошибки клиенту",
		EN: "proxyRequestLlamaCpp: the done chunk reported an n_ctx error — " +
			"the n_ctx cache is stale, starting auto-reload instead of returning the error to the client",
	},
	"balancer.llamacpp_transport.tool_calls_extracted_info": {
		RU: "proxyRequestLlamaCpp: tool_calls извлечены из финального (done) чанка",
		EN: "proxyRequestLlamaCpp: tool_calls extracted from the final (done) chunk",
	},
	"balancer.llamacpp_transport.auto_continue_agent_turn_skipped_info": {
		RU: "proxyRequestLlamaCpp: R187 auto-continue пропущен — агентский ход " +
			"(в запросе есть tools[] или ответ содержит tool_calls)",
		EN: "proxyRequestLlamaCpp: R187 auto-continue skipped — agent turn " +
			"(the request has tools[] or the response contains tool_calls)",
	},
	"balancer.llamacpp_transport.auto_continue_client_capped_info": {
		RU: "proxyRequestLlamaCpp: R60.21 auto-continue пропущен — " +
			"клиент сам ограничил длину ответа",
		EN: "proxyRequestLlamaCpp: R60.21 auto-continue skipped — " +
			"the client capped the response length itself",
	},
	"balancer.llamacpp_transport.regeneration_suppression_disabled_warn": {
		RU: "proxyRequestLlamaCpp: R83 подавление перегенерации выключено политикой — " +
			"дубли ответа в клиенте ВОЗМОЖНЫ",
		EN: "proxyRequestLlamaCpp: R83 regeneration suppression is disabled by policy — " +
			"duplicate responses in the client are POSSIBLE",
	},
	"balancer.llamacpp_transport.regeneration_suppression_hint": {
		RU: "LB_AUTO_CONTINUE_CHAT_POLICY=smart (дефолт) включает подавление",
		EN: "LB_AUTO_CONTINUE_CHAT_POLICY=smart (the default) enables suppression",
	},
	"balancer.llamacpp_transport.strip_done_content_mismatch_warn": {
		RU: "stripNativeDoneContent: текст done-чанка не совпадает с " +
			"уже отданным дельтами — снимаем его как дубль, но апстрим мог изменить " +
			"семантику финального чанка",
		EN: "stripNativeDoneContent: the done-chunk text does not match what the deltas " +
			"already delivered — stripping it as a duplicate, but the upstream may have changed " +
			"the semantics of the final chunk",
	},

	// === unified_queue_r73.go: единая admission-очередь ================
	"balancer.unified_queue_r73.image_lock_no_candidates_wait_capped_warn": {
		RU: "unified queue: кандидатов нет из-за активного image-лока — лимит ожидания сужен до balancing.image.queueWaitTimeoutSec",
		EN: "unified queue: there are no candidates because of an active image lock — the wait limit is narrowed to balancing.image.queueWaitTimeoutSec",
	},
	"balancer.unified_queue_r73.queue_overloaded_fast_503_warn": {
		RU: "unified queue: очередь переполнена, быстрый 503",
		EN: "unified queue: the queue is overloaded, returning a fast 503",
	},
	"balancer.unified_queue_r73.enqueued_any_backend_info": {
		RU: "unified queue: запрос встал в admission-очередь (любой бэкенд)",
		EN: "unified queue: the request joined the admission queue (any backend)",
	},
	"balancer.unified_queue_r73.slot_acquired_after_wait_info": {
		RU: "unified queue: слот получен после ожидания",
		EN: "unified queue: slot acquired after waiting",
	},
	"balancer.unified_queue_r73.slot_wait_timeout_warn": {
		RU: "unified queue: ожидание слота истекло, 503",
		EN: "unified queue: the slot wait expired, 503",
	},
	"balancer.unified_queue_r73.client_canceled_while_waiting_info": {
		RU: "unified queue: клиент отменил запрос, пока ждал слот",
		EN: "unified queue: the client cancelled the request while waiting for a slot",
	},

	// === llamacpp_backend_helpers.go: выбор и автозагрузка ==============
	"balancer.llamacpp_backend_helpers.r82_ready_copy_after_load_failure_warn": {
		RU: "R82: переезд на готовую копию модели (загрузка не удалась)",
		EN: "R82: moving to a ready copy of the model (the load failed)",
	},
	"balancer.llamacpp_backend_helpers.r82_ready_copy_after_backend_failure_warn": {
		RU: "R82: переезд на готовую копию после отказа бэкенда",
		EN: "R82: moving to a ready copy after a backend failure",
	},
	"balancer.llamacpp_backend_helpers.r82_live_copy_after_backend_failure_warn": {
		RU: "R82: переезд на живую копию после отказа бэкенда",
		EN: "R82: moving to a live copy after a backend failure",
	},
	"balancer.llamacpp_backend_helpers.replica_selected_by_group_debug": {
		RU: "llama.cpp: реплика выбрана группой",
		EN: "llama.cpp: the replica was selected by the group",
	},
	"balancer.llamacpp_backend_helpers.model_not_loaded_node_with_file_debug": {
		RU: "llama.cpp: модель не загружена — выбран узел, где файл есть на диске",
		EN: "llama.cpp: the model is not loaded — selected a node that has the file on disk",
	},
	"balancer.llamacpp_backend_helpers.load_wait_cap_hint": {
		RU: "по умолчанию капа нет: ждём loaded/error/недоступность бэкенда",
		EN: "there is no cap by default: we wait for loaded/error/backend unreachable",
	},
	"balancer.llamacpp_backend_helpers.probe_failed_but_loading_wait_info": {
		RU: "ensureModelLoadedOnBackend: проба не ответила, но загрузка идёт — ждём, а не считаем узел мёртвым",
		EN: "ensureModelLoadedOnBackend: the probe did not answer, but a load is in progress — we wait instead of treating the node as dead",
	},
	"balancer.llamacpp_backend_helpers.load_failed_event_message": {
		RU: "Не удалось загрузить модель %s: %s",
		EN: "Failed to load model %s: %s",
	},

	// === nctx_reload.go: координатор перезагрузки ======================
	"balancer.nctx_reload.auto_reload_ceiling_below_loaded_error": {
		RU: "auto_reload_max_n_ctx НИЖЕ загруженного окна модели — " +
			"запросы на границе будут отклоняться 413",
		EN: "auto_reload_max_n_ctx is BELOW the loaded model window — " +
			"requests at the boundary will be rejected with 413",
	},
	"balancer.nctx_reload.auto_reload_ceiling_below_loaded_hint": {
		RU: "поднимите LB_NCTX_RELOAD_MAX_N_CTX до окна модели (или 0 = без потолка)",
		EN: "raise LB_NCTX_RELOAD_MAX_N_CTX to the model window (or 0 = no ceiling)",
	},
	"balancer.nctx_reload.no_http_client_no_timeout_warn": {
		RU: "nctx reload: HTTP-клиент не задан — используем без таймаута",
		EN: "nctx reload: no HTTP client is set — using one without a timeout",
	},
	"balancer.nctx_reload.strategy_lowers_gpu_offload_warn": {
		RU: "nctx_reload: адаптивная стратегия понижает GPU-оффлоад — оставляю текущий",
		EN: "nctx_reload: the adaptive strategy lowers the GPU offload — keeping the current one",
	},
	"balancer.nctx_reload.strategy_lowers_gpu_offload_reason": {
		RU: "бюджет стратегии считает свободную VRAM без учёта памяти самой модели",
		EN: "the strategy budget counts free VRAM without accounting for the model's own memory",
	},
	"balancer.nctx_reload.reload_skipped_window_unchanged_warn": {
		RU: "[nctx_reload] reload пропущен — окно не изменилось бы",
		EN: "[nctx_reload] reload skipped — the window would not change",
	},
	"balancer.nctx_reload.reload_skipped_reason": {
		RU: "перезагрузка стоила бы 1–3 минуты простоя без выигрыша в окне",
		EN: "the reload would cost 1–3 minutes of downtime with no window gain",
	},
	"balancer.nctx_reload.window_grew_less_than_requested_warn": {
		RU: "[nctx_reload] окно выросло МЕНЬШЕ запрошенного — клиент получит меньше контекста",
		EN: "[nctx_reload] the window grew LESS than requested — the client gets less context",
	},
	"balancer.nctx_reload.window_grew_less_reason": {
		RU: "адаптивная стратегия cppworker ограничила размер окна (VRAM/RAM)",
		EN: "the cppworker adaptive strategy limited the window size (VRAM/RAM)",
	},
	"balancer.nctx_reload.window_grew_less_hint": {
		RU: "уменьшите окно, поставьте parallel=1 или возьмите модель/квант полегче",
		EN: "reduce the window, set parallel=1 or pick a lighter model/quant",
	},

	// === num_ctx_resolver.go: резолвер num_ctx =========================
	"balancer.num_ctx_resolver.profile_saved_normalized_key_info": {
		RU: "R136: профиль сохранён под нормализованным ключом",
		EN: "R136: the profile is saved under the normalized key",
	},
	"balancer.num_ctx_resolver.profile_saved_normalized_key_hint": {
		RU: "карточка диска и карточка загруженной модели пишут одну запись",
		EN: "the disk card and the loaded-model card write the same entry",
	},
	"balancer.num_ctx_resolver.backend_addr_unknown_warn": {
		RU: "resolveLoadedWindowNCtx: адрес бэкенда неизвестен — состояние модели не определить",
		EN: "resolveLoadedWindowNCtx: the backend address is unknown — the model state cannot be determined",
	},
	"balancer.num_ctx_resolver.poll_failed_state_unknown_warn": {
		RU: "resolveLoadedWindowNCtx: опрос /api/models не удался — состояние модели неизвестно",
		EN: "resolveLoadedWindowNCtx: the /api/models poll failed — the model state is unknown",
	},
	"balancer.num_ctx_resolver.poll_failed_state_unknown_hint": {
		RU: "по недостоверным данным reload не запускаем",
		EN: "no reload is started on unreliable data",
	},
	"balancer.num_ctx_resolver.clamp_to_per_seq_window_info": {
		RU: "ResolveNumCtx: R83 — ограничиваем запрос окном слота",
		EN: "ResolveNumCtx: R83 — clamping the request to the slot window",
	},
	"balancer.num_ctx_resolver.clamp_to_loaded_window_info": {
		RU: "ResolveNumCtx: R83 — не просим больше загруженного окна",
		EN: "ResolveNumCtx: R83 — not asking for more than the loaded window",
	},

	// === placement_replication_r74.go: replicated/pool =================
	"balancer.placement_replication_r74.manager_start_failed_warn": {
		RU: "placement: не удалось поднять менеджер репликации для strategy=replicated",
		EN: "placement: failed to start the replication manager for strategy=replicated",
	},
	"balancer.placement_replication_r74.manager_started_on_demand_info": {
		RU: "placement: менеджер репликации поднят по требованию политики",
		EN: "placement: the replication manager was started on policy demand",
	},
	"balancer.placement_replication_r74.action_skipped_mask": {
		RU: "пропущено (маска, нужен точный model): %s",
		EN: "skipped (mask, an exact model is required): %s",
	},
	"balancer.placement_replication_r74.action_manager_unavailable": {
		RU: "менеджер репликации недоступен: %s",
		EN: "the replication manager is unavailable: %s",
	},
	"balancer.placement_replication_r74.action_group_create_failed": {
		RU: "ошибка создания группы %s: %v",
		EN: "failed to create group %s: %v",
	},
	"balancer.placement_replication_r74.group_created_by_policy_info": {
		RU: "placement: группа репликации создана по политике",
		EN: "placement: a replication group was created by policy",
	},
	"balancer.placement_replication_r74.action_group_created": {
		RU: "группа создана: %s (min=%d max=%d targets=%d)",
		EN: "group created: %s (min=%d max=%d targets=%d)",
	},
	"balancer.placement_replication_r74.layout_resynced_info": {
		RU: "placement: раскладка пересобрана после изменения состава бэкендов",
		EN: "placement: the layout was rebuilt after the backend set changed",
	},

	// === proxy.go: основной путь запроса ==============================
	"balancer.proxy.virtual_alias_pool_not_allowed_warn": {
		RU: "virtual_router: модель — виртуальный алиас, но pool не разрешён " +
			"(operatingMode != virtual_router и placement-стратегия != pool); идём стандартным путём",
		EN: "virtual_router: the model is a virtual alias, but pool is not allowed " +
			"(operatingMode != virtual_router and the placement strategy != pool); taking the standard path",
	},
	"balancer.proxy.placement_decision_debug": {
		RU: "placement: решение о размещении",
		EN: "placement: placement decision",
	},
	"balancer.proxy.session_restick_copy_not_covering_window_info": {
		RU: "session re-stick: привязанная копия не покрывает окно клиента",
		EN: "session re-stick: the pinned copy does not cover the client window",
	},
	"balancer.proxy.unified_slot_acquired_info": {
		RU: "unified queue: слот получен, продолжаем без legacy-очереди",
		EN: "unified queue: slot acquired, continuing without the legacy queue",
	},
	"balancer.proxy.unified_request_canceled_info": {
		RU: "unified queue: запрос отменён до получения слота",
		EN: "unified queue: the request was cancelled before a slot was acquired",
	},
	"balancer.proxy.client_canceled_not_unhealthy_info": {
		RU: "backend request canceled by client — бэкенд НЕ помечаем нездоровым",
		EN: "backend request canceled by client — the backend is NOT marked unhealthy",
	},
}

func init() { Register(messagesBalancer2) }
