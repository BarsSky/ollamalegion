// preflight_nctx.go — Preflight n_ctx check: проверяем, помещается ли запрос
// в текущее n_ctx бэкенда ДО отправки. Если нет — auto-reload через
// NCtxReloadCoordinator (если влезает в MaxVRAMNCtx / модель max). Если даже
// reload не поможет — структурированный 413.
//
// Проблема (Issue: «Cline → prompt 55111 токенов, current_n_ctx=32768,
// max_vram_n_ctx=32719, model_max=262144»):
//
//	Без preflight первый запрос всегда идёт «round-trip»: cppworker загружает
//	модель, делает prefill, возвращает code 2/3 → balancer решает reload →
//	второй round-trip. Latency на больших promptах — десятки секунд впустую.
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
	"encoding/json"
	"strings"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/tokencount"
)

// requiredNCtxForMeta — сколько контекста нужно запросу:
// prompt + ожидаемая длина ответа + 10% slack.
//
// R83/v67 (2026-10-02): ожидаемая длина — это ИСКЛЮЧИТЕЛЬНО явное намерение
// клиента (options.num_predict / max_tokens → RequestedNPredict).
// MaxOutputTokensUpperBound сюда НЕ входит намеренно: это верхняя граница
// «сколько клиент вообще примет» (Cline шлёт 32000-64000 «на всякий случай»), и
// её вклад раздувал требуемое окно — балансер делал лишний reload в большее окно
// либо отвечал 413 на короткий вопрос. Реальную длину ответа ограничивает окно
// модели: cppworker клампит num_predict по свободному месту в контексте.
func requiredNCtxForMeta(meta *RequestMeta) int {
	if meta == nil {
		return 0
	}
	required := meta.EstimatedPromptTokens + meta.RequestedNPredict + 1
	required += meta.EstimatedPromptTokens / 10
	return required
}

// ============================================================
// Метаданные запроса
// ============================================================

