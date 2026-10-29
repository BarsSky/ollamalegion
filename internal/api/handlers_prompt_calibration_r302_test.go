// handlers_prompt_calibration_r302_test.go — R302 (2026-10-29): HTTP-вход
// калибровки отвечает по контракту и не отдаёт null вместо массива.
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"
)

// TestR302PromptCalibrationEndpoint — GET отдаёт массив (даже пустой), POST
// сбрасывает калибровку, прочие методы — 405.
func TestR302PromptCalibrationEndpoint(t *testing.T) {
	p := balancer.NewProxy(&types.LoadBalancerConfig{})
	s := &Server{proxy: p}

	rec := httptest.NewRecorder()
	s.HandleStatsPromptCalibration(rec, httptest.NewRequest(http.MethodGet, "/api/v1/stats/prompt-calibration", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Fatalf("GET тело = %q, want [] (null ломает клиентов WebUI)", got)
	}

	rec = httptest.NewRecorder()
	s.HandleStatsPromptCalibration(rec, httptest.NewRequest(http.MethodPost, "/api/v1/stats/prompt-calibration", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("POST тело не JSON: %v", err)
	}
	if body["status"] != "reset" {
		t.Fatalf("POST status = %q, want reset", body["status"])
	}

	rec = httptest.NewRecorder()
	s.HandleStatsPromptCalibration(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/stats/prompt-calibration", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE status = %d, want 405", rec.Code)
	}

	// Прокси не инициализирован — 500, а не паника: endpoint не должен ронять
	// балансер, если поднят раньше прокси.
	bare := &Server{}
	rec = httptest.NewRecorder()
	bare.HandleStatsPromptCalibration(rec, httptest.NewRequest(http.MethodGet, "/api/v1/stats/prompt-calibration", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("GET без proxy status = %d, want 500", rec.Code)
	}
}
