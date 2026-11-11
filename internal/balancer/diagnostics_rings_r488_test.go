// diagnostics_rings_r488_test.go — R488 (2026-11-11): кольца диагностики не
// должны заполняться рутиной.
//
// ЖИВОЙ ФАКТ. Архив второго стенда: в balancer-logs.txt из 2000 строк 733 были
// «CPU занят (soft…)» и столько же «VRAM above threshold», а строк `request
// trace` — одна. В worker/failures.json все 50 записей были `hold_released`
// (каждые ~20 с), поэтому ни одного `client_cancelled` в архиве не осталось — а
// именно им отвечают на вопрос «кто отменил запрос».
package balancer

import (
	"testing"
	"time"
)

func TestR488_SoftLogThrottled(t *testing.T) {
	resetSoftLogThrottleR488()
	now := time.Now()

	if !softLogAllowedR488("be-1", "cpu_busy", now, softLogEveryR488) {
		t.Fatal("первое наблюдение обязано печататься")
	}
	if softLogAllowedR488("be-1", "cpu_busy", now.Add(time.Second), softLogEveryR488) {
		t.Fatal("повтор через секунду должен быть подавлен")
	}
	if !softLogAllowedR488("be-1", "vram_threshold", now.Add(time.Second), softLogEveryR488) {
		t.Fatal("другой вид того же бэкенда — свой счётчик, он обязан печататься")
	}
}

// TestR488_ThrottlePerBackendAndKind — ключ это (бэкенд, вид), иначе один шумный
// узел глушил бы наблюдения по остальным.
func TestR488_ThrottlePerBackendAndKind(t *testing.T) {
	resetSoftLogThrottleR488()
	now := time.Now()
	if !softLogAllowedR488("be-1", "cpu_busy", now, softLogEveryR488) {
		t.Fatal("be-1/cpu_busy первое — allowed")
	}
	if !softLogAllowedR488("be-2", "cpu_busy", now, softLogEveryR488) {
		t.Fatal("be-2/cpu_busy независим от be-1")
	}
	if !softLogAllowedR488("be-1", "gpu_busy", now, softLogEveryR488) {
		t.Fatal("be-1/gpu_busy независим от be-1/cpu_busy")
	}
	if softLogAllowedR488("be-1", "cpu_busy", now.Add(softLogEveryR488-time.Second), softLogEveryR488) {
		t.Fatal("внутри окна — подавлено")
	}
	if !softLogAllowedR488("be-1", "cpu_busy", now.Add(softLogEveryR488), softLogEveryR488) {
		t.Fatal("по истечении окна — снова allowed")
	}
}

// TestR488_ThrottleDisabledWhenZero — every<=0 означает «не троттлить» (тесты и
// отладочные прогоны не должны зависеть от состояния троттлера).
func TestR488_ThrottleDisabledWhenZero(t *testing.T) {
	resetSoftLogThrottleR488()
	now := time.Now()
	first := softLogAllowedR488("be", "k", now, 0)
	second := softLogAllowedR488("be", "k", now, 0)
	if !first || !second {
		t.Fatal("every=0 обязан пропускать всё")
	}
}
