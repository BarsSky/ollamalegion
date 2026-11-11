// availability_first_r493.go — R493 (2026-11-11): ДОСТУПНОСТЬ КАК ПРЕДИКАТ.
//
// ЖИВОЙ ДЕФЕКТ (разбор: docs/architecture/availability-first-r493.ru.md, по
// архивам второго стенда 2026-11-11). Мастер (A10, 27B загружена, 2.4 ГБ VRAM
// свободно) был занят одной генерацией, рядом простаивал A10-126 (20.5 ГБ
// свободно) — и запрос встал в ОЧЕРЕДЬ к мастеру на 300 с. В логах свободных
// узлов ни одной попытки загрузки: балансер даже не пытался поднять модель там.
//
// ПОЧЕМУ ТАК. Выбор бэкенда судил о занятости по КОЭФФИЦИЕНТУ
// `ActiveReqs / MaxConcurrentReqs` с порогом 0.8. В этом кластере у ВСЕХ узлов
// `maxConcurrentRequests = 1`, поэтому коэффициент принимал только 0 и 1 и
// ничего не говорил о реальном слоте; а «привязка сессии» действовала как замок
// (proxy.go передавал бэкенд сессии в очередь без проверки доступности).
//
// ПРАВИЛО R493. Кандидат «доступен сейчас» — это ПРЕДИКАТ (есть свободный слот),
// а не коэффициент. И если ни один узел с загруженной моделью не принимает
// сейчас, а подходящий свободный узел есть (health, ресурсы, VRAM хватает), то
// заявка едет туда и модель там поднимается — очередь остаётся последним
// вариантом, а не первым.
package balancer

