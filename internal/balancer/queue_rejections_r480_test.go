// queue_rejections_r480_test.go — R480 (P1-8): причина «в очереди» называет
// гейт окна, а не только model_not_loaded/over_load_threshold.
package balancer

import (
	"strings"
	"testing"
)

// TestR480_RejectionsIncludeWindowTooSmall — b2 с окном 32768 при need 65536
// обязан попасть в почему-в-очереди как window_too_small с числами.
func TestR480_RejectionsIncludeWindowTooSmall(t *testing.T) {
	t.Setenv("LB_R459_SESSION_TTL_SEC", "900")
	p := r480SelectorProxy(t)

	list := p.backendRejectionsR460(r459Model, "", &backendRejectionMoveR480{Need: 65536, NeedSource: "session"})
	var b2 *backendRejectionR460
	for i := range list {
		if list[i].BackendID == "b2" {
			b2 = &list[i]
		}
	}
	if b2 == nil {
		t.Fatalf("b2 не попал в причины отказа: %+v", list)
	}
	if b2.Reason != rejectWindowTooSmallR460 {
		t.Fatalf("b2.reason = %q, ожидался %q (detail=%q)", b2.Reason, rejectWindowTooSmallR460, b2.Detail)
	}
	if !strings.Contains(b2.Detail, "32768") || !strings.Contains(b2.Detail, "65536") {
		t.Fatalf("в detail нет чисел окон: %q", b2.Detail)
	}
	if !strings.Contains(rejectionsSummaryR460(list), "window_too_small") {
		t.Fatalf("why_queued не содержит window_too_small: %s", rejectionsSummaryR460(list))
	}

	// Без контекста переезда причина прежняя — путь новых сессий не изменился.
	for _, r := range p.backendRejectionsR460(r459Model, "") {
		if r.Reason == rejectWindowTooSmallR460 {
			t.Fatalf("без контекста переезда появилась причина window_too_small: %+v", r)
		}
	}
}
