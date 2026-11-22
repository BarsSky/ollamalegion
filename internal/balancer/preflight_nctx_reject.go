// preflight_nctx_reject.go — R635 (2026-11-19): отказы preflight: причины расхождения параметров, «окно слишком велико», ожидание и тексты.
//
// ЗАЧЕМ ВЫНЕСЕНО. `preflight_nctx.go` держал 1601 строку: разбор запроса и
// метаданных, решение о preflight, отказы и сам RunPreflight. Здесь — один из слоёв;
// поведение не менялось.
package balancer

import (
	"encoding/json"
	"net/http"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"os"
	"strconv"
	"strings"
	"time"
)

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
	want := i18n.T("balancer.preflight_nctx.reasoning_enabled")
	have := i18n.T("balancer.preflight_nctx.reasoning_disabled")
	if *state.CurrentReasoningEnabled {
		have = i18n.T("balancer.preflight_nctx.reasoning_enabled")
	}
	if !*meta.RequestedThink {
		want = i18n.T("balancer.preflight_nctx.reasoning_disabled")
	}
	explain := i18n.T("balancer.preflight_nctx.reasoning_mismatch_explain",
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
		parts = append(parts, i18n.T("balancer.preflight_nctx.param_diff_line", d.Parameter, d.Loaded, d.Requested))
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
		"error": i18n.T("balancer.preflight_nctx.param_mismatch_error",
			meta.ModelName, strings.Join(parts, "; ")),
		"reason":            strings.Join(parts, "; "),
		"param_differences": diffs,
		"loaded":            loaded,
		"requested":         requested,
		"what_to_do":        i18n.T("balancer.preflight_nctx.param_mismatch_what_to_do"),
		"backend_id":        state.BackendID,
		"model":             meta.ModelName,
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
	msg := i18n.T("balancer.preflight_nctx.window_too_big_error",
		requestedPerClient, because, slots, ceiling, perClientCeil)
	body := map[string]interface{}{
		"error":                 msg,
		"reason":                i18n.T("balancer.preflight_nctx.window_too_big_reason", requestedPerClient, requestedTotal, perClientCeil),
		"requested_n_ctx":       requestedPerClient,
		"requested_total_n_ctx": requestedTotal,
		"max_per_client_n_ctx":  perClientCeil,
		"backend_ceiling_n_ctx": ceiling,
		"slots":                 slots,
		"loaded_n_ctx":          state.CurrentNCtx,
		"context_per_seq":       state.effectivePerSeqNCtx(),
		"gguf_max_context":      state.GGUFMaxContext,
		"auto_reload_max_n_ctx": state.AutoReloadMaxNCtx,
		"what_to_do":            i18n.T("balancer.preflight_nctx.window_too_big_what_to_do", perClientCeil),
		"backend_id":            state.BackendID,
		"model":                 meta.ModelName,
	}
	encoded, _ := json.Marshal(body)
	return &PreflightResult{
		Decision:     PreflightReject,
		RejectStatus: http.StatusRequestEntityTooLarge,
		RejectBody:   string(encoded),
	}
}

// contextWindowExhaustedCodeR664 — машинный код отказа «данные не влезают, а окно
// клиент не заявил» (R664, шаг B).
//
// Один код на ВСЕ пути (preflight-движок и стриминговый вердикт R301): клиент и
// оператор должны видеть одну причину, а не три разных текста. Код машинный,
// потому что по нему клиент решает, что делать («заявить окно»), — человеческий
// текст для этого не годится (R664: «отказать с конкретными числами, чтобы было
// понятно, что заявить»).
const contextWindowExhaustedCodeR664 = "context_window_exhausted_no_growth_request"

// windowCeilingNumbersR664 — ПОТОЛКИ окна для тела отказа шага B.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ТИП (живой дефект прогона r679). Сборщик тела принимал
// одиннадцать ПОЗИЦИОННЫХ чисел, и стриминговый путь передавал на месте трёх
// потолков «0, 0, 0». Компилятор такой пропуск не видит, читатель — тоже, а
// клиент получал отказ, в котором `physical_ceiling_tokens`/`gguf_max_context`/
// `auto_reload_max_n_ctx` равны нулю при реальных 65536/131072/65536. Ноль в этих
// полях читается как «потолок неизвестен», то есть «заявить больше нельзя» —
// ровно противоположное тому, что нужно сказать.
//
// Теперь потолки — ИМЕНОВАННЫЕ поля, и заполняет их одна из двух функций сборки,
// каждая из своего источника. Формула одна на обе: `resolvePhysicalMaxContext`
// (R265) — вторая не заводится.
type windowCeilingNumbersR664 struct {
	// Physical — СУММАРНЫЙ физический потолок модели
	// (min(GGUF, contextLengthMax профиля, операторский потолок)). 0 = неизвестен.
	Physical int
	// GGUF — потолок обучения модели из метаданных GGUF (0 = воркер не сообщил).
	GGUF int
	// AutoReload — операторский потолок авто-роста (LB_NCTX_RELOAD_MAX_N_CTX).
	AutoReload int
}

