package balancer

// backend_disable_r250.go — R250 (2026-10-28): «не использовать бэкенд».
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА (R248/R249). Нужно исключать узел из выбора
// балансером, НЕ останавливая его. До этой правки способов было два, и оба
// плохие:
//   - `docker stop` узла: запись либо исчезает из панели (пока нет роли), либо
//     висит offline — настраивать узел неудобно, а вернуть его в работу можно
//     только подняв контейнер;
//   - снять роль (`standalone`): ломает раскладку, к которой узел относится, и
//     главный остаётся без доноров.
//
// РЕШЕНИЕ. Флаг в записи бэкенда (types.Backend.Disabled + DisabledReason +
// DisabledAt), который читает СЕЛЕКТОР. Выключенный бэкенд:
//   - не попадает в кандидаты (expandCandidates) и не выбирается
//     (selectBackend/selectByResources/selectBackendExcluding и «ручные» поиски
//     узлов под модель/переезд);
//   - не становится мастером раскладки и не отдаётся репликацией: иначе запрос
//     уходил бы на выключенный узел в обход селектора;
//   - ОСТАЁТСЯ в GET /api/v1/backends, в статусах, метриках и раскладке: его
//     видно, его можно настраивать, и он возвращается одним переключением;
//   - уже идущие на нём запросы НЕ обрываются: флаг влияет только на выбор, а
//     рвать живую генерацию из-за галочки в панели — хуже, чем доиграть её.
//
// ПЕРСИСТ. Как у ролей (R118) и тумблера раскладки (R229): в bundled-
// развёртывании config.json смонтирован read-only, а запись бэкенда
// пересобирается саморегистрацией агента. Поэтому решение оператора живёт ещё и
// в sidecar `runtime-overrides/backend-disabled.json` (см. internal/api), а
// балансер умеет приводить запись к нему повторно — запись могла быть собрана
// заново путём, который про флаг не знает.

