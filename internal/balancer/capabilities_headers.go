package balancer

import (
	"net/http"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// addModelCapabilitiesHeaders добавляет X-Model-* заголовки в HTTP response.
// Round 18 P0.1 (2026-08-03).
//
// Использует capabilities из metrics cache (Round 18 hotfix: берем из
// llamaMetrics[id].LoadedModels[].Capabilities, не из metrics[id].LlamaCpp).
// Fallback на DefaultCapabilitiesForModelName если модель не в кэше.
//
// Применяется в proxyRequest (все inference endpoints: /v1/chat, /v1/embeddings,
// /api/chat, /api/generate, /api/embed, /v1/completions, /v1/embeddings).
func (p *Proxy) addModelCapabilitiesHeaders(w http.ResponseWriter, modelName string) {
	if modelName == "" {
		return
	}

	caps := p.findModelCapabilities(modelName)
	if caps == nil {
		return
	}

	headers := caps.Headers()
	for k, v := range headers {
		w.Header().Set(k, v)
	}
}

// findModelCapabilities ищет capabilities модели в любом бэкенде.
// Возвращает nil если модель нигде не загружена.
//
// Safe для concurrent reads — snapshot llamaMetrics в локальную переменную.
func (p *Proxy) findModelCapabilities(modelName string) *types.ModelCapabilities {
	if p == nil || p.metricsMgr == nil {
		return nil
	}

	p.metricsMgr.mu.RLock()
	snapshot := make([]types.LlamaCppModel, 0, 8)
	for _, lm := range p.metricsMgr.llamaMetrics {
		if lm == nil {
			continue
		}
		snapshot = append(snapshot, lm.LoadedModels...)
	}
	p.metricsMgr.mu.RUnlock()

	for i := range snapshot {
		m := &snapshot[i]
		if m.Name == modelName && m.Capabilities != nil {
			logger.Get().Debugw("addModelCapabilitiesHeaders: using cppworker capabilities",
				"model", modelName, "caps", m.Capabilities)
			return m.Capabilities
		}
	}

	// Fallback: auto-detect по имени модели (без loaded info).
	caps := types.DefaultCapabilitiesForModelName(modelName)
	logger.Get().Debugw("addModelCapabilitiesHeaders: using auto-detect fallback",
		"model", modelName, "caps", &caps)
	return &caps
}
