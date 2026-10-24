// nctx_reload_adaptive.go — адаптивная стратегия reload через cppworker /adaptive/strategy API.
// Добавляет запрос стратегии в DoReload: kvCacheType, gpuLayers, n_ctx подбираются
// cppworker'ом на основе реальной VRAM и архитектуры модели.
package balancer

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
)

// AdaptiveStrategy — ответ от /api/v1/cppworker/adaptive/strategy.
type AdaptiveStrategy struct {
	GPULayers   int    `json:"gpuLayers"`
	NCtx        int    `json:"nCtx"`
	KVCacheType string `json:"kvCacheType"`
	// Round 34 (2026-08-12) Phase 4: flash attention hint. -1=auto (default),
	// 0=off, 1=on. cppworker может рекомендовать 0 для CPU-only partial offload
	// (flash_attn требует GPU) или 1 для полного GPU. 0 = use cppworker's default.
	FlashAttnType int    `json:"flashAttnType"`
	UseMmap       bool   `json:"useMmap"`
	Stage         string `json:"stage"` // "exact_fit", "partial_offload", "cpu_only", "moe_offload"
	KVReduced     bool   `json:"kvReduced"`
	GPUReduced    bool   `json:"gpuReduced"`
	NCtxReduced   bool   `json:"nCtxReduced"`
	MaxViableNCtx int    `json:"maxViableNCtx"`
	Explanation   string `json:"explanation"`
	// Round 7: parallel arrays for MoE override-tensors.
	// Each pair is (regex-pattern, buft-name). Forwarded to cppworker reload
	// via the load-with-params handler so expert tensors are routed to CPU
	// and attention stays on GPU regardless of gpu_layers.
	OverrideTensors     []string `json:"overrideTensors,omitempty"`
	OverrideTensorBufts []string `json:"overrideTensorBufts,omitempty"`
}

// queryAdaptiveStrategy запрашивает у cppworker оптимальную стратегию загрузки
// через GET /api/v1/cppworker/adaptive/strategy?name=...&n_ctx=...&gpu_layers=-2
//
// kvHint (R70, 2026-09-24) — известный рабочий тип KV-cache ("f16"/"q8_0"/
// "q4_0"): профиль модели или фактический тип загруженной модели. Без хинта
// cppworker считает по f16 и на 8 GB VRAM для 65K отдаёт cpu_only
// (gpu_layers=0), хотя с q4_0 та же модель влезает на GPU. "" = прежнее
// поведение.
//
// Возвращает стратегию или nil при ошибке (caller использует fallback).
func queryAdaptiveStrategy(backendAddr, modelName string, targetNCtx int, kvHint string, httpClient *http.Client) *AdaptiveStrategy {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Second}
	}
	url := fmt.Sprintf("%s/api/v1/cppworker/adaptive/strategy?name=%s&n_ctx=%d&gpu_layers=-2",
		backendAddr, modelName, targetNCtx)
	if kvHint != "" {
		url += "&kv_cache_type=" + urlQueryEscape(kvHint)
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		logger.Get().Debugw("queryAdaptiveStrategy: failed to create request",
			"url", url, "error", err)
		return nil
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		logger.Get().Debugw("queryAdaptiveStrategy: request failed",
			"url", url, "error", err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// R83 (2026-09-25): было Debug — из-за этого «модель не найдена» на
		// стороне cppworker (404) выглядело как «стратегия просто не выбрана»,
		// и причина неразрешимого имени модели не попадала в логи балансера.
		// 404 здесь означает, что имя модели не разрезолвилось (или файла нет) —
		// это диагностически важно, поэтому Warn. Остальные коды — тоже Warn:
		// без стратегии загрузка уходит в дефолты и может не влезть в память.
		logger.Get().Warnw(i18n.T("balancer.nctx_reload_adaptive.non_200_strategy_warn"),
			"url", url, "status", resp.StatusCode, "model", modelName)
		return nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}

	var strategy AdaptiveStrategy
	if err := json.Unmarshal(body, &strategy); err != nil {
		logger.Get().Debugw("queryAdaptiveStrategy: unmarshal failed",
			"error", err)
		return nil
	}

	logger.Get().Infow("queryAdaptiveStrategy: got strategy",
		"model", modelName,
		"stage", strategy.Stage,
		"gpuLayers", strategy.GPULayers,
		"kvCacheType", strategy.KVCacheType,
		"nCtx", strategy.NCtx,
		"maxViableNCtx", strategy.MaxViableNCtx,
		"explanation", strategy.Explanation)

	// R60.12 (2026-09-07): explicit warning when adaptive strategy chooses
	// cpu_only. Это значит что VRAM не хватает даже с partial offload, и
	// cppworker будет грузить KV-cache в RAM. На типичном 8GB VRAM железе
	// (RTX 3070/A10) reload 5GB модели с 131072 n_ctx занимает 60-180 сек.
	// Оператор видит это в логах и понимает почему 503 Retry-After такой большой.
	//
	// Рекомендация для 8GB VRAM (RTX 3070/A10): используйте n_ctx <= 65536
	// с kvCacheType=q4_0 для больших моделей. Для 131072+ — нужно >= 24GB VRAM
	// или модель с < 3B параметрами.
	if strategy.Stage == "cpu_only" {
		logger.Get().Warnw("queryAdaptiveStrategy: CPU-only strategy selected (R60.12)",
			"model", modelName,
			"target_n_ctx", strategy.NCtx,
			"gpu_layers", strategy.GPULayers,
			"kv_cache_type", strategy.KVCacheType,
			"warning", "reload will be slow (60-180s) due to RAM fallback",
			"recommendation", "for 8GB VRAM, use n_ctx <= 65536 with kvCacheType=q4_0")
	}

	return &strategy
}

