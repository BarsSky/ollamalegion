// cancel_status_r343_test.go — R343 (2026-10-30), этап 4.
//
// ЖИВОЙ ДЕФЕКТ (аудит docs/plans/2026-10-30-abort-and-metrics-audit.md, A1):
// верхний `status` ответа /api/cancel считался по HTTP-коду («любой 2xx — успех»),
// а воркер на пустом трекере отвечает 200 + `cancelled:0`. В результате клиент
// видел "cancelled" и тогда, когда генерация продолжалась. Плюс имя модели
// сверяется на двух концах по-разному (балансер мягко, воркер точно), поэтому
// «приказ доставлен» не означало «отменено».
//
// Тесты фиксируют честный статус по ФАКТИЧЕСКОМУ числу отменённых генераций.
package balancer

import (
	"strings"
	"testing"
)

// TestR343_NoCancelledIsNotSuccess — главный случай: 200 + cancelled:0 = не отменено.
func TestR343_NoCancelledIsNotSuccess(t *testing.T) {
	results := []map[string]interface{}{
		{"backend": "b1", "ok": true, "cancelled": float64(0), "status": "ok"},
	}
	status, hint := cancelStatusR343(results, []string{"b1"}, 1)
	if status == "cancelled" {
		t.Fatalf("статус %q при cancelled:0 — это и есть живой дефект: клиент считает, что генерация остановлена", status)
	}
	if status != "not_cancelled" {
		t.Errorf("status = %q, want not_cancelled", status)
	}
	if hint == "" {
		t.Error("нет подсказки: оператор должен понимать, почему не отменилось (нет генераций / имя не совпало)")
	}
	if !strings.Contains(hint, "имя модели") {
		t.Errorf("подсказка не называет возможное расхождение имени: %q", hint)
	}
}

// TestR343_RealCancelIsSuccess — хотя бы одна отменённая генерация = успех.
func TestR343_RealCancelIsSuccess(t *testing.T) {
	results := []map[string]interface{}{
		{"backend": "b1", "ok": true, "cancelled": float64(2)},
	}
	status, hint := cancelStatusR343(results, []string{"b1"}, 1)
	if status != "cancelled" {
		t.Errorf("status = %q, want cancelled (воркер отменил 2 генерации)", status)
	}
	if hint != "" {
		t.Errorf("для успешной отмены подсказка не нужна: %q", hint)
	}
}

// TestR343_IntAndFloatCounts — счётчик приходит и числом JSON, и int.
func TestR343_IntAndFloatCounts(t *testing.T) {
	for _, v := range []interface{}{1, float64(1)} {
		results := []map[string]interface{}{{"ok": true, "cancelled": v}}
		if status, _ := cancelStatusR343(results, []string{"b1"}, 1); status != "cancelled" {
			t.Errorf("cancelled=%T(%v): status = %q, want cancelled", v, v, status)
		}
	}
}

// TestR343_NoTargetsAndUnreachable — честные статусы для «некому» и «не доехало».
func TestR343_NoTargetsAndUnreachable(t *testing.T) {
	if status, hint := cancelStatusR343(nil, nil, 0); status != "no_active_generation" || hint == "" {
		t.Errorf("без целей: status=%q hint=%q, want no_active_generation с подсказкой (значение сохранено ради клиентов)", status, hint)
	}
	results := []map[string]interface{}{{"backend": "b1", "ok": false}}
	if status, hint := cancelStatusR343(results, nil, 1); status != "unreachable" || hint == "" {
		t.Errorf("цели есть, приказ не принят: status=%q hint=%q, want unreachable с подсказкой", status, hint)
	}
}

// TestR343_PartialAcceptNoCancel — часть воркеров приняла, но отмены нет.
func TestR343_PartialAcceptNoCancel(t *testing.T) {
	results := []map[string]interface{}{
		{"backend": "b1", "ok": true, "cancelled": float64(0)},
		{"backend": "b2", "ok": false},
	}
	status, _ := cancelStatusR343(results, []string{"b1"}, 2)
	if status != "not_cancelled" {
		t.Errorf("status = %q, want not_cancelled (принял один, отменено 0)", status)
	}
}
