//go:build llama_stub

// r67a_testmain_test.go — R67a (2026-09-23).
//
// Интеграционные тесты пакета ./tests проверяют маршрутизацию, режимы и
// контракты, а НЕ ожидание авто-загрузки модели. До R67a cold-start запрос к
// незагруженной модели получал быстрый 503, и тесты опирались на это
// (например TestServeHTTP_VirtualRouterMode_RejectsOllamaBackends вызывал
// auto-load путь и завершался за миллисекунды).
//
// После R67a балансер по умолчанию ЖДЁТ загрузку (LB_AUTO_LOAD_WAIT_SEC=180,
// см. internal/balancer/autoload_wait.go) — это правильное поведение для
// клиентов, но в тестах оно превращалось в 180-секундное ожидание и таймаут
// пакета (test timed out after 1m40s).
//
// Поэтому для всего пакета явно включаем legacy-режим «сразу 503».
// Новое поведение проверяется там, где ему и место:
//   - internal/balancer/autoload_wait_r67a_test.go (юнит: ждём и обслуживаем);
//   - tests/cppworker_lazy_load/*_r67a_test.go (интеграционный контракт).
package tests

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// legacy: не ждать авто-загрузку в интеграционных тестах
	_ = os.Setenv("LB_AUTO_LOAD_WAIT_SEC", "0")
	os.Exit(m.Run())
}
