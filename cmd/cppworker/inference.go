// inference.go — Inference core, token counting, RAM fallback, and reload helpers.
package main

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/pkg/logger"
)

// ============================================================
// Token counting
// ============================================================

func countTokens(modelName, text string) int {
	if text == "" {
		return 0
	}
	return backend.CountTokens(modelName, text)
}

// countModelTokensByLoadedInfo — если модель ещё не загружена, не пытаться
// загружать её ради подсчёта токенов; вернуть грубую оценку.
// Безопасен при backend == nil (например, в unit-тестах): возвращает грубую оценку
// по 4 символа на токен. Раньше здесь был nil pointer panic — см. репро в
// inference_internal_test.go (TestClampNPredictToFitContext_ToolsPromptOverflow).
func countModelTokensByLoadedInfo(modelName, text string) int {
	if text == "" {
		return 0
	}
	if backend == nil {
		// backend не инициализирован (unit-тест, нештатный запуск). Возвращаем
		// грубую оценку — лучше, чем паника.
		return len([]rune(text)) / 4
	}
	if _, err := backend.GetModel(modelName); err != nil {
		return len([]rune(text)) / 4
	}
	return backend.CountTokens(modelName, text)
}

// ============================================================
// Load options comparison
// ============================================================

// sameLoadOptions сравнивает параметры загруженной модели с запрошенными
// параметрами загрузки. Используется handleLoadModel для защиты от
// повторной загрузки модели с теми же параметрами.
func sameLoadOptions(info cppbackend.ModelInfo, opts cppbackend.LoadModelOpts) bool {
	if info.ContextSize != opts.ContextSize {
		return false
	}
	if info.BatchSize != opts.BatchSize {
		return false
	}
	if info.GPULayers != opts.GPULayers {
		return false
	}
	if info.FlashAttnType != opts.FlashAttnType {
		return false
	}
	if info.NUMA != opts.NUMA {
		return false
	}
	if info.UseMmap != opts.UseMmap {
		return false
	}
	if len(info.TensorSplit) != len(opts.TensorSplit) {
		return false
	}
	for i := range info.TensorSplit {
		if info.TensorSplit[i] != opts.TensorSplit[i] {
			return false
		}
	}
	// Session 16 (2026-06-27): Parallel + KVCacheType.
	// Если профиль модели изменил parallel=2 → kv=q8_0, handleLoadModel/
	// handleReloadModel должны видеть разницу и перезагрузить модель с новыми
	// параметрами. До этой правки сравнение игнорировало parallel/kv, что
	// делало per-model profile частично нерабочим (можно было поменять parallel
	// в UI, но модель продолжала работать со старыми значениями).
	if info.Parallel != opts.Parallel {
		return false
	}
	if info.KVCacheType != opts.KVCacheType {
		return false
	}
	return true
}

// ============================================================
// C-bridge error classification
// ============================================================

// isNCtxNeedsReload — true, если последняя ошибка C-bridge говорит
// "requested n_ctx exceeds model's effective n_ctx" (code 2).
func isNCtxNeedsReload() bool {
	info := bridge.GetLastErrorInfo()
	return info != nil && info.Code == bridge.ErrCodeNCtxNeedsReload
}

// isGpuOomOrNCtxNeedsReload — true, если последняя ошибка C-bridge
// требует перезагрузки модели с другими параметрами: либо n_ctx
// недостаточен (code 2), либо GPU OOM (code 4). В обоих случаях
// RAM fallback может помочь, снизив GPU-слои и/или увеличив n_ctx.
func isGpuOomOrNCtxNeedsReload() bool {
	info := bridge.GetLastErrorInfo()
	if info == nil {
		return false
	}
	return info.Code == bridge.ErrCodeNCtxNeedsReload || info.Code == bridge.ErrCodeGPUOOM
}

// reloadInProgress tracks models currently being reloaded by RAM fallback.
// The value is a chan struct{} that is closed when the reload (including any
// rollback) completes. Concurrent requests that encounter "model not loaded"
// during this window wait on this channel and retry instead of returning
// connection reset to the client.
var reloadInProgress sync.Map

// waitReloadInProgress — если для модели modelName сейчас выполняется
// RAM fallback reload (reloadInProgress flag установлен), ждёт его
// завершения. Возвращает true, если дождались; false если канал не найден.
func waitReloadInProgress(modelName string) bool {
	chRaw, ok := reloadInProgress.Load(modelName)
	if !ok {
		return false
	}
	ch, ok := chRaw.(chan struct{})
	if !ok {
		return false
	}
	// Ждём завершения перезагрузки (канал закроется в defer)
	<-ch
	return true
}

