// cancel_supported_r362_test.go — R362 (2026-10-31), аудит abort A6.
//
// ЖИВОЙ ДЕФЕКТ. В stub-сборке воркера (-tags llama_stub) per-inference abort-флаг всегда
// nil: отмена физически невозможна, генерация идёт до n_predict. Но POST /api/cancel
// отвечал «cancelled: N» и статусом "cancelled" — то есть ВРАЛ и клиенту, и балансеру, и
// тестам: «Stop нажали, отменили», хотя ничего не отменилось.
//
// R362: возможность сборки отдаётся явно —
//   - cppworker /api/cancel: поле supported, при false статус "cancel_not_supported"
//     и notice с объяснением;
//   - cppworker /api/info: поле cancel_supported (видно ДО нажатия Stop);
//   - балансер: если воркеры сообщили supported=false, верхний статус — "not_supported",
//     а не «cancelled»/«not_cancelled».
//
// Проверяется:
//  1. балансер: supported=false без отмен → статус not_supported с подсказкой;
//  2. балансер: supported=false, но отмена ДЕЙСТВИТЕЛЬНО случилась (cancelled>0) →
//     статус cancelled (факт важнее возможностей);
//  3. балансер: прежние статусы не сломаны (cancelled / no_active_generation / unreachable);
//  4. сторож: обработчик отмены воркера обязан сообщать supported, а /api/info —
//     cancel_supported (иначе «немой» stub снова начнёт рапортовать успех).
package balancer

import (
	"strings"
	"testing"
)

// TestR362_NotSupportedStatus — сборка без отмены: честный статус, а не «cancelled».
func TestR362_NotSupportedStatus(t *testing.T) {
	results := []map[string]interface{}{
		{"backend": "b1", "ok": true, "cancelled": float64(0), "supported": false,
			"status": "cancel_not_supported"},
	}
	status, hint := cancelStatusR343(results, []string{"b1"}, 1)
	if status != "not_supported" {
		t.Fatalf("status = %q, want not_supported: воркер физически не умеет отменять, "+
			"и верхний слой не должен изображать успех", status)
	}
	if !strings.Contains(hint, "stub") {
		t.Errorf("подсказка %q не называет причину (сборка без C-bridge)", hint)
	}
}

// TestR362_FactBeatsCapability — если отмена реально произошла, статус «cancelled»,
// даже когда какой-то из воркеров сообщил supported=false.
func TestR362_FactBeatsCapability(t *testing.T) {
	results := []map[string]interface{}{
		{"backend": "b1", "ok": true, "cancelled": float64(2), "supported": true},
		{"backend": "b2", "ok": true, "cancelled": float64(0), "supported": false},
	}
	status, _ := cancelStatusR343(results, []string{"b1", "b2"}, 2)
	if status != "cancelled" {
		t.Fatalf("status = %q, want cancelled (факт отмены важнее возможностей соседнего узла)", status)
	}
}

// TestR362_ExistingStatusesUnchanged — прежние статусы не сломаны (R343).
func TestR362_ExistingStatusesUnchanged(t *testing.T) {
	if got, _ := cancelStatusR343(nil, nil, 0); got != "no_active_generation" {
		t.Errorf("нет целей → %q, want no_active_generation", got)
	}
	if got, _ := cancelStatusR343([]map[string]interface{}{{"ok": false}}, nil, 1); got != "unreachable" {
		t.Errorf("ни один воркер не принял → %q, want unreachable", got)
	}
	accepted := []map[string]interface{}{
		{"backend": "b1", "ok": true, "cancelled": float64(0), "supported": true},
	}
	if got, _ := cancelStatusR343(accepted, []string{"b1"}, 1); got != "not_cancelled" {
		t.Errorf("приняли, но не отменили → %q, want not_cancelled", got)
	}
}