// RequestMeta — извлечённые из HTTP-запроса метаданные для preflight-проверки.
type RequestMeta struct {
	// EstimatedPromptTokens — оценка числа токенов в prompt.
	//
	// R302 (2026-10-29): на путях, прошедших через p.extractRequestMeta, это
	// КАЛИБРОВАННАЯ оценка: сырой heuristic pkg/tokencount, умноженный на
	// фактическое отношение actual/estimated для модели (см.
	// prompt_calibration.go). Сырое значение остаётся в
	// EstimatedPromptTokensRaw.
	EstimatedPromptTokens int
	// EstimatedPromptTokensRaw — сырая эвристическая оценка ДО калибровки.
	//
	// Живой дефект, ради которого появилось поле: на русском тексте сырая
	// оценка была 66001 токена при фактических 17616 (3.7x), и по ней
	// балансер отказывал запросу, который помещался в загруженное окно 32768.
	EstimatedPromptTokensRaw int
	// PromptBytes — длина текста промпта в байтах (только диагностика R302:
	// «prompt estimate: bytes=... nonascii=... estimated=... calibrated=...»).
	PromptBytes int
	// PromptNonASCIIShare — доля не-ASCII рун в тексте промпта (0..1).
	//
	// Нужна, чтобы отличить «завышение из-за кириллицы» от «завышения из-за
	// шаблона» и применить запасной коэффициент, пока калибровки нет.
	PromptNonASCIIShare float64
	// ExactSampleText — текст, который отправляется токенизатору модели для
	// ТОЧНОГО подсчёта (R473): содержимое сообщений плюс сериализованные tools[].
	// Пустая строка — точный подсчёт недоступен (нет текста).
	ExactSampleText string
	// PromptTokensSource — чем посчитано EstimatedPromptTokens: "estimate"
	// (эвристика pkg/tokencount + калибровка) или "tokenizer" (реальный
	// токенизатор модели через POST /api/tokenize, R473).
	PromptTokensSource string
	// RequestID — correlation-id запроса (R476): заголовок X-Request-ID, который
	// выдал балансер. Заполняется там, где есть исходный *http.Request; в
	// функциях, которые видят только тело, поле пустое (это честное «неизвестно»,
	// а не другая правда).
	RequestID string
	// RequestedNPredict — max_tokens / num_predict из тела (0 = default).
	//
	// R83/v67 (2026-10-02): сюда попадает ТОЛЬКО явное намерение клиента
	// (options.num_predict или max_tokens). max_output_tokens — НЕ намерение, а
	// верхняя граница «сколько я вообще приму» (Cline шлёт 32000-64000 «на
	// всякий случай»), и раньше он подставлялся сюда как ожидаемая длина ответа.
	// Последствие: required = prompt + 64000 раздувал требование к окну, и
	// балансер либо перезагружал модель в большее окно, либо возвращал 413 —
	// хотя клиент просил обычный ответ. См. MaxOutputTokensUpperBound.
	RequestedNPredict int
	// MaxOutputTokensUpperBound — max_output_tokens из тела (nil = не задан).
	//
	// Верхняя граница объёма ответа, которую клиент готов принять. Используется
	// ТОЛЬКО для диагностики и для проверки «а не обрежем ли мы клиента»: она не
	// участвует ни в выборе n_ctx, ни в расчёте required, ни в решении о reload
	// и ни в оценке памяти. Реальную длину генерации ограничивает окно модели
	// (cppworker клампит num_predict по свободному месту в контексте).
	MaxOutputTokensUpperBound *int
	// RequestedNCtxOverride — num_ctx из тела (0 = не задан).
	RequestedNCtxOverride int
	// EffectiveCeiling — R323: действующий потолок авто-роста окна для этой
	// модели, посчитанный Proxy.effectiveAutoReloadCeiling = max(env, заявленное
	// в профиле). Заполняется тем, у кого есть Proxy (runInferencePreflight);
	// 0 = не заполнено — тогда preflight падает на сырое cfg.AutoReloadMaxNCtx.
	//
	// ЗАЧЕМ НА ЭТОЙ СТРУКТУРЕ. DecidePreflight — чистая функция без Proxy, и до
	// R323 она сравнивала запрошенное окно с СЫРЫМ env-потолком, хотя решатель
	// перезагрузки уже жил по правилу max(env, declared). Из-за этого запрос с
	// окном больше env получал 413 «auto_reload_ceiling_reason» даже когда
	// профиль заявлял большее окно (живая жалоба оператора про 131072).
	EffectiveCeiling int
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
	RequestedFlashAttnType int   // -1/0/1, 0 = не задано
	RequestedUseMmap       *bool // nil = не задано
	// R83-политика (2026-10-01): требование клиента по «размышлению».
	//
	// nil  = клиент не упоминал параметр (мы ничего не требуем);
	// true = клиент требует ВКЛЮЧЁННЫЙ reasoning (`think: true`/`"high"`);
	// false = клиент требует ВЫКЛЮЧЕННЫЙ (`think: false`).
	//
	// Зачем. Раньше флаговый mismatch молча игнорировался: клиент просил
	// «думать», модель была загружена без reasoning, и ответ приходил другой
	// по смыслу — без объяснения. Теперь это явная ошибка с перечислением
	// расхождений (см. semanticParamDiffs).
	RequestedThink *bool
}

// EstimatePromptTokens — оценка числа токенов в prompt.
//
// R66: использует pkg/tokencount вместо эвристики «1 токен ≈ 4 символа».
// Прежняя формула занижала счёт на любом измеренном словаре (латиница 63-81%,
// кириллица до 304%), а здесь это особенно опасно: preflight решает, влезает
// ли prompt в n_ctx, и занижение пропускает запрос за границу контекста —
// ровно тот сценарий, ради которого preflight и существует.
//
// Раньше функцию спасал только ручной порог «минимум 1 токен», который не
// решал главной проблемы: для prompt в 1000 кириллических символов эвристика
// давала 250 токенов вместо реальных ~1000.
func EstimatePromptTokens(prompt string) int {
	return tokencount.Estimate(prompt)
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
		NumCtx      int    `json:"num_ctx"`
		NumPredict  int    `json:"num_predict"`
		KVCacheType string `json:"kv_cache_type"` // Round 34: profile mismatch
		FlashAttn   *int   `json:"flash_attn"`    // -1=auto, 0=off, 1=on
		UseMmap     *bool  `json:"use_mmap"`      // Round 34: profile mismatch
	} `json:"options"`
	// R83-политика (2026-10-01): требование клиента по reasoning.
	Think json.RawMessage `json:"think"`
	// R60.55 (2026-09-13): top-level max_tokens (OpenAI-style alias for num_predict).
	MaxTokens int `json:"max_tokens"`
	// R69 (2026-09-23): max_output_tokens — ещё один алиас, который шлёт реальный
	// клиент Cline (наблюдалось на Ollama-провайдере вместе с tools[]).
	MaxOutputTokens *int `json:"max_output_tokens"`
	Stream          bool `json:"stream"`
}

