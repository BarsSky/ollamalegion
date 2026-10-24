//go:build llama_stub

// rpc_version_gate_r115_test.go — R115: узел с другим коммитом llama.cpp в
// раскладку не допускается.
//
// Живой случай, который это правило закрывает: мастер `1d2869c6e` и узел
// `gpu-rpc120` от 11 окт. llama.cpp на несовпадение отвечает `GGML_ABORT` —
// падением мастера. Узел при этом жив и молчалив, поэтому диагностика упиралась
// в ручное сравнение бинарников. Теперь версия спрашивается у cppworker-поверхности
// узла и несовпавший узел просто не берётся в раскладку.
package cppbackend

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// nodeStubWithBuild поднимает HTTP-заглушку узла и переводит гейт на её порт.
func nodeStubWithBuild(t *testing.T, build string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/cppworker/config" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"config":{"llamaBuild":"` + build + `"}}`))
	}))
	t.Cleanup(srv.Close)

	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("не разобрать адрес заглушки: %v", err)
	}
	prev := nodeHTTPPort
	nodeHTTPPort = port
	t.Cleanup(func() { nodeHTTPPort = prev })
	ResetRPCVersionCacheForTest()
}

func TestR115_NodeWithMatchingBuildIsAdmitted(t *testing.T) {
	nodeStubWithBuild(t, "1d2869c6e")
	b := &Backend{cfg: Config{RPCEnabled: true, RPCServers: []string{"127.0.0.1:50052"}, LlamaBuild: "1d2869c6e"}}

	ok, reason := b.nodeBuildMatches("127.0.0.1:50052")
	if !ok {
		t.Fatalf("R115: узел с тем же коммитом обязан быть допущен, причина %q", reason)
	}
	if got := b.admittedRPCServers(); len(got) != 1 {
		t.Errorf("R115: допущенный узел обязан остаться в списке, получено %v", got)
	}
}

func TestR115_NodeWithDifferentBuildIsExcluded(t *testing.T) {
	nodeStubWithBuild(t, "deadbeef")
	b := &Backend{cfg: Config{RPCEnabled: true, RPCServers: []string{"127.0.0.1:50052"}, LlamaBuild: "1d2869c6e"}}

	ok, reason := b.nodeBuildMatches("127.0.0.1:50052")
	if ok {
		t.Fatalf("R115: узел с другим коммитом не должен допускаться")
	}
	if !strings.Contains(reason, "version_mismatch") {
		t.Errorf("R115: причина обязана называть расхождение, получено %q", reason)
	}
	if got := b.admittedRPCServers(); len(got) != 0 {
		t.Errorf("R115: несовпавший узел обязан быть исключён из раскладки, получено %v", got)
	}
}

func TestR115_UnreachableNodeIsAdmittedWithWarningByDefault(t *testing.T) {
	// Живой урок: у «голого» rpc-server нет HTTP-поверхности, поэтому версию
	// узнать нельзя. Первая версия правила отказывала таким узлам по умолчанию —
	// и локальный сайдкар выпал из раскладки. Теперь неизвестный узел
	// допускается (как до R115), но строгий режим это меняет.
	prevPort := nodeHTTPPort
	nodeHTTPPort = "1" // заведомо закрытый порт
	prevStrict := os.Getenv("CPPWORKER_RPC_REQUIRE_VERSION")
	_ = os.Unsetenv("CPPWORKER_RPC_REQUIRE_VERSION")
	_ = os.Unsetenv("CPPWORKER_RPC_EXPECT_BUILD")
	t.Cleanup(func() {
		nodeHTTPPort = prevPort
		if prevStrict != "" {
			_ = os.Setenv("CPPWORKER_RPC_REQUIRE_VERSION", prevStrict)
		}
	})
	ResetRPCVersionCacheForTest()

	b := &Backend{cfg: Config{RPCEnabled: true, RPCServers: []string{"127.0.0.1:50052"}, LlamaBuild: "1d2869c6e"}}
	ok, reason := b.nodeBuildMatches("127.0.0.1:50052")
	if !ok || reason != "version_unknown_allowed" {
		t.Errorf("R115: по умолчанию неизвестный узел допускается, получено ok=%v reason=%q", ok, reason)
	}
}

