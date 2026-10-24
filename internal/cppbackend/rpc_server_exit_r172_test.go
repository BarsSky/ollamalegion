//go:build llama_stub

// rpc_server_exit_r172_test.go — R172 (2026-10-18).
//
// Проверяется то, чего не было до R172: ПРИЧИНА смерти встроенного узла.
//
// До разбора авария выглядела одинаково при двух совершенно разных диагнозах:
//
//	сервер разбился (segfault/OOM)         → мастер: "Remote RPC server crashed"
//	сервер жив, операция вернула false     → мастер: "Remote RPC server crashed"
//
// (ggml-rpc.cpp:35 поднимает одно и то же сообщение на любой неуспешный ответ, а
// сервер закрывает сессию на первой неудачной операции — ggml-rpc.cpp:1857+).
// Теперь супервизор обязан назвать exit-статус и сохранить последние строки
// вывода процесса — по ним диагноз и ставится.
package cppbackend

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestRPCServerCrashHelper — «rpc-server», который падает, как настоящий:
// печатает причину в stderr (GGML_LOG_ERROR сервера) и выходит с кодом 41.
func TestRPCServerCrashHelper(t *testing.T) {
	if os.Getenv("CPPBACKEND_RPC_CRASH_HELPER") != "1" {
		t.Skip("helper process (запускается супервизором, а не go test)")
	}
	fmt.Fprintln(os.Stderr, "ggml-rpc: [server] tensor region out of buffer bounds")
	fmt.Fprintln(os.Stdout, "Accepted client connection")
	os.Exit(41)
}

// shortListenWait — тестам не нужно ждать полные 2.5 с: их «rpc-server» падает
// сразу, и ожидание порта только удлиняет прогон.
func shortListenWait(t *testing.T) {
	t.Helper()
	oldWait, oldGrace := rpcServerListenWait, rpcServerListenGrace
	rpcServerListenWait = 300 * time.Millisecond
	rpcServerListenGrace = 100 * time.Millisecond
	t.Cleanup(func() {
		rpcServerListenWait = oldWait
		rpcServerListenGrace = oldGrace
	})
}

// newCrashChild — супервизор, у которого в роли rpc-server падающий бинарь.
func newCrashChild(t *testing.T, restart bool) *RPCServerChild {
	t.Helper()
	shortListenWait(t)
	t.Setenv("CPPBACKEND_RPC_CRASH_HELPER", "1")
	c := NewRPCServerChild(Config{
		RPCServerEnabled: true,
		RPCServerBind:    "127.0.0.1",
		RPCServerPort:    freePort(t),
		RPCServerBin:     os.Args[0],
		RPCServerRestart: restart,
	})
	c.extraArgs = []string{"-test.run=TestRPCServerCrashHelper", "--"}
	t.Cleanup(func() { _ = c.Stop() })
	return c
}

// TestR172_Death_ReportsExitAndKeepsServerOutput — падение узла обязано оставить
// ДВА факта: как именно умер процесс и что он успел сказать в stderr.
func TestR172_Death_ReportsExitAndKeepsServerOutput(t *testing.T) {
	c := newCrashChild(t, false)
	started := time.Now()
	if err := c.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	st := waitStatus(t, c, func(s RPCServerStatus) bool { return !s.Running && s.LastExit != "" })
	if elapsed := time.Since(started); elapsed > 15*time.Second {
		t.Fatalf("смерть узла обнаружена только через %v", elapsed)
	}

	// 1. Причина смерти названа и содержит код выхода.
	if !strings.Contains(st.LastExit, "41") {
		t.Errorf("LastExit = %q, want упоминание кода 41 (процесс обязан отчитаться, как умер)", st.LastExit)
	}
	if st.LastExitPID <= 0 {
		t.Errorf("LastExitPID = %d, want PID умершего процесса", st.LastExitPID)
	}
	if st.LastExitAt == "" {
		t.Error("LastExitAt пуст: время падения неизвестно")
	} else if _, err := time.Parse(time.RFC3339, st.LastExitAt); err != nil {
		t.Errorf("LastExitAt = %q не RFC3339: %v", st.LastExitAt, err)
	}

	// 2. Последние слова СЕРВЕРА сохранены (именно их не хватало при разборе).
	log := c.LogTail(50)
	if len(log) == 0 {
		t.Fatal("лог узла пуст: вывод дочернего процесса не сохранён")
	}
	joined := joinLogTexts(log)
	if !strings.Contains(joined, "tensor region out of buffer bounds") {
		t.Errorf("в логе нет серверной причины отказа, есть только: %q", joined)
	}
	if !strings.Contains(joined, "Accepted client connection") {
		t.Errorf("в логе нет stdout-строки сервера (потоки обязаны попадать в один буфер): %q", joined)
	}

	// 3. Хвост лога обязан быть и в СТАТУСЕ: узел лежит — это то состояние, в
	// котором оператор и балансер читают статус.
	if len(st.LogTail) == 0 {
		t.Error("status.LogTail пуст при упавшем узле: причина не видна там, где её ищут")
	}

	// 4. Признак «процесс был, но умер» отличается от «процесс не запускали».
	if st.PID != 0 {
		t.Errorf("PID = %d у упавшего узла, want 0", st.PID)
	}
	if st.LastError == "" {
		t.Error("LastError пуст: о падении никто не узнает")
	}
}

