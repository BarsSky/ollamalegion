package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// aggregateLoadedModel — плоское представление загруженной модели для cluster-уровня.
// Позволяет UI и пользовательским скриптам видеть, на каких бэкендах загружена модель
// и с какими параметрами — через один запрос к балансировщику (порт 18081).
type aggregateLoadedModel struct {
	Name          string `json:"name"`
	BackendID     string `json:"backendId"`
	BackendType   string `json:"backendType"`
	Engine        string `json:"engine,omitempty"`
	ContextLength int    `json:"contextLength,omitempty"`
	BatchSize     int    `json:"batchSize,omitempty"`
	NumGPULayers  int    `json:"numGpuLayers,omitempty"`
	Quantization  string `json:"quantization,omitempty"`
	VRAMUsage     uint64 `json:"vramUsage,omitempty"`
	RAMUsage      uint64 `json:"ramUsage,omitempty"`
	Size          uint64 `json:"size,omitempty"`
	State         string `json:"state,omitempty"`
}

// aggregateLoadingModel — модель, которая сейчас загружается/перезагружается.
type aggregateLoadingModel struct {
	Name             string `json:"name"`
	BackendID        string `json:"backendId"`
	StartedAt        string `json:"startedAt,omitempty"` // RFC3339Nano, если задан
	ContextLength    int    `json:"contextLength,omitempty"`
	BatchSize        int    `json:"batchSize,omitempty"`
	NumGPULayers     int    `json:"numGpuLayers,omitempty"`
	Quantization     string `json:"quantization,omitempty"`
	LoadingError     string `json:"error,omitempty"`
	LoadingSizeBytes int64  `json:"loadingSizeBytes,omitempty"`
}

// clusterLoadedModelsResponse — агрегированный список загруженных моделей по всему кластеру.
type clusterLoadedModelsResponse struct {
	Count            int                    `json:"count"`
	Models           []aggregateLoadedModel `json:"models"`
	ModelsPerBackend map[string]int         `json:"modelsPerBackend,omitempty"`
}

// clusterLoadingModelsResponse — список моделей в процессе загрузки.
type clusterLoadingModelsResponse struct {
	Count  int                     `json:"count"`
	Models []aggregateLoadingModel `json:"models"`
}

// clusterLoadedModelsHandler — GET /api/v1/cluster/models/loaded.
//
// Возвращает список всех загруженных моделей по всем бэкендам. Для llama.cpp (cppworker)
// бэкендов в ответ добавляются параметры загрузки (n_ctx, batch_size, gpu_layers,
// quantization, vram/ram usage). Это позволяет UI и скриптам пользователя проверять
// состояние моделей через один запрос к балансировщику (порт 18081), а не опрашивать
// каждый cppworker (порт 18092) отдельно.
//
// Требует аутентификации (X-API-Token).
func (s *Server) clusterLoadedModelsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error": "method not allowed",
		})
		return
	}

	cs := s.getClusterState()
	if cs == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "cluster state unavailable",
		})
		return
	}

	resp := clusterLoadedModelsResponse{
		Count:            0,
		Models:           []aggregateLoadedModel{},
		ModelsPerBackend: map[string]int{},
	}

	// Bug #11 (2026-06-30): де-дупликация бэкендов по (host, port).
	// Один физический cppworker может быть зарегистрирован в кластере
	// несколько раз (shell-script + Go-side cppworker + agent) — без
	// де-дупа модели дублируются в ответе. Оставляем предпочитаемого
	// кандидата: (a) бэкенд с hasAgent=true, (b) иначе первый встретившийся.
	dedupedBackends := dedupBackendsByEndpoint(
		cs.Backends,
		func(b types.BackendMetrics) endpointKey {
			return endpointKey{Host: b.Host, Port: b.CppWorkerPort, NodeAddr: b.NodeAddr}
		},
		func(b types.BackendMetrics) bool { return b.HasAgent },
		true,
	)

	for _, b := range dedupedBackends {
		if b.BackendType != types.BackendTypeLlamaCpp {
			continue
		}
		for _, m := range b.LlamaCpp.LoadedModels {
			entry := aggregateLoadedModel{
				Name:          m.Name,
				BackendID:     b.ID,
				BackendType:   string(b.BackendType),
				Engine:        string(b.Engine),
				ContextLength: m.ContextLength,
				BatchSize:     m.BatchSize,
				NumGPULayers:  m.NumGPULayers,
				Quantization:  m.Quantization,
				VRAMUsage:     m.VRAMUsage,
				RAMUsage:      m.RAMUsage,
				Size:          m.Size,
				State:         m.State,
			}
			resp.Models = append(resp.Models, entry)
			resp.Count++
			resp.ModelsPerBackend[b.ID]++
		}
	}

	s.writeJSON(w, http.StatusOK, resp)
}

// clusterLoadingModelsHandler — GET /api/v1/cluster/models/loading.
//
// Возвращает список моделей, которые сейчас загружаются или перезагружаются
// (reload с новым n_ctx). Полезно для диагностики зависших загрузок — UI может
// показать, что модель уже несколько минут в статусе loading.
//
// Требует аутентификации.
func (s *Server) clusterLoadingModelsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error": "method not allowed",
		})
		return
	}

	cs := s.getClusterState()
	if cs == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "cluster state unavailable",
		})
		return
	}

	resp := clusterLoadingModelsResponse{
		Count:  0,
		Models: []aggregateLoadingModel{},
	}

	// Bug #11 (2026-06-30): де-дупликация бэкендов по (host, port) — иначе
	// модели, загружаемые на одном физическом cppworker, видны несколько раз
	// (по числу дублей регистрации в state).
	dedupedBackends := dedupBackendsByEndpoint(
		cs.Backends,
		func(b types.BackendMetrics) endpointKey {
			return endpointKey{Host: b.Host, Port: b.CppWorkerPort, NodeAddr: b.NodeAddr}
		},
		func(b types.BackendMetrics) bool { return b.HasAgent },
		true,
	)

	for _, b := range dedupedBackends {
		if b.BackendType != types.BackendTypeLlamaCpp {
			continue
		}
		for _, m := range b.LlamaCpp.LoadingModels {
			entry := aggregateLoadingModel{
				Name:             m.Name,
				BackendID:        b.ID,
				ContextLength:    m.ContextLength,
				BatchSize:        m.BatchSize,
				NumGPULayers:     m.NumGPULayers,
				Quantization:     m.Quantization,
				LoadingError:     m.LoadingError,
				LoadingSizeBytes: m.LoadingSizeBytes,
			}
			if m.LoadingStartedAt != nil {
				entry.StartedAt = *m.LoadingStartedAt
			}
			resp.Models = append(resp.Models, entry)
			resp.Count++
		}
	}

	s.writeJSON(w, http.StatusOK, resp)
}

