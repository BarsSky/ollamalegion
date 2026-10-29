// reload_params_r286.go — R286 (2026-10-29): сравнение ВСЕХ значимых параметров
// загрузки, а не только окна контекста.
//
// ЖИВОЙ ДЕФЕКТ (стенд r284). Оператор менял в WebUI `kvCacheType` и жал «перезагрузить»
// — в ответ приходило `ok: already loaded`, модель не перезагружалась, и настройка
// не применялась. Причина: после R281 «нужна ли перезагрузка» решала функция
// needContextReload, которая сравнивает ТОЛЬКО `contextSize`. То есть смена
// kv-cache/flash-attn/mmap/batch/gpu-layers была no-op — снаружи неотличимо от
// «балансер игнорирует мои настройки» (ровно та жалоба, с которой начался аудит).
//
// ПРАВИЛО СРАВНЕНИЯ (одно, чистое, покрыто таблицей в тесте):
//   - параметр НЕ задан в запросе → не сравниваем (nil = «оператор не трогал»);
//   - значение загруженной модели НЕИЗВЕСТНО → не перезагружаем из-за неизвестности:
//     догадка «раз не сходится, значит надо перезагрузить» жгла бы минуты на
//     каждой команде, где cppworker не сообщает поле (старые сборки, gpu_layers);
//   - известно и отличается → diff с обоими значениями (в ответе — числа/имена,
//     чтобы «перезагрузили» было отличимо от «ничего не делали»).
package api

import (
	"fmt"
	"strings"

	"ollama-loadbalancer/pkg/types"
)

// loadedModelParams — ФАКТИЧЕСКИЕ параметры загруженной модели, насколько о них
// сообщает cppworker (types.LlamaCppModel). Каждое поле знает, известно ли оно:
// «0/пусто» у разных полей означает разное, и путать «неизвестно» с «выключено»
// здесь нельзя.
type loadedModelParams struct {
	ContextSize  int
	ContextKnown bool

	// KVCacheType — "" = cppworker не сообщил.
	KVCacheType string
	// FlashAttn — nil = неизвестно. Известным считаем только положительное
	// значение либо явный 0 при известном kv-cache: cppworker исторически отдаёт
	// 0 и как «off», и как «не сообщил» (см. комментарий в types.LlamaCppModel),
	// поэтому 0 сам по себе доказательством не является.
	FlashAttn *int
	// Mmap — nil = неизвестно. Известным считаем только true: у bool-поля нет
	// третьего состояния, и «false» одинаково означает «выключен» и «не сообщил».
	// Поэтому diff возможен лишь в сторону «было включено, просят выключить».
	Mmap *bool
	// BatchSize — 0 = неизвестно.
	BatchSize int
	// GPULayers — nil = неизвестно. cppworker НЕ отдаёт gpu_layers в /api/models
	// (вместо него приходит точная раскладка слоёв, R162), поэтому это поле
	// фактически всегда неизвестно — и сравнение по нему не делается.
	GPULayers *int
}

// loadParamDiff — одно значимое отличие «загружено → запрошено».
//
// Поля — строки, чтобы параметры с разной природой (число, bool, имя kv-cache)
// попадали в один ответ и один лог без второй структуры на каждый случай.
type loadParamDiff struct {
	Param     string `json:"param"`
	Loaded    string `json:"loaded,omitempty"`
	Requested string `json:"requested"`
}

// loadedModelParamsOnBackend — снять фактические параметры загруженной модели.
//
// Второе значение = «модель вообще загружена на этом бэкенде». Оно нужно отдельно
// от полей: если модель не загружена, сравнивать нечего и перезагружать нечего.
func (s *Server) loadedModelParamsOnBackend(backendID, modelName string) (loadedModelParams, bool) {
	var out loadedModelParams
	cs := s.getClusterState()
	if cs == nil {
		return out, false
	}
	for i := range cs.Backends {
		b := &cs.Backends[i]
		if b.ID != backendID {
			continue
		}
		for j := range b.LlamaCpp.LoadedModels {
			m := &b.LlamaCpp.LoadedModels[j]
			if m.Name != modelName {
				continue
			}
			out.ContextSize = m.ContextLength
			out.ContextKnown = m.ContextLength > 0
			out.KVCacheType = strings.TrimSpace(m.KvCacheType)
			if m.KvCacheType != "" {
				// FlashAttn становится сопоставимым только когда kv-cache сообщён:
				// иначе 0 неотличим от «не сообщил».
				fa := m.FlashAttnType
				out.FlashAttn = &fa
			}
			if m.UseMmap {
				yes := true
				out.Mmap = &yes
			}
			out.BatchSize = m.BatchSize
			if m.NumGPULayers > 0 {
				layers := m.NumGPULayers
				out.GPULayers = &layers
			}
			return out, true
		}
	}
	return out, false
}

