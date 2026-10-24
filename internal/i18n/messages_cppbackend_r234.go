package i18n

// messagesCppbackend — R234 (2026-10-24): сообщения бэкендов воркера, батч r234.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ФАЙЛ. Каталог пополняется батчами по областям (см. register.go):
// параллельные правки одного messages.go конфликтуют. Здесь живут ключи файлов
// internal/cppbackend/{backend,rpc_server_child,memfit_adapter,rpc_health,
// rpc_session_lost_r224,cuda_arch_guard,batched_scheduler,rpc_reach_r229}.go и
// internal/sdbackend/idle.go.
//
// ПРАВИЛА. Ключ — "<компонент>.<файл без .go>.<смысл>"; RU — исходный текст
// байт-в-байт (операторские инструкции и живые замеры ссылаются на него), EN — тот
// же смысл с тем же набором fmt-глаголов (TestCatalogVerbParity).
//
// ВАЖНО ПРО %w. В каталоге его быть не должно: i18n.T подставляет аргументы через
// fmt.Sprintf, а %w понимает только fmt.Errorf — в Sprintf он печатается как
// "%!w(...)" и обёртка ошибки теряется. Текст берётся из каталога без глагола
// ошибки, а обёртка остаётся в коде: fmt.Errorf("%s: %w", i18n.T(...), err).
var messagesCppbackend = map[string]map[Lang]string{
	// === internal/cppbackend/backend.go: загрузка модели ==============
	"cppbackend.backend.load_abort_on_timeout": {
		RU: "загрузка отменяется по таймауту — ждём выхода из native load",
		EN: "load is cancelled on timeout — waiting for the native load to exit",
	},
	"cppbackend.backend.context_per_seq_recomputed": {
		RU: "load: окно на клиента пересчитано в суммарный n_ctx",
		EN: "load: the per-client window was recalculated into the total n_ctx",
	},
	"cppbackend.backend.all_layout_nodes_unreachable": {
		RU: "загрузка: все узлы раскладки недоступны — грузим ЛОКАЛЬНО",
		EN: "load: all layout nodes are unreachable — loading LOCALLY",
	},
	"cppbackend.backend.layout_nodes_excluded": {
		RU: "загрузка: недоступные узлы раскладки исключены",
		EN: "load: unreachable layout nodes were excluded",
	},

	// === internal/cppbackend/backend.go: гейт готовности узлов =========
	"cppbackend.backend.layout_node_ready": {
		RU: "узел раскладки готов принять загрузку",
		EN: "the layout node is ready to accept the load",
	},
	"cppbackend.backend.layout_node_no_devices": {
		RU: "узел раскладки не отдал устройства за отведённое время - " +
			"загрузка пойдёт без раскладки (будет помечена layout_degraded); " +
			"вероятная причина: на узле висит сессия прошлого клиента и держит VRAM - " +
			"перезапустите rpc-server узла (rpcServerEnabled off/on) или его контейнер",
		EN: "the layout node did not hand over its devices in time - " +
			"the load will proceed without layout (it will be marked layout_degraded); " +
			"the likely cause is a previous client's session hanging on the node and holding VRAM - " +
			"restart the node's rpc-server (rpcServerEnabled off/on) or its container",
	},
	"cppbackend.backend.layout_node_stuck": {
		RU: "раскладка: узел не обслуживает команды несколько загрузок подряд - " +
			"похоже, процесс узла завис и держит VRAM; keepalive такое НЕ лечит",
		EN: "layout: the node has not served commands for several loads in a row - " +
			"the node process looks hung and holding VRAM; keepalive does NOT fix this",
	},
	"cppbackend.backend.rpc_layout_handed_to_bridge": {
		RU: "RPC-раскладка передана в мост",
		EN: "the RPC layout was handed to the bridge",
	},
	"cppbackend.backend.layout_devices_seen": {
		RU: "раскладка: какие устройства видит планировщик",
		EN: "layout: which devices the scheduler sees",
	},
	"cppbackend.backend.tensor_split_not_applied": {
		RU: "tensor split НЕ применён: посчитан для другого числа устройств - пересчитываю по факту",
		EN: "tensor split NOT applied: it was computed for a different device count - recalculating from the actual one",
	},
	"cppbackend.backend.layout_not_applied": {
		RU: "раскладка НЕ применилась: узлы переданы, а RPC-устройств в загрузке нет",
		EN: "layout NOT applied: the nodes were passed, but the load has no RPC devices",
	},

	// === internal/cppbackend/backend.go: размеры контекста =============
	"cppbackend.backend.ctx_sizes_unreadable": {
		RU: "LoadModelWithOpts: не удалось прочитать реальные размеры контекста, используем запрошенные",
		EN: "LoadModelWithOpts: could not read the real context sizes, using the requested ones",
	},
	"cppbackend.backend.ctx_window_differs": {
		RU: "LoadModelWithOpts: фактическое окно отличается от запрошенного (выравнивание llama.cpp)",
		EN: "LoadModelWithOpts: the actual window differs from the requested one (llama.cpp alignment)",
	},

	// === internal/cppbackend/backend.go: решение memfit ===============
	"cppbackend.backend.memory_verdict": {
		RU: "checkVRAMForModel: решение о памяти (memfit)",
		EN: "checkVRAMForModel: memory decision (memfit)",
	},
	"cppbackend.backend.parallel_slots_window_divided": {
		RU: "checkVRAMForModel: несколько параллельных слотов — окно делится между клиентами",
		EN: "checkVRAMForModel: several parallel slots — the window is divided between clients",
	},
	"cppbackend.backend.parallel_slots_window_hint": {
		RU: "llama.cpp делит СУММАРНОЕ окно между слотами (n_ctx_seq = n_ctx / слоты); " +
			"чтобы каждый клиент получил нужное окно, задайте contextPerSeq или суммарное n_ctx = окно × слоты",
		EN: "llama.cpp divides the TOTAL window between slots (n_ctx_seq = n_ctx / slots); " +
			"so that each client gets the window it needs, set contextPerSeq or the total n_ctx = window × slots",
	},
	"cppbackend.backend.no_model_metadata": {
		RU: "checkVRAMForModel: метаданных модели нет — раскладка слоёв без изменений",
		EN: "checkVRAMForModel: no model metadata — the layer layout stays unchanged",
	},
	"cppbackend.backend.resource_limits_ceilings": {
		RU: "CalculateResourceLimits: потолки n_ctx (memfit)",
		EN: "CalculateResourceLimits: n_ctx ceilings (memfit)",
	},

	// === internal/cppbackend/backend.go: реестр и отмена загрузки =====
	"cppbackend.backend.load_failure_entry_replaced": {
		RU: "load failure: запись в реестре заменена новым инстансом — не удаляем",
		EN: "load failure: the registry entry was replaced by a new instance — not removing it",
	},
	"cppbackend.backend.unload_new_instance": {
		RU: "unload: в реестре уже новый инстанс модели — выгрузку не делаю",
		EN: "unload: the registry already holds a new instance of the model — skipping the unload",
	},
	"cppbackend.backend.abort_request_not_applied": {
		RU: "AbortLoad: запрос отмены не применён",
		EN: "AbortLoad: the abort request was not applied",
	},
	"cppbackend.backend.abort_request_applied": {
		RU: "AbortLoad: загрузка помечена на отмену",
		EN: "AbortLoad: the load was marked for cancellation",
	},

	// === internal/cppbackend/backend.go: idle unload на ходу ==========
	"cppbackend.backend.idle_unload_disabled_runtime": {
		RU: "idle unload: автовыгрузка выключена (idleUnloadMinutes=0)",
		EN: "idle unload: auto-unload disabled (idleUnloadMinutes=0)",
	},
	"cppbackend.backend.idle_unload_enabled_runtime": {
		RU: "idle unload: автовыгрузка включена на ходу",
		EN: "idle unload: auto-unload enabled at runtime",
	},
	"cppbackend.backend.idle_unload_timeout_changed": {
		RU: "idle unload: таймаут изменён на ходу",
		EN: "idle unload: timeout changed at runtime",
	},

	// === internal/cppbackend/rpc_server_child.go: внешний узел ========
	"cppbackend.rpc_server_child.external_node_available": {
		RU: "rpc-server: внешний узел раскладки доступен (управление у оператора)",
		EN: "rpc-server: the external layout node is reachable (controlled by the operator)",
	},
	"cppbackend.rpc_server_child.external_unreachable_error": {
		RU: "внешний узел не отвечает на %s: поднимите контейнер rpc-node " +
			"(docker compose --profile rpc-node up -d rpc-node)",
		EN: "the external node does not answer at %s: start the rpc-node container " +
			"(docker compose --profile rpc-node up -d rpc-node)",
	},
	"cppbackend.rpc_server_child.external_node_unreachable": {
		RU: "rpc-server: внешний узел не отвечает",
		EN: "rpc-server: the external node does not answer",
	},

	// === internal/cppbackend/rpc_server_child.go: запуск процесса =====
	"cppbackend.rpc_server_child.start_failed_error": {
		RU: "не удалось запустить %s %s: %v",
		EN: "failed to start %s %s: %v",
	},
	"cppbackend.rpc_server_child.start_failed": {
		RU: "rpc-server: запуск не удался",
		EN: "rpc-server: start failed",
	},
	"cppbackend.rpc_server_child.node_started": {
		RU: "rpc-server: узел раскладки поднят внутри cppworker",
		EN: "rpc-server: the layout node was started inside cppworker",
	},
	"cppbackend.rpc_server_child.node_started_note": {
		RU: "отдельный контейнер ol-rpc-server больше не нужен",
		EN: "a separate ol-rpc-server container is no longer needed",
	},
	"cppbackend.rpc_server_child.port_listening": {
		RU: "rpc-server: порт узла слушается",
		EN: "rpc-server: the node port is listening",
	},
	"cppbackend.rpc_server_child.process_up_port_silent": {
		RU: "rpc-server: процесс поднят, но порт пока не отвечает",
		EN: "rpc-server: the process is up, but the port does not answer yet",
	},

	// === internal/cppbackend/rpc_server_child.go: завершение процесса ==
	"cppbackend.rpc_server_child.exited_error": {
		RU: "rpc-server завершился: %s",
		EN: "rpc-server exited: %s",
	},
	"cppbackend.rpc_server_child.exited_code_error": {
		RU: "rpc-server завершился (%s)",
		EN: "rpc-server exited (%s)",
	},
	"cppbackend.rpc_server_child.exit_code_zero": {
		RU: "код 0",
		EN: "code 0",
	},
	"cppbackend.rpc_server_child.process_exited_abnormally": {
		RU: "rpc-server: процесс завершился нештатно",
		EN: "rpc-server: the process exited abnormally",
	},
	"cppbackend.rpc_server_child.process_exited": {
		RU: "rpc-server: процесс завершился",
		EN: "rpc-server: the process exited",
	},

	// === internal/cppbackend/rpc_server_child.go: автоперезапуск ======
	"cppbackend.rpc_server_child.node_will_restart": {
		RU: "rpc-server: узел будет поднят автоматически",
		EN: "rpc-server: the node will be started automatically",
	},
	"cppbackend.rpc_server_child.restart_budget_note": {
		RU: "бюджет перезапусков — не больше %d в час",
		EN: "restart budget — no more than %d per hour",
	},
	"cppbackend.rpc_server_child.restart_failed": {
		RU: "rpc-server: автоперезапуск узла не удался",
		EN: "rpc-server: the automatic node restart failed",
	},
	"cppbackend.rpc_server_child.restarted_after_crash": {
		RU: "rpc-server: узел поднят автоматически после падения",
		EN: "rpc-server: the node was started automatically after the crash",
	},

	// === internal/cppbackend/rpc_server_child.go: остановка и статус ===
	"cppbackend.rpc_server_child.external_left_running": {
		RU: "rpc-server: внешний узел оставлен работать (гасит оператор)",
		EN: "rpc-server: the external node was left running (the operator shuts it down)",
	},
	"cppbackend.rpc_server_child.process_already_exited": {
		RU: "rpc-server: процесс уже завершён",
		EN: "rpc-server: the process has already exited",
	},
	"cppbackend.rpc_server_child.node_stopped": {
		RU: "rpc-server: узел раскладки остановлен",
		EN: "rpc-server: the layout node was stopped",
	},
	"cppbackend.rpc_server_child.status_port_not_answering": {
		RU: "процесс запущен, но порт не отвечает: %s",
		EN: "the process is running, but the port does not answer: %s",
	},
	"cppbackend.rpc_server_child.status_port_opening": {
		RU: "порт открывается (процесс только запущен)",
		EN: "the port is opening (the process was just started)",
	},

	// === internal/cppbackend/memfit_adapter.go: бюджет и снимок ========
	"cppbackend.memfit_adapter.rpc_capacity_added": {
		RU: "MemfitBudget: ёмкость RPC-узлов добавлена в бюджет",
		EN: "MemfitBudget: the RPC node capacity was added to the budget",
	},
	"cppbackend.memfit_adapter.snapshot_from_last_good": {
		RU: "снимок устройств раскладки взят из последнего успешного замера: " +
			"идёт работа, обращение к узлу заняло бы его соединение",
		EN: "the layout device snapshot was taken from the last successful measurement: " +
			"work is in progress, contacting the node would occupy its connection",
	},
	"cppbackend.memfit_adapter.snapshot_without_rpc": {
		RU: "снимок устройств взят БЕЗ RPC: идёт работа, а успешного замера узлов " +
			"ещё не было — планирование пойдёт по локальной карте",
		EN: "the device snapshot was taken WITHOUT RPC: work is in progress and the nodes " +
			"have not been measured successfully yet — planning will use the local GPU",
	},
	"cppbackend.memfit_adapter.node_unreachable_snapshot": {
		RU: "узел раскладки недоступен — снимок устройств без RPC " +
			"(планирование по локальной карте); обращение к узлу здесь уронило бы процесс: " +
			"llama.cpp на мёртвом узле вызывает abort()",
		EN: "the layout node is unreachable — device snapshot without RPC " +
			"(planning by the local GPU); contacting the node here would crash the process: " +
			"llama.cpp calls abort() on a dead node",
	},
	"cppbackend.memfit_adapter.rpc_node_no_free_vram": {
		RU: "RPC-узел без свободной видеопамяти исключён из раскладки",
		EN: "an RPC node with no free VRAM was excluded from the layout",
	},
	"cppbackend.memfit_adapter.reclaim_returned": {
		RU: "MemfitBudgetFor: память перезагружаемой модели возвращена в бюджет",
		EN: "MemfitBudgetFor: the memory of the reloaded model was returned to the budget",
	},

	// === internal/cppbackend/rpc_health.go: ошибка недоступного узла ===
	"cppbackend.rpc_health.no_connection": {
		RU: "нет соединения",
		EN: "no connection",
	},
	"cppbackend.rpc_health.node_unreachable_error": {
		RU: "RPC-узел раскладки %s недоступен (%s). Загрузка отменена намеренно: " +
			"llama.cpp на недоступном узле падает с abort() (SIGABRT) и роняет мастер. " +
			"Проверьте узел (на доноре: docker ps -> ol-rpc-server, порт 50052) и повторите запрос",
		EN: "the RPC layout node %s is unreachable (%s). The load was cancelled deliberately: " +
			"llama.cpp crashes with abort() (SIGABRT) on an unreachable node and takes the master down. " +
			"Check the node (on the donor: docker ps -> ol-rpc-server, port 50052) and repeat the request",
	},
	"cppbackend.rpc_health.tcp_no_answer_reason": {
		RU: "не отвечает TCP; недоступно узлов: %d из %d",
		EN: "TCP does not answer; unreachable nodes: %d of %d",
	},

	// === internal/cppbackend/rpc_health.go: watchdog ==================
	"cppbackend.rpc_health.node_lost_unload_pending": {
		RU: "RPC-узел потерян: модель будет выгружена в простое, " +
			"иначе первый же запрос к ней уронит мастер (abort в C-мосте)",
		EN: "the RPC node was lost: the model will be unloaded when idle, " +
			"otherwise the first request to it will crash the master (abort in the C bridge)",
	},
	"cppbackend.rpc_health.node_lost_model_busy": {
		RU: "RPC-узел потерян, но модель занята — выгрузка отложена",
		EN: "the RPC node was lost, but the model is busy — the unload was postponed",
	},
	"cppbackend.rpc_health.watchdog_model_already_unloaded": {
		RU: "RPC-watchdog: модель уже выгружена",
		EN: "RPC-watchdog: the model is already unloaded",
	},
	"cppbackend.rpc_health.watchdog_unload_failed": {
		RU: "RPC-watchdog: не удалось выгрузить модель после потери узла",
		EN: "RPC-watchdog: failed to unload the model after the node was lost",
	},
	"cppbackend.rpc_health.watchdog_model_unloaded": {
		RU: "RPC-watchdog: модель выгружена после потери узла — " +
			"следующий запрос загрузит её заново (гейт проверит узлы)",
		EN: "RPC-watchdog: the model was unloaded after the node was lost — " +
			"the next request will load it again (the gate will check the nodes)",
	},

	// === internal/cppbackend/rpc_health.go: гейт перед генерацией =====
	"cppbackend.rpc_health.inference_not_started": {
		RU: "RPC-узел недоступен — генерация не начата " +
			"(llama.cpp на мёртвом узле вызывает abort и роняет мастер)",
		EN: "the RPC node is unreachable — generation was not started " +
			"(llama.cpp calls abort on a dead node and crashes the master)",
	},
	"cppbackend.rpc_health.no_tcp_answer_before_generation": {
		RU: "не отвечает TCP перед генерацией",
		EN: "TCP does not answer before generation",
	},

	// === internal/cppbackend/rpc_session_lost_r224.go ================
	"cppbackend.rpc_session_lost_r224.session_lost_error": {
		RU: "RPC-сессия с узлом раскладки %s потеряна во время генерации: узел не отвечает " +
			"по TCP. Это не OOM и не n_ctx: узел ушёл. Модель выгружена — повторите " +
			"запрос, раскладка соберётся заново (узел проверяется перед подключением)",
		EN: "the RPC session with the layout node %s was lost during generation: the node does not " +
			"answer over TCP. This is not OOM and not n_ctx: the node is gone. The model has been unloaded — repeat the " +
			"request, the layout will be rebuilt (the node is checked before connecting)",
	},
	"cppbackend.rpc_session_lost_r224.session_lost_unloading": {
		RU: "раскладка: сессия с узлом потеряна — выгружаю модель, " +
			"чтобы следующая загрузка собрала раскладку заново",
		EN: "layout: the session with the node was lost — unloading the model " +
			"so that the next load rebuilds the layout",
	},
	"cppbackend.rpc_session_lost_r224.auto_unload_failed": {
		RU: "раскладка: авто-выгрузка после потери сессии не удалась — " +
			"выгрузите модель вручную",
		EN: "layout: the automatic unload after the session loss failed — " +
			"unload the model manually",
	},
	"cppbackend.rpc_session_lost_r224.unloaded_after_loss": {
		RU: "раскладка: модель выгружена после потери сессии — " +
			"следующий запрос загрузит её заново",
		EN: "layout: the model was unloaded after the session loss — " +
			"the next request will load it again",
	},

	// === internal/cppbackend/cuda_arch_guard.go ======================
	"cppbackend.cuda_arch_guard.arch_mismatch_detail": {
		RU: "CUDA-ядра образа не подходят этой карте: %s имеет compute capability %d.%d (sm_%s), " +
			"а образ собран под %s. Загрузка модели начнётся и даже завершится, но ПЕРВОЕ " +
			"вычисление на GPU вызовет «CUDA error: no kernel image is available for execution " +
			"on the device» и SIGABRT — процесс cppworker погибнет вместе с загруженной моделью " +
			"(снаружи это выглядит как «backend недоступен», а следующая загрузка идёт уже с " +
			"другими параметрами). Пересоберите образ с этой архитектурой: " +
			"CUDA_ARCH=%s (или CUDA_ARCH=%s, чтобы сохранить прежние карты) — " +
			"см. scripts/build-containers.ps1 -CudaArch",
		EN: "the image CUDA kernels do not fit this GPU: %s has compute capability %d.%d (sm_%s), " +
			"while the image was built for %s. The model load will start and even finish, but the FIRST " +
			"computation on the GPU will raise «CUDA error: no kernel image is available for execution " +
			"on the device» and SIGABRT — the cppworker process will die together with the loaded model " +
			"(from the outside it looks like «backend is unreachable», and the next load runs with " +
			"other parameters). Rebuild the image with this architecture: " +
			"CUDA_ARCH=%s (or CUDA_ARCH=%s to keep the previous GPUs) — " +
			"see scripts/build-containers.ps1 -CudaArch",
	},
	"cppbackend.cuda_arch_guard.arch_mismatch": {
		RU: "CUDA-архитектуры образа и карты не совпадают — GPU-вычисления уронят процесс",
		EN: "the CUDA architectures of the image and the GPU do not match — GPU computations will crash the process",
	},
	"cppbackend.cuda_arch_guard.arch_mismatch_hint": {
		RU: "пересоберите образ с CUDA_ARCH, включающим архитектуру карты",
		EN: "rebuild the image with CUDA_ARCH that includes the GPU architecture",
	},
	"cppbackend.cuda_arch_guard.arch_compatible": {
		RU: "CUDA-архитектуры: образ и карты совместимы",
		EN: "CUDA architectures: the image and the GPUs are compatible",
	},

	// === internal/cppbackend/batched_scheduler.go ====================
	"cppbackend.batched_scheduler.unsupported_sampler_params": {
		RU: "BatchedScheduler: запрос задал sampler-параметры, " +
			"которые batched-режим не поддерживает (они БУДУТ проигнорированы; " +
			"отключите enableBatchedParallel для этой модели, если это критично)",
		EN: "BatchedScheduler: the request set sampler parameters " +
			"that the batched mode does not support (they WILL be ignored; " +
			"disable enableBatchedParallel for this model if that is critical)",
	},

	// === internal/cppbackend/rpc_reach_r229.go =======================
	"cppbackend.rpc_reach_r229.node_excluded": {
		RU: "раскладка: недоступный узел исключён на время работы с моделью",
		EN: "layout: an unreachable node was excluded for the duration of the work with the model",
	},
	"cppbackend.rpc_reach_r229.node_excluded_hint": {
		RU: "llama.cpp на мёртвом узле падает abort(); модель будет работать на живых узлах локально",
		EN: "llama.cpp calls abort() on a dead node; the model will run locally on the live nodes",
	},

	// === internal/sdbackend/idle.go ==================================
	"sdbackend.idle.disabled_note": {
		RU: "idle unload disabled (idleUnloadMinutes=0) — sd-server будет держать модель в VRAM до явного unload",
		EN: "idle unload disabled (idleUnloadMinutes=0) — sd-server will keep the model in VRAM until an explicit unload",
	},
}

func init() { Register(messagesCppbackend) }
