//go:build llama_stub

// handlers_link_probe_test.go — R118.15 (2026-10-14).
//
// Зонд канала — часть контракта узла: по нему балансер мерит пропускную
// способность до узла раскладки. Тесты фиксируют:
//  1. объём ответа равен запрошенному (иначе замер «врёт»);
//  2. есть Content-Length и application/octet-stream (клиент мерит передачу,
//     а не ожидание);
//  3. границы: меньше минимума поднимается до минимума, больше максимума
//     обрезается до максимума (замер не превращается в нагрузку);
//  4. нечисловой параметр — 400, а не молчаливый дефолт;
//  5. поток НЕ нулевой (сжимаемый нулевой поток промежуточное оборудование
//     могло бы сжать и завысить замер);
//  6. метод кроме GET/HEAD — 405.
package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestR118_15_LinkProbeReturnsRequestedBytes(t *testing.T) {
	const bytesWanted = 256 << 10
	req := httptest.NewRequest(http.MethodGet, "/api/rpc-link-probe?bytes=262144", nil)
	rec := httptest.NewRecorder()
	handleLinkProbe(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", ct)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "262144" {
		t.Errorf("Content-Length = %q, want 262144", cl)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) != bytesWanted {
		t.Errorf("получено %d байт, want %d", len(body), bytesWanted)
	}
	allZero := true
	for _, b := range body {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Error("поток нулевой — сжимаемый ответ завысил бы замер канала")
	}
}

func TestR118_15_LinkProbeBounds(t *testing.T) {
	// Меньше минимума — поднимаем до минимума (64 КиБ).
	req := httptest.NewRequest(http.MethodGet, "/api/rpc-link-probe?bytes=100", nil)
	rec := httptest.NewRecorder()
	handleLinkProbe(rec, req)
	if got := rec.Result().Header.Get("Content-Length"); got != "65536" {
		t.Errorf("малый объём: Content-Length = %q, want 65536", got)
	}

	// Больше максимума — обрезаем до 64 МиБ, тело не читаем (только заголовки).
	req = httptest.NewRequest(http.MethodGet, "/api/rpc-link-probe?bytes=999999999", nil)
	rec = httptest.NewRecorder()
	handleLinkProbe(rec, req)
	if got := rec.Result().Header.Get("Content-Length"); got != "67108864" {
		t.Errorf("большой объём: Content-Length = %q, want 67108864", got)
	}
}

func TestR118_15_LinkProbeErrorsAndMethods(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/rpc-link-probe?bytes=abc", nil)
	rec := httptest.NewRecorder()
	handleLinkProbe(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("нечисловой bytes: HTTP %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "error") {
		t.Errorf("нет JSON-ошибки: %s", rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/rpc-link-probe", nil)
	rec = httptest.NewRecorder()
	handleLinkProbe(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: HTTP %d, want 405", rec.Code)
	}
}

func TestR118_15_LinkProbeHeadHasNoBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodHead, "/api/rpc-link-probe?bytes=131072", nil)
	rec := httptest.NewRecorder()
	handleLinkProbe(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD: HTTP %d", rec.Code)
	}
	if got := rec.Body.Len(); got != 0 {
		t.Errorf("HEAD вернул тело %d байт", got)
	}
	if cl := rec.Result().Header.Get("Content-Length"); cl != "131072" {
		t.Errorf("HEAD: Content-Length = %q, want 131072", cl)
	}
}
