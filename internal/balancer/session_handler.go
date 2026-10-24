package balancer

import (
	"net/http"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// resolveSessionBackend — определяет целевой бэкенд с учётом session stickiness и rebalance.
// Если сессия не привязана или stickiness отключён — возвращает пустую строку (выбор через selectBackend).
func (p *Proxy) resolveSessionBackend(r *http.Request, model, sessionID, clientName string) string {
	if sessionID == "" || !p.config.Balancing.SessionStickiness {
		return ""
	}

	session := p.sessionMgr.Get(sessionID)
	if session == nil {
		return ""
	}

	targetBackend := session.BackendID

	p.mu.RLock()
	backendState, exists := p.backends[targetBackend]
	p.mu.RUnlock()

	if !exists || backendState.Backend.Status != types.StatusHealthy {
		logger.Get().Warnw("session backend unavailable, selecting new backend",
			"session_backend", targetBackend,
			"exists", exists,
			"status", backendState.Backend.Status,
		)
		return ""
	}

	// Проверяем загрузку и ребалансируем при необходимости
	targetBackend = p.rebalanceIfNeeded(model, targetBackend, sessionID, clientName, r, backendState)

	// Обновляем привязку сессии
	if targetBackend != "" && p.config.Balancing.SessionStickiness {
		p.sessionMgr.SetWithProtocol(sessionID, targetBackend, model, clientName, p.getClientRealIP(r), r.UserAgent(), RequestProtocol(r.URL.Path))
	}

	return targetBackend
}

// rebalanceIfNeeded — проверяет загрузку бэкенда и выполняет rebalance если нужно.
func (p *Proxy) rebalanceIfNeeded(model, targetBackend, sessionID, clientName string, r *http.Request, backendState *BackendState) string {
	backendState.mu.Lock()
	active := backendState.ActiveReqs
	maxReqs := backendState.Backend.MaxConcurrentReqs
	backendState.mu.Unlock()

	// R139: нагрузку ЭТОЙ ЖЕ сессии не считаем поводом уводить её на другой
	// backend — см. sessionInflight и такой же блок в proxy.go.
	own := p.sessionInflight(sessionID)
	foreign := active - own
	if foreign < 0 {
		foreign = 0
	}

	if maxReqs <= 0 || foreign <= 0 {
		return targetBackend
	}

	loadRatio := float64(foreign) / float64(maxReqs)
	forceRebalance := loadRatio >= 1.0

	if loadRatio > 0.50 || forceRebalance {
		// Сначала ищем бэкенд с той же моделью
		altBackend := p.findLessLoadedBackendWithModel(model, targetBackend, nil)
		if altBackend != "" {
			logger.Get().Infow("rebalancing session to less loaded backend (same model)",
				"session", sessionID, "from", targetBackend, "to", altBackend,
				"load_ratio", loadRatio, "force", forceRebalance)
			return altBackend
		}

		// Затем ищем любой менее загруженный (даже без модели — Ollama загрузит при первом запросе).
		// Это холодный старт, но лучше чем 503 на перегруженном бэкенде.
		altBackend = p.findLessLoadedBackendAny(model, targetBackend, nil)
		if altBackend != "" {
			logger.Get().Infow("rebalancing session to less loaded backend (any, may cold-start)",
				"session", sessionID, "from", targetBackend, "to", altBackend,
				"load_ratio", loadRatio, "force", forceRebalance)
			return altBackend
		}

		if forceRebalance {
			logger.Get().Warnw("force rebalance failed, queueing request",
				"session", sessionID, "backend", targetBackend, "load_ratio", loadRatio)
			return ""
		}
	}

	return targetBackend
}

// sessionInflight — сколько запросов этой сессии сейчас в полёте.
//
// R139 (2026-10-16): различие «backend занят моим же стримом» и «backend занят
// чужими запросами» — ключевое для длинных генераций. Живой дефект (deepseek
// harness через балансер): сессия привязана к прогретой копии, её собственный
// стрим в полёте (ActiveReqs=1 при MaxConcurrentReqs=1) — и следующий запрос
// той же сессии ребалансировался на ХОЛОДНЫЙ backend, где модель грузится
// заново. Клиент при этом видит тишину (pi-ai stream idle timeout 300000ms).
func (p *Proxy) sessionInflight(sessionID string) int {
	if p.admission == nil {
		return 0
	}
	return p.admission.inflightOf(sessionID)
}

// bindSession — привязывает сессию к выбранному бэкенду.
func (p *Proxy) bindSession(sessionID, backendID, model, clientName string, r *http.Request) {
	if sessionID != "" && p.config.Balancing.SessionStickiness {
		p.sessionMgr.SetWithProtocol(sessionID, backendID, model, clientName, p.getClientRealIP(r), r.UserAgent(), RequestProtocol(r.URL.Path))
	}
}
