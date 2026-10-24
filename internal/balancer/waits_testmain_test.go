// waits_testmain_test.go — R67a/R67b/R68 (2026-09-23).
//
// Пакет internal/balancer содержит ~800 юнит-тестов, которые проверяют
// маршрутизацию, выбор бэкенда, профили и т.п., а НЕ ожидание готовности
// модели/слота. К R68 в балансере появились три «ждём вместо ошибки» ручки,
// и все три по умолчанию включены (это правильное поведение для клиентов, но
// в тестах оно превращается в минуты ожидания):
//
//   - LB_AUTO_LOAD_WAIT_SEC      (R67a, default 180 c) — ждать авто-загрузку;
//   - LB_ADMISSION_WAIT_SEC      (R67b, default 300 c) — ждать свободный слот;
//   - LB_NCTX_PREFLIGHT_WAIT_SEC (R68,  default 240 c) — ждать reload n_ctx.
//
// Замер: без этого TestMain пакет с -race шёл 250 c вместо 90 c (тесты,
// случайно уходившие в wait-путь, ждали таймаут). Для всего пакета включаем
// legacy-режим «сразу 503», а проверка нового поведения — в тестах, которые
// выставляют значение явно (t.Setenv / setAdmissionWait):
//
//   - autoload_wait_r67a_test.go        — авто-загрузка;
//   - admission_queue_r67b_test.go      — admission-очередь;
//   - preflight_profile_hint_r68_test.go — ожидание reload n_ctx.
package balancer

import (
	"net/http"
	"os"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	_ = os.Setenv("LB_AUTO_LOAD_WAIT_SEC", "0")
	_ = os.Setenv("LB_ADMISSION_WAIT_SEC", "0")
	_ = os.Setenv("LB_NCTX_PREFLIGHT_WAIT_SEC", "0")

	// R118.14 (2026-10-14): ПРЕДОХРАНИТЕЛЬ ОТ ЗАВИСАНИЯ ВСЕГО ПАКЕТА.
	//
	// Десятки тестов в этом пакете ходят в свои httptest-серверы через
	// http.Post/http.Get, то есть через http.DefaultClient — БЕЗ таймаута. Если
	// конкретный сценарий (например «обрыв соединения», scenarios_cppworker_test.go)
	// подвисает, зависает не тест, а ВЕСЬ пакет: `go test` падал по общему
	// таймауту с «panic: test timed out», и по выводу нельзя было понять, какой
	// тест виноват (живой прогон: пакет прошёл за 105 с, следующий запуск повис
	// на TestCPPWorker_Failure_NetworkDrop и упал по таймауту 2m30s).
	//
	// Таймаут на DefaultClient превращает такое зависание в обычный FAIL того
	// самого теста. 90 c — с запасом больше любого легитимного ожидания в тестах
	// (все «ждём вместо ошибки» ручки здесь выключены выше).
	http.DefaultClient.Timeout = 90 * time.Second

	os.Exit(m.Run())
}
