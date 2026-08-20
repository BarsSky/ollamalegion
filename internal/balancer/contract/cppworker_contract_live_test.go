//go:build integration
// +build integration

// Round 51.5 (2026-08-20): LIVE integration tests for wire-format contract.
//
// These tests run against a REAL cppworker (live container or test instance)
// and assert that each payload the balancer sends is actually accepted by
// the backend. Unlike the L1 mirror tests (which use local Go structs), these
// validate the ACTUAL bytes cppworker accepts.
//
// Usage:
//   CPPWORKER_URL=http://cppworker:18092 go test -tags=integration ./internal/balancer/contract/...
//
//   Or with docker:
//   docker compose exec ol-bundled-balancer env CPPWORKER_URL=http://cppworker-gpu:18092 \
//     go test -tags=integration -count=1 -v ./internal/balancer/contract/...
//
// Failure here means: cppworker returned 4xx/5xx for a payload the balancer
// sends. This is the "mechanism подтверждения что бэкенд понял" requested by user.

package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// getCppworkerURL — returns CPPWORKER_URL env or skips the test.
func getCppworkerURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("CPPWORKER_URL")
	if url == "" {
		t.Skip("CPPWORKER_URL not set; skipping live integration test")
	}
	return strings.TrimRight(url, "/")
}

// livePost — POST payload к cppworker endpoint. Возвращает status и body.
func livePost(t *testing.T, base, path string, payload interface{}) (int, string) {
	t.Helper()
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest("POST", base+path, body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// authMiddleware защищает /api/models/reload — берём токен из env.
	if tok := os.Getenv("CPPWORKER_TOKEN"); tok != "" {
		req.Header.Set("X-API-Token", tok)
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(respBody)
}

// isWireFormatError — отличает wire-format ошибки (400 с "invalid JSON" /
// "unknown field" / "missing field") от state ошибок (404 model not loaded,
// 409 already loaded). Только wire errors — REGRESSION в нашем коде.
func isWireFormatError(status int, body string) bool {
	if status < 400 {
		return false
	}
	// cppworker uses golang.org errors (json.Decoder, json.Unmarshal) для парсинга.
	// Wire errors обычно имеют status 400 и body вида:
	//   {"error":"invalid JSON: json: unknown field \"xxx\""}
	//   {"error":"empty request body"}
	//   {"error":"request body too large"}
	//   {"error":"name query parameter is required"}
	if status != http.StatusBadRequest {
		return false
	}
	// State errors that look like 400:
	//   "model already loaded" — это state, не wire
	//   "model gemma-4 not found" — state
	// Wire errors всегда содержат "JSON", "field", "required" (для required field)
	wireKeywords := []string{"JSON", "field", "required", "empty request body", "body too large"}
	for _, kw := range wireKeywords {
		if strings.Contains(body, kw) {
			return true
		}
	}
	return false
}

// TestLiveCppworker_AcceptsAllPayloads — для каждого endpoint'а с payload'ом
// отправляет representative payload в реальный cppworker и проверяет
// что backend ПОНЯЛ запрос (wire error = regression; state error = OK).
func TestLiveCppworker_AcceptsAllPayloads(t *testing.T) {
	base := getCppworkerURL(t)
	for _, ep := range AllEndpoints {
		if ep.RequestStruct == nil && ep.PayloadBuilder == nil {
			continue // GET endpoints
		}
		t.Run(ep.Name+"_live", func(t *testing.T) {
			var payload interface{}
			path := ep.Path
			if ep.PayloadBuilder != nil {
				payload = ep.PayloadBuilder()
			}
			// unload requires ?name=... in query, not body
			if ep.Name == "unload" {
				path = ep.Path + "?name=gemma-4-E4B-it-Q4_K_M"
			}
			status, body := livePost(t, base, path, payload)
			if isWireFormatError(status, body) {
				t.Errorf("cppworker at %s REJECTED payload as WIRE ERROR:\n  endpoint: %s %s\n  status:   %d\n  body:     %s\n  payload:  %s",
					base, ep.Method, path, status, truncate(body, 500), summarize(payload))
				return
			}
			// state errors (404, 409, 400 missing-arg) — not a contract violation, log
			if status >= 400 {
				t.Logf("cppworker returned state error (OK for contract test):\n  endpoint: %s %s\n  status:   %d\n  body:     %s",
					ep.Method, path, status, truncate(body, 200))
			}
		})
	}
}

// TestLiveCppworker_RejectsForbiddenFields — отправляем payload с запрещёнными
// полями ("reason", "adaptiveStage" в reload) и проверяем что cppworker
// ОТВЕРГАЕТ их с 400. Если тест ПРОХОДИТ с 200 — значит cppworker стал принимать
// эти поля (regression: был fix R51.4).
func TestLiveCppworker_RejectsForbiddenFields(t *testing.T) {
	base := getCppworkerURL(t)
	tests := []struct {
		name   string
		path   string
		extra  map[string]interface{}
		expect int // expected status (400 для strict)
	}{
		{
			name: "reload_with_reason",
			path: "/api/models/reload",
			extra: map[string]interface{}{
				"name":        "test-model",
				"contextSize": 32768,
				"reason":      "this should be rejected",
			},
			expect: 400,
		},
		{
			name: "reload_with_adaptiveStage",
			path: "/api/models/reload",
			extra: map[string]interface{}{
				"name":          "test-model",
				"contextSize":   32768,
				"adaptiveStage": "exact_fit",
			},
			expect: 400,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, body := livePost(t, base, tt.path, tt.extra)
			if status != tt.expect {
				t.Errorf("cppworker returned status=%d, want %d\nbody: %s", status, tt.expect, truncate(body, 500))
			}
		})
	}
}

// TestLiveCppworker_InfoEndpointAlive — sanity-check: cppworker отвечает на /api/info.
// Без этого все остальные тесты бесполезны (cppworker может быть down).
func TestLiveCppworker_InfoEndpointAlive(t *testing.T) {
	base := getCppworkerURL(t)
	resp, err := http.Get(base + "/api/info")
	if err != nil {
		t.Fatalf("GET /api/info: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("/api/info: status=%d, body=%s", resp.StatusCode, truncate(string(body), 500))
	}
	var info CppworkerInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatalf("decode /api/info: %v", err)
	}
	t.Logf("cppworker version=%q build=%q loaded_models=%d", info.Version, info.Build, len(info.LoadedModels))
}

// summarize — для error message: payload в кратком виде.
func summarize(v interface{}) string {
	raw, _ := json.Marshal(v)
	s := string(raw)
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}

// truncate — обрезает строку до max байт.
func truncate(s string, max int) string {
	if len(s) > max {
		return s[:max] + fmt.Sprintf("... (%d more bytes)", len(s)-max)
	}
	return s
}
