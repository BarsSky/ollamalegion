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
