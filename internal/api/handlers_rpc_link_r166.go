// handlers_rpc_link_r166.go — R166 (2026-10-17): ручка «замерить канал сейчас» и
// ответ с разрешённой пропускной способностью.
//
// ЗАЧЕМ. Канал — ключевой параметр для раскладки слоёв между хостами: по нему
// оценивается трафик (composition.EstimateTraffic) и принимается решение
// «layer или row», поэтому оператору нужен не .env с ручным числом, а две вещи:
//
//  1. АВТОопределение при инициализации (балансер сам замеряет канал до узлов
//     раскладки — см. rpc_link_measure_r118.go, первый замер через 30 с после
//     старта и далее раз в LB_RPC_LINK_MEASURE_SEC);
//  2. возможность ПЕРЕзамерить по кнопке — этот файл.
//
// Ответ намеренно показывает и по-узловые числа, и «что пойдёт в оценку» (effective):
// оператор видит, почему в плане именно такая скорость.
package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// rpcLinkMeasureHandler — POST /api/v1/rpc/link/measure
//
// Тело (необязательно): {"backendId": "<id>"} — замерить только этот узел.
// Без тела (или с пустым backendId) замеряются все участники раскладки (main|donor).
func (s *Server) rpcLinkMeasureHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
			"success": false,
			"error":   "method not allowed (только POST)",
		})
		return
	}
	if s.proxy == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
			"success": false,
			"error":   "балансер недоступен",
		})
		return
	}

	var req struct {
		BackendID string `json:"backendId"`
	}
	if r.Body != nil && r.ContentLength > 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
				"success": false,
				"error":   "некорректное тело запроса: " + err.Error(),
			})
			return
		}
	}

	targets := make([]string, 0, 4)
	if id := strings.TrimSpace(req.BackendID); id != "" {
		targets = append(targets, id)
	} else {
		for _, b := range s.proxy.GetAllBackends() {
			role := strings.TrimSpace(b.Role)
			if role != types.BackendRoleDonor && role != types.BackendRoleMain {
				continue
			}
			targets = append(targets, b.ID)
		}
	}
	if len(targets) == 0 {
		s.writeJSON(w, http.StatusConflict, map[string]interface{}{
			"success": false,
			"error":   "нет узлов раскладки: назначьте роли main/donor (без них канал мерить не к чему)",
		})
		return
	}

	results := make([]map[string]interface{}, 0, len(targets))
	for _, id := range targets {
		mbps, err := s.proxy.MeasureRPCLinkForBackend(id)
		item := map[string]interface{}{
			"backendId": id,
			"mbps":      mbps,
			"mbpsSlash": float64(mbps) / 8, // МБ/с — то, что ждёт оценка трафика
		}
		if b := s.proxy.GetBackend(id); b != nil {
			item["node"] = b.Host
			item["role"] = b.Role
		}
		if err != nil {
			item["error"] = err.Error()
		}
		results = append(results, item)
	}

	// Effective — что фактически пойдёт в оценку трафика прямо сейчас.
	effective := s.proxy.LinkBandwidthInfo(0)
	logger.Get().Infow("RPC link measure по запросу оператора",
		"targets", len(targets), "effective_mbps", effective.Mbps, "source", effective.Source)

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":   true,
		"results":   results,
		"effective": effective,
	})
}
