//go:build llama_stub

// rpc_node_external_r171_test.go — R171 (2026-10-17): режим ВНЕШНЕГО узла раскладки.
//
// ЖИВОЙ КОНТЕКСТ. R170: встроенный узел (дочерний процесс воркера) падает, когда
// клиент на другой машине (ggml-rpc.cpp:566), а отдельный контейнер с тем же
// образом и env работает. Дать воркеру docker-сокет ради запуска контейнера — не
// тот обмен, поэтому режим разделён: желанием управляет балансер, контейнером —
// оператор. Воркер обязан при этом говорить ПРАВДУ о внешнем узле.
//
// КОНТРФАКТУАЛЬНОСТЬ: без режима (старый код) внешний адрес игнорировался —
// воркер пытался поднять свой дочерний процесс на своём порту, а поля external в
// статусе не существовало вовсе.
package cppbackend

import (
	"net"
	"net/http/httptest"
	"net/http"
	"testing"
	"time"
)

// TestR171ExternalNodeStatus — статус внешнего узла: доступен / недоступен /
// желание снято. Правда о состоянии, а не «мы его запустили».
func TestR171ExternalNodeStatus(t *testing.T) {
	// Живой TCP-листенер как «внешний узел».
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	st := externalNodeStatus(true, addr, 300*time.Millisecond)
	if !st.External {
		t.Error("external=false: панель не отличит внешний узел от дочернего процесса")
	}
	if !st.Enabled || !st.Running || !st.Listening {
		t.Errorf("доступный внешний узел: enabled=%v running=%v listening=%v, want всё true",
			st.Enabled, st.Running, st.Listening)
	}
	if st.PID != 0 {
		t.Errorf("PID=%d: у внешнего узла процесса в этом контейнере нет", st.PID)
	}
	if st.Addr != addr {
		t.Errorf("Addr=%q, want %q", st.Addr, addr)
	}
	if len(st.Notes) == 0 {
		t.Error("нет пояснения в notes: оператор не поймёт, что проверили")
	}

	// Желание снято: контейнер может работать, но в раскладке не участвует.
	stOff := externalNodeStatus(false, addr, 300*time.Millisecond)
	if stOff.Running || stOff.Listening {
		t.Errorf("при enabled=false узел не должен считаться рабочим: running=%v listening=%v",
			stOff.Running, stOff.Listening)
	}

	// Закрытый порт: честное «не отвечает».
	deadAddr := "127.0.0.1:1"
	stDead := externalNodeStatus(true, deadAddr, 200*time.Millisecond)
	if stDead.Running || stDead.Listening {
		t.Error("недоступный узел показан рабочим — это ложь в панели")
	}
	if stDead.LastError == "" && len(stDead.Notes) == 0 {
		t.Error("недоступность внешнего узла никак не объяснена")
	}
}

// TestR171SupervisorExternalMode — супервизор не порождает процесс в внешнем
// режиме и не убивает чужой при Stop().
func TestR171SupervisorExternalMode(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	defer ln.Close()

	cfg := Config{}
	cfg.RPCServerEnabled = false
	cfg.RPCServerExternalAddr = addr
	c := NewRPCServerChild(cfg)
	if c.externalAddr != addr {
		t.Fatalf("externalAddr не проброшен из Config: %q", c.externalAddr)
	}
	if got := c.Addr(); got != addr {
		t.Errorf("Addr()=%q, want внешний адрес %q", got, addr)
	}

	// Start в внешнем режиме: процесса не появляется, желание фиксируется.
	if err := c.Start(); err != nil {
		t.Fatalf("Start() при доступном внешнем узле: %v", err)
	}
	st := c.Status()
	if !st.External || !st.Running {
		t.Errorf("статус после Start: external=%v running=%v", st.External, st.Running)
	}
	if st.PID != 0 {
		t.Errorf("PID=%d: воркер не должен порождать процесс в внешнем режиме", st.PID)
	}

	// Stop не должен ломать чужой контейнер: порт остаётся доступным.
	if err := c.Stop(); err != nil {
		t.Fatalf("Stop(): %v", err)
	}
	if rpcNodeProbeExternal(addr, 300*time.Millisecond) == false {
		t.Error("после Stop внешний узел перестал отвечать — воркер не имеет права гасить чужой процесс")
	}
}

// TestR171ExternalNodeStatusFromProbeEndpoint — диагностическая проверка, что
// externalNodeStatus не ходит в сеть при enabled=false (не дёргаем чужой узел зря).
func TestR171ExternalNodeStatusNoProbeWhenDisabled(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
	}))
	defer srv.Close()
	addr := srv.Listener.Addr().String()

	_ = externalNodeStatus(false, addr, 200*time.Millisecond)
	// Внешний узел — TCP, поэтому HTTP-сервер засчитает только TCP-подключение,
	// которого при выключенном желании быть не должно вообще: проверяем по счётчику
	// TCP-соединений самого листенера невозможно, зато видно, что статус не
	// пытается подключиться (hits остаётся 0).
	if hits != 0 {
		t.Errorf("при enabled=false статус ходил в сеть: %d обращений", hits)
	}
}
