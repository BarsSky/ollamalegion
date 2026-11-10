// backend_rejections_r460.go — R460 (2026-11-06): когда запрос уходит в
// ОЧЕРЕДЬ, в логе и в теле 503 видно, почему не подошёл КАЖДЫЙ бэкенд.
//
// ЖИВАЯ ЖАЛОБА (второй стенд, A10 ×2, 50 ГБ, Qwen3.8-27B): «есть свободный
// бэкенд, а запрос встал в очередь и ждал». По прежним логам ответить было
// нельзя: писалась одна строка «admission: request queued» без причин, а выбор
// бэкенда — молчаливая последовательность фильтров. Оператор видел простой и не
// мог отличить «свободен, но выключен оператором» от «свободен, но модели нет»
// (а это, по R459, осознанное решение: холодную копию не берём) и от «свободен,
// но это донор раскладки».
//
// ПРАВИЛО: причина отсутствия кандидата — такое же наблюдаемое значение, как
// окно или загрузка. Список причин строится ТЕМИ ЖЕ предикатами, что и реальный
// отбор (не копией условий), иначе он однажды начнёт врать.
package balancer

import (
	"fmt"
	"sort"
	"strings"

	"ollama-loadbalancer/pkg/types"
)

// backendRejectionR460 — почему конкретный бэкенд не может взять запрос ПРЯМО
// СЕЙЧАС. Reason — машинный код (для фильтра в логах), Detail — числа для
// человека.
type backendRejectionR460 struct {
	BackendID string `json:"backend_id"`
	Reason    string `json:"reason"`
	Detail    string `json:"detail,omitempty"`
}

// Коды причин (стабильные, их видно в логе и в теле 503).
const (
	rejectDonorOrWorkerR460 = "donor_or_group_worker"
	rejectDisabledR460      = "disabled_by_operator"
	rejectNotHealthyR460    = "not_healthy"
	rejectTypeR460          = "type_not_allowed"
	rejectNoMetricsR460     = "no_metrics"
	rejectModelAbsentR460   = "model_not_loaded"
	rejectResourceLimitR460 = "resource_limit"
	rejectNoSlotsConfigR460 = "no_slots_configured"
	rejectSlotsBusyR460     = "all_slots_busy"
	// R479: селектор отбрасывает бэкенд при загрузке ≥ превью-порога (80% по
	// умолчанию), но в списке причин этого не было — «свободен (1 слот из 10),
	// а в очереди» оставалось необъяснимым.
	rejectOverLoadThresholdR460 = "over_load_threshold"
	// R480 (P1-8): кандидат не проходит гейт «нужного окна» R459. Раньше такая
	// причина существовала только в отдельной INFO-строке R459, а why_queued
	// показывал model_not_loaded/over_load_threshold — не то, что решило дело.
	rejectWindowTooSmallR460 = "window_too_small"
)

// backendRejectionMoveR480 — контекст переезда сессии для списка причин.
// Передаётся вариативно, чтобы не менять сигнатуру у существующих вызовов.
type backendRejectionMoveR480 struct {
	Need       int
	NeedSource string
}

