// recent_clients.go — R629 (2026-11-19): учёт «недавних клиентов» для монитора.
//
// ЗАЧЕМ ВЫНЕСЕНО. `proxy.go` держал 2422 строки: в одном файле жили маршрутизация
// (`ServeHTTP` 936 строк), конструктор `NewProxy` (340) и три независимых слоя
// учёта. Вынесен слой «кто к нам ходил»: `recordRecentClient` /
// `recordRecentClientParsed` / `markRecentClientStreamActive` /
// `cleanupZombieSessions` / `getRecentClients`. Он не участвует в решениях
// балансировки и читается целиком ради панели и TTL-очистки.
//
// ПОВЕДЕНИЕ НЕ МЕНЯЛОСЬ: код перенесён как есть.
package balancer

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// recordRecentClient — трекинг клиента для монитора (TTL 60 секунд)
func (p *Proxy) recordRecentClient(r *http.Request) {
	clientIP := p.getClientRealIP(r)
	clientName := p.getClientName(r)
	userAgent := r.UserAgent()
	key := clientIP + "::" + clientName

	// Определяем модель из запроса (если доступно)
	model := ""
	if r.Method == http.MethodPost {
		if body, ok := peekBodyR479(r, 64<<10); ok {
			var req map[string]interface{}
			if err := json.Unmarshal(body, &req); err == nil {
				if m, ok := req["model"].(string); ok {
					model = m
				}
			}
		}
	}

	p.recentClientsMu.Lock()
	defer p.recentClientsMu.Unlock()

	now := time.Now()
	if existing, ok := p.recentClients[key]; ok {
		existing.LastRequestAt = now
		existing.RequestCount++
		existing.UserAgent = userAgent
		if model != "" {
			existing.Model = model
		}
		p.recentClients[key] = existing
	} else {
		p.recentClients[key] = &types.RecentClient{
			ClientName:    clientName,
			ClientIP:      clientIP,
			UserAgent:     userAgent,
			LastRequestAt: now,
			RequestCount:  1,
			Model:         model,
		}
	}

	// Очистка старых записей (> 60 секунд)
	cutoff := now.Add(-60 * time.Second)
	for k, rc := range p.recentClients {
		if rc.LastRequestAt.Before(cutoff) && !rc.IsStreamActive {
			delete(p.recentClients, k)
		}
	}
}

// recordRecentClientParsed — трекинг клиента для монитора с использованием уже распарсенного тела.
// Вызывается из ServeHTTP после parseRequestBody чтобы избежать повторного чтения.
func (p *Proxy) recordRecentClientParsed(r *http.Request, parsed *parsedRequest) {
	clientIP := p.getClientRealIP(r)
	clientName := p.getClientName(r)
	userAgent := r.UserAgent()
	key := clientIP + "::" + clientName

	p.recentClientsMu.Lock()
	defer p.recentClientsMu.Unlock()

	now := time.Now()
	if existing, ok := p.recentClients[key]; ok {
		existing.LastRequestAt = now
		existing.RequestCount++
		existing.UserAgent = userAgent
		if parsed.Model != "" {
			existing.Model = parsed.Model
		}
		p.recentClients[key] = existing
	} else {
		p.recentClients[key] = &types.RecentClient{
			ClientName:    clientName,
			ClientIP:      clientIP,
			UserAgent:     userAgent,
			LastRequestAt: now,
			RequestCount:  1,
			Model:         parsed.Model,
		}
	}

	// Очистка старых записей (> 60 секунд)
	cutoff := now.Add(-60 * time.Second)
	for k, rc := range p.recentClients {
		if rc.LastRequestAt.Before(cutoff) && !rc.IsStreamActive {
			delete(p.recentClients, k)
		}
	}
}

// markRecentClientStreamActive — помечает клиента как имеющего активный стриминг
func (p *Proxy) markRecentClientStreamActive(r *http.Request, active bool) {
	clientIP := p.getClientRealIP(r)
	clientName := p.getClientName(r)
	key := clientIP + "::" + clientName

	p.recentClientsMu.Lock()
	defer p.recentClientsMu.Unlock()

	if existing, ok := p.recentClients[key]; ok {
		existing.IsStreamActive = active
		existing.LastRequestAt = time.Now()
		p.recentClients[key] = existing
	}
}

// cleanupZombieSessions — проверяет и очищает зомби-сессии, освобождая бэкенды.
// Вызывается при старте ServeHTTP чтобы предотвратить "все бэкенды заняты"
// из-за старых оборванных сессий.
func (p *Proxy) cleanupZombieSessions() {
	zombies := p.sessionMgr.DetectZombieSessions(p.getZombieSessionThreshold())
	if len(zombies) == 0 {
		return
	}

	logger.Get().Warnw("detected zombie sessions, cleaning up",
		"zombie_count", len(zombies))

	for _, zombie := range zombies {
		logger.Get().Warnw("removing zombie session and releasing backend",
			"session_id", zombie.ID,
			"backend_id", zombie.BackendID,
			"model", zombie.Model,
			"last_request_at", zombie.LastRequestAt,
			"has_active_stream", zombie.HasActiveStream,
		)

		// Освобождаем бэкенд
		if zombie.BackendID != "" {
			p.releaseSlot(zombie.BackendID)
		}

		// Удаляем сессию
		p.sessionMgr.ForceRemove(zombie.ID)
	}
}

// getRecentClients — получение списка активных клиентов для монитора
func (p *Proxy) getRecentClients() []types.RecentClient {
	p.recentClientsMu.RLock()
	defer p.recentClientsMu.RUnlock()

	now := time.Now()
	cutoff := now.Add(-60 * time.Second)
	result := make([]types.RecentClient, 0, len(p.recentClients))

	for _, rc := range p.recentClients {
		if rc.LastRequestAt.After(cutoff) {
			result = append(result, *rc)
		}
	}

	// Сортируем по времени последнего запроса (новые — первые)
	sort.Slice(result, func(i, j int) bool {
		return result[i].LastRequestAt.After(result[j].LastRequestAt)
	})

	return result
}
