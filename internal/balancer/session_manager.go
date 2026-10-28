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
	ttl      time.Duration // абсолютный TTL сессии (создания) — страховка, см. cleanup
	idleTTL  time.Duration // TTL простоя (после последнего запроса)
	// zombieThreshold — сколько может «молчать» сессия с активным стримом, прежде
	// чем её сочтут зомби. R251: вынесено в поле и задаётся из конфига
	// (Proxy.getZombieSessionThreshold), а не хардкодом 120 с: у длинного
	// префилла 27B-модели пауза между чанками легально доходит до минут, и
	// жёсткие 120 с удаляли живую сессию.
	zombieThreshold time.Duration
	// identity — R252: «кто это и какой разговор» по каждому id (см.
	// SessionIdentity). Отдельная карта, а не поля types.Session: наружу
	// (/api/v1/sessions) эти данные не отдаются, и публичный контракт не меняется.
	identity map[string]SessionIdentity
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
		sessions:        make(map[string]*types.Session),
		identity:        make(map[string]SessionIdentity, 8),
		ttl:             sessionTTL,
		idleTTL:         idleTTL,
		zombieThreshold: defaultZombieSessionThreshold,
		stopCh:          make(chan struct{}),
	}
	go sm.cleanupLoop()
	return sm
}

// defaultZombieSessionThreshold — совместимый с прежним хардкодом дефолт.
const defaultZombieSessionThreshold = 120 * time.Second

// SetZombieThreshold — R251: задать порог «молчащей» стрим-сессии из конфига.
//
// Зачем сеттер, а не параметр конструктора: SessionManager создаётся в
// NewProxy до применения конфига в тестах и в лёгких сценариях, а значения
// AdvancedTiming читаются из p.config позже. Значение <= 0 игнорируется
// (остаётся дефолт), чтобы «ноль в конфиге» не означал «удалять сразу».
func (sm *SessionManager) SetZombieThreshold(d time.Duration) {
	if sm == nil || d <= 0 {
		return
	}
	sm.mu.Lock()
	sm.zombieThreshold = d
	sm.mu.Unlock()
}

// SessionIdentity — R252 (2026-10-28): «кто это и какой разговор» для сессии.
//
// ХРАНИТСЯ ОТДЕЛЬНО ОТ types.Session, а не полями сессии:
//   - types.Session уходит клиенту в /api/v1/sessions, и добавлять туда поля
//     ради внутренней эвристики — менять публичный контракт без нужды;
//   - данные нужны ровно одному месту (резолвер id) и живут столько же, сколько
//     запись сессии (чистятся вместе с ней в cleanup).
//
// ConvKey — отпечаток НАЧАЛА разговора (session_instance_r118.go). Именно он
// меняется, когда клиент сжимает историю (Cline переписывает начало), из-за чего
// id сессии «переезжал» и работа продолжалась под новой строкой в панели.
type SessionIdentity struct {
	IP           string
	Client       string
	Model        string
	EndpointType string
	ConvKey      string
	At           time.Time
}