// ollamaGenerateRequestRaw — минимальная структура для /api/generate.
type ollamaGenerateRequestRaw struct {
	Model   string `json:"model"`
	Prompt  string `json:"prompt"`
	System  string `json:"system"`
	Options struct {
		NumCtx     int `json:"num_ctx"`
		NumPredict int `json:"num_predict"`
	} `json:"options"`
	Tools []json.RawMessage `json:"tools"`
	// R60.55 (2026-09-13): top-level max_tokens (OpenAI-style alias for num_predict).
	MaxTokens int `json:"max_tokens"`
	// R69 (2026-09-23): max_output_tokens (Cline) — ещё один алиас num_predict.
	MaxOutputTokens *int `json:"max_output_tokens"`
}

// openAIChatRequestRaw — для /v1/chat/completions.
type openAIChatRequestRaw struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Tools     []json.RawMessage `json:"tools"`
	MaxTokens int               `json:"max_tokens"`
	// R69 (2026-09-23): max_output_tokens (Cline) — алиас max_tokens.
	MaxOutputTokens *int `json:"max_output_tokens"`
	// Round 34 follow-up: добавил OpenAI num_ctx (top-level). Без этого
	// preflight не видел requested n_ctx для OpenAI клиентов (Cline,
	// Open WebUI OpenAI-compat mode) → проксировал напрямую в cppworker →
	// cppworker возвращал 400 "n_ctx too large" → балансер конвертировал
	// в 413 → клиент получал 413 sync вместо 503+Retry-After.
	NumCtx int `json:"num_ctx"`
	// R83-политика (2026-10-01): требование клиента по reasoning (нестандартное
	// расширение, но его шлют и Ollama-клиенты, и часть OpenAI-совместимых).
	Think json.RawMessage `json:"think"`
}

