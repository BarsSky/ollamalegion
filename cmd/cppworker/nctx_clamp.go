// nctx_clamp.go — Phase D.3-fix + D.6: применение политики n_ctx от балансировщика
// (через X-Cpp-Ctx header) к GenerationParams, плюс антипромпты и model-family helpers.
//
// ВЫНЕСЕНО ИЗ main.go (Phase refactoring-2026-06-09) для лучшей тестируемости и
// переиспользования в /api/generate, /api/chat, /v1/chat/completions, /v1/completions.
//
// Связанная документация:
//   - docs/plans/archive/test-report-2026-06-09-cppworker-clamping.md
//   - docs/plans/archive/cppworker-routing-fixes-2026-06-07.md (Phase D.3-fix оригинал)
package main

import (
	"net/http"
	"strconv"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/internal/modelfamily"
	"ollama-loadbalancer/pkg/logger"
)

// ApplyCppCtxHeader — применяет X-Cpp-Ctx header (от балансировщика) к params.
//
// Семантика Phase D.3-fix: header — это UPPER LIMIT (потолок), заданный
// профилем модели или defaultProfile. Если body задал num_ctx > header,
// params.NCtxOverride клампится к header (защита от ситуации, когда клиент
// вроде OpenWebUI шлёт options.num_ctx=16384, а VRAM бэкенда не позволяет
// больше 4096 — без clamp'а cppworker вернёт
// "requested n_ctx=16384 exceeds model's effective n_ctx=4096").
//
// Если body задал num_ctx <= header или не задал вовсе — header не меняет
// body num_ctx (т.е. клиент может запросить МЕНЬШЕ, чем потолок — это
// допустимо). Политика балансировщика НЕ повышает num_ctx — только ограничивает.
//
// Phase D.6: одновременно подгоняет default n_predict, чтобы сумма
// prompt+n_predict+1 гарантированно не превысила n_ctx. Иначе cppworker использует
// default NPredict (из bridge.DefaultGenerationParams), и при n_ctx=2048 короткий
// prompt OpenWebUI (~28 токенов) уже не проходит: 28+2048+1=2077 > 2048 →
// bridge code 3: prompt too long → stream-чанк с пустым content. Без этого
// фикса OpenWebUI получает «пустой ответ».
//
// Phase D.8 (regression fix): предыдущая версия сравнивала params.NPredict с
// hardcoded defaultNPredict=4096 и заменяла только при NPredict >= 4096.
// После D.6 bridge.DefaultGenerationParams().NPredict был уменьшен до 2048 —
// условие NPredict >= 4096 стало ВСЕГДА ложным, и NPredict ни разу не
// клампился → регрессия D.8 (пустой ответ OpenWebUI при n_ctx=2048).
// Исправление: используем bridge.DefaultGenerationParams().NPredict как
// reference default вместо hardcoded 4096.
//
// Стратегия: ставим NPredict = n_ctx / 2 (если NPredict равен reference default
// из bridge — значит клиент не задал своё значение, можно безопасно заменить).
// Это даёт n_ctx/2 токенов на prompt, что покрывает OpenWebUI-сценарии
// (system + 10-20 туров диалога = обычно < 2-3K токенов). Если клиент явно
// задал NPredict в body (MaxTokens/NumPredict) — buildGenerationParams уже
// установил params.NPredict отличным от reference default — оставляем его.
//
// Параметры:
//   - r: HTTP-запрос с возможным X-Cpp-Ctx header
//   - params: GenerationParams, в который будет записан n_ctx override (если
//     применимо) и скорректирован NPredict
//
// Возвращает: ничего (мутирует params in-place; логирует через zap).
// CppCtxApplyOptions — опциональные параметры ApplyCppCtxHeader.
// Используем opt-in pattern (HasTools, ToolsPromptReserveTokens), чтобы не
// ломать существующий API для вызовов без tools.
type CppCtxApplyOptions struct {
	// HasTools — true, если запрос содержит tool definitions (OpenAI tools,
	// function calling). В этом случае резерв под prompt увеличивается.
	HasTools bool
	// ToolsPromptReserveTokens — сколько токенов зарезервировать под tools-prompt.
	// Используется только если HasTools=true. По умолчанию (если 0) —
	// toolsPromptDefaultReserveTokens.
	ToolsPromptReserveTokens int
}

const (
	// toolsPromptDefaultReserveTokens — базовый резерв токенов под tools-prompt.
	// Подобран эмпирически: buildToolsSystemPrompt для 5-10 tools даёт
	// 800-1500 символов ~ 300-600 токенов, плюс user history в OpenWebUI обычно
	// занимает 1-2K токенов. Итого 2-3K — берём с запасом.
	toolsPromptDefaultReserveTokens = 2048
	// basePromptReserveTokens — базовый резерв токенов под prompt без tools.
	// Используется при расчёте maxPredict: n_ctx - reserve.
	basePromptReserveTokens = 1024
	// minNPredict — нижний предел NPredict (даже при n_ctx=2048).
	minNPredict = 512
)

