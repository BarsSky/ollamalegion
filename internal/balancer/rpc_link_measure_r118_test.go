//go:build llama_stub

// rpc_link_measure_r118_test.go — R118.15 (2026-10-14).
//
// ЗАЧЕМ ЭТИ ТЕСТЫ. Число «канал до узла раскладки» раньше приходило только из .env
// оператора (LB_RPC_LINK_MBPS) и молча устаревало. Теперь балансер измеряет канал
// сам: качает известный объём с cppworker'а узла (/api/rpc-link-probe) и считает
// Мбит/с. Тесты фиксируют:
//  1. замер идёт именно на проб-путь с известным объёмом и даёт правдоподобную
//     цифру (проверяем на управляемой задержке: 1 МиБ за ~0.5 с ≈ 16 Мбит/с);
//  2. удачный замер запоминается и отдаётся наружу;
//  3. НЕудачный замер НЕ затирает последнюю достоверную цифру (в панели остаётся
//     прежнее число, причина — в логе);
//  4. устаревший замер наружу не отдаётся (свежесть считается от периода);
//  5. замер не делается для бэкенда, не участвующего в раскладке.
package balancer

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// newLinkProbeStub — заглушка cppworker с обработчиком замера: отдаёт ровно
// столько байт, сколько просят (не больше 64 МиБ), и держит паузу delay, чтобы
// результат замера был предсказуемым.
func newLinkProbeStub(t *testing.T, bytesToServe int, delay time.Duration) (*httptest.Server, *int64) {
	t.Helper()
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/rpc-link-probe" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt64(&hits, 1)
		n := bytesToServe
		if v := r.URL.Query().Get("bytes"); v != "" {
			if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
				n = parsed
			}
		}
		time.Sleep(delay)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(n))
		w.WriteHeader(http.StatusOK)
		buf := make([]byte, 32<<10)
		for n > 0 {
			chunk := len(buf)
			if n < chunk {
				chunk = n
			}
			if _, err := w.Write(buf[:chunk]); err != nil {
				return
			}
			n -= chunk
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// newLinkMeasureProxy — прокси с бэкендом, указывающим на заглушку.
func newLinkMeasureProxy(t *testing.T, srv *httptest.Server, role string) (*Proxy, string) {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse stub url: %v", err)
	}
	port, _ := strconv.Atoi(u.Port())
	const id = "node-1"
	p := &Proxy{
		config:     &types.LoadBalancerConfig{},
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
	}
	p.backends[id] = &BackendState{Backend: &types.Backend{
		ID: id, Host: u.Hostname(), CppWorkerPort: port, Type: types.BackendTypeLlamaCpp,
		Status: types.StatusHealthy, Role: role,
	}}
	return p, id
}

// TestR118_15_MeasureComputesMbps — замер ходит на проб-путь и даёт правдоподобную
// цифру. Заглушка держит 0.5 с и отдаёт РОВНО столько, сколько просит балансер
// (rpcLinkProbeBytes = 8 МиБ), то есть ожидаем ≈ 8 МиБ × 8 / 0.5 с ≈ 134 Мбит/с;
// коридор широкий (60..300), потому что локальный httptest добавляет свой шум.
func TestR118_15_MeasureComputesMbps(t *testing.T) {
	t.Setenv("LB_RPC_LINK_MEASURE_SEC", "900")
	srv, hits := newLinkProbeStub(t, rpcLinkProbeBytes, 500*time.Millisecond)
	p, id := newLinkMeasureProxy(t, srv, types.BackendRoleDonor)

	mbps, err := p.MeasureRPCLinkForBackend(id)
	if err != nil {
		t.Fatalf("замер: %v", err)
	}
	if atomic.LoadInt64(hits) == 0 {
		t.Fatal("заглушка не получила запрос замера")
	}
	if mbps < 60 || mbps > 300 {
		t.Errorf("Мбит/с = %d, ожидали 60..300 (%.1f МиБ за 0.5 с)", mbps, float64(rpcLinkProbeBytes)/(1<<20))
	}

	got, at, ok := p.RPCLinkMeasured(id)
	if !ok {
		t.Fatal("замер не отдаётся наружу (RPCLinkMeasured)")
	}
	if got != mbps {
		t.Errorf("RPCLinkMeasured = %d, want %d", got, mbps)
	}
	if time.Since(at) > time.Minute {
		t.Errorf("время замера подозрительное: %s", at)
	}
}