import (
	"fmt"
	"strings"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// hasFreeSlotNowR493 — есть ли на бэкенде свободный слот ПРЯМО СЕЙЧАС.
//
// Отличие от canAcceptRequest: тот смотрит коэффициент с порогом 0.8 и мягкие
// ресурсные пороги (для прогрева/скоринга это правильно). Здесь нужен именно
// ответ «займём ли слот, не встав в очередь».
func (p *Proxy) hasFreeSlotNowR493(backendID string) bool {
	if p == nil || strings.TrimSpace(backendID) == "" {
		return false
	}
	p.mu.RLock()
	state, ok := p.backends[backendID]
	p.mu.RUnlock()
	if !ok || state == nil || state.Backend == nil {
		return false
	}
	if state.Backend.Status != types.StatusHealthy && state.Backend.Status != "" {
		return false
	}
	if p.backendDisabled(backendID) {
		return false
	}
	state.mu.Lock()
	active := state.ActiveReqs
	maxReqs := state.Backend.EffectiveMaxConcurrentRequests()
	state.mu.Unlock()
	if maxReqs <= 0 {
		maxReqs = 1
	}
	return active < maxReqs
}

// stickyBackendIfAvailableR493 — бэкенд привязки сессии, но только если он может
// принять запрос СЕЙЧАС.
//
// R493: привязка — ПРЕДПОЧТЕНИЕ, а не замок. Раньше backend сессии безусловно
// уезжал в ожидание слота (R480/P1-7), и заявка ждала занятый узел, пока рядом
// простаивал подходящий. Правило R480 остаётся в силе для случая «слот есть»:
// прогретая копия побеждает; при занятости — обычный выбор.
func (p *Proxy) stickyBackendIfAvailableR493(sessionKey string) string {
	if p == nil || sessionKey == "" || p.sessionMgr == nil {
		return ""
	}
	s, ok := p.sessionMgr.GetSnapshot(sessionKey)
	if !ok || s.BackendID == "" {
		return ""
	}
	if !p.hasFreeSlotNowR493(s.BackendID) {
		if logger.Get() != nil {
			logger.Get().Infow("R493: привязка сессии не действует — у бэкенда нет свободного слота, решает доступность",
				"backend", s.BackendID, "session", sessionKey)
		}
		return ""
	}
	return s.BackendID
}

// pickLoadableFreeBackendR493 — подходящий СВОБОДНЫЙ бэкенд, где модель можно
// поднять (health + свободный слот + ресурсы + VRAM хватает).
//
// Раньше загрузку на свободном узле запускал только этап P3 и только при
// `SyncModelLoad.Enabled`; при срабатывании P1 (модель загружена у занятого
// узла) до P3 дело не доходило. Здесь — отдельный выбор, который вызывается
// ПОСЛЕ того, как выяснилось, что принимающего узла среди loaded нет.
//
// КОГО РАССМАТРИВАЕМ:
//   - P3 (FREE): узел без модели в памяти и со свободным слотом — обычный случай;
//   - узлы ТОЛЬКО из P4 (FALLBACK): это, как правило, узлы БЕЗ МЕТРИК. Именно они
//     дали живой дефект: computeNoMetricsScore даёт такому узлу score 1, занятый
//     мастер выглядит лучше по score, и заявка встаёт в очередь к мастеру, хотя
//     свободная машина простаивает. Загруженные узлы, не попавшие в P1 из-за
//     порога прогрева, НЕ рассматриваются: их выбирать «вместо очереди» нельзя,
//     иначе порог LB_PREWARM_TRIGGER_LOAD перестал бы действовать.
//
// КАК ВЫБИРАЕМ: R248 (VRAM-fit) — узел, где модель не влезает, отбрасывается
// (27B на 8-гигабайтной карте не поднимется, R493 п.5). Среди оставшихся —
// обычный score того же вида, что в P4: свободный узел не должен выигрывать у
// более подходящего только потому, что перебор шёл по map. «VRAM неизвестна» не
// запрещает выбор — иначе стенд без метрик не поднимал бы ничего.
// vramNeedVsFreeR493 — сколько нужно под модель и сколько свободно у узла.
//
// Одна и та же арифметика для двух мест, чтобы они не разошлись: выбор узла под
// загрузку (pickLoadableFreeBackendR493) и причина отказа в why_queued
// (backendRejectionsR460). Правило R248/R493: потребность = размер файла × 1.05
// (KV-cache и буферы контекста), «нет данных» не считается отказом.
//
// known=false, если размер модели неизвестен ИЛИ у узла нет данных о VRAM:
// тогда причину называть нечем, и вызывающий оставляет прежнюю формулировку.
func (p *Proxy) vramNeedVsFreeR493(backendID, model string) (needMB float64, freeMB int, known bool) {
	if p == nil {
		return 0, 0, false
	}
	sizeBytes := p.getModelSizeBytes(model)
	if sizeBytes <= 0 {
		return 0, 0, false
	}
	needMB = float64(sizeBytes) / (1024 * 1024) * vramFitSlack
	freeMB, vrOK := p.backendVramFreeMB(backendID)
	if !vrOK {
		return needMB, 0, false
	}
	return needMB, freeMB, true
}

// formatGiBR493 — МБ в «N.N GiB» для сообщений оператору. Гибибайты, потому что
// и размер модели, и свободная VRAM считаются в МиБ; «ГБ» в подписи путало бы.
func formatGiBR493(mb float64) string {
	return fmt.Sprintf("%.1f GiB", mb/1024)
}

// modelLoadedSomewhereR493 — есть ли модель в памяти ХОТЯ БЫ на одном узле,
// который вообще рассматривается для этой заявки.
//
// ЗАЧЕМ ЭТОТ ПРЕДИКАТ. R493 решает узкий случай: модель УЖЕ загружена, но её
// узел занят, а рядом простаивает свободный. Без такой проверки этап 3b
// превращался в «выбери первый свободный по score» и забирал заявки у обычного
// распределения: живой регресс — TestReplication_Scenario_LiveRequestReplicated
// (10 запросов к двум узлам БЕЗ метрик уходили на один, второй получал 0, а
// алгоритм round-robin переставал действовать). Модель не загружена нигде —
// значит, никакого «занятого узла с прогретой копией» нет, и решать должен
// обычный выбор, а не этот этап.
func (p *Proxy) modelLoadedSomewhereR493(model string, allowedTypes []types.BackendType) bool {
	if p == nil || model == "" || p.metricsMgr == nil {
		return false
	}
	p.mu.RLock()
	ids := make([]string, 0, len(p.backends))
	for id, state := range p.backends {
		if state == nil || state.Backend == nil {
			continue
		}
		if isDisabledBackendState(state) || p.cannotHostModelR304(id) {
			continue
		}
		if !isBackendTypeAllowed(normalizeBackendType(state.Backend.Type), allowedTypes) {
			continue
		}
		ids = append(ids, id)
	}
	p.mu.RUnlock()
	for _, id := range ids {
		if p.backendHasModelByID(id, model) {
			return true
		}
	}
	return false
}

func (p *Proxy) pickLoadableFreeBackendR493(model string, allowedTypes []types.BackendType) string {
	if p == nil {
		return ""
	}
	// См. modelLoadedSomewhereR493: «загрузка вместо очереди» применима только
	// тогда, когда прогретая копия где-то есть и занята. Иначе распределение
	// заявок остаётся за обычным алгоритмом.
	if !p.modelLoadedSomewhereR493(model, allowedTypes) {
		return ""
	}
	sizeBytes := p.getModelSizeBytes(model)
	needMB := 0.0
	if sizeBytes > 0 {
		needMB = float64(sizeBytes) / (1024 * 1024) * vramFitSlack
	}

	// Группировка кандидатов. Сначала собираем ВСЕ группы, потом считаем:
	//   свободные         — P3;
	//   «только fallback» — P4 минус P1/P2/P3 (обычно узлы без метрик).
	var freeGroup, fallbackGroup []string
	loadedOrWarming := make(map[string]bool, 8)
	for _, group := range p.expandCandidates(model, allowedTypes) {
		for _, id := range group.BackendIDs {
			switch group.Priority {
			case 1, 2:
				loadedOrWarming[id] = true
			case 3:
				freeGroup = append(freeGroup, id)
			case 4:
				fallbackGroup = append(fallbackGroup, id)
			}
		}
	}
	freeSet := make(map[string]bool, len(freeGroup))
	for _, id := range freeGroup {
		freeSet[id] = true
	}
	candidates := make([]string, 0, len(freeGroup)+len(fallbackGroup))
	candidates = append(candidates, freeGroup...)
	for _, id := range fallbackGroup {
		if loadedOrWarming[id] || freeSet[id] {
			continue
		}
		candidates = append(candidates, id)
	}

	var bestID string
	bestScore := -1.0
	for _, backendID := range p.orderCandidatesByVramFit(candidates, model) {
		if !p.hasFreeSlotNowR493(backendID) {
			continue
		}
		if !p.canAcceptRequest(backendID) {
			continue
		}
		state := p.backends[backendID]
		if state == nil {
			continue
		}
		// Phase 6: пока на хосте идёт image-генерация, текстовую модель там не
		// поднимаем — GPU занят картинкой, и загрузка (140-260 с) всё равно
		// встанет в очередь за локом.
		if p.imageGateBlocksTextBackend(state) {
			continue
		}
		if needMB > 0 {
			if freeMB, known := p.backendVramFreeMB(backendID); known && float64(freeMB) < needMB {
				if logger.Get() != nil {
					logger.Get().Infow("R493: свободный узел пропущен — модель не влезает в свободную VRAM",
						"backend", backendID, "model", model,
						"need_mb", int(needMB), "free_mb", freeMB)
				}
				continue
			}
		}
		score := p.calculateScore(backendID)
		if score > bestScore {
			bestScore = score
			bestID = backendID
		}
	}
	if bestID != "" && logger.Get() != nil {
		logger.Get().Infow("R493: свободный узел выбран под загрузку модели",
			"backend", bestID, "model", model, "score", bestScore)
	}
	return bestID
}
