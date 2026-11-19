package api

import (
	"net/http"

	"ollama-loadbalancer/internal/balancer"
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

	// R371 (2026-11-01): загрузку запретила РОЛЬ узла (донор раскладки не держит
	// своих копий). Отдельное поле, чтобы UI показал «выбран донор» без разбора
	// текста ошибки, и чтобы HTTP-код можно было честно поставить 409, а не 500.
	RoleRefused bool `json:"roleRefused,omitempty"`

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