// ============================================================
// RAM fallback reload-loop protection
// ============================================================
//
// Проблема (см. docs/remaining-real-plan.md, OpenWebUI tools-flow bug):
// При работе с OpenWebUI tools (web search и т.п.) каждая итерация диалога
// добавляет tool definitions / tool calls / tool results в prompt. Если
// n_ctx модели слишком мал, на КАЖДОЙ итерации происходит n_ctx overflow
// → tryRamFallbackReload → unload + reload модели → это занимает 10-30 сек.
// OpenWebUI при этом не получает ответа, отменяет запрос, шлёт новый —
// и цикл повторяется. В худшем случае бесконечный reload-loop без шансов
// на успех (если n_ctx всё равно недостаточен для system prompt + tools).
//
// Решение: ограничить количество последовательных reload-попыток для
// каждой модели в скользящем окне cycleResetInterval. Если превышен лимит —
// возвращаем ошибку ErrReloadLoopLimit вместо reload. Caller получит
// HTTP 413 с понятным сообщением "model cannot fit prompt even after
// N reload attempts, increase n_ctx or reduce prompt/tool definitions".
//
// Счётчик сбрасывается после успешного инференса без n_ctx ошибки.

const (
	// ramFallbackMaxAttempts — максимум reload-попыток для одной модели
	// в скользящем окне cycleResetInterval.
	ramFallbackMaxAttempts = 3
	// ramFallbackCycleResetInterval — скользящее окно для подсчёта попыток.
	// После успешного инференса без n_ctx ошибки счётчик сбрасывается немедленно.
	ramFallbackCycleResetInterval = 60 * time.Second
)

// ramFallbackAttempts — per-model счётчик reload-попыток с временной меткой.
// Используем sync.Map для потокобезопасности без блокировок на горячем пути.
var ramFallbackAttempts sync.Map

// ramFallbackAttemptState хранит состояние счётчика для одной модели.
type ramFallbackAttemptState struct {
	mu               sync.Mutex
	count            int
	firstAttemptTime time.Time
	lastAttemptTime  time.Time
}

// getReloadAttempts возвращает (count, isCycleLimit) для модели.
// count — число reload-попыток в текущем окне.
// isCycleLimit — true, если превышен лимит (нужно decline reload).
// Автоматически сбрасывает счётчик, если окно истекло.
func getReloadAttempts(modelName string) (count int, isCycleLimit bool) {
	stateRaw, ok := ramFallbackAttempts.Load(modelName)
	if !ok {
		return 0, false
	}
	state, ok := stateRaw.(*ramFallbackAttemptState)
	if !ok {
		return 0, false
	}
	state.mu.Lock()
	defer state.mu.Unlock()

	// Если прошло больше cycleResetInterval с ПЕРВОЙ попытки в окне — сбрасываем.
	if !state.firstAttemptTime.IsZero() &&
		time.Since(state.firstAttemptTime) > ramFallbackCycleResetInterval {
		state.count = 0
		state.firstAttemptTime = time.Time{}
		state.lastAttemptTime = time.Time{}
	}
	if state.count >= ramFallbackMaxAttempts {
		return state.count, true
	}
	return state.count, false
}

// recordReloadAttempt увеличивает счётчик reload-попыток для модели.
// Вызывается в начале каждой tryRamFallbackReload, когда принято решение
// действительно делать reload (после прохождения isCycleLimit check).
func recordReloadAttempt(modelName string) {
	stateRaw, _ := ramFallbackAttempts.LoadOrStore(modelName, &ramFallbackAttemptState{})
	state := stateRaw.(*ramFallbackAttemptState)
	state.mu.Lock()
	defer state.mu.Unlock()
	now := time.Now()
	if state.firstAttemptTime.IsZero() ||
		now.Sub(state.firstAttemptTime) > ramFallbackCycleResetInterval {
		// Начинаем новое окно
		state.count = 1
		state.firstAttemptTime = now
		state.lastAttemptTime = now
		return
	}
	state.count++
	state.lastAttemptTime = now
}

