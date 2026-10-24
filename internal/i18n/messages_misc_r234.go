package i18n

// messages_misc_r234.go — R234, батч «misc»: логи и сообщения об ошибках,
// оставшиеся вне батчей api/balancer/core/cmd.
//
// ЗАЧЕМ ОТДЕЛЬНЫМ ФАЙЛОМ. Каталог пополняется батчами по областям кода (см.
// register.go). Этот файл собирает «смешанный» батч из 26 файлов пяти пакетов:
//
//	cmd/cppworker  — гейт n_ctx, memfit, авто-оффлоад, переменные окружения,
//	                 служебные ручки /api/v1/cppworker/*;
//	cmd/balancer   — источник токенов аутентификации (env_auth.go);
//	internal/agent — сбор метрик (GPU/NVML/сеть) и причины «GPU не найден»;
//	internal/api   — вытеснение модели по памяти, вход в WebUI, AutoTune-admin,
//	                 каталог моделей бэкендов.
//
// ПРАВИЛА ТЕ ЖЕ, что в messages.go: RU — исходная формулировка без правки
// смысла, EN — перевод с сохранением технических терминов (backend, worker,
// model, GPU, NVML, RPC, n_ctx, KV-cache, VRAM, RAM, heartbeat, layout node),
// набор fmt-глаголов в RU и EN обязан совпадать (TestCatalogVerbParity).
//
// ОСОБЕННОСТЬ %w. Ни в одном тексте здесь нет %w: причину дописывает вызывающий
// через fmt.Errorf("%s: %w", i18n.T("ключ"), err), иначе errors.Is/errors.As
// перестают разворачивать цепочку — fmt.Sprintf (на котором стоит i18n.T)
// глагол %w не поддерживает.
func init() { Register(messagesMisc) }

