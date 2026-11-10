//go:build llama_stub

// rpc_version_gate_r486_test.go — R486 (2026-11-10): расхождение ВЕРСИИ ПРОТОКОЛА
// RPC останавливает раскладку раньше llama.cpp.
//
// ЖИВОЙ КОНТЕКСТ. Гейт R115 сравнивает только коммит llama.cpp. Но протокол
// версионируется отдельно (`RPC_PROTO_MAJOR/MINOR/PATCH` в ggml-rpc.h), и
// патч-версия меняется вместе с числом ggml-операций: два образа с ОДНИМ коммитом
// llama.cpp могут иметь разные протоколы, если собраны из разных сабмодулей/патчей.
// Несовпадение llama.cpp ловит сам — `GGML_ABORT`, то есть падением мастера уже
// после загрузки весов в сокет. Теперь обе стороны сообщают RPC_PROTO_*, и узел с
// другим протоколом в раскладку не идёт.
package cppbackend

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/version"
)

// nodeStubWithIdentity — заглушка узла, которая сообщает и коммит, и протокол.
func nodeStubWithIdentity(t *testing.T, build, proto string) {
	t.Helper()
	body := `{"config":{"llamaBuild":"` + build + `","rpcProto":"` + proto + `"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/cppworker/config" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
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

// stubMasterProtoR486 — подменить версию протокола «этого» процесса.
func stubMasterProtoR486(t *testing.T, proto string) {
	t.Helper()
	prev := version.RPCProto
	version.RPCProto = proto
	t.Cleanup(func() { version.RPCProto = prev })
}

// TestR486_ProtoMismatchExcludesNode — тот же коммит llama.cpp, другой протокол.
func TestR486_ProtoMismatchExcludesNode(t *testing.T) {
	stubMasterProtoR486(t, "6.0.0")
	nodeStubWithIdentity(t, "1d2869c6e", "7.0.0")
	b := &Backend{cfg: Config{RPCEnabled: true, RPCServers: []string{"127.0.0.1:50052"}, LlamaBuild: "1d2869c6e"}}

	ok, reason := b.nodeBuildMatches("127.0.0.1:50052")
	if ok {
		t.Fatal("R486: узел с другим протоколом RPC не должен допускаться в раскладку")
	}
	if !strings.Contains(reason, "rpc_proto_mismatch") {
		t.Fatalf("R486: причина обязана называть расхождение протокола, получено %q", reason)
	}
	if !strings.Contains(reason, "7.0.0") || !strings.Contains(reason, "6.0.0") {
		t.Fatalf("R486: в причине нет обеих версий протокола: %q", reason)
	}
	if got := b.admittedRPCServers(); len(got) != 0 {
		t.Fatalf("R486: узел обязан быть исключён, получено %v", got)
	}
}

// TestR486_ProtoMatchAdmitsNode — протокол и коммит совпали.
func TestR486_ProtoMatchAdmitsNode(t *testing.T) {
	stubMasterProtoR486(t, "6.0.0")
	nodeStubWithIdentity(t, "1d2869c6e", "6.0.0")
	b := &Backend{cfg: Config{RPCEnabled: true, RPCServers: []string{"127.0.0.1:50052"}, LlamaBuild: "1d2869c6e"}}

	ok, reason := b.nodeBuildMatches("127.0.0.1:50052")
	if !ok || reason != "version_match" {
		t.Fatalf("R486: совпадающий узел обязан быть допущен, получено ok=%v reason=%q", ok, reason)
	}
}

// TestR486_ProtoUnknownOnEitherSideIsNotAVerdict — «неизвестно» это не «разошлось».
//
// Узел старого образа версию протокола не сообщает; мастер без запечённого
// значения тоже. Отказывать в этом случае значит молча отнимать ёмкость у
// рабочей конфигурации — ровно тот урок, что был в R115.
func TestR486_ProtoUnknownOnEitherSideIsNotAVerdict(t *testing.T) {
	stubMasterProtoR486(t, "6.0.0")
	nodeStubWithIdentity(t, "1d2869c6e", "")
	b := &Backend{cfg: Config{RPCEnabled: true, RPCServers: []string{"127.0.0.1:50052"}, LlamaBuild: "1d2869c6e"}}
	if ok, reason := b.nodeBuildMatches("127.0.0.1:50052"); !ok {
		t.Fatalf("R486: узел без proto обязан допускаться, получено %q", reason)
	}

	stubMasterProtoR486(t, "")
	nodeStubWithIdentity(t, "1d2869c6e", "7.0.0")
	if ok, reason := b.nodeBuildMatches("127.0.0.1:50052"); !ok {
		t.Fatalf("R486: мастер без proto не судит, получено %q", reason)
	}
}
