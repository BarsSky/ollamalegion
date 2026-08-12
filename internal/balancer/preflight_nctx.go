// preflight_nctx.go — Preflight n_ctx check: проверяем, помещается ли запрос
// в текущее n_ctx бэкенда ДО отправки. Если нет — auto-reload через
// NCtxReloadCoordinator (если влезает в MaxVRAMNCtx / модель max). Если даже
// reload не поможет — структурированный 413.
//
// Проблема (Issue: «Cline → prompt 55111 токенов, current_n_ctx=32768,
// max_vram_n_ctx=32719, model_max=262144»):
//
//   Без preflight первый запрос всегда идёт «round-trip»: cppworker загружает
//   модель, делает prefill, возвращает code 2/3 → balancer решает reload →
//   второй round-trip. Latency на больших promptах — десятки секунд впустую.
//
// Решение: balancer ДО проксирования оценивает размер prompt и, если не
// хватает n_ctx, делает preflight reload. После reload запрос отправляется
// в уже подготовленную модель — round-trip один.
//
// Поток:
//  1. extractRequestMeta(r) — estimatePromptTokens() + extractNPredict()
//  2. Если estimated + n_predict + 1 <= lastKnownNCtx → NoOp
//  3. Если <= MaxVRAMNCtx*safety AND <= modelMaxContext → DecisionReload
//  4. Иначе → DecisionReject (HTTP 413 с bridge_info)
//
// Файл активируется через opt-in preflight hook в Ollama и LlamaCpp роутерах.
package balancer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"ollama-loadbalancer/pkg/logger"
)

// ============================================================
// Метаданные запроса
// ============================================================

// RequestMeta — извлечённые из HTTP-запроса метаданные для preflight-проверки.
type RequestMeta struct {
	// EstimatedPromptTokens — оценка числа токенов в prompt (chars/4).
	EstimatedPromptTokens int
	// RequestedNPredict — max_tokens / num_predict из тела (0 = default).
	RequestedNPredict int
	// RequestedNCtxOverride — num_ctx из тела (0 = не задан).
	RequestedNCtxOverride int
	// ModelName — имя модели из body (для логов).
	ModelName string
	// HasTools — true, если запрос содержит tool definitions.
	HasTools bool
	// === Round 34 (2026-08-12) Phase 2: profile mismatch detection ===
	// Параметры, которые клиент явно запросил в options.*:
	//   - kv_cache_type: "f16" | "q8_0" | "q4_0" — тип KV-cache квантизации
	//   - flash_attn: -1 (auto) | 0 (off) | 1 (on) — flash attention
	//   - use_mmap: bool — memory-map model file
	//
	// nil pointer = клиент не задал (использовать default / auto).
	// Непустое / ненулевое = клиент явно требует — должно совпадать с текущей
	// загруженной моделью, иначе → reload.
	//
	// NB: только Ollama /api/chat (через ollamaChatRequestRaw.options) реально
	// парсит эти поля. OpenAI /v1/chat/completions использует отдельный
	// path без options.* — для OpenAI эти поля всегда nil → match detection
	// не триггерит reload по params mismatch (только по n_ctx).
	RequestedKvCacheType   string
	RequestedFlashAttnType int    // -1/0/1, 0 = не задано
	RequestedUseMmap       *bool  // nil = не задано
}

// EstimatePromptTokens — простая эвристика: 1 токен ≈ 4 символа.
// Для точного подсчёта нужен tokenizer, но для preflight достаточно оценки
// (если ошибёмся на 20% — это 5-10К токенов, что всё равно меньше
// MaxVRAMNCtx*safety в типичных случаях).
func EstimatePromptTokens(prompt string) int {
	if prompt == "" {
		return 0
	}
	// Используем rune count для корректной работы с unicode.
	runes := len([]rune(prompt))
	tokens := runes / 4
	if tokens < 1 && runes > 0 {
		tokens = 1
	}
	return tokens
}

// ============================================================
// Преобразование raw HTTP body → RequestMeta
// ============================================================

