package i18n

// messages_balancer — R234 (2026-10-24): сообщения балансера, батч r234
// (диагностика инструмента генерации картинок и раскладки RPC).
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ФАЙЛ. Каталог пополняется батчами по областям (см. register.go):
// параллельные правки одного messages.go конфликтуют. Здесь живут ключи файлов
// internal/balancer/{image_tool,rpc_link_measure_r118,image_batch_r99,
// image_tool_loop,image_tool_loop_ollama,image_tool_gate_r119,rpc_layout_guard,
// rpc_layout_node_r133}.go.
//
// ПРАВИЛА. Ключ — "<компонент>.<файл>.<смысл>"; RU — исходный текст байт-в-байт
// (тесты и операторские инструкции ссылаются на него), EN — тот же смысл с тем же
// набором fmt-глаголов (TestCatalogVerbParity). Текст, который уходит МОДЕЛИ в
// tool-результате, здесь намеренно не переводится: это содержимое промпта, а не
// диагностика балансера.
var messagesBalancer = map[string]map[Lang]string{
	// === image_tool.go: объявление и исполнение инструмента ============
	"balancer.image_tool.generation_timeout_hint": {
		RU: "по умолчанию капа нет: вызов инструмента ждёт терминального состояния генерации",
		EN: "there is no cap by default: the tool call waits for the terminal generation state",
	},
	"balancer.image_tool.load_timeout_hint": {
		RU: "по умолчанию ждём терминального состояния загрузки (loaded/error)",
		EN: "by default we wait for the terminal load state (loaded/error)",
	},
	"balancer.image_tool.no_backend_debug": {
		RU: "image tool: не объявляем — нет здоровых image_cpp-бэкендов",
		EN: "image tool: not declaring — there are no healthy image_cpp backends",
	},
	"balancer.image_tool.no_loaded_model_debug": {
		RU: "image tool: не объявляем — нет загруженной модели (LB_IMAGE_TOOL_ALLOW_LOAD=off)",
		EN: "image tool: not declaring — no model is loaded (LB_IMAGE_TOOL_ALLOW_LOAD=off)",
	},
	"balancer.image_tool.no_models_debug": {
		RU: "image tool: не объявляем — ни на одном image-бэкенде нет моделей",
		EN: "image tool: not declaring — none of the image backends reports models",
	},
	"balancer.image_tool.injected_info": {
		RU: "image tool: инструменты объявлены модели",
		EN: "image tool: tools declared to the model",
	},
	"balancer.image_tool.not_injected_info": {
		RU: "image tool: инструменты НЕ объявлены",
		EN: "image tool: tools NOT declared",
	},
	"balancer.image_tool.skip_disabled": {
		RU: "LB_IMAGE_TOOL=off — инструмент отключён оператором",
		EN: "LB_IMAGE_TOOL=off — the tool is disabled by the operator",
	},
	"balancer.image_tool.skip_no_backend": {
		RU: "в кластере нет здорового image_cpp-бэкенда",
		EN: "there is no healthy image_cpp backend in the cluster",
	},
	"balancer.image_tool.skip_no_models": {
		RU: "на image-бэкенде нет моделей (нечего генерировать)",
		EN: "the image backend reports no models (nothing to generate)",
	},
	"balancer.image_tool.skip_allow_load_off": {
		RU: "нет загруженной модели, а LB_IMAGE_TOOL_ALLOW_LOAD=off",
		EN: "no model is loaded and LB_IMAGE_TOOL_ALLOW_LOAD=off",
	},
	"balancer.image_tool.skip_client_none": {
		RU: "клиент прислал tool_choice=\"none\" — инструменты запрещены в этом запросе",
		EN: "the client sent tool_choice=\"none\" — tools are forbidden in this request",
	},
	"balancer.image_tool.skip_client_owns": {
		RU: "клиент сам объявил наши инструменты — исполняет его сторона",
		EN: "the client declared our tools itself — its side executes them",
	},
	"balancer.image_tool.skip_no_client_tools": {
		RU: "LB_IMAGE_TOOL_INJECT=auto, а клиент не прислал ни одного tools[] — " +
			"объявление добавило бы ~200 токенов преамбулы в промпт и увело модель в reasoning",
		EN: "LB_IMAGE_TOOL_INJECT=auto, but the client sent no tools[] — " +
			"declaring the tool would add ~200 tokens of preamble to the prompt and push the model into reasoning",
	},
	"balancer.image_tool.skip_inject_off": {
		RU: "LB_IMAGE_TOOL_INJECT=off — инструмент не объявляется никому (R216)",
		EN: "LB_IMAGE_TOOL_INJECT=off — the tool is declared to nobody (R216)",
	},
	"balancer.image_tool.args_no_prompt": {
		RU: "в аргументах нет prompt",
		EN: "the arguments contain no prompt",
	},
	"balancer.image_tool.args_not_json": {
		RU: "arguments не JSON-объект: %s",
		EN: "arguments are not a JSON object: %s",
	},
	"balancer.image_tool.resolve_missing_debug": {
		RU: "image tool: запрошенной модели нет в каталоге — отдаём имя воркеру как есть",
		EN: "image tool: the requested model is not in the catalog — passing the name to the worker as is",
	},
	"balancer.image_tool.prompt_empty": {
		RU: "prompt пуст: опиши изображение словами (движок на пустом промпте заливает картинку одним цветом)",
		EN: "the prompt is empty: describe the image in words (with an empty prompt the engine fills the picture with one solid color)",
	},
	"balancer.image_tool.prompt_too_short": {
		RU: "prompt слишком короткий (%q): нужен осмысленный текстовый запрос, иначе движок выдаст однотонную заливку",
		EN: "the prompt is too short (%q): a meaningful text request is required, otherwise the engine produces a flat solid fill",
	},
	"balancer.image_tool.prompt_degenerate": {
		RU: "prompt %q не описывает изображение (повтор одного символа): сформулируй, что нарисовать",
		EN: "the prompt %q does not describe an image (a single character repeated): say what to draw",
	},
	"balancer.image_tool.unknown_model_none": {
		RU: "модели %q нет на image-воркере, а загруженных моделей нет вовсе: " +
			"вызови list_image_models и выбери из доступного, либо попроси оператора загрузить модель",
		EN: "model %q is not on the image worker, and no models are loaded at all: " +
			"call list_image_models and pick one of the available models, or ask the operator to load a model",
	},
	"balancer.image_tool.unknown_model_list": {
		RU: "модели %q нет на image-воркере. Доступные модели (имя использовать ровно как есть): %s. " +
			"Вызови list_image_models, чтобы увидеть их описания, и повтори генерацию с одним из этих имён.",
		EN: "model %q is not on the image worker. Available models (use the name exactly as is): %s. " +
			"Call list_image_models to see their descriptions and repeat the generation with one of these names.",
	},
	"balancer.image_tool.load_request_failed": {
		RU: "не удалось отправить запрос на загрузку модели %q: %w",
		EN: "failed to send the model load request %q: %w",
	},
	"balancer.image_tool.load_read_failed": {
		RU: "чтение ответа на загрузку модели %q: %w",
		EN: "reading the model load response %q: %w",
	},
	"balancer.image_tool.load_refused": {
		RU: "воркер отказал в загрузке модели %q (HTTP %d): %s",
		EN: "the worker refused to load model %q (HTTP %d): %s",
	},
	"balancer.image_tool.load_interrupted": {
		RU: "загрузка модели %q прервана: %w",
		EN: "loading model %q was interrupted: %w",
	},
	"balancer.image_tool.load_timeout": {
		RU: "модель %q не поднялась за %s (state=%s): кап взведён явно (%s) — см. логи воркера",
		EN: "model %q did not come up within %s (state=%s): the cap was armed explicitly (%s) — see the worker logs",
	},
	"balancer.image_tool.load_failed": {
		RU: "модель %q не поднялась: %s",
		EN: "model %q did not come up: %s",
	},
	"balancer.image_tool.load_worker_error_state": {
		RU: "воркер сообщил state=error",
		EN: "the worker reported state=error",
	},
	"balancer.image_tool.load_wrong_model": {
		RU: "воркер поднял модель %q вместо запрошенной %q (одна модель на процесс; выгрузите текущую)",
		EN: "the worker loaded model %q instead of the requested %q (one model per process; unload the current one)",
	},
	"balancer.image_tool.not_loaded_allow_load_off": {
		RU: "модель %q не загружена, а автозагрузка выключена (%s=off): загрузите модель заранее",
		EN: "model %q is not loaded and auto-load is disabled (%s=off): load the model in advance",
	},
	"balancer.image_tool.gate_denied_warn": {
		RU: "image tool: гейт ресурсов отказал",
		EN: "image tool: the resource gate denied the request",
	},
	"balancer.image_tool.engine_http_error": {
		RU: "движок отказал (HTTP %d): %s",
		EN: "the engine refused (HTTP %d): %s",
	},
	"balancer.image_tool.engine_no_image": {
		RU: "движок не вернул изображение",
		EN: "the engine returned no image",
	},

	// === rpc_link_measure_r118.go: замер канала до узлов раскладки ======
	"balancer.rpc_link.unknown_address": {
		RU: "backend %s: неизвестен адрес cppworker",
		EN: "backend %s: cppworker address is unknown",
	},
	"balancer.rpc_link.probe_http_status": {
		RU: "узел %s:%d ответил HTTP %d (обработчик замера есть в сборке cppworker r118-probe1+)",
		EN: "node %s:%d responded HTTP %d (the measuring handler is available in cppworker builds r118-probe1+)",
	},
	"balancer.rpc_link.empty_measure": {
		RU: "пустой замер: %d байт за %s",
		EN: "empty measurement: %d bytes in %s",
	},
	"balancer.rpc_link.zero_mbps": {
		RU: "замер дал 0 Мбит/с (%d байт за %s)",
		EN: "the measurement gave 0 Mbit/s (%d bytes in %s)",
	},
	"balancer.rpc_link.backend_not_in_layout": {
		RU: "backend %s не участвует в раскладке (role=%q)",
		EN: "backend %s does not take part in the layout (role=%q)",
	},
	"balancer.rpc_link.measure_failed_warn": {
		RU: "RPCLinkMeasure: замер канала не удался",
		EN: "RPCLinkMeasure: the link measurement failed",
	},
	"balancer.rpc_link.measured_info": {
		RU: "RPCLinkMeasure: канал измерен",
		EN: "RPCLinkMeasure: link measured",
	},

	// === image_batch_r99.go: пачка картинок по нескольким узлам =========
	"balancer.image_batch.body_not_json": {
		RU: "image batch: тело не JSON: %w",
		EN: "image batch: the body is not JSON: %w",
	},
	"balancer.image_batch.empty_responses": {
		RU: "image batch: пустой набор ответов",
		EN: "image batch: empty set of responses",
	},
	"balancer.image_batch.part_not_json": {
		RU: "image batch: ответ узла %d не JSON: %w",
		EN: "image batch: the response of node %d is not JSON: %w",
	},
	"balancer.image_batch.node_skipped_info": {
		RU: "image batch: узел пропущен (гейт отказал)",
		EN: "image batch: node skipped (the gate denied it)",
	},
	"balancer.image_batch.merge_failed_error": {
		RU: "image batch: не удалось склеить ответы",
		EN: "image batch: failed to merge the responses",
	},

	// === image_tool_loop.go: цикл инструмента на /v1 ====================
	"balancer.image_tool_loop.prepare_failed": {
		RU: "image tool: не удалось подготовить запрос",
		EN: "image tool: failed to prepare the request",
	},
	"balancer.image_tool_loop.turn1_failed": {
		RU: "image tool: turn 1 не удался, отдаём обычным путём",
		EN: "image tool: turn 1 failed, falling back to the regular path",
	},
	"balancer.image_tool_loop.limit_exhausted": {
		RU: "лимит генераций на один запрос (%d) исчерпан",
		EN: "the per-request generation limit (%d) is exhausted",
	},
	"balancer.image_tool_loop.followup_failed": {
		RU: "image tool: не удалось собрать follow-up",
		EN: "image tool: failed to build the follow-up",
	},
	"balancer.image_tool_loop.turn2_failed": {
		RU: "image tool: turn 2 не удался — отдаём ссылку напрямую",
		EN: "image tool: turn 2 failed — returning the link directly",
	},
	"balancer.image_tool_loop.bad_args": {
		RU: "image tool: некорректные аргументы вызова",
		EN: "image tool: invalid tool call arguments",
	},
	"balancer.image_tool_loop.catalog_requested": {
		RU: "image tool: модель запросила каталог моделей",
		EN: "image tool: the model requested the model catalog",
	},
	"balancer.image_tool_loop.generating": {
		RU: "image tool: генерация по вызову модели",
		EN: "image tool: generating on the model's tool call",
	},
	"balancer.image_tool_loop.generation_failed": {
		RU: "image tool: генерация не удалась",
		EN: "image tool: generation failed",
	},
	"balancer.image_tool_loop.result_missing": {
		RU: "результат не получен",
		EN: "no result was produced",
	},
	"balancer.image_tool_loop.empty_upstream_warn": {
		RU: "synthesizeChatSSE: пустой ответ апстрима — отдаём ошибку вместо пустого успеха",
		EN: "synthesizeChatSSE: the upstream response is empty — returning an error instead of an empty success",
	},

	// === image_tool_loop_ollama.go: тот же цикл на /api/chat ============
	"balancer.image_tool_loop_ollama.prepare_failed": {
		RU: "image tool (ollama): не удалось подготовить запрос",
		EN: "image tool (ollama): failed to prepare the request",
	},
	"balancer.image_tool_loop_ollama.turn1_failed": {
		RU: "image tool (ollama): turn 1 не удался, отдаём обычным путём",
		EN: "image tool (ollama): turn 1 failed, falling back to the regular path",
	},
	"balancer.image_tool_loop_ollama.marker_unrecognized": {
		RU: "image tool (ollama): в ответе есть маркер вызова, но вызов НЕ распознан",
		EN: "image tool (ollama): the response contains a call marker, but the call was NOT recognised",
	},
	"balancer.image_tool_loop_ollama.limit_exhausted": {
		RU: "лимит генераций на один запрос (%d) исчерпан",
		EN: "the per-request generation limit (%d) is exhausted",
	},
	"balancer.image_tool_loop_ollama.followup_failed": {
		RU: "image tool (ollama): не удалось собрать follow-up",
		EN: "image tool (ollama): failed to build the follow-up",
	},
	"balancer.image_tool_loop_ollama.turn2_failed": {
		RU: "image tool (ollama): turn 2 не удался — отдаём ссылку напрямую",
		EN: "image tool (ollama): turn 2 failed — returning the link directly",
	},
	"balancer.image_tool_loop_ollama.bad_args": {
		RU: "image tool (ollama): некорректные аргументы вызова",
		EN: "image tool (ollama): invalid tool call arguments",
	},
	"balancer.image_tool_loop_ollama.catalog_requested": {
		RU: "image tool (ollama): модель запросила каталог моделей",
		EN: "image tool (ollama): the model requested the model catalog",
	},
	"balancer.image_tool_loop_ollama.generating": {
		RU: "image tool (ollama): генерация по вызову модели",
		EN: "image tool (ollama): generating on the model's tool call",
	},
	"balancer.image_tool_loop_ollama.prompt_info": {
		RU: "image tool (ollama): промпт вызова",
		EN: "image tool (ollama): tool call prompt",
	},
	"balancer.image_tool_loop_ollama.generation_failed": {
		RU: "image tool (ollama): генерация не удалась",
		EN: "image tool (ollama): generation failed",
	},
	"balancer.image_tool_loop_ollama.empty_backend_url": {
		RU: "пустой адрес бэкенда",
		EN: "the backend address is empty",
	},
	"balancer.image_tool_loop_ollama.result_missing": {
		RU: "результат не получен",
		EN: "no result was produced",
	},

	// === image_tool_gate_r119.go: структурированный отказ гейта =========
	"balancer.image_tool_gate.result_missing": {
		RU: "результат не получен",
		EN: "no result was produced",
	},

	// === rpc_layout_guard.go: раскладка слоёв на узлы ===================
	"balancer.rpc_layout.unknown_port": {
		RU: "backend %s: неизвестен порт cppworker",
		EN: "backend %s: cppworker port is unknown",
	},
	"balancer.rpc_layout.nodes_unreachable": {
		RU: "узлы раскладки недоступны (%s): раскладка выключена, загрузка идёт локально",
		EN: "the layout nodes are unreachable (%s): the layout is disabled, loading locally",
	},
	"balancer.rpc_layout.nodes_partial": {
		RU: "часть узлов раскладки недоступна (%s): работаем на живых (%s)",
		EN: "some layout nodes are unreachable (%s): continuing on the live ones (%s)",
	},
	"balancer.rpc_layout.apply_failed_warn": {
		RU: "EnsureRPCLayout: не удалось применить раскладку",
		EN: "EnsureRPCLayout: failed to apply the layout",
	},
	"balancer.rpc_layout.not_applied": {
		RU: "раскладка не применена: %v",
		EN: "the layout was not applied: %v",
	},
	"balancer.rpc_layout.degraded_warn": {
		RU: "EnsureRPCLayout: раскладка работает не в полном составе",
		EN: "EnsureRPCLayout: the layout is not running with its full set of nodes",
	},
	"balancer.rpc_layout.applied_info": {
		RU: "EnsureRPCLayout: раскладка применена",
		EN: "EnsureRPCLayout: layout applied",
	},
	"balancer.rpc_layout.disabled_info": {
		RU: "EnsureRPCLayout: раскладка выключена (самодостаточный режим)",
		EN: "EnsureRPCLayout: the layout is disabled (self-contained mode)",
	},

	// === rpc_layout_node_r133.go: узел раскладки (rpc-server) ===========
	// R315 (2026-10-30): приказ о раскладке недоступному бэкенду не отправляется.
	"balancer.rpc_layout.backend_offline": {
		RU: "бэкенд не в сети: приказ о раскладке не отправляется (узел вернётся — гард применит состав сам)",
		EN: "backend is offline: the layout order is not sent (the guard will apply the layout once the node returns)",
	},
	"balancer.rpc_layout_node.unknown_port": {
		RU: "backend %s: неизвестен порт cppworker",
		EN: "backend %s: cppworker port is unknown",
	},
	"balancer.rpc_layout_node.stop_failed_reason": {
		RU: "не удалось погасить узел раскладки после смены роли: %v",
		EN: "failed to stop the layout node after the role change: %v",
	},
	"balancer.rpc_layout_node.stop_failed_warn": {
		RU: "EnsureRPCNodeServing: узел не погашен",
		EN: "EnsureRPCNodeServing: the node was not stopped",
	},
	"balancer.rpc_layout_node.stopped_info": {
		RU: "EnsureRPCNodeServing: узел раскладки погашен (роль больше не донор)",
		EN: "EnsureRPCNodeServing: the layout node is stopped (the role is no longer donor)",
	},
	"balancer.rpc_layout_node.snapshot_refreshed_info": {
		RU: "EnsureRPCNodeServing: снимок узла обновлён принудительно — состояние отличалось от кэша",
		EN: "EnsureRPCNodeServing: the node snapshot was refreshed by force — the state differed from the cache",
	},
	"balancer.rpc_layout_node.not_reachable_reason": {
		RU: "узел раскладки поднят на %s, но с балансера недоступен " +
			"(проверьте публикацию порта при запуске контейнера и фаервол)",
		EN: "the layout node is up on %s but unreachable from the balancer " +
			"(check the port publishing at container start and the firewall)",
	},
	"balancer.rpc_layout_node.unreachable_warn": {
		RU: "EnsureRPCNodeServing: узел донора недоступен с балансера",
		EN: "EnsureRPCNodeServing: the donor node is unreachable from the balancer",
	},
	// R350: воркер донора сообщил адрес, который не отвечает, а host записи бэкенда
	// отвечает. Живой случай: донор переехал (100.64.0.7), а CPPWORKER_RPC_EXTERNAL_ADDR
	// на нём остался старый (192.168.13.34) — мастер получал мёртвый адрес и грузил
	// модель локально, хотя раскладка была включена.
	"balancer.rpc_layout_node.reported_addr_dead_using_host": {
		RU: "адрес узла из отчёта воркера не отвечает — использую host записи бэкенда",
		EN: "the node address reported by the worker does not answer — using the backend host",
	},
	"balancer.rpc_layout_node.offline_reason": {
		RU: "узел %s не в сети (балансер помечает запись недоступной): " +
			"приказ поднять узел будет отправлен, когда узел вернётся",
		EN: "node %s is offline (the balancer marks the record unavailable): " +
			"the order to start the node will be sent when the node is back",
	},
	"balancer.rpc_layout_node.order_pending_debug": {
		RU: "EnsureRPCNodeServing: приказ уже отправлен, ждём смены состояния",
		EN: "EnsureRPCNodeServing: the order has already been sent, waiting for the state to change",
	},
	"balancer.rpc_layout_node.state_mismatch_debug": {
		RU: "EnsureRPCNodeServing: состояние узла не соответствует роли донора",
		EN: "EnsureRPCNodeServing: the node state does not match the donor role",
	},
	"balancer.rpc_layout_node.start_failed_reason": {
		RU: "не удалось поднять узел раскладки: %v",
		EN: "failed to start the layout node: %v",
	},
	"balancer.rpc_layout_node.order_not_delivered_warn": {
		RU: "EnsureRPCNodeServing: приказ поднять узел не доставлен",
		EN: "EnsureRPCNodeServing: the order to start the node was not delivered",
	},
	"balancer.rpc_layout_node.order_sent_reason": {
		RU: "узлу %s отправлен приказ поднять rpc-server на порту %d: ждём подтверждения",
		EN: "node %s was ordered to start rpc-server on port %d: waiting for confirmation",
	},
	"balancer.rpc_layout_node.order_sent_info": {
		RU: "EnsureRPCNodeServing: донору отправлен приказ поднять узел раскладки",
		EN: "EnsureRPCNodeServing: the donor was ordered to start the layout node",
	},

	// === R304: донор держит только память мастера ========================
	// Пояснение уходит в GET /api/v1/backends (donorNote) и показывается в панели
	// рядом с бейджем донора: «донор» без причины оператору ничего не говорит.
	"balancer.donor.note_with_master": {
		RU: "донор мастера %s: отдаёт VRAM через rpc-server и не держит своих копий моделей",
		EN: "donor of master %s: hands VRAM to it via rpc-server and keeps no model copies of its own",
	},
	"balancer.donor.note_no_master": {
		RU: "донор (мастер не назначен): отдаёт VRAM через rpc-server и не держит своих копий моделей",
		EN: "donor (no master assigned): hands VRAM via rpc-server and keeps no model copies of its own",
	},
	"balancer.donor.load_refused": {
		RU: "узел %s — донор раскладки: он отдаёт VRAM мастеру и не поднимает своих копий моделей",
		EN: "node %s is a layout donor: it hands its VRAM to the master and does not host model copies of its own",
	},
	"balancer.donor.reload_refused": {
		RU: "узел %s — донор раскладки: он отдаёт VRAM мастеру и не держит своих копий моделей, поэтому перезагрузка окна на нём невозможна",
		EN: "node %s is a layout donor: it hands its VRAM to the master and keeps no model copies of its own, so a window reload on it is impossible",
	},
	"balancer.donor.note_layout_worker": {
		RU: "узел — воркер логического бэкенда: модель разрезана между узлами",
		EN: "the node is a worker of a logical backend: the model is split between nodes",
	},
}

func init() { Register(messagesBalancer) }
