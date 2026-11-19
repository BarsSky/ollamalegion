// handlers_cluster_models_result.go — R639 (2026-11-19): итог перезагрузки: сводный статус, «всё ещё загружается», отложенный результат.
//
// ЗАЧЕМ ВЫНЕСЕНО. `handlers_cluster_models.go` держал 1628 строк:
// агрегированные списки, перезагрузку модели, массовые операции и состояние
// модели на бэкенде. Здесь — один из слоёв; поведение не менялось.
package api

import (
	"context"
	"fmt"
	"net/http"
	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/logger"
	"strings"
)

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

// selectReloadTargets — R263: перенесён в reload_targets.go вместе с
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
	// Правило живёт в loadWithEvictionOnMemoryFailure (cluster_load_eviction.go)
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
		result.HTTPStatus = opFailureStatusR371(opResult)
		result.Status = "error"
		result.Error = opResult.Error
		result.RoleRefused = opResult.RoleRefused
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
