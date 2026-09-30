//go:build llama_stub

// nonblocking_status_r83_test.go — R83-fix (2026-09-30): читающие эндпоинты
// (/api/models, /api/info, /api/models/active-queries) не должны ждать
// завершения генерации.
//
// Живой дефект локального стенда (gemma-4, RTX 3070): пока шёл запрос Cline с
// prompt ≈16k токенов, cppworker отдавал двенадцать GET /api/info с duration
// 2m51s / 2m36s / … / 6s — все завершились в одну миллисекунду с окончанием
// генерации. GET /api/models в это же время не отвечал вовсе (таймаут 25 s).
//
// Причина: inst.mu удерживается на всё время инференса (см. Generate), а
// getActiveQueries читал счётчик под тем же мьютексом, и его вызывали
// GetMetrics (→ /api/info) и ListModels (→ /api/models). Последствия на стенде:
// метрики балансера и WebUI не обновлялись, агент не мог отдать heartbeat.
package cppbackend

import (
	"testing"
	"time"
)

// TestR83Fix_ListModelsDoesNotBlockDuringInference — ListModels обязан отвечать,
// пока inst.mu занят генерацией.
func TestR83Fix_ListModelsDoesNotBlockDuringInference(t *testing.T) {
	b := NewBackend(Config{ModelsDir: t.TempDir()})
	const name = "busy-model"

	inst := &modelInstance{info: ModelInfo{Name: name, State: StateLoaded, GPULayers: 19}}
	b.mu.Lock()
	b.models[name] = inst
	b.mu.Unlock()

	// Симулируем активную генерацию: Generate держит inst.mu от входа в
	// Infer до return.
	inst.mu.Lock()
	defer inst.mu.Unlock()
	inst.incQueries()
	defer inst.decQueries()

	done := make(chan []ModelInfo, 1)
	go func() { done <- b.ListModels() }()

	select {
	case models := <-done:
		if len(models) != 1 {
			t.Fatalf("ListModels вернул %d моделей, ожидалась 1", len(models))
		}
		if models[0].ActiveQueries != 1 {
			t.Errorf("ActiveQueries = %d, ожидалось 1 (счётчик должен быть виден "+
				"во время генерации)", models[0].ActiveQueries)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ListModels заблокировался на inst.mu, который удерживает генерация — " +
			"именно это вешало /api/models (и метрики балансера) на минуты")
	}
}

// TestR83Fix_GetMetricsDoesNotBlockDuringInference — то же для /api/info.
func TestR83Fix_GetMetricsDoesNotBlockDuringInference(t *testing.T) {
	b := NewBackend(Config{ModelsDir: t.TempDir()})
	const name = "busy-model-metrics"

	inst := &modelInstance{info: ModelInfo{Name: name, State: StateLoaded}}
	b.mu.Lock()
	b.models[name] = inst
	b.mu.Unlock()

	inst.mu.Lock()
	defer inst.mu.Unlock()
	inst.incQueries()
	inst.incQueries()
	defer func() { inst.decQueries(); inst.decQueries() }()

	done := make(chan map[string]interface{}, 1)
	go func() { done <- b.GetMetrics() }()

	select {
	case m := <-done:
		if got, _ := m["activeQueries"].(int); got != 2 {
			t.Errorf("activeQueries = %v, ожидалось 2", m["activeQueries"])
		}
		if got, _ := m["totalQueries"].(int64); got != 2 {
			t.Errorf("totalQueries = %v, ожидалось 2", m["totalQueries"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("GetMetrics заблокировался на inst.mu — /api/info висел всё время генерации")
	}
}

// TestR83Fix_CountersStayInSyncWithInfoMirror — зеркало в inst.info (его читают
// тесты и код, смотрящий в modelInstance напрямую) должно совпадать с атомиками
// на путях, где мьютекс уже удержан.
func TestR83Fix_CountersStayInSyncWithInfoMirror(t *testing.T) {
	inst := &modelInstance{info: ModelInfo{Name: "mirror", State: StateLoaded}}

	inst.mu.Lock()
	inst.incQueries()
	inst.syncQueryCountersLocked()
	inst.mu.Unlock()

	if inst.info.ActiveQueries != 1 || inst.info.TotalQueries != 1 {
		t.Fatalf("зеркало info не синхронизировано: active=%d total=%d",
			inst.info.ActiveQueries, inst.info.TotalQueries)
	}

	inst.mu.Lock()
	inst.decQueries()
	inst.syncQueryCountersLocked()
	inst.mu.Unlock()

	if inst.info.ActiveQueries != 0 || inst.info.TotalQueries != 1 {
		t.Fatalf("после завершения: active=%d (ждём 0), total=%d (ждём 1)",
			inst.info.ActiveQueries, inst.info.TotalQueries)
	}
	if got := inst.activeQueriesCount(); got != 0 {
		t.Errorf("activeQueriesCount = %d, ожидалось 0", got)
	}
}