import (
	"strings"
	"time"

	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// SetBackendDisabled — записать решение оператора «не использовать бэкенд».
//
// disabled == true: бэкенд исключается из выбора, сохраняются причина и момент
// (аудит: через неделю оператор не вспомнит, почему узел выключен).
// disabled == false: флаг, причина и момент снимаются — узел снова кандидат.
//
// Возвращает false, если бэкенда нет. Идемпотентен: повторная запись того же
// значения не тянет autosave state.json на каждом тике восстановления.
func (p *Proxy) SetBackendDisabled(backendID string, disabled bool, reason string) bool {
	if p == nil || backendID == "" {
		return false
	}

	p.mu.Lock()
	state, ok := p.backends[backendID]
	if !ok || state == nil || state.Backend == nil {
		p.mu.Unlock()
		return false
	}

	reason = strings.TrimSpace(reason)
	state.mu.Lock()
	changed := state.Backend.Disabled != disabled ||
		(disabled && state.Backend.DisabledReason != reason)
	state.Backend.Disabled = disabled
	if disabled {
		state.Backend.DisabledReason = reason
		if changed || state.Backend.DisabledAt == nil {
			now := time.Now().UTC()
			state.Backend.DisabledAt = &now
		}
	} else {
		state.Backend.DisabledReason = ""
		state.Backend.DisabledAt = nil
	}
	state.mu.Unlock()
	p.mu.Unlock()

	if changed {
		logger.Get().Infow(i18n.T("balancer.backend_disable.changed"),
			"backend", backendID, "disabled", disabled, "reason", reason)
		p.scheduleSave()
	}
	return true
}

// BackendDisabledInfo — состояние флага для API/WebUI.
//
// ok == false означает «бэкенда нет», а не «выключен». since — момент
// выключения (nil, если бэкенд включён).
func (p *Proxy) BackendDisabledInfo(backendID string) (disabled bool, reason string, since *time.Time, ok bool) {
	if p == nil || backendID == "" {
		return false, "", nil, false
	}

	p.mu.RLock()
	defer p.mu.RUnlock()

	state, exists := p.backends[backendID]
	if !exists || state == nil || state.Backend == nil {
		return false, "", nil, false
	}
	state.mu.Lock()
	defer state.mu.Unlock()

	out := *state.Backend
	return out.Disabled, out.DisabledReason, cloneTimePtr(out.DisabledAt), true
}

// isDisabledBackendState — выключен ли бэкенд (без блокировки).
//
// Чтение без защёлки сознательно: селекторы уже держат p.mu.RLock, а запись флага
// идёт под p.mu.Lock (та же дисциплина, что у Status, который эти циклы читают
// так же). Отдельный state.mu здесь не берём, чтобы не менять порядок блокировок
// внутри циклов выбора.
func isDisabledBackendState(state *BackendState) bool {
	if state == nil || state.Backend == nil {
		return false
	}
	return state.Backend.Disabled
}

// backendDisabled — выключен ли бэкенд по ID (для путей, работающих вне
// селектора: мастер раскладки, репликация).
func (p *Proxy) backendDisabled(backendID string) bool {
	if p == nil || backendID == "" {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return isDisabledBackendState(p.backends[backendID])
}

// LoadedModelNames — имена моделей, которые СЕЙЧАС держатся на бэкенде.
//
// Зачем в balancer, а не в API: источник у двух типов бэкендов разный (Ollama
// отдаёт runningModels в метриках, llama_cpp — loadedModels в своих метриках), и
// склеивать их должен тот, кто эти метрики уже держит. Ошибка «ноль моделей» тут
// была бы вредной: по этому списку панель предупреждает оператора, что модель на
// выключаемом узле сейчас работает и продолжит работать.
func (p *Proxy) LoadedModelNames(backendID string) []string {
	out := make([]string, 0, 4)
	if p == nil || backendID == "" || p.metricsMgr == nil {
		return out
	}

	seen := make(map[string]bool, 4)
	add := func(name string) {
		n := strings.TrimSpace(name)
		if n == "" || seen[n] {
			return
		}
		seen[n] = true
		out = append(out, n)
	}

	if metrics, ok := p.metricsMgr.SnapshotBackendMetrics(backendID); ok && metrics != nil {
		for _, m := range metrics.Ollama.RunningModels {
			add(m.Name)
		}
	}
	if lm, ok := p.metricsMgr.SnapshotLlamaCppMetrics(backendID); ok && lm != nil {
		for _, m := range lm.LoadedModels {
			add(m.Name)
		}
	}
	return out
}

// disabledSinceRFC3339 — момент выключения строкой для снапшота кластера
// (пусто = бэкенд не выключен).
func disabledSinceRFC3339(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// warnIfAllCandidatesDisabled — понятная причина «кандидатов нет» вместо тишины.
//
// Живой сценарий: оператор исключил все узлы (или единственный), запрос получает
// 503, и без этой записи в логе причина неотличима от «нет здоровых бэкендов»,
// «нет модели» и перегрузки. Проверяем только тип/статус, без VRAM и слотов:
// задача — ответить на вопрос «а не выключил ли я всё сам».
func (p *Proxy) warnIfAllCandidatesDisabled(model string, allowedTypes []types.BackendType) {
	if p == nil {
		return
	}
	p.mu.RLock()
	disabledIDs := make([]string, 0, 4)
	healthyAlive := 0
	for id, state := range p.backends {
		if state == nil || state.Backend == nil || p.isGroupWorkerR92(id) {
			continue
		}
		if !isBackendTypeAllowed(normalizeBackendType(state.Backend.Type), allowedTypes) {
			continue
		}
		if !isDisabledBackendState(state) {
			healthyAlive++
			continue
		}
		disabledIDs = append(disabledIDs, id)
	}
	p.mu.RUnlock()

	if len(disabledIDs) == 0 {
		return
	}
	if healthyAlive > 0 {
		// Есть включённые узлы — «всё выключено» неверно; причину отсутствия
		// кандидатов назовут обычные проверки (модель/слоты/ресурсы).
		return
	}
	logger.Get().Warnw(i18n.T("balancer.backend_disable.all_disabled"),
		"model", model, "backends", disabledIDs)
}
