// proxy_log_entry_r481_test.go — R481: «Прокси-логи» связываются с остальной
// диагностикой по request_id и показывают стадию сбоя.
//
// Живая жалоба: в таблице прокси-логов был текст ошибки, но не было ни
// request_id (по нему ищут ход в логах балансера и воркера), ни стадии
// (select_backend/admission/preflight/upstream/stream/client_write), поэтому
// «какой компонент дал сбой» снова выяснялось grep'ом по контейнерам.
package balancer

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestR481_ProxyLogEntryCarriesRequestIDAndStage(t *testing.T) {
	entry := ProxyLogEntry{
		Method:    "POST",
		Path:      "/api/chat",
		StatusCode: 503,
		RequestID: "req-abc123",
		Stage:     "upstream",
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, `"requestID":"req-abc123"`) {
		t.Fatalf("в JSON нет requestID (контракт /api/v1/proxy/logs): %s", body)
	}
	if !strings.Contains(body, `"stage":"upstream"`) {
		t.Fatalf("в JSON нет stage (контракт /api/v1/proxy/logs): %s", body)
	}
}

// TestR481_ProxyLogEntryOmitsEmptyTailFields — поля необязательные: пустые не
// должны засорять ответ (omitempty), иначе UI рисует пустые колонки как «-».
func TestR481_ProxyLogEntryOmitsEmptyTailFields(t *testing.T) {
	raw, err := json.Marshal(ProxyLogEntry{Method: "GET", Path: "/api/tags", StatusCode: 200})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)
	if strings.Contains(body, `"requestID"`) || strings.Contains(body, `"stage"`) {
		t.Fatalf("пустые requestID/stage не должны попадать в JSON: %s", body)
	}
}
