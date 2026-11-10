// release_proto_r486_test.go — R486 (2026-11-10): валидатор композиции видит
// РАЗНЫЕ РЕЛИЗЫ узлов иРАЗНЫЕ версии протокола RPC.
//
// ЖИВОЙ КОНТЕКСТ. До этой проверки валидатор сравнивал только коммит llama.cpp
// (`checkVersions`). Стенд, где балансер и воркер собраны из разных релизов (образ
// воркера проштампован предыдущим коммитом), при совпадающем llama.cpp считался
// совместимым — и «один и тот же запрос ведёт себя по-разному» искали в модели, а
// не в версиях. Версия протокола RPC сравнивается отдельно: патч-версия меняется
// вместе с числом ggml-операций, поэтому по коммиту llama.cpp она не выводится.
package composition

import (
	"strings"
	"testing"
)

// nodesWithIdentityR486 — два здоровых узла с заданными релизом и протоколом.
func nodesWithIdentityR486(releaseA, releaseB, protoA, protoB string) []Node {
	nodes := validNodesR95()
	nodes[0].ReleaseVersion, nodes[0].ReleaseCommit, nodes[0].RPCProto = releaseA, releaseA, protoA
	nodes[1].ReleaseVersion, nodes[1].ReleaseCommit, nodes[1].RPCProto = releaseB, releaseB, protoB
	return nodes
}

func validateWithNodesR486(nodes []Node) Report {
	req := validRequestR95()
	req.Group.Nodes = nodes
	return Validate(req)
}

// TestR486_SameReleaseAndProtoIsClean — одинаковый релиз и протокол: замечаний нет.
func TestR486_SameReleaseAndProtoIsClean(t *testing.T) {
	report := validateWithNodesR486(nodesWithIdentityR486("r261110-1925", "r261110-1925", "6.0.0", "6.0.0"))
	if issue, ok := findIssue(report, CheckReleaseMismatch); ok {
		t.Fatalf("одинаковый релиз не должен давать release_mismatch: %+v", issue)
	}
	if issue, ok := findIssue(report, CheckRPCProtoMismatch); ok {
		t.Fatalf("одинаковый протокол не должен давать rpc_proto_mismatch: %+v", issue)
	}
}

// TestR486_ReleaseMismatchIsError — разные релизы: error с обоими тегами и узлами.
func TestR486_ReleaseMismatchIsError(t *testing.T) {
	report := validateWithNodesR486(nodesWithIdentityR486("r261110-1925", "r261110-1740", "6.0.0", "6.0.0"))
	issue, ok := findIssue(report, CheckReleaseMismatch)
	if !ok {
		t.Fatalf("разные релизы обязаны давать release_mismatch, issues=%+v", report.Issues)
	}
	if issue.Level != LevelError {
		t.Fatalf("уровень release_mismatch = %v, ожидался error (это отказ раскладки)", issue.Level)
	}
	text := issue.Text
	for _, want := range []string{"r261110-1925", "r261110-1740", "node-a", "node-b"} {
		if !strings.Contains(text, want) {
			t.Errorf("в тексте замечания нет %q: %s", want, text)
		}
	}
}

// TestR486_RPCProtoMismatchIsError — одинаковый релиз, разные протоколы.
func TestR486_RPCProtoMismatchIsError(t *testing.T) {
	report := validateWithNodesR486(nodesWithIdentityR486("r261110-1925", "r261110-1925", "6.0.0", "7.0.0"))
	issue, ok := findIssue(report, CheckRPCProtoMismatch)
	if !ok {
		t.Fatalf("разные версии протокола обязаны давать rpc_proto_mismatch, issues=%+v", report.Issues)
	}
	if issue.Level != LevelError {
		t.Fatalf("уровень rpc_proto_mismatch = %v, ожидался error", issue.Level)
	}
	for _, want := range []string{"6.0.0", "7.0.0"} {
		if !strings.Contains(issue.Text, want) {
			t.Errorf("в тексте нет версии %q: %s", want, issue.Text)
		}
	}
}

// TestR486_UnknownIdentityIsHintNotWarning — образы до R486 не несут
// идентичности: это hint (информация), а не warning, иначе постоянное
// предупреждение на каждом старом стенде приучало бы игнорировать список.
func TestR486_UnknownIdentityIsHintNotWarning(t *testing.T) {
	report := validateWithNodesR486(nodesWithIdentityR486("", "", "", ""))
	for _, id := range []CheckID{CheckReleaseUnknown, CheckRPCProtoUnknown} {
		issue, ok := findIssue(report, id)
		if !ok {
			t.Fatalf("ожидался %s для образов без идентичности", id)
		}
		if issue.Level != LevelHint {
			t.Fatalf("%s: уровень %v, ожидался hint", id, issue.Level)
		}
	}
	if len(report.Warnings()) != 0 {
		t.Fatalf("образы без идентичности не должны попадать в предупреждения: %+v", report.Warnings())
	}
}

// TestR486_OneUnknownReleaseIsHintOnly — один узел знает релиз, второй нет:
// сравнивать не с чем, error быть не должно.
func TestR486_OneUnknownReleaseIsHintOnly(t *testing.T) {
	report := validateWithNodesR486(nodesWithIdentityR486("r261110-1925", "", "6.0.0", ""))
	if issue, ok := findIssue(report, CheckReleaseMismatch); ok {
		t.Fatalf("при одном неизвестном релизе release_mismatch недопустим: %+v", issue)
	}
	if issue, ok := findIssue(report, CheckRPCProtoMismatch); ok {
		t.Fatalf("при одном неизвестном протоколе rpc_proto_mismatch недопустим: %+v", issue)
	}
}
