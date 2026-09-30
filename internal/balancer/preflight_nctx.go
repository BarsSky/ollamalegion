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
	RequestedFlashAttnType int   // -1/0/1, 0 = не задано
	RequestedUseMmap       *bool // nil = не задано
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
		// max_tokens — без этого preflight считал n_predict=0 (дефолт 2048) и
		// недооценивал требуемый контекст.
		if meta.RequestedNPredict <= 0 && req.MaxOutputTokens != nil {
			meta.RequestedNPredict = *req.MaxOutputTokens
		}
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
		// R60.55 (2026-09-13): top-level max_tokens имеет приоритет над options.num_predict.
		meta.RequestedNPredict = req.Options.NumPredict
		if meta.RequestedNPredict <= 0 {
			meta.RequestedNPredict = req.MaxTokens
		}
		if meta.RequestedNPredict <= 0 && req.MaxOutputTokens != nil {
			meta.RequestedNPredict = *req.MaxOutputTokens
		}
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
		// R69 (2026-09-23): max_output_tokens (Cline) — алиас max_tokens.
		if meta.RequestedNPredict <= 0 && req.MaxOutputTokens != nil {
			meta.RequestedNPredict = *req.MaxOutputTokens
		}
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
// ЗАЧЕМ. Живой дефект на локальном стенде (gemma-4, 65536 суммарно / 2 слота,
// RTX 3070 8 GB, клиент Cline с prompt ≈16k токенов и max_tokens=8192):
//
//  1. preflight сравнивает ОЦЕНКУ промпта (44 230 токенов) с окном слота
//     (32 768) и решает растить окно;
//  2. cppworker выгружает модель и грузит заново;
//  3. адаптивная стратегия, спрошенная ДО выгрузки, считает бюджет по СВОБОДНОЙ
//     VRAM — то есть по памяти, которую занимает сама перезагружаемая модель,
//     — и возвращает stage=cpu_only, gpuLayers=0, nCtx=65536;
//  4. перезагрузка идёт с contextSize=65536, то есть ровно с текущим окном,
//     но без части GPU-слоёв.
//
// Итог: 2–3 минуты простоя, 503/ожидание у клиента и НИ ОДНОГО выигрыша —
// окно не изменилось. Поэтому перед перезагрузкой спрашиваем у cppworker ту же
// стратегию, что использует DoReload, и отказываемся от reload'а, если:
//
//   - окно не вырастет (strategy.NCtx <= текущего суммарного), или
//   - стратегия понижает GPU-оффлоад относительно уже работающей раскладки
//     (stage=cpu_only или меньше слоёв).
//
// Возвращает (worth, why). worth=true при любой неопределённости (нет адреса,
// нет стратегии) — прежнее поведение сохраняется.
func (c *NCtxReloadCoordinator) growthWorthReload(
	backendAddr, backendID, modelName string,
	targetTotalNCtx, currentTotalNCtx, currentPerSeqNCtx, currentGPULayers int,
) (bool, string) {
	if backendAddr == "" || modelName == "" {
		return true, ""
	}
	hints := c.ReloadHintsFor(backendID, modelName)
	httpClient := &http.Client{Timeout: 3 * time.Second}
	strategy := queryAdaptiveStrategy(backendAddr, modelName, targetTotalNCtx, hints.KVCacheType, httpClient)
	if strategy == nil {
		// Стратегии нет (cppworker не ответил/404) — не берём на себя
		// решение: прежнее поведение (reload).
		return true, ""
	}

	// 1. Понижение GPU-оффлоада. -1 = «все слои», это не понижение.
	if currentGPULayers > 0 {
		switch {
		case strategy.GPULayers == 0 && strategy.Stage == "cpu_only":
			return false, fmt.Sprintf(
				"адаптивная стратегия вернула cpu_only (gpu_layers=0), а модель уже работает "+
					"с %d слоями на GPU: перезагрузка выкинула бы модель из VRAM ради окна %d "+
					"(текущее %d), то есть без выигрыша",
				currentGPULayers, targetTotalNCtx, currentTotalNCtx)
		case strategy.GPULayers > 0 && strategy.GPULayers < currentGPULayers:
			return false, fmt.Sprintf(
				"адаптивная стратегия предлагает %d GPU-слоёв против %d работающих: "+
					"перезагрузка ухудшила бы раскладку ради окна %d (текущее %d)",
				strategy.GPULayers, currentGPULayers, targetTotalNCtx, currentTotalNCtx)
		}
	}

	// 2. Окно не увеличится. Проверяем только когда reload затевался РАДИ роста:
	// если target <= current, это не growth-перезагрузка (другая причина), и
	// решать за неё мы не имеем права.
	if targetTotalNCtx > currentTotalNCtx && strategy.NCtx > 0 && currentTotalNCtx > 0 &&
		strategy.NCtx <= currentTotalNCtx {
		return false, fmt.Sprintf(
			"стратегия даёт суммарное окно %d при текущем %d (на клиента %d): "+
				"reload не изменил бы окно, но выгрузил бы модель на 1–3 минуты",
			strategy.NCtx, currentTotalNCtx, currentPerSeqNCtx)
	}

	return true, ""
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
	// Round 35c+ (2026-08-13): diagnostic logging для отладки "num_ctx downgrade
	// 32768 → 8192" на A10. Логируем на Info уровне чтобы видеть что реально
	// приходит от клиента в OpenAI /v1/chat/completions path (Cline использует
	// этот path). Показывает:
	//   - body_n_ctx: что прислал клиент (RequestedNCtxOverride)
	//   - loaded_n_ctx: что в данный момент загружено в cppworker
	//   - model_max_n_ctx: что профиль говорит (modelMaxContext)
	//   - estimated_prompt_tokens: расчётная длина prompt
	//   - n_predict: max tokens to generate (default 2048)
	//   - required: estimated + n_predict + reserve (что реально нужно)
	logger.Get().Infow("preflight: DecidePreflight inputs",
		"backend_id", state.BackendID,
		"model", meta.ModelName,
		"body_n_ctx", meta.RequestedNCtxOverride,
		"loaded_n_ctx", state.CurrentNCtx,
		"model_max_n_ctx", state.ModelMaxContext,
		"max_vram_n_ctx", state.MaxVRAMNCtx,
		"estimated_prompt_tokens", meta.EstimatedPromptTokens,
		"n_predict", meta.RequestedNPredict,
		"has_tools", meta.HasTools)
	// Резерв под округление токенизатора +1 токен под EOS. Cline может дослать
	// токены в function-call парсинге, поэтому запас нужен.
	//
	// R83-fix (2026-09-30): запас ОГРАНИЧЕН сверху. Было ровно 10% от промпта —
	// на длинном продолжении сессии (120k токенов) это 12k «виртуальных» токенов,
	// и required раздувался выше потолка/загруженного окна, хотя сам контекст
	// помещается. Клиент получал 413 «prompt + n_predict exceeds n_ctx», хотя
	// история влезала. Теперь запас = min(10%, 2048) — этого хватает на
	// округление и EOS, и он не растёт линейно с длиной диалога.
	slack := meta.EstimatedPromptTokens / 10
	if slack > maxReserveSlackTokens {
		slack = maxReserveSlackTokens
	}
	reserveSlack := slack
	nPredict := meta.RequestedNPredict
	if nPredict <= 0 {
		nPredict = 2048 // default из bridge
	}
	required := meta.EstimatedPromptTokens + nPredict + 1 + reserveSlack

	// R60.31 (2026-09-10): STICKINESS FIX — убираем reload loop.
	//
	// Round 34 follow-up: если клиент ЯВНО запросил больше n_ctx чем
	// загружено → нужен reload, НЕЗАВИСИМО от фактического размера prompt.
	// Cline/OpenWebUI шлют num_ctx=65536 даже для маленьких prompts.
	//
	// R60.31 fix: добавляем check "loaded покрывает required" ДО
	// reload. Если loaded=8192 и client num_ctx=2048, но required=500
	// (маленький prompt), loaded покрывает — NoOp. Без этого fix
	// OpenWebUI шлёт num_ctx=8192, preflight trigger reload на 8192.
	// Другой запрос шлёт num_ctx=2048 → reload вниз. Loop.
	//
	// Ollama и LM Studio решают это sticky n_ctx: loaded == requested
	// без auto-reload. У нас auto-reload РАЗРЕШЁН (фича), но с защитой
	// от loop: trigger reload ТОЛЬКО если loaded < required (НЕ
	// только потому что client указал больший num_ctx).
	//
	// Сценарии:
	//   loaded=8192, client num_ctx=2048, required=500 → NoOp
	//   (stickiness, loaded покрывает)
	//   loaded=2048, client num_ctx=8192, required=500 → NoOp
	//   (loaded покрывает required, НЕ делаем upgrade-only)
	//   loaded=2048, required=5000 → Reload (loaded не покрывает)
	if meta.RequestedNCtxOverride > 0 && state.CurrentNCtx > 0 &&
		meta.RequestedNCtxOverride > state.CurrentNCtx &&
		required > state.CurrentNCtx {
		// R60.31: дополнительная проверка — loaded покрывает required?
		// Если да, NoOp (stickiness), даже если client указал больше.
		target := meta.RequestedNCtxOverride
		// R68 (2026-09-23): сравниваем с ФИЗИЧЕСКИМ потолком, а не с profile
		// hint. Раньше здесь стоял ModelMaxContext, который в Tier 1 равнялся
		// contextLength профиля (8192 для gemma) — и запрос Cline с num_ctx=65536
		// отбивался 413 «exceeds model max context=8192», хотя модель держит
		// 131072 и VRAM позволяет 66K.
		if ceil := state.physicalCeiling(); ceil > 0 && target > ceil {
			// Запрошено больше, чем физически возможно — reject с честными
			// цифрами (профиль/hint, GGUF, operator cap, VRAM).
			return makePreflightReject(state, required, fmt.Sprintf(
				"requested n_ctx=%d exceeds backend context ceiling=%d (profile hint=%d, gguf max=%d, auto_reload_max_n_ctx=%d)",
				target, ceil, state.ProfileHintNCtx, state.GGUFMaxContext, state.AutoReloadMaxNCtx))
		}
		if cfg.AutoReloadMaxNCtx > 0 && target > cfg.AutoReloadMaxNCtx {
			return makePreflightReject(state, required, fmt.Sprintf(
				"requested n_ctx=%d exceeds configured auto_reload_max_n_ctx=%d",
				target, cfg.AutoReloadMaxNCtx))
		}
		logger.Get().Infow("preflight: client requested n_ctx > loaded AND required > loaded, triggering reload",
			"backend_id", state.BackendID,
			"requested_n_ctx", target, "current_n_ctx", state.CurrentNCtx,
			"estimated_tokens", meta.EstimatedPromptTokens, "n_predict", nPredict,
			"required_n_ctx", required)
		// Сразу возвращаем Reload (RunPreflight конвертирует в AsyncReload если
		// включён PreflightAsyncReload). Возвращать PreflightAsyncReload здесь
		// НЕЛЬЗЯ — switch в RunPreflight его не обрабатывает и фолбэчит на NoOp.
		return &PreflightResult{
			Decision:   PreflightReload,
			TargetNCtx: roundUpPow2(target),
		}
	}

	// Уже помещается — NoOp.
	//
	// R83 §9.2 (2026-09-26): перед NoOp проверяем случай «VRAM известна, но веса
	// не влезают» — НО только если клиент ПРОСИТ больше, чем уже загружено.
	// Если запрос помещается в текущий n_ctx, отказывать нельзя: сессия уже
	// работает (модель загружена через partial offload, cppworker это умеет), и
	// ломать её отказом было бы регрессом. Отказ адресован ровно той ситуации,
	// ради которой раньше уходили в reload: «нужно больше контекста», а веса в
	// VRAM не помещаются ни при каком gpu_layers, поэтому reload не поможет.
	//
	// R83-fix (2026-09-30): сравниваем с окном СЛОТА, а не с суммарным.
	// Живое воспроизведение (стенд): gemma-4 загружена ctx=65536 при
	// max_slots=2 → клиенту доступно 32768, а история занимала ~101k токенов.
	// Проверка по суммарному окну давала «required <= current» → NoOp и обрыв
	// дальше, либо уходила в ветку VRAM-отказа с текстом «reload cannot raise
	// this ceiling», хотя перезагрузка с окном 101k × 2 слота решает задачу
	// (KV уезжает в RAM — модель и так работает через partial offload).
	if perSeq := state.effectivePerSeqNCtx(); perSeq > 0 && required <= perSeq {
		return &PreflightResult{Decision: PreflightNoOp}
	}
	if state.CurrentNCtx > 0 && required <= state.CurrentNCtx && state.CurrentContextPerSeq <= 0 {
		// Окно слота неизвестно — прежнее поведение (сравнение с суммарным).
		return &PreflightResult{Decision: PreflightNoOp}
	}
	if state.VRAMKnown && state.MaxVRAMNCtx == 0 {
		// Веса не влезают в VRAM целиком даже без KV-cache: exact-fit потолок
		// равен нулю.
		//
		// R83-fix (2026-09-30): это НЕ повод отказывать, если физический потолок
		// (GGUF/operator cap) покрывает требование. Раньше здесь стоял безусловный
		// reject «reload cannot raise this ceiling» — и запрос с длинной историей
		// получал 413 за 0 c, хотя модель уже работает через partial offload, а
		// перезагрузка с бОльшим окном (KV в RAM) задачу решает. Отказ оставляем
		// только когда требование действительно выше физического потолка.
		ceil := state.physicalCeiling()
		if ceil <= 0 || required > ceil {
			logger.Get().Warnw("preflight: VRAM известна, но max_vram_n_ctx=0 — веса не влезают",
				"backend_id", state.BackendID,
				"required_n_ctx", required,
				"current_n_ctx", state.CurrentNCtx,
				"context_per_seq", state.CurrentContextPerSeq,
				"model_max_context", state.ModelMaxContext,
				"gguf_max_context", state.GGUFMaxContext,
				"physical_ceiling", ceil,
				"note", "требование выше физического потолка модели — reload не поможет")
			return makePreflightRejectWithSuggestion(state, required, fmt.Sprintf(
				"VRAM is known (%d MB available) but this model's weights do not fit: "+
					"max_vram_n_ctx=0 at any gpu_layers (model_max_context=%d, current_n_ctx=%d, "+
					"context_per_seq=%d, required=%d, physical_ceiling=%d). "+
					"Reload cannot raise this ceiling.",
				state.AvailableVRAMMB, state.ModelMaxContext, state.CurrentNCtx,
				state.CurrentContextPerSeq, required, ceil),
				"Reduce the prompt/history size, or use a smaller model or a more aggressive "+
					"quantization (Q4_K_M instead of Q8_0/F16).")
		}
		// Покрывается физическим потолком → растём (KV уйдёт в RAM при
		// partial offload). Ниже отработает общая логика reload с проверкой
		// операторского потолка.
		logger.Get().Infow("preflight: max_vram_n_ctx=0, но требование в пределах физического потолка — растём окно",
			"backend_id", state.BackendID,
			"required_n_ctx", required, "current_n_ctx", state.CurrentNCtx,
			"context_per_seq", state.CurrentContextPerSeq,
			"physical_ceiling", ceil)
	}

	// Потолок модели/оператора (если известен) — найжестший потолок.
	//
	// R68: используем physicalCeiling() (GGUF/operator cap), а не profile hint.
	//
	// R83-fix (2026-09-30): при нескольких слотах потолок НА КЛИЕНТА меньше
	// суммарного — llama.cpp делит окно между слотами (n_ctx_seq = n_ctx / slots).
	// Раньше здесь сравнивали per-seq требование с СУММАРНЫМ потолком, поэтому
	// клиент с историей 101k на модели с total-потолком 131072 при 2 слотах
	// проходил эту проверку, уходил в ветку VRAM-отказа и получал невнятное
	// «reload cannot raise this ceiling». Теперь отказ (если он неизбежен)
	// называет ПОТОЛОК НА КЛИЕНТА и способы его поднять.
	if ceil := state.physicalCeiling(); ceil > 0 {
		slots := state.effectiveSlots()
		perClientCeil := ceil
		if slots > 1 {
			perClientCeil = ceil / slots
		}
		if required > perClientCeil {
			msg := fmt.Sprintf(
				"required n_ctx=%d exceeds per-client context ceiling=%d "+
					"(backend context ceiling=%d, slots=%d, "+
					"profile hint=%d, gguf max=%d, auto_reload_max_n_ctx=%d)",
				required, perClientCeil, ceil, slots,
				state.ProfileHintNCtx, state.GGUFMaxContext, state.AutoReloadMaxNCtx)
			suggestion := ""
			if slots > 1 {
				suggestion = fmt.Sprintf(
					"При %d параллельных слотах модель делит окно между клиентами: "+
						"каждому доступно %d токенов из %d. Варианты: уменьшить историю/промпт, "+
						"поставить parallel=1 (тогда окно целиком одному клиенту), "+
						"взять модель с бОльшим окном или поднять суммарный потолок "+
						"(contextLengthMax / LB_NCTX_RELOAD_MAX_N_CTX).",
					slots, perClientCeil, ceil)
			}
			return makePreflightRejectWithSuggestion(state, required, msg, suggestion)
		}
	}

	// Потолок VRAM — больше НЕ reject, а trigger reload.
	// cppworker при reload применит AutoTuneNCtx для partial offload.
	// Reject только если AutoReloadMaxNCtx (operator cap) превышен.
	//
	// R83 §9.2: случай «VRAM известна, но веса не влезают» (MaxVRAMNCtx == 0 при
	// VRAMKnown) обработан ВЫШЕ, до NoOp-ветки: если он попадает сюда, значит
	// запрос всё равно требует больше текущего n_ctx и reload бессмыслен.
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

	// R83-fix (2026-09-30): окно не покрывает требование → ЯВНО просим рост.
	//
	// До этой правки при `max_vram_n_ctx == 0` (веса не влезают в VRAM целиком,
	// модель работает через partial offload) функция доходила до конца и
	// возвращала NoOp — то есть «растим» мы разрешили, но рост никто не
	// запрашивал: запрос уходил в cppworker с прежним окном и падал там
	// («prompt + n_predict exceeds n_ctx») либо упирался в 413.
	//
	// Условие роста: требование больше окна СЛОТА и не превышает проверенные
	// выше потолки (физический и операторский) — рост возможен, KV уедет в RAM.
	if perSeq := state.effectivePerSeqNCtx(); perSeq > 0 && required > perSeq {
		logger.Get().Infow("preflight: требование больше окна слота — запрашиваю рост окна",
			"backend_id", state.BackendID,
			"required_n_ctx", required,
			"context_per_seq", perSeq,
			"current_n_ctx", state.CurrentNCtx,
			"slots", state.effectiveSlots(),
			"max_vram_n_ctx", state.MaxVRAMNCtx)
		return &PreflightResult{
			Decision:   PreflightReload,
			TargetNCtx: roundUpPow2(required),
		}
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
	// R68: зажимаем по ФИЗИЧЕСКОМУ потолку (GGUF/operator cap), не по profile hint.
	if ceil := state.physicalCeiling(); ceil > 0 && target > ceil {
		target = ceil
	}
	rounded := roundUpPow2(target)
	if rounded < target {
		rounded = target
	}

	// Round 53.2 (2026-08-24): Round 34 (2026-08-12) Phase 2 flag-based reload
	// REMOVED. Решение о reload теперь опирается ТОЛЬКО на n_ctx (контекстное
	// окно), а НЕ на флаги (kv_cache_type, flash_attn, use_mmap).
	//
	// Pre-R53.2 (Round 34 follow-up): если client прислал options.kv_cache_type="q4_0",
	// а модель загружена с "f16" — balancer выгружал модель и перезагружал
	// с новыми params, даже если n_ctx уже подходил. Это ИЗБЫТОЧНО:
	//   - Клиент (Cline/OpenWebUI) часто не указывает эти флаги явно — get
	//     defaults от backend.
	//   - Модель с kv_cache=f16 корректно обслуживает запросы с options.kv_cache=q4_0
	//     (настройка KV-cache применяется к запросу, не к загруженной модели).
	//   - Reload занимает 5-15 сек и блокирует другие запросы.
	//   - Параллелизм (n_parallel) и так встроен в backend — клиенту не нужно
	//     это контролировать.
	//
	// User feedback (2026-08-24): "определять в каком состоянии надо перезагружать
	// модель еще опиралось на наличие флагов, то это избыточно сейчас требуется
	// оставить решение по перезагрузки модели только по контекстному окну".
	//
	// Post-R53.2: n_ctx mismatch (required > currentNCtx) → reload, иначе NoOp.
	// Клиент может менять флаги в options — backend обрабатывает per-request.

	// R60.46 (2026-09-11): НЕ trigger reload если user не указал n_predict
	// AND loaded >= requested. Иначе false-positive cascade:
	//   user sent num_ctx=2048, n_predict не указан → cppworker default 2048
	//   required(1 + 2048 + 0.1) = 2050 > loaded(2048) → reload 2048→4096
	//   User ждёт 90s, retry, опять reload, опять ждёт — застрял.
	//
	// Skip reload check если:
	//   1. n_predict не указан (user хочет default behavior)
	//   2. loaded >= requested (user's num_ctx satisfied, no need to upgrade)
	if meta != nil && meta.RequestedNPredict <= 0 && state.CurrentNCtx > 0 &&
		meta.RequestedNCtxOverride > 0 && meta.RequestedNCtxOverride <= state.CurrentNCtx {
		return &PreflightResult{Decision: PreflightNoOp}
	}

	// Если n_ctx уже fits (required <= currentNCtx) — NoOp, перезагрузка НЕ нужна.
	if state.CurrentNCtx > 0 && required <= state.CurrentNCtx {
		return &PreflightResult{Decision: PreflightNoOp}
	}

	return &PreflightResult{
		Decision:   PreflightReload,
		TargetNCtx: rounded,
	}
}

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

// nctxPreflightWaitDefaultSec — R68 (2026-09-23): сколько ЖДАТЬ завершения
// async reload, прежде чем ответить клиенту 503/stream-dialog.
//
// Жалоба R68 (Cline): клиент с Model Context Window = 65536 получал либо 413
// (preflight reject по profile hint), либо 503 «retry in 30s» — и работал
// только повторный запрос. Тот же принцип, что в R67a для авто-загрузки:
// первый же запрос дожидается готовности модели и обслуживается.
//
// 240 c < типичного клиентского таймаута (Cline 300000 ms, OpenWebUI 300 c),
// но с запасом больше реального reload'а (gemma-4 65K на 8 GB — 119 c).
const nctxPreflightWaitDefaultSec = 240

// nctxPreflightWaitTimeout — R68: предел ожидания async reload.
//
//	LB_NCTX_PREFLIGHT_WAIT_SEC=N — N секунд (0 = прежнее поведение: сразу
//	503+Retry-After / stream-dialog с keepalive; отрицательное = ждать до
//	отмены клиентом, но не дольше 30 минут).
//	По умолчанию 240 c.
func nctxPreflightWaitTimeout() time.Duration {
	v := strings.TrimSpace(os.Getenv("LB_NCTX_PREFLIGHT_WAIT_SEC"))
	if v == "" {
		return nctxPreflightWaitDefaultSec * time.Second
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return nctxPreflightWaitDefaultSec * time.Second
	}
	switch {
	case n == 0:
		return 0
	case n < 0:
		return 30 * time.Minute
	default:
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
		if worth, why := c.growthWorthReload(backendAddr, backendID, gateModelName,
			plan.NewNCtx, state.CurrentNCtx, state.effectivePerSeqNCtx(),
			c.ReloadHintsFor(backendID, gateModelName).GPULayers); !worth {
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
					reloadCtx, cancel := context.WithTimeout(context.Background(), cfg.effectiveTimeout())
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
					reloadCtx, cancel := context.WithTimeout(context.Background(), cfg.effectiveTimeout())
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
			if wait := nctxPreflightWaitTimeout(); wait > 0 {
				waitStart := time.Now()
				if waitErr := c.WaitReloadDone(backendID, modelName, wait); waitErr == nil {
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