// clampStrategyToCurrentLayout — R83-fix (2026-09-30): не дать reload'у
// понизить GPU-оффлоад относительно уже работающей раскладки.
//
// Возвращает true, если стратегию пришлось поправить (caller логирует WARN).
// Изменяет strategy на месте:
//   - gpuLayers := currentGPULayers;
//   - stage=cpu_only превращается в "gpu_layers_kept" (иначе load-failure
//     события и логи говорили бы «модель загружена без GPU» про GPU-раскладку);
//   - gpuReduced снимается.
//
// Почему это вообще нужно: запрос стратегии приходит ПЕРЕД reload'ом модели,
// которая занимает VRAM прямо сейчас. Её собственная память в env.FreeVRAM не
// учтена, поэтому стратегия считает раскладку невыполнимой и уходит в cpu_only
// (живой случай: gemma-4 с 19 слоями на GPU → gpuLayers=0). cppworker после
// R83-fix сам добавляет память перезагружаемой модели в бюджет (см.
// handleAdaptiveStrategy), но эта защита не зависит от его версии.
func clampStrategyToCurrentLayout(strategy *AdaptiveStrategy, currentGPULayers int) bool {
	if strategy == nil || currentGPULayers <= 0 {
		return false
	}
	// -1 = «все слои на GPU» — это не понижение.
	if strategy.GPULayers < 0 || strategy.GPULayers >= currentGPULayers {
		return false
	}
	strategy.GPULayers = currentGPULayers
	strategy.GPUReduced = false
	if strategy.Stage == "cpu_only" {
		strategy.Stage = "gpu_layers_kept"
	}
	return true
}