// RecordIdentity — запомнить идентичность сессии.
//
// No-op, если записи ещё нет: вызывающий код считает id ДО создания сессии
// (getSessionIDWithModel → SetWithProtocol), поэтому первая запись получит
// идентичность со второго запроса. Это осознанно: до второго запроса
// переиспользовать нечего, а «подвешенные» идентичности без сессии только
// копились бы.
func (sm *SessionManager) RecordIdentity(sessionID string, ident SessionIdentity) {
	if sm == nil || sessionID == "" {
		return
	}
	if ident.At.IsZero() {
		ident.At = time.Now()
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if _, ok := sm.sessions[sessionID]; !ok {
		return
	}
	if sm.identity == nil {
		sm.identity = make(map[string]SessionIdentity, 8)
	}
	sm.identity[sessionID] = ident
}

// IdentityOf — идентичность сессии (пустая, если не записана).
func (sm *SessionManager) IdentityOf(sessionID string) SessionIdentity {
	if sm == nil || sessionID == "" {
		return SessionIdentity{}
	}
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.identity[sessionID]
}

// ResolveID — R252: переиспользовать существующую сессию вместо создания новой.
//
// ЗАЧЕМ. Без явных идентификаторов (Cline их не шлёт) id выводится из отпечатка
// начала разговора (client.go:366-370). Сжатие истории меняет начало → новый id:
// старая строка висит в панели до idle TTL, а «кошелёк» admission-очереди (его
// ключ — sessionID) обнуляется посреди задачи. Для оператора это выглядит как
// «клиент пропал, началась другая сессия».
//
// Правило: если candidateID ещё не существует, отдаём id уже известной сессии,
// которую признал подходящей matcher. Сравнение клиента/модели/разговора делает
// вызывающий код — он один знает, какие признаки у него есть в запросе.
//
// Матчер получает КОПИЮ сессии и вызывается ВНЕ блокировки: внутри него можно
// звать IdentityOf (рекурсивный RLock под ожидающим писателем — риск дедлока).
func (sm *SessionManager) ResolveID(candidateID string, matcher func(*types.Session) bool) string {
	if sm == nil || candidateID == "" || matcher == nil {
		return candidateID
	}

	sm.mu.RLock()
	if _, ok := sm.sessions[candidateID]; ok {
		sm.mu.RUnlock()
		return candidateID
	}
	candidates := make([]types.Session, 0, len(sm.sessions))
	for _, s := range sm.sessions {
		if s == nil {
			continue
		}
		candidates = append(candidates, *s)
	}
	sm.mu.RUnlock()

	bestID := ""
	var bestAt time.Time
	for i := range candidates {
		c := candidates[i]
		if c.ID == candidateID || !matcher(&c) {
			continue
		}
		// Детерминированно: самая свежая подходящая сессия.
		if bestID == "" || c.LastRequestAt.After(bestAt) {
			bestID, bestAt = c.ID, c.LastRequestAt
		}
	}
	if bestID != "" {
		return bestID
	}
	return candidateID
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
		// R251 (2026-10-28): АБСОЛЮТНЫЙ TTL БОЛЬШЕ НЕ УБИВАЕТ ЖИВУЮ СЕССИЮ.
		//
		// ЖИВОЙ ДЕФЕКТ (Cline, длинная серия tool-call'ов). Правило
		// `now - CreatedAt > ttl` удаляло запись через 15 минут ОТ СОЗДАНИЯ, даже
		// если клиент работал всё это время: LastRequestAt обновлялся каждым
		// запросом, а CreatedAt — нет. В панели это выглядело как «балансер
		// потерял клиента»: строка сессии исчезала посреди задачи, а вместе с ней
		// терялась привязка к бэкенду. Задача агента (создать файл, прочитать,
		// поправить) идёт десятками минут — это штатный сценарий, а не аномалия.
		//
		// Теперь единственный критерий удаления — ПРОСТОЙ (idleTTL от последнего
		// запроса). Абсолютный TTL остаётся только страховкой от сессий, которые
		// «никогда не простаивают» из-за фонового пинга: он применяется, лишь
		// когда простой уже превысил половину idleTTL, то есть сессия всё равно
		// не живая.
		idle := now.Sub(session.LastRequestAt)
		if session.HasActiveStream {
			// Порог зомби-сессий использует тот же параметр что и в Proxy.cleanupZombieSessions.
			// Для session_manager нет прямого доступа к p.config, используем поле zombieThreshold
			// которое задаётся при создании (дефолт 120s — совместимо с дефолтом конфига).
			if idle > sm.zombieThreshold {
				toDelete = append(toDelete, id)
			}
			continue
		}
		if idle > sm.idleTTL {
			toDelete = append(toDelete, id)
			continue
		}
		if now.Sub(session.CreatedAt) > sm.ttl && idle > sm.idleTTL/2 {
			toDelete = append(toDelete, id)
		}
	}
	sm.mu.RUnlock()

	// Фаза 2: удаление под write-lock (очень быстро — только delete по собранным ID)
	if len(toDelete) > 0 {
		sm.mu.Lock()
		for _, id := range toDelete {
			delete(sm.sessions, id)
			// R252: идентичность живёт ровно столько же, сколько запись —
			// иначе карта росла бы вместе с числом завершённых задач.
			delete(sm.identity, id)
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
		delete(sm.identity, id)
		return true
	}
	return false
}

// Get - получение сессии
//
// ВНИМАНИЕ (R66c, 2026-09-22): возвращает ОБЩИЙ указатель. Читать его поля
// без блокировки нельзя — параллельный Set мутирует ту же структуру под sm.mu
// (гонка, которую ловил детектор на TestLoadBalancing_MultipleClients:
// SessionManager.Set vs чтение session.BackendID в proxy.go:751).
// Для чтения полей используйте GetBackendID / GetAllSnapshots.
func (sm *SessionManager) Get(id string) *types.Session {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	return sm.sessions[id]
}

// GetBackendID — снимок поля BackendID под блокировкой (R66c).
//
// Нужен вместо `s := Get(id); s.BackendID`: сессия одна на клиента+модель,
// поэтому два параллельных запроса от одного клиента делят один *Session, и
// второй запрос пишет в него через Set, пока первый читает.
// ProtocolOf — R229: протокол сессии (openai/ollama) под блокировкой; пусто =
// протокол ещё не определён (служебное обновление или старый клиент).
func (sm *SessionManager) ProtocolOf(id string) string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	s, ok := sm.sessions[id]
	if !ok || s == nil {
		return ""
	}
	return s.Protocol
}
func (sm *SessionManager) GetBackendID(id string) (string, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	s, ok := sm.sessions[id]
	if !ok || s == nil {
		return "", false
	}
	return s.BackendID, true
}

