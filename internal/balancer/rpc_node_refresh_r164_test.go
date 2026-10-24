//go:build llama_stub

// rpc_node_refresh_r164_test.go — R164 (2026-10-17): приказ узлу раскладки
// принимается по СВЕЖИМ данным, а не по залипшему кэшу.
//
// ЖИВОЙ СЛУЧАЙ. Воркер слушал порт и отдавал rpc_server.listening=true, а балансер
// видел listening=false, повторял приказ и «ожил» только после перезапуска
// процесса. Точную причину залипания доказать не удалось (редкая гонка при
// пересоздании контейнера), но класс проблемы закрывается детерминированно: если
// узел НЕ подтверждён, балансер обязан опросить бэкенд перед решением.
//
// КОНТРФАКТУАЛЬНОСТЬ: на старом коде (без принудительного опроса) тест падает —
// приказ уходит повторно, хотя воркер уже слушает.
package balancer

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// staleNodeWorker — подставной cppworker: /api/models отдаёт ЖИВОЕ состояние узла,
// PUT config/update записывается (чтобы видеть, повторяли ли приказ).
type staleNodeWorker struct {
	mu      sync.Mutex
	pushes  []map[string]interface{}
	rpcJSON string
	models  string
}

func (s *staleNodeWorker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/models":
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, s.models)
	case "/api/v1/cppworker/config/update":
		raw, _ := io.ReadAll(r.Body)
		var body map[string]interface{}
		_ = json.Unmarshal(raw, &body)
		s.mu.Lock()
		s.pushes = append(s.pushes, body)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	default:
		http.NotFound(w, r)
	}
}

func (s *staleNodeWorker) pushCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pushes)
}

// TestR164StaleSnapshotIsRefreshedBeforeOrder — главный контрпример: кэш говорит
// «не слушает», воркер говорит «слушает» — приказа быть не должно.
func TestR164StaleSnapshotIsRefreshedBeforeOrder(t *testing.T) {
	worker := &staleNodeWorker{models: `{
		"count":0,
		"models":[],
		"rpc_server":{"enabled":true,"running":true,"listening":true,"port":50052,"addr":"0.0.0.0:50052","pid":123}
	}`}
	srv := httptest.NewServer(worker)
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse stub url: %v", err)
	}
	port, _ := strconv.Atoi(u.Port())

	p := &Proxy{
		config:     &types.LoadBalancerConfig{},
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
	}
	p.backends["node-1"] = &BackendState{Backend: &types.Backend{
		ID: "node-1", Host: u.Hostname(), CppWorkerPort: port,
		Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp, Role: types.BackendRoleDonor,
	}}
	// Поллер нужен как ИНСТРУМЕНТ опроса, но НЕ запускаем его цикл: тест
	// проверяет именно принудительный опрос внутри решения.
	p.llamaCppMetricsPoller = newLlamaCppMetricsPoller(p)

	// ЗАЛИПШИЙ кэш: процесс есть, порт якобы не слушается.
	p.metricsMgr.mu.Lock()
	p.metricsMgr.llamaMetrics["node-1"] = &types.LlamaCppMetrics{
		RPCNode: &types.RPCNodeStatus{Enabled: true, Running: true, Listening: false, Port: 50052},
	}
	p.metricsMgr.mu.Unlock()

	// Узел считается доступным с балансера (проверка TCP подменена).
	setReachability(p, p.rpcNodeAddr(p.GetBackend("node-1")))

	changed, reason := p.EnsureRPCNodeServing("node-1")
	if changed {
		t.Fatalf("приказ отправлен повторно, хотя воркер уже слушает (reason=%q): "+
			"балансер поверил кэшу вместо опроса", reason)
	}
	if n := worker.pushCount(); n != 0 {
		t.Errorf("приказов узлу = %d, want 0 — лишняя запись файла и ложное «недоступен»", n)
	}
	rep := p.RPCNodeReport("node-1")
	if !rep.Listening || !rep.Running {
		t.Errorf("отчёт после обновления: running=%v listening=%v, want true/true", rep.Running, rep.Listening)
	}
	if !rep.Reachable {
		t.Error("узел доступен, а отчёт говорит reachable=false")
	}
	if rep.Reason != "" {
		t.Errorf("причина не должна оставаться: %q", rep.Reason)
	}
}

// TestR164NodeStillDownGetsOrder — обратная сторона: если узел НЕ поднят и после
// свежего опроса, приказ обязан уйти.
func TestR164NodeStillDownGetsOrder(t *testing.T) {
	worker := &staleNodeWorker{models: `{
		"count":0,"models":[],
		"rpc_server":{"enabled":false,"running":false,"listening":false,"port":50052}
	}`}
	srv := httptest.NewServer(worker)
	t.Cleanup(srv.Close)

	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	p := &Proxy{
		config:     &types.LoadBalancerConfig{},
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
	}
	p.backends["node-1"] = &BackendState{Backend: &types.Backend{
		ID: "node-1", Host: u.Hostname(), CppWorkerPort: port,
		Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp, Role: types.BackendRoleDonor,
	}}
	p.llamaCppMetricsPoller = newLlamaCppMetricsPoller(p)

	changed, reason := p.EnsureRPCNodeServing("node-1")
	if !changed {
		t.Fatalf("узел не поднят, а приказ не отправлен (reason=%q)", reason)
	}
	if n := worker.pushCount(); n != 1 {
		t.Errorf("приказов узлу = %d, want 1", n)
	}
	_ = time.Now()
}