// parseThinkRequirement — требование клиента по reasoning из `think`.
//
// Возвращает nil, если клиент параметр НЕ упоминал (тогда мы ничего не
// требуем и не сравниваем). Поддерживаются формы, которые реально шлют
// клиенты: bool (`true`/`false`), строка-уровень (`"high"`, `"medium"`,
// `"low"`, `"none"`, `"off"`), числа 1/0.
func parseThinkRequirement(raw json.RawMessage) *bool {
	if len(raw) == 0 {
		return nil
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return &b
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true", "on", "yes", "high", "medium", "low", "minimal":
			v := true
			return &v
		case "false", "off", "no", "none", "disabled":
			v := false
			return &v
		}
		return nil
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err == nil {
		v := n != 0
		return &v
	}
	return nil
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
		// R60.55 (2026-09-13): top-level max_tokens имеет приоритет над options.num_predict.
		// OpenWebUI в Ollama-режиме шлёт max_tokens на верхнем уровне body.
		meta.RequestedNPredict = req.Options.NumPredict
		if meta.RequestedNPredict <= 0 {
			meta.RequestedNPredict = req.MaxTokens
		}
		// R69 (2026-09-23): Cline шлёт max_output_tokens вместо num_predict/
		// max_tokens.
		// R83/v67 (2026-10-02): читаем его, но НЕ как ожидаемую длину ответа —
		// только как верхнюю границу (см. MaxOutputTokensUpperBound): иначе
		// «на всякий случай 64000» раздувало требование к окну и приводило к
		// лишнему reload/413.
		meta.MaxOutputTokensUpperBound = req.MaxOutputTokens
		meta.HasTools = len(req.Tools) > 0
		// Round 34 (2026-08-12) Phase 2: profile mismatch detection.
		// Только Ollama /api/chat парсит options.* — для OpenAI эти поля остаются нулевыми.
		meta.RequestedKvCacheType = req.Options.KVCacheType
		if req.Options.FlashAttn != nil {
			meta.RequestedFlashAttnType = *req.Options.FlashAttn
		}
		meta.RequestedUseMmap = req.Options.UseMmap
		meta.RequestedThink = parseThinkRequirement(req.Think)
		// Конкатенируем все messages content.
		var sb strings.Builder
		for _, m := range req.Messages {
			sb.WriteString(m.Content)
			sb.WriteString("\n")
		}
		meta.EstimatedPromptTokens = EstimatePromptTokens(sb.String())
		meta.PromptBytes, meta.PromptNonASCIIShare = promptTextStats(sb.String())
		meta.ExactSampleText = exactSampleWithToolsR473(sb.String(), req.Tools)
		meta.PromptTokensSource = "estimate"

	case strings.HasSuffix(path, "/api/generate"):
		var req ollamaGenerateRequestRaw
		if err := json.Unmarshal(body, &req); err != nil {
			return nil
		}
		meta.ModelName = req.Model
		meta.RequestedNCtxOverride = req.Options.NumCtx
		// R60.55 (2026-09-13): top-level max_tokens имеет приоритет над options.num_predict.
		meta.RequestedNPredict = req.Options.NumPredict
		if meta.RequestedNPredict <= 0 {
			meta.RequestedNPredict = req.MaxTokens
		}
		// R83/v67: верхняя граница, а не ожидаемая длина (см. RequestMeta).
		meta.MaxOutputTokensUpperBound = req.MaxOutputTokens
		meta.HasTools = len(req.Tools) > 0
		meta.EstimatedPromptTokens = EstimatePromptTokens(req.Prompt + req.System)
		meta.PromptBytes, meta.PromptNonASCIIShare = promptTextStats(req.Prompt + req.System)
		meta.ExactSampleText = exactSampleWithToolsR473(req.Prompt+req.System, req.Tools)
		meta.PromptTokensSource = "estimate"

	case strings.HasSuffix(path, "/v1/chat/completions"):
		var req openAIChatRequestRaw
		if err := json.Unmarshal(body, &req); err != nil {
			return nil
		}
		meta.ModelName = req.Model
		meta.RequestedNCtxOverride = req.NumCtx // Round 34 follow-up
		meta.RequestedNPredict = req.MaxTokens
		// R83/v67 (2026-10-02): max_output_tokens (Cline) — ВЕРХНЯЯ ГРАНИЦА, а не
		// алиас max_tokens. Раньше он подставлялся в RequestedNPredict, и
		// «max_output_tokens: 64000» на короткий вопрос заставлял балансер
		// считать, что клиенту нужно 64000 токенов ответа: это раздувало
		// требуемое окно (лишний reload) либо давало 413.
		meta.MaxOutputTokensUpperBound = req.MaxOutputTokens
		meta.HasTools = len(req.Tools) > 0
		var sb strings.Builder
		for _, m := range req.Messages {
			sb.WriteString(m.Content)
			sb.WriteString("\n")
		}
		meta.EstimatedPromptTokens = EstimatePromptTokens(sb.String())
		meta.PromptBytes, meta.PromptNonASCIIShare = promptTextStats(sb.String())
		meta.ExactSampleText = exactSampleWithToolsR473(sb.String(), req.Tools)
		meta.PromptTokensSource = "estimate"
		meta.RequestedThink = parseThinkRequirement(req.Think)
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
	// R60.6 (2026-09-07): EstimatedRetryAfter — рекомендуемое клиенту время
	// ожидания в секундах (для Retry-After header). 0 = use cfg default.
	// Для PreflightAsyncReload: derived from cppworker estimatedLoadTimeMs
	// (или fallback на heuristic если model size неизвестен).
	EstimatedRetryAfter int
	// R253 (2026-10-28): окно выведено балансером из оценки промпта (клиент
	// num_ctx не присылал) — для отдельной записи в логе.
	AgentDerivedWindow bool
}

// maxReserveSlackTokens — R83-fix (2026-09-30): верхняя граница «запаса» в
// расчёте required = prompt + n_predict + 1 + reserveSlack.
//
// ЗАЧЕМ. Запас нужен (округление токенизатора, EOS, доклейка токенов в
// function-call парсинге), но он НЕ должен расти линейно с длиной диалога:
// 10% от промпта на продолжении сессии в 120k токенов добавляли 12k
// «виртуальных» токенов, required превышал потолок бэкенда, и клиент получал
// 413 «preflight: prompt + n_predict exceeds n_ctx for this backend» — при том
// что история в загруженное окно помещается.
const maxReserveSlackTokens = 2048

