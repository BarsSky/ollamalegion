// nctx_clamp.go — Phase D.3-fix + D.6: применение политики n_ctx от балансировщика
// (через X-Cpp-Ctx header) к GenerationParams, плюс антипромпты и model-family helpers.
//
// ВЫНЕСЕНО ИЗ main.go (Phase refactoring-2026-06-09) для лучшей тестируемости и
// переиспользования в /api/generate, /api/chat, /v1/chat/completions, /v1/completions.
//
// Связанная документация:
//   - docs/test-report-2026-06-09-cppworker-clamping.md
//   - docs/cppworker-routing-fixes-2026-06-07.md (Phase D.3-fix оригинал)
package main

import (
	"net/http"
	"strconv"
	"strings"

	"ollama-loadbalancer/c/bridge"
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
func ApplyCppCtxHeader(r *http.Request, params *bridge.GenerationParams) {
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
	// Шаг 2: фикс D.6 + D.8 — ограничиваем default NPredict, чтобы он
	// гарантированно помещался в n_ctx ВМЕСТЕ с prompt любого разумного размера.
	//
	// Эвристика: NPredict = n_ctx - 1024. Резервируем 1024 токена на prompt
	// (system + диалог в OpenWebUI), остальное — на генерацию вывода.
	// Это значительно лучше старой эвристики n_ctx/2, при которой для
	// n_ctx=2048 оставалось всего 1024 токена на ответ — катастрофически
	// мало для длинных ответов на русском языке.
	//
	// Минимум: 2048 токенов на вывод (если n_ctx позволяет), иначе n_ctx-1024.
	// Если клиент явно задал NPredict в body (MaxTokens/NumPredict),
	// то buildGenerationParams уже установил params.NPredict отличным от
	// reference default — оставляем его значение (условие >= referenceDefault
	// будет false).
	maxPredict := params.NCtxOverride - 1024
	if maxPredict < 1024 {
		maxPredict = 1024 // нижний предел: хотя бы 1024 токена на вывод
	}
	// D.8 fix: используем reference default из bridge вместо hardcoded 4096.
	referenceDefault := bridge.DefaultGenerationParams().NPredict
	if params.NPredict >= referenceDefault {
		logger.Get().Debugw("applyCppCtxHeader: replacing default n_predict with n_ctx-1024",
			"old_n_predict", params.NPredict, "new_n_predict", maxPredict, "n_ctx", params.NCtxOverride,
			"reference_default", referenceDefault)
		params.NPredict = maxPredict
	}
}

// defaultAntipromptsForModel — возвращает дефолтный набор стоп-последовательностей
// для указанной модели, чтобы модель корректно останавливалась в конце своего хода.
// Для gemma: "<end_of_turn>" (нормальный EOS) + "<start_of_turn>user" (защита от
// ситуации, когда модель генерирует открывающий токен следующего хода вместо EOS).
// Для прочих: "<|end|>" + "<|user|>" + "<|assistant|>".
func defaultAntipromptsForModel(modelName string) []string {
	ml := strings.ToLower(modelName)
	if strings.Contains(ml, "gemma") {
		return []string{"<end_of_turn>", "<start_of_turn>user", "<start_of_turn>model"}
	}
	return []string{"<|end|>", "<|user|>", "<|assistant|>"}
}

// isGemmaModel — true, если имя модели содержит "gemma" (gemma, gemma-2, gemma-4, и т.п.).
func isGemmaModel(modelName string) bool {
	return strings.Contains(strings.ToLower(modelName), "gemma")
}