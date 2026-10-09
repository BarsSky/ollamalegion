package balancer

import (
	"os"
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// R91 (2026-10-09): уборка «призрачных» записей бэкендов.
//
// Что наблюдалось на живой паре машин. После прогона нагрузочных тестов
// (`scripts/loadtest`, отдельный sdworker, зарегистрировавшийся под ID
// `share-probe`) в `/api/v1/backends?includeUnhealthy=true` и в `state.json`
// осталась запись: тип image_cpp, метки sdworker/auto-registered/image,
// hasAgent=false, статус unhealthy, lastAgentContact=0001-01-01. Она пережила и
// удаление тестового контейнера, и рестарт балансера, потому что лежит в
// `state.json` и удаляется только оператором (DELETE /api/v1/backends/{id}) или
// веткой dedup при регистрации «правильного» бэкенда.
//
// Причина системная, а не тестовая: запись, созданная саморегистрацией узла,
// не имеет срока жизни. Агент, которого больше нет (стенд разобрали, контейнер
// удалили, машину вывели), не может ни перерегистрироваться, ни дерегистрироваться.
//
// Правило: запись живёт, пока жив её агент. Кандидат на уборку — запись
//   - с меткой `auto-registered` (её ставит сама нода: cmd/cppworker,
//     cmd/sdworker; записи, созданные оператором из WebUI, метки не имеют и
//     остаются неприкосновенными — их судьба принадлежит оператору),
//   - без живого агента (hasAgent=false; чекер выше снимает флаг через 60 с
//     молчания),
//   - не healthy и не draining (воркер, который отвечает на health-check,
//     не удаляем даже при мёртвом агенте: инференс по нему ещё работает),
//   - без запросов в полёте,
//   - и молчащая дольше TTL (LB_GHOST_BACKEND_TTL_SEC, default 1800 с).
//
// Удаляется тем же путём, что и операторский DELETE (Proxy.RemoveBackend):
// запись исчезает из пула и из state.json, поэтому не возвращается после
// рестарта. Когда нода вернётся, её агент зарегистрируется заново и создаст
// запись с нуля — как при первом запуске.
//
// Что теряется при уборке: операторские runtime-лимиты (PUT /limits) и
// GPUIndex этой записи. Это осознанный компромисс: запись уничтоженного/
// выведенного узла всё равно не обслуживает запросы, а «вечная» строка в
// панели вводит оператора в заблуждение. Уборку можно выключить целиком
// (LB_GHOST_BACKEND_TTL_SEC=0) или удлинить TTL.

// defaultGhostBackendTTL — сколько молчания агента считается «запись осталась
// от исчезнувшего узла». 30 минут: заметно больше и агентского таймаута (60 с),
// и интервала heartbeat (10-30 с), поэтому случайная сетевая пауза или
// перезапуск агента запись не уносит.
const defaultGhostBackendTTL = 30 * time.Minute

// GhostBackendTTL — TTL уборки из окружения.
//
// LB_GHOST_BACKEND_TTL_SEC:
//   - пусто          → defaultGhostBackendTTL (1800 с);
//   - 0 или меньше   → уборка выключена (возвращает 0);
//   - не число       → default + WARN (не ломаем стенд опечаткой в .env, но и
//     не прячем её молча).
func GhostBackendTTL() time.Duration {
	v := strings.TrimSpace(os.Getenv("LB_GHOST_BACKEND_TTL_SEC"))
	if v == "" {
		return defaultGhostBackendTTL
	}
	sec, err := strconv.Atoi(v)
	if err != nil {
		logger.Get().Warnw("LB_GHOST_BACKEND_TTL_SEC is not a number, using default",
			"value", v,
			"default_sec", int(defaultGhostBackendTTL.Seconds()))
		return defaultGhostBackendTTL
	}
	if sec <= 0 {
		return 0
	}
	return time.Duration(sec) * time.Second
}

// ReapGhostBackends — один проход уборки. Возвращает ID удалённых записей
// (для тестов и логов); пустой слайс = убирать нечего.
func (p *Proxy) ReapGhostBackends(now time.Time) []string {
	ttl := GhostBackendTTL()
	if ttl <= 0 {
		return nil
	}
	return p.reapGhostBackends(now, ttl)
}

// ghostSilentSince — с какого момента запись молчит без агента.
//
// Порядок: живой контакт агента (lastAgentContact) → ранее записанный якорь
// (agentSilentSince) → нулевое время = «отсчёт ещё не начат».
func ghostSilentSince(b *types.Backend) time.Time {
	if b == nil {
		return time.Time{}
	}
	if !b.LastAgentContact.IsZero() {
		return b.LastAgentContact
	}
	if b.AgentSilentSince != nil {
		return *b.AgentSilentSince
	}
	return time.Time{}
}

// reapGhostBackends — ядро уборки с явным TTL (тесты не зависят от env).
func (p *Proxy) reapGhostBackends(now time.Time, ttl time.Duration) []string {
	if p == nil || ttl <= 0 {
		return nil
	}

	type candidate struct {
		id     string
		silent time.Time
		// anchor — запись без контакта с агентом и без якоря: его нужно
		// проставить и сохранить, а срок начнётся со следующего прохода.
		anchor bool
	}

	var (
		candidates []candidate
		anchored   bool
	)

	p.mu.Lock()
	for id, state := range p.backends {
		if state == nil || state.Backend == nil {
			continue
		}
		// Порядок блокировок как в agent_manager.go и cluster_state.go:
		// p.mu, затем state.mu.
		state.mu.Lock()
		b := state.Backend
		eligible := hasLabelFold(b.Labels, "auto-registered") &&
			!b.HasAgent &&
			b.Status != types.StatusHealthy &&
			b.Status != types.StatusDraining &&
			state.ActiveReqs == 0
		silent := ghostSilentSince(b)
		if eligible && silent.IsZero() {
			// Первое наблюдение записи без единого контакта агента: якорь
			// сохраняем в самой записи, иначе рестарт балансера (graceful
			// shutdown перезаписывает state.json) начинал бы отсчёт заново —
			// именно так призрак и переживал перезапуски.
			t := now.UTC()
			b.AgentSilentSince = &t
			anchored = true
		}
		state.mu.Unlock()
		if eligible {
			candidates = append(candidates, candidate{id: id, silent: silent, anchor: silent.IsZero()})
		}
	}
	p.mu.Unlock()

	if anchored {
		// Дебаунс 5 с: якорь переживёт штатный рестарт, а лишних записей на диск
		// на каждом тике не будет.
		p.scheduleSave()
	}

	var removed []string
	for _, c := range candidates {
		if c.anchor {
			continue // только что начали отсчёт — в этом проходе не удаляем
		}
		if now.Sub(c.silent) <= ttl {
			continue
		}
		if !p.isStillGhost(c.id) {
			// Между сканом и удалением агент ожил (heartbeat/перерегистрация) —
			// запись больше не призрак.
			continue
		}
		logger.Get().Warnw("removing stale backend record: its agent has been silent longer than TTL",
			"backend", c.id,
			"silent_since", c.silent.UTC().Format(time.RFC3339),
			"silent_sec", int(now.Sub(c.silent).Seconds()),
			"ttl_sec", int(ttl.Seconds()),
		)
		if err := p.RemoveBackend(c.id); err != nil {
			logger.Get().Warnw("failed to remove stale backend record", "backend", c.id, "error", err)
			continue
		}
		removed = append(removed, c.id)
	}
	return removed
}

// isStillGhost — повторная проверка непосредственно перед удалением: heartbeat
// агента мог прийти между сканом и RemoveBackend (и тогда запись живая).
func (p *Proxy) isStillGhost(backendID string) bool {
	p.mu.RLock()
	state, exists := p.backends[backendID]
	p.mu.RUnlock()
	if !exists || state == nil || state.Backend == nil {
		return false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	b := state.Backend
	return hasLabelFold(b.Labels, "auto-registered") &&
		!b.HasAgent &&
		b.Status != types.StatusHealthy &&
		b.Status != types.StatusDraining &&
		state.ActiveReqs == 0
}