// ceilingNumbersFromStateR664 — потолки из СОБРАННОГО состояния preflight-движка.
//
// Источник — NCtxBackendState: он собран один раз и на момент решения
// зафиксирован (R265: повторный поход в метрики посреди решения менял бы картину).
func ceilingNumbersFromStateR664(state *NCtxBackendState) windowCeilingNumbersR664 {
	if state == nil {
		return windowCeilingNumbersR664{}
	}
	return windowCeilingNumbersR664{
		Physical:   state.physicalCeiling(),
		GGUF:       state.GGUFMaxContext,
		AutoReload: state.AutoReloadMaxNCtx,
	}
}

// ceilingNumbersForR664 — те же потолки для путей, у которых нет состояния
// preflight-движка (транспортный вердикт R301 на стриминге).
//
// Источники ТЕ ЖЕ, что у сборки состояния: GGUF именно этой модели
// (ggufMaxContextForModel), потолок профиля (profileCapCtx с fold — как у
// physicalNumCtxCeiling) и операторский потолок (preflightAutoReloadMaxNCtx), и
// одна формула resolvePhysicalMaxContext. Именно поэтому тела отказа двух путей
// совпадают, а не «примерно похожи».
//
// Physical берётся у physicalNumCtxCeiling — той же функции, которой вердикт
// считает available_context_tokens: иначе два числа одного тела могли бы
// разойтись (available больше физического потолка — абсурд для клиента).
func (p *Proxy) ceilingNumbersForR664(backendID, model string) windowCeilingNumbersR664 {
	if p == nil {
		return windowCeilingNumbersR664{}
	}
	return windowCeilingNumbersR664{
		Physical:   p.physicalNumCtxCeiling(model, backendID),
		GGUF:       p.ggufMaxContextForModel(backendID, model),
		AutoReload: preflightAutoReloadMaxNCtx(p),
	}
}

// windowRejectNumbersR664 — ВСЕ числа тела отказа шага B.
//
// Одна структура вместо одиннадцати позиционных аргументов: живой дефект r679
// состоял ровно в том, что три из них были переданы нулями, и заметить это можно
// было только на стенде — по ответу клиенту. Теперь пропустить поле нельзя
// незаметно: его видно и в месте вызова, и в имени структуры.
type windowRejectNumbersR664 struct {
	BackendID string
	Model     string
	// PromptTokens — сколько токенов в промпте по нашей оценке.
	PromptTokens int
	// LoadedWindow — окно ОДНОГО слота сейчас (столько дадут клиенту).
	LoadedWindow int
	// RequiredWindow — МИНИМУМ, который нужно заявить, чтобы промпт обслужился
	// (промпт + нижняя граница ответа + slack, выровнено по 256).
	RequiredWindow int
	// Requested — что заявил клиент (0 — не заявил ничего).
	Requested int
	// AvailablePerClient — потолок НА КЛИЕНТА (0 = неизвестен): больше заявить
	// нельзя, и совет «заявите больше» был бы ложью.
	AvailablePerClient int
	Slots              int
	Ceilings           windowCeilingNumbersR664
}