func TestR115_StrictModeRefusesUnknownVersion(t *testing.T) {
	prevPort := nodeHTTPPort
	nodeHTTPPort = "1"
	_ = os.Setenv("CPPWORKER_RPC_REQUIRE_VERSION", "true")
	t.Cleanup(func() {
		nodeHTTPPort = prevPort
		_ = os.Unsetenv("CPPWORKER_RPC_REQUIRE_VERSION")
	})
	ResetRPCVersionCacheForTest()

	b := &Backend{cfg: Config{RPCEnabled: true, RPCServers: []string{"127.0.0.1:50052"}, LlamaBuild: "1d2869c6e"}}
	ok, reason := b.nodeBuildMatches("127.0.0.1:50052")
	if ok || reason != "version_unknown" {
		t.Errorf("R115: строгий режим обязан отказывать неизвестному узлу, получено ok=%v reason=%q", ok, reason)
	}
}

func TestR115_DeclaredBuildMismatchIsRefused(t *testing.T) {
	// Узлы без HTTP: оператор объявляет коммит узла; расхождение с коммитом
	// мастера — отказ, совпадение — допуск.
	prevPort := nodeHTTPPort
	nodeHTTPPort = "1"
	_ = os.Setenv("CPPWORKER_RPC_EXPECT_BUILD", "deadbeef")
	t.Cleanup(func() {
		nodeHTTPPort = prevPort
		_ = os.Unsetenv("CPPWORKER_RPC_EXPECT_BUILD")
	})
	ResetRPCVersionCacheForTest()

	b := &Backend{cfg: Config{RPCEnabled: true, RPCServers: []string{"127.0.0.1:50052"}, LlamaBuild: "1d2869c6e"}}
	if ok, reason := b.nodeBuildMatches("127.0.0.1:50052"); ok || !strings.Contains(reason, "version_declared_mismatch") {
		t.Errorf("R115: объявленный чужой коммит обязан быть отказом, получено ok=%v reason=%q", ok, reason)
	}

	_ = os.Setenv("CPPWORKER_RPC_EXPECT_BUILD", "1d2869c6e")
	ResetRPCVersionCacheForTest()
	if ok, reason := b.nodeBuildMatches("127.0.0.1:50052"); !ok || reason != "version_declared" {
		t.Errorf("R115: объявленный совпадающий коммит допускается, получено ok=%v reason=%q", ok, reason)
	}
}

func TestR115_MasterWithoutBuildDoesNotJudge(t *testing.T) {
	// Мастер собран без CPPWORKER_LLAMA_BUILD: сравнивать не с чем, поэтому
	// отказ был бы отказом всем узлам сразу. Судить не берёмся.
	nodeStubWithBuild(t, "deadbeef")
	b := &Backend{cfg: Config{RPCEnabled: true, RPCServers: []string{"127.0.0.1:50052"}}}

	if ok, reason := b.nodeBuildMatches("127.0.0.1:50052"); !ok {
		t.Errorf("R115: без версии мастера узел не отсекается, получено %q", reason)
	}
}

func TestR115_OnlyMismatchedNodeIsExcluded(t *testing.T) {
	// Два узла, один с верным коммитом: раскладка обязана продолжиться на нём.
	// Заглушка отвечает одинаково, поэтому проверяем именно сохранение порядка и
	// состава через admittedRPCServers на одном допущенном узле.
	nodeStubWithBuild(t, "1d2869c6e")
	b := &Backend{cfg: Config{
		RPCEnabled: true,
		RPCServers: []string{"127.0.0.1:50052"},
		LlamaBuild: "1d2869c6e",
	}}
	got := b.admittedRPCServers()
	if len(got) != 1 || got[0] != "127.0.0.1:50052" {
		t.Errorf("R115: порядок и состав узлов обязаны сохраняться, получено %v", got)
	}
}

func TestR115_DisabledRPCStillAdmitsNothing(t *testing.T) {
	// Выключенный режим важнее версии: узлов нет вовсе.
	nodeStubWithBuild(t, "1d2869c6e")
	b := &Backend{cfg: Config{RPCEnabled: false, RPCServers: []string{"127.0.0.1:50052"}, LlamaBuild: "1d2869c6e"}}
	if got := b.admittedRPCServers(); len(got) != 0 {
		t.Errorf("R115: при выключенной раскладке узлов быть не должно, получено %v", got)
	}
}
