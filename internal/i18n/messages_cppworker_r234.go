package i18n

// R234: сообщения cppworker — вторая часть (батч по assigned-файлам).
//
// Здесь живут ключи областей: handlers_chat (стрим /api/chat и tools-путь),
// handlers_model (load/unload/reload), runtime_overrides_r119 (операторские
// переопределения из WebUI), lazyload (ленивая загрузка и профиль модели),
// main (таймауты HTTP-сервера, режим раскладки, rpc-server) и handlers_openai
// (Chat Completions / Completions: reasoning, кап max_tokens, logit_bias).
//
// Тексты перенесены из кода без правки смысла: RU — исходная формулировка,
// EN — перевод с сохранением технических терминов (backend, worker, model,
// layer, context, slot, batch, RPC, VRAM, GGUF). Набор fmt-глаголов в ru и en
// одинаков — это проверяет TestCatalogVerbParity.
var messagesCppworker = map[string]map[Lang]string{
	// === cppworker: /api/chat, стрим ответа (handlers_chat.go) ==========
	"cppworker.handlers_chat.max_output_tokens_upper_bound": {
		RU: "buildGenerateRequestFromChat: max_output_tokens учтён как ВЕРХНЯЯ ГРАНИЦА",
		EN: "buildGenerateRequestFromChat: max_output_tokens treated as an UPPER BOUND",
	},
	"cppworker.handlers_chat.empty_output_retry": {
		RU: "writeChatStreamResponse: пустой вывод модели — повторная генерация",
		EN: "writeChatStreamResponse: model output is empty — regenerating",
	},
	"cppworker.handlers_chat.empty_output_retry_failed": {
		RU: "writeChatStreamResponse: повторная генерация не удалась",
		EN: "writeChatStreamResponse: regeneration failed",
	},
	"cppworker.handlers_chat.empty_output_retry_done": {
		RU: "writeChatStreamResponse: повторная генерация завершена",
		EN: "writeChatStreamResponse: regeneration finished",
	},
	"cppworker.handlers_chat.tools_reasoning_block_closed": {
		RU: "writeChatStreamResponseWithTools: блок размышлений закрыт — дельты ответа отдаём",
		EN: "writeChatStreamResponseWithTools: reasoning block closed — sending answer deltas",
	},
	"cppworker.handlers_chat.tools_starts_with_reasoning": {
		RU: "writeChatStreamResponseWithTools: вывод начинается с reasoning-блока — дельты не отдаём",
		EN: "writeChatStreamResponseWithTools: output starts with a reasoning block — no deltas sent",
	},
	"cppworker.handlers_chat.tools_starts_with_service_token": {
		RU: "writeChatStreamResponseWithTools: вывод начинается со служебного токена — дельты не отдаём",
		EN: "writeChatStreamResponseWithTools: output starts with a service token — no deltas sent",
	},
	"cppworker.handlers_chat.tools_marker_found": {
		RU: "writeChatStreamResponseWithTools: маркер tool call — content больше не отдаём дельтами",
		EN: "writeChatStreamResponseWithTools: tool call marker — content is no longer sent as deltas",
	},
	"cppworker.handlers_chat.tools_json_call_in_prose": {
		RU: "writeChatStreamResponseWithTools: JSON вызова внутри прозы — content больше не отдаём дельтами",
		EN: "writeChatStreamResponseWithTools: call JSON inside prose — content is no longer sent as deltas",
	},
	"cppworker.handlers_chat.tools_json_struct_in_tail": {
		RU: "writeChatStreamResponseWithTools: в хвосте началась JSON-структура — content больше не отдаём дельтами",
		EN: "writeChatStreamResponseWithTools: a JSON structure starts in the tail — content is no longer sent as deltas",
	},
	"cppworker.handlers_chat.tools_tail_held": {
		RU: "writeChatStreamResponseWithTools: хвост удержан — content больше не отдаём дельтами",
		EN: "writeChatStreamResponseWithTools: tail held back — content is no longer sent as deltas",
	},
	"cppworker.handlers_chat.tools_reasoning_split": {
		RU: "writeChatStreamResponseWithTools: reasoning отделён от ответа",
		EN: "writeChatStreamResponseWithTools: reasoning separated from the answer",
	},
	"cppworker.handlers_chat.tools_retry_limited_r188": {
		RU: "writeChatStreamResponseWithTools: R188 повтор ограничен — клиент задал длину ответа",
		EN: "writeChatStreamResponseWithTools: R188 retry limited — the client set the answer length",
	},
	"cppworker.handlers_chat.tools_retry": {
		RU: "writeChatStreamResponseWithTools: повторная генерация",
		EN: "writeChatStreamResponseWithTools: regenerating",
	},
	"cppworker.handlers_chat.tools_retry_failed": {
		RU: "writeChatStreamResponseWithTools: повторная генерация не удалась",
		EN: "writeChatStreamResponseWithTools: regeneration failed",
	},
	"cppworker.handlers_chat.tools_retry_done": {
		RU: "writeChatStreamResponseWithTools: повторная генерация завершена",
		EN: "writeChatStreamResponseWithTools: regeneration finished",
	},
	"cppworker.handlers_chat.tools_markup_unparsed": {
		RU: "writeChatStreamResponseWithTools: разметка вызова в выводе, но вызов не распознан — отдаём error-чанк (в content разметку не кладём)",
		EN: "writeChatStreamResponseWithTools: call markup present in the output but the call was not recognised — sending an error chunk (the markup is not put into content)",
	},
	"cppworker.handlers_chat.tools_markup_stripped": {
		RU: "writeChatStreamResponseWithTools: разметка вызова вырезана из видимого ответа",
		EN: "writeChatStreamResponseWithTools: call markup cut out of the visible answer",
	},
	"cppworker.handlers_chat.tools_unparsed_raw_content": {
		RU: "writeChatStreamResponseWithTools: tool call не распознан — отдаём сырой вывод как content",
		EN: "writeChatStreamResponseWithTools: tool call not recognised — sending the raw output as content",
	},
	"cppworker.handlers_chat.tools_empty_after_prefix": {
		RU: "writeChatStreamResponseWithTools: вывод пуст после отданного префикса — закрываем стрим stop",
		EN: "writeChatStreamResponseWithTools: output is empty after the emitted prefix — closing the stream with stop",
	},
	"cppworker.handlers_chat.tools_empty_inference": {
		RU: "writeChatStreamResponseWithTools: inference вернула пустой вывод — отдаём error-чанк",
		EN: "writeChatStreamResponseWithTools: inference returned empty output — sending an error chunk",
	},
	"cppworker.handlers_chat.tools_reasoning_separate_final_chunk": {
		RU: "writeChatStreamResponseWithTools: reasoning отдельно, ответ в финальном чанке",
		EN: "writeChatStreamResponseWithTools: reasoning sent separately, the answer is in the final chunk",
	},
	"cppworker.handlers_chat.tools_text_response_streamed": {
		RU: "writeChatStreamResponseWithTools: text response отдан дельтами",
		EN: "writeChatStreamResponseWithTools: text response sent as deltas",
	},

	// === cppworker: load/unload/reload модели (handlers_model.go) =======
	"cppworker.handlers_model.load_canonical_name": {
		RU: "load: имя модели канонизировано по владельцу файла",
		EN: "load: model name canonicalized to the owner of the file",
	},
	"cppworker.handlers_model.load_profile_from_file_owner": {
		RU: "handleLoadWithParams: профиль берём у владельца файла",
		EN: "handleLoadWithParams: taking the profile from the owner of the file",
	},
	"cppworker.handlers_model.load_enable_reasoning_from_profile": {
		RU: "handleLoadWithParams: enableReasoning из профиля модели (R83/v67)",
		EN: "handleLoadWithParams: enableReasoning taken from the model profile (R83/v67)",
	},
	"cppworker.handlers_model.load_refused_no_fit": {
		RU: "handleLoadWithParams: отказано — модель не влезает даже в CPU-only",
		EN: "handleLoadWithParams: refused — the model does not fit even in CPU-only",
	},
	"cppworker.handlers_model.unload_force_cancels_load": {
		RU: "unload force=true during active load: загрузка отменяется",
		EN: "unload force=true during active load: the load is cancelled",
	},
	"cppworker.handlers_model.unload_load_cancelled_message": {
		RU: "загрузка отменена, VRAM освобождена",
		EN: "load cancelled, VRAM released",
	},
	"cppworker.handlers_model.reload_profile_from_file_owner": {
		RU: "handleReloadModel: профиль берём у владельца файла",
		EN: "handleReloadModel: taking the profile from the owner of the file",
	},
	"cppworker.handlers_model.reload_enable_reasoning_from_profile": {
		RU: "handleReloadModel: enableReasoning из профиля модели (R83/v67)",
		EN: "handleReloadModel: enableReasoning taken from the model profile (R83/v67)",
	},
	"cppworker.handlers_model.reload_no_fit_forced": {
		RU: "reload: does_not_fit, но force=true — загрузка разрешена оператором",
		EN: "reload: does_not_fit, but force=true — the load is allowed by the operator",
	},
	"cppworker.handlers_model.reload_refused_no_fit": {
		RU: "reload: отказано — модель не влезает даже в CPU-only",
		EN: "reload: refused — the model does not fit even in CPU-only",
	},
	"cppworker.handlers_model.reload_cpu_only_degraded": {
		RU: "reload: загрузка в режиме CPU-only (деградация)",
		EN: "reload: loading in CPU-only mode (degraded)",
	},
	"cppworker.handlers_model.reload_wait_drain": {
		RU: "reload force=true: ждём дренажа активных запросов (не дольше 5s)",
		EN: "reload force=true: waiting for active requests to drain (no longer than 5s)",
	},
	"cppworker.handlers_model.reload_drain_timeout": {
		RU: "reload force=true: запросы не дренировались за 5s — грузим всё равно",
		EN: "reload force=true: requests did not drain within 5s — loading anyway",
	},

	// === cppworker: операторские переопределения (runtime_overrides_r119.go) ===
	"cppworker.runtime_overrides_r119.file_unparsed": {
		RU: "runtime overrides: файл не разобран, игнорирую",
		EN: "runtime overrides: the file could not be parsed, ignoring it",
	},
	"cppworker.runtime_overrides_r119.value_not_serializable": {
		RU: "runtime overrides: значение не сериализуется",
		EN: "runtime overrides: the value cannot be serialized",
	},
	"cppworker.runtime_overrides_r119.save_failed": {
		RU: "runtime overrides: не удалось сохранить",
		EN: "runtime overrides: failed to save",
	},
	"cppworker.runtime_overrides_r119.saved": {
		RU: "runtime overrides: сохранено (приоритет над env)",
		EN: "runtime overrides: saved (takes priority over env)",
	},
	"cppworker.runtime_overrides_r119.idle_unload_overridden": {
		RU: "runtime overrides: idleUnloadMinutes переопределён оператором",
		EN: "runtime overrides: idleUnloadMinutes overridden by the operator",
	},
	"cppworker.runtime_overrides_r119.idle_unload_unparsed": {
		RU: "runtime overrides: idleUnloadMinutes не разобран",
		EN: "runtime overrides: idleUnloadMinutes could not be parsed",
	},
	"cppworker.runtime_overrides_r119.rpc_server_enabled_overridden": {
		RU: "runtime overrides: rpcServerEnabled переопределён оператором",
		EN: "runtime overrides: rpcServerEnabled overridden by the operator",
	},
	"cppworker.runtime_overrides_r119.rpc_server_enabled_unparsed": {
		RU: "runtime overrides: rpcServerEnabled не разобран",
		EN: "runtime overrides: rpcServerEnabled could not be parsed",
	},
	"cppworker.runtime_overrides_r119.rpc_server_port_unparsed": {
		RU: "runtime overrides: rpcServerPort не разобран",
		EN: "runtime overrides: rpcServerPort could not be parsed",
	},
	"cppworker.runtime_overrides_r119.rpc_enabled_overridden": {
		RU: "runtime overrides: rpcEnabled переопределён применённым состоянием",
		EN: "runtime overrides: rpcEnabled overridden by the applied state",
	},
	"cppworker.runtime_overrides_r119.rpc_enabled_unparsed": {
		RU: "runtime overrides: rpcEnabled не разобран",
		EN: "runtime overrides: rpcEnabled could not be parsed",
	},
	"cppworker.runtime_overrides_r119.rpc_servers_unparsed": {
		RU: "runtime overrides: rpcServers не разобран",
		EN: "runtime overrides: rpcServers could not be parsed",
	},
	"cppworker.runtime_overrides_r119.rpc_servers_cleared": {
		RU: "runtime overrides: rpcServers очищены применённым состоянием",
		EN: "runtime overrides: rpcServers cleared by the applied state",
	},
	"cppworker.runtime_overrides_r119.rpc_servers_invalid": {
		RU: "runtime overrides: rpcServers не прошёл проверку",
		EN: "runtime overrides: rpcServers failed validation",
	},
	"cppworker.runtime_overrides_r119.rpc_servers_overridden": {
		RU: "runtime overrides: rpcServers переопределены применённым состоянием",
		EN: "runtime overrides: rpcServers overridden by the applied state",
	},

	// === cppworker: ленивая загрузка и профиль (lazyload.go) ============
	"cppworker.lazyload.armed_timeout": {
		RU: "armed lazy-load timeout (operator opt-in): загрузка модели будет " +
			"прервана по таймеру, а не по терминальному состоянию",
		EN: "armed lazy-load timeout (operator opt-in): the model load will be " +
			"interrupted by a timer, not by a terminal state",
	},
	"cppworker.lazyload.model_ready_after_wait": {
		RU: "waitForModelReady: модель готова после ожидания",
		EN: "waitForModelReady: the model became ready after waiting",
	},
	"cppworker.lazyload.model_not_ready_503": {
		RU: "waitForModelReady: модель не готова — отдаём 503 (повторите запрос)",
		EN: "waitForModelReady: the model is not ready — returning 503 (retry the request)",
	},
	"cppworker.lazyload.model_not_ready_hint": {
		RU: "CPPWORKER_LOAD_WAIT_SEC увеличивает предел ожидания",
		EN: "CPPWORKER_LOAD_WAIT_SEC raises the waiting limit",
	},
	"cppworker.lazyload.waiting_for_model": {
		RU: "waitForModelReady: ждём готовности модели",
		EN: "waitForModelReady: waiting for the model to become ready",
	},
	"cppworker.lazyload.search_started": {
		RU: "lazy-load: начинаю поиск модели",
		EN: "lazy-load: starting the model search",
	},
	"cppworker.lazyload.window_from_profile": {
		RU: "lazy-load: окно загрузки взято из профиля модели (R229)",
		EN: "lazy-load: the load context window is taken from the model profile (R229)",
	},
	"cppworker.lazyload.profiles_unavailable": {
		RU: "lazy-load: профили моделей недоступны (синхронизатор не запущен) — " +
			"загрузка пойдёт с параметрами по умолчанию",
		EN: "lazy-load: model profiles are unavailable (the synchronizer is not running) — " +
			"the load will proceed with the default parameters",
	},
	"cppworker.lazyload.profile_not_found": {
		RU: "lazy-load: профиль модели не найден — загрузка с параметрами по умолчанию " +
			"(n_ctx из CPPWORKER_CTX_SIZE)",
		EN: "lazy-load: model profile not found — loading with the default parameters " +
			"(n_ctx from CPPWORKER_CTX_SIZE)",
	},
	"cppworker.lazyload.applying_profile": {
		RU: "lazy-load: применяю профиль модели",
		EN: "lazy-load: applying the model profile",
	},
	"cppworker.lazyload.parallel_from_profile": {
		RU: "lazy-load: parallel взят из профиля модели (R135)",
		EN: "lazy-load: parallel taken from the model profile (R135)",
	},
	"cppworker.lazyload.kv_cache_from_profile": {
		RU: "lazy-load: kvCacheType взят из профиля модели (R83/v52)",
		EN: "lazy-load: kvCacheType taken from the model profile (R83/v52)",
	},
	"cppworker.lazyload.enable_reasoning_from_profile": {
		RU: "lazy-load: enableReasoning взят из профиля модели (R83/v67)",
		EN: "lazy-load: enableReasoning taken from the model profile (R83/v67)",
	},

	// === cppworker: старт и остановка процесса (main.go) ================
	"cppworker.main.write_timeout_enabled": {
		RU: "CPPWORKER_WRITE_TIMEOUT включён: сервер ОБОРВЁТ потоковый ответ по истечении срока " +
			"(это duration-кап на работу, а не таймаут опроса состояния)",
		EN: "CPPWORKER_WRITE_TIMEOUT enabled: the server will CUT the streaming response once the deadline expires " +
			"(this is a duration cap on the work, not a state polling timeout)",
	},
	"cppworker.main.read_timeout_enabled": {
		RU: "CPPWORKER_READ_TIMEOUT включён: сервер ОБОРВЁТ приём запроса по истечении срока",
		EN: "CPPWORKER_READ_TIMEOUT enabled: the server will CUT the request intake once the deadline expires",
	},
	"cppworker.main.rpc_mode": {
		RU: "RPC-режим: %s",
		EN: "RPC mode: %s",
	},
	"cppworker.main.llama_build_mismatch": {
		RU: "llama.cpp: КОММИТ В ENV НЕ СОВПАДАЕТ С БИНАРНИКОМ — раскладка с другими узлами может не поехать",
		EN: "llama.cpp: THE COMMIT IN ENV DOES NOT MATCH THE BINARY — a layout with other nodes may fail",
	},
	"cppworker.main.llama_build_mismatch_hint": {
		RU: "обновите CPPWORKER_LLAMA_BUILD в .env под образ (git -C c/llama.cpp rev-parse --short HEAD)",
		EN: "update CPPWORKER_LLAMA_BUILD in .env to match the image (git -C c/llama.cpp rev-parse --short HEAD)",
	},
	"cppworker.main.rpc_layout_requested": {
		RU: "RPC-раскладка запрошена: узлы регистрируются при загрузке модели",
		EN: "RPC layout requested: nodes are registered when the model is loaded",
	},
	"cppworker.main.rpc_server_start_failed": {
		RU: "rpc-server: узел раскладки НЕ поднялся",
		EN: "rpc-server: the layout node did NOT come up",
	},
	"cppworker.main.rpc_server_start_failed_hint": {
		RU: "проверьте CPPWORKER_RPC_SERVER_BIN (в образе это /app/rpc-server) и свободен ли порт",
		EN: "check CPPWORKER_RPC_SERVER_BIN (in the image it is /app/rpc-server) and whether the port is free",
	},
	"cppworker.main.rpc_server_started": {
		RU: "rpc-server: узел раскладки поднят внутри cppworker",
		EN: "rpc-server: the layout node is running inside cppworker",
	},
	"cppworker.main.rpc_server_started_note": {
		RU: "отдельный контейнер ol-rpc-server для этой машины больше не нужен",
		EN: "the separate ol-rpc-server container is no longer needed for this machine",
	},
	"cppworker.main.rpc_server_disabled": {
		RU: "rpc-server: узел раскладки выключен (CPPWORKER_RPC_SERVER_ENABLED)",
		EN: "rpc-server: the layout node is disabled (CPPWORKER_RPC_SERVER_ENABLED)",
	},
	"cppworker.main.rpc_server_disabled_hint": {
		RU: "включается здесь же или из WebUI (runtime-ключ rpcServerEnabled)",
		EN: "enabled here or from the WebUI (runtime key rpcServerEnabled)",
	},
	"cppworker.main.api_token_empty": {
		RU: "API token is empty: protected cppworker endpoints are UNAUTHENTICATED. " +
			"Задайте API_TOKEN (или CPPWORKER_API_TOKEN) в окружении контейнера — " +
			"иначе балансер не сможет менять параметры модели, а сам воркер открыт всем.",
		EN: "API token is empty: protected cppworker endpoints are UNAUTHENTICATED. " +
			"Set API_TOKEN (or CPPWORKER_API_TOKEN) in the container environment — " +
			"otherwise the balancer cannot change model parameters and the worker itself is open to everyone.",
	},
	"cppworker.main.model_watcher_hint": {
		RU: "0 отключает фоновую проверку",
		EN: "0 disables the background check",
	},
	"cppworker.main.rpc_server_stopping": {
		RU: "rpc-server: остановка узла раскладки",
		EN: "rpc-server: stopping the layout node",
	},
	"cppworker.main.http_timeouts": {
		RU: "cppworker HTTP server timeouts (0 = без ограничения)",
		EN: "cppworker HTTP server timeouts (0 = no limit)",
	},

	// === cppworker: OpenAI-совместимые API (handlers_openai.go) =========
	"cppworker.handlers_openai.logit_bias_ignored": {
		RU: "logit_bias принят, но НЕ применяется: C-bridge не поддерживает logit-bias/грамматики",
		EN: "logit_bias accepted but NOT applied: the C-bridge does not support logit-bias/grammars",
	},
	"cppworker.handlers_openai.logit_bias_hint": {
		RU: "используйте параметры сэмплинга (temperature/top_k/min_p/repeat_penalty) — они реально применяются",
		EN: "use the sampling parameters (temperature/top_k/min_p/repeat_penalty) — they are actually applied",
	},
	"cppworker.handlers_openai.max_output_tokens_capped_chat": {
		RU: "buildOpenAIChatParams: max_output_tokens ограничил длину ответа сверху",
		EN: "buildOpenAIChatParams: max_output_tokens capped the answer length from above",
	},
	"cppworker.handlers_openai.unclosed_reasoning_as_content": {
		RU: "handleV1ChatCompletions: модель ушла в незакрытый reasoning — " +
			"текст отдан как content, иначе клиент получил бы пустой ответ",
		EN: "handleV1ChatCompletions: the model went into unclosed reasoning — " +
			"the text was returned as content, otherwise the client would get an empty answer",
	},
	"cppworker.handlers_openai.truncated_by_max_tokens": {
		RU: "handleV1ChatCompletions: ответ обрезан капом max_tokens — " +
			"отдаём finish_reason=length (клиент должен продолжить или предупредить оператора)",
		EN: "handleV1ChatCompletions: the answer was cut by the max_tokens cap — " +
			"returning finish_reason=length (the client must continue or warn the operator)",
	},
	"cppworker.handlers_openai.static_text_stream_empty": {
		RU: "writeStaticTextStream: пустой ответ (ни content, ни reasoning) — " +
			"клиент увидит пустой ответ; проверьте max_tokens и stop-последовательности",
		EN: "writeStaticTextStream: empty answer (neither content nor reasoning) — " +
			"the client will see an empty answer; check max_tokens and the stop sequences",
	},
	"cppworker.handlers_openai.stream_unclosed_reasoning_as_content": {
		RU: "writeOpenAIChatStream: модель ушла в незакрытый reasoning — " +
			"текст отдан как content, иначе клиент получил бы пустой ответ",
		EN: "writeOpenAIChatStream: the model went into unclosed reasoning — " +
			"the text was returned as content, otherwise the client would get an empty answer",
	},
	"cppworker.handlers_openai.stream_reasoning_unclosed_length": {
		RU: "writeOpenAIChatStream: блок размышлений не закрыт — " +
			"отдаём finish_reason=length (content без дубля размышлений)",
		EN: "writeOpenAIChatStream: the reasoning block was not closed — " +
			"returning finish_reason=length (content without a duplicate of the reasoning)",
	},
	"cppworker.handlers_openai.stream_truncated_by_max_tokens": {
		RU: "writeOpenAIChatStream: ответ обрезан капом max_tokens — " +
			"отдаём finish_reason=length (клиент должен продолжить или предупредить оператора)",
		EN: "writeOpenAIChatStream: the answer was cut by the max_tokens cap — " +
			"returning finish_reason=length (the client must continue or warn the operator)",
	},
	"cppworker.handlers_openai.max_output_tokens_capped_completions": {
		RU: "handleOpenAICompletions: max_output_tokens ограничил длину ответа сверху",
		EN: "handleOpenAICompletions: max_output_tokens capped the answer length from above",
	},
}

func init() { Register(messagesCppworker) }
