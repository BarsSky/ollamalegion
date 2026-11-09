//go:build llama_stub

// stage_r477_test.go — R477 (2026-11-09): стадия хода и кольцо сбоев.
//
// ЗАЧЕМ. Живая жалоба «ответ не дошёл, хотя токены генерировались» требует
// ответа «какой компонент дал сбой». Тест держит три вещи: стадия пишется в
// контекст, строка request trace её печатает, а ответ 5xx попадает в кольцо,
// которое видно в сводке и в архиве.
package balancer

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/logger"
)

// TestR477_StageTrackerStoresStage — стадия ставится и читается.
func TestR477_StageTrackerStoresStage(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	tracker := newStageTrackerR477()
	req = withStageTrackerR477(req, tracker)

	SetStageR477(req.Context(), stagePreflightR477)
	stage, at := CurrentStageR477(req.Context())
	if stage != stagePreflightR477 {
		t.Fatalf("stage = %q, ожидался %q", stage, stagePreflightR477)
	}
	if at.IsZero() {
		t.Fatal("время стадии не выставлено")
	}
}

// TestR477_TraceLineCarriesStage — строка трассировки печатает стадию.
func TestR477_TraceLineCarriesStage(t *testing.T) {
	var buf bytes.Buffer
	restore := logger.CaptureToBuffer(&buf)
	defer restore()

	req := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	req = withStageTrackerR477(req, newStageTrackerR477())
	SetStageR477(req.Context(), stageStreamR477)
	rec := httptest.NewRecorder()

	w, finish := startRequestTraceR476(rec, req, "rid-stage-477")
	w.WriteHeader(http.StatusOK)
	finish()

	out := buf.String()
	if !strings.Contains(out, "request trace") {
		t.Fatalf("нет строки трассировки: %s", out)
	}
	// Лог JSON: поле может быть как "stage":"stream", так и stage=stream
	// (текстовый encoder в тестах).
	if !strings.Contains(out, `"stage":"stream"`) && !strings.Contains(out, "stage=stream") {
		t.Fatalf("в трассировке нет стадии stream: %s", out)
	}
}

// TestR477_ServerErrorRecordedWithStage — 5xx попадает в кольцо со стадией.
func TestR477_ServerErrorRecordedWithStage(t *testing.T) {
	resetFailuresR477()
	defer resetFailuresR477()

	req := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	req = withStageTrackerR477(req, newStageTrackerR477())
	SetStageR477(req.Context(), stageUpstreamR477)
	rec := httptest.NewRecorder()

	w, finish := startRequestTraceR476(rec, req, "rid-fail-477")
	w.WriteHeader(http.StatusBadGateway)
	finish()

	list := RecentFailuresR477()
	if len(list) != 1 {
		t.Fatalf("в кольце %d сбоев, ожидался 1", len(list))
	}
	f := list[0]
	if f.Stage != stageUpstreamR477 {
		t.Fatalf("stage = %q, ожидался %q", f.Stage, stageUpstreamR477)
	}
	if f.Status != http.StatusBadGateway || f.RequestID != "rid-fail-477" {
		t.Fatalf("сбой записан неверно: %+v", f)
	}
}

// TestR477_SuccessNotRecorded — успешные ответы не забивают кольцо.
func TestR477_SuccessNotRecorded(t *testing.T) {
	resetFailuresR477()
	defer resetFailuresR477()

	req := httptest.NewRequest(http.MethodGet, "/api/version", nil)
	req = withStageTrackerR477(req, newStageTrackerR477())
	rec := httptest.NewRecorder()
	w, finish := startRequestTraceR476(rec, req, "rid-ok-477")
	w.WriteHeader(http.StatusOK)
	finish()

	if n := len(RecentFailuresR477()); n != 0 {
		t.Fatalf("успешный ответ попал в кольцо сбоев (%d)", n)
	}
}

// TestR477_FailureRingIsCapped — кольцо не растёт бесконечно.
func TestR477_FailureRingIsCapped(t *testing.T) {
	resetFailuresR477()
	defer resetFailuresR477()

	for i := 0; i < diagFailuresMaxR477+10; i++ {
		recordFailureR477(diagFailureR477{Path: "/api/chat", Stage: stageStreamR477, Status: 500})
	}
	if n := len(RecentFailuresR477()); n != diagFailuresMaxR477 {
		t.Fatalf("в кольце %d записей, ожидался предел %d", n, diagFailuresMaxR477)
	}
}
