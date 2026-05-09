package balancer

import (
	"sync"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// SessionManager - менеджер сессий
type SessionManager struct {
	sessions map[string]*types.Session
	mu       sync.RWMutex
	ttl      time.Duration
	stopCh   chan struct{}
}

// NewSessionManager - создание менеджера сессий с дефолтным TTL
func NewSessionManager() *SessionManager {
	return NewSessionManagerWithTTL(15 * time.Minute)
}

// NewSessionManagerWithTTL - создание менеджера сессий с заданным TTL
func NewSessionManagerWithTTL(ttl time.Duration) *SessionManager {
	sm := &SessionManager{
		sessions: make(map[string]*types.Session),
		ttl:      ttl,
		stopCh:   make(chan struct{}),
	}
	go sm.cleanupLoop()
	return sm
}

// Stop - остановка cleanup loop
func (sm *SessionManager) Stop() {
	select {
	case <-sm.stopCh:
	default:
		close(sm.stopCh)
	}
}

// cleanupLoop - цикл очистки сессий
func (sm *SessionManager) cleanupLoop() {
	interval := sm.ttl / 3
	if interval < time.Minute {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			sm.cleanup()
		case <-sm.stopCh:
			return
		}
	}
}

// cleanup - очистка просроченных сессий
func (sm *SessionManager) cleanup() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	now := time.Now()
	for id, session := range sm.sessions {
		if now.Sub(session.LastRequestAt) > sm.ttl {
			delete(sm.sessions, id)
		}
	}
}

// Get - получение сессии
func (sm *SessionManager) Get(id string) *types.Session {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	return sm.sessions[id]
}

// GetAll - получение всех сессий
func (sm *SessionManager) GetAll() []*types.Session {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	sessions := make([]*types.Session, 0, len(sm.sessions))
	for _, s := range sm.sessions {
		sessions = append(sessions, s)
	}
	return sessions
}

// Delete - удаление сессии по ID
func (sm *SessionManager) Delete(id string) bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if _, exists := sm.sessions[id]; exists {
		delete(sm.sessions, id)
		return true
	}
	return false
}

// Clear - удаление всех сессий
func (sm *SessionManager) Clear() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	sm.sessions = make(map[string]*types.Session)
}

// Set - установка сессии (с clientName, clientIP и userAgent)
func (sm *SessionManager) Set(id, backendID, model, clientName, clientIP, userAgent string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	now := time.Now()
	if session, ok := sm.sessions[id]; ok {
		session.BackendID = backendID
		session.Model = model
		session.ClientName = clientName
		session.ClientIP = clientIP
		session.UserAgent = userAgent
		session.LastRequestAt = now
		session.RequestCount++
	} else {
		if clientIP == "" {
			// Не используем fingerprint/id как IP — оставляем пустым
			// IP будет заполнен при следующем реальном запросе через getClientRealIP
			clientIP = ""
		}
		sm.sessions[id] = &types.Session{
			ID:            id,
			BackendID:     backendID,
			Model:         model,
			ClientName:    clientName,
			ClientIP:      clientIP,
			UserAgent:     userAgent,
			CreatedAt:     now,
			LastRequestAt: now,
			RequestCount:  1,
		}
	}
}