// asyncReloadLoadBody — R119 (2026-10-15): тело async-перезагрузки
// (`executeAsyncReload` → POST /api/models/load).
//
// ЗАЧЕМ ОТДЕЛЬНАЯ ФУНКЦИЯ. Это был ТРЕТИЙ независимый сборщик тела (после
// DoReload и его load-fallback), и он жёстко зашивал настройки:
//
//	"kvCacheType": "q4_0", "gpuLayers": -2, "useMmap": true
//
// Комментарий Phase 1 объяснял q4_0 необходимостью влезть в 8 ГБ VRAM — для
// gemma-4 это верно, но означало, что ЯВНАЯ настройка оператора (профиль с
// kvCacheType=q8_0) молча перебивается константой: preflight-перезагрузка
// возвращала модель с другим типом KV-кэша, а параллельность и размышления
// терялись вовсе (их в теле не было).
//
// ПРАВИЛО: намерение оператора (профиль) важнее исторических констант, но сами
// константы остаются fallback'ом — при отсутствии профиля поведение ровно
// прежнее (иначе на 8 ГБ можно потерять достижимое окно).
func asyncReloadLoadBody(modelName string, requestedNCtx int, hints ReloadHints) map[string]interface{} {
	body := map[string]interface{}{
		"name":        modelName,
		"contextSize": requestedNCtx,
		// "force" removed: /api/models/load doesn't have this field.
		// The "reason" field is load-specific (it triggers profile re-apply).
		"reason": "balancer preflight async auto-load-or-reload (loaded<requested or cppworker restart)",
		// Phase 1 (2026-07-06): RAM fallback to maximize n_ctx in 8GB VRAM.
		// Without q4_0 KV-cache + GPU-offload=-2, gemma-4 maxes at ~17K n_ctx.
		"kvCacheType": "q4_0",
		"gpuLayers":   -2,
		"useMmap":     true,
	}
	// R119: значения из профиля/фактические перекрывают константы.
	if hints.KVCacheType != "" {
		body["kvCacheType"] = hints.KVCacheType
	}
	if hints.Parallel > 0 {
		body["parallel"] = hints.Parallel
	}
	if hints.EnableReasoning != nil {
		body["enableReasoning"] = *hints.EnableReasoning
	}
	return body
}

// applyReloadHintsToPayload — R119 (2026-10-15): перенести в тело reload те
// параметры, которые оператор задал в профиле модели (или которые фактически
// действуют у загруженной модели).
//
// ЗАЧЕМ. До этой правки payload n_ctx-перезагрузки собирался С НУЛЯ
// (name/contextSize/force/gpuLayers/flashAttn/useMmap) и профиль в него не
// попадал вообще. Живые последствия:
//   - kvCacheType не уходил → cppworker брал свой дефолт (в стенде q8_0, в
//     других сборках f16), и модель незаметно переезжала с q4_0 на другой тип
//     KV-кэша. Оператор узнавал об этом по расходу VRAM, а не по UI;
//   - parallel не уходил → cppworker брал CPPWORKER_N_PARALLEL, и модель,
//     поднятая профилем с parallel=3, возвращалась к 2 слотам;
//   - enableReasoning не уходил → перезагрузка молча выключала размышления.
//
// ПРО contextPerSeq — НАМЕРЕННО не отправляем. На этом пути цель перезагрузки —
// СУММАРНОЕ окно (`plan.NewNCtx`; клиентское окно «на клиента» балансер
// пересчитывает в суммарное через loadedSlotsNCtx). cppworker же при
// contextPerSeq > 0 считает суммарное как PAD256(contextPerSeq) × слотов и
// `contextSize` ИГНОРИРУЕТ — то есть отправка contextPerSeq здесь сделала бы
// contextSize недействующим и перезагрузка не достигла бы запрошенного окна.
// При `contextSize` + `parallel` cppworker выводит окно слота сам
// (PAD256(total/slots)) — это ровно то же число. Путь «Применить профиль»
// (reloadModelOnCppWorker) contextPerSeq отправляет: там окно задаёт оператор.
func applyReloadHintsToPayload(payload map[string]interface{}, hints ReloadHints) {
	if payload == nil {
		return
	}
	if hints.KVCacheType != "" {
		payload["kvCacheType"] = hints.KVCacheType
	}
	if hints.Parallel > 0 {
		payload["parallel"] = hints.Parallel
	}
	if hints.EnableReasoning != nil {
		payload["enableReasoning"] = *hints.EnableReasoning
	}
}

