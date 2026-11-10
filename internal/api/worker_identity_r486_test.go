//go:build llama_stub

// worker_identity_r486_test.go — R486 (2026-11-10): балансер видит, с какого
// РЕЛИЗА собран узел.
//
// ЖИВОЙ КОНТЕКСТ. До R486 узел сообщал только коммит llama.cpp (BuildInfo).
// Стенд из мастера и узла на РАЗНЫХ релизных образах выглядел рабочим, пока не
// начинались загадки «тот же запрос ведёт себя по-разному»: расхождение версий
// обнаруживалось глазами оператора, а не системой.
package api

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
	"ollama-loadbalancer/pkg/version"
)

// stubBalancerCommitR486 — подменить коммит балансера (обычно -ldflags).
func stubBalancerCommitR486(t *testing.T, commit string) {
	t.Helper()
	prev := version.Commit
	version.Commit = commit
	t.Cleanup(func() { version.Commit = prev })
}

// TestR486_IdentityNotOverwrittenByEmpty — пустые поля не стирают известное.
//
// Сборки до R486 этих полей не знают; их heartbeat не должен обнулять уже
// полученную идентичность, иначе проверка «узел из того же релиза» мигала бы.
func TestR486_IdentityNotOverwrittenByEmpty(t *testing.T) {
	b := &types.Backend{ID: "w1"}
	applyWorkerIdentityR486(b, "0.7.246", "abc1234", "2026-11-10T14:40:00Z", "6.0.0")
	applyWorkerIdentityR486(b, "", "", "", "")

	if b.WorkerVersion != "0.7.246" || b.WorkerCommit != "abc1234" ||
		b.WorkerBuildDate != "2026-11-10T14:40:00Z" || b.WorkerRPCProto != "6.0.0" {
		t.Fatalf("известная идентичность затёрта пустыми значениями: %+v", b)
	}
	// nil-бэкенд не должен паниковать: вызов ниже — сама эта проверка.
	applyWorkerIdentityR486(nil, "x", "y", "z", "p")
}

// TestR486_ReleaseMismatch — узел из другого релиза виден как расхождение.
func TestR486_ReleaseMismatch(t *testing.T) {
	stubBalancerCommitR486(t, "8b246ffe")

	same := &types.Backend{ID: "w1", WorkerCommit: "8b246ffe"}
	if workerReleaseMismatchR486(same) {
		t.Fatal("одинаковый коммит не должен считаться расхождением")
	}

	other := &types.Backend{ID: "w2", WorkerCommit: "850923d7"}
	if !workerReleaseMismatchR486(other) {
		t.Fatal("узел с другим коммитом обязан быть помечен как расхождение релизов")
	}

	// Узел старого образа коммита не сообщает: это «неизвестно», а не «разошлось».
	unknown := &types.Backend{ID: "w3"}
	if workerReleaseMismatchR486(unknown) {
		t.Fatal("пустой коммит узла — это неизвестность, а не расхождение")
	}
	if workerReleaseMismatchR486(nil) {
		t.Fatal("nil-бэкенд не должен давать расхождение")
	}
}

// TestR486_ReleaseMismatchWithoutOwnCommit — балансер без штампа не судит.
func TestR486_ReleaseMismatchWithoutOwnCommit(t *testing.T) {
	stubBalancerCommitR486(t, "")
	b := &types.Backend{ID: "w1", WorkerCommit: "850923d7"}
	if workerReleaseMismatchR486(b) {
		t.Fatal("без собственного коммита сравнивать нечем — расхождения быть не должно")
	}
}