// GetAll - получение всех сессий.
//
// ВНИМАНИЕ (R66c): как и Get, возвращает общие указатели — для чтения полей
// используйте GetAllSnapshots.
func (sm *SessionManager) GetAll() []*types.Session {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	sessions := make([]*types.Session, 0, len(sm.sessions))
	for _, s := range sm.sessions {
		sessions = append(sessions, s)
	}
	return sessions
}

// GetAllSnapshots — копии сессий под блокировкой (R66c).
//
// types.Session состоит только из value-полей, поэтому копия — полноценный
// снимок: можно безопасно читать без удержания sm.mu.
func (sm *SessionManager) GetAllSnapshots() []types.Session {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	sessions := make([]types.Session, 0, len(sm.sessions))
	for _, s := range sm.sessions {
		if s == nil {
			continue
		}
		sessions = append(sessions, *s)
	}
	return sessions
}

// Delete - удаление сессии по ID
func (sm *SessionManager) Delete(id string) bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if _, exists := sm.sessions[id]; exists {
		delete(sm.sessions, id)
		delete(sm.identity, id)
		return true
	}
	return false
}

// Clear - удаление всех сессий
func (sm *SessionManager) Clear() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	sm.sessions = make(map[string]*types.Session)
	if sm.identity != nil {
		sm.identity = make(map[string]SessionIdentity, 8)
	}
}

// Set - установка сессии (с clientName, clientIP и userAgent)
func (sm *SessionManager) Set(id, backendID, model, clientName, clientIP, userAgent string) {
	sm.SetWithProtocol(id, backendID, model, clientName, clientIP, userAgent, "")
}

// SetWithProtocol — R229 (2026-10-22): то же, что Set, но с протоколом запроса
// (openai/ollama). Протокол ПИШЕТСЯ только непустым значением: последующий вызов
// Set (из места, где протокол не определён) не должен стирать уже известный —
// иначе бейдж исчезнет после первого же служебного обновления сессии.
func (sm *SessionManager) SetWithProtocol(id, backendID, model, clientName, clientIP, userAgent, protocol string) {
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
		if protocol != "" {
			session.Protocol = protocol
		}
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
			Protocol:      protocol,
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
