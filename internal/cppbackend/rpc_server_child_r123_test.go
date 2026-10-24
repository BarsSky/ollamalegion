//go:build llama_stub

// rpc_server_child_r123_test.go — R123 (2026-10-16, пункт 2).
//
// Проверяется ДОНОРСКАЯ половина раскладки: cppworker обязан уметь поднять
// llama.cpp rpc-server дочерним процессом (вместо отдельного контейнера
// ol-rpc-server), честно показывать его состояние и не врать «running» о
// завершившемся процессе.
//
// Дочерний процесс — НЕ заглушка: в его роли выступает сам тестовый бинарь
// (приём TestHelperProcess), который слушает порт. Поэтому проверяется и
// реальная доступность порта (Listening), а не только факт exec.
package cppbackend

import (
	"net"
	"os"
	"strconv"
	"testing"
	"time"
)

// TestRPCServerHelperProcess — «rpc-server» для тестов: слушает порт из -p и
// висит, пока его не убьют.
func TestRPCServerHelperProcess(t *testing.T) {
	if os.Getenv("CPPBACKEND_RPC_HELPER") != "1" {
		t.Skip("helper process (запускается супервизором, а не go test)")
	}
	port := 0
	for i, a := range os.Args {
		if a == "-p" && i+1 < len(os.Args) {
			port, _ = strconv.Atoi(os.Args[i+1])
		}
	}
	if port == 0 {
		os.Exit(2)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		os.Exit(3)
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		_ = conn.Close()
	}
}

// freePort — свободный порт (закрываем сразу: супервизор займёт его сам).
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// newHelperChild — супервизор, у которого в роли rpc-server выступает тестовый
// бинарь.
func newHelperChild(t *testing.T, port int) *RPCServerChild {
	t.Helper()
	t.Setenv("CPPBACKEND_RPC_HELPER", "1")
	c := NewRPCServerChild(Config{
		RPCServerEnabled: true,
		RPCServerBind:    "127.0.0.1",
		RPCServerPort:    port,
		RPCServerBin:     os.Args[0],
	})
	c.extraArgs = []string{"-test.run=TestRPCServerHelperProcess", "--"}
	t.Cleanup(func() { _ = c.Stop() })
	return c
}

// TestR123_RPCServerChild_StartListensAndStops — поднятый узел слушает порт, а
// после остановки порт закрывается (иначе «узел есть» было бы иллюзией).
func TestR123_RPCServerChild_StartListensAndStops(t *testing.T) {
	port := freePort(t)
	c := newHelperChild(t, port)

	if err := c.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// ВАЖНО: проверяем СРАЗУ после Start, без опроса. До правки здесь было
	// listening=false с ложной пометкой «порт не отвечает» — оператор включал узел
	// из WebUI и видел отказ при полностью рабочем процессе (живой замер).
	if st := c.Status(); !st.Listening {
		t.Errorf("сразу после Start listening=%v (notes=%v): Start обязан дождаться "+
			"открытия порта, иначе WebUI показывает ложный отказ", st.Listening, st.Notes)
	}
	st := waitStatus(t, c, func(s RPCServerStatus) bool { return s.Listening })
	if !st.Running || st.PID <= 0 {
		t.Errorf("статус поднятого узла: running=%v pid=%d — процесс не запущен", st.Running, st.PID)
	}
	if st.Addr != net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) {
		t.Errorf("Addr = %q, want 127.0.0.1:%d (адрес нужен мастеру для строки узла)", st.Addr, port)
	}

	if err := c.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	st = c.Status()
	if st.Running {
		t.Errorf("после Stop статус всё ещё running=true (pid=%d) — это ложь оператору", st.PID)
	}
	// Порт обязан закрыться: иначе следующий узел на этом порту не поднимется.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 300*time.Millisecond)
		if err != nil {
			return
		}
		_ = conn.Close()
		time.Sleep(100 * time.Millisecond)
	}
	t.Error("после Stop порт всё ещё принимает соединения — процесс не погашен")
}

