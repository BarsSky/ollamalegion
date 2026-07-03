package api

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestDedupBackendsByHostPort_NoDuplicates — sanity check: если все бэкенды
// уникальны по (host, port), функция возвращает их без изменений (порядок сохраняется).
func TestDedupBackendsByHostPort_NoDuplicates(t *testing.T) {
	backends := []types.BackendMetrics{
		{ID: "b1", Host: "host1", CppWorkerPort: 18092},
		{ID: "b2", Host: "host2", CppWorkerPort: 18092},
		{ID: "b3", Host: "host1", CppWorkerPort: 18093},
	}
	result := dedupBackendsByHostPort(
		backends,
		func(b types.BackendMetrics) string { return b.Host },
		func(b types.BackendMetrics) int { return b.CppWorkerPort },
		func(b types.BackendMetrics) bool { return b.HasAgent },
		true,
	)
	if len(result) != 3 {
		t.Errorf("expected 3 backends, got %d", len(result))
	}
}

// TestDedupBackendsByHostPort_WithDuplicates — основной сценарий: 3 бэкенда
// на одном (host, port), де-дуп должен оставить один.
func TestDedupBackendsByHostPort_WithDuplicates(t *testing.T) {
	backends := []types.BackendMetrics{
		{ID: "cppworker-gpu", Host: "cppworker", CppWorkerPort: 18092, HasAgent: false},
		{ID: "cppworker-gpu-bundled", Host: "cppworker", CppWorkerPort: 18092, HasAgent: false},
		{ID: "agent-1", Host: "cppworker", CppWorkerPort: 18092, HasAgent: true},
	}
	result := dedupBackendsByHostPort(
		backends,
		func(b types.BackendMetrics) string { return b.Host },
		func(b types.BackendMetrics) int { return b.CppWorkerPort },
		func(b types.BackendMetrics) bool { return b.HasAgent },
		true,
	)
	if len(result) != 1 {
		t.Fatalf("expected 1 backend after dedup, got %d", len(result))
	}
	// Должен победить hasAgent=true кандидат (agent-1)
	if result[0].ID != "agent-1" {
		t.Errorf("expected agent-1 (hasAgent=true), got %s", result[0].ID)
	}
}

// TestDedupBackendsByHostPort_PreferAgentDisabled — если preferAgent=false,
// побеждает первый встретившийся бэкенд (порядок итерации).
func TestDedupBackendsByHostPort_PreferAgentDisabled(t *testing.T) {
	backends := []types.BackendMetrics{
		{ID: "first", Host: "h", CppWorkerPort: 18092, HasAgent: false},
		{ID: "second-with-agent", Host: "h", CppWorkerPort: 18092, HasAgent: true},
	}
	result := dedupBackendsByHostPort(
		backends,
		func(b types.BackendMetrics) string { return b.Host },
		func(b types.BackendMetrics) int { return b.CppWorkerPort },
		func(b types.BackendMetrics) bool { return b.HasAgent },
		// preferAgent=false — не учитываем hasAgent
		false,
	)
	if len(result) != 1 {
		t.Fatalf("expected 1 backend, got %d", len(result))
	}
	if result[0].ID != "first" {
		t.Errorf("expected 'first' (порядок итерации), got %s", result[0].ID)
	}
}

// TestDedupBackendsByHostPort_Empty — пустой вход → пустой выход.
func TestDedupBackendsByHostPort_Empty(t *testing.T) {
	result := dedupBackendsByHostPort(
		[]types.BackendMetrics{},
		func(b types.BackendMetrics) string { return b.Host },
		func(b types.BackendMetrics) int { return b.CppWorkerPort },
		func(b types.BackendMetrics) bool { return b.HasAgent },
		true,
	)
	if len(result) != 0 {
		t.Errorf("expected empty, got %d", len(result))
	}
}

// TestDedupBackendsByHostPort_OrderPreserved — порядок первого появления
// каждого ключа сохраняется в результате.
func TestDedupBackendsByHostPort_OrderPreserved(t *testing.T) {
	backends := []types.BackendMetrics{
		{ID: "a1", Host: "h1", CppWorkerPort: 18092},
		{ID: "b1", Host: "h2", CppWorkerPort: 18092},
		{ID: "a2", Host: "h1", CppWorkerPort: 18092}, // дубль a1
		{ID: "c1", Host: "h3", CppWorkerPort: 18092},
	}
	result := dedupBackendsByHostPort(
		backends,
		func(b types.BackendMetrics) string { return b.Host },
		func(b types.BackendMetrics) int { return b.CppWorkerPort },
		func(b types.BackendMetrics) bool { return b.HasAgent },
		true,
	)
	if len(result) != 3 {
		t.Fatalf("expected 3, got %d", len(result))
	}
	expectedOrder := []string{"a1", "b1", "c1"}
	for i, want := range expectedOrder {
		if result[i].ID != want {
			t.Errorf("at index %d: want %s, got %s", i, want, result[i].ID)
		}
	}
}

// TestDedupBackendsByHostPort_DifferentPortsSameHost — один host, разные port
// не считаются дублями (т.к. это разные физические эндпоинты).
func TestDedupBackendsByHostPort_DifferentPortsSameHost(t *testing.T) {
	backends := []types.BackendMetrics{
		{ID: "primary", Host: "h", CppWorkerPort: 18092},
		{ID: "secondary", Host: "h", CppWorkerPort: 18093},
	}
	result := dedupBackendsByHostPort(
		backends,
		func(b types.BackendMetrics) string { return b.Host },
		func(b types.BackendMetrics) int { return b.CppWorkerPort },
		func(b types.BackendMetrics) bool { return b.HasAgent },
		true,
	)
	if len(result) != 2 {
		t.Errorf("expected 2 backends (different ports), got %d", len(result))
	}
}

// TestBackendMetricsDeDupKey — sanity check для хелпера извлечения ключа.
func TestBackendMetricsDeDupKey(t *testing.T) {
	bm := types.BackendMetrics{
		Host:          "cppworker",
		CppWorkerPort: 18092,
		HasAgent:      true,
	}
	host, port, hasAgent := BackendMetricsDeDupKey(bm)
	if host != "cppworker" || port != 18092 || !hasAgent {
		t.Errorf("BackendMetricsDeDupKey returned wrong values: host=%s port=%d hasAgent=%v",
			host, port, hasAgent)
	}
}