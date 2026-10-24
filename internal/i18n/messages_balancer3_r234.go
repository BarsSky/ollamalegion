package i18n

// messages_balancer3 — R234 (2026-10-24): сообщения балансера, батч r234 #3.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ФАЙЛ. Каталог пополняется батчами по областям (см. register.go):
// параллельные правки одного messages.go конфликтуют. Здесь живут ключи файлов
// internal/balancer/{rpc_layout_failure_r118,llamacpp_metrics_poller,model_management,
// image_file_route,llamacpp_transport_nonstream,slot_manager,stale_model_snapshot_r160,
// auto_continue,auto_pull,backend_registry,cancel_client_r229,cancel_upstream_r184,
// model_copy_window_r118,model_missing_r119,placement_reject_r76,primary_model_policy,
// autotune,cuda_arch_refusal_r185,image_resources,load_fit_r188,nctx_infeasible_r118,
// nctx_reload_adaptive,placement_fit_r75,preflight_helper,profiles_persistence,
// proxy_request,sharded_master_r92,state}.go.
//
// ПРАВИЛА. Ключ — "<компонент>.<файл>.<смысл>"; RU — исходный текст байт-в-байт
// (тесты и операторские инструкции ссылаются на него), EN — тот же смысл с тем же
// набором fmt-глаголов (TestCatalogVerbParity). %w в каталог НЕ попадает: текст
// ошибки-обёртки остаётся в коде — fmt.Errorf("%s: %w", i18n.T("key"), err).
//
// ЧЕГО ЗДЕСЬ НЕТ И ПОЧЕМУ: тексты, которые уходят МОДЕЛИ (промпт автопродолжения,
// auto_continue.go), маркеры-эвристики, по которым распознаётся чужой/модельный
// текст (detectIncompleteResponse, autoLoadErrorSaysModelMissing), и поля-данные
// анализа (reason/suggestion/issues у AutoTune и placement-fit) — это не логи и не
// ошибки, а содержимое промпта/данные отчёта; их язык задаётся не каталогом.
var messagesBalancer3 = map[string]map[Lang]string{
	// === rpc_layout_failure_r118.go: отказ раскладки при загрузке =======
	"balancer.rpc_layout_failure.disable_failed_warn": {
		RU: "DisableRPCLayoutFor: не удалось выключить раскладку",
		EN: "DisableRPCLayoutFor: failed to disable the layout",
	},
	"balancer.rpc_layout_failure.disabled_after_failed_load_warn": {
		RU: "DisableRPCLayoutFor: раскладка выключена после неудачной загрузки",
		EN: "DisableRPCLayoutFor: the layout was disabled after a failed load",
	},
	"balancer.rpc_layout_failure.degraded_warn": {
		RU: "executeLlamaCppLoad: раскладка деградировала — грузим без мёртвых узлов",
		EN: "executeLlamaCppLoad: the layout degraded — loading without the dead nodes",
	},
	"balancer.rpc_layout_failure.unresponsive_marker": {
		RU: "cppworker не отвечает после загрузки с раскладкой (вероятно, рестарт из-за узла)",
		EN: "cppworker is not responding after a load with the layout (probably a restart caused by the node)",
	},
	"balancer.rpc_layout_failure.disabled_reason": {
		RU: "загрузка с раскладкой не удалась (%s: %s) — раскладка выключена, грузим локально",
		EN: "the load with the layout failed (%s: %s) — the layout is disabled, loading locally",
	},
	"balancer.rpc_layout_failure.retry_locally_warn": {
		RU: "executeLlamaCppLoad: загрузка упала с включённой раскладкой — повторяю локально",
		EN: "executeLlamaCppLoad: the load failed while the layout was active — retrying locally",
	},
	"balancer.rpc_layout_failure.retry_not_done_error": {
		RU: "%s; раскладка выключена (%s), но бэкенд %s не ответил за %s — повтор локально не сделан",
		EN: "%s; the layout is disabled (%s), but backend %s did not respond within %s — the local retry was not performed",
	},
	"balancer.rpc_layout_failure.disabled_note": {
		RU: "раскладка отключена: %s",
		EN: "layout disabled: %s",
	},
	"balancer.rpc_layout_failure.retry_ok_info": {
		RU: "executeLlamaCppLoad: локальный повтор после отказа раскладки удался",
		EN: "executeLlamaCppLoad: the local retry after the layout failure succeeded",
	},
	"balancer.rpc_layout_failure.retry_failed_error": {
		RU: "загрузка с раскладкой не удалась (%s); локальный повтор тоже не удался: %s",
		EN: "the load with the layout failed (%s); the local retry failed as well: %s",
	},

	// === llamacpp_metrics_poller.go: кэш метрик llama.cpp ================
	"balancer.llamacpp_metrics_poller.rpc_server_from_worker_debug": {
		RU: "llamaCppMetricsPoller: rpc_server из воркера",
		EN: "llamaCppMetricsPoller: rpc_server as reported by the worker",
	},
	"balancer.llamacpp_metrics_poller.slots_missing_capacity_reset_info": {
		RU: "llamaCppMetricsPoller: слотов у загруженной модели нет — вместимость сброшена",
		EN: "llamaCppMetricsPoller: the loaded model reports no slots — capacity reset",
	},
	"balancer.llamacpp_metrics_poller.slots_unrecognized_debug": {
		RU: "llamaCppMetricsPoller: слоты модели не распознаны, вместимость прежняя",
		EN: "llamaCppMetricsPoller: the model slots were not recognised, capacity left unchanged",
	},
	"balancer.llamacpp_metrics_poller.capacity_adopted_info": {
		RU: "llamaCppMetricsPoller: вместимость бэкенда приведена к числу слотов модели",
		EN: "llamaCppMetricsPoller: backend capacity set to the model slot count",
	},
	"balancer.llamacpp_metrics_poller.capacity_from_slots_note": {
		RU: "значение живёт в RuntimeModelSlots: MaxConcurrentReqs переписывается перерегистрацией агента",
		EN: "the value lives in RuntimeModelSlots: MaxConcurrentReqs is overwritten when the agent re-registers",
	},

	// === model_management.go: операции с моделью ========================
	"balancer.model_management.opt_in_what_model_ops": {
		RU: "операции с моделью (pull/push/load/unload)",
		EN: "model operations (pull/push/load/unload)",
	},
	"balancer.model_management.opt_in_what_load_request": {
		RU: "HTTP-запрос загрузки модели",
		EN: "model load HTTP request",
	},
	"balancer.model_management.profile_applied_ignore_defaults_info": {
		RU: "applyProfileLoadParams: применён профиль модели, «настройки по умолчанию» для неё отключены (ignoreDefaults=true)",
		EN: "applyProfileLoadParams: the model profile was applied, the default settings are disabled for it (ignoreDefaults=true)",
	},
	"balancer.model_management.auto_load_not_appeared_error": {
		RU: "auto-load failed: model %q did not appear in cppworker model list or load progress " +
			"for %d consecutive polls — load did not start (проверь наличие GGUF-файла и логи cppworker)",
		EN: "auto-load failed: model %q did not appear in the cppworker model list or load progress " +
			"for %d consecutive polls — the load did not start (check that the GGUF file exists and see the cppworker logs)",
	},
	"balancer.model_management.default_profile_skipped_info": {
		RU: "executeLlamaCppLoad: «настройки по умолчанию» не применены (ignoreDefaults=true в профиле модели)",
		EN: "executeLlamaCppLoad: the default settings were not applied (ignoreDefaults=true in the model profile)",
	},
	"balancer.model_management.cuda_arch_refused_error": {
		RU: "load refused: несовместимые CUDA-архитектуры",
		EN: "load refused: incompatible CUDA architectures",
	},
	"balancer.model_management.load_fit_refused_error": {
		RU: "load refused: модель не влезает в свободную память",
		EN: "load refused: the model does not fit into the free memory",
	},
	"balancer.model_management.layout_wait_extended_info": {
		RU: "executeLlamaCppLoad: раскладка активна — окно ожидания расширено",
		EN: "executeLlamaCppLoad: the layout is active — the wait window was extended",
	},

	// === image_file_route.go: отдача файлов картинок ====================
	"balancer.image_file_route.method_not_allowed": {
		RU: "используйте GET для получения изображения",
		EN: "use GET to fetch the image",
	},
	"balancer.image_file_route.backend_unreachable_debug": {
		RU: "image file: бэкенд не ответил",
		EN: "image file: the backend did not respond",
	},
	"balancer.image_file_route.client_gone_warn": {
		RU: "image file: клиент отвалился во время отдачи",
		EN: "image file: the client disconnected while the file was being served",
	},
	"balancer.image_file_route.not_found_any_backend_info": {
		RU: "image file: файл не найден ни на одном бэкенде",
		EN: "image file: the file was not found on any backend",
	},
	"balancer.image_file_route.not_found_message": {
		RU: "image %q not found on any image backend (файлы живут в каталоге воркера и могут быть очищены)",
		EN: "image %q not found on any image backend (files live in the worker directory and may be cleaned up)",
	},

	// === llamacpp_transport_nonstream.go: нестриминговый транспорт ======
	"balancer.llamacpp_transport_nonstream.window_unreachable_413_info": {
		RU: "proxyRequestLlamaCppNonStream: окно недостижимо — отдаю 413 клиенту",
		EN: "proxyRequestLlamaCppNonStream: the window is unreachable — returning 413 to the client",
	},
	"balancer.llamacpp_transport_nonstream.model_missing_404_warn": {
		RU: "proxyRequestLlamaCppNonStream: модели нет на бэкенде — отдаю 404 клиенту",
		EN: "proxyRequestLlamaCppNonStream: the model is not on the backend — returning 404 to the client",
	},
	"balancer.llamacpp_transport_nonstream.model_ready_after_reload_info": {
		RU: "proxyRequestLlamaCppNonStream: модель готова после ожидания reload — запрос продолжается",
		EN: "proxyRequestLlamaCppNonStream: the model is ready after waiting for the reload — the request continues",
	},

	// === slot_manager.go: soft-диагностика допуска запроса ==============
	"balancer.slot_manager.vram_above_headroom_soft_info": {
		RU: "backend VRAM выше резерва headroom (soft, не блокирует): " +
			"занятая VRAM загруженной модели — норма, лишние запросы ждут в admission-очереди",
		EN: "backend VRAM is above the headroom reservation (soft, non-blocking): " +
			"VRAM held by a loaded model is normal, extra requests wait in the admission queue",
	},
	"balancer.slot_manager.gpu_busy_soft_info": {
		RU: "backend GPU занят (soft, не блокирует): бэкенд продолжает " +
			"принимать запросы, параллелизм ограничен слотами",
		EN: "backend GPU is busy (soft, non-blocking): the backend keeps " +
			"accepting requests, parallelism is limited by the slots",
	},
	"balancer.slot_manager.cpu_busy_soft_info": {
		RU: "backend CPU занят (soft, не блокирует): генерация на CPU — " +
			"норма для частичного offload, запрос встанет в очередь",
		EN: "backend CPU is busy (soft, non-blocking): CPU generation is " +
			"normal for a partial offload, the request will be queued",
	},

	// === stale_model_snapshot_r160.go: устаревший снимок метрик =========
	"balancer.stale_model_snapshot.missing_everywhere_404_warn": {
		RU: "handleStaleModelSnapshot: модели нет ни на одной машине — отдаю 404 вместо 503",
		EN: "handleStaleModelSnapshot: the model is on no machine at all — returning 404 instead of 503",
	},
	"balancer.stale_model_snapshot.missing_everywhere_reason": {
		RU: "модели нет ни на одной машине: файла нет в каталоге ни одного узла",
		EN: "the model is on no machine at all: the file is not in any node's catalog",
	},
	"balancer.stale_model_snapshot.load_not_started_debug": {
		RU: "handleStaleModelSnapshot: загрузка после устаревшего снимка не стартовала",
		EN: "handleStaleModelSnapshot: the load after the stale snapshot did not start",
	},
	"balancer.stale_model_snapshot.worker_unknown_model_warn": {
		RU: "handleStaleModelSnapshot: воркер не знает модель — снимок обновлён, вместо 502 отдаём 503",
		EN: "handleStaleModelSnapshot: the worker does not know the model — the snapshot was refreshed, returning 503 instead of 502",
	},

	// === auto_continue.go: таймер автопродолжения =======================
	"balancer.auto_continue.timeout_unparsed_warn": {
		RU: "LB_AUTO_CONTINUE_TIMEOUT_SEC не распознан — автопродолжение без капа",
		EN: "LB_AUTO_CONTINUE_TIMEOUT_SEC was not recognised — auto-continue without a cap",
	},
	"balancer.auto_continue.timeout_armed_warn": {
		RU: "armed auto-continue timeout (operator opt-in): продолжение будет " +
			"оборвано по таймеру, а не по завершению генерации",
		EN: "armed auto-continue timeout (operator opt-in): the continuation will be " +
			"cut off by the timer rather than by the end of generation",
	},

	// === auto_pull.go: ожидание закачки модели ==========================
	"balancer.auto_pull.pull_timeout_armed_warn": {
		RU: "auto-pull: PullTimeout включён — ожидание закачки будет прервано по таймеру " +
			"(это лимит на работу, а не таймаут опроса состояния; pull продолжится в фоне)",
		EN: "auto-pull: PullTimeout is enabled — the wait for the download will be interrupted by the timer " +
			"(this is a work limit, not a state-poll timeout; the pull continues in the background)",
	},
	"balancer.auto_pull.pull_timeout_unparsed_warn": {
		RU: "auto-pull: PullTimeout не распознан, ожидание без лимита",
		EN: "auto-pull: PullTimeout was not recognised, waiting without a limit",
	},

	// === backend_registry.go: приём данных от узла/агента ===============
	"balancer.backend_registry.adopt_capacity_info": {
		RU: "AdoptCapacityFromNode: зафиксирована заявка узла о вместимости",
		EN: "AdoptCapacityFromNode: the node capacity claim was recorded",
	},
	"balancer.backend_registry.adopt_token_info": {
		RU: "AdoptCppWorkerToken: сохранён токен cppworker, сообщённый агентом",
		EN: "AdoptCppWorkerToken: the cppworker token reported by the agent was saved",
	},

	// === cancel_client_r229.go: отмена от клиента =======================
	"balancer.cancel_client.broadcast_info": {
		RU: "client cancel: отмена от клиента разослана воркерам",
		EN: "client cancel: the client cancellation was broadcast to the workers",
	},
	"balancer.cancel_client.not_accepted_warn": {
		RU: "client cancel: воркер не принял отмену",
		EN: "client cancel: the worker did not accept the cancellation",
	},

	// === cancel_upstream_r184.go: отмена генерации на воркере ===========
	"balancer.cancel_upstream.not_accepted_warn": {
		RU: "cancel upstream: воркер не принял отмену",
		EN: "cancel upstream: the worker did not accept the cancellation",
	},
	"balancer.cancel_upstream.cancelled_info": {
		RU: "cancel upstream: генерация отменена на воркере",
		EN: "cancel upstream: the generation was cancelled on the worker",
	},

	// === model_copy_window_r118.go: выбор копии модели по окну ==========
	"balancer.model_copy_window.select_backend_info": {
		RU: "selectBackend: копия модели выбрана по окну клиента",
		EN: "selectBackend: the model copy was chosen by the client window",
	},
	"balancer.model_copy_window.llamacpp_info": {
		RU: "llama.cpp: копия модели выбрана по окну клиента",
		EN: "llama.cpp: the model copy was chosen by the client window",
	},

	// === model_missing_r119.go: модели нет на бэкенде ===================
	"balancer.model_missing.recorded_warn": {
		RU: "model missing: запомнил, что модели нет на бэкенде — перезагрузку планировать не будем",
		EN: "model missing: recorded that the model is not on the backend — no reload will be planned",
	},
	"balancer.model_missing.auto_load_404_warn": {
		RU: "auto-load: модели нет на бэкенде — отдаю 404 вместо 503 «повтори»",
		EN: "auto-load: the model is not on the backend — returning 404 instead of a 503 retry",
	},

	// === placement_reject_r76.go: отказ политики размещения =============
	"balancer.placement_reject.strategy_not_executable": {
		RU: "стратегия размещения %s не исполняется этой сборкой " +
			"(исполняются single/pool/replicated/sharded/auto; rpc — legacy-путь координатора-" +
			"заглушки, переведённый §9 плана в sharded), а placement.fallback=error: запрос не " +
			"будет обслужен другой стратегией молча. Причина: %s",
		EN: "placement strategy %s is not implemented by this build " +
			"(single/pool/replicated/sharded/auto are; rpc is the legacy stub-coordinator " +
			"path, moved to sharded by §9 of the plan), and placement.fallback=error: the request " +
			"will not be silently served by another strategy. Reason: %s",
	},
	"balancer.placement_reject.auto_could_not_choose": {
		RU: "auto не смогла выбрать стратегию размещения (placement.fallback=error): %s",
		EN: "auto could not choose a placement strategy (placement.fallback=error): %s",
	},
	"balancer.placement_reject.strategy_not_executable_now": {
		RU: "стратегия размещения %s не может быть исполнена сейчас " +
			"(placement.fallback=error): %s",
		EN: "placement strategy %s cannot be executed right now " +
			"(placement.fallback=error): %s",
	},
	"balancer.placement_reject.rejected_by_policy_warn": {
		RU: "placement: запрос отклонён политикой (fallback=error)",
		EN: "placement: the request was rejected by the policy (fallback=error)",
	},
	"balancer.placement_reject.hint": {
		RU: "исправьте balancing.placement для этой модели, включите fallback=single " +
			"или auto.allowDegraded=true — см. GET /api/v1/placement",
		EN: "fix balancing.placement for this model, enable fallback=single " +
			"or auto.allowDegraded=true — see GET /api/v1/placement",
	},

	// === primary_model_policy.go: зафиксированные настройки модели ======
	"balancer.primary_model_policy.served_info": {
		RU: "primary: запрос обслужен с зафиксированным n_ctx (настройки модели не меняются)",
		EN: "primary: the request was served with the fixed n_ctx (the model settings are not changed)",
	},
	"balancer.primary_model_policy.insufficient_warn": {
		RU: "primary: запрос требует контекста больше зафиксированного — отказ",
		EN: "primary: the request needs more context than the fixed one — rejecting",
	},
	"balancer.primary_model_policy.settings_locked_message": {
		RU: "model %q имеет ЗАФИКСИРОВАННЫЕ настройки: n_ctx=%d, и его нельзя изменить " +
			"клиентским запросом. Этому запросу нужно %d токенов " +
			"(prompt≈%d + generation=%d + reserve). Уменьшите num_predict/размер промпта " +
			"или попросите администратора увеличить контекст модели в WebUI " +
			"(GGUF → настройки бэкенда) и перезагрузить её.",
		EN: "model %q has LOCKED settings: n_ctx=%d, and they cannot be changed " +
			"by a client request. This request needs %d tokens " +
			"(prompt≈%d + generation=%d + reserve). Reduce num_predict or the prompt size " +
			"or ask the administrator to increase the model context in the WebUI " +
			"(GGUF → backend settings) and reload it.",
	},
	"balancer.primary_model_policy.admin_action": {
		RU: "измените настройки модели в WebUI (GGUF → настройки) и перезагрузите её",
		EN: "change the model settings in the WebUI (GGUF → settings) and reload it",
	},

	// === autotune.go: применение плана AutoTune =========================
	"balancer.autotune.layers_auto_no_reload_info": {
		RU: "autotune: рекомендация layers=-2 (auto) не превращается в reload",
		EN: "autotune: the layers=-2 (auto) recommendation does not turn into a reload",
	},
	"balancer.autotune.layers_auto_no_reload_reason": {
		RU: "факт после auto-загрузки не равен -2: план повторялся бы бесконечно",
		EN: "the actual value after an auto load is never -2: the plan would repeat forever",
	},

	// === cuda_arch_refusal_r185.go: карта не поддерживается образом =====
	"balancer.cuda_arch_refusal.arch_mismatch_detail": {
		RU: "CUDA-архитектуры образа и карты не совпадают",
		EN: "the CUDA architectures of the image and the GPU do not match",
	},
	"balancer.cuda_arch_refusal.built_unknown": {
		RU: "неизвестно",
		EN: "unknown",
	},
	"balancer.cuda_arch_refusal.refusal_reason": {
		RU: "бэкенд %s не может считать на этой карте: %s (образ собран под sm_%s). " +
			"Загрузка НЕ запущена, повторы бессмысленны: пересоберите образ с CUDA_ARCH, " +
			"включающим архитектуру карты (например 75;86;120), и перезапустите воркер [%s]",
		EN: "backend %s cannot compute on this GPU: %s (the image was built for sm_%s). " +
			"The load is NOT started, retries are pointless: rebuild the image with CUDA_ARCH " +
			"covering the GPU architecture (for example 75;86;120) and restart the worker [%s]",
	},
	"balancer.cuda_arch_refusal.auto_load_400_warn": {
		RU: "auto-load: карта не поддерживается образом — отдаю 400 вместо 503 «повтори»",
		EN: "auto-load: the GPU is not supported by the image — returning 400 instead of a 503 retry",
	},

	// === image_resources.go: предохранитель ресурсного лока =============
	"balancer.image_resources.lock_fuse_hint": {
		RU: "лок будет снят принудительно по будильнику; " +
			"уберите exclusiveLockTimeoutSec, чтобы предохранитель был снят " +
			"(лок освобождается по жизненному циклу запроса)",
		EN: "the lock will be released forcibly by the timer; " +
			"remove exclusiveLockTimeoutSec to disarm the fuse " +
			"(the lock is released by the request lifecycle)",
	},
	"balancer.image_resources.lock_forced_release_warn": {
		RU: "image GPU lock held too long — forced release (предохранитель exclusiveLockTimeoutSec)",
		EN: "image GPU lock held too long — forced release (exclusiveLockTimeoutSec fuse)",
	},

	// === load_fit_r188.go: отказ «не влезает в память» ==================
	"balancer.load_fit.fits_debug": {
		RU: "load fit check: модель влезает",
		EN: "load fit check: the model fits",
	},
	"balancer.load_fit.not_fit_reason": {
		RU: "модель %s (%d МБ) не влезает в свободную память бэкенда %s: свободно %d МБ " +
			"(RAM %d МБ, VRAM %d МБ, раскладка %d МБ при %d узлах). Загрузка НЕ запущена: " +
			"выгрузите другую модель на этом бэкенде, уменьшите контекст/квант или добавьте узел раскладки [%s]",
		EN: "model %s (%d MB) does not fit into the free memory of backend %s: %d MB free " +
			"(RAM %d MB, VRAM %d MB, layout %d MB across %d nodes). The load is NOT started: " +
			"unload another model on this backend, reduce the context/quant, or add a layout node [%s]",
	},

	// === nctx_infeasible_r118.go: потолок окна от cppworker =============
	"balancer.nctx_infeasible.ceiling_remembered_warn": {
		RU: "nctxInfeasible: перезагрузка окна отвергнута cppworker'ом — запомнил потолок",
		EN: "nctxInfeasible: the window reload was rejected by the cppworker — the ceiling was recorded",
	},

	// === nctx_reload_adaptive.go: адаптивная стратегия ==================
	"balancer.nctx_reload_adaptive.non_200_strategy_warn": {
		RU: "queryAdaptiveStrategy: non-200 status (стратегия не получена, " +
			"загрузка пойдёт по значениям по умолчанию)",
		EN: "queryAdaptiveStrategy: non-200 status (the strategy was not obtained, " +
			"the load will use the default values)",
	},

	// === placement_fit_r75.go: выбор стратегии по VRAM ==================
	"balancer.placement_fit.auto_degraded_warn": {
		RU: "placement auto: не удалось выбрать стратегию по VRAM, деградация на single",
		EN: "placement auto: failed to choose a strategy by VRAM, degrading to single",
	},

	// === preflight_helper.go: ожидание готовности модели ================
	"balancer.preflight_helper.model_ready_after_reload_info": {
		RU: "preflight: модель готова после ожидания reload — запрос обслуживается",
		EN: "preflight: the model is ready after waiting for the reload — the request is served",
	},

	// === profiles_persistence.go: загрузка профилей с диска =============
	"balancer.profiles_persistence.key_normalized_info": {
		RU: "R136: ключ профиля нормализован при загрузке",
		EN: "R136: the profile key was normalised on load",
	},
	"balancer.profiles_persistence.key_normalized_hint": {
		RU: "карточка диска и карточка загруженной модели теперь пишут одну запись",
		EN: "the disk card and the loaded model card now write a single record",
	},

	// === proxy_request.go: маршрутизация запроса ========================
	"balancer.proxy_request.readonly_direct_info": {
		RU: "proxyRequest: read-only/mgmt путь — прямой прокси без n_ctx preflight и слотов",
		EN: "proxyRequest: read-only/mgmt path — direct proxy without n_ctx preflight and slots",
	},

	// === sharded_master_r92.go: маршрут на мастер группы ================
	"balancer.sharded_master.routed_to_master_info": {
		RU: "sharded: запрос направлен на мастер логического бэкенда",
		EN: "sharded: the request was routed to the logical backend master",
	},

	// === state.go: согласование вместимости узла ========================
	"balancer.state.runtime_capacity_clamped_info": {
		RU: "R71: runtime-вместимость выше n_parallel узла — приведена к вместимости ноды",
		EN: "R71: the runtime capacity is above the node n_parallel — clamped to the node capacity",
	},
}

func init() { Register(messagesBalancer3) }