// TestR172_Restart_PolicyHonouredAndBounded — автоперезапуск поднимает узел
// после падения (паритет с restart: unless-stopped у отдельного контейнера), но
// только пока не выключен оператором.
func TestR172_Restart_PolicyHonouredAndBounded(t *testing.T) {
	c := newCrashChild(t, true)
	if err := c.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Ждём ФАКТ перезапуска: счётчик Restarts растёт только при повторном
	// Start после смерти, то есть подтверждает, что узел действительно поднимали.
	st := waitStatus(t, c, func(s RPCServerStatus) bool { return s.Restarts >= 1 })
	if !st.RestartEnabled {
		t.Error("RestartEnabled = false, хотя перезапуск включён в конфиге")
	}
	if st.LastExit == "" {
		t.Error("перезапуск без причины смерти: оператор не узнает, почему узел падал")
	}
}

// TestR172_Restart_StopWinsOverPolicy — намеренное выключение узла сильнее
// политики перезапуска: Stop из WebUI не должен через полсекунды вернуть
// процесс к жизни.
func TestR172_Restart_StopWinsOverPolicy(t *testing.T) {
	c := newCrashChild(t, true)
	if err := c.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Именно выключение оператором (не Stop в обход конфига): SetEnabled(false)
	// снимает желание держать узел и гасит процесс.
	if err := c.SetEnabled(false); err != nil {
		t.Fatalf("SetEnabled(false): %v", err)
	}
	before := c.Status()
	if before.Enabled {
		t.Error("после Stop Enabled = true (желание оператора не снято)")
	}

	// Пауза заметно больше базовой задержки автоперезапуска (500 мс).
	time.Sleep(1500 * time.Millisecond)

	after := c.Status()
	if after.Running || after.PID != 0 {
		t.Errorf("узел поднялся после Stop (running=%v pid=%d): выключение обязано побеждать политику",
			after.Running, after.PID)
	}
	if after.Restarting {
		t.Error("после Stop статус всё ещё обещает перезапуск")
	}
}