// clusterReloadModelRequest — тело POST /api/v1/cluster/models/{name}/reload.
//
// Поддерживаемые операции (поле Operation):
//   - "load"   — загрузить модель (по умолчанию, если Operation пусто).
//   - "unload" — выгрузить модель.
//   - "reload" — выгрузить и затем загрузить с новыми параметрами
//     (синоним "all" для обратной совместимости с предыдущей версией API).
//
// Параметры загрузки:
//   - contextSize, gpuLayers — опциональные overrides (если не заданы,
//     берутся из профиля модели в config.bundled.json).
//   - insecure, stream        — проксируются в ModelOpRequest.
//
// Порядок полей — как требует govet fieldalignment (enable-all в .golangci.yml):
// указатели, затем строки, затем bool (иначе struct тратит лишние байты на
// выравнивание).
type clusterReloadModelRequest struct {
	ContextSize *int `json:"contextSize,omitempty"` // n_ctx override
	GPULayers   *int `json:"gpuLayers,omitempty"`   // gpu_layers override (-1=auto)
	// Force — R66c (2026-09-22): принудительная выгрузка занятой модели.
	// Прокидывается в cppworker как ?force=true (обрывает активные генерации).
	// Без него unload занятой модели возвращал 409 и выгрузить её из WebUI было
	// невозможно.
	Force *bool `json:"force,omitempty"`
	// R66d (2026-09-23): расширенные параметры загрузки — те же, что принимает
	// balancer.ModelOpRequest и cppworker /api/models/load. Раньше HTTP-слой их
	// не декодировал, поэтому reload «с новыми настройками» терял batch/flash/
	// mmap/kv-cache и применял только contextSize+gpuLayers.
	KVCacheType *string `json:"kvCacheType,omitempty"` // f16 | q8_0 | q4_0
	UseMmap     *bool   `json:"useMmap,omitempty"`
	FlashAttn   *int    `json:"flashAttn,omitempty"` // -1=auto, 0=off, 1=on
	BatchSize   *int    `json:"batchSize,omitempty"`

	Operation string `json:"operation,omitempty"` // "load" | "unload" | "reload" (default: "load")
	BackendID string `json:"backendId,omitempty"` // "" = все llama_cpp бэкенды
	Reason    string `json:"reason,omitempty"`    // комментарий для логов

	Insecure bool `json:"insecure,omitempty"`
	Stream   bool `json:"stream,omitempty"`
}

// toModelOpRequest — маппинг reload-запроса в ModelOpRequest.
// operation переопределяется вызывающей стороной (у reload после unload это
// "load"), поэтому передаётся параметром.
func (req clusterReloadModelRequest) toModelOpRequest(modelName, operation string) balancer.ModelOpRequest {
	return balancer.ModelOpRequest{
		Operation:   operation,
		ModelName:   modelName,
		ContextSize: req.ContextSize,
		GPULayers:   req.GPULayers,
		Insecure:    req.Insecure,
		Stream:      req.Stream,
		Force:       req.Force,
		KVCacheType: req.KVCacheType,
		UseMmap:     req.UseMmap,
		FlashAttn:   req.FlashAttn,
		BatchSize:   req.BatchSize,
	}
}

// clusterReloadModelResponse — результат reload-операции на каждом бэкенде.
type clusterReloadModelResponse struct {
	Model     string                            `json:"model"`
	Operation string                            `json:"operation"`
	Results   []clusterReloadModelBackendResult `json:"results"`
	// Skipped — R263: llama_cpp-узлы, которые НЕ тронуты, и причина по каждому
	// (offline/unhealthy/draining/starting/исключён оператором). Без этого списка
	// «операция прошла» читалось бы как «прошла на всех», хотя часть узлов
	// пропущена — оператор должен видеть, кого не тронули и почему.
	Skipped []reloadTargetRejection `json:"skipped,omitempty"`
}

// clusterReloadModelBackendResult — результат операции на одном бэкенде.
type clusterReloadModelBackendResult struct {
	BackendID  string `json:"backendId"`
	Status     string `json:"status"` // "ok" | "skipped" | "error" | "started"
	Message    string `json:"message,omitempty"`
	Error      string `json:"error,omitempty"`
	HTTPStatus int    `json:"httpStatus,omitempty"`

	// R66c: модель занята активными запросами; RetryWithForce=true — повтор
	// с {"force":true} её выгрузит (оборвав генерации). WebUI показывает по
	// этим полям подтверждение вместо тупикового "model is busy".
	Busy           bool `json:"busy,omitempty"`
	RetryWithForce bool `json:"retryWithForce,omitempty"`

	// R281 (2026-10-29): оператор просил ДРУГОЕ окно контекста, а модель уже
	// была загружена — значит это перезагрузка, а не «уже загружено».
	//
	// ЖИВОЙ ДЕФЕКТ (воспроизведён дважды): POST reload {contextSize: 8192} на
	// уже загруженной модели отвечал «ok: already loaded» и НЕ перезагружал её,
	// поэтому окно оставалось прежним. Снаружи это выглядело как «выставил окно
	// в WebUI, а загрузилось старое» — и оператор шёл выгружать модель руками.
	Reloaded        bool `json:"reloaded"`                  // была ли реальная перезагрузка
	PreviousContext int  `json:"previousContext,omitempty"` // какое окно было загружено
	NewContext      int  `json:"newContext,omitempty"`      // какое окно запрошено

	// R286 (2026-10-29): КАКИЕ параметры загрузки отличались от фактических.
	//
	// ЖИВОЙ ДЕФЕКТ. Оператор менял `kvCacheType` в WebUI, жал «перезагрузить» и
	// получал `already loaded`: проверка сравнивала только окно, поэтому смена
	// kv-cache/flash/mmap/batch была no-op — «балансер игнорирует настройки».
	// Теперь перезагрузка идёт по любому ЗНАЧИМОМУ отличию, а список отличий
	// уходит в ответ: иначе «перезагрузили» неотличимо от «ничего не делали».
	ChangedParams []loadParamDiff `json:"changedParams,omitempty"`

	// R287 (2026-10-29): операция ОТЛОЖЕНА (модель занята / идёт стрим), а не
	// провалена. Отдельные поля нужны потому, что живой прогон показал ответ,
	// в котором одновременно были 500 «busy» и `reloaded:true`: клиент не мог
	// понять, применилось ли что-то. Теперь «отложено» — это 503 + deferred +
	// reason и НИ ОДНОГО поля успеха в том же ответе.
	Deferred       bool   `json:"deferred,omitempty"`
	DeferredReason string `json:"deferredReason,omitempty"`
	RetryAfterSec  int    `json:"retryAfterSec,omitempty"`

	// R300 (2026-10-29): «ещё грузится» — это 202 Accepted, а НЕ ошибка.
	//
	// ЖИВОЙ ФАКТ (стенд b5b45c74, загрузка Qwen3.8-27B 16.4 ГБ). Балансер ждал
	// 6m26s, получил от себя же готовый формат «модель ещё загружается … это не
	// ошибка» (R262/R299, StillLoading=true), но HTTP-слой кластерного reload
	// смотрел только на Success и превращал его в `status:"error"` +
	// `httpStatus:500`. То есть текст ответа говорил «это не ошибка», а код —
	// «сломалось, повторяй»: ровно тот дефект, который R299 закрывал в балансере,
	// остался живым этажом выше (правка в одном месте не помогла — четвёртый
	// раз в серии). Здесь он закрывается в HTTP-слое.
	StillLoading bool `json:"stillLoading,omitempty"`
	PollAfterSec int  `json:"pollAfterSec,omitempty"`
}

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
	// ответа нет». Выбор состояния не проверял вовсе (см. reload_targets_r263.go).
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