// ollamaChatRequestRaw — минимальная структура для парсинга Ollama chat body.
// Не импортируем openai_types, чтобы не плодить зависимости.
type ollamaChatRequestRaw struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Tools   []json.RawMessage `json:"tools"`
	Options struct {
		NumCtx     int    `json:"num_ctx"`
		NumPredict int    `json:"num_predict"`
		KVCacheType string `json:"kv_cache_type"` // Round 34: profile mismatch
		FlashAttn   *int   `json:"flash_attn"`     // -1=auto, 0=off, 1=on
		UseMmap     *bool  `json:"use_mmap"`       // Round 34: profile mismatch
	} `json:"options"`
	Stream bool `json:"stream"`
}

// ollamaGenerateRequestRaw — минимальная структура для /api/generate.
type ollamaGenerateRequestRaw struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	System string `json:"system"`
	Options struct {
		NumCtx     int `json:"num_ctx"`
		NumPredict int `json:"num_predict"`
	} `json:"options"`
	Tools []json.RawMessage `json:"tools"`
}

// openAIChatRequestRaw — для /v1/chat/completions.
type openAIChatRequestRaw struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Tools    []json.RawMessage `json:"tools"`
	MaxTokens int             `json:"max_tokens"`
	// Round 34 follow-up: добавил OpenAI num_ctx (top-level). Без этого
	// preflight не видел requested n_ctx для OpenAI клиентов (Cline,
	// Open WebUI OpenAI-compat mode) → проксировал напрямую в cppworker →
	// cppworker возвращал 400 "n_ctx too large" → балансер конвертировал
	// в 413 → клиент получал 413 sync вместо 503+Retry-After.
	NumCtx int `json:"num_ctx"`
}

// ExtractRequestMeta пытается распарсить body как Ollama / OpenAI chat
// и вернуть RequestMeta. Body уже прочитан (balancer передаёт []byte).
// Возвращает nil, если body не распознан — caller пропускает preflight.
func ExtractRequestMeta(body []byte, path string) *RequestMeta {
	if len(body) == 0 {
		return nil
	}
	meta := &RequestMeta{}

	switch {
	case strings.HasSuffix(path, "/api/chat"):
		var req ollamaChatRequestRaw
		if err := json.Unmarshal(body, &req); err != nil {
			return nil
		}
		meta.ModelName = req.Model
		meta.RequestedNCtxOverride = req.Options.NumCtx
		meta.RequestedNPredict = req.Options.NumPredict
		meta.HasTools = len(req.Tools) > 0
		// Round 34 (2026-08-12) Phase 2: profile mismatch detection.
		// Только Ollama /api/chat парсит options.* — для OpenAI эти поля остаются нулевыми.
		meta.RequestedKvCacheType = req.Options.KVCacheType
		if req.Options.FlashAttn != nil {
			meta.RequestedFlashAttnType = *req.Options.FlashAttn
		}
		meta.RequestedUseMmap = req.Options.UseMmap
		// Конкатенируем все messages content.
		var sb strings.Builder
		for _, m := range req.Messages {
			sb.WriteString(m.Content)
			sb.WriteString("\n")
		}
		meta.EstimatedPromptTokens = EstimatePromptTokens(sb.String())

	case strings.HasSuffix(path, "/api/generate"):
		var req ollamaGenerateRequestRaw
		if err := json.Unmarshal(body, &req); err != nil {
			return nil
		}
		meta.ModelName = req.Model
		meta.RequestedNCtxOverride = req.Options.NumCtx
		meta.RequestedNPredict = req.Options.NumPredict
		meta.HasTools = len(req.Tools) > 0
		meta.EstimatedPromptTokens = EstimatePromptTokens(req.Prompt + req.System)

	case strings.HasSuffix(path, "/v1/chat/completions"):
		var req openAIChatRequestRaw
		if err := json.Unmarshal(body, &req); err != nil {
			return nil
		}
		meta.ModelName = req.Model
		meta.RequestedNCtxOverride = req.NumCtx // Round 34 follow-up
		meta.RequestedNPredict = req.MaxTokens
		meta.HasTools = len(req.Tools) > 0
		var sb strings.Builder
		for _, m := range req.Messages {
			sb.WriteString(m.Content)
			sb.WriteString("\n")
		}
		meta.EstimatedPromptTokens = EstimatePromptTokens(sb.String())
	}

	if meta.ModelName == "" && meta.EstimatedPromptTokens == 0 {
		return nil
	}
	return meta
}

