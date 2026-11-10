// handlers_logs_export_fields_r481_test.go — R481: поля события отдельно от
// сообщения и в UI, и в экспорте.
//
// Живая жалоба: в «Системных логах» запись выглядела как «текст + нечитаемый
// JSON», поля нельзя было ни прочитать, ни скопировать по отдельности.
// Теперь LogEntry.Fields — структура: JSON-экспорт отдаёт объект, TXT —
// детерминированные пары key=value после сообщения, Message остаётся чистым.
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

func withFieldsBrokerR481(t *testing.T) {
	t.Helper()
	base := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	withTestLogBrokerR389(t, []logger.LogEntry{{
		Time: base, Level: "warn", Message: "узел раскладки молчит", Source: "layout",
		Fields: map[string]interface{}{"backend": "donor-1", "stalled_sec": int64(12)},
	}})
}

// TestR481_TextExportKeepsMessageClean — Message без JSON, поля парами после него.
func TestR481_TextExportKeepsMessageClean(t *testing.T) {
	withFieldsBrokerR481(t)
	srv := &Server{}
	rec := httptest.NewRecorder()
	srv.handleLogsExport(rec, httptest.NewRequest(http.MethodGet, "/api/v1/logs/export?format=txt", nil))

	body := rec.Body.String()
	line := strings.TrimSpace(body)
	if strings.Contains(line, `{"backend"`) {
		t.Fatalf("в TXT остался JSON-суффикс в сообщении: %s", line)
	}
	if !strings.Contains(line, "узел раскладки молчит") {
		t.Fatalf("сообщение потерялось: %s", line)
	}
	// Порядок пар детерминирован (sort) — это важно для diff/awk.
	if !strings.Contains(line, "backend=donor-1 stalled_sec=12") {
		t.Fatalf("поля не выгружены парами key=value: %s", line)
	}
}

// TestR481_JSONExportHasFieldsObject — в JSON поля — объект, а не текст.
func TestR481_JSONExportHasFieldsObject(t *testing.T) {
	withFieldsBrokerR481(t)
	srv := &Server{}
	rec := httptest.NewRecorder()
	srv.handleLogsExport(rec, httptest.NewRequest(http.MethodGet, "/api/v1/logs/export?format=json", nil))

	var entries []struct {
		Message string                 `json:"message"`
		Fields  map[string]interface{} `json:"fields"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &entries); err != nil {
		t.Fatalf("JSON-экспорт не разбирается: %v (тело: %s)", err, rec.Body.String())
	}
	if len(entries) != 1 {
		t.Fatalf("записей %d, ожидалась 1", len(entries))
	}
	e := entries[0]
	if e.Message != "узел раскладки молчит" {
		t.Fatalf("message = %q, ожидался чистый текст", e.Message)
	}
	if e.Fields["backend"] != "donor-1" {
		t.Fatalf("fields.backend = %v, ожидался donor-1", e.Fields["backend"])
	}
}