// overallReloadStatus — R287: один код на весь ответ из per-backend исходов.
//
// Правило сознательно УЗКОЕ: 503 отдаём только когда ВСЕ узлы отложили операцию
// (именно этот случай оператор и видел: «200 + success:false», по которому нельзя
// понять, применилось ли что-то). Остальное сохраняет прежний контракт — 200 с
// per-backend `status`/`httpStatus`/`error`: на него опираются существующие тесты
// и WebUI, и «частичный успех» на нескольких узлах тоже должен читаться как 200.
func overallReloadStatus(results []clusterReloadModelBackendResult) int {
	if len(results) == 0 {
		return http.StatusNotFound
	}
	allDeferred := true
	anyAccepted := false
	for _, r := range results {
		if r.HTTPStatus == http.StatusOK || r.HTTPStatus == http.StatusCreated {
			return http.StatusOK
		}
		// R300 (2026-10-29): «ещё грузится» на ВСЕХ узлах — общий код 202.
		//
		// Живой факт: загрузка шла 6m26s, per-backend результат был «ещё
		// загружается», а общий код — 200 с status:"error" внутри. Клиент,
		// который смотрит на HTTP-код (Cline/скрипты/WebUI), читал это как
		// «всё ок» и «всё сломалось» одновременно. Если ни один узел не
		// применил операцию, но хотя бы один ещё работает — это 202.
		if r.HTTPStatus == http.StatusAccepted {
			anyAccepted = true
		}
		if !(r.Deferred || r.HTTPStatus == http.StatusServiceUnavailable) {
			allDeferred = false
		}
	}
	if anyAccepted {
		return http.StatusAccepted
	}
	if allDeferred {
		return http.StatusServiceUnavailable
	}
	return http.StatusOK
}

// splitClusterModelPath — извлекает имя модели из URL вида
// /api/v1/cluster/models/{name}/reload.
//
// Возвращает (name, true) если URL корректный.
func splitClusterModelPath(path string) (string, bool) {
	const prefix = "/api/v1/cluster/models/"
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(path, prefix)
	if rest == "" {
		return "", false
	}
	// rest = "{name}/reload" или "{name}/..." или просто "{name}"
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		return "", false
	}
	return parts[0], true
}

// selectReloadTargets — R263: перенесён в reload_targets_r263.go вместе с
// проверкой состояния узла. Здесь оставлен только комментарий-указатель, чтобы
// поиск по старому имени приводил к актуальной реализации, а не к копии правил.
//
// Было (до R263): возвращались ВСЕ llama_cpp-бэкенды (или ровно указанный), без
// проверки offline/unhealthy/disabled — отсюда «загрузка на мёртвый узел».

