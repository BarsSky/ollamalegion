// cancel_result_verdict_r361_test.go — R361 (2026-10-31).
//
// ЖИВОЙ ФАКТ. После R360 балансер отправил адресную отмену с request_id, воркер ответил
// 404 (генерации с таким id уже нет: он сам увидел закрытие соединения и снял запись), а
// журнал балансера написал:
//
//	info "cancel upstream: генерация отменена на воркере" ... status=404
//
// То есть сообщение утверждало успех там, где отменять было нечего. Такой лог хуже
// отсутствующего: при разборе «почему Stop не работает» он уводит в противоположную
// сторону.
//
// R361 различает три исхода: cancelled (воркер подтвердил), already_done (отменять было
// нечего — не ошибка), failed (приказ не дошёл/отвергнут — вот здесь Warn).
package balancer

import "testing"

// TestR361_VerdictCancelled — воркер подтвердил отмену.
func TestR361_VerdictCancelled(t *testing.T) {
	cases := []map[string]interface{}{
		{"cancelled": float64(1), "status": "cancelled"},
		{"cancelled": 1},   // int из другого места
		{"cancelled": "2"}, // строка (старые сборки)
	}
	for _, body := range cases {
		if got := cancelVerdictFromResponseR361(200, body); got != cancelVerdictCancelled {
			t.Errorf("body=%v → %q, want cancelled", body, got)
		}
	}
}

// TestR361_VerdictAlreadyDone — отменять было нечего: НЕ ошибка.
func TestR361_VerdictAlreadyDone(t *testing.T) {
	if got := cancelVerdictFromResponseR361(404, nil); got != cancelVerdictAlreadyDone {
		t.Errorf("404 → %q, want already_done (воркер уже снял генерацию с учёта)", got)
	}
	if got := cancelVerdictFromResponseR361(200, map[string]interface{}{"cancelled": float64(0), "status": "no_active_generation"}); got != cancelVerdictAlreadyDone {
		t.Errorf("no_active_generation → %q, want already_done", got)
	}
	if got := cancelVerdictFromResponseR361(200, nil); got != cancelVerdictAlreadyDone {
		t.Errorf("200 без тела → %q, want already_done (успех не подтверждён)", got)
	}
}

// TestR361_VerdictFailed — приказ не дошёл или отвергнут: здесь нужен Warn.
func TestR361_VerdictFailed(t *testing.T) {
	for _, status := range []int{401, 403, 500, 502, 503, 400, 429} {
		if got := cancelVerdictFromResponseR361(status, nil); got != cancelVerdictFailed {
			t.Errorf("status %d → %q, want failed (иначе поломка отмены останется невидимой)",
				status, got)
		}
	}
}

// TestR361_IntParsing — числа из JSON приходят float64, но бывают int/строкой.
func TestR361_IntParsing(t *testing.T) {
	if n, ok := intFromAnyR361(float64(3)); !ok || n != 3 {
		t.Errorf("float64(3) → %d/%v", n, ok)
	}
	if n, ok := intFromAnyR361(4); !ok || n != 4 {
		t.Errorf("int(4) → %d/%v", n, ok)
	}
	if n, ok := intFromAnyR361("5"); !ok || n != 5 {
		t.Errorf("\"5\" → %d/%v", n, ok)
	}
	if _, ok := intFromAnyR361("не число"); ok {
		t.Error("нечисловая строка принята за число")
	}
	if _, ok := intFromAnyR361(nil); ok {
		t.Error("nil принят за число")
	}
}
