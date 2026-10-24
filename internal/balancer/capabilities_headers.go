package balancer

import (
	"net/http"
	"strconv"

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
	// R216 (2026-10-21): ФАКТИЧЕСКОЕ окно загруженного инстанса.
	//
	// Клиент (deepseek harness, OpenWebUI) не может вычислить его сам: X-Model-
	// Max-Context — это потолок модели из GGUF (например 131072), а инстанс
	// поднят с n_ctx=8192. Пока инстанс не загружен, заголовка нет — и это
	// честно: фактического окна ещё не существует.
	if loaded, _ := p.modelContextInfo(modelName); loaded > 0 {
		w.Header().Set("X-Model-Context-Length", strconv.Itoa(loaded))
	}
}

// modelContextInfo — фактическое окно загруженного инстанса и потолок модели.
//
// loadedCtx = 0 означает «модель не загружена» (или метрик ещё нет).
// maxCtx    = 0 означает «потолок неизвестен».
//
// ЗАЧЕМ. Клиенты подбирают историю по окну модели, а модель может быть поднята
// с меньшим n_ctx (оператор, профиль, VRAM). Тогда клиент шлёт 30k токенов в
// 8k-инстанс и получает 413 «prompt too long» либо (при авто-reload) новую
// загрузку — снаружи это выглядит как «контекст не учитывается».
func (p *Proxy) modelContextInfo(modelName string) (loadedCtx, maxCtx int) {
	if p == nil || p.metricsMgr == nil || modelName == "" {
		return 0, 0
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
		if !modelNameMatches(m.Name, modelName) {
			continue
		}
		loaded := m.ContextLength
		if m.ContextPerSeq > 0 && m.ContextPerSeq < loaded {
			// Клиенту полезнее окно НА СЛОТ: именно его он упирает своим prompt.
			loaded = m.ContextPerSeq
		}
		maxCtx = m.MaxContext
		if maxCtx < m.GGUFMaxContext {
			maxCtx = m.GGUFMaxContext
		}
		return loaded, maxCtx
	}
	return 0, 0
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
		// R66d: нормализация .gguf/пути/регистра — иначе клиент (Cline/OpenWebUI),
		// запросивший "X.gguf", не получал X-Model-* заголовки способностей.
		if modelNameMatches(m.Name, modelName) && m.Capabilities != nil {
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
