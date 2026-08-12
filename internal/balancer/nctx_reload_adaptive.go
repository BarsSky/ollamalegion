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

	"ollama-loadbalancer/pkg/logger"
)

// AdaptiveStrategy — ответ от /api/v1/cppworker/adaptive/strategy.
type AdaptiveStrategy struct {
	GPULayers    int    `json:"gpuLayers"`
	NCtx         int    `json:"nCtx"`
	KVCacheType  string `json:"kvCacheType"`
	// Round 34 (2026-08-12) Phase 4: flash attention hint. -1=auto (default),
	// 0=off, 1=on. cppworker может рекомендовать 0 для CPU-only partial offload
	// (flash_attn требует GPU) или 1 для полного GPU. 0 = use cppworker's default.
	FlashAttnType int    `json:"flashAttnType"`
	UseMmap      bool   `json:"useMmap"`
	Stage        string `json:"stage"` // "exact_fit", "partial_offload", "cpu_only", "moe_offload"
	KVReduced    bool   `json:"kvReduced"`
	GPUReduced   bool   `json:"gpuReduced"`
	NCtxReduced  bool   `json:"nCtxReduced"`
	MaxViableNCtx int   `json:"maxViableNCtx"`
	Explanation  string `json:"explanation"`
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
// Возвращает стратегию или nil при ошибке (caller использует fallback).
func queryAdaptiveStrategy(backendAddr, modelName string, targetNCtx int, httpClient *http.Client) *AdaptiveStrategy {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Second}
	}
	url := fmt.Sprintf("%s/api/v1/cppworker/adaptive/strategy?name=%s&n_ctx=%d&gpu_layers=-2",
		backendAddr, modelName, targetNCtx)

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
		logger.Get().Debugw("queryAdaptiveStrategy: non-200 status",
			"url", url, "status", resp.StatusCode)
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

	return &strategy
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

	// Отмечаем что стратегия подобрана адаптивно
	payload["adaptiveStage"] = strategy.Stage

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
