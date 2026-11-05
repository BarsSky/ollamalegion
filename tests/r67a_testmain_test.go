//go:build llama_stub

// r67a_testmain_test.go — R67a/R67b (2026-09-23).
//
// Интеграционные тесты пакета ./tests проверяют маршрутизацию, режимы и
// контракты, а НЕ ожидание авто-загрузки модели и НЕ admission-очередь.
// До R67a cold-start запрос к незагруженной модели получал быстрый 503, и тесты
// опирались на это (например TestServeHTTP_VirtualRouterMode_RejectsOllamaBackends
// вызывал auto-load путь и завершался за миллисекунды).
//
// R67a: балансер по умолчанию ЖДЁТ загрузку (LB_AUTO_LOAD_WAIT_SEC=180,
// см. internal/balancer/autoload_wait.go) — это правильное поведение для
// клиентов, но в тестах оно превращалось в 180-секундное ожидание и таймаут
// пакета (test timed out after 1m40s).
//
// R67b: появилась admission-очередь слотов (LB_ADMISSION_WAIT_SEC=300,
// см. internal/balancer/admission_queue.go) — запрос при занятых слотах ждёт до
// 5 минут. В интеграционных тестах это дало бы такие же зависания, поэтому оба
// ожидания здесь выключаются (legacy-поведение «сразу 503»).
//
// Новое поведение проверяется там, где ему и место:
//   - internal/balancer/autoload_wait_r67a_test.go (юнит: ждём и обслуживаем);
//   - internal/balancer/admission_queue_r67b_test.go (юнит + HTTP-уровень);
//   - tests/cppworker_lazy_load/*_r67a_test.go (интеграционный контракт).
package tests

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// legacy: не ждать авто-загрузку и не вставать в admission-очередь
	_ = os.Setenv("LB_AUTO_LOAD_WAIT_SEC", "0")
	_ = os.Setenv("LB_ADMISSION_WAIT_SEC", "0")
	// R447: опрос upstream llama.cpp (R269) выключаем на весь пакет.
	//
	// Живой факт: прогон ./tests/... оставлял в рабочем дереве untracked
	// `tests/testdata/llama-upstream.json` с `"lastError":"github api: HTTP
	// 403"` — то есть модульный тест ходил в api.github.com. Это и зависимость
	// от внешней сети (в CI — rate limit и таймауты), и та же грязь в дереве,
	// что чинил R70 для state.json. Тест на самую проверку живёт там, где ей
	// место (internal/balancer/llama_upstream_r269_test.go, с подменой базы API).
	_ = os.Setenv("LB_LLAMA_UPSTREAM_CHECK_ENABLED", "0")
	os.Exit(m.Run())
}