// loadBodyFromReloadPayload — R119 (2026-10-15): тело для резервного
// /api/models/load из уже собранного тела reload.
//
// ЗАЧЕМ. Fallback (reload → 404 «not currently loaded») собирал тело заново и
// переносил только kvCacheType/gpuLayers/useMmap/flashAttn — то есть терял всё,
// что добавили hint'ы и стратегия (parallel, enableReasoning, overrideTensors).
// Один builder на оба пути: копируем как есть и убираем только `force`, которого
// в loadModelRequest нет (cppworker декодирует тело строгим декодером и ответил бы
// 400 «unknown field force»).
func loadBodyFromReloadPayload(payload map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(payload))
	for k, v := range payload {
		if k == "force" {
			continue
		}
		out[k] = v
	}
	return out
}

// enrichReloadPayload добавляет параметры из AdaptiveStrategy в payload reload-запроса.
// Возвращает модифицированный payload (gpuLayers, kvCacheType, contextSize, useMmap, flashAttn).
//
// Round 34 (2026-08-12) Phase 4: добавлен flashAttn (auto-detect для reasoning моделей
// в Q4_K_M; off для CPU-only partial offload).
func enrichReloadPayload(payload map[string]interface{}, strategy *AdaptiveStrategy) map[string]interface{} {
	if strategy == nil {
		return payload
	}
	// gpuLayers: используем из стратегии
	payload["gpuLayers"] = strategy.GPULayers
	payload["useMmap"] = strategy.UseMmap

	// Round 34 Phase 4: flash attention — передаём рекомендацию strategy в cppworker.
	// 0 = не задано (cppworker's default -1=auto), -1 = auto, 1 = on.
	if strategy.FlashAttnType != 0 {
		payload["flashAttn"] = strategy.FlashAttnType
	}

	// kvCacheType: передаём cppworker'у
	if strategy.KVCacheType != "" && strategy.KVCacheType != "f16" {
		payload["kvCacheType"] = strategy.KVCacheType
	}

	// contextSize: если стратегия уменьшила n_ctx — используем новый
	if strategy.NCtx > 0 {
		payload["contextSize"] = strategy.NCtx
	}

	// Round 51.4 (2026-08-20): REMOVED `payload["adaptiveStage"] = strategy.Stage`.
	// cppworker's reloadModelRequest struct (cmd/cppworker/types.go:134) does NOT
	// have an "adaptiveStage" field — Go's json.Decoder rejects unknown fields
	// with "invalid JSON: unknown field 'adaptiveStage'" → 400 response.
	// adaptiveStage is balancer-internal metadata for logging/tracking; it must
	// NOT be sent to cppworker. The log line below still includes it for ops
	// visibility without polluting the wire payload.

	// Round 7: forward MoE override-tensors to /api/models/reload payload.
	// cppworker handler accepts parallel arrays via overrideTensors / overrideTensorBufts.
	if len(strategy.OverrideTensors) > 0 && len(strategy.OverrideTensors) == len(strategy.OverrideTensorBufts) {
		payload["overrideTensors"] = strategy.OverrideTensors
		payload["overrideTensorBufts"] = strategy.OverrideTensorBufts
		logger.Get().Infow("enrichReloadPayload: forwarding MoE override-tensors",
			"count", len(strategy.OverrideTensors))
	}

	logger.Get().Infow("enrichReloadPayload: payload enriched with adaptive strategy",
		"gpuLayers", strategy.GPULayers,
		"kvCacheType", strategy.KVCacheType,
		"flashAttn", strategy.FlashAttnType,
		"contextSize", payload["contextSize"],
		"stage", strategy.Stage)

	return payload
}
