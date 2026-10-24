// cluster_load_eviction_r119.go — R119 (2026-10-15): загрузка второй модели на
// одном бэкенде.
//
// ЖИВОЙ ДЕФЕКТ. `executeReloadOnBackend` перед ЛЮБОЙ загрузкой выгружал чужую
// модель с того же бэкенда — безусловно и без проверки памяти:
//
//	// Bug #8: auto-unload чужой модели при load
//	loadedModel, loaded := s.getLoadedModelOnBackend(backendID)
//	if loaded && loadedModel != modelName { → unload(loadedModel) }
//
// Комментарий объяснял это страхом OOM («cppworker пытается загрузить B, но model A
// уже заняла VRAM»), но правило было грубее проблемы: пока на бэкенде лежит модель,
// балансер физически не мог поднять вторую — даже если обе влезают. Оператор видел
// «вторая модель не грузится, первая пропала».
//
// ЧТО ТЕПЕРЬ. Вытеснение из «до» стало «только если действительно не влезло»:
//
//  1. сначала пробуем загрузить как есть — cppworker сам считает memfit по
//     доступной памяти и при нехватке отвечает понятной ошибкой (не падением);
//  2. если загрузка упала И ошибка похожа на нехватку памяти И на бэкенде
//     осталась другая модель — выгружаем её и повторяем попытку ОДИН раз;
//  3. если чужую модель выгрузить нельзя (занята активными запросами) — честно
//     говорим об этом, а не сносим её молча.
//
// Итог: две модели на одном бэкенде работают, когда память позволяет; когда не
// позволяет — поведение прежнее (освободить и загрузить), плюс понятное объяснение.
package api

import (
	"fmt"
	"strings"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/logger"
)

// modelOpRunner — минимум от *balancer.ModelManager, нужный загрузке с
// вытеснением. Интерфейс, а не конкретный тип, чтобы правило проверялось
// юнит-тестом с фейковым раннером: в api-пакете нет живого ModelManager.
type modelOpRunner interface {
	ExecuteOperation(backendID string, req balancer.ModelOpRequest) *balancer.ModelOpResult
}

// loadFailureLooksLikeMemoryShortage — «эта ошибка про память?».
//
// Матчер намеренно ПЕРМИССИВНЫЙ: ложное «да» стоит одной лишней выгрузки и
// повторной попытки, ложное «нет» — оператор снова увидит «вторая модель не
// грузится». Поэтому ловим и английские, и русские формулировки (cppworker
// отдаёт `cannot load model <name>: <suggestion>`, где suggestion — текст memfit
// вида «Веса модели не помещаются ни в VRAM, ни в RAM»).
func loadFailureLooksLikeMemoryShortage(msg string) bool {
	low := strings.ToLower(msg)
	if low == "" {
		return false
	}
	for _, marker := range []string{
		"vram", "ram", "memory", "памят", "помеща", "allocate", "alloc", "oom",
		"out of memory", "insufficient", "too large", "no space",
		"not fit", "do not fit", "doesn't fit", "does not fit",
	} {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}

// loadWithEvictionOnMemoryFailure — загрузить модель, вытеснив чужую ТОЛЬКО если
// первая попытка упала по памяти.
//
// Возвращает результат последней попытки и человекочитаемую заметку о том, что
// именно произошло с чужой моделью ("" — ничего не вытесняли).
func loadWithEvictionOnMemoryFailure(
	runner modelOpRunner,
	backendID, modelName, otherLoaded string,
	req balancer.ModelOpRequest,
) (*balancer.ModelOpResult, string) {
	if runner == nil {
		return nil, ""
	}
	first := runner.ExecuteOperation(backendID, req)
	if first != nil && first.Success {
		// Поместилось рядом — чужую модель не трогаем (это и есть «две модели на
		// одном бэкенде»).
		return first, ""
	}
	if otherLoaded == "" || otherLoaded == modelName {
		return first, ""
	}
	failureMsg := ""
	if first != nil {
		failureMsg = first.Error + " " + first.Message
	}
	if !loadFailureLooksLikeMemoryShortage(failureMsg) {
		// Ошибка не про память (нет файла, неверное имя, битый GGUF) — выгрузка
		// чужой модели ничего не исправит и только убьёт рабочий прогрев.
		return first, ""
	}

	logger.Get().Infow("cluster load: загрузка не влезла рядом с другой моделью — пробуем освободить",
		"backend_id", backendID, "requested_model", modelName, "loaded_model", otherLoaded)

	unloadResult := runner.ExecuteOperation(backendID, balancer.ModelOpRequest{
		Operation: "unload",
		ModelName: otherLoaded,
	})
	if unloadResult == nil || !unloadResult.Success {
		errText := "unload failed"
		busy := false
		if unloadResult != nil {
			errText = unloadResult.Error
			busy = unloadResult.Busy
		}
		if busy {
			return first, fmt.Sprintf(
				"модель %q занята активными inference-запросами — выгрузка не выполнена, "+
					"поэтому вторая модель не загружена; повторите после завершения запросов",
				otherLoaded)
		}
		return first, fmt.Sprintf(
			"не удалось выгрузить %q, чтобы освободить память (%s)", otherLoaded, errText)
	}

	retry := runner.ExecuteOperation(backendID, req)
	if retry != nil && retry.Success {
		logger.Get().Infow("cluster load: загружено после выгрузки другой модели",
			"backend_id", backendID, "requested_model", modelName, "unloaded_model", otherLoaded)
		return retry, fmt.Sprintf(
			"загружено после выгрузки %q: памяти не хватало на две модели", otherLoaded)
	}
	retryErr := "load failed"
	if retry != nil {
		retryErr = retry.Error
	}
	return retry, fmt.Sprintf(
		"загрузка не удалась даже после выгрузки %q: %s", otherLoaded, retryErr)
}
