//go:build llama_stub

// link_bench_handler_r129_test.go — R129 (пункт 3): приёмник замера и его проводка
// в ручку rpc-peers.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

// linkSink — приёмник замера, поднятый в тесте.
func linkSink(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		fmt.Fprintf(w, `{"bytes":%d}`, n)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestR129_LinkBench_SinkCountsBytes — приёмник обязан прочитать всё тело и
// вернуть измеренную скорость (это и есть «вторая сторона» замера).
func TestR129_LinkBench_SinkCountsBytes(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cppworker/link-bench",
		strings.NewReader(strings.Repeat("x", 2<<20)))
	w := httptest.NewRecorder()
	handleCppWorkerLinkBench(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Bytes      int64   `json:"bytes"`
		DurationMS int64   `json:"durationMs"`
		MBps       float64 `json:"mbps"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("ответ не JSON: %v", err)
	}
	if resp.Bytes != 2<<20 {
		t.Errorf("bytes = %d, want %d (2 МБ)", resp.Bytes, 2<<20)
	}
	if resp.MBps <= 0 {
		t.Errorf("mbps = %v, want > 0: приёмник обязан измерить, а не отдать ноль", resp.MBps)
	}
}

// TestR129_LinkBench_GetDescribesAndDoesNotMeasure — GET не грузит сеть: он лишь
// объясняет, что это за ручка.
func TestR129_LinkBench_GetDescribesAndDoesNotMeasure(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cppworker/link-bench", nil)
	w := httptest.NewRecorder()
	handleCppWorkerLinkBench(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "maxMb") {
		t.Errorf("в описании нет границы объёма: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "не health-check") {
		t.Errorf("описание не отделяет ручку от health-check: %s", w.Body.String())
	}
}

// TestR129_LinkBench_WrongMethod — только POST (замер) и GET (описание).
func TestR129_LinkBench_WrongMethod(t *testing.T) {
	req := httptest.NewRequest(http.MethodPut, "/api/v1/cppworker/link-bench", nil)
	w := httptest.NewRecorder()
	handleCppWorkerLinkBench(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

// TestR129_RPCPeers_BenchMeasuresThroughputToLiveNode — ?bench=1 добавляет
// измеренную скорость до узла (по HTTP воркера, а не по порту RPC).
func TestR129_RPCPeers_BenchMeasuresThroughputToLiveNode(t *testing.T) {
	sink := linkSink(t)
	u := strings.TrimPrefix(sink.URL, "http://")
	_, port, err := net.SplitHostPort(u)
	if err != nil {
		t.Fatalf("split: %v", err)
	}

	prev := currentConfig
	t.Cleanup(func() { currentConfig = prev })
	// Адрес узла в списке — это адрес rpc-сервера; порт HTTP задаём параметром,
	// как это делает мастер на стенде (по умолчанию 18092).
	currentConfig = &cppbackend.Config{
		RPCEnabled: true,
		RPCServers: []string{u},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cppworker/rpc-peers?bench=1&mb=2&benchPort="+port, nil)
	w := httptest.NewRecorder()
	handleCppWorkerRPCPeers(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		LinkSpeed   []cppbackend.LinkSpeedResult `json:"linkSpeed"`
		LinkSpeedMb int                          `json:"linkSpeedMb"`
		Reachable   int                          `json:"reachable"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("ответ не JSON: %v", err)
	}
	if resp.Reachable != 1 {
		t.Fatalf("reachable = %d, want 1 (приёмник слушает)", resp.Reachable)
	}
	if len(resp.LinkSpeed) != 1 {
		t.Fatalf("linkSpeed = %+v, want одну запись", resp.LinkSpeed)
	}
	if resp.LinkSpeed[0].Error != "" {
		t.Fatalf("замер не удался: %s", resp.LinkSpeed[0].Error)
	}
	if resp.LinkSpeed[0].MBps <= 0 {
		t.Errorf("MBps = %v, want > 0", resp.LinkSpeed[0].MBps)
	}
	if resp.LinkSpeedMb != 2 {
		t.Errorf("linkSpeedMb = %d, want 2 (объём замера из запроса)", resp.LinkSpeedMb)
	}
}

// TestR129_RPCPeers_NoBenchByDefault — без ?bench=1 замер НЕ запускается: ручка
// опрашивается в том числе из UI, и десятки МБ трафика на каждое открытие были бы
// недопустимы.
func TestR129_RPCPeers_NoBenchByDefault(t *testing.T) {
	sink := linkSink(t)
	u := strings.TrimPrefix(sink.URL, "http://")

	prev := currentConfig
	t.Cleanup(func() { currentConfig = prev })
	currentConfig = &cppbackend.Config{RPCEnabled: true, RPCServers: []string{u}}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cppworker/rpc-peers", nil)
	w := httptest.NewRecorder()
	handleCppWorkerRPCPeers(w, req)

	if strings.Contains(w.Body.String(), "\"linkSpeed\":") {
		t.Errorf("замер выполнен без запроса — это лишний трафик: %s", w.Body.String())
	}
}