// executeReloadOnBackend — выполняет операцию load/unload/reload на одном бэкенде
// через ModelManager.ExecuteOperation.
//
// Семантика operation:
//   - "load"   → одна операция load. Если на бэкенде уже загружена ДРУГАЯ модель
//     (не запрошенная) — автоматически выгружает её перед load.
//   - "unload" → одна операция unload.
//   - "reload" → сначала unload (если модель загружена), затем load.
func (s *Server) executeReloadOnBackend(ctx context.Context, backendID, modelName string, req *clusterReloadModelRequest) clusterReloadModelBackendResult {
	result := clusterReloadModelBackendResult{BackendID: backendID}

	mm := s.proxy.GetModelManager()
	if mm == nil {
		result.Status = "error"
		result.Error = "model manager not available"
		result.HTTPStatus = http.StatusServiceUnavailable
		return result
	}

	// R297 (2026-10-29): СНАЧАЛА проверяем, что запрошенное окно ВООБЩЕ возможно,
	// и только потом трогаем загруженную модель.
	//
	// ЖИВОЙ ДЕФЕКТ: reload с `contextSize=200000` (физически невозможно) выгрузил
	// модель, затем упал с 500 — стенд остался без модели (`loaded_models:0`), на
	// восстановление ушло 72 секунды. Отказ в перезагрузке не имеет права
	// оставлять систему без работающей модели, поэтому проверка физики стоит
	// ДО unload, а не после.
	if req.ContextSize != nil && *req.ContextSize > 0 && s.proxy != nil {
		if ok, ceiling, why := s.proxy.WindowFeasibility(backendID, modelName, *req.ContextSize); !ok {
			logger.Get().Warnw("cluster reload rejected before unload: requested window is infeasible (R297)",
				"backend", backendID, "model", modelName,
				"requested_context", *req.ContextSize, "ceiling", ceiling, "reason", why)
			result.Status = "error"
			result.Error = fmt.Sprintf(
				"contextSize=%d exceeds model/backend ceiling=%d (%s); the model was left loaded",
				*req.ContextSize, ceiling, why)
			result.HTTPStatus = http.StatusBadRequest
			return result
		}
	}

	// R300 (2026-10-29): модель ЕЩЁ загружается — «already loaded» было бы ложью.
	//
	// ЖИВОЙ ФАКТ (стенд b5b45c74): пока Qwen3.8-27B грузился (16.4 ГБ, ~6 мин),
	// повторный `{"operation":"load"}` отвечал за 0.22 с `200 {"status":"ok",
	// "message":"already loaded","reloaded":false}` — при том что в тот же момент
	// `GET /api/v1/cluster/models/loaded` показывал у этой модели `"state":
	// "loading"`, а первая загрузка продолжалась. Причина: isModelLoaded()
	// смотрит только на наличие записи в LoadedModels, а State не различает.
	// Оператор по такому ответу считает, что модель готова, и идёт работать с
	// ней — получает ошибку от воркера.
	//
	// Правило: состояние «loading» — уже известный факт, значит ответ обязан быть
	// 202 + stillLoading + совет опросить статус (тот же контракт R262/R299), а не
	// «ok» и не «error».
	if st := s.modelLoadStateOnBackend(backendID, modelName); st == modelStateLoadingR300 {
		logger.Get().Infow("cluster reload: модель ещё загружается — 202 вместо «already loaded» (R300)",
			"backend", backendID, "model", modelName, "operation", req.Operation)
		return stillLoadingReloadResultR300(backendID, modelName, fmt.Sprintf(
			"модель «%s» ещё загружается на бэкенде «%s»: операция %q не выполнена, "+
				"это не ошибка — опрос через %ds покажет итог",
			modelName, backendID, req.Operation, reloadPollAfterSecR300))
	}

	// R297: параметры ТЕКУЩЕЙ загрузки — чтобы вернуть модель, если новая
	// конфигурация не поднимется. Снимаем ДО unload: после него факта уже нет.
	var prevParams loadedModelParams
	prevParamsKnown := false
	if s.isModelLoaded(backendID, modelName) {
		prevParams, prevParamsKnown = s.loadedModelParamsOnBackend(backendID, modelName)
	}

	// Для reload сначала unload, если модель загружена.
	needUnload := req.Operation == "reload"
	if req.Operation == "reload" {
		loaded := s.isModelLoaded(backendID, modelName)
		needUnload = loaded
	}
	if req.Operation == "load" && s.isModelLoaded(backendID, modelName) {
		// R281: модель уже загружена — но с КАКИМИ параметрами? Если оператор
		// просит другое окно, «already loaded» было бы ложью: он получил бы
		// ответ «ok», а окно осталось прежним (живой дефект, воспроизведён
		// дважды).
		//
		// R286: сравниваем не только окно, но и остальные ЗНАЧИМЫЕ параметры
		// загрузки (kv-cache, flash-attn, mmap, batch, gpu-layers). Живой
		// дефект: смена kvCacheType в WebUI давала no-op с ответом
		// «already loaded» — снаружи это «балансер игнорирует настройки».
		loadedParams, _ := s.loadedModelParamsOnBackend(backendID, modelName)
		diffs := loadParamsDiff(loadedParams, *req)
		if len(diffs) > 0 {
			result.Reloaded = true
			result.ChangedParams = diffs
			if loadedParams.ContextKnown {
				result.PreviousContext = loadedParams.ContextSize
			}
			if req.ContextSize != nil && *req.ContextSize > 0 {
				result.NewContext = *req.ContextSize
			}
			needUnload = true
			logger.Get().Infow("cluster reload: загруженные параметры отличаются — перезагружаем модель (R286)",
				"backend", backendID, "model", modelName,
				"loaded_context", loadedParamsContextLabel(loadedParams),
				"requested_context", ctxSizeForLog(req.ContextSize),
				"changed_params", loadParamDiffNames(diffs),
				"diffs", diffs)
		} else {
			result.Status = "ok"
			result.Message = "already loaded"
			result.HTTPStatus = http.StatusOK
			return result
		}
	}

	if needUnload {
		// R271/R280: не выгружаем модель под ЖИВЫМ стримом. Операторский reload
		// — такая же разрушительная операция: cppworker закрывает поток клиента
		// без терминального маркера, и тот получает «stream ended by upstream»,
		// хотя upstream был жив. Ждём до LB_RELOAD_DEFER_MAX_SEC; не дождались —
		// честно говорим, сколько стримов мешает, вместо тихой перезагрузки.
		if s.proxy != nil && !s.proxy.WaitStreamsIdleBeforeDestructive(ctx, backendID, modelName) {
			n := s.proxy.ActiveStreamsOnBackend(backendID)
			// R287: «отложено» — это НЕ ошибка и НЕ успех. Отдельные поля и 503,
			// без reloaded/status=ok: иначе клиент получает противоречивый ответ
			// (в живом прогоне было 500 «busy» вместе с reloaded:true).
			return deferredReloadResult(backendID, fmt.Sprintf(
				"model reload deferred: %d active stream(s) on backend; retry shortly", n), "active streams")
		}
		if s.proxy != nil {
			s.proxy.NoteDestructiveReload(backendID, modelName)
		}
		unloadOp := balancer.ModelOpRequest{
			Operation: "unload",
			ModelName: modelName,
		}
		unloadResult := mm.ExecuteOperation(backendID, unloadOp)
		if !unloadResult.Success {
			// R287: «модель занята» — это deferred, а не 500. Живой прогон: во
			// время AutoTune-reload ручной reload вернул 500 «busy» И
			// reloaded:true одновременно — оператор не мог понять, применилось ли.
			if reason := reloadBusyReason(unloadResult.Error); reason != "" || unloadResult.Busy {
				if reason == "" {
					reason = "model busy"
				}
				logger.Get().Infow("cluster reload deferred: model busy (R287)",
					"backend", backendID, "model", modelName, "reason", reason)
				return deferredReloadResult(backendID,
					"model reload deferred: "+reason+"; retry shortly", reason)
			}
			result.Status = "error"
			result.Error = "unload failed: " + unloadResult.Error
			result.HTTPStatus = http.StatusInternalServerError
			// Успеха не было — и полей успеха в ответе быть не должно.
			result.Reloaded = false
			result.ChangedParams = nil
			return result
		}
		req.Operation = "load"
	}
	// R296: unload выше прошёл успешно — модель РЕАЛЬНО снята, только теперь
	// обрыв чужого стрима можно объяснять нашей перезагрузкой (а не самой
	// пометкой «перезагрузка началась»).
	if s.proxy != nil {
		s.proxy.ConfirmDestructiveUnloaded(backendID, modelName)
	}

	// ============================================================
	// R119 (2026-10-15): auto-unload чужой модели — ТОЛЬКО по факту нехватки памяти
	// ============================================================
	//
	// БЫЛО (Bug #8): перед ЛЮБОЙ загрузкой безусловно выгружалась чужая модель с
	// того же бэкенда. Мотив — страх OOM («cppworker пытается загрузить B, но
	// model A уже заняла VRAM»), но правило было грубее проблемы: пока на бэкенде
	// лежит модель, балансер физически не мог поднять вторую, даже если обе
	// влезают. Живая жалоба оператора: «вторая модель не грузится, первая пропала».
	//
	// ТЕПЕРЬ: сначала пробуем загрузить как есть (cppworker сам считает memfit и
	// при нехватке отвечает понятной ошибкой), и только если попытка упала по
	// памяти — выгружаем чужую модель и повторяем один раз. Если чужую модель
	// нельзя выгрузить (занята активными запросами), честно говорим об этом.
	// Правило живёт в loadWithEvictionOnMemoryFailure (cluster_load_eviction_r119.go)
	// и покрыто юнит-тестами с фейковым раннером.
	var loadedOther string
	otherLoaded := false
	if req.Operation == "load" {
		// R281: ветка «уже загружено» переехала ВЫШЕ и там сравнивает окна.
		// Второй такой же проверки здесь быть не должно: два мнения об одном
		// факте — ровно тот класс дефекта, из-за которого «правка в одном месте
		// не помогает» (в серии ловили четыре раза).
		loadedOther, otherLoaded = s.getLoadedModelOnBackend(backendID)
		if loadedOther == modelName {
			otherLoaded = false
		}
	}

	opReq := req.toModelOpRequest(modelName, req.Operation)

	var opResult *balancer.ModelOpResult
	var evictionNote string
	if otherLoaded {
		opResult, evictionNote = loadWithEvictionOnMemoryFailure(mm, backendID, modelName, loadedOther, opReq)
	} else {
		opResult = mm.ExecuteOperation(backendID, opReq)
	}

	result.HTTPStatus = http.StatusOK
	if !opResult.Success {
		// R287: «занято» — отложено, а не провал. Один исход на ответ: либо
		// deferred (503 + reason + retryAfter), либо error (5xx) — но никогда
		// «ошибка вместе с reloaded:true» (живой дефект r284).
		if reason := reloadBusyReason(opResult.Error); reason != "" || opResult.Busy {
			if reason == "" {
				reason = "model busy"
			}
			logger.Get().Infow("cluster reload deferred: model busy (R287)",
				"backend", backendID, "model", modelName, "reason", reason)
			return deferredReloadResult(backendID,
				"model reload deferred: "+reason+"; retry shortly", reason)
		}
		// R300 (2026-10-29): «ещё грузится» — 202, а НЕ error/500.
		//
		// Формат R262/R299 (StillLoading=true, Error пуст, Message «это не
		// ошибка») уже приходил из балансера, но эта ветка смотрела только на
		// Success и писала status:"error" + httpStatus:500. Живой прогон видел
		// ровно это: 6m26s ожидания, текст «это не ошибка» и код 500 рядом.
		// Ветка обязана стоять ДО restore из R297: новая загрузка ещё идёт,
		// возвращать на её место прежнюю модель нельзя.
		if opResult.StillLoading {
			return stillLoadingReloadResultR300(backendID, modelName, opResult.Message)
		}
		result.HTTPStatus = http.StatusInternalServerError
		result.Status = "error"
		result.Error = opResult.Error
		result.Reloaded = false
		result.ChangedParams = nil
		if opResult.Message != "" {
			result.Message = opResult.Message
		}
		// R66c: сообщаем клиенту, что модель занята и её можно выгрузить
		// повтором с force=true. WebUI по этому полю показывает подтверждение.
		result.Busy = opResult.Busy
		result.RetryWithForce = opResult.RetryWithForce
		if evictionNote != "" {
			result.Message = strings.TrimSpace(result.Message + " (" + evictionNote + ")")
		}
		// R297: не оставляем стенд без модели. Если новая конфигурация не
		// поднялась, а прежняя была снята нами — возвращаем прежнюю и говорим об
		// этом прямо в ответе. Иначе оператор видит «ошибка» и пустой бэкенд.
		if needUnload && prevParamsKnown {
			if restored := s.restorePreviousModelOnBackend(backendID, modelName, prevParams); restored {
				result.Message = strings.TrimSpace(result.Message + " (previous model restored)")
				// Не начинаем текст с «; » — склейка должна читаться и когда
				// воркер не дал своего текста ошибки.
				if result.Error != "" {
					result.Error += "; previous configuration restored"
				} else {
					result.Error = "previous configuration restored"
				}
			} else if s.isModelLoaded(backendID, modelName) {
				// Модель всё-таки на месте (воркер успел поднять) — не пугаем.
				result.Message = strings.TrimSpace(result.Message + " (model is loaded)")
			} else {
				result.Message = strings.TrimSpace(result.Message + " (previous model could NOT be restored)")
			}
		}
		return result
	}

	result.Status = "ok"
	result.Message = opResult.Message
	if evictionNote != "" {
		result.Message = strings.TrimSpace(result.Message + " (" + evictionNote + ")")
	}
	return result
}

