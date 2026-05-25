package balancer

import (
	"net/http"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// acquireSlotWithRetry — пытается захватить слот на целевом бэкенде.
// При неудаче делает до maxRetries попыток на альтернативных бэкендах.
// Возвращает (backendID, true) при успехе, ("", false) если ни один бэкенд не принял.
func (p *Proxy) acquireSlotWithRetry(model, targetBackend, sessionID, clientName string, r *http.Request) (string, bool) {
	if targetBackend == "" {
		return "", false
	}

	if p.tryAcquireSlot(targetBackend) {
		return targetBackend, true
	}

	// Retry на других бэкендах
	attemptedBackends := map[string]bool{targetBackend: true}
	const maxRetries = 10
	for retry := 0; retry < maxRetries; retry++ {
		altBackend := p.selectBackendExcluding(model, attemptedBackends, p.determineRequestBackendType(r))
		if altBackend == "" {
			break
		}
		if p.tryAcquireSlot(altBackend) {
			p.bindSession(sessionID, altBackend, model, clientName, r)
			logger.Get().Infow("acquired slot on alternate backend",
				"backend", altBackend, "original", targetBackend)
			return altBackend, true
		}
		attemptedBackends[altBackend] = true
	}

	return "", false
}

// executeWithFallback — выполняет проксирование запроса к бэкенду с fallback на другие при ошибке.
// Возвращает true если запрос успешно обработан (ответ отправлен клиенту).
// Вызывающий код обязан освободить слот backendID через defer releaseSlot.
func (p *Proxy) executeWithFallback(w http.ResponseWriter, r *http.Request, backendID, model, sessionID, clientName string) bool {
	err := p.proxyRequest(w, r, backendID)
	if err == nil {
		return true
	}

	logger.Get().Errorw("backend request failed", "backend", backendID, "error", err)
	p.UpdateBackendStatus(backendID, types.StatusUnhealthy)
	p.scheduleRecoveryCheck(backendID)

	// Fallback на другие бэкенды (макс. 2 попытки)
	attemptedBackends := map[string]bool{backendID: true}
	for attempt := 1; attempt < 3; attempt++ {
		altBackend := p.selectBackendExcluding(model, attemptedBackends, p.determineRequestBackendType(r))
		if altBackend == "" {
			break
		}
		if !p.tryAcquireSlot(altBackend) {
			attemptedBackends[altBackend] = true
			continue
		}

		p.bindSession(sessionID, altBackend, model, clientName, r)
		attemptedBackends[altBackend] = true
		defer p.releaseSlot(altBackend)

		err := p.proxyRequest(w, r, altBackend)
		if err == nil {
			return true
		}
		logger.Get().Errorw("backend request failed", "backend", altBackend, "attempt", attempt, "error", err)
	}

	return false
}