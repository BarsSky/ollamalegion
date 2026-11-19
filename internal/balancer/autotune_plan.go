// autotune_plan.go — R636 (2026-11-19): планирование применения: PlanApplyAutoTune и правила минимального контекста.
//
// ЗАЧЕМ ВЫНЕСЕНО. `autotune.go` держал 1666 строк: типы профиля, правила
// выбора KV-типа и числа слоёв, отчёт, предохранитель, трекер, две перезагрузки
// и планирование применения. Здесь — один из слоёв; поведение не менялось.
package balancer

import (
	"fmt"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
	"strings"
)

// PlanApplyAutoTune — Round 54.4 (2026-08-24): строит reload-план из AutoTuneAnalysis.
// Возвращает (nil, false) если ничего менять не нужно.
//
// Логика:
//   - n_ctx over-allocation: recommendedNumCtx = p.FeasibleMaxContext (или optimal)
//   - KV cache mismatch: рекомендуем optimal, но ТОЛЬКО если у нас size_bytes
//     (computeOptimalKVCacheType иначе возвращает "no data")
//   - num_gpu_layers: если -1 (all) и все слои влезают → keep current -1
//
// Параметр currentLoaded — текущее состояние модели (для diff).
func PlanApplyAutoTune(analysis *AutoTuneAnalysis, currentLoaded types.LlamaCppModel) *AutoTuneReloadPlan {
	return PlanApplyAutoTuneWithMinContext(analysis, currentLoaded, 0)
}

// autoTuneMinContextR425 — нижняя граница n_ctx для AutoTune-плана.
//
// Максимум из двух величин:
//   - clientRequested — сколько окна попросил клиент (R69): опускать окно ниже
//     запроса нельзя, иначе reload-пинг-понг и 503 клиенту;
//   - profileContext — окно, заявленное ОПЕРАТОРОМ в профиле модели (R425):
//     это политика, а не «овер-аллокация», и AutoTune её не оспаривает.
//
// Вынесено отдельной функцией, чтобы правило было покрыто тестом (см.
// autotune_profile_floor_r425_test.go) — формула внутри triggerAutoTuneReload
// иначе осталась бы непроверяемой.
func autoTuneMinContextR425(clientRequested, profileContext int) int {
	if profileContext > clientRequested {
		return profileContext
	}
	return clientRequested
}

