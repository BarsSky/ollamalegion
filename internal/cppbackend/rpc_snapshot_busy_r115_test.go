//go:build llama_stub

// rpc_snapshot_busy_r115_test.go — R115: во время загрузки/инференса фоновая
// проверка НЕ дозванивается до узлов раскладки.
//
// Почему это отдельное правило, а не оптимизация. Узел (`rpc-server`) обслуживает
// соединения по одному, а запрос памяти устройства — такая же RPC-команда, как
// передача тензора. Если фон открывает второе соединение, пока идёт загрузка,
// ответ приходит неразобранным, и llama.cpp отвечает `RPC_STATUS_ASSERT` ->
// `GGML_ABORT` (ggml-rpc.cpp:30) — падает весь процесс.
//
// Живое падение (обе машины на ОДНОМ образе и коммите):
//
//	load_tensors: RPC0[192.0.2.10:50052] model buffer size = 2246.03 MiB
//	ggml-rpc.cpp:823: Remote RPC server crashed or returned malformed response
//
// Строка 823 — это `get_device_memory()`, то есть ОПРОС ПАМЯТИ, а не загрузка
// весов: падал именно фоновый сбор бюджета.
package cppbackend

import (
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// countingDialer — слушатель, который считает входящие соединения. Никакого
// протокола не обслуживает: нам важно только то, дозвонились ли до него.
func countingAccepts(t *testing.T) (*net.TCPAddr, *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listener: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var accepted atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			_ = c.Close()
		}
	}()
	return ln.Addr().(*net.TCPAddr), &accepted
}

func TestR115_NoDialWhileModelLoading(t *testing.T) {
	addr, accepted := countingAccepts(t)
	b := &Backend{
		cfg:     Config{RPCEnabled: true, RPCServers: []string{addr.String()}},
		loading: map[string]chan struct{}{"Qwen3.8-27B": make(chan struct{})},
	}

	_ = b.rpcDeviceSnapshot()
	time.Sleep(150 * time.Millisecond)

	if n := accepted.Load(); n != 0 {
		t.Errorf("R115: во время загрузки модели фон дозвонился до узла %d раз(а) — "+
			"это и есть причина падения с ggml-rpc.cpp:823", n)
	}
}

func TestR115_NoDialWhileInference(t *testing.T) {
	addr, accepted := countingAccepts(t)
	b := &Backend{
		cfg: Config{RPCEnabled: true, RPCServers: []string{addr.String()}},
	}
	// Активный запрос к модели: inference-фаза.
	inst := &modelInstance{info: ModelInfo{Name: "Qwen3.8-27B", State: StateLoaded}}
	inst.activeQueries.Store(1)
	b.models = map[string]*modelInstance{"Qwen3.8-27B": inst}

	_ = b.rpcDeviceSnapshot()
	time.Sleep(150 * time.Millisecond)

	if n := accepted.Load(); n != 0 {
		t.Errorf("R115: во время инференса фон дозвонился до узла %d раз(а)", n)
	}
}

func TestR115_LayoutBusyReportsReason(t *testing.T) {
	b := &Backend{loading: map[string]chan struct{}{"m": make(chan struct{})}}
	busy, why := b.layoutBusy()
	if !busy || why != "model_load" {
		t.Errorf("R115: идёт загрузка -> busy=true/model_load, получено %v/%q", busy, why)
	}

	idle := &Backend{}
	if busy, _ := idle.layoutBusy(); busy {
		t.Errorf("R115: пустой бэкенд не занят")
	}
}
