// handlers_cluster_models_state.go — R639 (2026-11-19): состояние модели на бэкенде: загружена ли, какой контекст, cluster state, поля для лога.
//
// ЗАЧЕМ ВЫНЕСЕНО. `handlers_cluster_models.go` держал 1628 строк:
// агрегированные списки, перезагрузку модели, массовые операции и состояние
// модели на бэкенде. Здесь — один из слоёв; поведение не менялось.
package api

import (
	"fmt"
	"net/http"
	"ollama-loadbalancer/pkg/types"
	"strings"
)

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
