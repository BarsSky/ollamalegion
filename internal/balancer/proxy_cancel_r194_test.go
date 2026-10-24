// proxy_cancel_r194_test.go — R194 (2026-10-20).
//
// Живой дефект: после обрыва клиента следующий запрос получал 503 — отмена
// клиента помечала бэкенд нездоровым. Отмена клиента не является отказом узла.
package balancer

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestR194_ClientCancelIsNotBackendFailure(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{context.Canceled, true},
		{fmt.Errorf("wrap: %w", context.Canceled), true},
		{errors.New("llama.cpp request failed [attempt=1, error_type=context_canceled]: context canceled"), true},
		{errors.New("write tcp: broken pipe"), true},
		{context.DeadlineExceeded, false},
		{errors.New("dial tcp 10.0.0.5:18092: connect: connection refused"), false},
		{nil, false},
	}
	for _, c := range cases {
		if got := isClientDisconnectErr(c.err); got != c.want {
			t.Errorf("err=%v got=%v want=%v", c.err, got, c.want)
		}
	}
}
