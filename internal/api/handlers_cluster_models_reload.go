// handlers_cluster_models_reload.go — R639 (2026-11-19): перезагрузка модели на кластере: диспетчер пути, разбор запроса, исполнение по бэкендам.
//
// ЗАЧЕМ ВЫНЕСЕНО. `handlers_cluster_models.go` держал 1628 строк:
// агрегированные списки, перезагрузку модели, массовые операции и состояние
// модели на бэкенде. Здесь — один из слоёв; поведение не менялось.
package api

import (
	"encoding/json"
	"net/http"
	"ollama-loadbalancer/pkg/logger"
)

// clusterModelItemDispatcher — единый dispatcher для /api/v1/cluster/models/{name}/*.
//
// Маршрутизирует по суффиксу URL:
//   - /{name}/info  → GET  → clusterModelInfoHandler  (см. handlers_cluster_model_info.go)
//   - /{name}/reload → POST → clusterReloadModelHandler (load/unload/reload)
//
// Без dispatcher'а оба handler'а конкурировали бы за один prefix-pattern
// "/api/v1/cluster/models/" в net/http ServeMux, и порядок регистрации
// определял бы, кто "съест" запрос. Этот dispatcher делает routing явным
// и устойчивым к добавлению новых sub-resources в будущем (например,
// /{name}/stats, /{name}/metrics).
func (s *Server) clusterModelItemDispatcher(w http.ResponseWriter, r *http.Request) {
	// Splitting on /info, /reload — оставляем общий splitClusterModelPath для {name}.
	// Конкретный суффикс определяем по последнему сегменту URL.
	const (
		suffixInfo   = "/info"
		suffixReload = "/reload"
	)
	path := r.URL.Path
	switch {
	case hasSuffix(path, suffixInfo):
		// GET /api/v1/cluster/models/{name}/info
		s.clusterModelInfoHandler(w, r)
	case hasSuffix(path, suffixReload):
		// POST /api/v1/cluster/models/{name}/reload
		s.clusterReloadModelHandler(w, r)
	default:
		s.writeJSON(w, http.StatusNotFound, map[string]string{
			"error":   "unknown cluster model sub-resource",
			"message": "supported: /api/v1/cluster/models/{name}/info (GET), /api/v1/cluster/models/{name}/reload (POST)",
			"path":    path,
		})
	}
}

// hasSuffix — строковый helper, аналог strings.HasSuffix (избегаем импорта strings здесь).
func hasSuffix(s, suffix string) bool {
	if len(s) < len(suffix) {
		return false
	}
	return s[len(s)-len(suffix):] == suffix
}

