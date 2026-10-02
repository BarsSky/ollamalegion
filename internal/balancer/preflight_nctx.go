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
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
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
	// EstimatedPromptTokens — оценка числа токенов в prompt (chars/4).
	EstimatedPromptTokens int
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
func reloadTargetForState(perSeqTarget int, state *NCtxBackendState) int {
	if perSeqTarget <= 0 {
		return perSeqTarget
	}
	slots := state.effectiveSlots()
	if slots <= 1 {
		return perSeqTarget
	}
	total := cppbackend.PadContext(perSeqTarget) * slots
	logger.Get().Infow("preflight: целевое окно пересчитано «на клиента → суммарно»",
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
func (s *NCtxBackendState) physicalCeiling() int {
	if s == nil {
		return 0
	}
	if s.PhysicalMaxContext > 0 {
		return s.PhysicalMaxContext
	}
	return s.ModelMaxContext
}

// growthWorthReload — R83-fix (2026-09-30): стоит ли вообще перезагружать
// модель ради большего окна.
//
// ЗАЧЕМ. Живой дефект на локальном стенде (gemma-4, RTX 3070 8 GB, клиент Cline
// с prompt ≈16k токенов и max_tokens=8192):
//
//  1. preflight сравнивает ОЦЕНКУ промпта (44 230 токенов, tokencount —
//     консервативная верхняя граница; реально в промпте 16 210) с окном слота
//     и решает растить окно;
//  2. cppworker выгружает модель и грузит заново;
//  3. адаптивная стратегия возвращает nCtx, РАВНЫЙ текущему окну, — и модель
//     всё равно перезагружается: 2–3 минуты простоя, 503/ожидание у клиента и
//     ни одного выигрыша (проверено: `new_ctx` в логе cppworker совпадал со
//     старым, менялось только число GPU-слоёв).
//
// Поэтому здесь проверяется единственное, что действительно делает reload
// бессмысленным: стратегия не может дать окно больше текущего.
//
// ЧЕГО ЗДЕСЬ СОЗНАТЕЛЬНО НЕТ. Проверки «стратегия предлагает меньше
// GPU-слоёв». Первая версия фикса отклоняла reload при stage=cpu_only
// (gpuLayers=0) — на живом стенде это оказалось неверно: `gpuLayers: 0` в
// reload-запросе cppworker трактует как «взять из профиля» (sync_profile.go:
// `else if *gpuLayers == 0 { *gpuLayers = profile.NumGPULayers }` → -1 = все
// слои → auto-раскладка по VRAM). Подтверждено логом: при стратегии cpu_only
// reload всё равно дал `new_gpu_layers:-1` → `gpuLayers=22` и
// `offloaded 22/43 layers` — то есть раскладка стала ЛУЧШЕ, а отклонение роста
// окна только лишило бы клиента нужного контекста. Явное понижение (например,
// 12 против 19) по-прежнему отсекается — но в DoReload, а не здесь
// (clampStrategyToCurrentLayout).
//
// Возвращает (worth, achievablePerClient, why):
//   - worth=true  → перезагрузка осмысленна (achievablePerClient не определён);
//   - worth=false → achievablePerClient — окно НА КЛИЕНТА, которое cppworker
//     реально сможет дать (0 = неизвестно), why — причина словами.
//
// worth=true при любой неопределённости (нет адреса, нет стратегии) — прежнее
// поведение сохраняется.
func (c *NCtxReloadCoordinator) growthWorthReload(
	backendAddr, backendID, modelName string,
	targetTotalNCtx, currentTotalNCtx, currentPerSeqNCtx, currentGPULayers, slots int,
) (bool, int, string) {
	if backendAddr == "" || modelName == "" {
		return true, 0, ""
	}
	hints := c.ReloadHintsFor(backendID, modelName)
	httpClient := &http.Client{Timeout: 3 * time.Second}
	strategy := queryAdaptiveStrategy(backendAddr, modelName, targetTotalNCtx, hints.KVCacheType, httpClient)
	if strategy == nil {
		// Стратегии нет (cppworker не ответил/404) — не берём на себя
		// решение: прежнее поведение (reload).
		return true, 0, ""
	}
	if strategy.Stage == "cpu_only" {
		// Диагностика для оператора: сама по себе не отменяет рост окна
		// (см. комментарий выше), но объясняет, почему раскладка может
		// измениться.
		logger.Get().Warnw("preflight: стратегия предлагает cpu_only — окно растим, "+
			"раскладку решает cppworker (gpuLayers=0 для него = «из профиля»)",
			"backend_id", backendID, "model", modelName,
			"strategy_gpu_layers", strategy.GPULayers,
			"current_gpu_layers", currentGPULayers,
			"strategy_n_ctx", strategy.NCtx)
	}

	// Окно не увеличится. Проверяем только когда reload затевался РАДИ роста:
	// если target <= current, это не growth-перезагрузка (другая причина), и
	// решать за неё мы не имеем права.
	if targetTotalNCtx > currentTotalNCtx && strategy.NCtx > 0 && currentTotalNCtx > 0 &&
		strategy.NCtx <= currentTotalNCtx {
		// strategy.NCtx — СУММАРНОЕ окно: клиенту достанется его доля по слотам.
		achievablePerClient := strategy.NCtx
		if slots > 1 {
			achievablePerClient = strategy.NCtx / slots
		}
		return false, achievablePerClient, fmt.Sprintf(
			"стратегия даёт суммарное окно %d при текущем %d (на клиента %d): "+
				"reload не изменил бы окно, но выгрузил бы модель на 1–3 минуты",
			strategy.NCtx, currentTotalNCtx, currentPerSeqNCtx)
	}

	return true, 0, ""
}

// DecidePreflight решает, что делать с запросом ДО отправки в cppworker.
//
// R83-политика (2026-10-01): решение принимается ТОЛЬКО по тому, что клиент
// ЯВНО указал, — а не по нашей оценке длины промпта.
//
// Почему так. Раньше окно «росло» по оценке промпта (tokencount — консервативная
// верхняя граница, завышающая в 2–4 раза): обычный запрос Cline с 16k-промптом
// давал оценку 44 230, окно слота 32 768 объявлялось малым, и модель уходила в
// перезагрузку на 2–3 минуты — иногда с тем же самым окном на выходе. Загрузка
// «по оценке» — это и есть принудительный элемент, который убирается: если
// клиент не сказал, какое окно ему нужно, мы не гадаем.
//
// Порядок решений:
//
//  1. Расхождение СЕМАНТИЧЕСКИХ параметров (reasoning/thinking) → отказ 409 с
//     перечислением отличий. Тихая подмена поведения недопустима: клиент просит
//     «думать», а модель загружена без reasoning — это другой ответ.
//  2. Клиент не указал num_ctx → NoOp. Никаких перезагрузок «на всякий случай»;
//     если история действительно не влезает, cppworker вернёт свою ошибку, а
//     балансер передаст её объяснение клиенту.
//  3. Модель ещё не загружена → NoOp: cppworker загрузит её С ОКНОМ КЛИЕНТА
//     (ensureModelLoaded принимает num_ctx запроса), а не с дефолтом профиля.
//  4. Загруженное окно на клиента >= запрошенного → NoOp. Загружено больше —
//     перезагружать под клиента не нужно и уведомлять не о чем.
//  5. Загруженное окно меньше → перезагрузка модели под окно клиента (остальные
//     параметры совпадают, поэтому это безопасно). Если запрошенное окно не
//     влезает в физический потолок (GGUF/операторский cap) — отказ с понятным
//     текстом: «столько не поместится, вот почему и что делать».
func DecidePreflight(meta *RequestMeta, state *NCtxBackendState, cfg NCtxReloadConfig) *PreflightResult {
	if meta == nil || state == nil {
		return &PreflightResult{Decision: PreflightNoOp}
	}
	logger.Get().Infow("preflight: DecidePreflight inputs",
		"backend_id", state.BackendID,
		"model", meta.ModelName,
		"body_n_ctx", meta.RequestedNCtxOverride,
		"loaded_n_ctx", state.CurrentNCtx,
		"context_per_seq", state.CurrentContextPerSeq,
		"slots", state.effectiveSlots(),
		"model_max_n_ctx", state.ModelMaxContext,
		"max_vram_n_ctx", state.MaxVRAMNCtx,
		"estimated_prompt_tokens", meta.EstimatedPromptTokens,
		"n_predict", meta.RequestedNPredict,
		"has_tools", meta.HasTools,
		"requested_think", reqStr(meta.RequestedThink),
		"loaded_reasoning", reqStr(state.CurrentReasoningEnabled))

	// (1) Семантические параметры: клиент требует то, чего у загруженной модели
	// нет. Перезагрузкой это НЕ лечим (пользователь просил именно ошибку), и
	// молчать нельзя — ответ был бы другим по смыслу.
	if diffs := semanticParamDiffs(meta, state); len(diffs) > 0 {
		logger.Get().Warnw("preflight: отказ — параметры загруженной модели не совпадают с требованием запроса",
			"backend_id", state.BackendID,
			"model", meta.ModelName,
			"differences", len(diffs),
			"detail", diffs[0].Explain)
		return makeParamMismatchReject(state, meta, diffs)
	}

	reqWindow := meta.RequestedNCtxOverride

	// (2) Клиент не сообщил окно — не гадаем и не перезагружаем.
	if reqWindow <= 0 {
		logger.Get().Infow("preflight: клиент не указал num_ctx — модель не перезагружаем",
			"backend_id", state.BackendID,
			"model", meta.ModelName,
			"loaded_n_ctx", state.CurrentNCtx,
			"estimated_prompt_tokens", meta.EstimatedPromptTokens)
		return &PreflightResult{Decision: PreflightNoOp}
	}

	// (3) Модель не загружена — грузим её окном клиента. Решение о загрузке
	// принимает cppworker (ensureModelLoaded получает num_ctx запроса), поэтому
	// здесь именно NoOp: «проксируй запрос как есть».
	if state.effectivePerSeqNCtx() <= 0 && state.CurrentNCtx <= 0 {
		logger.Get().Infow("preflight: модель не загружена — грузим окном клиента",
			"backend_id", state.BackendID,
			"model", meta.ModelName,
			"requested_n_ctx", reqWindow)
		return &PreflightResult{Decision: PreflightNoOp}
	}

	// (4) Загруженное окно на клиента уже не меньше запрошенного → NoOp.
	perSeq := state.effectivePerSeqNCtx()
	if perSeq > 0 && reqWindow <= perSeq {
		logger.Get().Infow("preflight: загруженное окно не меньше запрошенного — reload не нужен",
			"backend_id", state.BackendID,
			"model", meta.ModelName,
			"requested_n_ctx", reqWindow,
			"context_per_seq", perSeq,
			"loaded_n_ctx", state.CurrentNCtx)
		return &PreflightResult{Decision: PreflightNoOp}
	}
	if perSeq <= 0 && state.CurrentNCtx > 0 && reqWindow <= state.CurrentNCtx {
		// Окно слота неизвестно (старые метрики) — сравниваем с суммарным.
		return &PreflightResult{Decision: PreflightNoOp}
	}

	// (5) Нужен рост окна под клиента. Проверяем только физику: поместится ли
	// запрошенное окно вообще (GGUF-потолок модели и операторский cap). Если нет
	// — понятный отказ вместо перезагрузки «в никуда».
	slots := state.effectiveSlots()
	targetTotal := perClientToTotal(reqWindow, slots)
	if ceil := state.physicalCeiling(); ceil > 0 && targetTotal > ceil {
		logger.Get().Warnw("preflight: запрошенное окно больше физического потолка — отказ",
			"backend_id", state.BackendID,
			"model", meta.ModelName,
			"requested_per_client", reqWindow,
			"requested_total", targetTotal,
			"slots", slots,
			"physical_ceiling", ceil)
		return makeWindowTooBigReject(state, meta, reqWindow, targetTotal, ceil,
			fmt.Sprintf("модель поддерживает не более %d токенов контекста", ceil))
	}
	if cfg.AutoReloadMaxNCtx > 0 && targetTotal > cfg.AutoReloadMaxNCtx {
		logger.Get().Warnw("preflight: запрошенное окно больше операторского потолка — отказ",
			"backend_id", state.BackendID,
			"model", meta.ModelName,
			"requested_total", targetTotal,
			"auto_reload_max_n_ctx", cfg.AutoReloadMaxNCtx)
		return makeWindowTooBigReject(state, meta, reqWindow, targetTotal, cfg.AutoReloadMaxNCtx,
			fmt.Sprintf("в настройках балансера задан потолок авто-перезагрузки %d токенов (LB_NCTX_RELOAD_MAX_N_CTX)",
				cfg.AutoReloadMaxNCtx))
	}

	logger.Get().Infow("preflight: загруженное окно меньше запрошенного — перезагружаем под клиента",
		"backend_id", state.BackendID,
		"model", meta.ModelName,
		"requested_per_client", reqWindow,
		"requested_total", targetTotal,
		"context_per_seq", perSeq,
		"loaded_n_ctx", state.CurrentNCtx,
		"slots", slots)
	// TargetNCtx — окно НА КЛИЕНТА: RunPreflight сам пересчитает его в суммарное
	// (total = окно × слоты), чтобы не умножить дважды.
	return &PreflightResult{
		Decision:   PreflightReload,
		TargetNCtx: reqWindow,
	}
}

// perClientToTotal — пересчёт «окно на клиента» → «суммарное окно модели».
//
// llama.cpp при kv_unified=false делит суммарное окно между слотами:
// n_ctx_seq = PAD(n_ctx / n_seq_max, 256). Значит для N клиентов нужно
// суммарно ≈ окно × N (с выравниванием вверх до 256).
func perClientToTotal(perSeq, slots int) int {
	if perSeq <= 0 {
		return perSeq
	}
	if slots <= 1 {
		return perSeq
	}
	return cppbackend.PadContext(perSeq) * slots
}

// ParamDiff — расхождение между требованием клиента и загруженной моделью.
//
// Поля намеренно человекочитаемые: этот объект уходит КЛИЕНТУ, а не только в
// лог, поэтому «param X + param Y = error» здесь недопустим.
type ParamDiff struct {
	// Parameter — машинное имя ("reasoning").
	Parameter string `json:"parameter"`
	// Loaded / Requested — как есть и как требует клиент, словами.
	Loaded    string `json:"loaded"`
	Requested string `json:"requested"`
	// Explain — одна понятная фраза, что именно не совпало.
	Explain string `json:"explain"`
}

// semanticParamDiffs — расхождения по параметрам, которые МЕНЯЮТ СМЫСЛ ответа.
//
// Сюда попадают только те параметры, из-за которых ответ будет другим, а не
// просто медленнее: сейчас это reasoning/thinking. Технические параметры
// (kv_cache_type, flash_attn, use_mmap) сознательно НЕ сравниваются: они влияют
// на скорость и память, но не на смысл ответа, и отказ из-за них ломал бы
// работу там, где всё в порядке.
func semanticParamDiffs(meta *RequestMeta, state *NCtxBackendState) []ParamDiff {
	if meta == nil || state == nil {
		return nil
	}
	if meta.RequestedThink == nil || state.CurrentReasoningEnabled == nil {
		return nil
	}
	if *meta.RequestedThink == *state.CurrentReasoningEnabled {
		return nil
	}
	want := "включено"
	have := "выключено"
	if *state.CurrentReasoningEnabled {
		have = "включено"
	}
	if !*meta.RequestedThink {
		want = "выключено"
	}
	explain := fmt.Sprintf(
		"Запрос требует %s размышление (reasoning), а модель %s уже загружена с %s.",
		want, meta.ModelName, have)
	return []ParamDiff{{
		Parameter: "reasoning",
		Loaded:    have,
		Requested: want,
		Explain:   explain,
	}}
}

// makeParamMismatchReject — HTTP 409 с перечислением расхождений.
//
// Текст пишется для человека: что загружено, что просит клиент, что делать.
// Балансер здесь выступает как прокси и обязан объяснить причину, а не выдать
// «requested param mismatch» или набор чисел.
func makeParamMismatchReject(state *NCtxBackendState, meta *RequestMeta, diffs []ParamDiff) *PreflightResult {
	parts := make([]string, 0, len(diffs))
	for _, d := range diffs {
		parts = append(parts, fmt.Sprintf("%s: загружено «%s», запрос требует «%s»", d.Parameter, d.Loaded, d.Requested))
	}
	loaded := map[string]interface{}{
		"context_per_seq":   state.effectivePerSeqNCtx(),
		"context_size":      state.CurrentNCtx,
		"slots":             state.effectiveSlots(),
		"reasoning_enabled": reqStr(state.CurrentReasoningEnabled),
		"kv_cache_type":     state.CurrentKvCacheType,
	}
	requested := map[string]interface{}{
		"num_ctx": meta.RequestedNCtxOverride,
		"think":   reqStr(meta.RequestedThink),
	}
	body := map[string]interface{}{
		"error": fmt.Sprintf(
			"Модель %s загружена с другими параметрами, чем требует запрос: %s. "+
				"Перезагрузка под этот запрос не выполняется — измените параметр в клиенте "+
				"или перезагрузите модель с нужными настройками в WebUI.",
			meta.ModelName, strings.Join(parts, "; ")),
		"reason":            strings.Join(parts, "; "),
		"param_differences": diffs,
		"loaded":            loaded,
		"requested":         requested,
		"what_to_do": "Измените параметр в клиенте (например, уберите think=true) — либо " +
			"перезагрузите модель с этим параметром: WebUI → Настройки → llama.cpp → профиль " +
			"модели → Enable reasoning → «Применить (reload)».",
		"backend_id": state.BackendID,
		"model":      meta.ModelName,
	}
	encoded, _ := json.Marshal(body)
	return &PreflightResult{
		Decision:     PreflightReject,
		RejectStatus: http.StatusConflict,
		RejectBody:   string(encoded),
	}
}

// makeWindowTooBigReject — HTTP 413: запрошенное окно физически не поместится.
//
// Единственный случай, когда о «маленьком окне» нужно уведомлять (по требованию
// пользователя): остальные расхождения лечатся перезагрузкой, а это — нет.
func makeWindowTooBigReject(
	state *NCtxBackendState, meta *RequestMeta,
	requestedPerClient, requestedTotal, ceiling int, because string,
) *PreflightResult {
	slots := state.effectiveSlots()
	perClientCeil := ceiling
	if slots > 1 {
		perClientCeil = ceiling / slots
	}
	msg := fmt.Sprintf(
		"Запрошенное контекстное окно %d токенов на клиента не поместится: %s. "+
			"При %d параллельных слотах суммарное окно %d делится между клиентами, "+
			"поэтому одному клиенту доступно не больше %d токенов.",
		requestedPerClient, because, slots, ceiling, perClientCeil)
	body := map[string]interface{}{
		"error":                 msg,
		"reason":                fmt.Sprintf("требуется %d (суммарно %d), доступно не более %d на клиента", requestedPerClient, requestedTotal, perClientCeil),
		"requested_n_ctx":       requestedPerClient,
		"requested_total_n_ctx": requestedTotal,
		"max_per_client_n_ctx":  perClientCeil,
		"backend_ceiling_n_ctx": ceiling,
		"slots":                 slots,
		"loaded_n_ctx":          state.CurrentNCtx,
		"context_per_seq":       state.effectivePerSeqNCtx(),
		"gguf_max_context":      state.GGUFMaxContext,
		"auto_reload_max_n_ctx": state.AutoReloadMaxNCtx,
		"what_to_do": fmt.Sprintf(
			"Уменьшите окно в клиенте (до %d или меньше), либо поставьте параллельные слоты в 1 "+
				"(тогда всё окно достанется одному клиенту), либо поднимите потолок "+
				"(профиль модели → contextLengthMax / LB_NCTX_RELOAD_MAX_N_CTX).",
			perClientCeil),
		"backend_id": state.BackendID,
		"model":      meta.ModelName,
	}
	encoded, _ := json.Marshal(body)
	return &PreflightResult{
		Decision:     PreflightReject,
		RejectStatus: http.StatusRequestEntityTooLarge,
		RejectBody:   string(encoded),
	}
}

func preflightDecisionFromCfg(cfg NCtxReloadConfig) PreflightDecision {
	if cfg.PreflightAsyncReload {
		return PreflightAsyncReload
	}
	return PreflightReload
}

// nctxPreflightWaitDefaultSec — исторический дефолт 240 c (R68). Оставлен как
// документация: R83/v67 убрал его из поведения — ждать нужно терминального
// состояния reload, а не 240 секунд по часам.
const nctxPreflightWaitDefaultSec = 240

// nctxPreflightWaitUnlimited — «ждать терминального состояния».
//
// Отрицательное значение выбрано намеренно: 0 в этой функции означает
// «не ждать вовсе» (сразу 503), а WaitReloadDone(…, 0) — «ждать завершения».
// Caller (RunPreflight) переводит -1 в 0 для WaitReloadDone.
const nctxPreflightWaitUnlimited = -1 * time.Second

// nctxPreflightWaitTimeout — сколько балансер ждёт завершения async reload,
// прежде чем отдать клиенту 503.
//
// R83/v67 (2026-10-02): по умолчанию — ЖДАТЬ ТЕРМИНАЛЬНОГО СОСТОЯНИЯ.
//
//	LB_NCTX_PREFLIGHT_WAIT_SEC не задан → -1: ждать, пока reload не завершится
//	    (успех или ошибка) либо пока клиент не отменит запрос. Раньше здесь
//	    стояли 240 секунд, и на A10 с 15.7 ГБ моделью reload не укладывался —
//	    первый же запрос клиента получал 503 «being reloaded», хотя перезагрузка
//	    шла нормально (жалоба «проходит только повторный запрос»).
//	N > 0 → N секунд: осознанный кап оператора (взведение видно в логе).
//	0 → не ждать вовсе (прежнее поведение: сразу 503 + Retry-After).
//	отрицательное → -1 (то же, что «не задан»): ждать терминального состояния.
//	нечисловое значение → -1 (ошибка конфига не должна обрывать ожидание).
func nctxPreflightWaitTimeout() time.Duration {
	v := strings.TrimSpace(os.Getenv("LB_NCTX_PREFLIGHT_WAIT_SEC"))
	if v == "" {
		return nctxPreflightWaitUnlimited
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		logger.Get().Warnw("LB_NCTX_PREFLIGHT_WAIT_SEC не распознан — ждём терминального состояния reload",
			"value", v, "error", err)
		return nctxPreflightWaitUnlimited
	}
	switch {
	case n == 0:
		return 0
	case n < 0:
		return nctxPreflightWaitUnlimited
	default:
		logger.Get().Warnw("armed preflight wait cap (operator opt-in): первый запрос клиента "+
			"получит 503, если reload не успеет за этот срок",
			"wait_sec", n, "source", "LB_NCTX_PREFLIGHT_WAIT_SEC")
		return time.Duration(n) * time.Second
	}
}

// paramsMatch — Round 34 (2026-08-12) Phase 2 helper для флаговой// profile mismatch detection. УДАЛЁН в Round 53.2 (2026-08-24) — флаговая
// логика признана избыточной. Reload теперь ТОЛЬКО по n_ctx.
//
// Поля остались в RequestMeta (RequestedKvCacheType, RequestedFlashAttnType,
// RequestedUseMmap) для совместимости с parser — они парсятся из request body
// но НЕ влияют на решение о reload. Backend обрабатывает их per-request
// (cppworker применяет kv_cache_type/flash_attn к контексту при inference).
//
// State поля (CurrentKvCacheType, CurrentFlashAttnType, CurrentUseMmap) тоже
// остаются — они нужны для отображения в WebUI (/api/v1/backends.loadedModels).

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
//
// R68 (2026-09-23): тело дополнено честными цифрами — profile hint, GGUF max,
// operator cap и физический потолок. Раньше в поле model_max_context попадал
// contextLength профиля (hint для первичной загрузки), и оператор видел
// «exceeds model max context=8192» для модели, которая держит 131072.
func makePreflightReject(state *NCtxBackendState, required int, reason string) *PreflightResult {
	return makePreflightRejectWithSuggestion(state, required, reason, "")
}

// makePreflightRejectWithSuggestion — то же, но с заменой поля suggestion.
// Нужно там, где общий совет («увеличьте контекст в клиенте») неверен: например,
// когда VRAM известна и веса модели в неё не влезают ни при каком gpu_layers —
// клиенту не поможет ничего, кроме меньшей модели/кванта.
func makePreflightRejectWithSuggestion(state *NCtxBackendState, required int, reason, suggestion string) *PreflightResult {
	if suggestion == "" {
		suggestion = "Increase the client's context window request, or raise contextLengthMax/LB_NCTX_RELOAD_MAX_N_CTX; a model profile's contextLength is only the initial-load hint"
	}
	body := map[string]interface{}{
		"error":                 "preflight: prompt + n_predict exceeds n_ctx for this backend",
		"reason":                reason,
		"required_n_ctx":        required,
		"current_n_ctx":         state.CurrentNCtx,
		"model_max_context":     state.ModelMaxContext,
		"physical_max_context":  state.physicalCeiling(),
		"profile_hint_n_ctx":    state.ProfileHintNCtx,
		"gguf_max_context":      state.GGUFMaxContext,
		"auto_reload_max_n_ctx": state.AutoReloadMaxNCtx,
		"max_vram_n_ctx":        state.MaxVRAMNCtx,
		"vram_known":            state.VRAMKnown,
		"backend_id":            state.BackendID,
		"suggestion":            suggestion,
		"profile_endpoint":      "/api/v1/cppworker/model-profiles",
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
			NewNCtx:  reloadTargetForState(decision.TargetNCtx, state),
			Reason: fmt.Sprintf("preflight: target=%d (current=%d, context_per_seq=%d, slots=%d, max_vram=%d, model_max=%d)",
				reloadTargetForState(decision.TargetNCtx, state),
				state.CurrentNCtx, state.CurrentContextPerSeq, state.effectiveSlots(),
				state.MaxVRAMNCtx, state.ModelMaxContext),
		}

		// R83-fix (2026-09-30): ПРОВЕРКА ДОСТИЖИМОСТИ. Не перезагружаем модель,
		// если рост окна недостижим или достигается ценой потери GPU-слоёв —
		// подробности в growthWorthReload. Без этой проверки живой стенд уходил
		// в 2–3-минутную выгрузку/загрузку с тем же самым окном.
		gateModelName := ""
		if meta != nil {
			gateModelName = meta.ModelName
		}
		worth, achievablePerClient, why := c.growthWorthReload(backendAddr, backendID, gateModelName,
			plan.NewNCtx, state.CurrentNCtx, state.effectivePerSeqNCtx(),
			c.ReloadHintsFor(backendID, gateModelName).GPULayers, state.effectiveSlots())
		if !worth {
			reqWindow := 0
			if meta != nil {
				reqWindow = meta.RequestedNCtxOverride
			}
			// R83-политика (2026-10-01): клиент ЯВНО просит окно, которое
			// cppworker дать не может → это тот самый случай, о котором надо
			// уведомить понятным текстом (а не молча обслужить в меньшем окне).
			if reqWindow > 0 && achievablePerClient > 0 && reqWindow > achievablePerClient {
				logger.Get().Warnw("preflight: запрошенное окно недостижимо — отказ с объяснением",
					"backend_id", backendID,
					"model", gateModelName,
					"requested_per_client", reqWindow,
					"achievable_per_client", achievablePerClient,
					"reason", why)
				return makeWindowTooBigReject(state, meta, reqWindow, plan.NewNCtx,
					achievablePerClient*state.effectiveSlots(),
					fmt.Sprintf("cppworker может дать не больше %d токенов на клиента при %d слотах",
						achievablePerClient, state.effectiveSlots())), nil
			}
			logger.Get().Warnw("preflight: рост окна пропущен — reload не дал бы выигрыша",
				"backend_id", backendID,
				"model", gateModelName,
				"target_n_ctx", plan.NewNCtx,
				"current_n_ctx", state.CurrentNCtx,
				"context_per_seq", state.effectivePerSeqNCtx(),
				"slots", state.effectiveSlots(),
				"reason", why,
				"action", "запрос обслуживается в текущем окне; cppworker ответит честным "+
					"413, если prompt действительно не влезает")
			return &PreflightResult{Decision: PreflightNoOp, TargetNCtx: state.CurrentNCtx}, nil
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

			// R60.6 (2026-09-07): рассчитываем estimatedRetryAfter на основе
			// (model_size + target_n_ctx) формулы, mirror cppworker's
			// estimateLoadTimeMs. Это даст клиенту адекватное время retry
			// (60-180s для 5GB+131072 n_ctx на RTX 3070 8GB) вместо hardcoded
			// 30s. Cppworker's actual estimatedLoadTimeMs появится только
			// после успешного load (200 body), а нам нужна оценка ДО запуска reload.
			//
			// Coordinator doesn't have direct access to Proxy's metrics manager.
			// Caller (preflight_helper.go) reads model size and passes via state.
			// For now we use 0 (size unknown) — caller can override EstimatedRetryAfter
			// after RunPreflight returns. Future: extend state to carry SizeBytes.
			var modelSizeBytes int64
			if state != nil && state.ModelSizeBytes > 0 {
				modelSizeBytes = state.ModelSizeBytes
			}
			estimatedMs := EstimateReloadTimeMs(modelSizeBytes, decision.TargetNCtx)
			// R60.6: clamp по operator config (default 120s). 0 = use cfg default.
			estimatedRetryAfter := cfg.effectiveAsyncRetryAfter()
			if estimatedMs > 0 {
				estSec := int((estimatedMs + 999) / 1000) // round up to seconds
				if estSec > estimatedRetryAfter {
					estimatedRetryAfter = estSec
				}
				maxRA := cfg.PreflightAsyncRetryAfterMaxSec
				if maxRA <= 0 {
					maxRA = 120
				}
				if estimatedRetryAfter > maxRA {
					estimatedRetryAfter = maxRA
				}
			}

			logger.Get().Infow("preflight: triggering ASYNC reload (Round 31 #2, R60.6)",
				"backend_id", backendID,
				"current_n_ctx", state.CurrentNCtx,
				"target_n_ctx", decision.TargetNCtx,
				"max_vram_n_ctx", state.MaxVRAMNCtx,
				"model_max_context", state.ModelMaxContext,
				"model_size_bytes", modelSizeBytes,
				"estimated_load_time_ms", estimatedMs,
				"has_tools", meta != nil && meta.HasTools,
				"retry_after_sec", estimatedRetryAfter)

			// Async reload — НЕ ждём завершения. DoReload имеет внутренний
			// singleflight-like lock, так что параллельные reload'ы коалесцируются.
			//
			// R53.4 (2026-08-24) HOTFIX: defer recover() защищает balancer от
			// crash при любом nil pointer в goroutine body. Без этого — nil deref
			// (например, в c.state() или DoReload internal path) валит весь
			// balancer → in-flight Cline клиенты получают "Did not receive done".
			// Корень: cascading reload requests (Cline retries пока cppworker
			// busy) → multiple async goroutines + race conditions.
			//
			// R60.6 fix: регистрируем reload в reloadDedupRegistry ДО старта
			// goroutine, чтобы subsequent preflight calls видели IsReloadPending=true
			// и не триггерили ещё один reload (double-trigger cascade).
			if c.reloadDedup != nil {
				_, startedNew := c.reloadDedup.StartReloadIfNotPending(backendID, modelName, decision.TargetNCtx, func(entry *reloadEntry) {
					defer func() {
						if r := recover(); r != nil {
							logger.Get().Errorw("preflight: PANIC in async reload goroutine — RECOVERED",
								"backend_id", backendID, "panic", r)
						}
					}()
					reloadCtx, cancel := reloadTimeoutContext(context.Background(), cfg.effectiveTimeout())
					defer cancel()
					if err := c.DoReload(reloadCtx, backendID, backendAddr, modelName, plan, loader); err != nil {
						logger.Get().Errorw("preflight: async reload failed",
							"backend_id", backendID, "error", err)
						entry.err = err
						return
					}
					// Reload успешен — обновляем lastKnownNCtx.
					c.SetLastKnownNCtx(backendID, decision.TargetNCtx)
					logger.Get().Infow("preflight: async reload succeeded",
						"backend_id", backendID, "new_n_ctx", decision.TargetNCtx)
					c.ResetCycleCounter(backendID)
				})
				if !startedNew {
					// R60.6: reload уже в процессе для этой модели — возвращаем
					// PreflightAsyncReload БЕЗ новой goroutine (dedup hits).
					logger.Get().Infow("preflight: reload already pending, returning 503 (dedup)",
						"backend_id", backendID, "model", modelName,
						"target_n_ctx", decision.TargetNCtx)
				}
			} else {
				// fallback: no dedup registry, fire-and-forget как раньше
				go func() {
					defer func() {
						if r := recover(); r != nil {
							logger.Get().Errorw("preflight: PANIC in async reload goroutine — RECOVERED",
								"backend_id", backendID, "panic", r)
						}
					}()
					reloadCtx, cancel := reloadTimeoutContext(context.Background(), cfg.effectiveTimeout())
					defer cancel()
					if err := c.DoReload(reloadCtx, backendID, backendAddr, modelName, plan, loader); err != nil {
						logger.Get().Errorw("preflight: async reload failed",
							"backend_id", backendID, "error", err)
						return
					}
					c.SetLastKnownNCtx(backendID, decision.TargetNCtx)
					logger.Get().Infow("preflight: async reload succeeded",
						"backend_id", backendID, "new_n_ctx", decision.TargetNCtx)
					c.ResetCycleCounter(backendID)
				}()
			}

			// R68 (2026-09-23): дожидаемся reload'а и обслуживаем ПЕРВЫЙ же
			// запрос клиента (как R67a делает для авто-загрузки). Reload уже
			// зарегистрирован в dedup-реестре выше, поэтому WaitReloadDone
			// дождётся именно его (или уже завершённого — тогда nil).
			//
			// Раньше: клиенту сразу уходил 503+Retry-After (non-stream) или
			// stream-dialog с keepalive и финальным [DONE] БЕЗ ответа модели —
			// Cline показывал пустой ответ, и проходил только повторный запрос.
			// R83/v67 (2026-10-02): wait != 0 → ждём. Значение < 0 означает
			// «терминальное состояние» (переводим в 0 — так это понимает
			// WaitReloadDone: 0 = ждать до done, без таймера).
			if wait := nctxPreflightWaitTimeout(); wait != 0 {
				waitArg := wait
				if waitArg < 0 {
					waitArg = 0
				}
				waitStart := time.Now()
				if waitErr := c.WaitReloadDone(backendID, modelName, waitArg); waitErr == nil {
					c.SetLastKnownNCtx(backendID, decision.TargetNCtx)
					c.ResetCycleCounter(backendID)
					logger.Get().Infow("preflight: async reload finished while client waited, serving request",
						"backend_id", backendID, "model", modelName,
						"new_n_ctx", decision.TargetNCtx,
						"waited_ms", time.Since(waitStart).Milliseconds(),
						"wait_max_sec", int(wait.Seconds()))
					// Тот же контракт, что у sync reload: caller продолжает
					// проксирование запроса (round-trip один).
					return &PreflightResult{Decision: PreflightReload, TargetNCtx: decision.TargetNCtx}, nil
				} else {
					logger.Get().Warnw("preflight: wait for async reload did not succeed, returning 503",
						"backend_id", backendID, "model", modelName,
						"target_n_ctx", decision.TargetNCtx,
						"waited_ms", time.Since(waitStart).Milliseconds(),
						"error", waitErr)
				}
			}

			return &PreflightResult{
				Decision:            PreflightAsyncReload,
				TargetNCtx:          decision.TargetNCtx,
				EstimatedRetryAfter: estimatedRetryAfter,
			}, nil
		}

		// Sync mode (default, обратно совместимо со старым поведением).
		logger.Get().Infow("preflight: triggering reload",
			"backend_id", backendID,
			"current_n_ctx", state.CurrentNCtx,
			"target_n_ctx", decision.TargetNCtx,
			"max_vram_n_ctx", state.MaxVRAMNCtx,
			"model_max_context", state.ModelMaxContext,
			"has_tools", meta != nil && meta.HasTools)

		// R53.4 (2026-08-24) HOTFIX: sync reload path also can panic from
		// nil deref in c.state() or DoReload internal path. Defer recover
		// returns reject result instead of crashing the whole balancer.
		defer func() {
			if r := recover(); r != nil {
				logger.Get().Errorw("preflight: PANIC in sync reload — RECOVERED, returning reject",
					"backend_id", backendID, "panic", r)
			}
		}()

		reloadCtx, cancel := reloadTimeoutContext(ctx, cfg.effectiveTimeout())
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

// R60.6 (2026-09-07): EstimateReloadTimeMs — оценочное время reload в мс.
// Mirrors cppworker cmd/cppworker/handlers_model_async.go:estimateLoadTimeMs
// (тот же formula, тот же fallback). Используется как fallback когда
// cppworker 200 body с estimatedLoadTimeMs недоступен (e.g. модель ещё
// не загружена и metricsMgr не имеет size info).
//
// Формула: max(1000, size_MB / 100 + ctx/4K*500 + 2000) мс
// - base 1000ms minimum (модель не может грузиться мгновенно)
// - size/speed 100MB/s fallback для cold load
// - ctxInit 500ms per 4K tokens (KV-cache init, q4_0/q8_0 typical)
// - overhead 2000ms (mmap, metadata parse, locks)
func EstimateReloadTimeMs(modelSizeBytes int64, targetNCtx int) int64 {
	const baseSpeed = 100 * 1024 * 1024 // 100 MB/s fallback
	const baseMinMs = 1000
	const overheadMs = 2000
	const ctxInitMsPer4K = 500
	if modelSizeBytes <= 0 && targetNCtx <= 0 {
		return 0 // unknown, caller должен fall back на cfg default
	}
	var baseMs int64
	if modelSizeBytes > 0 {
		baseMs = modelSizeBytes * 1000 / baseSpeed
		if baseMs < baseMinMs {
			baseMs = baseMinMs
		}
	} else {
		baseMs = baseMinMs
	}
	var ctxMs int64
	if targetNCtx > 0 {
		blocksOf4K := int64((targetNCtx + 4095) / 4096)
		ctxMs = blocksOf4K * ctxInitMsPer4K
	}
	return baseMs + ctxMs + overheadMs
}