// NCtxBackendState — состояние n_ctx на бэкенде (для preflight-расчётов).
// Заполняется из cluster state + cppworker metrics.
type NCtxBackendState struct {
	// Строковые поля — первыми (pointer data prefix, govet fieldalignment).
	BackendID string
	// === Round 34 (2026-08-12) Phase 2: profile mismatch detection ===
	// Текущие параметры загруженной модели (из cppworker /api/models callback
	// + llamaCppMetricsPoller). Используются в DecidePreflight для проверки
	// совпадения с requested* полями в RequestMeta.
	CurrentKvCacheType string // "f16"/"q8_0"/"q4_0" — "" = unknown

	CurrentNCtx     int // lastKnownNCtx из координатора
	MaxVRAMNCtx     int // из cppworker metrics (0 = unknown ИЛИ веса не влезают — см. VRAMKnown)
	ModelMaxContext int // из GGUF metadata (0 = unknown)
	// R83 §9.2 (2026-09-26): различает два смысла нуля в MaxVRAMNCtx.
	//
	// cppworker с R83 отдаёт top-level `vram_known` в /api/models (и per-model).
	// Раньше «0» означало и «метрик нет» (fail-open: проверку пропускаем), и
	// «VRAM известна, но веса не влезают» (reload бессмыслен). Теперь:
	//   VRAMKnown=false → данных нет, прежнее поведение;
	//   VRAMKnown=true  → VRAM измерена; MaxVRAMNCtx==0 означает «веса не влезают».
	VRAMKnown bool
	// AvailableVRAMMB — свободная VRAM бэкенда в MB (для текста отказа).
	AvailableVRAMMB uint64
	// R60.6 (2026-09-07): размер файла модели в байтах (из cppworker metrics).
	// Используется EstimateReloadTimeMs для расчёта Retry-After.
	// 0 = unknown (fallback на cfg.effectiveAsyncRetryAfter).
	ModelSizeBytes int64
	// === Round 37 (2026-08-18): auto-adapt n_ctx 3-tier state ===
	// Дополнительные поля для 3-tier resolution (profile / feasible / GGUF).
	// Заполняются из /api/models response (cppworker exposes since Round 37).
	// 0 = unknown (fallback to ModelMaxContext, который раньше назывался max_vram).
	//
	// 3-tier precedence в resolveModelMaxContext v2 (см. nctx_reload_handlers.go):
	//   1. profile.contextLengthAuto=false → profile (жёсткий cap)
	//   2. profile.contextLengthAuto=true  → min(profileMaxContext, MaxFeasibleContext)
	//   3. fallback → ModelMaxContext (старое поведение, обратная совместимость)
	MaxFeasibleContext int // min(VRAM, RAM, GGUF) для конкретной модели
	GGUFMaxContext     int // GGUF training context (hard upper bound, e.g. 262144)
	// === R68 (2026-09-23): profile — HINT, а не потолок для клиента ===
	//
	// PhysicalMaxContext — потолок, выше которого запрос клиента удовлетворить
	// НЕЛЬЗЯ даже reload'ом: min(GGUF max из метаданных модели, operator cap
	// contextLengthMax/AutoReloadMaxNCtx). profile.contextLength сюда НЕ входит:
	// по доктрине R43 это значение для ПЕРВИЧНОЙ загрузки (hint), а не предел,
	// который можно предъявлять клиенту.
	//
	// Зачем: жалоба R68 (Cline, Model Context Window = 65536). Профиль
	// gemma-4-E4B-it-Q4_K_M имеет contextLength=8192 (обход старого upstream
	// GGML_ASSERT) и не имеет contextLengthAuto → Tier 1 resolver возвращал
	// ModelMaxContext=8192, и preflight отвечал 413 «requested n_ctx=65536
	// exceeds model max context=8192», хотя GGUF держит 131072, а cppworker
	// сообщал max_vram_n_ctx=66125. Клиент получал невнятную ошибку вместо
	// выделения модели. 0 = unknown (fallback на ModelMaxContext).
	PhysicalMaxContext int
	// ProfileHintNCtx — contextLength из профиля (для честного текста отказа:
	// оператор должен видеть, что 8192 — это hint профиля, а не предел модели).
	ProfileHintNCtx int
	// AutoReloadMaxNCtx — operator cap (LB_NCTX_RELOAD_MAX_N_CTX).
	AutoReloadMaxNCtx    int
	CurrentFlashAttnType int // -1/0/1, 0 = unknown
	CurrentUseMmap       bool
	// CurrentContextPerSeq — R83-fix (2026-09-30): окно ОДНОГО слота
	// (n_ctx_seq у llama.cpp, `context_per_seq` в /api/models).
	//
	// ЗАЧЕМ. CurrentNCtx — СУММАРНОЕ окно модели, а один запрос обслуживает один
	// слот: при parallel=2 суммарные 65536 дают клиенту 32768. Проверка «промпт
	// помещается» по суммарному окну пропускала запрос, который слот не обслужит,
	// а проверка «нужно больше, чем есть» — не срабатывала, и балансер уходил в
	// ветку отказа «VRAM исчерпана / reload не поможет» вместо роста окна.
	// 0 = неизвестно (считаем, что окно слота равно суммарному).
	CurrentContextPerSeq int
	// CurrentSlots — сколько слотов заведено у загруженной модели (>=1).
	// Нужно, чтобы пересчитать требование «на клиента» в суммарный n_ctx для
	// перезагрузки: total = окно_клиента × слоты.
	CurrentSlots int
	// CurrentReasoningEnabled — R83-политика (2026-10-01): режим reasoning
	// ЗАГРУЖЕННОЙ модели (per-model `reasoning_enabled` из /api/models).
	//
	// nil = cppworker не сообщил (модель не загружена или старая версия) —
	// сравнивать не с чем, расхождение не объявляем.
	CurrentReasoningEnabled *bool
}

