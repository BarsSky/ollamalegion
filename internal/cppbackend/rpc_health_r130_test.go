//go:build llama_stub

// rpc_health_r130_test.go — R130 (2026-10-16): гейт узлов раскладки обязан
// отличать ЖИВОЙ узел от «опубликованного порта без слушателя».
//
// ЖИВОЙ ДЕФЕКТ (падение мастера). Во время миграции узел донора остался
// опубликованным портом без слушателя внутри контейнера. Прежний гейт проверял
// только TCP-соединение — оно УСТАНАВЛИВАЛОСЬ, узел признавался доступным и
// уходил в C-мост, после чего llama.cpp вызывал abort() и мастер падал целиком:
//
//	SIGABRT: abort (signal arrived during cgo execution)
//	  c/bridge.CollectDevices                       bridge.go:507
//	  cppbackend.(*Backend).rpcDeviceSnapshot       memfit_adapter.go:249
//	  cppbackend.(*Backend).MemfitBudget            memfit_adapter.go:141
//	  main.handleListModels                         ← обычный GET /api/models
//
// ЖИВОЙ ЗАМЕР, на котором построено правило (мастер):
//
//	dead-end (порт опубликован, слушателя нет): connected -> CLOSE за 14 мс
//	живой rpc-server донора                   : connected -> SILENT (ждёт хэндшейка)
package cppbackend

import (
	"io"
	"net"
	"testing"
	"time"
)

// deadEndListener — принимает соединение и сразу закрывает его: ровно так ведёт
// себя опубликованный порт без слушателя.
func deadEndListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	return ln.Addr().String()
}

// silentListener — принимает и МОЛЧИТ, держа соединение: так ведёт себя живой
// rpc-server, который ждёт хэндшейка клиента.
func silentListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { _, _ = io.Copy(io.Discard, c) }(conn)
		}
	}()
	return ln.Addr().String()
}

// TestR130_DeadEndPortIsUnreachable — ГЛАВНЫЙ тест дефекта: порт принимает TCP,
// но за ним никого нет. Такой узел обязан считаться недоступным, иначе он
// уезжает в C-мост и роняет мастер через abort().
func TestR130_DeadEndPortIsUnreachable(t *testing.T) {
	addr := deadEndListener(t)

	// Сначала убеждаемся, что ловушка реальна: соединение УСТАНАВЛИВАЕТСЯ.
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("подготовка теста: TCP не устанавливается — это не dead-end, а закрытый порт: %v", err)
	}
	_ = conn.Close()

	if bad := unreachableRPCServers([]string{addr}, time.Second); len(bad) != 1 {
		t.Errorf("unreachableRPCServers = %v, want [%s]: узел принимает TCP, но сразу "+
			"закрывает соединение — это мёртвый проброс порта, и передача его в мост "+
			"роняет процесс (llama.cpp abort)", bad, addr)
	}
	if err := CheckRPCServers([]string{addr}, time.Second); err == nil {
		t.Error("гейт загрузки пропустил dead-end узел — модель уйдёт в мост и уронит мастер")
	}
}

// TestR130_SilentListenerIsReachable — регресс-защита от ложного отказа: живой
// rpc-server молчит, ожидая нашего хэндшейка, и обязан считаться доступным.
func TestR130_SilentListenerIsReachable(t *testing.T) {
	addr := silentListener(t)

	if bad := unreachableRPCServers([]string{addr}, time.Second); len(bad) != 0 {
		t.Errorf("unreachableRPCServers = %v, want пусто: молчащий узел — это НОРМА "+
			"для rpc-server, он ждёт клиента", bad)
	}
}

// TestR130_ChattyListenerIsReachable — узел, который сам что-то прислал, живой.
func TestR130_ChattyListenerIsReachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = conn.Write([]byte{1})
			_ = conn.Close()
		}
	}()

	if bad := unreachableRPCServers([]string{ln.Addr().String()}, time.Second); len(bad) != 0 {
		t.Errorf("unreachableRPCServers = %v, want пусто: узел сам прислал байт", bad)
	}
}

// TestR130_ClosedPortIsUnreachable — прежнее поведение сохранено.
func TestR130_ClosedPortIsUnreachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	if bad := unreachableRPCServers([]string{addr}, 300*time.Millisecond); len(bad) != 1 {
		t.Errorf("unreachableRPCServers = %v, want [%s] (порт закрыт)", bad, addr)
	}
}

// TestR130_MixedNodesNamesTheDeadOne — из живого и мёртвого узлов называем именно
// мёртвый: оператор должен видеть, кого чинить.
func TestR130_MixedNodesNamesTheDeadOne(t *testing.T) {
	good := silentListener(t)
	bad := deadEndListener(t)

	badList := unreachableRPCServers([]string{good, bad}, time.Second)
	if len(badList) != 1 || badList[0] != bad {
		t.Errorf("unreachableRPCServers = %v, want [%s] (живой узел не должен попадать в список)",
			badList, bad)
	}
}