func ApplyCppCtxHeader(r *http.Request, params *bridge.GenerationParams) {
	// Backwards-compat wrapper: применяем стандартную логику без tools-reserve.
	ApplyCppCtxHeaderWithOptions(r, params, CppCtxApplyOptions{})
}

// ApplyCppCtxHeaderWithOptions — расширенная версия с учётом tools reserve.
// Если opts.HasTools=true, резервирует больше места под tools-prompt и историю,
// чтобы избежать n_ctx overflow → reload loop.
func ApplyCppCtxHeaderWithOptions(r *http.Request, params *bridge.GenerationParams, opts CppCtxApplyOptions) {
	s := r.Header.Get("X-Cpp-Ctx")
	if s == "" {
		return
	}
	headerLimit, err := strconv.Atoi(s)
	if err != nil || headerLimit <= 0 {
		return
	}
	// Шаг 1: верхний предел для n_ctx (Phase D.3-fix, semantics UPPER LIMIT).
	if params.NCtxOverride <= 0 {
		// body не задал num_ctx — header используется как дефолт-политика балансировщика
		params.NCtxOverride = headerLimit
	} else if params.NCtxOverride > headerLimit {
		// body задал num_ctx > header — клампим (Phase D.3-fix)
		logger.Get().Warnw("applyCppCtxHeader: clamping body num_ctx to balancer header limit",
			"body_n_ctx", params.NCtxOverride, "header_limit", headerLimit)
		params.NCtxOverride = headerLimit
	}

	// Шаг 2: рассчитываем резерв токенов под prompt.
	// Если есть tools — увеличенный резерв, чтобы system+tools+history не вытеснили вывод.
	promptReserve := basePromptReserveTokens
	if opts.HasTools {
		promptReserve = opts.ToolsPromptReserveTokens
		if promptReserve <= 0 {
			promptReserve = toolsPromptDefaultReserveTokens
		}
		logger.Get().Debugw("applyCppCtxHeader: tools-aware prompt reserve",
			"reserve_tokens", promptReserve, "n_ctx", params.NCtxOverride)
	}

	// Шаг 3: ограничиваем default NPredict, чтобы он гарантированно помещался
	// в n_ctx ВМЕСТЕ с prompt любого разумного размера.
	//
	// Эвристика: NPredict = n_ctx - promptReserve.
	// Минимум: minNPredict токенов на вывод.
	maxPredict := params.NCtxOverride - promptReserve
	if maxPredict < minNPredict {
		maxPredict = minNPredict // нижний предел
	}

	// D.8 fix: используем reference default из bridge вместо hardcoded 4096.
	referenceDefault := bridge.DefaultGenerationParams().NPredict
	if params.NPredict >= referenceDefault {
		logger.Get().Debugw("applyCppCtxHeader: replacing default n_predict with n_ctx-reserve",
			"old_n_predict", params.NPredict,
			"new_n_predict", maxPredict,
			"n_ctx", params.NCtxOverride,
			"prompt_reserve", promptReserve,
			"has_tools", opts.HasTools,
			"reference_default", referenceDefault)
		params.NPredict = maxPredict
	}
}

// defaultAntipromptsForModel — стоп-последовательности модели.
//
// R268 (2026-10-28): ОТКУДА БЕРУТСЯ ЗНАЧЕНИЯ. Раньше здесь был switch с
// семействами (gemma-4 / gemma / qwen / прочее) — то есть знание о семействе
// жило в файле про n_ctx, а обновление семейства требовало помнить про это
// место. Теперь набор приходит из реестра семейств (`internal/modelfamily`):
// у gemma/qwen он свой (с теми же строками и теми же объяснениями, включая
// R248-правку про "<|im_end|>"), у остальных — общий набор «по умолчанию».
//
// ПОВЕДЕНИЕ НЕ ИЗМЕНИЛОСЬ: строки перенесены дословно, это подтверждено
// тестами antiprompt_r248_test.go (характеризационными) и table-тестом
// modelfamily.
//
// Защита от race: если модель не имеет training-своего stop-token в списке,
// llama.cpp сам остановит на n_predict. Поэтому добавляем несколько запасных.
func defaultAntipromptsForModel(modelName string) []string {
	if stops := modelfamily.StopStringsFor(modelName); len(stops) > 0 {
		return stops
	}
	// Реестр не дал набора (не должно случаться: generic всегда совпадает) —
	// не оставляем модель без антипромптов вовсе.
	return []string{"<|end|>", "<|user|>", "<|assistant|>"}
}