// clusterReloadModelHandler — POST /api/v1/cluster/models/{name}/reload.
//
// Управляет жизненным циклом модели через балансировщик: пользователь может
// загружать/выгружать/перезагружать модели, не обращаясь напрямую к cppworker
// (порт 18092). Особенно полезно, когда cppworker находится в Docker-network,
// недоступной из внешнего мира — все запросы идут через balancer (18080/18081).
//
// Формат запроса:
//
//	POST /api/v1/cluster/models/gemma-4-E4B-it-Q4_K_M/reload
//	{
//	  "operation":   "load",     // или "unload" / "reload"
//	  "contextSize": 32768,
//	  "gpuLayers":   -2,         // -2 = auto
//	  "reason":      "manual reload from UI"
//	}
//
// Endpoint проксирует запросы на cppworker через существующий
// /api/v1/backends/{id}/models (POST) handler через ModelManager.ExecuteOperation,
// который уже умеет делать load/unload и возвращать корректные статусы.
//
// Требует аутентификации.
func (s *Server) clusterReloadModelHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error": "method not allowed",
		})
		return
	}
	// R66d: reload = unload + load, может длиться минуты; серверный
	// WriteTimeout=60s (cmd/balancer/main.go:383) обрывает ответ. Снимаем deadline.
	extendWriteDeadline(w)

	// Извлекаем имя модели из URL: /api/v1/cluster/models/{name}/reload
	name, ok := splitClusterModelPath(r.URL.Path)
	if !ok || name == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing model name in URL (expected /api/v1/cluster/models/{name}/reload)",
		})
		return
	}

	var req clusterReloadModelRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{
				"error":   "invalid request body",
				"message": err.Error(),
			})
			return
		}
	}

	// По умолчанию operation = "load".
	if req.Operation == "" {
		req.Operation = "load"
	}

	// Валидация операции.
	switch req.Operation {
	case "load", "unload", "reload", "all": // "all" — обратная совместимость с предыдущей версией
		// OK
	default:
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid operation",
			"message": "supported operations: load, unload, reload",
		})
		return
	}

	// Семантический алиас: "all" / "reload" → unload + load.
	if req.Operation == "all" || req.Operation == "reload" {
		req.Operation = "reload"
	}

	// R263 (2026-10-28): собираем цели загрузки С ПРОВЕРКОЙ СОСТОЯНИЯ узла.
	//
	// ЖИВОЙ ФАКТ (стенд r262): без явного backendId операция уходила на
	// offline/unhealthy узел `cppworker-cyborg-bundled-agent` — «нажал загрузить,
	// ответа нет». Выбор состояния не проверял вовсе (см. reload_targets.go).
	sel := s.selectReloadTargetsChecked(req.BackendID)

	// Явно указанный узел непригоден → отвечаем СРАЗУ и по адресу, а не через
	// многоминутный таймаут: оператор выбрал конкретный узел и должен знать, что
	// именно с ним не так.
	if sel.Explicit && len(sel.Targets) == 0 {
		code := http.StatusServiceUnavailable
		if sel.ExplicitReason == "backend not found in cluster state" {
			code = http.StatusNotFound
		}
		msg := map[string]interface{}{
			"error":     "backend is not usable for model load",
			"message":   sel.ExplicitReason,
			"backendId": req.BackendID,
			"model":     name,
			"operation": req.Operation,
		}
		address := ""
		if len(sel.Skipped) > 0 {
			address = sel.Skipped[0].Address
			msg["skipped"] = sel.Skipped
			if address != "" {
				msg["address"] = address
			}
		}
		logger.Get().Warnw("cluster model operation refused: backend is not usable",
			"model", name, "operation", req.Operation,
			"backend", req.BackendID, "reason", sel.ExplicitReason,
			"address", address)
		s.writeJSON(w, code, msg)
		return
	}

	if len(sel.Targets) == 0 {
		// Различаем «узлов нет вовсе» (прежние 404) и «есть, но все непригодны»
		// (503 + причины): во втором случае оператору нужно чинить узлы, а не искать
		// несуществующие бэкенды.
		if sel.LlamaCppTotal == 0 {
			s.writeJSON(w, http.StatusNotFound, map[string]string{
				"error":   "no llama_cpp backends available",
				"message": "request a model reload requires at least one llama_cpp backend",
			})
			return
		}
		logger.Get().Warnw("cluster model operation refused: no usable backends",
			"model", name, "operation", req.Operation, "skipped", len(sel.Skipped))
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
			"error":   "no usable llama_cpp backend",
			"message": "every llama_cpp backend is offline, unhealthy or excluded by the operator",
			"model":   name,
			"skipped": sel.Skipped,
		})
		return
	}

	logger.Get().Infow("cluster model operation requested",
		"model", name,
		"operation", req.Operation,
		"backend_id", req.BackendID,
		"targets", len(sel.Targets),
		"skipped", len(sel.Skipped),
		"context_size", ctxSizeForLog(req.ContextSize),
		"gpu_layers", gpuLayersForLog(req.GPULayers),
		"reason", req.Reason,
	)
	for _, sk := range sel.Skipped {
		// Одна строка на пропущенный узел: оператор должен видеть, что узел не
		// тронут и почему (а не искать его в результатах).
		logger.Get().Infow("model load target skipped",
			"model", name, "backend", sk.BackendID,
			"address", sk.Address, "reason", sk.Reason)
	}

	resp := clusterReloadModelResponse{
		Model:     name,
		Operation: req.Operation,
		Results:   make([]clusterReloadModelBackendResult, 0, len(sel.Targets)),
		Skipped:   sel.Skipped,
	}

	for _, backendID := range sel.Targets {
		result := s.executeReloadOnBackend(r.Context(), backendID, name, &req)
		resp.Results = append(resp.Results, result)
	}

	// R287: общий HTTP-код должен отражать исход, а не всегда быть 200.
	//
	// Живой дефект: ручной reload при занятой модели отвечал HTTP 200 с
	// `success:false`, а рядом — `reloaded:true`. Клиент (и WebUI, и оператор)
	// не мог понять, применилось ли что-то. Теперь:
	//   - хотя бы один бэкенд применил → 200 (частичный успех виден по results);
	//   - все отложены → 503 + per-backend deferred;
	//   - иначе — реальная ошибка (5xx по первому такому результату).
	s.writeJSON(w, overallReloadStatus(resp.Results), resp)
}