// ============================================================
// Preflight decision
// ============================================================

// PreflightDecision — что делать preflight'у.
type PreflightDecision int

const (
	// PreflightNoOp — preflight не нужен (current_n_ctx покрывает).
	PreflightNoOp PreflightDecision = iota
	// PreflightReload — выполнить reload на бэкенде и подождать (sync mode).
	PreflightReload
	// PreflightReject — даже reload не поможет (VRAM/model_max потолок).
	PreflightReject
	// Round 31 #2 (2026-08-09): PreflightAsyncReload — async mode.
	// Reload запущен в фоне, balancer отдаёт 503+Retry-After клиенту.
	PreflightAsyncReload
)

// PreflightResult — результат работы preflight.
type PreflightResult struct {
	Decision     PreflightDecision
	TargetNCtx   int    // n_ctx, до которого reload'ить (для PreflightReload / PreflightAsyncReload)
	RejectStatus int    // HTTP статус для отказа (обычно 413)
	RejectBody   string // тело JSON для отказа
}

// NCtxBackendState — состояние n_ctx на бэкенде (для preflight-расчётов).
// Заполняется из cluster state + cppworker metrics.
type NCtxBackendState struct {
	BackendID       string
	CurrentNCtx     int // lastKnownNCtx из координатора
	MaxVRAMNCtx     int // из cppworker metrics (0 = unknown)
	ModelMaxContext int // из GGUF metadata (0 = unknown)
	// === Round 34 (2026-08-12) Phase 2: profile mismatch detection ===
	// Текущие параметры загруженной модели (из cppworker /api/models callback
	// + llamaCppMetricsPoller). Используются в DecidePreflight для проверки
	// совпадения с requested* полями в RequestMeta.
	CurrentKvCacheType   string // "f16"/"q8_0"/"q4_0" — "" = unknown
	CurrentFlashAttnType int    // -1/0/1, 0 = unknown
	CurrentUseMmap       bool
}