// TestR118_15_FailedMeasureKeepsPreviousValue — неудачный замер не затирает
// последнюю достоверную цифру, но и не выдаёт её за свежую.
func TestR118_15_FailedMeasureKeepsPreviousValue(t *testing.T) {
	t.Setenv("LB_RPC_LINK_MEASURE_SEC", "900")
	srv, _ := newLinkProbeStub(t, 1<<20, 0)
	p, id := newLinkMeasureProxy(t, srv, types.BackendRoleDonor)

	first, err := p.MeasureRPCLinkForBackend(id)
	if err != nil {
		t.Fatalf("первый замер: %v", err)
	}
	srv.Close() // узел ушёл

	if _, err := p.MeasureRPCLinkForBackend(id); err == nil {
		t.Fatal("замер недоступного узла не вернул ошибку")
	}
	p.mu.RLock()
	rec := p.rpcLinkMeasured[id]
	p.mu.RUnlock()
	if rec.Mbps != first {
		t.Errorf("прежняя цифра потеряна: %d, want %d (в панели должно остаться последнее достоверное число)", rec.Mbps, first)
	}
	if rec.Error == "" {
		t.Error("причина неудачного замера не записана")
	}
}

// TestR118_15_StaleMeasurementNotServed — устаревший замер наружу не отдаётся.
func TestR118_15_StaleMeasurementNotServed(t *testing.T) {
	t.Setenv("LB_RPC_LINK_MEASURE_SEC", "1") // период 1 с, свежесть = 3 периода
	srv, _ := newLinkProbeStub(t, 256<<10, 0)
	p, id := newLinkMeasureProxy(t, srv, types.BackendRoleDonor)

	if _, err := p.MeasureRPCLinkForBackend(id); err != nil {
		t.Fatalf("замер: %v", err)
	}
	if _, _, ok := p.RPCLinkMeasured(id); !ok {
		t.Fatal("свежий замер должен отдаваться")
	}
	// Старим запись.
	p.mu.Lock()
	rec := p.rpcLinkMeasured[id]
	rec.At = time.Now().Add(-time.Hour)
	p.rpcLinkMeasured[id] = rec
	p.mu.Unlock()
	if _, _, ok := p.RPCLinkMeasured(id); ok {
		t.Error("устаревший замер не должен отдаваться как актуальный")
	}
}

// TestR118_15_MeasureOnlyForLayoutRoles — бэкенд без роли раскладки не меряем.
func TestR118_15_MeasureOnlyForLayoutRoles(t *testing.T) {
	srv, hits := newLinkProbeStub(t, 64<<10, 0)
	p, id := newLinkMeasureProxy(t, srv, "") // роль не задана

	if _, err := p.MeasureRPCLinkForBackend(id); err == nil {
		t.Fatal("замер для бэкенда без роли раскладки не должен выполняться")
	}
	if atomic.LoadInt64(hits) != 0 {
		t.Errorf("заглушку дёрнули %d раз, хотя роль не задана", atomic.LoadInt64(hits))
	}
}

// TestR118_15_IntervalFromEnv — период замеров читается из окружения, 0 выключает.
func TestR118_15_IntervalFromEnv(t *testing.T) {
	t.Setenv("LB_RPC_LINK_MEASURE_SEC", "120")
	if got := RPCLinkMeasureInterval(); got != 120*time.Second {
		t.Errorf("интервал = %s, want 120s", got)
	}
	t.Setenv("LB_RPC_LINK_MEASURE_SEC", "0")
	if got := RPCLinkMeasureInterval(); got != 0 {
		t.Errorf("при 0 замеры должны быть выключены, получено %s", got)
	}
	t.Setenv("LB_RPC_LINK_MEASURE_SEC", "")
	if got := RPCLinkMeasureInterval(); got != rpcLinkMeasureDefaultSec*time.Second {
		t.Errorf("пустое значение → дефолт %ds, получено %s", rpcLinkMeasureDefaultSec, got)
	}
	t.Setenv("LB_RPC_LINK_MEASURE_SEC", "abc")
	if got := RPCLinkMeasureInterval(); got != rpcLinkMeasureDefaultSec*time.Second {
		t.Errorf("мусор → дефолт, получено %s", got)
	}
}

// TestR118_15_UnreachableNodeErrorIsExplicit — недоступный узел: понятная ошибка
// (в ней видно адрес), а не «0 Мбит/с».
func TestR118_15_UnreachableNodeErrorIsExplicit(t *testing.T) {
	srv, _ := newLinkProbeStub(t, 64<<10, 0)
	p, id := newLinkMeasureProxy(t, srv, types.BackendRoleDonor)
	srv.Close()

	_, err := p.MeasureRPCLinkForBackend(id)
	if err == nil {
		t.Fatal("ожидалась ошибка для недоступного узла")
	}
	low := strings.ToLower(err.Error())
	if !strings.Contains(low, "refused") && !strings.Contains(low, "connect") &&
		!strings.Contains(low, "eof") && !strings.Contains(low, "no such host") {
		t.Errorf("ошибка не объясняет недоступность: %v", err)
	}
}