// backendRejectionsR460 — по одной причине на каждый бэкенд, который НЕ может
// обслужить запрос сейчас. Бэкенды, способные его взять, в список не попадают:
// если список непуст, а запрос всё равно в очереди — значит его забрал кто-то
// быстрее (это другая ситуация, и она видна в строке про захват слота).
func (p *Proxy) backendRejectionsR460(model string, bt types.BackendType, move ...*backendRejectionMoveR480) []backendRejectionR460 {
	if p == nil || len(p.backends) == 0 {
		return nil
	}
	var mv *backendRejectionMoveR480
	if len(move) > 0 {
		mv = move[0]
	}
	p.mu.RLock()
	defer p.mu.RUnlock()

	allowed := p.getAllowedTypesList(bt)
	out := make([]backendRejectionR460, 0, len(p.backends))
	for id, state := range p.backends {
		if state == nil || state.Backend == nil {
			continue
		}
		reason, detail := "", ""
		switch {
		case p.cannotHostModelR304(id):
			// Донор раскладки или воркер группы: своих копий не держит.
			reason = rejectDonorOrWorkerR460
			detail = "своих копий модели не держит (раскладка/донор)"
		case isDisabledBackendState(state):
			reason = rejectDisabledR460
			detail = "выключен оператором («не использовать»)"
		case state.Backend.Status != types.StatusHealthy:
			reason = rejectNotHealthyR460
			detail = "статус " + string(state.Backend.Status)
		case !isBackendTypeAllowed(normalizeBackendType(state.Backend.Type), allowed):
			reason = rejectTypeR460
			detail = "тип " + string(state.Backend.Type) + " не подходит для этого запроса"
		}

		if reason == "" {
			if p.metricsMgr == nil {
				reason = rejectNoMetricsR460
				detail = "метрик нет — загрузку модели проверить нельзя"
			} else if metrics, ok := p.metricsMgr.SnapshotBackendMetrics(id); !ok {
				reason = rejectNoMetricsR460
				detail = "метрик нет — загрузку модели проверить нельзя"
			} else if !p.backendHasModel(metrics, model) {
				// Это НЕ ошибка: по R459 холодную копию под живую сессию не
				// берём. Но оператор должен видеть, что «свободный» бэкенд
				// свободен ровно потому, что модели на нём нет.
				reason = rejectModelAbsentR460
				detail = "модель не загружена (загрузка = минуты и, возможно, меньшее окно)"
			}
		}
		if reason == "" && mv != nil && mv.Need > 0 {
			// R480 (P1-8): «нужное окно» — такая же причина отказа, как слоты.
			if ok, why := p.sessionMoveKeepsWindowNeedR480(id, model, mv.Need, mv.NeedSource); !ok {
				reason = rejectWindowTooSmallR460
				detail = why
			}
		}
		if reason == "" && !p.checkResourceLimitsLocked(id) {
			reason = rejectResourceLimitR460
			detail = "не проходит по лимитам ресурсов (VRAM/RAM)"
		}
		if reason == "" {
			state.mu.Lock()
			active, maxReqs := state.ActiveReqs, state.Backend.MaxConcurrentReqs
			state.mu.Unlock()
			if maxReqs <= 0 {
				reason = rejectNoSlotsConfigR460
				detail = "не задано maxConcurrentReqs — брать запросы нельзя"
			} else if active >= maxReqs {
				reason = rejectSlotsBusyR460
				detail = fmt.Sprintf("заняты все слоты (%d/%d)", active, maxReqs)
			} else if ratio := float64(active) / float64(maxReqs); ratio >= p.loadThresholdR479() {
				// R479: ровно тот порог, с которым селектор пропускает бэкенд.
				reason = rejectOverLoadThresholdR460
				detail = fmt.Sprintf("загрузка %.0f%% ≥ порога %.0f%% (свободно %d из %d слотов)",
					ratio*100, p.loadThresholdR479()*100, maxReqs-active, maxReqs)
			}
		}
		if reason == "" {
			continue // бэкенд способен взять запрос — это не «отказ»
		}
		out = append(out, backendRejectionR460{BackendID: id, Reason: reason, Detail: detail})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BackendID < out[j].BackendID })
	return out
}

// rejectionsSummaryR460 — одна строка для лога: «b1=all_slots_busy(заняты все
// слоты (1/1)); b2=disabled_by_operator».
func rejectionsSummaryR460(list []backendRejectionR460) string {
	if len(list) == 0 {
		return ""
	}
	parts := make([]string, 0, len(list))
	for _, r := range list {
		if r.Detail == "" {
			parts = append(parts, r.BackendID+"="+r.Reason)
			continue
		}
		parts = append(parts, r.BackendID+"="+r.Reason+"("+r.Detail+")")
	}
	return strings.Join(parts, "; ")
}

// loadThresholdR479 — порог загрузки, при котором селектор не берёт бэкенд.
// Одно место с истиной: раньше число 0.80 жило только в селекторе, а причины
// отказов R460 его не знали.
func (p *Proxy) loadThresholdR479() float64 {
	if p != nil && p.config != nil && p.config.Balancing.Prewarm.TriggerLoadThreshold > 0 {
		return p.config.Balancing.Prewarm.TriggerLoadThreshold
	}
	return 0.80
}