// isGemmaModel — true, если модель относится к семейству gemma.
//
// R268: спрашиваем реестр семейств, а не строку имени. Разница содержательная:
// раньше "gemma" в имени решало за нас всё (например, попадание в ветку
// antiprompts), теперь семейство — единственный источник, и его id виден в логах.
func isGemmaModel(modelName string) bool {
	f := modelfamily.Of(modelName)
	return f != nil && f.ID() == "gemma"
}

// ============================================================
// Round 26 v0.5.13: n_ctx overflow detection + X-Model-Context-Warning
// ============================================================
//
// Зачем: пользователь жалуется, что в OpenWebUI при длинной беседе
// ответ обрывается (cutoff). Возможные причины:
//   1. n_ctx overflow → prompt > n_ctx → llama.cpp errors/relods
//   2. StreamingIdleTimeout (balancer, 120s) — пауза между токенами > 2min
//   3. StreamTimeout (balancer, 600s) — total stream > 10min
//   4. cppworker WriteTimeout (30min) — последний рубеж
//
// Этот файл добавляет preflight detection для #1: если prompt_tokens +
// n_predict + slack > loaded_n_ctx, ставим X-Model-Context-Warning header
// с уровнем ("approaching" / "exceeded") И обновляем n_predict чтобы
// избежать overflow в принципе.
//
// Уровни предупреждения:
//   - "ok"             : prompt < 60% of n_ctx, room для 8K+ output
//   - "approaching"    : prompt > 80% of n_ctx, output будет урезан
//   - "overflow"       : prompt + n_predict > n_ctx, БУДЕТ overflow
//   - "impossible"     : prompt > n_ctx, overflow прямо сейчас

// contextWarningLevel — уровень предупреждения для X-Model-Context-Warning.
type contextWarningLevel string

const (
	contextWarnOK         contextWarningLevel = "ok"
	contextWarnApproach   contextWarningLevel = "approaching" // prompt > 80% of n_ctx
	contextWarnOverflow   contextWarningLevel = "overflow"    // prompt + n_predict > n_ctx
	contextWarnImpossible contextWarningLevel = "impossible"  // prompt > n_ctx
)

// contextWarningThreshold — пороги в % от n_ctx.
const (
	contextWarnApproachPercent = 80 // > 80% → approaching
)

// ContextWarning — детали предупреждения для клиента.
type ContextWarning struct {
	Level           contextWarningLevel `json:"level"`
	NCtx            int                 `json:"nCtx"`            // loaded n_ctx
	GGUFContextMax  int                 `json:"ggufContextMax"`  // model's max n_ctx per GGUF
	PromptTokens    int                 `json:"promptTokens"`    // фактический размер prompt в токенах
	NPredict        int                 `json:"nPredict"`        // запрошенный n_predict
	EffectiveMaxOut int                 `json:"effectiveMaxOut"` // сколько токенов доступно на вывод после prompt
	UsedPercent     int                 `json:"usedPercent"`     // prompt / n_ctx в %
	Suggestion      string              `json:"suggestion"`      // человекочитаемая рекомендация
}

// ComputeContextWarning — рассчитывает уровень предупреждения для prompt + params.
// Возвращает (ContextWarning, adjustedNPredict). Если adjustedNPredict != inputNPredict,
// значит мы урезали n_predict чтобы влезть в n_ctx. Caller должен использовать
// adjustedNPredict вместо inputNPredict.
//
// Параметры:
//   - modelName: имя модели
//   - prompt: финальный prompt (после chat template)
//   - nPredict: запрошенный n_predict (max tokens для генерации)
//   - nCtxOverride: n_ctx из header/body (0 = использовать loaded)
//
// Не делает HTTP вызовов, не меняет r — чистая функция для тестирования.
func ComputeContextWarning(modelName, prompt string, nPredict, nCtxOverride int) (ContextWarning, int) {
	loaded, _ := backend.GetModel(modelName)
	loadedNCtx := 0
	ggufMax := 0
	if loaded != nil {
		loadedNCtx = loaded.ContextSize
		ggufMax = loaded.GGUFContextLength
	}
	return computeContextWarningFromLoaded(modelName, prompt, nPredict, nCtxOverride, loadedNCtx, ggufMax)
}

