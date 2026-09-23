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
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	_ = os.Setenv("LB_AUTO_LOAD_WAIT_SEC", "0")
	_ = os.Setenv("LB_ADMISSION_WAIT_SEC", "0")
	_ = os.Setenv("LB_NCTX_PREFLIGHT_WAIT_SEC", "0")
	os.Exit(m.Run())
}