// DecidePreflight решает, нужен ли reload ДО отправки запроса.
//
// Логика:
//  1. required := estimatedTokens + nPredict + 1 + reserveSlack (10%)
//  2. Если required <= currentNCtx → NoOp
//  3. Если required > modelMaxContext (если известен) → Reject
//  4. Если required > MaxVRAMNCtx*safety → trigger Reload (partial offload)
//  5. Иначе → Reload (target = roundUpPow2(required), capped по modelMax)
//
// Round 34 follow-up: добавлен шаг 1.5 — если клиент ЯВНО указал num_ctx
// в body (RequestedNCtxOverride > 0) и это больше loaded n_ctx → trigger
// reload с target = RequestedNCtxOverride, НЕЗАВИСИМО от фактического
// размера prompt. Раньше (Round 14) считалось только по estimated
// prompt + n_predict, что для маленьких prompt'ов возвращало NoOp даже
// когда клиент явно запрашивал большее окно (например Cline с
// num_ctx=65536 на пустой prompt "hi") → запрос проксировался в
// cppworker → cppworker возвращал 400 "n_ctx too large" → 413 sync
// вместо 503+Retry-After.
//
// ВАЖНО (2026-06-24): при required > MaxVRAMNCtx*safety мы БОЛЬШЕ НЕ reject,
// а trigger reload. Причина: max_vram_n_ctx рассчитывается C-bridge для
// ТЕКУЩИХ gpu_layers. Если все слои на GPU (gpu_layers=-1) и модель большая,
// свободной VRAM почти нет → max_vram_n_ctx маленький. Но cppworker может
// сделать partial offload (уменьшить gpu_layers, вытеснить веса в RAM),
// после чего max_vram_n_ctx увеличится. Reject только если:
//   - required > modelMaxContext (модель физически не поддерживает)
//   - required > AutoReloadMaxNCtx (operator cap)
func DecidePreflight(meta *RequestMeta, state *NCtxBackendState, cfg NCtxReloadConfig) *PreflightResult {
	if meta == nil || state == nil {
		return &PreflightResult{Decision: PreflightNoOp}
	}
	// Резерв 10% + 1 токен под EOS. Cline может дослать токены в
	// function-call парсинге, поэтому reserveSlack обязателен.
	const reserveSlackPercent = 10
	reserveSlack := meta.EstimatedPromptTokens * reserveSlackPercent / 100
	nPredict := meta.RequestedNPredict
	if nPredict <= 0 {
		nPredict = 2048 // default из bridge
	}
	required := meta.EstimatedPromptTokens + nPredict + 1 + reserveSlack

	// Round 34 follow-up: если клиент ЯВНО запросил больше n_ctx чем
	// загружено → нужен reload, НЕЗАВИСИМО от фактического размера prompt.
	// Cline/OpenWebUI шлют num_ctx=65536 даже для маленьких prompts.
	if meta.RequestedNCtxOverride > 0 && state.CurrentNCtx > 0 &&
		meta.RequestedNCtxOverride > state.CurrentNCtx {
		target := meta.RequestedNCtxOverride
		if state.ModelMaxContext > 0 && target > state.ModelMaxContext {
			// Запрошено больше чем модель поддерживает — reject.
			return makePreflightReject(state, required, fmt.Sprintf(
				"requested n_ctx=%d exceeds model max context=%d",
				target, state.ModelMaxContext))
		}
		if cfg.AutoReloadMaxNCtx > 0 && target > cfg.AutoReloadMaxNCtx {
			return makePreflightReject(state, required, fmt.Sprintf(
				"requested n_ctx=%d exceeds configured auto_reload_max_n_ctx=%d",
				target, cfg.AutoReloadMaxNCtx))
		}
		logger.Get().Infow("preflight: client requested n_ctx > loaded, triggering reload",
			"backend_id", state.BackendID,
			"requested_n_ctx", target, "current_n_ctx", state.CurrentNCtx,
			"estimated_tokens", meta.EstimatedPromptTokens, "n_predict", nPredict)
		// Сразу возвращаем Reload (RunPreflight конвертирует в AsyncReload если
		// включён PreflightAsyncReload). Возвращать PreflightAsyncReload здесь
		// НЕЛЬЗЯ — switch в RunPreflight его не обрабатывает и фолбэчит на NoOp.
		return &PreflightResult{
			Decision:   PreflightReload,
			TargetNCtx: roundUpPow2(target),
		}
	}

	// Уже помещается — NoOp.
	if state.CurrentNCtx > 0 && required <= state.CurrentNCtx {
		return &PreflightResult{Decision: PreflightNoOp}
	}

	// Потолок модели (если известен) — найжестший потолок.
	if state.ModelMaxContext > 0 && required > state.ModelMaxContext {
		return makePreflightReject(state, required, fmt.Sprintf(
			"required n_ctx=%d exceeds model max context=%d (gemma-4 supports 256K — save a profile with larger context_length)",
			required, state.ModelMaxContext))
	}

	// Потолок VRAM — больше НЕ reject, а trigger reload.
	// cppworker при reload применит AutoTuneNCtx для partial offload.
	// Reject только если AutoReloadMaxNCtx (operator cap) превышен.
	if state.MaxVRAMNCtx > 0 {
		safety := cfg.effectiveSafetyFactor()
		safeMax := int(float64(state.MaxVRAMNCtx) * safety)
		if required > safeMax {
			if cfg.AutoReloadMaxNCtx > 0 && required > cfg.AutoReloadMaxNCtx {
				return makePreflightReject(state, required, fmt.Sprintf(
					"required n_ctx=%d exceeds configured auto_reload_max_n_ctx=%d (max_vram_n_ctx=%d, safety=%.2f). "+
						"Reduce prompt/tools or save a model profile with bigger context_length",
					required, cfg.AutoReloadMaxNCtx, state.MaxVRAMNCtx, safety))
			}
			// НЕ reject — trigger reload. cppworker сделает partial offload.
			logger.Get().Infow("preflight: max_vram_n_ctx small for current gpu_layers, triggering reload for partial offload",
				"backend_id", state.BackendID,
				"required_n_ctx", required,
				"max_vram_n_ctx", state.MaxVRAMNCtx,
				"safe_max", safeMax,
				"current_n_ctx", state.CurrentNCtx,
				"note", "cppworker will apply AutoTuneNCtx to reduce gpu_layers and free VRAM for KV-cache")
			// Пadeем к DecisionReload логике ниже (не return).
		}
	}

	// AutoReloadMaxNCtx (operator cap).
	if cfg.AutoReloadMaxNCtx > 0 && required > cfg.AutoReloadMaxNCtx {
		return makePreflightReject(state, required, fmt.Sprintf(
			"required n_ctx=%d exceeds configured auto_reload_max_n_ctx=%d",
			required, cfg.AutoReloadMaxNCtx))
	}

	// Compute target n_ctx: required, rounded up до степени 2, capped.
	// ВАЖНО: при partial offload target может быть БОЛЬШЕ max_vram_n_ctx*safety,
	// потому что после reload gpu_layers уменьшится и max_vram_n_ctx увеличится.
	// Поэтому НЕ зажимаем target по max_vram_n_ctx — позволяем cppworker
	// решить через AutoTuneNCtx, какой n_ctx реально achievable.
	target := required
	if cfg.AutoReloadMaxNCtx > 0 && target > cfg.AutoReloadMaxNCtx {
		target = cfg.AutoReloadMaxNCtx
	}
	// Не зажимаем по MaxVRAMNCtx — после partial offload оно изменится.
	if state.ModelMaxContext > 0 && target > state.ModelMaxContext {
		target = state.ModelMaxContext
	}
	rounded := roundUpPow2(target)
	if rounded < target {
		rounded = target
	}

	// Round 34 (2026-08-12) Phase 2: profile mismatch detection.
	// User's expectation: "если клиент присылает запрос с окном большим или
	// с включением отличных флагов от тех с которыми загружена модель то вот
	// теперь необходимо эту модель выгрузить если она загружена и загрузить
	// с новыми параметрами". Если client прислал options.kv_cache_type="q4_0",
	// а модель загружена с "f16" — reload на нужный params (не только n_ctx).
	// Если n_ctx уже fits (required <= currentNCtx), но params различаются —
	// всё равно reload.
	// NB: target n_ctx вычислен выше для n_ctx mismatch; params mismatch
	// просто форсирует Reload с тем же target (current n_ctx не меняется).
	if currentFits := state.CurrentNCtx > 0 && required <= state.CurrentNCtx; currentFits {
		if !paramsMatch(state, meta) {
			logger.Get().Infow("preflight: params mismatch with currently loaded model, triggering reload",
				"backend_id", state.BackendID,
				"current_kv_cache_type", state.CurrentKvCacheType,
				"requested_kv_cache_type", meta.RequestedKvCacheType,
				"current_flash_attn_type", state.CurrentFlashAttnType,
				"requested_flash_attn_type", meta.RequestedFlashAttnType,
				"current_use_mmap", state.CurrentUseMmap,
				"requested_use_mmap", reqStr(meta.RequestedUseMmap),
				"current_n_ctx", state.CurrentNCtx,
			)
			// Fall through to Reload decision (params mismatch forces reload).
		} else {
			return &PreflightResult{Decision: PreflightNoOp}
		}
	}

	return &PreflightResult{
		Decision:   PreflightReload,
		TargetNCtx: rounded,
	}
}