var messagesMisc = map[string]map[Lang]string{
	// === cmd/cppworker/handlers_config.go: применение настроек ==========
	"cppworker.handlers_config.rpc_mode_changed": {
		RU: "RPC-режим изменён через API",
		EN: "RPC mode changed via the API",
	},
	"cppworker.handlers_config.idle_unload_applied": {
		RU: "idle unload: применено из WebUI",
		EN: "idle unload: applied from the WebUI",
	},
	"cppworker.handlers_config.rpc_identity_changed": {
		RU: "rpc-server: идентичность узла изменилась — процесс пересоздаётся " +
			"(активная сессия раскладки, если она есть, будет прервана)",
		EN: "rpc-server: the node identity changed — the process is being recreated " +
			"(an active layout session, if any, will be interrupted)",
	},
	"cppworker.handlers_config.rpc_start_failed": {
		RU: "rpc-server: узел раскладки не поднялся из WebUI",
		EN: "rpc-server: the layout node did not start from the WebUI",
	},
	"cppworker.handlers_config.rpc_recreated": {
		RU: "rpc-server: узел раскладки пересоздан по настройкам",
		EN: "rpc-server: the layout node was recreated from the settings",
	},
	"cppworker.handlers_config.rpc_confirmed_noop": {
		RU: "rpc-server: приказ подтверждён без изменений — процесс не тронут",
		EN: "rpc-server: the command is acknowledged with no changes — the process was left untouched",
	},

	// === cmd/cppworker/nctx_feasibility.go: гейт n_ctx ==================
	"cppworker.nctx_feasibility.log_infeasible": {
		RU: "R83 n_ctx превышает физический предел: загрузка обречена " +
			"(выше RAM-границы или обучающего контекста модели)",
		EN: "R83 n_ctx exceeds the physical ceiling: the load is doomed " +
			"(above the RAM ceiling or the model training context)",
	},
	"cppworker.nctx_feasibility.log_degraded": {
		RU: "R83 n_ctx выше VRAM-границы: загрузка уйдёт в partial_offload/cpu_only " +
			"(часть слоёв в RAM, ответ будет заметно медленнее; для A10 24 GB " +
			"используйте n_ctx в пределах max_vram_n_ctx и kvCacheType=q4_0)",
		EN: "R83 n_ctx is above the VRAM ceiling: the load will go to partial_offload/cpu_only " +
			"(some layers in RAM, responses will be noticeably slower; for an A10 24 GB " +
			"keep n_ctx within max_vram_n_ctx and use kvCacheType=q4_0)",
	},
	"cppworker.nctx_feasibility.log_exact_fit": {
		RU: "R83 n_ctx укладывается в VRAM (exact_fit)",
		EN: "R83 n_ctx fits in VRAM (exact_fit)",
	},
	"cppworker.nctx_feasibility.log_unknown": {
		RU: "R83 n_ctx-оценка недоступна: нет метрик VRAM/RAM/GGUF",
		EN: "R83 n_ctx estimate unavailable: no VRAM/RAM/GGUF metrics",
	},
	"cppworker.nctx_feasibility.log_metadata_missing": {
		RU: "R83 гейт n_ctx: метаданные модели недоступны — проверка пропущена",
		EN: "R83 n_ctx gate: model metadata is unavailable — the check was skipped",
	},
	"cppworker.nctx_feasibility.log_config_ceiling": {
		RU: "R83 потолок n_ctx из конфига недостижим на этом железе: " +
			"CPPWORKER_RAM_FALLBACK_MAX_N_CTX больше физического предела — " +
			"уменьшите его, иначе конфиг обещает то, чего не будет",
		EN: "R83 the n_ctx ceiling from the config is unreachable on this hardware: " +
			"CPPWORKER_RAM_FALLBACK_MAX_N_CTX is above the physical ceiling — " +
			"lower it, otherwise the config promises something it cannot deliver",
	},
	"cppworker.nctx_feasibility.suggest_no_fit": {
		RU: "Веса модели не помещаются ни в VRAM, ни в доступную RAM при любом " +
			"n_ctx — уменьшение контекста не поможет. Нужна меньшая модель или " +
			"более сильное квантование.",
		EN: "The model weights do not fit into VRAM or the available RAM at any " +
			"n_ctx — reducing the context will not help. A smaller model or " +
			"stronger quantisation is required.",
	},
	"cppworker.nctx_feasibility.suggest_train_ctx": {
		RU: "n_ctx ограничен обучающим контекстом модели: %d. " +
			"Укажите n_ctx <= %d.",
		EN: "n_ctx is limited by the model training context: %d. " +
			"Use n_ctx <= %d.",
	},
	"cppworker.nctx_feasibility.suggest_exact_fit": {
		RU: "Для загрузки целиком в VRAM используйте n_ctx <= %d " +
			"(feasible_max_context). До %d возможно с выгрузкой части слоёв в RAM " +
			"(partial_offload) — это заметно медленнее.",
		EN: "To load entirely into VRAM use n_ctx <= %d " +
			"(feasible_max_context). Up to %d is possible with some layers offloaded to RAM " +
			"(partial_offload) — that is noticeably slower.",
	},
	"cppworker.nctx_feasibility.suggest_reduce_ctx": {
		RU: "Снизьте n_ctx или используйте модель/железо с большим объёмом памяти.",
		EN: "Lower n_ctx or use a model/hardware with more memory.",
	},

	// === cmd/cppworker/memfit_gate.go: тип KV-cache и вердикт ===========
	"cppworker.memfit_gate.kv_from_plan": {
		RU: "R83 гейт n_ctx: тип KV-cache из плана загрузки",
		EN: "R83 n_ctx gate: KV-cache type from the load plan",
	},
	"cppworker.memfit_gate.kv_from_profile": {
		RU: "R83 гейт n_ctx: тип KV-cache взят из профиля модели",
		EN: "R83 n_ctx gate: KV-cache type taken from the model profile",
	},
	"cppworker.memfit_gate.kv_for_estimate": {
		RU: "R83 гейт n_ctx: тип KV-cache для оценки",
		EN: "R83 n_ctx gate: KV-cache type used for the estimate",
	},
	"cppworker.memfit_gate.verdict": {
		RU: "memfit: вердикт по памяти",
		EN: "memfit: memory verdict",
	},

	// === cmd/cppworker/handlers_model_async.go: асинхронный reload ======
	"cppworker.handlers_model_async.backend_nil": {
		RU: "runAsyncReload: backend не инициализирован — reload пропущен",
		EN: "runAsyncReload: backend is not initialised — reload skipped",
	},
	"cppworker.handlers_model_async.drain_wait": {
		RU: "runAsyncReload: waiting for in-flight to drain (не дольше 10s)",
		EN: "runAsyncReload: waiting for in-flight requests to drain (no longer than 10s)",
	},
	"cppworker.handlers_model_async.drain_timeout": {
		RU: "runAsyncReload: запросы не дренировались за 10s — перезагружаем всё равно",
		EN: "runAsyncReload: requests did not drain within 10s — reloading anyway",
	},
	"cppworker.handlers_model_async.settle_pause": {
		RU: "runAsyncReload: пауза на закрытие сессии узла перед загрузкой",
		EN: "runAsyncReload: pause to let the node session close before loading",
	},
	"cppworker.handlers_model_async.degraded_confirmed": {
		RU: "runAsyncReload: подтверждена деградированная загрузка",
		EN: "runAsyncReload: degraded load confirmed",
	},

	// === cmd/cppworker/handlers_generate.go: параметры генерации ========
	"cppworker.handlers_generate.upper_bound_noted": {
		RU: "normalizeGenerateRequest: max_output_tokens учтён как ВЕРХНЯЯ ГРАНИЦА (не как длина ответа)",
		EN: "normalizeGenerateRequest: max_output_tokens is treated as an UPPER BOUND (not as the response length)",
	},
	"cppworker.handlers_generate.upper_bound_clamped": {
		RU: "buildGenerationParams: max_output_tokens ограничил длину ответа сверху",
		EN: "buildGenerationParams: max_output_tokens capped the response length from above",
	},
	"cppworker.handlers_generate.reasoning_split": {
		RU: "writeStreamResponse: reasoning отделён от ответа",
		EN: "writeStreamResponse: reasoning separated from the response",
	},

	// === cmd/cppworker/utils.go: токен и подсказки в ответах =============
	"cppworker.utils.token_not_set": {
		RU: "не задан (защищённые эндпоинты открыты)",
		EN: "not set (protected endpoints are open)",
	},
	"cppworker.utils.hint_model_not_found": {
		RU: "проверьте имя модели (алиасы) и что файл .gguf лежит в каталоге моделей; " +
			"список доступных файлов — GET /api/models/files",
		EN: "check the model name (aliases) and that the .gguf file is present in the models directory; " +
			"the list of available files is served by GET /api/models/files",
	},
	"cppworker.utils.hint_rpc_node_unreachable": {
		RU: "поднимите узел раскладки (на доноре: " +
			"docker compose -f docker-compose.rpc-node.yml up -d) и повторите запрос",
		EN: "bring the layout node up (on the donor: " +
			"docker compose -f docker-compose.rpc-node.yml up -d) and retry the request",
	},
	"cppworker.utils.hint_rpc_session_lost": {
		RU: "модель выгружена, повторите запрос через несколько секунд — раскладка соберётся заново",
		EN: "the model was unloaded, retry the request in a few seconds — the layout will be rebuilt",
	},

	// === cmd/cppworker/adaptive_loader.go: бюджет загрузки ==============
	"cppworker.adaptive_loader.rpc_capacity_added": {
		RU: "EnvironmentProfile: ёмкость RPC-узлов добавлена",
		EN: "EnvironmentProfile: RPC node capacity added",
	},
	"cppworker.adaptive_loader.reload_budget_reclaim": {
		RU: "handleAdaptiveStrategy: бюджет reload'а учитывает память самой модели",
		EN: "handleAdaptiveStrategy: the reload budget accounts for the memory of the model being reloaded",
	},
	"cppworker.adaptive_loader.reload_reclaim_reason": {
		RU: "reload освобождает память перезагружаемой модели",
		EN: "the reload frees the memory of the model being reloaded",
	},

	// === cmd/cppworker/auto_offload.go: оценка числа слоёв ==============
	"cppworker.auto_offload.free_vram_unknown": {
		RU: "auto_offload: свободная VRAM неизвестна, используем полную ёмкость карты " +
			"(оценка числа слоёв оптимистична)",
		EN: "auto_offload: free VRAM is unknown, using the full card capacity " +
			"(the layer count estimate is optimistic)",
	},
	"cppworker.auto_offload.kv_real_params": {
		RU: "auto_offload: KV посчитан по реальным параметрам кэша (R83 §9.4 шаг 2)",
		EN: "auto_offload: KV computed from the real cache parameters (R83 §9.4 step 2)",
	},

	// === cmd/cppworker/handlers_effective_r116.go =======================
	"cppworker.handlers_effective_r116.ctx_hint": {
		RU: "укажите ?ctx= или задайте контекст в профиле модели",
		EN: "pass ?ctx= or set the context in the model profile",
	},
	"cppworker.handlers_effective_r116.log_effective": {
		RU: "R116 effective: эффективные параметры модели",
		EN: "R116 effective: effective model parameters",
	},

	// === cmd/cppworker/vram_detect.go ===================================
	"cppworker.vram_detect.ram_env_invalid": {
		RU: "CPPWORKER_AVAILABLE_RAM_BYTES задан, но некорректен — " +
			"автоопределение RAM пропущено, возвращаем 0 (AutoTune пропустит RAM-проверку). " +
			"Ожидалось положительное целое число байт.",
		EN: "CPPWORKER_AVAILABLE_RAM_BYTES is set but invalid — " +
			"RAM auto-detection skipped, returning 0 (AutoTune will skip the RAM check). " +
			"A positive integer number of bytes was expected.",
	},
	"cppworker.vram_detect.probe_ram_failed": {
		RU: "availableRAMBytes: memfit.ProbeRAM не смог определить память",
		EN: "availableRAMBytes: memfit.ProbeRAM could not determine the memory size",
	},

	// === cmd/cppworker/auto_tune_nctx.go ================================
	"cppworker.auto_tune_nctx.kv_conservative": {
		RU: "AutoTuneNCtx: KV посчитан консервативно (нет метаданных KV) — рекомендация n_ctx может быть ниже реально возможной",
		EN: "AutoTuneNCtx: KV computed conservatively (no KV metadata) — the n_ctx recommendation may be below what is actually possible",
	},

	// === cmd/cppworker/handlers_link_bench_r129.go ======================
	"cppworker.handlers_link_bench_r129.usage": {
		RU: "POST тело произвольного размера; воркер читает и отбрасывает его, " +
			"возвращая bytes/durationMs/mbps",
		EN: "POST a body of arbitrary size; the worker reads and discards it, " +
			"returning bytes/durationMs/mbps",
	},
	"cppworker.handlers_link_bench_r129.note": {
		RU: "измеряется пропускная способность TCP между узлами; " +
			"это не health-check и не замер протокола llama.cpp RPC",
		EN: "measures TCP throughput between nodes; " +
			"this is not a health check and not a llama.cpp RPC protocol benchmark",
	},
	"cppworker.handlers_link_bench_r129.method_not_allowed": {
		RU: "use POST (замер) or GET (описание)",
		EN: "use POST (measurement) or GET (description)",
	},
	"cppworker.handlers_link_bench_r129.body_unread": {
		RU: "тело не прочитано: %s",
		EN: "the body was not read: %s",
	},

	// === cmd/cppworker/handlers_rpc_peers_r125.go =======================
	"cppworker.handlers_rpc_peers_r125.note": {
		RU: "проверяется доступность узла (TCP) и задержка соединения; " +
			"пропускная способность — только по запросу ?bench=1 (поле linkSpeed)",
		EN: "node reachability (TCP) and connection latency are checked; " +
			"throughput only on request ?bench=1 (the linkSpeed field)",
	},

	// === cmd/cppworker/handlers_rpc_server_log_r172.go ==================
	"cppworker.handlers_rpc_server_log_r172.not_configured": {
		RU: "узел раскладки в этом процессе не настроен (CPPWORKER_RPC_SERVER_ENABLED)",
		EN: "the layout node is not configured in this process (CPPWORKER_RPC_SERVER_ENABLED)",
	},

	// === cmd/cppworker/json_decode_client_r106c.go ======================
	"cppworker.json_decode_client_r106c.unknown_fields": {
		RU: "клиент прислал поля, которых нет в структуре — игнорируем (Ollama-совместимость)",
		EN: "the client sent fields that are not in the struct — ignoring them (Ollama compatibility)",
	},

	// === cmd/cppworker/sync_profile.go ==================================
	"cppworker.sync_profile.lazy_load_failed": {
		RU: "lazy-load: не удалось подтянуть профили моделей — " +
			"использую параметры по умолчанию",
		EN: "lazy-load: failed to fetch the model profiles — " +
			"using the default parameters",
	},

	// === cmd/balancer/env_auth.go: источник токенов =====================
	"balancer.env_auth.not_configured": {
		RU: "auth не сконфигурирован",
		EN: "auth is not configured",
	},
	"balancer.env_auth.disabled": {
		RU: "auth ВЫКЛЮЧЕН (запросы без токена)",
		EN: "auth is DISABLED (requests without a token)",
	},
	"balancer.env_auth.single_replaces": {
		RU: "LB_API_TOKEN (len=%d) ЗАМЕНЯЕТ config.json",
		EN: "LB_API_TOKEN (len=%d) REPLACES config.json",
	},
	"balancer.env_auth.csv_replaces": {
		RU: "LB_AUTH_TOKENS (%d шт.) ЗАМЕНЯЕТ config.json",
		EN: "LB_AUTH_TOKENS (%d entries) REPLACES config.json",
	},
	"balancer.env_auth.bundled_default": {
		RU: "config.json: 'bundled-default' — ЗАМЕНИТЕ (LB_API_TOKEN)",
		EN: "config.json: 'bundled-default' — REPLACE IT (LB_API_TOKEN)",
	},
	"balancer.env_auth.config_count": {
		RU: "config.json (%d шт.)",
		EN: "config.json (%d entries)",
	},
	"balancer.env_auth.tokens_shadowed": {
		RU: "auth.tokens из config.json НЕ ДЕЙСТВУЕТ: список полностью переопределён переменной окружения. " +
			"Меняйте токен в deployments/.env (CPPWORKER_API_TOKEN → LB_API_TOKEN), правка config/config.json результата не даст.",
		EN: "auth.tokens from config.json has NO EFFECT: the list is fully overridden by an environment variable. " +
			"Change the token in deployments/.env (CPPWORKER_API_TOKEN → LB_API_TOKEN); editing config/config.json will not help.",
	},

	// === internal/agent/collector.go: причины «GPU не найден» ===========
	"agent.collector.reason_nvml_init": {
		RU: "nvml.Init() failed (см. лог NVML выше)",
		EN: "nvml.Init() failed (see the NVML log above)",
	},
	"agent.collector.reason_dev_nvidia0_missing": {
		RU: "/dev/nvidia0 отсутствует — NVML skip (защита от SIGSEGV)",
		EN: "/dev/nvidia0 is missing — NVML skipped (SIGSEGV protection)",
	},
	"agent.collector.reason_nvidia_smi_no_gpu": {
		RU: "nvidia-smi не вернул GPU",
		EN: "nvidia-smi returned no GPU",
	},
	"agent.collector.no_gpu_reasons": {
		RU: "No GPU detected, CPU mode. Причины: %s",
		EN: "No GPU detected, CPU mode. Reasons: %s",
	},

	// === internal/agent/collector_network.go ============================
	"agent.collector_network.cppworker_unavailable": {
		RU: "detectCppWorkerParallelism: cppworker недоступен (%v) — " +
			"вместимость оставляем на усмотрение балансера",
		EN: "detectCppWorkerParallelism: cppworker is unreachable (%v) — " +
			"leaving the capacity to the balancer",
	},
	"agent.collector_network.batched_disabled": {
		RU: "detectCppWorkerParallelism: cppworker сообщил " +
			"defaultNParallel=%d при enableBatchedParallel=false — сообщаем балансеру 1 " +
			"(legacy-путь сериализует llama_decode; иначе второй клиент ждёт молча внутри cppworker)",
		EN: "detectCppWorkerParallelism: cppworker reported " +
			"defaultNParallel=%d while enableBatchedParallel=false — reporting 1 to the balancer " +
			"(the legacy path serialises llama_decode; otherwise the second client waits silently inside cppworker)",
	},
	"agent.collector_network.parallelism_reported": {
		RU: "detectCppWorkerParallelism: cppworker сообщил defaultNParallel=%d",
		EN: "detectCppWorkerParallelism: cppworker reported defaultNParallel=%d",
	},

	// === internal/agent/collector_gpu.go ================================
	"agent.collector_gpu.metrics_unavailable": {
		RU: "GPU metrics unavailable (nvidia-smi failed, NVML returned zeros). Retry через %s.",
		EN: "GPU metrics unavailable (nvidia-smi failed, NVML returned zeros). Retry in %s.",
	},
	"agent.collector_gpu.smi_binary_missing": {
		RU: "nvidia-smi: binary not found in PATH (ожидаемо в nvidia/cuda base)",
		EN: "nvidia-smi: binary not found in PATH (expected in the nvidia/cuda base image)",
	},

	// === internal/agent/nvml_unix.go ====================================
	"agent.nvml_unix.lib_not_found": {
		RU: "[NVML] libnvidia-ml.so.1 not found в известных путях — NVML недоступен (безопасно пропускаем)",
		EN: "[NVML] libnvidia-ml.so.1 not found in the known paths — NVML is unavailable (skipping safely)",
	},
	"agent.nvml_unix.panic_during_init": {
		RU: "[NVML] Panic during Init (lib=%s): %v — NVML помечен как недоступный",
		EN: "[NVML] Panic during Init (lib=%s): %v — NVML marked as unavailable",
	},

	// === internal/api/cluster_load_eviction_r119.go =====================
	"api.cluster_load_eviction_r119.load_not_fit": {
		RU: "cluster load: загрузка не влезла рядом с другой моделью — пробуем освободить",
		EN: "cluster load: the model did not fit alongside another model — trying to free memory",
	},
	"api.cluster_load_eviction_r119.busy_note": {
		RU: "модель %q занята активными inference-запросами — выгрузка не выполнена, " +
			"поэтому вторая модель не загружена; повторите после завершения запросов",
		EN: "model %q is busy with active inference requests — the unload was not performed, " +
			"so the second model was not loaded; retry once the requests finish",
	},
	"api.cluster_load_eviction_r119.unload_failed_note": {
		RU: "не удалось выгрузить %q, чтобы освободить память (%s)",
		EN: "failed to unload %q to free memory (%s)",
	},
	"api.cluster_load_eviction_r119.loaded_after_evict": {
		RU: "cluster load: загружено после выгрузки другой модели",
		EN: "cluster load: loaded after unloading another model",
	},
	"api.cluster_load_eviction_r119.loaded_after_evict_note": {
		RU: "загружено после выгрузки %q: памяти не хватало на две модели",
		EN: "loaded after unloading %q: there was not enough memory for two models",
	},
	"api.cluster_load_eviction_r119.retry_failed_note": {
		RU: "загрузка не удалась даже после выгрузки %q: %s",
		EN: "the load failed even after unloading %q: %s",
	},

	// === internal/api/handlers.go: вход в WebUI =========================
	"api.handlers.auth_webui_enabled": {
		RU: "auth: WebUI-вход включён (/users, сессии)",
		EN: "auth: WebUI sign-in is enabled (/users, sessions)",
	},
	"api.handlers.auth_no_users": {
		RU: "auth: пользователей нет — вход в WebUI недоступен, работает только X-API-Token (break-glass)",
		EN: "auth: no users configured — WebUI sign-in is unavailable, only X-API-Token works (break-glass)",
	},

	// === internal/api/handlers_autotune_admin.go ========================
	"api.handlers_autotune_admin.config_updated": {
		RU: "AutoTune config updated (in-memory; reload balancer для персистенции в bundled config.json если не через runtime overrides)",
		EN: "AutoTune config updated (in-memory; reload the balancer to persist it into the bundled config.json unless runtime overrides are used)",
	},

	// === internal/api/handlers_backends.go ==============================
	"api.handlers_backends.current_config_optimal": {
		RU: "Текущая конфигурация оптимальна. Альтернативные варианты показаны для ознакомления.",
		EN: "The current configuration is optimal. Alternative options are shown for information only.",
	},

	// === internal/api/handlers_model_catalog.go =========================
	"api.handlers_model_catalog.dir_unavailable": {
		RU: "каталог моделей воркера недоступен: %s",
		EN: "the worker model directory is unavailable: %s",
	},
	"api.handlers_model_catalog.no_gguf": {
		RU: "на диске воркера нет ни одного .gguf (список пуст по факту, не из-за ошибки)",
		EN: "there is not a single .gguf on the worker disk (the list is genuinely empty, not an error)",
	},
	"api.handlers_model_catalog.not_polled_yet": {
		RU: "каталог этого бэкенда ещё не опрошен: выбор для незагруженной модели работает как раньше (любой healthy узел)",
		EN: "this backend catalog has not been polled yet: selection for a non-loaded model works as before (any healthy node)",
	},
	"api.handlers_model_catalog.llamacpp_only": {
		RU: "каталог ведётся только для llama.cpp-бэкендов (у них есть /api/models/files)",
		EN: "the catalog is maintained only for llama.cpp backends (they expose /api/models/files)",
	},
	"api.handlers_model_catalog.note": {
		RU: "каталог — кэш листингов /api/models/files; «known» = снимок есть, " +
			"пустой список файлов при known=true означает, что моделей на диске нет " +
			"(а dirError — что каталог воркера недоступен)",
		EN: "the catalog is a cache of /api/models/files listings; \"known\" = a snapshot exists, " +
			"an empty file list with known=true means there are no models on disk " +
			"(while dirError means the worker directory is unavailable)",
	},
}