// TestR123_RPCServerChild_StartIsIdempotent — повторный Start не плодит второй
// процесс на том же порту (второй молча не занял бы порт, и узел выглядел бы
// поднятым, хотя слушает первый).
func TestR123_RPCServerChild_StartIsIdempotent(t *testing.T) {
	port := freePort(t)
	c := newHelperChild(t, port)

	if err := c.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	first := waitStatus(t, c, func(s RPCServerStatus) bool { return s.Running })
	if err := c.Start(); err != nil {
		t.Fatalf("повторный Start: %v", err)
	}
	second := c.Status()
	if second.PID != first.PID {
		t.Errorf("PID сменился %d → %d: повторный Start поднял второй процесс", first.PID, second.PID)
	}
	if second.Restarts != 0 {
		t.Errorf("Restarts = %d, want 0: повторный Start не перезапуск", second.Restarts)
	}
}

// TestR123_RPCServerChild_FailedStartIsHonest — если бинаря нет, оператор должен
// видеть «включено, но не работает + причина», а не тихое «выключено».
func TestR123_RPCServerChild_FailedStartIsHonest(t *testing.T) {
	c := NewRPCServerChild(Config{
		RPCServerEnabled: true,
		RPCServerBind:    "127.0.0.1",
		RPCServerPort:    freePort(t),
		RPCServerBin:     "/nonexistent/rpc-server-r123",
	})
	if err := c.Start(); err == nil {
		t.Fatal("Start с несуществующим бинарём вернул успех")
	}
	st := c.Status()
	if !st.Enabled {
		t.Error("Enabled = false: оператор включал узел, и это желаемое состояние")
	}
	if st.Running || st.Listening {
		t.Errorf("running=%v listening=%v, want false/false", st.Running, st.Listening)
	}
	if st.LastError == "" {
		t.Error("LastError пуст: причина отказа не видна ни оператору, ни в метриках")
	}
}

// TestR123_RPCServerChild_DeadProcessIsNotRunning — процесс убит извне: статус
// обязан перестать показывать running (иначе мастер раскладки получит «узел
// жив» и будет планировать по несуществующей карте).
func TestR123_RPCServerChild_DeadProcessIsNotRunning(t *testing.T) {
	port := freePort(t)
	c := newHelperChild(t, port)
	if err := c.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	st := waitStatus(t, c, func(s RPCServerStatus) bool { return s.Running })

	// Убиваем процесс «извне» — как при падении rpc-server.
	if err := c.cmd.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}

	dead := waitStatus(t, c, func(s RPCServerStatus) bool { return !s.Running })
	if dead.PID != 0 {
		t.Errorf("PID = %d у завершённого процесса, want 0", dead.PID)
	}
	if dead.LastError == "" {
		t.Error("LastError пуст: о падении узла никто не узнает")
	}
	_ = st
}

// TestR123_RPCServerChild_SetEnabled — переключение из WebUI: включение поднимает
// процесс, выключение гасит, состояние остаётся честным.
func TestR123_RPCServerChild_SetEnabled(t *testing.T) {
	port := freePort(t)
	c := newHelperChild(t, port)

	if err := c.SetEnabled(true); err != nil {
		t.Fatalf("SetEnabled(true): %v", err)
	}
	if st := waitStatus(t, c, func(s RPCServerStatus) bool { return s.Listening }); !st.Enabled {
		t.Error("после SetEnabled(true) Enabled = false")
	}
	if err := c.SetEnabled(false); err != nil {
		t.Fatalf("SetEnabled(false): %v", err)
	}
	if st := c.Status(); st.Enabled || st.Running {
		t.Errorf("после SetEnabled(false): enabled=%v running=%v, want false/false", st.Enabled, st.Running)
	}
}

// waitStatus — ждём состояния (опрос ≤5 c): запуск процесса и открытие порта
// асинхронны.
func waitStatus(t *testing.T, c *RPCServerChild, ok func(RPCServerStatus) bool) RPCServerStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last RPCServerStatus
	for time.Now().Before(deadline) {
		last = c.Status()
		if ok(last) {
			return last
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("состояние не наступило за 5 c: %+v", last)
	return last
}
