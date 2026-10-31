// cancel_load_r363.go — R363 (2026-10-31, аудит abort A4): отмена во время ЗАГРУЗКИ.
//
// ЖИВОЙ ДЕФЕКТ. Отмена во время загрузки модели не работала ПО ЗАМЫСЛУ: загрузка
// отвязана от клиентского ctx (это правильно — иначе первый же ушедший клиент обрывал
// загрузку, которую ждут остальные), abort-флаг выставлялся только для генерации.
// Клиент нажимал Stop, получал 200 «no_active_generation», а модель продолжала читаться:
// минуты ожидания и занятая VRAM, отменить нельзя.
//
// РЕШЕНИЕ ОПЕРАТОРА (дословно): «Да загрузку прерывать при отмене». Реализуем явной
// меткой cancel_load в приказе: балансер ставит её ТОЛЬКО для клиентской отмены (кнопка
// Stop), а не для сторожевых обрывов (таймаут чужого стрима, тишина upstream) — иначе
// таймаут одного клиента обрывал бы загрузку, которую ждут другие.
//
// Механика та же, что у force-выгрузки грузящейся модели (R215): bridge.RequestLoadAbort
// выставляет флаг, llama.cpp проверяет его в progress-callback и останавливает чтение
// весов, после чего сам освобождает частично загруженную модель (включая буферы на
// узлах раскладки). Флаг проверяется не мгновенно (раз в тензор), поэтому отдельно
// различаем «уже остановилась» и «флаг выставлен, ещё останавливается».
package main

import (
	"time"
)

const (
	// cancelLoadOutcomeCancelledR363 — загрузка остановлена, модель освобождена.
	cancelLoadOutcomeCancelledR363 = "load_cancelled"
	// cancelLoadOutcomeStillLoadingR363 — флаг выставлен, загрузка ещё останавливается.
	cancelLoadOutcomeStillLoadingR363 = "load_abort_requested"
	// cancelLoadOutcomeNotLoadingR363 — модель не в состоянии загрузки: отменять нечего.
	cancelLoadOutcomeNotLoadingR363 = ""
)

// cancelLoadWaitR363 — сколько ждём, пока загрузка реально отпустит после флага.
//
// R215 использует те же 15 с для force-выгрузки: C-мост проверяет флаг на каждом
// тензоре, поэтому обычно это сотни миллисекунд, но на крупном тензоре с диска может
// быть дольше. Ждать дольше нельзя — клиент ждёт ответа на Stop.
const cancelLoadWaitR363 = 15 * time.Second

// cancelLoadOutcomeR363 — вердикт по фактам (вынесено для тестов без C-моста).
//
// loadingBefore — модель была в состоянии загрузки на момент приказа;
// aborted — bridge.RequestLoadAbort принял флаг;
// loadingAfter — модель всё ещё грузится после ожидания.
func cancelLoadOutcomeR363(loadingBefore, aborted, loadingAfter bool) string {
	if !loadingBefore {
		return cancelLoadOutcomeNotLoadingR363
	}
	if aborted && !loadingAfter {
		return cancelLoadOutcomeCancelledR363
	}
	// Флаг не приняли (ptr не выставлен) ИЛИ загрузка ещё идёт: говорим правду, а не
	// «отменено». Второй случай штатный — флаг проверяется раз в тензор.
	return cancelLoadOutcomeStillLoadingR363
}

// abortLoadingModelR363 — попытаться прервать загрузку модели.
//
// Возвращает вердикт ("" — модель не грузится) и признак «флаг принят».
func abortLoadingModelR363(model string) (outcome string, aborted bool) {
	if model == "" {
		return cancelLoadOutcomeNotLoadingR363, false
	}
	loadingBefore := backend.IsLoading(model)
	if !loadingBefore {
		return cancelLoadOutcomeNotLoadingR363, false
	}
	aborted = backend.AbortLoad(model)
	loadingAfter := loadingBefore
	if aborted {
		// Ждём, пока загрузка отпустит: C-мост проверяет флаг на каждом тензоре.
		deadline := time.Now().Add(cancelLoadWaitR363)
		for time.Now().Before(deadline) && backend.IsLoading(model) {
			time.Sleep(100 * time.Millisecond)
		}
		loadingAfter = backend.IsLoading(model)
	}
	return cancelLoadOutcomeR363(loadingBefore, aborted, loadingAfter), aborted
}
