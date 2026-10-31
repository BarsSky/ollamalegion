// cancel_supported_r362_test.go — R362 (2026-10-31), аудит abort A6 (сторона воркера).
//
// ЖИВОЙ ДЕФЕКТ. В stub-сборке (-tags llama_stub) per-inference abort-флаг всегда nil:
// отмена физически невозможна, генерация идёт до n_predict. Но POST /api/cancel отвечал
// «cancelled: N» и статусом "cancelled" — то есть врал и клиенту, и балансеру, и тестам.
//
// R362 отдаёт возможность сборки явно: /api/cancel → supported, при false статус
// cancel_not_supported + notice; /api/info → cancel_supported.
//
// Проверяется:
//  1. stub-сборка сообщает supported=false (базовый факт: abort-флага нет);
//  2. обработчик отмены использует это поле и статус cancel_not_supported;
//  3. /api/info отдаёт cancel_supported;
//  4. обе точки ссылаются на одну функцию bridge.InferAbortSupported (не на копию
//     константы), иначе сборки разойдутся.
package main

import (
	"os"
	"strings"
	"testing"

	"ollama-loadbalancer/c/bridge"
)

// TestR362_StubBuildReportsNoCancelSupport — под тегом llama_stub поддержка обязана быть false.
func TestR362_StubBuildReportsNoCancelSupport(t *testing.T) {
	if bridge.InferAbortSupported() {
		t.Fatal("stub-сборка сообщает, что отмена ПОДДЕРЖИВАЕТСЯ: у неё abort-флаг всегда nil, " +
			"значит /api/cancel снова начнёт рапортовать успех (аудит A6)")
	}
}

// TestR362_CancelHandlerReportsSupport_STEWARD — обработчик и /api/info используют поле.
func TestR362_CancelHandlerReportsSupport_STEWARD(t *testing.T) {
	read := func(name string) string {
		for _, p := range []string{name, "cmd/cppworker/" + name} {
			if b, err := os.ReadFile(p); err == nil {
				return string(b)
			}
		}
		return ""
	}

	cancel := read("handlers_cancel.go")
	if cancel == "" {
		t.Fatal("не удалось прочитать handlers_cancel.go")
	}
	if !strings.Contains(cancel, "bridge.InferAbortSupported()") {
		t.Error("/api/cancel не спрашивает bridge.InferAbortSupported: в stub-сборке он " +
			"снова будет рапортовать «cancelled: N», хотя отменять нечем")
	}
	if !strings.Contains(cancel, `"supported"`) {
		t.Error("/api/cancel не отдаёт поле supported — вызывающий не отличит " +
			"«отменили» от «отменять нечем»")
	}
	if !strings.Contains(cancel, "cancel_not_supported") {
		t.Error("/api/cancel не использует статус cancel_not_supported")
	}

	info := read("handlers_health.go")
	if info == "" {
		t.Fatal("не удалось прочитать handlers_health.go")
	}
	if !strings.Contains(info, `status["cancel_supported"] = bridge.InferAbortSupported()`) {
		t.Error("/api/info не отдаёт cancel_supported: оператор узнаёт о невозможности " +
			"отмены только по факту «нажал — ничего не произошло»")
	}
}