// reloadPollAfterSecR300 — через сколько секунд советуем опросить статус.
//
// То же число, что в R262 (30 с): к этому моменту воркер либо догрузил модель,
// либо ещё нет — второго опроса клиенту хватает, чтобы не превращать ожидание в
// поток запросов.
const reloadPollAfterSecR300 = 30

// modelStateLoadingR300 — состояние модели в cppworker, означающее «ещё грузится».
const modelStateLoadingR300 = "loading"

// modelLoadStateOnBackend — R300: РЕАЛЬНОЕ состояние модели на бэкенде
// ("loaded"/"loading"/"error"/""), а не только факт её наличия в списке.
//
// Зачем отдельно от isModelLoaded: isModelLoaded отвечает на вопрос «есть ли
// запись», и этим отличал загруженную модель от загружающейся. Живой дефект:
// во время загрузки Qwen3.8-27B повторный load отвечал «already loaded».
//
// Смотрим оба списка cppworker — LoadedModels и LoadingModels: поллер кладёт
// грузящуюся модель в LoadingModels, а API /cluster/models/loaded может показать
// её же с state="loading" (это разные представления одного факта).
func (s *Server) modelLoadStateOnBackend(backendID, modelName string) string {
	return modelLoadStateR300(s.getClusterState(), backendID, modelName)
}

// modelLoadStateR300 — чистая функция: состояние модели в cluster state.
//
// Вынесено из метода, чтобы правило проверялось без поднятия сервера.
// Приоритет: "loaded" > "loading" > "error": если запись есть в обоих списках
// (поллер мог не успеть перенести), модель считается загруженной только когда
// состояние это подтверждает.
func modelLoadStateR300(cs *types.ClusterState, backendID, modelName string) string {
	if cs == nil {
		return ""
	}
	best := ""
	for _, b := range cs.Backends {
		if b.ID != backendID {
			continue
		}
		for _, m := range append(append([]types.LlamaCppModel{}, b.LlamaCpp.LoadedModels...), b.LlamaCpp.LoadingModels...) {
			if !modelNameEqualR300(m.Name, modelName) {
				continue
			}
			st := strings.ToLower(strings.TrimSpace(m.State))
			switch st {
			case "loaded":
				return "loaded"
			case "loading":
				best = "loading"
			case "error":
				if best == "" {
					best = "error"
				}
			}
		}
	}
	return best
}

// modelNameEqualR300 — сравнение имён моделей без учёта регистра и ":tag".
func modelNameEqualR300(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	if strings.EqualFold(a, b) {
		return true
	}
	base := func(s string) string {
		if i := strings.Index(s, ":"); i > 0 {
			return s[:i]
		}
		return s
	}
	return strings.EqualFold(base(a), base(b))
}

// stillLoadingReloadResultR300 — единый вид «операция ещё идёт» для HTTP-слоя.
//
// Status="started" (значение уже описано контрактом этого поля), HTTP 202,
// stillLoading:true, pollAfterSec — и НИ ОДНОГО поля ошибки: клиент по 202+
// stillLoading показывает «загружается», а не «ошибка загрузки».
func stillLoadingReloadResultR300(backendID, modelName, message string) clusterReloadModelBackendResult {
	if strings.TrimSpace(message) == "" {
		message = fmt.Sprintf(
			"модель «%s» ещё загружается на бэкенде «%s»: это не ошибка — опрос через %ds покажет итог",
			modelName, backendID, reloadPollAfterSecR300)
	}
	return clusterReloadModelBackendResult{
		BackendID:    backendID,
		Status:       "started",
		Message:      message,
		HTTPStatus:   http.StatusAccepted,
		StillLoading: true,
		PollAfterSec: reloadPollAfterSecR300,
	}
}

// deferredReloadResult — R287: единый вид «операция отложена».
//
// Отложенность — это НЕ провал и НЕ успех: ничего не применено, но и ломать
// нечего. Поэтому 503 + deferred + reason + retryAfter и ни одного поля успеха —
// иначе клиент получает противоречивый ответ (живой дефект: 500 «busy» вместе с
// reloaded:true в одном теле).
func deferredReloadResult(backendID, message, reason string) clusterReloadModelBackendResult {
	return clusterReloadModelBackendResult{
		BackendID:      backendID,
		Status:         "deferred",
		Message:        message,
		HTTPStatus:     http.StatusServiceUnavailable,
		Deferred:       true,
		DeferredReason: reason,
		RetryAfterSec:  30,
	}
}