// TestR172_Restart_BudgetBounded — бюджет автоперезапусков: не больше
// rpcServerRestartMaxPerHour в час, пауза растёт вдвое. Бесконечный цикл
// падений на 8-гигабайтной карте сам стал бы причиной отказов.
func TestR172_Restart_BudgetBounded(t *testing.T) {
	c := NewRPCServerChild(Config{
		RPCServerEnabled: true,
		RPCServerRestart: true,
		RPCServerPort:    freePort(t),
	})
	c.enabled = true

	now := time.Now()
	c.mu.Lock()
	ok, delay := c.restartBudgetLocked(now)
	c.mu.Unlock()
	if !ok {
		t.Fatal("первый перезапуск запрещён, хотя бюджет пуст")
	}
	if delay != rpcServerRestartBaseDelay {
		t.Errorf("пауза первого перезапуска %v, want %v", delay, rpcServerRestartBaseDelay)
	}

	// Три смерти за час: пауза удваивается дважды.
	c.exitTimes = []time.Time{now.Add(-3 * time.Minute), now.Add(-2 * time.Minute), now.Add(-time.Minute)}
	c.mu.Lock()
	ok, delay = c.restartBudgetLocked(now)
	c.mu.Unlock()
	if !ok {
		t.Fatal("перезапуск запрещён на третьей смерти, хотя бюджет не исчерпан")
	}
	if want := rpcServerRestartBaseDelay * 4; delay != want {
		t.Errorf("пауза после трёх смертей %v, want %v", delay, want)
	}

	// Бюджет исчерпан.
	full := make([]time.Time, rpcServerRestartMaxPerHour)
	for i := range full {
		full[i] = now.Add(-time.Duration(i+1) * time.Minute)
	}
	c.mu.Lock()
	c.exitTimes = full
	ok, _ = c.restartBudgetLocked(now)
	c.mu.Unlock()
	if ok {
		t.Errorf("перезапуск разрешён после %d смертей в час — узел уйдёт в цикл падений",
			rpcServerRestartMaxPerHour)
	}

	// Смерти вне окна не мешают: окно — час, а не вся жизнь процесса.
	c.mu.Lock()
	c.exitTimes = []time.Time{now.Add(-2 * time.Hour)}
	ok, _ = c.restartBudgetLocked(now)
	c.mu.Unlock()
	if !ok {
		t.Error("старая смерть (2 часа назад) заблокировала перезапуск: окно бюджета не соблюдено")
	}

	// Политика выключена — перезапуска нет независимо от бюджета.
	c.mu.Lock()
	c.restart = false
	c.exitTimes = nil
	ok, _ = c.restartBudgetLocked(now)
	c.mu.Unlock()
	if ok {
		t.Error("перезапуск разрешён при выключенной политике")
	}
}

// TestR172_Debug_PassesGGMLRpcDebugToChild — отладочный режим обязан доходить до
// дочернего процесса переменной окружения (иначе «включил отладку» ничего не
// меняет, и это ровно тот класс дефектов, который вычищаем: настройка есть, а
// эффекта нет).
func TestR172_Debug_PassesGGMLRpcDebugToChild(t *testing.T) {
	for _, debug := range []bool{true, false} {
		t.Run(fmt.Sprintf("debug=%v", debug), func(t *testing.T) {
			t.Setenv("CPPBACKEND_RPC_ENV_HELPER", "1")
			shortListenWait(t)
			c := NewRPCServerChild(Config{
				RPCServerEnabled: true,
				RPCServerBind:    "127.0.0.1",
				RPCServerPort:    freePort(t),
				RPCServerBin:     os.Args[0],
				RPCServerDebug:   debug,
				RPCServerRestart: false,
			})
			c.extraArgs = []string{"-test.run=TestRPCServerEnvHelper", "--"}
			t.Cleanup(func() { _ = c.Stop() })

			if err := c.Start(); err != nil {
				t.Fatalf("Start: %v", err)
			}
			// Процесс печатает значение переменной и выходит — ждём строку.
			deadline := time.Now().Add(10 * time.Second)
			var joined string
			for time.Now().Before(deadline) {
				joined = joinLogTexts(c.LogTail(50))
				if strings.Contains(joined, "GGML_RPC_DEBUG_VALUE=") {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			if !strings.Contains(joined, "GGML_RPC_DEBUG_VALUE=") {
				t.Fatalf("дочерний процесс не отчитался о переменной: %q", joined)
			}
			if debug {
				if !strings.Contains(joined, "GGML_RPC_DEBUG_VALUE=1") {
					t.Errorf("при debug=true в дочерний процесс не передан GGML_RPC_DEBUG=1: %q", joined)
				}
			} else if strings.Contains(joined, "GGML_RPC_DEBUG_VALUE=1") {
				t.Errorf("при debug=false дочерний процесс всё равно получил GGML_RPC_DEBUG: %q", joined)
			}
		})
	}
}

// TestRPCServerEnvHelper — «rpc-server», который сообщает значение GGML_RPC_DEBUG
// и выходит штатно.
func TestRPCServerEnvHelper(t *testing.T) {
	if os.Getenv("CPPBACKEND_RPC_ENV_HELPER") != "1" {
		t.Skip("helper process (запускается супервизором, а не go test)")
	}
	fmt.Fprintln(os.Stderr, "GGML_RPC_DEBUG_VALUE="+os.Getenv("GGML_RPC_DEBUG"))
	os.Exit(0)
}

// joinLogTexts — склейка строк лога для проверок «есть ли там такое-то слово».
func joinLogTexts(lines []RPCServerLogLine) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.Text)
		b.WriteString("\n")
	}
	return b.String()
}
