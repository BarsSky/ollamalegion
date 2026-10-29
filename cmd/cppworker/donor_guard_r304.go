// donor_guard_r304.go — R304 (2026-10-30): единый ГАРД «донор не поднимает своих
// копий моделей» на стороне cppworker.
//
// ДОКТРИНА ОПЕРАТОРА (дословно): «Если бэкенд становится донором, он уже не может
// подымать свои копии моделей — он становится именно донором мастера и может
// только делить память с мастером.»
//
// ЗАЧЕМ ГАРД-ФУНКЦИЯ, А НЕ ПРОВЕРКА В КАЖДОМ ОБРАБОТЧИКЕ. Точек, создающих
// локальную копию модели, в этом сервисе пять (явная загрузка, загрузка с
// параметрами, reload, импорт файла, ленивая загрузка по запросу чата) плюс три
// фоновых (авто-загрузка при старте, reload под новые дефолты, RAM-fallback).
// Пока запрет копировался бы по обработчикам, каждая новая ручка загрузки
// появлялась бы БЕЗ него — это повторяющийся класс дефектов проекта «новая точка
// обхода общего пути». Поэтому запрет здесь один, а стюард-тест
// (donor_guard_steward_r304_test.go) проверяет, что ни один обработчик не зовёт
// загрузку в обход него.
//
// ПОЧЕМУ 409, А НЕ 403/400. Запрос синтаксически верен и в другой роли прошёл бы;
// конфликт в ТЕКУЩЕМ СОСТОЯНИИ машины — это ровно Conflict. Код ошибки
// (donor_cannot_host_models) — машинный, чтобы балансер и панель не разбирали
// текст.
package main

import (
	"errors"
	"fmt"
	"net/http"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
)

// donorErrorCodeR304 — машинный код отказа (уезжает в JSON полем error и служит
// признаком в errors.Is для внутренних путей).
const donorErrorCodeR304 = "donor_cannot_host_models"

// errDonorCannotHostModelsR304 — тот же отказ в виде ошибки: внутренние пути
// (ленивая загрузка, RAM-fallback) не пишут HTTP-ответ сами, они возвращают
// ошибку, а ответ строит обработчик через writeModelLoadError/handleInferenceError.
var errDonorCannotHostModelsR304 = errors.New(donorErrorCodeR304)

// donorMarkR304 — пометка донора из ЖИВОГО бэкенда.
//
// backend == nil возможен на самом раннем старте и в тестах без поднятого
// бэкенда: тогда роль неизвестна и запрета нет. Отказ «по отсутствию данных»
// заблокировал бы загрузку тем, кто просто ещё не инициализирован.
func donorMarkR304() cppbackend.DonorMark {
	if backend == nil {
		return cppbackend.DonorMark{Role: "", Donor: false, CanHostOwnModels: true}
	}
	return backend.DonorMark()
}

// donorRefusalBodyR304 — тело отказа. Состав полей важен: панель показывает
// role/reason, а message/hint объясняют оператору, что произошло и что делать
// (текст из i18n, чтобы язык совпадал с остальным интерфейсом).
func donorRefusalBodyR304(mark cppbackend.DonorMark) map[string]interface{} {
	return map[string]interface{}{
		"status":  "error",
		"error":   donorErrorCodeR304,
		"role":    cppbackend.RoleDonor,
		"reason":  mark.Reason,
		"message": i18n.T("cppbackend.donor.load_refused"),
		"hint":    i18n.T("cppbackend.donor.load_refused_hint"),
	}
}

// writeDonorRefusalR304 — записать отказ (409 + JSON) и оставить след в логе.
//
// Лог обязателен: отказ приходит и от явного действия оператора, и от
// автоматического пути (ленивая загрузка, reload под дефолты), и без строки в
// логе «почему модель не поднялась» пришлось бы выяснять по коду ответа в чужом
// клиенте.
func writeDonorRefusalR304(w http.ResponseWriter, mark cppbackend.DonorMark) {
	logger.Get().Warnw(i18n.T("cppworker.donor.load_refused_log"),
		"role", mark.Role,
		"reason", mark.Reason,
		"own_copies", mark.OwnCopies,
		"hint", i18n.T("cppbackend.donor.load_refused_hint"))
	writeJSON(w, http.StatusConflict, donorRefusalBodyR304(mark))
}

// donorLoadGuard — ПЕРВАЯ содержательная проверка обработчика, создающего
// локальную копию модели. Возвращает true, если запрос отклонён (ответ уже
// записан) — вызывающий обязан сделать return.
//
// МЕСТО В ОБРАБОТЧИКЕ. Проверка стоит до разбора тела, до TryLockLoad и до
// ответа 202 «загрузка принята»: копию, которую поднимать нельзя, не нужно ни
// заявлять, ни ставить в очередь — иначе клиент получит «loading», а через
// секунду провал, и на пустом месте появится запись о неудачной загрузке.
func donorLoadGuard(w http.ResponseWriter) bool {
	mark := donorMarkR304()
	if mark.CanHostOwnModels {
		return false
	}
	writeDonorRefusalR304(w, mark)
	return true
}

// donorOwnCopyRefusedR304 — тот же запрет для путей БЕЗ http.ResponseWriter
// (авто-загрузка при старте, reload под дефолты, RAM-fallback, CLI).
//
// where — короткая метка источника («auto-load», «reload-defaults»), чтобы в логе
// было видно, какой именно путь упёрся в роль: у всех них разное поведение при
// отказе, и «просто запрещено» без источника не отладить.
func donorOwnCopyRefusedR304(where string) (bool, cppbackend.DonorMark) {
	mark := donorMarkR304()
	if mark.CanHostOwnModels {
		return false, mark
	}
	logger.Get().Warnw(i18n.T("cppworker.donor.load_refused_log"),
		"where", where,
		"role", mark.Role,
		"reason", mark.Reason,
		"own_copies", mark.OwnCopies,
		"hint", i18n.T("cppbackend.donor.load_refused_hint"))
	return true, mark
}

// donorRefusalErrorR304 — отказ в виде ошибки для внутренних путей.
//
// Текст собирается из кода и причины (а не только из i18n-сообщения): ошибка
// попадает и в реестр причин загрузки (/api/models/load/progress), и в лог, где
// машинный код нужнее человеческого текста.
func donorRefusalErrorR304(mark cppbackend.DonorMark) error {
	return fmt.Errorf("%w: reason=%s", errDonorCannotHostModelsR304, mark.Reason)
}