// loadedParamsContextLabel — как показать загруженное окно в логе («unknown»,
// если cppworker его не сообщил).
func loadedParamsContextLabel(p loadedModelParams) string {
	if !p.ContextKnown {
		return "unknown"
	}
	return fmt.Sprintf("%d", p.ContextSize)
}

// isModelLoaded — проверяет, загружена ли модель на бэкенде (через cluster state).
func (s *Server) isModelLoaded(backendID, modelName string) bool {
	cs := s.getClusterState()
	if cs == nil {
		return false
	}
	for _, b := range cs.Backends {
		if b.ID != backendID {
			continue
		}
		for _, m := range b.LlamaCpp.LoadedModels {
			if m.Name == modelName {
				return true
			}
		}
	}
	return false
}

// needContextReload — R281: нужно ли перезагружать уже загруженную модель,
// потому что оператор просит ДРУГОЕ окно контекста.
//
// Вынесено в чистую функцию, чтобы правило было одно и проверялось таблицей:
//
//	requested nil/<=0            → false (оператор окно не задавал: прежнее поведение)
//	requested совпадает с фактом  → false («already loaded» — правда)
//	requested отличается          → true  (перезагружаем и говорим об этом)
//	факт неизвестен (ctx=0)       → true, если requested>0: старые сборки
//	                                 cppworker не сообщают contextLength, и
//	                                 «уже загружено» было бы догадкой
func needContextReload(loadedCtx int, ctxKnown bool, requested *int) bool {
	if requested == nil || *requested <= 0 {
		return false
	}
	if !ctxKnown {
		return true
	}
	return loadedCtx != *requested
}

// loadedModelContextOnBackend — R281: какое окно контекста РЕАЛЬНО загружено
// для модели на этом бэкенде.
//
// Зачем отдельная функция, а не «isModelLoaded + удача»: чтобы ответить
// оператору на вопрос «почему загрузилось старое окно», нужно сравнить
// запрошенное окно с ФАКТИЧЕСКИМ. Второе возвращаемое значение — известен ли
// факт: у старых сборок cppworker contextLength в /api/models может быть 0, и
// тогда мы не имеем права утверждать «другое окно» (иначе каждый reload с
// contextSize превращался бы в перезагрузку).
func (s *Server) loadedModelContextOnBackend(backendID, modelName string) (int, bool) {
	cs := s.getClusterState()
	if cs == nil {
		return 0, false
	}
	for _, b := range cs.Backends {
		if b.ID != backendID {
			continue
		}
		for _, m := range b.LlamaCpp.LoadedModels {
			if m.Name == modelName {
				if m.ContextLength > 0 {
					return m.ContextLength, true
				}
				return 0, false
			}
		}
	}
	return 0, false
}

// getLoadedModelOnBackend — возвращает имя загруженной модели на бэкенде (если есть).
// Используется Bug #8: auto-unload чужой модели при load новой.
//
// Возвращает (modelName, true) если на бэкенде есть загруженная модель.
// Если моделей нет — возвращает ("", false).
// Если на бэкенде загружено несколько моделей — возвращает первую (такое
// практически невозможно для llama.cpp, но defensive programming).
func (s *Server) getLoadedModelOnBackend(backendID string) (string, bool) {
	cs := s.getClusterState()
	if cs == nil {
		return "", false
	}
	for _, b := range cs.Backends {
		if b.ID != backendID {
			continue
		}
		for _, m := range b.LlamaCpp.LoadedModels {
			if m.Name != "" {
				return m.Name, true
			}
		}
	}
	return "", false
}

// getClusterState — единая точка получения cluster state с проверкой proxy.
func (s *Server) getClusterState() *types.ClusterState {
	if s.proxy == nil {
		return nil
	}
	return s.proxy.GetClusterState()
}

// ctxSizeForLog — возвращает значение *int для structured-логов (nil-safe).
func ctxSizeForLog(p *int) interface{} {
	if p == nil {
		return nil
	}
	return *p
}

// gpuLayersForLog — возвращает значение *int для structured-логов (nil-safe).
func gpuLayersForLog(p *int) interface{} {
	if p == nil {
		return nil
	}
	return *p
}

// ===== Bulk operations (Session A — Q3 W4) =====

// bulkOperationMaxModels — максимальное число моделей в одном bulk-запросе.
// Лимит защищает cppworker от одновременной обработки слишком большого пакета:
// на больших кластерах (50+ моделей) единичный запрос может занять десятки секунд,
// а UI должен иметь возможность отменить/обновить состояние.
const bulkOperationMaxModels = 100

// bulkOperationConcurrency — максимальное число параллельных операций внутри
// одного backend'а (на одну модель). Используется только при >50 моделях.
// На практике cppworker сам сериализует load/unload через tryAcquireOp, поэтому
// распараллеливание по бэкендам безопасно.
const bulkOperationConcurrency = 4

// bulkModelItem — одна модель в bulk-запросе.
type bulkModelItem struct {
	Model       string `json:"model"`                 // имя модели (обязательно)
	Operation   string `json:"operation,omitempty"`   // "load" | "unload" | "reload" (default: req.Operation)
	BackendID   string `json:"backendId,omitempty"`   // override глобального BackendID
	ContextSize *int   `json:"contextSize,omitempty"` // override глобального ContextSize
	GPULayers   *int   `json:"gpuLayers,omitempty"`   // override глобального GPUlayers
	Reason      string `json:"reason,omitempty"`      // комментарий для логов
	// Force — R66c: per-item override для выгрузки занятой модели (см.
	// clusterBulkModelsRequest.Force).
	Force *bool `json:"force,omitempty"`
}

// forceForItem — R66c: разрешает per-item override force поверх глобального.
func forceForItem(item *bulkModelItem, req *clusterBulkModelsRequest) *bool {
	if item != nil && item.Force != nil {
		return item.Force
	}
	if req != nil {
		return req.Force
	}
	return nil
}

