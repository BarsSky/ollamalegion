package api

// R96 (S5 плана 2026-10-07): POST /api/v1/placement/plan — предлагаемый план
// размещения для мастера первоначальной настройки.
//
// ЧТО ЭТО. Мастер (setup wizard) больше не спрашивает «какой режим работы» —
// он показывает СОСТАВ КЛАСТЕРА и предлагаемый план: какая модель на что
// ложится, с числами (сколько нужно, сколько свободно по узлам и в сумме),
// оценкой трафика раскладки и предполётными замечаниями §7.
//
// ПОЧЕМУ СЧИТАЕТ СЕРВЕР. Предложение обязано совпадать с тем, что потом
// исполнит резолвер: если мастер посчитает «влезает» по своим формулам, а
// резолвер откажет 503-й — оператор получит стенд, который «настроился», но не
// работает. Поэтому числа считает балансер (тот же VRAM-fit, что у auto, те же
// формулы §3, тот же валидатор §7), а JS только рисует.
//
// ЭНДПОИНТ НИЧЕГО НЕ МЕНЯЕТ. Возвращает готовый фрагмент `balancing.placement`
// (+ `balancing.backendGroups`), который применяется отдельной операцией конфига
// после подтверждения оператора (правило стенда).

import (
	"encoding/json"
	"net/http"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/internal/i18n"
)

func (s *Server) placementPlanHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
			"success": false,
			"error":   i18n.T("api.handlers_placement_plan.method_post_only"),
		})
		return
	}

	var req balancer.SetupPlanRequestR96
	if r.Body != nil {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			// Пустое тело допустимо: план по всем известным моделям стенда.
			// Ошибку декодирования при непустом теле показываем как 400.
			if r.ContentLength > 0 {
				s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
					"success": false,
					"error":   i18n.T("api.handlers_placement_plan.bad_request_body", err),
				})
				return
			}
		}
	}

	plan := s.proxy.PlacementPlanR96(req)
	// R166: та же честность, что и в validate — какая скорость канала и откуда.
	bandwidth := s.proxy.LinkBandwidthInfo(req.BandwidthMBps)
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":          true,
		"dryRun":           true,
		"lang":             plan.Lang,
		"plans":            plan.Plans,
		"placement":        plan.Placement,
		"backendGroups":    plan.BackendGroups,
		"warnings":         plan.Warnings,
		"hint":             plan.Hint,
		"discoveredModels": plan.Discovered,
		"bandwidth":        bandwidth,
	})
}
