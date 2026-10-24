//go:build llama_stub

// rpc_peers_r125_test.go — R125 (пункт 3): «какой донор активен» проверяется
// ФАКТОМ доступности, а не наличием строки в списке.
package cppbackend

import (
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// listenLocal — живой слушающий сокет (в тесте роль доступного узла).
func listenLocal(t *testing.T) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln.Addr().String(), func() { _ = ln.Close() }
}

// TestR125_ProbeRPCServers_TellsReachableFromUnreachable — доступный узел
// помечен reachable, недоступный — нет, и у него есть причина.
func TestR125_ProbeRPCServers_TellsReachableFromUnreachable(t *testing.T) {
	good, closeGood := listenLocal(t)
	defer closeGood()
	bad := "127.0.0.1:1" // порт 1 закрыт

	peers := ProbeRPCServers([]string{good, bad}, 500*time.Millisecond)
	if len(peers) != 2 {
		t.Fatalf("узлов в снимке: %d, want 2", len(peers))
	}
	if peers[0].Addr != good || !peers[0].Reachable {
		t.Errorf("доступный узел %s: %+v — оператор увидит «донор не активен»", good, peers[0])
	}
	if peers[0].Error != "" {
		t.Errorf("у доступного узла не должно быть ошибки: %q", peers[0].Error)
	}
	if peers[1].Addr != bad || peers[1].Reachable {
		t.Errorf("недоступный узел %s помечен доступным: %+v", bad, peers[1])
	}
	if peers[1].Error == "" {
		t.Error("у недоступного узла нет причины: UI покажет «не активен» без объяснения")
	}
}

// TestR125_ProbeRPCServers_CountsReachable — сводка для UI/метрик.
func TestR125_ProbeRPCServers_CountsReachable(t *testing.T) {
	a, closeA := listenLocal(t)
	defer closeA()
	b, closeB := listenLocal(t)
	defer closeB()

	peers := ProbeRPCServers([]string{a, b, "127.0.0.1:1"}, 500*time.Millisecond)
	if got := CountReachableRPC(peers); got != 2 {
		t.Errorf("CountReachableRPC = %d, want 2 из 3 (третий порт закрыт)", got)
	}
}

// TestR125_ProbeRPCServers_EmptyAndBlankAreSkipped — раскладка не настроена или в
// списке пустые строки: это не «недоступные узлы», а «нет узлов».
func TestR125_ProbeRPCServers_EmptyAndBlankAreSkipped(t *testing.T) {
	if peers := ProbeRPCServers(nil, 0); len(peers) != 0 {
		t.Errorf("пустой список дал %d записей, want 0", len(peers))
	}
	peers := ProbeRPCServers([]string{"", "   "}, 0)
	if len(peers) != 0 {
		t.Errorf("пустые строки дали %d записей, want 0", len(peers))
	}
	if CountReachableRPC(peers) != 0 {
		t.Error("в пустом снимке не может быть доступных узлов")
	}
}

// TestR125_ProbeRPCServers_LatencyIsMeasuredForReachable — задержка есть у
// доступного узла (это то, что можно измерить честно; скорость — нет, и UI не
// должен выдавать задержку за пропускную способность).
func TestR125_ProbeRPCServers_LatencyIsMeasuredForReachable(t *testing.T) {
	good, closeGood := listenLocal(t)
	defer closeGood()

	peers := ProbeRPCServers([]string{good}, 500*time.Millisecond)
	if len(peers) != 1 || !peers[0].Reachable {
		t.Fatalf("подготовка теста: %+v", peers)
	}
	if peers[0].LatencyMS < 0 {
		t.Errorf("LatencyMS = %d, want >= 0", peers[0].LatencyMS)
	}
}

// TestR125_ProbeRPCServers_MatchesLoadGateVerdict — вердикт пробника совпадает с
// гейтом загрузки: иначе UI показывал бы «донор активен», а загрузка падала бы на
// том же узле.
func TestR125_ProbeRPCServers_MatchesLoadGateVerdict(t *testing.T) {
	good, closeGood := listenLocal(t)
	defer closeGood()
	bad := "127.0.0.1:" + strconv.Itoa(freeClosedPort(t))

	peers := ProbeRPCServers([]string{good, bad}, 400*time.Millisecond)
	reachable := map[string]bool{}
	for _, p := range peers {
		reachable[p.Addr] = p.Reachable
	}

	if err := CheckRPCServers([]string{good}, 400*time.Millisecond); err != nil {
		t.Errorf("гейт загрузки не пропустил доступный узел: %v", err)
	}
	if !reachable[good] {
		t.Error("пробник счёл доступный узел недоступным — расходится с гейтом")
	}
	if err := CheckRPCServers([]string{bad}, 400*time.Millisecond); err == nil {
		t.Error("гейт загрузки пропустил недоступный узел")
	} else if reachable[bad] {
		t.Error("пробник счёл недоступный узел доступным — расходится с гейтом")
	}
	if !strings.Contains(bad, ":") {
		t.Fatal("адрес узла должен быть host:port")
	}
}

// freeClosedPort — порт, который заведомо ничего не слушает (открыли и закрыли).
func freeClosedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}