// clusterBulkModelsRequest — тело POST /api/v1/cluster/models/bulk.
//
// Поддерживает два режима:
//  1. **Явный список моделей** (Models != nil && len > 0) — для multi-select UI
//     (Load/Unload/Delete Selected). Каждый элемент может переопределить
//     BackendID/ContextSize/GPULayers глобально или оставить их пустыми.
//  2. **Операция без списка** (Models == nil или пустой массив) — для командных
//     сценариев типа "Load All Available" / "Unload All Loaded". В этом случае:
//     - "unload" без списка → выгружаются все загруженные модели из cluster state.
//     - "load" без списка → 400 (опасно: нет списка моделей для загрузки).
//
// Приоритет параметров (от высшего к низшему):
//  1. Per-model override в BulkOperation.
//  2. Глобальный BackendID/ContextSize/GPULayers/Operation в запросе.
//  3. Значения по умолчанию ("load", n_ctx из профиля, gpu_layers=-1=auto).
type clusterBulkModelsRequest struct {
	Operation   string          `json:"operation,omitempty"`   // "load" | "unload" | "reload" (default: "load")
	Models      []bulkModelItem `json:"models"`                // список моделей (опционально для unload)
	BackendID   string          `json:"backendId,omitempty"`   // глобальный backendId override
	ContextSize *int            `json:"contextSize,omitempty"` // глобальный n_ctx override
	GPULayers   *int            `json:"gpuLayers,omitempty"`   // глобальный gpu_layers override (-1=auto)
	Insecure    bool            `json:"insecure,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
	Reason      string          `json:"reason,omitempty"`
	// Force — R66c (2026-09-22): принудительная выгрузка занятых моделей
	// (cppworker ?force=true). Применяется ко всем моделям списка, если у
	// элемента нет собственного Force.
	Force *bool `json:"force,omitempty"`
}

// clusterBulkModelsResponse — агрегированный результат bulk-операции по всем моделям.
//
// HTTP-статус всегда 200, кроме критичных ошибок (cluster state unavailable,
// невалидный JSON, превышение лимита моделей). Per-model и per-backend ошибки
// не ломают общий ответ — это согласуется с семантикой cluster endpoints из Session 17.
type clusterBulkModelsResponse struct {
	Operation  string                   `json:"operation"`  // фактически применённая операция
	Total      int                      `json:"total"`      // всего моделей в запросе
	Succeeded  int                      `json:"succeeded"`  // успешно выполнено (все бэкенды status=ok)
	Failed     int                      `json:"failed"`     // хотя бы один бэкенд ответил error
	Results    []clusterBulkModelResult `json:"results"`    // детали по каждой модели
	StartedAt  string                   `json:"startedAt"`  // RFC3339Nano начала выполнения
	DurationMs int64                    `json:"durationMs"` // общее время выполнения
}

// clusterBulkModelResult — результат одной модели из bulk-запроса.
type clusterBulkModelResult struct {
	Model     string                            `json:"model"`
	Operation string                            `json:"operation"` // применённая операция
	Succeeded bool                              `json:"succeeded"` // все бэкенды status=ok
	Results   []clusterReloadModelBackendResult `json:"results"`   // детали per-backend (переиспользуем Session 17)
}

// clusterBulkModelsHandler — POST /api/v1/cluster/models/bulk.
//
// Массовая операция над списком моделей через балансировщик. Переиспользует
// executeReloadOnBackend (Session 17) для per-backend выполнения, добавляя только
// агрегацию результатов и контроль concurrency при больших списках.
//
// Семантика response (как у других cluster endpoints):
//   - HTTP 200 + per-model results[] даже если все операции упали.
//   - HTTP 4xx только для критичных ошибок (validation).
//   - HTTP 5xx только при недоступности cluster state / model manager.
//
// Требует аутентификации (X-API-Token).
func (s *Server) clusterBulkModelsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error": "method not allowed",
		})
		return
	}

	var req clusterBulkModelsRequest
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
	case "load", "unload", "reload", "all":
		// OK
	default:
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid operation",
			"message": "supported operations: load, unload, reload",
		})
		return
	}

	// Семантический алиас: "all" → "reload".
	if req.Operation == "all" {
		req.Operation = "reload"
	}

	// Проверяем доступность cluster state.
	if s.getClusterState() == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "cluster state unavailable",
		})
		return
	}

	// Резолвим список моделей: явный список или derived (для unload без списка).
	models := req.Models
	if len(models) == 0 {
		// "unload" без списка → берём все загруженные модели из cluster state.
		// Это позволяет UI реализовать "Unload All" одной кнопкой.
		if req.Operation == "unload" {
			models = s.collectAllLoadedModels(req.BackendID)
		} else {
			// "load" / "reload" без списка — опасно, нужен явный список моделей.
			s.writeJSON(w, http.StatusBadRequest, map[string]string{
				"error":   "models list required for operation",
				"message": "provide a non-empty 'models' array or use operation='unload' to unload all loaded",
			})
			return
		}
	}

	// Проверка лимита на количество моделей (защита от перегрузки cppworker).
	if len(models) > bulkOperationMaxModels {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "too many models in bulk request",
			"message": "max " + intToString(bulkOperationMaxModels) + " models per request",
		})
		return
	}

	// Проверяем что есть ПРИГОДНЫЕ llama_cpp бэкенды для выполнения операции
	// (R263: offline/unhealthy/disabled узлы в цели не берём — иначе операция
	// уходит на мёртвый узел и «висит» до таймаута).
	bulkSel := s.selectReloadTargetsChecked(req.BackendID)
	if len(bulkSel.Targets) == 0 {
		if bulkSel.LlamaCppTotal == 0 {
			s.writeJSON(w, http.StatusNotFound, map[string]string{
				"error":   "no llama_cpp backends available",
				"message": "bulk operation requires at least one llama_cpp backend",
			})
			return
		}
		logger.Get().Warnw("cluster bulk operation refused: no usable backends",
			"operation", req.Operation, "skipped", len(bulkSel.Skipped))
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
			"error":   "no usable llama_cpp backend",
			"message": "every llama_cpp backend is offline, unhealthy or excluded by the operator",
			"skipped": bulkSel.Skipped,
		})
		return
	}
	targets := bulkSel.Targets

	startedAt := time.Now().UTC()
	startedAtRFC := startedAt.Format(time.RFC3339Nano)

	logger.Get().Infow("cluster bulk operation requested",
		"operation", req.Operation,
		"models_count", len(models),
		"backend_id", req.BackendID,
		"context_size", ctxSizeForLog(req.ContextSize),
		"gpu_layers", gpuLayersForLog(req.GPULayers),
		"reason", req.Reason,
	)

	resp := clusterBulkModelsResponse{
		Operation: req.Operation,
		Total:     len(models),
		Results:   make([]clusterBulkModelResult, 0, len(models)),
	}

	// Выполняем операции. Для малых списков (<=50) — последовательно,
	// для больших (>50) — параллельно с ограничением concurrency.
	if len(models) <= bulkOperationConcurrency*2 {
		for i := range models {
			result := s.executeBulkModelItem(r.Context(), targets, &models[i], &req)
			resp.Results = append(resp.Results, result)
			if result.Succeeded {
				resp.Succeeded++
			} else {
				resp.Failed++
			}
		}
	} else {
		resp.Results = s.executeBulkModelItemsParallel(r.Context(), targets, models, &req, &resp)
	}

	resp.DurationMs = time.Since(startedAt).Milliseconds()
	resp.StartedAt = startedAtRFC

	logger.Get().Infow("cluster bulk operation completed",
		"operation", req.Operation,
		"total", resp.Total,
		"succeeded", resp.Succeeded,
		"failed", resp.Failed,
		"duration_ms", resp.DurationMs,
	)

	s.writeJSON(w, http.StatusOK, resp)
}

// executeBulkModelItem — выполняет одну модель из bulk-запроса на всех целевых бэкендах.
// Переиспользует executeReloadOnBackend (Session 17), собирая результаты в clusterBulkModelResult.
func (s *Server) executeBulkModelItem(ctx context.Context, targets []string, item *bulkModelItem, req *clusterBulkModelsRequest) clusterBulkModelResult {
	// Резолвим per-item операцию: приоритет item.Operation > req.Operation.
	op := req.Operation
	if item.Operation != "" {
		op = item.Operation
	}
	// Нормализация "all" → "reload".
	if op == "all" {
		op = "reload"
	}

	// Резолвим per-item backendId.
	backendID := req.BackendID
	if item.BackendID != "" {
		backendID = item.BackendID
	}

	// Резолвим per-item contextSize / gpuLayers.
	contextSize := req.ContextSize
	if item.ContextSize != nil {
		contextSize = item.ContextSize
	}
	gpuLayers := req.GPULayers
	if item.GPULayers != nil {
		gpuLayers = item.GPULayers
	}

	// Резолвим per-item reason.
	reason := req.Reason
	if item.Reason != "" {
		reason = item.Reason
	}

	// Строим clusterReloadModelRequest для executeReloadOnBackend.
	perBackendReq := &clusterReloadModelRequest{
		Operation:   op,
		BackendID:   backendID,
		ContextSize: contextSize,
		GPULayers:   gpuLayers,
		Insecure:    req.Insecure,
		Stream:      req.Stream,
		Reason:      reason,
		// R66c: force выгружает занятую модель (cppworker ?force=true).
		// Per-item override приоритетнее глобального, как и остальные поля.
		Force: forceForItem(item, req),
	}

	// Резолвим фактические targets для этой модели (R263: только пригодные узлы).
	perSel := s.selectReloadTargetsChecked(backendID)
	modelTargets := perSel.Targets
	if len(modelTargets) == 0 {
		// Нет подходящих бэкендов для этой модели → результат с ошибкой и ПРИЧИНОЙ
		// (R263: раньше писалась «no llama_cpp backends», хотя узел есть — просто
		// offline/unhealthy/исключён; оператор шёл искать несуществующий бэкенд).
		reason := perSel.ExplicitReason
		if reason == "" {
			reason = "no usable llama_cpp backend"
		}
		return clusterBulkModelResult{
			Model:     item.Model,
			Operation: op,
			Succeeded: false,
			Results: []clusterReloadModelBackendResult{{
				BackendID:  backendID,
				Status:     "error",
				Error:      "backend is not usable for model load (" + reason + ")",
				HTTPStatus: http.StatusServiceUnavailable,
			}},
		}
	}

	results := make([]clusterReloadModelBackendResult, 0, len(modelTargets))
	allOK := true
	for _, bid := range modelTargets {
		res := s.executeReloadOnBackend(ctx, bid, item.Model, perBackendReq)
		if res.Status != "ok" {
			allOK = false
		}
		results = append(results, res)
	}

	return clusterBulkModelResult{
		Model:     item.Model,
		Operation: op,
		Succeeded: allOK,
		Results:   results,
	}
}

// executeBulkModelItemsParallel — параллельное выполнение bulk-операций с лимитом concurrency.
// Используется только для больших списков (>10 моделей), чтобы не блокировать UI.
func (s *Server) executeBulkModelItemsParallel(ctx context.Context, targets []string, models []bulkModelItem, req *clusterBulkModelsRequest, resp *clusterBulkModelsResponse) []clusterBulkModelResult {
	results := make([]clusterBulkModelResult, len(models))
	var wg sync.WaitGroup
	var mu sync.Mutex
	sem := make(chan struct{}, bulkOperationConcurrency)

	for i := range models {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			result := s.executeBulkModelItem(ctx, targets, &models[idx], req)
			results[idx] = result
			mu.Lock()
			if result.Succeeded {
				resp.Succeeded++
			} else {
				resp.Failed++
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	return results
}

// collectAllLoadedModels — собирает список загруженных моделей из cluster state
// для операции "unload без явного списка моделей".
//
// Возвращает []bulkModelItem с уникальными именами моделей (если модель загружена
// на нескольких бэкендах, она попадёт в список один раз — executeReloadOnBackend
// сам обработает все бэкенды для каждого имени).
//
// Если backendID задан, фильтрует только модели с этого бэкенда.
func (s *Server) collectAllLoadedModels(backendID string) []bulkModelItem {
	cs := s.getClusterState()
	if cs == nil {
		return nil
	}

	seen := make(map[string]bool)
	result := make([]bulkModelItem, 0)

	for _, b := range cs.Backends {
		if b.BackendType != types.BackendTypeLlamaCpp {
			continue
		}
		if backendID != "" && b.ID != backendID {
			continue
		}
		for _, m := range b.LlamaCpp.LoadedModels {
			if seen[m.Name] {
				continue
			}
			seen[m.Name] = true
			result = append(result, bulkModelItem{
				Model:     m.Name,
				Operation: "unload",
			})
		}
	}

	return result
}

// intToString — простой helper для сообщений об ошибках (избегаем strconv импорта в этом файле).
func intToString(n int) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// restorePreviousModelOnBackend — R297 (2026-10-29): вернуть модель в прежней
// конфигурации, если новая не поднялась.
//
// ЗАЧЕМ. Живой дефект: reload с невозможным окном выгружал модель и падал, а
// стенд оставался без модели вообще (`loaded_models:0`) — «ошибка настройки»
// превращалась в «сервиса нет», и на восстановление уходила минута с лишним.
// Отказ обязан быть обратимым: сняли — вернули то, что было.
//
// Возвращает true, только если модель реально снова загружена.
func (s *Server) restorePreviousModelOnBackend(backendID, modelName string, prev loadedModelParams) bool {
	mm := s.proxy.GetModelManager()
	if mm == nil {
		return false
	}
	op := balancer.ModelOpRequest{Operation: "load", ModelName: modelName}
	if prev.ContextKnown && prev.ContextSize > 0 {
		ctx := prev.ContextSize
		op.ContextSize = &ctx
	}
	if prev.KVCacheType != "" {
		kv := prev.KVCacheType
		op.KVCacheType = &kv
	}
	if prev.FlashAttn != nil {
		fa := *prev.FlashAttn
		op.FlashAttn = &fa
	}
	if prev.Mmap != nil {
		mmv := *prev.Mmap
		op.UseMmap = &mmv
	}
	if prev.BatchSize > 0 {
		bs := prev.BatchSize
		op.BatchSize = &bs
	}
	res := mm.ExecuteOperation(backendID, op)
	if res == nil || !res.Success {
		logger.Get().Warnw("R297: previous model configuration was NOT restored",
			"backend", backendID, "model", modelName,
			"previous_context", prev.ContextSize, "error", opErrText(res))
		return false
	}
	logger.Get().Warnw("R297: reload failed — previous model configuration restored",
		"backend", backendID, "model", modelName, "restored_context", prev.ContextSize)
	return true
}

// opErrText — nil-safe текст ошибки операции (для лога восстановления).
func opErrText(res *balancer.ModelOpResult) string {
	if res == nil {
		return "no result"
	}
	return res.Error
}