// contextWindowExhaustedBodyR664 — ОДНО тело отказа шага B, собранное из чисел.
//
// ПОЧЕМУ ИЗ ЧИСЕЛ, А НЕ ИЗ СОСТОЯНИЯ. Тело нужно двум путям: preflight-движку (у
// него есть NCtxBackendState: GGUF, операторский потолок, слоты) и стриминговому
// вердикту R301 (у него только числа решения). Второе тело для второго пути —
// ровно тот класс, из которого выросли «два мозга» (R493): один и тот же отказ,
// записанный дважды, расходится на первой же правке.
//
// ЧИСЛА (все — в токенах; окно на клиента, единица llama.cpp при kv_unified=false):
//
//	prompt_tokens            сколько токенов в промпте по нашей оценке;
//	loaded_window_tokens     окно ОДНОГО слота сейчас (столько дадут клиенту);
//	required_window_tokens   МИНИМУМ, который нужно заявить (промпт + нижняя
//	                         граница ответа + slack, выровнено по 256);
//	requested_n_ctx          что заявил клиент (0 — не заявил ничего);
//	available_context_tokens потолок НА КЛИЕНТА (0 = неизвестен): больше заявить
//	                         нельзя, и совет «заявите больше» был бы ложью;
//	physical_ceiling_tokens  суммарный физический потолок модели (0 = неизвестен);
//	gguf_max_context         потолок обучения из GGUF (0 = воркер не сообщил);
//	auto_reload_max_n_ctx    операторский потолок авто-роста.
//
// R679 (2026-11-20): последние три поля стриминговый путь передавал нулями.
// Теперь оба пути заполняют их явно (windowCeilingNumbersR664) и проверяются на
// совпадение телом (context_window_reject_parity_test.go).
func contextWindowExhaustedBodyR664(n windowRejectNumbersR664) string {
	body := map[string]interface{}{
		"error": contextWindowExhaustedCodeR664,
		"reason": i18n.T("balancer.preflight_nctx.context_window_exhausted_reason",
			n.PromptTokens, n.LoadedWindow, n.RequiredWindow),
		"model":                    n.Model,
		"backend_id":               n.BackendID,
		"prompt_tokens":            n.PromptTokens,
		"loaded_window_tokens":     n.LoadedWindow,
		"required_window_tokens":   n.RequiredWindow,
		"requested_n_ctx":          n.Requested,
		"available_context_tokens": n.AvailablePerClient,
		"physical_ceiling_tokens":  n.Ceilings.Physical,
		"slots":                    normalizedSlotsR504(n.Slots),
		"context_per_seq":          n.LoadedWindow,
		"gguf_max_context":         n.Ceilings.GGUF,
		"auto_reload_max_n_ctx":    n.Ceilings.AutoReload,
		"what_to_do": i18n.T("balancer.preflight_nctx.context_window_exhausted_what_to_do",
			n.RequiredWindow, n.AvailablePerClient),
	}
	encoded, _ := json.Marshal(body)
	return string(encoded)
}

// makePromptExceedsWindowRejectR664 — HTTP 413: промпт не влезает в загруженное
// окно, а окно клиент НЕ заявил (R664, шаг B).
//
// ЖИВОЙ ДЕФЕКТ, ради которого этот отказ существует. Запрос без num_ctx на
// промпт 13023 при загруженном окне 8192 заставлял балансер вывести окно из
// СОБСТВЕННОЙ оценки (оценка + бюджет + slack), выгрузить модель и загрузить её
// заново — минуты простоя ради числа, которого клиент не просил, — и всё равно
// не гарантировал обслуживание. После шага B рост идёт ТОЛЬКО под заявку
// (R83/R68), а без заявки ответ — этот 413: модель остаётся работать (R294:
// окно не уменьшаем, чужой ход не рвём), а клиент получает три числа и точное
// действие.
//
// ЧЕМ ЭТО НЕ ЯВЛЯЕТСЯ. Это не отказ по потолку (makeWindowTooBigReject): там
// клиент заявил окно, которого модель не даст; здесь — не заявил ничего, и
// отказ ровно об этом (код отличается, потому что действие клиента другое).
func makePromptExceedsWindowRejectR664(
	state *NCtxBackendState, meta *RequestMeta,
	promptTokens, loadedWindow, requiredWindow int,
) *PreflightResult {
	slots := state.effectiveSlots()
	ceilings := ceilingNumbersFromStateR664(state)
	body := contextWindowExhaustedBodyR664(windowRejectNumbersR664{
		BackendID:          state.BackendID,
		Model:              meta.ModelName,
		PromptTokens:       promptTokens,
		LoadedWindow:       loadedWindow,
		RequiredWindow:     requiredWindow,
		Requested:          meta.RequestedNCtxOverride,
		AvailablePerClient: derivedWindowCapR466(ceilings.Physical, slots),
		Slots:              slots,
		Ceilings:           ceilings,
	})
	return &PreflightResult{
		Decision:     PreflightReject,
		RejectStatus: http.StatusRequestEntityTooLarge,
		RejectBody:   body,
	}
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
		logger.Get().Warnw(i18n.T("balancer.preflight_nctx.wait_env_unrecognized_warn"),
			"value", v, "error", err)
		return nctxPreflightWaitUnlimited
	}
	switch {
	case n == 0:
		return 0
	case n < 0:
		return nctxPreflightWaitUnlimited
	default:
		logger.Get().Warnw(i18n.T("balancer.preflight_nctx.wait_cap_armed_warn"),
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