// PlanApplyAutoTuneWithMinContext — R69 (2026-09-23): то же, но с учётом
// минимально необходимого n_ctx (сколько просил клиент).
//
// minContext > 0 запрещает «over-allocation fix» вниз: на живом стенде Cline с
// num_ctx=65536 получал reload 65536 → 38385 (AutoTune) → снова 65536 → …,
// каждый запрос завершался 503 «n_ctx auto-reload in progress».
func PlanApplyAutoTuneWithMinContext(
	analysis *AutoTuneAnalysis, currentLoaded types.LlamaCppModel, minContext int,
) *AutoTuneReloadPlan {
	if analysis == nil || !analysis.IsSubOptimal {
		return nil
	}

	plan := &AutoTuneReloadPlan{
		ModelName: currentLoaded.Name,
	}

	for _, rec := range analysis.Recommendations {
		switch rec.Category {
		case "context":
			// R53.2: currentLoaded.ContextLength → recommendedNumCtx
			if rec.RecommendedNumCtx <= 0 || rec.RecommendedNumCtx == currentLoaded.ContextLength {
				continue
			}
			// R416c (2026-11-03): фактическое окно ещё НЕ пришло из метрик
			// (0 = неизвестно). Анализ в этот момент строит рекомендацию «поставить
			// 16384» при неизвестном текущем — и это превращается в reload на
			// КАЖДОМ запросе: план считает ContextSize изменением, хотя окно уже
			// 16384. Живой случай: Qwen3.8-9B только что загружен, поллер ещё не
			// обновил снапшот — балансер выгружал модель после каждого ответа.
			//
			// «Не знаем текущее окно» — не повод для перезагрузки. Настоящий рост
			// (клиент просит больше) идёт другим путём — preflight n_ctx reload.
			if currentLoaded.ContextLength <= 0 {
				if logger.Get() != nil {
					logger.Get().Infow("autotune: context recommendation skipped: current n_ctx unknown (R416c)",
						"model", currentLoaded.Name,
						"recommended_n_ctx", rec.RecommendedNumCtx)
				}
				continue
			}
			// R69: уменьшать ниже клиентского запроса нельзя — рекомендация
			// построена на текущем feasible и не знает про запросы клиента.
			//
			// R521 (§8.3.4): подавление больше НЕ тихое. Живой факт (архив
			// 2026-11-18): AutoTune рекомендовал 60 114 при загруженных 131 072 и
			// запросе 131 072, подавление срабатывало молча — и клиент получал
			// гарантированное падение декода (CUDA OOM), а не отказ по памяти.
			// Теперь событие уровня Warn с числами: видно, что окно выше
			// безопасного и почему понижение не сделано.
			if minContext > 0 && rec.RecommendedNumCtx < minContext {
				logger.Get().Warnw("autotune: n_ctx downgrade suppressed by R69 — окно выше безопасного, память под угрозой",
					"model", currentLoaded.Name,
					"current_n_ctx", currentLoaded.ContextLength,
					"recommended_n_ctx", rec.RecommendedNumCtx,
					"client_requested_n_ctx", minContext,
					"feasible_max_context", currentLoaded.FeasibleMaxContext,
					"risk", "память может не хватить на compute buffer → llama_decode failed (code 1)",
					"next", "окно поднимется только явной загрузкой с бОльшим n_ctx; для безопасного окна перезагрузите модель вручную")
				continue
			}
			plan.ContextSize = rec.RecommendedNumCtx
			plan.Reason = fmt.Sprintf("R54.4: n_ctx %d → %d (over-allocation fix)",
				currentLoaded.ContextLength, rec.RecommendedNumCtx)
		case "kv_cache":
			// Если recommendedKVCache отличается от current — план
			if rec.RecommendedKVCache != "" && rec.RecommendedKVCache != currentLoaded.KvCacheType {
				plan.KVCacheType = rec.RecommendedKVCache
				if plan.Reason == "" {
					plan.Reason = fmt.Sprintf("R54.4: kv_cache %s → %s (quality fix)",
						currentLoaded.KvCacheType, rec.RecommendedKVCache)
				}
			}
		case "layers":
			// R229 (2026-10-22): -2 («auto offload») — не ЦЕЛЬ reload'а, а режим,
			// в котором воркер сам решает раскладку. После перезагрузки с -2 воркер
			// сообщает ФАКТ (реальное число слоёв, например 20), анализ снова видит
			// «рекомендуется -2, а стоит 20» и выдаёт тот же план — получается серия
			// одинаковых reload'ов (AutoTune «включает авто» снова и снова), пока не
			// сработает circuit breaker. Живой симптом: модель перезагружается без
			// видимой причины, клиент получает 503.
			//
			// Поэтому -2 в план НЕ попадает никогда: auto — это настройка профиля/
			// панели, а не решение автоматики. Остальные рекомендации слоёв
			// (конкретное число) применяются как раньше.
			if rec.RecommendedNumGPULayers == -2 {
				if logger.Get() != nil {
					logger.Get().Infow(i18n.T("balancer.autotune.layers_auto_no_reload_info"),
						"model", currentLoaded.Name,
						"current_gpu_layers", currentLoaded.NumGPULayers,
						"reason", i18n.T("balancer.autotune.layers_auto_no_reload_reason"))
				}
				continue
			}
			// R416b (2026-11-03): -1 («все слои») — тоже НЕ цель reload'а, если
			// модель уже полностью на GPU. Воркер сообщает ФАКТ (32 из 32 слоёв
			// → например 33), анализ видит «рекомендуется -1, а стоит 33» и
			// строит тот же план снова и снова. Живой случай: Qwen3.8-9B после
			// R415 с запасом VRAM 512 MB загрузился всеми слоями — и балансер
			// выгружал/загружал его на каждом запросе, хотя KV уже был отклонён
			// гардом R416 (в плане оставался только -1).
			//
			// Частичный offload (20 из 32) по-прежнему получает план -1: там
			// «все слои» — реальная цель, а не факт.
			if rec.RecommendedNumGPULayers == -1 && currentLoaded.NLayers > 0 &&
				currentLoaded.NumGPULayers >= currentLoaded.NLayers {
				if logger.Get() != nil {
					logger.Get().Infow("autotune: layers=-1 skipped: model is already fully offloaded (R416b)",
						"model", currentLoaded.Name,
						"current_gpu_layers", currentLoaded.NumGPULayers,
						"n_layers", currentLoaded.NLayers)
				}
				continue
			}
			if rec.RecommendedNumGPULayers != 0 && rec.RecommendedNumGPULayers != currentLoaded.NumGPULayers {
				plan.NumGPULayers = rec.RecommendedNumGPULayers
				if plan.Reason == "" {
					plan.Reason = fmt.Sprintf("R54.4: gpu_layers %d → %d",
						currentLoaded.NumGPULayers, rec.RecommendedNumGPULayers)
				}
			}
		}
	}

	if !plan.NeedsReload() {
		return nil
	}

	// R83 (2026-09-29): если reload запускается НЕ ради контекста (kv_cache или
	// layers), контекст обязан остаться фактически загруженным.
	//
	// ДЕФЕКТ, который это закрывает (живой лог 2026-09-29): план менял только
	// kv_cache f16 → q4_0, ContextSize оставался 0, ApplyAutoTunePlan при нуле НЕ
	// кладёт contextSize в запрос, и cppworker поднимал модель со своим
	// env-дефолтом (CPPWORKER_CTX_SIZE=32768) вместо загруженных 131072. Оператор
	// видел «после запроса от Cline модель принудительно сброшена до 32768», хотя
	// Cline просил 128000; дальше каждый запрос получал code 2 (n_ctx exceeds).
	//
	// Ставим значение ТОЛЬКО если рекомендация «context» ничего не предложила
	// (ContextSize == 0), и только после проверки NeedsReload, чтобы «план без
	// изменений» по-прежнему возвращал nil (это контракт PlanApplyAutoTune).
	if plan.ContextSize <= 0 {
		plan.ContextSize = currentLoaded.ContextLength
	}
	return plan
}

// _ = strings.Contains — keep import
var _ = strings.Contains