// computeContextWarningFromLoaded — pure-function для тестирования.
// Все side-effects (GetModel, CountTokens) изолированы в вызывающем коде.
func computeContextWarningFromLoaded(modelName, prompt string, nPredict, nCtxOverride, loadedNCtx, ggufMax int) (ContextWarning, int) {
	// Effective n_ctx = override из body/header, иначе loaded n_ctx
	effectiveNCtx := loadedNCtx
	if nCtxOverride > 0 {
		effectiveNCtx = nCtxOverride
	}
	if effectiveNCtx <= 0 {
		// Без loaded info ничего не можем сказать. Возвращаем OK.
		return ContextWarning{Level: contextWarnOK}, nPredict
	}

	// Подсчёт токенов через llama.cpp
	promptTokens := countModelTokensByLoadedInfo(modelName, prompt)
	usedPercent := (promptTokens * 100) / effectiveNCtx

	// Уровень "impossible" — prompt уже больше n_ctx
	if promptTokens >= effectiveNCtx {
		suggestion := "Prompt exceeds n_ctx. Use a model profile with larger n_ctx, or reduce conversation length."
		return ContextWarning{
			Level:           contextWarnImpossible,
			NCtx:            effectiveNCtx,
			GGUFContextMax:  ggufMax,
			PromptTokens:    promptTokens,
			NPredict:        nPredict,
			EffectiveMaxOut: 0,
			UsedPercent:     usedPercent,
			Suggestion:      suggestion,
		}, 0 // n_predict=0 — нельзя ничего генерировать
	}

	// Сколько токенов доступно на вывод (с 64-токеновым slack для safety margin)
	const safetySlack = 64
	maxOut := effectiveNCtx - promptTokens - safetySlack
	if maxOut < 0 {
		maxOut = 0
	}

	// Adjust n_predict если он не влезает
	adjustedNPredict := nPredict
	if adjustedNPredict > maxOut {
		adjustedNPredict = maxOut
	}

	// Уровень "overflow" — даже adjusted n_predict может быть 0
	if adjustedNPredict <= 0 {
		return ContextWarning{
			Level:           contextWarnOverflow,
			NCtx:            effectiveNCtx,
			GGUFContextMax:  ggufMax,
			PromptTokens:    promptTokens,
			NPredict:        nPredict,
			EffectiveMaxOut: maxOut,
			UsedPercent:     usedPercent,
			Suggestion:      "Prompt too large for n_ctx. Save a model profile with larger n_ctx.",
		}, 0
	}

	// Уровень "approaching" — prompt > 80% of n_ctx
	if usedPercent >= contextWarnApproachPercent {
		return ContextWarning{
			Level:           contextWarnApproach,
			NCtx:            effectiveNCtx,
			GGUFContextMax:  ggufMax,
			PromptTokens:    promptTokens,
			NPredict:        adjustedNPredict,
			EffectiveMaxOut: maxOut,
			UsedPercent:     usedPercent,
			Suggestion:      "Prompt is using >80% of n_ctx. Output will be truncated or overflow imminent.",
		}, adjustedNPredict
	}

	return ContextWarning{
		Level:           contextWarnOK,
		NCtx:            effectiveNCtx,
		GGUFContextMax:  ggufMax,
		PromptTokens:    promptTokens,
		NPredict:        adjustedNPredict,
		EffectiveMaxOut: maxOut,
		UsedPercent:     usedPercent,
	}, adjustedNPredict
}

// SetContextWarningHeader — добавляет X-Model-Context-Warning и X-Model-Adjusted-NPredict
// headers если warning level != "ok". Используется в /v1/chat, /api/chat, /api/generate.
//
// Call после ComputeContextWarning. Если level == "ok", headers не ставим (clean response).
func SetContextWarningHeader(w http.ResponseWriter, warning ContextWarning) {
	if warning.Level == contextWarnOK {
		return
	}
	// X-Model-Context-Warning: level | nCtx | usedPercent | nPredict
	// Compact format: "approaching;nCtx=16384;pct=85;out=2400"
	headerVal := string(warning.Level)
	if warning.NCtx > 0 {
		headerVal += ";nCtx=" + strconv.Itoa(warning.NCtx)
	}
	if warning.UsedPercent > 0 {
		headerVal += ";pct=" + strconv.Itoa(warning.UsedPercent)
	}
	if warning.EffectiveMaxOut > 0 {
		headerVal += ";out=" + strconv.Itoa(warning.EffectiveMaxOut)
	}
	if warning.GGUFContextMax > 0 && warning.GGUFContextMax != warning.NCtx {
		headerVal += ";ggufMax=" + strconv.Itoa(warning.GGUFContextMax)
	}
	w.Header().Set("X-Model-Context-Warning", headerVal)
	if warning.Suggestion != "" {
		w.Header().Set("X-Model-Context-Suggestion", warning.Suggestion)
	}
	// Если мы adjusted n_predict — ставим отдельный header, чтобы клиент знал
	// почему получит меньше токенов чем просил
	if warning.NPredict > 0 {
		w.Header().Set("X-Model-Adjusted-NPredict", strconv.Itoa(warning.NPredict))
	}
}