// resetReloadAttempts сбрасывает счётчик reload-попыток для модели.
// Вызывается после успешного инференса без n_ctx ошибки (т.е. prompt
// влез в текущий n_ctx — больше reload не нужен).
func resetReloadAttempts(modelName string) {
	stateRaw, ok := ramFallbackAttempts.Load(modelName)
	if !ok {
		return
	}
	state, ok := stateRaw.(*ramFallbackAttemptState)
	if !ok {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	state.count = 0
	state.firstAttemptTime = time.Time{}
	state.lastAttemptTime = time.Time{}
}

// tryRamFallbackReloadAllowTools — глобальный флаг, разрешающий reload при tools.
//
// Когда true (default с 2026-06-23 в связке с preflight), RAM-fallback
// reload применяется и для tools-запросов: balancer решает, нужен ли reload
// (на основе VRAM и model_max), а cppworker исполняет. Это позволяет
// динамически подстраивать n_ctx под длинный prompt Cline/OpenWebUI.
//
// Флаг задаётся через env/flag --ram-fallback-allow-tools / CPPWORKER_RAM_FALLBACK_ALLOW_TOOLS
// (default: true для совместимости с поведением preflight; см. PR).
var tryRamFallbackReloadAllowTools = true

// ErrReloadLoopLimit возвращается из tryRamFallbackReload, когда
// превышен лимит reload-попыток для модели. Caller (handler) должен
// вернуть HTTP 413 с понятным сообщением.
type ReloadLoopLimitError struct {
	Model   string
	Count   int
	Elapsed time.Duration
}

func (e *ReloadLoopLimitError) Error() string {
	return fmt.Sprintf("RAM fallback reload limit reached for model %q: %d reloads in %s. "+
		"Either the prompt is too large for available n_ctx, or tool definitions + history exceed the configured n_ctx. "+
		"Reduce tools/prompt size, or save a model profile with larger context_length",
		e.Model, e.Count, e.Elapsed)
}

// isReloadLoopLimitError — true, если err это *ReloadLoopLimitError.
// Используется в handlers для трансляции ошибки в HTTP 413.
func isReloadLoopLimitError(err error) bool {
	if err == nil {
		return false
	}
	_, ok := err.(*ReloadLoopLimitError)
	return ok
}

// ============================================================
// Превентивный клампинг n_predict
// ============================================================

// PromptExceedsNCtxError — возвращается из clampNPredictToFitContext,
// когда actual_prompt_tokens сам по себе превышает n_ctx (даже с учётом
// minNPredictClamp). В этом случае ни один разумный n_predict не поможет —
// bridge вернёт code 3 "prompt too long". Лучше сразу вернуть HTTP 413
// с понятным сообщением и подсказкой увеличить n_ctx, чем молча клампить
// n_predict до 512 (как было до 2026-06-24) и получать «пустой ответ»
// с done_reason="stop" (модель эмитит <end_of_turn> на 1-м токене).
//
// ВАЖНО (2026-06-24): MaxVRAMNCtx и NCtxOverride используются writePromptExceedsNCtxResponse
// для формирования bridge_info в JSON-ответе. Без них балансировщик не сможет
// принять решение auto-reload (см. internal/balancer/llamacpp_error.go:ParseCppWorkerError):
//   - NCtxOverride — что клиент прислал в options.num_ctx (нужен для решения
//     "ignore override and reload to required" в DecideReloadBackend/code 3).
//   - MaxVRAMNCtx — оценочный максимум n_ctx для текущей VRAM (0 = unknown).
//     Если 0 — балансер применит fallback (reject).
type PromptExceedsNCtxError struct {
	ModelName         string
	ActualTokens      int
	RequestedNPredict int
	NCtx              int
	MinNPredictFloor  int
	NCtxOverride      int
	MaxVRAMNCtx       int
}

func (e *PromptExceedsNCtxError) Error() string {
	deficit := e.ActualTokens + e.MinNPredictFloor + 1 - e.NCtx
	return fmt.Sprintf("prompt exceeds n_ctx even with minimum n_predict floor: "+
		"model=%s actual_tokens=%d n_ctx=%d min_n_predict_floor=%d deficit_tokens=%d "+
		"requested_n_predict=%d — increase n_ctx (save model profile with larger context_length and reload), "+
		"or reduce conversation history / tools[] / system prompt",
		e.ModelName, e.ActualTokens, e.NCtx, e.MinNPredictFloor, deficit, e.RequestedNPredict)
}

// clampNPredictToFitContext — уменьшает params.NPredict так, чтобы
// (actualPromptTokens + params.NPredict + 1) <= n_ctx.
//
// ВАЖНО (2026-06-22): это КРИТИЧНО для tools-запросов. Даже если клиент указал
// max_tokens=31744, при prompt с tools (например 6887 токенов) сумма
// 6887+31744+1 = 38632 > n_ctx 32768 → bridge вернёт code 3 "prompt too long".
// При tools RAM fallback отключён (ReloadDisabledForToolsError в tryRamFallbackReload),
// клиент получит HTTP 413 без шанса на retry — это выглядит как «модель
// выгрузилась», хотя на самом деле это отказ от reload для tools.
//
// Здесь мы ЗАРАНЕЕ уменьшаем params.NPredict на основе actual_prompt_tokens,
// чтобы inference прошёл без code 3. Это лучше, чем ждать ошибку и потом
// отказывать клиенту.
//
// Если actualPromptTokens+1 уже >= n_ctx, оставляем minNPredictClamp (512),
// чтобы модель хоть что-то попыталась сгенерировать (лучше короткий ответ,
// чем 0 токенов). Tokenizer неидеален, и реальный prompt может оказаться
// короче — поэтому используем 512 как минимум, а не 0.
//
// Эта функция безопасна для concurrent вызовов: только читает params.
// Вызывать НЕПОСРЕДСТВЕННО ПЕРЕД backend.Generate / backend.GenerateStream.
//
// Возвращает *PromptExceedsNCtxError, если actual_tokens + minNPredictFloor + 1 > n_ctx
// (то есть prompt сам по себе больше n_ctx даже с учётом минимума).
// В этом случае params не модифицируется — caller должен вернуть HTTP 413.
func clampNPredictToFitContext(modelName, prompt string, params *bridge.GenerationParams) error {
	if params == nil {
		return errors.New("clampNPredictToFitContext: nil params")
	}
	nCtx := params.NCtxOverride
	if nCtx <= 0 {
		// Нет NCtxOverride — не можем оценить. Пропускаем клампинг.
		return nil
	}
	if params.NPredict <= 0 {
		// Клиент не задал n_predict — оставляем как есть (default 2048 уже учтён
		// в applyCppCtxHeader для n_ctx >= 2048).
		return nil
	}

	// Считаем токены в prompt через tokenizer модели.
	actualTokens := countModelTokensByLoadedInfo(modelName, prompt)
	if actualTokens <= 0 {
		// Tokenizer не смог посчитать (модель не загружена или ошибка).
		// Грубая оценка: 1 токен ≈ 4 символа.
		actualTokens = len([]rune(prompt)) / 4
	}

	const minNPredictClamp = 512
	// Запас 1 токен под EOS-маркер.
	maxAllowedNPredict := nCtx - actualTokens - 1

	// 2026-06-24: проверяем, что prompt сам по себе влезает в n_ctx с учётом
	// минимального n_predict. Если нет — это БАГ, который раньше приводил к
	// «пустому ответу с done_reason=stop» в Cline/OpenWebUI на gemma-4 (см.
	// обсуждение корневой причины 2026-06-24). Возвращаем явную ошибку.
	if actualTokens+minNPredictClamp+1 > nCtx {
		logger.Get().Errorw("clampNPredictToFitContext: prompt exceeds n_ctx even with min floor",
			"model", modelName,
			"actual_prompt_tokens", actualTokens,
			"requested_n_predict", params.NPredict,
			"n_ctx", nCtx,
			"min_n_predict_floor", minNPredictClamp,
			"deficit_tokens", actualTokens+minNPredictClamp+1-nCtx,
			"action", "returning PromptExceedsNCtxError so caller can return HTTP 413 with clear message")
		// 2026-06-24: берём MaxVRAMNCtx из bridge.GetLastErrorInfo если есть — это
		// позволит балансировщику (см. internal/balancer/llamacpp_error.go:ParseCppWorkerError)
		// принять решение auto-reload, а не сразу возвращать 413 клиенту.
		maxVRAMNCtx := 0
		if lastErr := bridge.GetLastErrorInfo(); lastErr != nil && lastErr.MaxVRAMNCtx > 0 {
			maxVRAMNCtx = lastErr.MaxVRAMNCtx
		}
		return &PromptExceedsNCtxError{
			ModelName:         modelName,
			ActualTokens:      actualTokens,
			RequestedNPredict: params.NPredict,
			NCtx:              nCtx,
			MinNPredictFloor:  minNPredictClamp,
			NCtxOverride:      params.NCtxOverride, // что прислал клиент в options.num_ctx
			MaxVRAMNCtx:       maxVRAMNCtx,         // 0 если неизвестно — balancer применит fallback
		}
	}

	if maxAllowedNPredict < minNPredictClamp {
		// Эта ветка теперь недостижима (предыдущий if уже вернул ошибку),
		// но оставлена как defense-in-depth.
		maxAllowedNPredict = minNPredictClamp
	}

	if params.NPredict > maxAllowedNPredict {
		reductionPct := float64(params.NPredict-maxAllowedNPredict) / float64(params.NPredict) * 100.0
		logger.Get().Infow("clamping n_predict to fit n_ctx",
			"model", modelName,
			"actual_prompt_tokens", actualTokens,
			"requested_n_predict", params.NPredict,
			"clamped_n_predict", maxAllowedNPredict,
			"n_ctx", nCtx,
			"prompt_chars", len(prompt),
			"min_n_predict_floor", minNPredictClamp,
			"reduction_pct", reductionPct,
			"reason", "prevent code 3 prompt-too-long for tools/long-prompt requests")
		if reductionPct > 50.0 {
			logger.Get().Warnw("n_predict severely clamped (>50% reduction) — response may be truncated",
				"model", modelName,
				"requested_n_predict", params.NPredict,
				"clamped_n_predict", maxAllowedNPredict,
				"n_ctx", nCtx,
				"actual_prompt_tokens", actualTokens,
				"reduction_pct", reductionPct,
				"advice", "consider increasing n_ctx or reducing conversation history")
		}
		params.NPredict = maxAllowedNPredict
		// Сохраняем флаг для добавления warning в ответ
		params.ClampedNPredict = true
		params.ClampedNPredictOriginal = params.NPredict // уже заменено, сохраняем maxAllowedNPredict как новый
	}
	return nil
}

// ============================================================
// RAM fallback
// ============================================================

// generateWithRamFallback пытается выполнить backend.Generate; если
// получает ErrCodeNCtxNeedsReload или ErrCodeGPUOOM и ram-fallback включён —
// перезагружает модель с запрошенным n_ctx через mmap/RAM и повторяет генерацию.
// Используется для non-streaming эндпоинтов.
// Если concurrent-запрос попадает на модель, которая сейчас перезагружается
// (reloadInProgress), он ждёт завершения reload и повторяет попытку вместо
// возврата "model not loaded" клиенту.
//
// Параметр hasTools=true отключает reload для tools-запросов: при tools каждая
// итерация диалога накапливает history, и reload не поможет — на следующей
// итерации prompt снова переполнит n_ctx. Лучше сразу вернуть ошибку.
//
// ВАЖНО (2026-06-22): вызываем clampNPredictToFitContext ПЕРЕД первым Generate.
// Это уменьшает params.NPredict так, чтобы prompt гарантированно влез в n_ctx,
// даже если reload для tools отключён. Без этого клиент получает code 3 /
// HTTP 413 при tools-сценариях, что воспринимается как «модель выгрузилась».
func generateWithRamFallback(modelName, prompt string, params bridge.GenerationParams, hasTools bool) (*bridge.InferenceResult, error) {
	// Превентивный клампинг n_predict: даже если reload при tools отключён,
	// мы можем уменьшить n_predict так, чтобы prompt влез в n_ctx.
	// 2026-06-24: если prompt>n_ctx даже с учётом min floor — возвращаем ошибку,
	// а не молча клампим до 512 (что приводило к пустому ответу в Cline).
	if clampErr := clampNPredictToFitContext(modelName, prompt, &params); clampErr != nil {
		return nil, clampErr
	}
	result, err := backend.Generate(modelName, prompt, params)
	if err == nil {
		// Успешный инференс без n_ctx ошибки — сбрасываем счётчик reload-попыток,
		// чтобы следующий overflow мог снова триггернуть reload (новое окно).
		resetReloadAttempts(modelName)
		return result, nil
	}
	// Если reload уже идёт — ждём и повторяем
	if waitReloadInProgress(modelName) {
		logger.Get().Debugw("RAM fallback: waiting for concurrent reload to complete before retry",
			"model", modelName)
		return backend.Generate(modelName, prompt, params)
	}
	if !isGpuOomOrNCtxNeedsReload() || params.NCtxOverride <= 0 {
		return result, err
	}
	if ok, fbErr := tryRamFallbackReload(modelName, params.NCtxOverride, hasTools); !ok {
		return result, err // возвращаем исходную ошибку; fbErr только логируем
	} else if fbErr != nil {
		logger.Get().Warnw("RAM fallback declined", "model", modelName, "error", fbErr)
		return result, err
	}
	result, err = backend.Generate(modelName, prompt, params)
	if err == nil {
		// Успешный retry после reload — тоже сбрасываем счётчик.
		resetReloadAttempts(modelName)
	}
	return result, err
}

// generateStreamWithRamFallback — аналог generateWithRamFallback для streaming.
// При ErrCodeNCtxNeedsReload или ErrCodeGPUOOM на старте (pre-flight) перезагружает
// модель и запускает стрим заново. Если стрим уже частично начался, fallback не
// применяется (вернётся текущая ошибка).
//
// Параметр hasTools=true отключает reload (см. generateWithRamFallback).
// ВАЖНО (2026-06-22): вызываем clampNPredictToFitContext ПЕРЕД стримом — иначе bridge
// упадёт в первом же prefill-токене при tools/long-prompt запросах.
func generateStreamWithRamFallback(modelName, prompt string, params bridge.GenerationParams, callback bridge.StreamCallback, hasTools bool) error {
	// Превентивный клампинг n_predict (для streaming тоже критично — иначе bridge
	// упадёт в первом же prefill-токене).
	// 2026-06-24: если prompt>n_ctx даже с учётом min floor — возвращаем ошибку.
	if clampErr := clampNPredictToFitContext(modelName, prompt, &params); clampErr != nil {
		return clampErr
	}
	err := backend.GenerateStream(modelName, prompt, params, callback)
	if err == nil {
		// Успешный стрим без n_ctx ошибки — сбрасываем счётчик reload-попыток.
		resetReloadAttempts(modelName)
		return nil
	}
	// Если reload уже идёт — ждём и повторяем
	if waitReloadInProgress(modelName) {
		logger.Get().Debugw("RAM fallback: waiting for concurrent reload to complete before stream retry",
			"model", modelName)
		err = backend.GenerateStream(modelName, prompt, params, callback)
		if err == nil {
			resetReloadAttempts(modelName)
		}
		return err
	}
	if !isGpuOomOrNCtxNeedsReload() || params.NCtxOverride <= 0 {
		return err
	}
	if ok, fbErr := tryRamFallbackReload(modelName, params.NCtxOverride, hasTools); !ok {
		return err
	} else if fbErr != nil {
		logger.Get().Warnw("RAM fallback declined", "model", modelName, "error", fbErr)
		return err
	}
	err = backend.GenerateStream(modelName, prompt, params, callback)
	if err == nil {
		// Успешный retry после reload — тоже сбрасываем счётчик.
		resetReloadAttempts(modelName)
	}
	return err
}

// effectiveRamFallbackGPULayers возвращает целевое число GPU-слоёв для
// RAM fallback: если пользователь явно задал ram-fallback-gpu-layers >= 0,
// используем его; иначе сохраняем текущее значение (оставляем llama.cpp
// решать, но mmap позволит вытеснить часть в RAM при нехватке VRAM).
func effectiveRamFallbackGPULayers(current int) int {
	if *ramFallbackGpuLayers >= 0 {
		return *ramFallbackGpuLayers
	}
	return current
}

// tryRamFallbackReload пытается перезагрузить модель с запрошенным n_ctx,
// используя RAM через mmap, если VRAM недостаточна. Вызывается только
// после ErrCodeNCtxNeedsReload и только если ram-fallback-n-ctx включён.
// Возвращает (ok=true, nil) если модель успешно перезагружена.
//
// Защита от reload-loop: если для этой модели в скользящем окне
// ramFallbackCycleResetInterval уже было выполнено ramFallbackMaxAttempts
// reload-попыток, возвращается *ReloadLoopLimitError. Caller (handler)
// транслирует его в HTTP 413 с понятным сообщением для клиента.
// ErrReloadDisabledForTools возвращается из tryRamFallbackReload, когда
// запрошен reload для tools-запроса. Reload при tools бесполезен — следующая
// итерация диалога принесёт ещё больше токенов (tool results + history), и
// overflow повторится. Вместо reload лучше сразу вернуть 413 с actionable
// советом (уменьшить tools/history или увеличить n_ctx в профиле).
type ReloadDisabledForToolsError struct {
	Model string
}

func (e *ReloadDisabledForToolsError) Error() string {
	return fmt.Sprintf("RAM fallback reload disabled for tools-request on model %q: "+
		"reload would not help because each chat iteration adds tool results to context. "+
		"Reduce the number of tools, chat history length, or increase n_ctx in the model profile.",
		e.Model)
}

// isReloadDisabledForToolsError — true, если err это *ReloadDisabledForToolsError.
func isReloadDisabledForToolsError(err error) bool {
	if err == nil {
		return false
	}
	_, ok := err.(*ReloadDisabledForToolsError)
	return ok
}

func tryRamFallbackReload(modelName string, requestedNCtx int, hasTools bool) (bool, error) {
	if !*ramFallbackNCtx {
		return false, nil
	}
	if requestedNCtx <= 0 {
		return false, fmt.Errorf("requested n_ctx must be > 0 for RAM fallback")
	}
	if *ramFallbackMaxNCtx > 0 && requestedNCtx > *ramFallbackMaxNCtx {
		return false, fmt.Errorf("requested n_ctx=%d exceeds ram-fallback-max-n-ctx=%d", requestedNCtx, *ramFallbackMaxNCtx)
	}

	// Шаг 0 (Шаг 4 фикса): при tools-запросах reload по умолчанию отключён —
	// НО с 2026-06-23 для динамической подстройки n_ctx под Cline/OpenWebUI
	// мы разрешаем reload при tools, если глобальный флаг
	// tryRamFallbackReloadAllowTools включён. Balancer через preflight
	// уже оценил VRAM/model_max и решил, что reload влезет; cppworker
	// просто исполняет решение.
	//
	// Если локально флаг выключен (env CPPWORKER_RAM_FALLBACK_ALLOW_TOOLS=false),
	// возвращаем ReloadDisabledForToolsError — для совместимости со
	// старым поведением (operator override).
	if hasTools && !tryRamFallbackReloadAllowTools {
		currentNCtx := 0
		currentInfo, infoErr := backend.GetModel(modelName)
		if infoErr == nil {
			currentNCtx = currentInfo.ContextSize
		}
		bridgeCode := 0
		if lastErr := bridge.GetLastErrorInfo(); lastErr != nil {
			bridgeCode = lastErr.Code
		}
		capRatio := 0.0
		if currentNCtx > 0 {
			capRatio = float64(requestedNCtx) / float64(currentNCtx)
		}
		logger.Get().Warnw("RAM fallback: reload disabled for tools-request (flag off)",
			"model", modelName,
			"reason", "tryRamFallbackReloadAllowTools=false; reduce tools or increase n_ctx in profile",
			"requested_n_ctx", requestedNCtx,
			"current_n_ctx", currentNCtx,
			"cap_ratio", capRatio,
			"bridge_last_error_code", bridgeCode,
			"advice", "reduce tools count / chat history, or increase n_ctx in model profile, "+
				"or enable --ram-fallback-allow-tools")
		return false, &ReloadDisabledForToolsError{Model: modelName}
	}
	if hasTools && tryRamFallbackReloadAllowTools {
		logger.Get().Infow("RAM fallback: tools-request reload allowed (flag on)",
			"model", modelName,
			"requested_n_ctx", requestedNCtx,
			"advice", "balancer preflight evaluated VRAM/model_max; reload should fit")
	}

	// Cycle-limit check: если уже было слишком много reload-попыток за последнее
	// окно — отказываем в reload, чтобы не попасть в бесконечный unload+reload цикл.
	if count, isLimit := getReloadAttempts(modelName); isLimit {
		stateRaw, _ := ramFallbackAttempts.Load(modelName)
		var elapsed time.Duration
		if state, ok := stateRaw.(*ramFallbackAttemptState); ok && state != nil {
			state.mu.Lock()
			if !state.firstAttemptTime.IsZero() {
				elapsed = time.Since(state.firstAttemptTime)
			}
			state.mu.Unlock()
		}
		logger.Get().Errorw("RAM fallback: cycle limit reached, refusing reload",
			"model", modelName,
			"attempts", count,
			"elapsed", elapsed.String(),
			"max_attempts", ramFallbackMaxAttempts,
			"reset_window", ramFallbackCycleResetInterval.String())
		return false, &ReloadLoopLimitError{
			Model:   modelName,
			Count:   count,
			Elapsed: elapsed,
		}
	}
	// Регистрируем попытку ДО начала reload — если что-то пойдёт не так,
	// следующий запрос увидит инкремент и не войдёт в бесконечный цикл.
	recordReloadAttempt(modelName)

	current, err := backend.GetModel(modelName)
	if err != nil {
		return false, fmt.Errorf("cannot get current model info: %w", err)
	}
	modelPath := current.Path
	if modelPath == "" {
		mm := backend.ModelManager()
		if mm != nil {
			foundPath, ferr := mm.FindModelByPath(modelName)
			if ferr == nil {
				modelPath = foundPath
			}
		}
	}
	if modelPath == "" {
		return false, fmt.Errorf("cannot resolve model path for %s", modelName)
	}

	newGPULayers := effectiveRamFallbackGPULayers(current.GPULayers)
	opts := cppbackend.LoadModelOpts{
		GPULayers:     newGPULayers,
		ContextSize:   requestedNCtx,
		BatchSize:     current.BatchSize,
		FlashAttnType: current.FlashAttnType,
		NUMA:          current.NUMA,
		UseMmap:       true, // RAM fallback через mmap
		TensorSplit:   current.TensorSplit,
	}

	// === AutoTuneNCtx (Issue: "Cline + 20GB GPU, есть RAM, но VRAM не хватает") ===
	// 2026-06-25: ВСЕГДА активен при RAM-fallback reload (даже без --auto-tune-nctx),
	// потому что без него каскад невозможен и cppworker возвращает generic 500.
	//
	// Каскад (3 уровня):
	//   1. requestedNCtx + current gpu_layers в VRAM → use as-is.
	//   2. partial offload: уменьшаем gpu_layers, weights для остальных через mmap.
	//   3. cpu-only (gpu_layers=0) + n_ctx reduction до max viable.
	//
	// Если и 3 не влезает → возвращаем *AutoTuneError, handler транслирует в
	// HTTP 413 + structured JSON (writeInsufficientResourcesResponse).
	tuned := AutoTuneNCtx(*current, requestedNCtx)
	opts.ContextSize = tuned.RecommendedNCtx
	opts.GPULayers = tuned.RecommendedGPULayers
	opts.UseMmap = tuned.UseMmap
	logger.Get().Infow("AutoTuneNCtx: applied to RAM-fallback reload",
		"model", modelName,
		"requested_n_ctx", requestedNCtx,
		"tuned_n_ctx", tuned.RecommendedNCtx,
		"tuned_gpu_layers", tuned.RecommendedGPULayers,
		"max_viable_n_ctx", tuned.MaxViableNCtx,
		"source", tuned.Source)
	if tuned.RecommendedNCtx == 0 {
		// Не влезает даже cpu-only — возвращаем ошибку с конкретным max.
		// handleInferenceError (utils.go) транслирует в HTTP 413 + JSON.
		return false, &AutoTuneError{
			Model:          modelName,
			RequestedNCtx:  requestedNCtx,
			MaxViableNCtx:  tuned.MaxViableNCtx,
			ModelSizeBytes: int64(current.SizeBytes),
			AvailableRAMMB: availableRAMBytes() / (1024 * 1024),
		}
	}

	lockOk, lockErr := backend.TryLockLoad(modelName)
	if lockErr != nil {
		// модель уже загружена — RAM fallback тривиально успешен
		return true, nil
	}
	if !lockOk {
		// другая горутина грузит эту модель — ждём
		if backend.WaitForLoad(modelName) {
			return true, nil
		}
		return false, fmt.Errorf("RAM fallback: model is being loaded by another request, but wait failed")
	}
	defer backend.UnlockLoad(modelName)

	// Устанавливаем сигнал reloadInProgress, чтобы concurrent-запросы
	// ждали завершения перезагрузки вместо получения "model not loaded".
	// Канал закрывается в defer после успешной загрузки или rollback'а.
	reloadCh := make(chan struct{})
	reloadInProgress.Store(modelName, reloadCh)
	defer close(reloadCh)
	defer reloadInProgress.Delete(modelName)

	logger.Get().Infow("RAM fallback: reloading model with larger n_ctx",
		"model", modelName,
		"path", modelPath,
		"old_n_ctx", current.ContextSize,
		"new_n_ctx", requestedNCtx,
		"old_gpu_layers", current.GPULayers,
		"new_gpu_layers", newGPULayers,
		"use_mmap", true)

	unloadStart := time.Now()
	if err := backend.UnloadModel(modelName); err != nil {
		return false, fmt.Errorf("RAM fallback unload failed: %w", err)
	}
	logger.Get().Infow("RAM fallback: model unloaded",
		"model", modelName, "unload_ms", time.Since(unloadStart).Milliseconds())

	loadStart := time.Now()
	loadErr := backend.LoadModelWithOpts(modelName, modelPath, opts)
	if loadErr != nil {
		logger.Get().Errorw("RAM fallback: reload failed",
			"model", modelName,
			"error", loadErr,
			"attempted_ctx", opts.ContextSize,
			"attempted_gpu_layers", opts.GPULayers)

		// 2026-06-25: 2-й уровень каскада. Если AutoTuneNCtx выбрал partial
		// offload, но LoadModel всё равно упал с OOM (например, llama.cpp
		// не смог выделить KV-cache для n_ctx в VRAM даже при partial offload),
		// пробуем второй fallback: gpu_layers=0 + уменьшенный n_ctx + mmap.
		// AutoTuneNCtx уже уменьшил ContextSize до tuned.MaxViableNCtx для
		// cpu-only случая, так что мы просто форсируем GPULayers=0.
		if opts.GPULayers != 0 {
			logger.Get().Warnw("RAM fallback: 2nd cascade attempt (cpu-only + max-viable n_ctx)",
				"model", modelName,
				"previous_gpu_layers", opts.GPULayers,
				"previous_n_ctx", opts.ContextSize,
				"reason", "first load failed, attempting cpu-only fallback")
			cpuOnlyOpts := opts
			cpuOnlyOpts.GPULayers = 0
			cpuOnlyOpts.UseMmap = true
			retryErr := backend.LoadModelWithOpts(modelName, modelPath, cpuOnlyOpts)
			if retryErr == nil {
				logger.Get().Infow("RAM fallback: cpu-only load succeeded",
					"model", modelName,
					"final_n_ctx", cpuOnlyOpts.ContextSize,
					"gpu_layers", 0)
				// успех — выходим без rollback
			} else {
				logger.Get().Errorw("RAM fallback: cpu-only also failed — insufficient resources",
					"model", modelName, "first_error", loadErr, "cpu_only_error", retryErr)
				// best-effort rollback к старым параметрам
				oldOpts := cppbackend.LoadModelOpts{
					GPULayers:     current.GPULayers,
					ContextSize:   current.ContextSize,
					BatchSize:     current.BatchSize,
					FlashAttnType: current.FlashAttnType,
					NUMA:          current.NUMA,
					UseMmap:       current.UseMmap,
					TensorSplit:   current.TensorSplit,
				}
				if rollbackErr := backend.LoadModelWithOpts(modelName, modelPath, oldOpts); rollbackErr != nil {
					logger.Get().Errorw("RAM fallback: rollback failed (model no longer loaded!)",
						"model", modelName, "rollback_error", rollbackErr)
				}
				// InsufficientResourcesError с actionable details для клиента.
				vramMB := int64(0)
				if vram := freeVRAMBytes(); vram > 0 {
					vramMB = vram / (1024 * 1024)
				}
				kvCacheMB := int64(0)
				if opts.ContextSize > 0 && current.NLayers > 0 {
					kvCacheBytes := estimateKVCacheBytes(opts.ContextSize, current.NLayers, current.NEmbd, current.NHeads, current.NKvHeads)
					kvCacheMB = kvCacheBytes / (1024 * 1024)
				}
				return false, &InsufficientResourcesError{
					Model:              modelName,
					RequestedNCtx:      requestedNCtx,
					MaxViableNCtx:      tuned.MaxViableNCtx,
					AvailableVRAMMB:    vramMB,
					AvailableRAMMB:     availableRAMBytes() / (1024 * 1024),
					ModelSizeBytes:     int64(current.SizeBytes),
					KVCacheRequiredMB:  kvCacheMB,
					GPULayersAttempted: 0,
				}
			}
		} else {
			// Уже cpu-only — 2-й уровень не поможет. Возвращаем InsufficientResources.
			logger.Get().Errorw("RAM fallback: cpu-only load failed — returning InsufficientResourcesError",
				"model", modelName, "error", loadErr)
			// best-effort rollback к старым параметрам
			oldOpts := cppbackend.LoadModelOpts{
				GPULayers:     current.GPULayers,
				ContextSize:   current.ContextSize,
				BatchSize:     current.BatchSize,
				FlashAttnType: current.FlashAttnType,
				NUMA:          current.NUMA,
				UseMmap:       current.UseMmap,
				TensorSplit:   current.TensorSplit,
			}
			if rollbackErr := backend.LoadModelWithOpts(modelName, modelPath, oldOpts); rollbackErr != nil {
				logger.Get().Errorw("RAM fallback: rollback failed (model no longer loaded!)",
					"model", modelName, "rollback_error", rollbackErr)
			}
			vramMB := int64(0)
			if vram := freeVRAMBytes(); vram > 0 {
				vramMB = vram / (1024 * 1024)
			}
			kvCacheMB := int64(0)
			if opts.ContextSize > 0 && current.NLayers > 0 {
				kvCacheBytes := estimateKVCacheBytes(opts.ContextSize, current.NLayers, current.NEmbd, current.NHeads, current.NKvHeads)
				kvCacheMB = kvCacheBytes / (1024 * 1024)
			}
			return false, &InsufficientResourcesError{
				Model:              modelName,
				RequestedNCtx:      requestedNCtx,
				MaxViableNCtx:      tuned.MaxViableNCtx,
				AvailableVRAMMB:    vramMB,
				AvailableRAMMB:     availableRAMBytes() / (1024 * 1024),
				ModelSizeBytes:     int64(current.SizeBytes),
				KVCacheRequiredMB:  kvCacheMB,
				GPULayersAttempted: 0,
			}
		}
	}
	logger.Get().Infow("RAM fallback: model reloaded successfully",
		"model", modelName,
		"new_n_ctx", requestedNCtx,
		"gpu_layers", newGPULayers,
		"load_ms", time.Since(loadStart).Milliseconds())

	if balancerReg != nil {
		if info, err := backend.GetModel(modelName); err == nil {
			balancerReg.notifyModelLoaded(modelName, info.SizeBytes, info.ContextSize, info.GPULayers)
		}
	}
	return true, nil
}
