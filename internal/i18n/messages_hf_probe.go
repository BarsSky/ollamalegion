package i18n

// messagesCore — R234 (2026-10-24): сообщения ядра репозитория, батч r234.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ФАЙЛ. Каталог пополняется батчами по областям (см. register.go):
// параллельные правки одного messages.go конфликтуют. Здесь живут ключи файлов
// internal/cppbackend/{hf_probe,rpc_version_gate_r115,load_stall_r227}.go,
// internal/config/image_resources.go, internal/sdbackend/{normalize,supervisor}.go
// и pkg/types/placement.go.
//
// ПРАВИЛА. Ключ — "<компонент>.<файл>.<смысл>"; RU — исходный текст байт-в-байт
// (тесты и операторские инструкции ссылаются на него), EN — тот же смысл с тем же
// набором fmt-глаголов (TestCatalogVerbParity).
//
// ВАЖНО ПРО %w. В каталоге его быть не должно: i18n.T подставляет аргументы через
// fmt.Sprintf, а %w понимает только fmt.Errorf — в Sprintf он печатается как
// "%!w(...)" и обёртка ошибки теряется. Текст берётся из каталога без глагола
// ошибки, а обёртка остаётся в коде: fmt.Errorf("%s: %w", i18n.T(...), err).
var messagesCore = map[string]map[Lang]string{
	// === internal/cppbackend/hf_probe.go: разбор шапки файла ==========
	"cppbackend.hf_probe.file_too_short": {
		RU: "слишком короткий файл (%d байт)",
		EN: "file is too short (%d bytes)",
	},
	"cppbackend.hf_probe.no_gguf_magic": {
		RU: "нет magic GGUF",
		EN: "no GGUF magic bytes",
	},
	"cppbackend.hf_probe.unknown_gguf_version": {
		RU: "неизвестная версия GGUF %d",
		EN: "unknown GGUF version %d",
	},
	"cppbackend.hf_probe.unknown_array_element_type": {
		RU: "неизвестный тип элемента массива %d у ключа %q",
		EN: "unknown array element type %d for the key %q",
	},
	"cppbackend.hf_probe.unknown_value_type": {
		RU: "неизвестный тип значения %d у ключа %q",
		EN: "unknown value type %d for the key %q",
	},
	"cppbackend.hf_probe.header_length_out_of_range": {
		RU: "длина JSON-заголовка вне прочитанного куска (%d байт)",
		EN: "the JSON header length is outside the read chunk (%d bytes)",
	},
	"cppbackend.hf_probe.json_header": {
		RU: "JSON-заголовок",
		EN: "JSON header",
	},

	// === internal/cppbackend/hf_probe.go: причина вердикта (Reason) ====
	"cppbackend.hf_probe.reason_gguf_unreadable": {
		RU: "GGUF-заголовок не читается: %v",
		EN: "the GGUF header cannot be read: %v",
	},
	"cppbackend.hf_probe.reason_safetensors_unreadable": {
		RU: "safetensors-заголовок не читается: %v",
		EN: "the safetensors header cannot be read: %v",
	},
	"cppbackend.hf_probe.reason_mlx_quantized": {
		RU: "MLX-квантование (тип весов %s вместе с scales/biases): stable-diffusion.cpp " +
			"читает GGUF и обычные safetensors (F16/BF16/F8), такой файл не загрузится — " +
			"нужен GGUF-квант",
		EN: "MLX quantization (weight type %s together with scales/biases): stable-diffusion.cpp " +
			"reads GGUF and regular safetensors (F16/BF16/F8), so this file will not load — " +
			"a GGUF quant is required",
	},
	"cppbackend.hf_probe.reason_not_weights_file": {
		RU: "не похоже на файл весов (.gguf/.safetensors)",
		EN: "this does not look like a weights file (.gguf/.safetensors)",
	},
	"cppbackend.hf_probe.reason_anchored": {
		RU: "движок узнаёт семейство по тензору %q",
		EN: "the engine recognises the family by the tensor %q",
	},
	"cppbackend.hf_probe.reason_anchor_dit": {
		RU: "; файл нужно подключать как --diffusion-model (семейство DiT: отдельные VAE/text encoder)",
		EN: "; the file must be attached as --diffusion-model (DiT family: separate VAE/text encoder)",
	},
	"cppbackend.hf_probe.reason_anchor_model": {
		RU: "; файл подключается как --model",
		EN: "; the file is attached as --model",
	},
	"cppbackend.hf_probe.reason_unet_two_encoders": {
		RU: "UNet с двумя текстовыми энкодерами — движок подключит как --model",
		EN: "UNet with two text encoders — the engine will attach it as --model",
	},
	"cppbackend.hf_probe.reason_unet_dim_768": {
		RU: "UNet с токен-эмбеддингом 768 — движок подключит как --model",
		EN: "UNet with a 768 token embedding — the engine will attach it as --model",
	},
	"cppbackend.hf_probe.reason_unet_dim_1024": {
		RU: "UNet с токен-эмбеддингом 1024 — движок подключит как --model",
		EN: "UNet with a 1024 token embedding — the engine will attach it as --model",
	},
	"cppbackend.hf_probe.reason_unet_dim_unknown": {
		RU: "UNet-тензоры есть, но размерности токен-эмбеддинга в заголовке нет — версию не " +
			"определить (движок решит по своей таблице)",
		EN: "UNet tensors are present, but the header has no token embedding dimension — the " +
			"version cannot be determined (the engine decides by its own table)",
	},
	"cppbackend.hf_probe.reason_no_version_tensors": {
		RU: "в заголовке нет тензоров, по которым движок узнаёт версию модели (возможно, это " +
			"VAE/text encoder/LoRA, а не diffusion-файл)",
		EN: "the header has no tensors by which the engine recognises the model version (this " +
			"may be a VAE/text encoder/LoRA rather than a diffusion file)",
	},

	// === internal/cppbackend/rpc_version_gate_r115.go =================
	"cppbackend.rpc_version_gate.node_addr_invalid": {
		RU: "некорректный адрес узла %q",
		EN: "invalid node address %q",
	},
	"cppbackend.rpc_version_gate.node_http_status": {
		RU: "узел %s ответил HTTP %d",
		EN: "node %s responded with HTTP %d",
	},
	"cppbackend.rpc_version_gate.version_lookup_failed": {
		RU: "узел раскладки: версию узнать не удалось",
		EN: "layout node: could not determine the version",
	},
	"cppbackend.rpc_version_gate.node_excluded": {
		RU: "узел раскладки исключён: версия llama.cpp не совпала",
		EN: "layout node excluded: the llama.cpp version does not match",
	},
	"cppbackend.rpc_version_gate.node_excluded_hint": {
		RU: "обновите образ узла до того же коммита; несовпадение приводит к GGML_ABORT на мастере",
		EN: "update the node image to the same commit; a mismatch leads to GGML_ABORT on the master",
	},

	// === internal/cppbackend/load_stall_r227.go =======================
	"cppbackend.load_stall.blocked": {
		RU: "загрузка заблокирована: предыдущая загрузка не завершилась за %s (вероятно, " +
			"зависла в native load — см. ERROR со стеком горутин в логе воркера)",
		EN: "load blocked: the previous load did not finish within %s (it is probably stuck in " +
			"native load — see the ERROR with the goroutine stack in the worker log)",
	},
	"cppbackend.load_stall.watchdog_fired": {
		RU: "загрузка модели не завершилась за отведённое время — печатаю стек горутин, чтобы " +
			"увидеть, на чём стоит (memfit/CUDA/RPC/GGUF)",
		EN: "the model load did not finish in time — dumping the goroutine stack to see where it " +
			"is stuck (memfit/CUDA/RPC/GGUF)",
	},

	// === internal/config/image_resources.go ===========================
	"config.image_resources.unknown_policy": {
		RU: "unknown coexistence policy %q (допустимо: exclusive, offload, dedicated или пусто = exclusive)",
		EN: "unknown coexistence policy %q (allowed: exclusive, offload, dedicated, or empty = exclusive)",
	},
	"config.image_resources.headroom_out_of_range": {
		RU: "vramHeadroomMb вне диапазона 0..%d (получено %d)",
		EN: "vramHeadroomMb is out of range 0..%d (got %d)",
	},
	"config.image_resources.queue_wait_out_of_range": {
		RU: "queueWaitTimeoutSec вне диапазона 0..%d (получено %d)",
		EN: "queueWaitTimeoutSec is out of range 0..%d (got %d)",
	},
	"config.image_resources.lock_timeout_out_of_range": {
		RU: "exclusiveLockTimeoutSec вне диапазона 0..%d (получено %d)",
		EN: "exclusiveLockTimeoutSec is out of range 0..%d (got %d)",
	},
	"config.image_resources.tool_load_out_of_range": {
		RU: "toolLoadTimeoutSec вне диапазона %d..%d (получено %d)",
		EN: "toolLoadTimeoutSec is out of range %d..%d (got %d)",
	},

	// === internal/sdbackend/normalize.go ==============================
	"sdbackend.normalize.webp_decode_failed": {
		RU: "webp не декодирован: %v; пришлите %s",
		EN: "webp was not decoded: %v; please send %s",
	},
	"sdbackend.normalize.want_png_jpeg": {
		RU: "PNG или JPEG",
		EN: "PNG or JPEG",
	},
	"sdbackend.normalize.want_png_mask": {
		RU: "PNG-маску (1 канал)",
		EN: "a PNG mask (1 channel)",
	},
	"sdbackend.normalize.notes_steps_clamped": {
		RU: "steps clamped to %d (движок молча клампит сам, но длина data[] тогда расходится с " +
			"ожиданием клиента)",
		EN: "steps clamped to %d (the engine clamps silently on its own, but then the data[] " +
			"length diverges from what the client expects)",
	},

	// === internal/sdbackend/supervisor.go =============================
	"sdbackend.supervisor.readiness_timeout": {
		RU: "readiness timeout after %s (кап взведён явно: %s)",
		EN: "readiness timeout after %s (the cap was armed explicitly: %s)",
	},
	"sdbackend.supervisor.port_busy_hint": {
		RU: "следующая загрузка подождёт освобождения порта (CUDA-процесс отпускает сокет не сразу)",
		EN: "the next load will wait for the port to be released (a CUDA process does not release the socket immediately)",
	},

	// === pkg/types/placement.go: валидация секции placement ===========
	"types.placement.invalid_strategy": {
		RU: "неизвестная стратегия %q (допустимо: %s)",
		EN: "unknown strategy %q (allowed: %s)",
	},
	"types.placement.rule_invalid_strategy": {
		RU: "%s: неизвестная стратегия %q (допустимо: %s)",
		EN: "%s: unknown strategy %q (allowed: %s)",
	},
	"types.placement.invalid_selection": {
		RU: "%s: неизвестный selection %q (допустимо: round_robin, least_loaded)",
		EN: "%s: unknown selection %q (allowed: round_robin, least_loaded)",
	},
	"types.placement.auto_prefer_invalid_strategy": {
		RU: "%s.auto.prefer[%d]: неизвестная стратегия %q",
		EN: "%s.auto.prefer[%d]: unknown strategy %q",
	},
	"types.placement.invalid_fallback": {
		RU: "placement.fallback: неизвестное значение %q (допустимо: %q, %q)",
		EN: "placement.fallback: unknown value %q (allowed: %q, %q)",
	},
	"types.placement.enabled_without_rules": {
		RU: "placement.enabled=true, но не задано ни одного правила (models[] или classes[]) — " +
			"политика не на что не влияет",
		EN: "placement.enabled=true, but no rule is set (models[] or classes[]) — the policy has " +
			"no effect on anything",
	},
	"types.placement.empty_model": {
		RU: "пустое поле model (нужно имя модели или маска)",
		EN: "empty model field (a model name or a mask is required)",
	},
	"types.placement.pool_without_list": {
		RU: "strategy=pool без списка pool[] — пул будет взят из " +
			"balancing.virtualModels[].backendPool, если модель зарегистрирована как " +
			"виртуальный алиас (alias-on-pool)",
		EN: "strategy=pool without a pool[] list — the pool will be taken from " +
			"balancing.virtualModels[].backendPool if the model is registered as a virtual " +
			"alias (alias-on-pool)",
	},
	"types.placement.rpc_not_executable": {
		RU: "%s: стратегия %q не исполняется (legacy-путь координатора-заглушки; §9 плана " +
			"переводит rpc_coordinator в sharded — используйте strategy=sharded) — запросы " +
			"пойдут по fallback=%s",
		EN: "%s: strategy %q is not executed (legacy stub-coordinator path; §9 of the plan moves " +
			"rpc_coordinator to sharded — use strategy=sharded) — requests will go by fallback=%s",
	},
	"types.placement.empty_match": {
		RU: "пустое match — нужно хотя бы одно условие (match.model или match.sizeGB)",
		EN: "empty match — at least one condition is required (match.model or match.sizeGB)",
	},
	"types.placement.empty_size_expression": {
		RU: "пустое выражение размера (примеры: \">12\", \"<4\", \"4-8\")",
		EN: "empty size expression (examples: \">12\", \"<4\", \"4-8\")",
	},
	"types.placement.not_a_number": {
		RU: "не число: %q",
		EN: "not a number: %q",
	},
	"types.placement.range_reversed": {
		RU: "диапазон %q перевёрнут: нижняя граница больше верхней",
		EN: "range %q is reversed: the lower bound is greater than the upper bound",
	},
	"types.placement.unknown_size_expression": {
		RU: "неизвестное выражение размера %q (примеры: \">12\", \"<4\", \"4-8\")",
		EN: "unknown size expression %q (examples: \">12\", \"<4\", \"4-8\")",
	},
}

func init() { Register(messagesCore) }
