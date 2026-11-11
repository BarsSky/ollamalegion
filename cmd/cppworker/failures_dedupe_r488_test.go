// failures_dedupe_r488_test.go — R488 (2026-11-11): повторы одного события не
// должны вытеснять из кольца воркера редкие события отмены.
//
// ЖИВОЙ ФАКТ. Архив второго стенда: все 50 записей failures.json были
// `hold_released` (сторож снимает вывод каждые ~20 с), поэтому `client_cancelled`
// в архив не попадал вообще — ответ на «кто отменил запрос» приходил пустым.
package main

import (
	"testing"
	"time"
)

func TestR488_HoldReleasedCollapsesIntoCount(t *testing.T) {
	resetWorkerFailuresR477()

	for i := 0; i < 37; i++ {
		// Заявка одна и та же — ровно то, что делает сторож удержания: снимает
		// вывод каждые ~20 с внутри ОДНОГО запроса.
		recordWorkerFailureR477(workerFailureR477{
			RequestID: "req-held-1", Path: "/api/chat",
			Kind: "hold_released", Detail: "hold_timeout held_ms=20000",
		})
	}
	events := RecentWorkerFailuresR477()
	if len(events) != 1 {
		t.Fatalf("37 однотипных событий дали %d записей, want 1 (свёртка R488)", len(events))
	}
	if events[0].Count != 36 {
		t.Fatalf("count = %d, want 36 (первая запись + 36 повторов)", events[0].Count)
	}
}

func TestR488_DifferentKindIsNotCollapsed(t *testing.T) {
	resetWorkerFailuresR477()
	recordWorkerFailureR477(workerFailureR477{RequestID: "r1", Path: "/api/chat", Kind: "hold_released", Detail: "a"})
	recordWorkerFailureR477(workerFailureR477{RequestID: "r1", Path: "/api/chat", Kind: "client_cancelled", Detail: "b"})
	recordWorkerFailureR477(workerFailureR477{RequestID: "r1", Path: "/api/chat", Kind: "hold_released", Detail: "c"})

	events := RecentWorkerFailuresR477()
	if len(events) != 3 {
		t.Fatalf("разные события схлопнулись: %d записей, want 3", len(events))
	}
	if events[1].Kind != "client_cancelled" {
		t.Fatalf("редкое событие потерялось: %+v", events)
	}
}

func TestR488_DifferentRequestIDIsNotCollapsed(t *testing.T) {
	resetWorkerFailuresR477()
	recordWorkerFailureR477(workerFailureR477{RequestID: "r1", Path: "/api/chat", Kind: "hold_released", Detail: "a"})
	recordWorkerFailureR477(workerFailureR477{RequestID: "r2", Path: "/api/chat", Kind: "hold_released", Detail: "b"})
	if got := len(RecentWorkerFailuresR477()); got != 2 {
		t.Fatalf("события разных заявок схлопнулись: %d записей, want 2", got)
	}
}

func TestR488_EmptyKindIsAlwaysAppended(t *testing.T) {
	resetWorkerFailuresR477()
	now := time.Now()
	recordWorkerFailureR477(workerFailureR477{RequestID: "r1", Path: "/api/chat", Status: 500, Time: now})
	recordWorkerFailureR477(workerFailureR477{RequestID: "r1", Path: "/api/chat", Status: 500, Time: now})
	if got := len(RecentWorkerFailuresR477()); got != 2 {
		t.Fatalf("5xx без kind схлопнулись: %d записей, want 2 (их счётчик отдельный)", got)
	}
}