// loadParamsDiff — какие ЗНАЧИМЫЕ параметры отличаются у загруженной модели от
// запрошенных. Пустой срез = «перезагружать нечего» (честное already loaded).
//
// Порядок проверок фиксирован, чтобы ответ и лог были детерминированными
// (оператор видит первым самое важное: окно, затем kv-cache).
func loadParamsDiff(loaded loadedModelParams, req clusterReloadModelRequest) []loadParamDiff {
	diffs := make([]loadParamDiff, 0, 4)

	// 1. Окно контекста — прежнее правило R281, без изменений семантики.
	if needContextReload(loaded.ContextSize, loaded.ContextKnown, req.ContextSize) {
		diffs = append(diffs, loadParamDiff{
			Param:     "contextSize",
			Loaded:    ctxLoadedString(loaded.ContextSize, loaded.ContextKnown),
			Requested: fmt.Sprintf("%d", *req.ContextSize),
		})
	}

	// 2. Тип kv-cache.
	if v := strings.TrimSpace(derefString(req.KVCacheType)); v != "" {
		switch {
		case loaded.KVCacheType == "":
			// неизвестно — не перезагружаем из-за неизвестности
		case !strings.EqualFold(loaded.KVCacheType, v):
			diffs = append(diffs, loadParamDiff{
				Param:     "kvCacheType",
				Loaded:    loaded.KVCacheType,
				Requested: v,
			})
		}
	}

	// 3. flash attention. -1 у запроса = «авто», сравнивать не с чем.
	if req.FlashAttn != nil && *req.FlashAttn >= 0 {
		if loaded.FlashAttn != nil && *loaded.FlashAttn != *req.FlashAttn {
			diffs = append(diffs, loadParamDiff{
				Param:     "flashAttn",
				Loaded:    fmt.Sprintf("%d", *loaded.FlashAttn),
				Requested: fmt.Sprintf("%d", *req.FlashAttn),
			})
		}
	}

	// 4. mmap. Сравниваем только «было включено → просят выключить»: у bool
	// нет «неизвестно», и обратное направление недоказуемо (false может значить
	// «не сообщил»).
	if req.UseMmap != nil && !*req.UseMmap && loaded.Mmap != nil && *loaded.Mmap {
		diffs = append(diffs, loadParamDiff{
			Param:     "useMmap",
			Loaded:    "true",
			Requested: "false",
		})
	}

	// 5. batch size.
	if req.BatchSize != nil && *req.BatchSize > 0 && loaded.BatchSize > 0 &&
		*req.BatchSize != loaded.BatchSize {
		diffs = append(diffs, loadParamDiff{
			Param:     "batchSize",
			Loaded:    fmt.Sprintf("%d", loaded.BatchSize),
			Requested: fmt.Sprintf("%d", *req.BatchSize),
		})
	}

	// 6. gpu layers. cppworker не сообщает это поле (R162: вместо него точная
	// раскладка), поэтому сравнение возможно, только если значение всё-таки
	// пришло. Без факта — молчим, а не перезагружаем «на всякий случай».
	if req.GPULayers != nil && loaded.GPULayers != nil && *req.GPULayers >= 0 &&
		*loaded.GPULayers != *req.GPULayers {
		diffs = append(diffs, loadParamDiff{
			Param:     "gpuLayers",
			Loaded:    fmt.Sprintf("%d", *loaded.GPULayers),
			Requested: fmt.Sprintf("%d", *req.GPULayers),
		})
	}

	return diffs
}

// ctxLoadedString — как показать загруженное окно в diff: число либо «unknown»
// (тогда перезагрузка идёт по правилу R281 «факт неизвестен + окно задано»).
func ctxLoadedString(loaded int, known bool) string {
	if !known {
		return "unknown"
	}
	return fmt.Sprintf("%d", loaded)
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// loadParamDiffNames — имена изменённых параметров для лога (одной строкой).
func loadParamDiffNames(diffs []loadParamDiff) []string {
	names := make([]string, 0, len(diffs))
	for _, d := range diffs {
		names = append(names, d.Param)
	}
	return names
}

// paramDiffIsBusyReport — сравнение пришло из статуса «занято» (для лога).
// Отдельная функция, чтобы формат лога был один на все места.
func reloadBusyReason(errText string) string {
	low := strings.ToLower(errText)
	switch {
	case strings.Contains(low, "busy"), strings.Contains(low, "active inference"):
		return "model busy with active inference requests"
	case strings.Contains(low, "reload already in progress"), strings.Contains(low, "already in progress"):
		return "reload already in progress"
	default:
		return ""
	}
}

var _ = types.LlamaCppModel{}
