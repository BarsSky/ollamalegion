// stream_limit_r259_test.go — R259 (2026-10-28): не обвинять upstream в наших
// лимитах и отменах.
//
// ЖИВАЯ ЖАЛОБА (Cline): «stream truncated by upstream before completion» — наш
// собственный текст. Часть срабатываний ложные: лимит строки SSE (прежний 1 МиБ)
// и наши отмены приписывались upstream.
package balancer

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestR259_LineLimitIsConfigurableAndNotUpstream — (а): лимит настраивается, а
// слишком длинная строка классифицируется как НАШ лимит, не как обрыв upstream.
func TestR259_LineLimitIsConfigurableAndNotUpstream(t *testing.T) {
	if got := streamMaxLineBytes(); got != streamMaxLineBytesDefault {
		t.Fatalf("дефолтный лимит = %d, want %d (8 МиБ)", got, streamMaxLineBytesDefault)
	}
	t.Setenv("LB_STREAM_MAX_LINE_BYTES", "1048576")
	if got := streamMaxLineBytes(); got != 1048576 {
		t.Fatalf("env-лимит = %d, want 1048576", got)
	}
	// Мусор и слишком маленькое значение игнорируются (дефолт).
	t.Setenv("LB_STREAM_MAX_LINE_BYTES", "42")
	if got := streamMaxLineBytes(); got != streamMaxLineBytesDefault {
		t.Fatalf("мусорный лимит = %d, want дефолт", got)
	}
	t.Setenv("LB_STREAM_MAX_LINE_BYTES", "nonsense")
	if got := streamMaxLineBytes(); got != streamMaxLineBytesDefault {
		t.Fatalf("нечисловой лимит = %d, want дефолт", got)
	}

	// Ошибка сканера «token too long» — НАШ лимит, не upstream.
	tooLong := errors.New("bufio.Scanner: token too long")
	if got := streamAbortReasonR259(tooLong, nil); got != streamAbortOurLimit {
		t.Fatalf("reason = %q, want %q", got, streamAbortOurLimit)
	}
	if streamAbortBlamesUpstream(streamAbortOurLimit) {
		t.Fatal("наш лимит не должен обвинять upstream")
	}
	if msg := streamAbortClientMessage(streamAbortOurLimit); strings.Contains(msg, "upstream") {
		t.Fatalf("клиентское сообщение обвиняет upstream: %q", msg)
	}
}

// TestR259_FinishReasonIsTerminal — (б): классификатор не считает обрывом поток,
// закрытый без [DONE]: сам цикл помечает streamCompleted по finish_reason
// (llamacpp_transport.go: hasFinishReason), поэтому сюда попадает только реальный
// обрыв. Проверяем контракт границы: чистый EOF без ошибки — причина eof (тогда
// как при finish_reason цикл до этого блока вообще не доходит).
func TestR259_FinishReasonIsTerminal(t *testing.T) {
	// Сам факт «finish_reason терминален» проверяется существующим разбором:
	// hasFinishReason обязан видеть finish_reason=length (n_predict).
	if !hasFinishReason(`{"choices":[{"delta":{"content":"x"},"finish_reason":"length"}]}`) {
		t.Fatal("finish_reason=length не распознан как терминальный — n_predict обрыв станет ложным truncated")
	}
	if !hasFinishReason(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`) {
		t.Fatal("finish_reason=stop не распознан как терминальный")
	}
}

// TestR259_OwnCancelsAreNotUpstream — (в): наши отмены и уход клиента не
// приписываются upstream.
func TestR259_OwnCancelsAreNotUpstream(t *testing.T) {
	if got := streamAbortReasonR259(nil, context.Canceled); got != streamAbortClientCancel {
		t.Fatalf("reason = %q, want %q", got, streamAbortClientCancel)
	}
	if got := streamAbortReasonR259(nil, context.DeadlineExceeded); got != streamAbortBalancerCancel {
		t.Fatalf("reason = %q, want %q", got, streamAbortBalancerCancel)
	}
	for _, reason := range []string{streamAbortClientCancel, streamAbortBalancerCancel} {
		if streamAbortBlamesUpstream(reason) {
			t.Fatalf("reason %q не должен обвинять upstream", reason)
		}
		if msg := streamAbortClientMessage(reason); strings.Contains(msg, "upstream") {
			t.Fatalf("сообщение для %q обвиняет upstream: %q", reason, msg)
		}
	}
	// Даже при ошибке сканера клиентская отмена приоритетнее: «клиент ушёл» — не
	// повод писать ему ошибку об upstream.
	if got := streamAbortReasonR259(errors.New("read tcp: connection reset"), context.Canceled); got != streamAbortClientCancel {
		t.Fatalf("reason = %q, want client_cancel", got)
	}
}

// TestR259_RealEOFIsUpstream — (г): настоящий обрыв без маркеров остаётся
// upstream_eof/upstream_error и сообщает клиенту причину.
func TestR259_RealEOFIsUpstream(t *testing.T) {
	if got := streamAbortReasonR259(nil, nil); got != streamAbortUpstreamEOF {
		t.Fatalf("reason = %q, want %q", got, streamAbortUpstreamEOF)
	}
	if got := streamAbortReasonR259(errors.New("unexpected EOF"), nil); got != streamAbortUpstreamError {
		t.Fatalf("reason = %q, want %q", got, streamAbortUpstreamError)
	}
	if !streamAbortBlamesUpstream(streamAbortUpstreamEOF) {
		t.Fatal("реальный EOF должен остаться виной upstream")
	}
	if msg := streamAbortClientMessage(streamAbortUpstreamEOF); !strings.Contains(msg, "upstream") {
		t.Fatalf("для реального обрыва причина должна называть upstream: %q", msg)
	}
}
