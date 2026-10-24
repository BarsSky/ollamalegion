//go:build llama_stub

// rpc_reach_r229_test.go — R229 (2026-10-22): исключение недоступного донора.
//
// ЖИВОЙ СЦЕНАРИЙ. Модель разложена на донора; донор пропадает. Правила R114/R115
// не дают дозваниваться до узлов RPC-опросом во время работы (это роняет мастер),
// поэтому планирование брало последний снимок ВМЕСТЕ с мёртвым узлом, а гейт перед
// загрузкой отменял загрузку ЦЕЛИКОМ — хотя без донора модель влезала.
//
// Тесты фиксируют:
//   - TCP-проба отличает живого (держит соединение молча) от мёртвого (порт закрыт);
//   - filterReachableRPC оставляет только живых и запоминает исключённых;
//   - все узлы мёртвы → nil (планирование/мост идут как «раскладки нет»);
//   - вернувшийся узел снимает отметку об исключении.
package cppbackend

import (
	"net"
	"testing"
	"time"
)

// silentRPCServer — «живой» rpc-server: принимает соединения и молчит (именно так
// ведёт себя настоящий узел, ожидая хэндшейка клиента).
func silentRPCServer(t *testing.T) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				// Держим соединение открытым и молчим.
				<-done
				_ = c.Close()
			}(conn)
		}
	}()
	return ln.Addr().String(), func() {
		close(done)
		_ = ln.Close()
	}
}

// deadAddr — закрытый порт: соединение отвергается сразу (как «published port без
// слушателя» из R130).
func deadAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func TestR229_ReachableRPCServerListSplitsAliveAndDead(t *testing.T) {
	ResetRPCReachCacheForTest()
	aliveAddr, stop := silentRPCServer(t)
	defer stop()
	dead := deadAddr(t)

	alive, bad := reachableRPCServerList([]string{aliveAddr, dead})
	if len(alive) != 1 || alive[0] != aliveAddr {
		t.Fatalf("alive = %v, want [%s]", alive, aliveAddr)
	}
	if len(bad) != 1 || bad[0] != dead {
		t.Fatalf("dead = %v, want [%s]", bad, dead)
	}
}

func TestR229_FilterReachableRPCKeepsAliveAndRemembersExcluded(t *testing.T) {
	ResetRPCReachCacheForTest()
	aliveAddr, stop := silentRPCServer(t)
	defer stop()
	dead := deadAddr(t)

	b := NewBackend(Config{RPCEnabled: true, RPCServers: []string{aliveAddr, dead}})
	got := b.filterReachableRPC(b.activeRPCServers())
	if len(got) != 1 || got[0] != aliveAddr {
		t.Fatalf("filterReachableRPC = %v, want [%s]", got, aliveAddr)
	}
	excluded := b.ExcludedRPCServers()
	if len(excluded) != 1 || excluded[0] != dead {
		t.Fatalf("ExcludedRPCServers = %v, want [%s]", excluded, dead)
	}
	if b.ExcludedRPCAt().IsZero() {
		t.Error("время исключения не записано")
	}
}

func TestR229_AllNodesDeadMeansNoLayout(t *testing.T) {
	ResetRPCReachCacheForTest()
	dead1 := deadAddr(t)
	dead2 := deadAddr(t)

	b := NewBackend(Config{RPCEnabled: true, RPCServers: []string{dead1, dead2}})
	if got := b.filterReachableRPC(b.activeRPCServers()); got != nil {
		t.Fatalf("filterReachableRPC = %v, want nil (раскладки нет → работаем локально)", got)
	}
	if len(b.ExcludedRPCServers()) != 2 {
		t.Errorf("исключено узлов %d, want 2", len(b.ExcludedRPCServers()))
	}
}

func TestR229_ReturnedNodeClearsExclusion(t *testing.T) {
	ResetRPCReachCacheForTest()
	aliveAddr, stop := silentRPCServer(t)
	defer stop()

	b := NewBackend(Config{RPCEnabled: true, RPCServers: []string{aliveAddr}})
	// Сначала «мёртвый» период: адрес не слушается — используем закрытый порт.
	dead := deadAddr(t)
	b.cfg.RPCServers = []string{dead}
	_ = b.filterReachableRPC(b.activeRPCServers())
	if len(b.ExcludedRPCServers()) != 1 {
		t.Fatalf("ожидалось исключение мёртвого узла, got %v", b.ExcludedRPCServers())
	}
	// Узел вернулся (тот же адрес теперь слушает): сбрасываем кэш, как по TTL.
	b.cfg.RPCServers = []string{aliveAddr}
	ResetRPCReachCacheForTest()
	if got := b.filterReachableRPC(b.activeRPCServers()); len(got) != 1 {
		t.Fatalf("вернувшийся узел не вернулся в раскладку: %v", got)
	}
	if len(b.ExcludedRPCServers()) != 0 {
		t.Errorf("отметка об исключении не снята: %v", b.ExcludedRPCServers())
	}
}

func TestR229_ReachCacheAvoidsSecondProbe(t *testing.T) {
	ResetRPCReachCacheForTest()
	aliveAddr, stop := silentRPCServer(t)
	stop() // закрываем сразу: проба должна кэшироваться, а не перепроверяться

	dead := deadAddr(t)
	_, _ = reachableRPCServerList([]string{dead})
	// Второй вызов в пределах TTL обязан отдать тот же результат без новой пробы.
	start := time.Now()
	_, bad := reachableRPCServerList([]string{dead})
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("повторный вызов делал пробу (%s) вместо кэша", elapsed)
	}
	if len(bad) != 1 || bad[0] != dead {
		t.Errorf("кэш вернул %v, want [%s]", bad, dead)
	}
	_ = aliveAddr
}
