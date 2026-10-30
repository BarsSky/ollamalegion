// growth_needed_rule_r330.go — R330 (2026-10-30): ОДНО правило «сколько токенов
// окна нужно запросу».
//
// ЗАЧЕМ. Аудит маршрутов нашёл, что решение «растим окно / отказываем / ждём»
// размазано по трём guard'ам (R288 contextGrowthBlocker, R299 growthDeferR299,
// R301 growthVerdictR301), и число needed считалось ДВУМЯ формулами:
//
//   - contextGrowthNumbers (deferred_ops_r291.go): needed =
//     max(оценка промпта + бюджет вывода, явно запрошенное окно);
//   - contextGrowthBlocker (context_growth_r288.go): та же идея, но записанная
//     отдельно через promptNeedsMoreContext — с иным порядком проверок.
//
// Пока формулы совпадали по результату, расхождение было незаметно, но любая
// правка одной из них разводила вердикт и текст отказа (класс дефекта R267:
// «правка одного слоя не лечила другой»).
//
// Теперь формула живёт здесь одна, а оба места её вызывают. Функция чистая — её
// можно проверять без Proxy, бэкендов и HTTP.
package balancer

import "fmt"

// neededWindowTokensR330 — сколько токенов окна нужно запросу, чтобы обслужить
// его целиком.
//
// Правило (одно и то же для вердикта, для текста отказа и для логов):
//   - по промпту: оценка промпта + бюджет вывода (agentOutputBudget);
//   - по клиенту: явно запрошенное окно (RequestedNCtxOverride);
//   - берётся максимум: клиент вправе просить больше, чем нужно промпту.
//
// 0 означает «чисел нет» (нет ни оценки промпта, ни явного окна) — вызывающий
// не должен строить на этом отказ.
func neededWindowTokensR330(meta *RequestMeta) int {
	if meta == nil {
		return 0
	}
	byPrompt := 0
	if meta.EstimatedPromptTokens > 0 {
		byPrompt = meta.EstimatedPromptTokens + agentOutputBudget(meta)
	}
	if requested := meta.RequestedNCtxOverride; requested > byPrompt {
		return requested
	}
	return byPrompt
}

// growthVerdictTextR330 — человекочитаемая строка тех же чисел (для логов и
// текстов отказов), чтобы в трёх местах не собирались три разных формата.
func growthVerdictTextR330(needed, loaded int) string {
	return fmt.Sprintf("needed=%d tokens, loaded window=%d tokens", needed, loaded)
}