// paramsMatch — сравнивает текущие параметры модели с запрошенными клиентом.
//
// Round 34 (2026-08-12) Phase 2: profile mismatch detection.
//
//   - kv_cache_type: пустая строка = unknown → считаем совпадающим (не reload'им
//     на основе неизвестного current). Непустая requested vs непустая current
//     → сравниваем строки.

// preflightDecisionFromCfg — выбирает между Reload и AsyncReload
// в зависимости от конфигурации.
//
// Round 34 follow-up: helper для early-return path когда клиент
// явно запросил num_ctx > loaded (см. RequestedNCtxOverride check
// в DecidePreflight).
func preflightDecisionFromCfg(cfg NCtxReloadConfig) PreflightDecision {
	if cfg.PreflightAsyncReload {
		return PreflightAsyncReload
	}
	return PreflightReload
}
//   - flash_attn: 0 = не задано. -1=auto, 0=off, 1=on. Сравниваем числа.
//   - use_mmap: nil = не задано. Сравниваем bool'ы.
//
// Возвращает true если все явно заданные поля совпадают (либо current неизвестно
// и мы не можем судить).
func paramsMatch(state *NCtxBackendState, meta *RequestMeta) bool {
	// kv_cache_type: оба пустые → match. Один задан, другой нет → mismatch.
	if meta.RequestedKvCacheType != "" {
		if state.CurrentKvCacheType == "" {
			// current unknown, не можем судить — assume match (avoid unnecessary reload)
			// (cppworker ещё не сообщил callback, или preflight пришёл до метрик)
		} else if meta.RequestedKvCacheType != state.CurrentKvCacheType {
			return false
		}
	}
	// flash_attn: 0 = не задано (default в Go). Реальные значения -1, 0, 1.
	// Проверяем только если client явно прислал.
	if meta.RequestedFlashAttnType != 0 {
		if state.CurrentFlashAttnType == 0 {
			// current unknown, assume match
		} else if meta.RequestedFlashAttnType != state.CurrentFlashAttnType {
			return false
		}
	}
	// use_mmap: nil = не задано.
	if meta.RequestedUseMmap != nil {
		// current may be true OR false (bool default в Go = false).
		// Не можем отличить "false реальное" от "unknown" по default. Полагаемся
		// на то, что cppworker callback'а (Round 34 Phase 3) установит CurrentUseMmap
		// явно через UpdateLlamaCppModelLoaded(..., useMmap).
		if *meta.RequestedUseMmap != state.CurrentUseMmap {
			return false
		}
	}
	return true
}

