//go:build llama_stub

// capacity_node_claim_r501_test.go — R501 (2026-11-19), аудит S3 (C-1/C-6).
//
// ЖИВОЙ ДЕФЕКТ. `markCapacityFromNode` вызывается при КАЖДОЙ перерегистрации
// узла и безусловно писал `RuntimeMaxConcurrentRequests = MaxConcurrentReqs`.
// Заявка узла считается один раз при старте агента, а операторский лимит
// (PUT /limits, WebUI) живёт в том же поле — значит после ближайшей
// перерегистрации лимит оператора молча исчезал, и узел снова получал своё
// значение. Тест держит это свойство: перерегистрация НЕ трогает операторское
// значение.
package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR501_MarkCapacityFromNodeKeepsOperatorLimit — операторское значение
// переживает перерегистрацию узла.
func TestR501_MarkCapacityFromNodeKeepsOperatorLimit(t *testing.T) {
	b := &types.Backend{
		ID:                           "cppworker-gpu",
		Type:                         types.BackendTypeLlamaCpp,
		MaxConcurrentReqs:            4, // заявка узла (n_parallel)
		RuntimeMaxConcurrentRequests: 1, // оператор поставил 1 через PUT /limits
		Labels:                       []string{"auto-registered"},
	}

	markCapacityFromNode(b)

	if b.RuntimeMaxConcurrentRequests != 1 {
		t.Fatalf("после перерегистрации узла runtime-лимит = %d, ожидалось 1: "+
			"перерегистрация перетёрла решение оператора (аудит C-1/C-6)",
			b.RuntimeMaxConcurrentRequests)
	}
	if !b.RuntimeCapacityFromNode {
		t.Error("признак «вместимость от ноды» не выставлен")
	}

	// Оператор своего значения не задавал → «эхо» пишется, как раньше.
	fresh := &types.Backend{
		ID:                "cppworker-new",
		Type:              types.BackendTypeLlamaCpp,
		MaxConcurrentReqs: 2,
		Labels:            []string{"auto-registered"},
	}
	markCapacityFromNode(fresh)
	if fresh.RuntimeMaxConcurrentRequests != 2 {
		t.Errorf("без операторского лимита runtime = %d, ожидалось 2 (эхо, как раньше)",
			fresh.RuntimeMaxConcurrentRequests)
	}
}
