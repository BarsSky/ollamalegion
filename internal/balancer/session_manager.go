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
	ttl      time.Duration // абсолютный TTL сессии (создания)
	idleTTL  time.Duration // TTL простоя (после последнего запроса)
	stopCh   chan struct{}
}

// NewSessionManager - создание менеджера сессий с дефолтным TTL
func NewSessionManager() *SessionManager {
	return NewSessionManagerWithTTL(15*time.Minute, 5*time.Minute)
}

// NewSessionManagerWithTTL - создание менеджера сессий с заданными TTL
// sessionTTL — абсолютный максимум жизни сессии
// idleTTL — время простоя после последнего запроса до удаления
func NewSessionManagerWithTTL(sessionTTL, idleTTL time.Duration) *SessionManager {
	if sessionTTL <= 0 {
		sessionTTL = 15 * time.Minute
	}
	if idleTTL <= 0 {
		idleTTL = 5 * time.Minute
	}
	sm := &SessionManager{
		sessions: make(map[string]*types.Session),
		ttl:      sessionTTL,
		idleTTL:  idleTTL,
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
	interval := sm.idleTTL / 3
	if interval < 30*time.Second {
		interval = 30 * time.Second
	}
	if interval > 2*time.Minute {
		interval = 2 * time.Minute
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

// cleanup - очистка просроченных сессий.
// Сначала собирает ID для удаления под read-lock, затем удаляет под write-lock.
// Это минимизирует время блокировки записи и не мешает Get/Set во время итерации.
func (sm *SessionManager) cleanup() {
	now := time.Now()

	// Фаза 1: сбор кандидатов на удаление под read-lock (быстро, не блокирует Get/Set)
	sm.mu.RLock()
	toDelete := make([]string, 0, 16)
	for id, session := range sm.sessions {
		if session.HasActiveStream {
			// Порог зомби-сессий использует тот же параметр что и в Proxy.cleanupZombieSessions.
			// Для session_manager нет прямого доступа к p.config, используем поле zombieThreshold
			// которое устанавливается при создании (сейчас хардкод 120s — совместимо с дефолтом).
			// В будущем можно прокинуть через NewSessionManagerWithTTL параметр zombieThreshold.
			if now.Sub(session.LastRequestAt) > 120*time.Second {
				toDelete = append(toDelete, id)
			}
			continue
		}
		if now.Sub(session.CreatedAt) > sm.ttl {
			toDelete = append(toDelete, id)
			continue
		}
		if now.Sub(session.LastRequestAt) > sm.idleTTL {
			toDelete = append(toDelete, id)
		}
	}
	sm.mu.RUnlock()

	// Фаза 2: удаление под write-lock (очень быстро — только delete по собранным ID)
	if len(toDelete) > 0 {
		sm.mu.Lock()
		for _, id := range toDelete {
			delete(sm.sessions, id)
		}
		sm.mu.Unlock()
	}
}

// DetectZombieSessions — возвращает список сессий-зомби:
// HasActiveStream = true, но LastRequestAt > zombieThreshold.
// Вызывающий код должен освободить бэкенды для этих сессий.
func (sm *SessionManager) DetectZombieSessions(zombieThreshold time.Duration) []*types.Session {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	now := time.Now()
	var zombies []*types.Session
	for _, session := range sm.sessions {
		if session.HasActiveStream && now.Sub(session.LastRequestAt) > zombieThreshold {
			zombies = append(zombies, session)
		}
	}
	return zombies
}

// ForceRemove — принудительное удаление сессии (включая активные streaming).
// Используется для освобождения бэкенда при обнаружении зомби.
func (sm *SessionManager) ForceRemove(id string) bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if _, exists := sm.sessions[id]; exists {
		delete(sm.sessions, id)
		return true
	}
	return false
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

// SetStreamActive — помечает сессию как имеющую активный streaming
func (sm *SessionManager) SetStreamActive(id string, active bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if session, ok := sm.sessions[id]; ok {
		session.HasActiveStream = active
		if !active {
			session.LastRequestAt = time.Now()
		}
	}
}

// HeartbeatStream — обновляет LastRequestAt для сессии с активным streaming
// чтобы предотвратить удаление по idle TTL во время долгого streaming-запроса
func (sm *SessionManager) HeartbeatStream(id string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if session, ok := sm.sessions[id]; ok {
		session.LastRequestAt = time.Now()
	}
}