// reloadTargetForState — R83-fix (2026-09-30): целевое СУММАРНОЕ окно для
// перезагрузки.
//
// `perSeqTarget` — сколько нужно ОДНОМУ клиенту. Поскольку llama.cpp делит
// суммарное окно между слотами (n_ctx_seq = n_ctx / slots), для двух слотов
// нужно суммарно в два раза больше — иначе клиент снова получит половину и
// упрётся в то же ограничение (это и наблюдалось: gemma-4 при max_slots=2
// давала клиенту 32768 при суммарных 65536).
//
// R493-12h: выравнивание по 256 — ВСЕГДА, в том числе при одном слоте. См.
// perClientToTotal: без этого балансер просил и кэшировал невыровненное окно,
// которого у воркера не бывает.
func reloadTargetForState(perSeqTarget int, state *NCtxBackendState) int {
	if perSeqTarget <= 0 {
		return perSeqTarget
	}
	slots := state.effectiveSlots()
	aligned := cppbackend.PadContext(perSeqTarget)
	if slots <= 1 {
		return aligned
	}
	total := aligned * slots
	logger.Get().Infow(i18n.T("balancer.preflight_nctx.target_window_recalculated"),
		"backend_id", state.BackendID,
		"per_seq_target", perSeqTarget, "slots", slots, "total_target", total)
	return total
}

// effectivePerSeqNCtx — окно, доступное ОДНОМУ клиенту (0 → суммарное).
func (s *NCtxBackendState) effectivePerSeqNCtx() int {
	if s == nil {
		return 0
	}
	if s.CurrentContextPerSeq > 0 {
		return s.CurrentContextPerSeq
	}
	return s.CurrentNCtx
}

// effectiveSlots — число слотов модели (>=1).
func (s *NCtxBackendState) effectiveSlots() int {
	if s == nil || s.CurrentSlots < 1 {
		return 1
	}
	return s.CurrentSlots
}

// physicalCeiling — потолок, выше которого клиентский запрос удовлетворить
// нельзя (R68). Если PhysicalMaxContext не заполнен (старые вызовы, юнит-тесты
// с одним ModelMaxContext) — используется ModelMaxContext: прежнее поведение
// сохраняется.
//
// R265 (2026-10-28): это НЕ четвёртая реализация потолка, а геттер уже
// посчитанного значения. Сам расчёт один — resolvePhysicalMaxContext, а входы
// собирает collectPreflightState (profileCapCtx + GGUF + операторский потолок).
// Свести геттер с расчётом нельзя: он вызывается на глубоких путях решения, где
// повторный поход в конфиг/метрики менял бы картину посреди решения о reload
// (значения зафиксированы на момент сборки состояния).
func (s *NCtxBackendState) physicalCeiling() int {
	if s == nil {
		return 0
	}
	if s.PhysicalMaxContext > 0 {
		return s.PhysicalMaxContext
	}
	return s.ModelMaxContext
}
