// autoload_wait_classify_r451_test.go — R451 (2026-11-06): отложенная загрузка
// не должна выглядеть как начатая.
//
// ЖИВОЙ ФАКТ (порт 18079, модель на диске, но не загружена). На бэкенде шёл
// чужой стрим, загрузку ОТЛОЖИЛИ (R271/R292), а клиент получил:
//
//	HTTP 503 "model %q is not loaded and auto-load failed: model %q load
//	started, waiting for cppworker to finish (~30-180s depending on n_ctx)"
//
// то есть совет ждать работу, которую не начинали. В логе в ту же секунду:
// `R292: model operation deferred — active client streams on backend`.
package balancer

import (
	"errors"
	"strings"
	"testing"
)

// TestR451_DeferredLoadIsNotReportedAsStarted — typed-ошибка «отложено»
// доходит до ответа клиента, а не подменяется текстом «load started».
func TestR451_DeferredLoadIsNotReportedAsStarted(t *testing.T) {
	deferred := &destructiveDeferredErrorR292{
		BackendID: "bk-1",
		Model:     "m",
		Streams:   1,
		RetrySec:  5,
	}

	cause, terminal := autoloadWaitFailureR451(deferred, nil)
	if !terminal {
		t.Fatal("отложенная загрузка признана «идёт» — клиент снова получит «load started»")
	}
	if !IsDestructiveDeferredR292(cause) {
		t.Fatalf("причина потеряла тип «отложено»: %v", cause)
	}

	// Итоговый текст ответа обязан говорить «занято», а не «загрузка идёт».
	text := autoLoadFailureTextR292("m", cause)
	if strings.Contains(text, "load started") {
		t.Fatalf("ответ утверждает, что загрузка начата: %q", text)
	}
	if !strings.Contains(text, "busy serving another request") {
		t.Fatalf("ответ не объясняет причину (занято живым стримом): %q", text)
	}
	if !strings.Contains(text, "retry in 5s") {
		t.Fatalf("ответ не называет срок повтора из R292: %q", text)
	}
}

// TestR451_RealLoadFailureStillTerminal — настоящий провал загрузки не теряется.
func TestR451_RealLoadFailureStillTerminal(t *testing.T) {
	loadErr := errors.New("model load failed: failed to open GGUF file: no such file")
	cause, terminal := autoloadWaitFailureR451(loadErr, nil)
	if !terminal {
		t.Fatal("реальный провал загрузки перестал быть терминальным")
	}
	if cause.Error() != loadErr.Error() {
		t.Fatalf("текст причины изменился: %q", cause.Error())
	}
}

// TestR451_FailureFromChannelStillReported — провал, пришедший по каналу.
func TestR451_FailureFromChannelStillReported(t *testing.T) {
	ch := make(chan error, 1)
	ch <- errors.New("no such host")

	cause, terminal := autoloadWaitFailureR451(errors.New("model load did not finish within 180s"), ch)
	if !terminal {
		t.Fatal("провал из канала потерян")
	}
	if !strings.Contains(cause.Error(), "no such host") {
		t.Fatalf("причина из канала не попала в ответ: %v", cause)
	}
}

// TestR451_GenuineTimeoutKeepsWaitMessage — загрузка действительно идёт:
// остаётся «подожди», потому что причина именно в этом.
func TestR451_GenuineTimeoutKeepsWaitMessage(t *testing.T) {
	cause, terminal := autoloadWaitFailureR451(nil, make(chan error, 1))
	if terminal || cause != nil {
		t.Fatalf("таймаут без причины не должен быть терминальным: cause=%v terminal=%v", cause, terminal)
	}
}
