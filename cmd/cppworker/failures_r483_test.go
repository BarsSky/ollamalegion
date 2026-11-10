// failures_r483_test.go — R483: кольцо сбоев воркера учитывает не только 5xx.
//
// Живой разбор «ответ пришёл одним куском / инструмент не применился / клиент
// ушёл» упирался в то, что в кольце были только 500-е, а события хода жили
// только в тексте лога. Теперь туда попадают hold_released (R482), client_gone,
// unparsed_tool_markup и turn_retry с машинным кодом Kind и числами в Detail.
package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestR483_WorkerRingRecordsNon5xxEvent — событие хода с кодом 200 и Kind.
func TestR483_WorkerRingRecordsNon5xxEvent(t *testing.T) {
	resetWorkerFailuresR477()
	t.Cleanup(resetWorkerFailuresR477)

	req := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	req.Header.Set("X-Request-Id", "req-r483-1")
	recordWorkerEventR483(req, "/api/chat", "hold_released", "hold_timeout held_ms=21000 held_bytes=9000")

	snap := RecentWorkerFailuresR477()
	if len(snap) != 1 {
		t.Fatalf("в кольце %d событий, ожидалось 1", len(snap))
	}
	e := snap[0]
	if e.RequestID != "req-r483-1" {
		t.Fatalf("request_id = %q, ожидался req-r483-1 (событие не связано с ходом)", e.RequestID)
	}
	if e.Kind != "hold_released" {
		t.Fatalf("kind = %q, ожидался hold_released", e.Kind)
	}
	if e.Status != http.StatusOK {
		t.Fatalf("status = %d, ожидался 200 (это не 5xx, но событие хода)", e.Status)
	}
	if !strings.Contains(e.Detail, "held_ms=21000") {
		t.Fatalf("detail потерял числа: %q", e.Detail)
	}
}

// TestR483_WorkerRingKeeps5xxWithoutKind — обычный 5xx по-прежнему пишется и
// Kind/Detail не обязательны (обратная совместимость формата failures.json).
func TestR483_WorkerRingKeeps5xxWithoutKind(t *testing.T) {
	resetWorkerFailuresR477()
	t.Cleanup(resetWorkerFailuresR477)

	recordWorkerFailureR477(workerFailureR477{
		RequestID: "req-500", Method: "POST", Path: "/api/chat", Status: 500,
	})
	snap := RecentWorkerFailuresR477()
	if len(snap) != 1 || snap[0].Status != 500 {
		t.Fatalf("5xx не записан: %+v", snap)
	}
	if snap[0].Kind != "" {
		t.Fatalf("kind = %q, ожидалось пусто у старого формата", snap[0].Kind)
	}
}