// reqStr — formatter для optional bool в логах.
func reqStr(b *bool) string {
	if b == nil {
		return "<not set>"
	}
	if *b {
		return "true"
	}
	return "false"
}

// makePreflightReject формирует PreflightResult с HTTP 413 и JSON-телом,
// понятным для клиента и оператора.
func makePreflightReject(state *NCtxBackendState, required int, reason string) *PreflightResult {
	body := map[string]interface{}{
		"error":             "preflight: prompt + n_predict exceeds n_ctx for this backend",
		"reason":             reason,
		"required_n_ctx":     required,
		"current_n_ctx":      state.CurrentNCtx,
		"max_vram_n_ctx":     state.MaxVRAMNCtx,
		"model_max_context":  state.ModelMaxContext,
		"backend_id":         state.BackendID,
		"suggestion":         "Reduce prompt/tools/n_predict, or save a model profile with larger context_length and reload via POST /api/v1/cppworker/model-profiles/{name}/apply",
		"profile_endpoint":   "/api/v1/cppworker/model-profiles",
	}
	encoded, _ := json.Marshal(body)
	return &PreflightResult{
		Decision:     PreflightReject,
		RejectStatus: http.StatusRequestEntityTooLarge,
		RejectBody:   string(encoded),
	}
}

// ============================================================
// Preflight HTTP-вызов
// ============================================================

// RunPreflight — точка входа из роутеров. Возвращает:
//
//   - result, nil если NoOp или успешный reload
//   - result с PreflightReject если нужно отказать
//   - error если reload упал (тогда balancer вернёт 502 + retry)
//
// Использует существующий NCtxReloadCoordinator для параллельной защиты.
func (c *NCtxReloadCoordinator) RunPreflight(
	ctx context.Context,
	backendID, backendAddr string,
	meta *RequestMeta,
	state *NCtxBackendState,
	loader NCtxReloadHTTPClient,
) (*PreflightResult, error) {
	cfg := c.Config()
	decision := DecidePreflight(meta, state, cfg)
	switch decision.Decision {
	case PreflightNoOp:
		return decision, nil
	case PreflightReject:
		logger.Get().Warnw("preflight: reject (VRAM/model_max exhausted)",
			"backend_id", backendID,
			"required_n_ctx", state.CurrentNCtx,
			"reason", decision.RejectBody)
		return decision, nil
	case PreflightReload:
		if !cfg.AutoReloadNCtx {
			// Kill-switch: reload отключён в конфиге.
			logger.Get().Infow("preflight: reload disabled in config",
				"backend_id", backendID, "required_n_ctx", decision.TargetNCtx)
			reject := makePreflightReject(state, decision.TargetNCtx,
				"auto_reload_n_ctx is disabled in balancer config")
			return reject, nil
		}
		plan := &ReloadPlan{
			Decision: DecisionReload,
			NewNCtx:  decision.TargetNCtx,
			Reason: fmt.Sprintf("preflight: target=%d (current=%d, max_vram=%d, model_max=%d)",
				decision.TargetNCtx, state.CurrentNCtx, state.MaxVRAMNCtx, state.ModelMaxContext),
		}

		// Round 31 #2 (2026-08-09): async mode — не блокируем на reload.
		// Запускаем reload в goroutine и возвращаем PreflightAsyncReload.
		// Balancer отдаст клиенту 503 + Retry-After, а reload доделается в фоне.
		// Через Retry-After секунд клиент повторит, и модель уже будет готова.
		if cfg.PreflightAsyncReload {
			modelName := ""
			if meta != nil {
				modelName = meta.ModelName
			}
			logger.Get().Infow("preflight: triggering ASYNC reload (Round 31 #2)",
				"backend_id", backendID,
				"current_n_ctx", state.CurrentNCtx,
				"target_n_ctx", decision.TargetNCtx,
				"max_vram_n_ctx", state.MaxVRAMNCtx,
				"model_max_context", state.ModelMaxContext,
				"has_tools", meta != nil && meta.HasTools,
				"retry_after_sec", cfg.effectiveAsyncRetryAfter())

			// Async reload — НЕ ждём завершения. DoReload имеет внутренний
			// singleflight-like lock, так что параллельные reload'ы коалесцируются.
			go func() {
				reloadCtx, cancel := context.WithTimeout(context.Background(), cfg.effectiveTimeout())
				defer cancel()
				if err := c.DoReload(reloadCtx, backendID, backendAddr, modelName, plan, loader); err != nil {
					logger.Get().Errorw("preflight: async reload failed",
						"backend_id", backendID, "error", err)
					return
				}
				// Reload успешен — обновляем lastKnownNCtx.
				c.SetLastKnownNCtx(backendID, decision.TargetNCtx)
				logger.Get().Infow("preflight: async reload succeeded",
					"backend_id", backendID, "new_n_ctx", decision.TargetNCtx)
				c.ResetCycleCounter(backendID)
			}()

			return &PreflightResult{Decision: PreflightAsyncReload, TargetNCtx: decision.TargetNCtx}, nil
		}

		// Sync mode (default, обратно совместимо со старым поведением).
		logger.Get().Infow("preflight: triggering reload",
			"backend_id", backendID,
			"current_n_ctx", state.CurrentNCtx,
			"target_n_ctx", decision.TargetNCtx,
			"max_vram_n_ctx", state.MaxVRAMNCtx,
			"model_max_context", state.ModelMaxContext,
			"has_tools", meta != nil && meta.HasTools)

		reloadCtx, cancel := context.WithTimeout(ctx, cfg.effectiveTimeout())
		defer cancel()
		modelName := ""
		if meta != nil {
			modelName = meta.ModelName
		}
		if err := c.DoReload(reloadCtx, backendID, backendAddr, modelName, plan, loader); err != nil {
			logger.Get().Errorw("preflight: reload failed",
				"backend_id", backendID, "error", err)
			// Возвращаем reject с осмысленным сообщением.
			reject := makePreflightReject(state, decision.TargetNCtx,
				fmt.Sprintf("preflight reload failed: %v", err))
			return reject, nil
		}
		// Reload успешен — обновляем lastKnownNCtx.
		c.SetLastKnownNCtx(backendID, decision.TargetNCtx)
		logger.Get().Infow("preflight: reload succeeded",
			"backend_id", backendID, "new_n_ctx", decision.TargetNCtx)
		// Reset cycle counter (если был)
		c.ResetCycleCounter(backendID)
		return &PreflightResult{Decision: PreflightReload, TargetNCtx: decision.TargetNCtx}, nil
	}
	return &PreflightResult{Decision: PreflightNoOp}, nil
}

// RoundUpPow2 — публичный алиас для roundUpPow2 (нужен в других пакетах).
// Вынесен из nctx_reload.go:roundUpPow2 для использования в preflight.
func RoundUpPow2(v int) int {
	return roundUpPow2(v)
}