// capacity_batched_r119_test.go — R119 (2026-10-15).
//
// ЖИВОЙ ДЕФЕКТ (стенд). cppworker: CPPWORKER_N_PARALLEL=2,
// CPPWORKER_ENABLE_BATCHED_PARALLEL=false. Встроенный агент сообщал балансеру
// вместимость 2 (просто пересказывая n_parallel), балансер пропускал двух
// клиентов — а cppworker обслуживал их ПОСЛЕДОВАТЕЛЬНО: замер напрямую
//
//	A(long):  firstData=1.09 c  total=12.29 c
//	B(short): firstData=11.76 c total=11.76 c   ← ровно конец A
//
// Второй клиент ждал МОЛЧА: без X-Queue-Wait-Ms, без позиции в очереди и без
// строки в UI — потому что до admission-очереди балансера он не доходил, его
// блокировал cppworker. Без batched-планировщика legacy-путь держит inst.mu на
// весь инференс (Round 8 race-fix), то есть llama_decode сериализуется.
//
// Тест падает до правки: агент возвращал 2 при enableBatchedParallel=false.
package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func detectWith(t *testing.T, config map[string]interface{}) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"config": config})
	}))
	defer srv.Close()
	a := &Agent{config: &types.AgentConfig{CppWorkerURL: srv.URL}, httpClient: srv.Client()}
	return a.detectCppWorkerParallelism()
}

// TestR119_DetectCppWorkerParallelism_BatchedOffReportsOne — n_parallel без
// batched-планировщика не даёт параллелизма: заявлять 2 нельзя.
func TestR119_DetectCppWorkerParallelism_BatchedOffReportsOne(t *testing.T) {
	got := detectWith(t, map[string]interface{}{
		"defaultNParallel":      2,
		"enableBatchedParallel": false,
	})
	if got != 1 {
		t.Errorf("detectCppWorkerParallelism() = %d, want 1: при "+
			"enableBatchedParallel=false legacy-путь сериализует llama_decode, и второй "+
			"клиент ждёт молча внутри cppworker вместо admission-очереди балансера", got)
	}
}

// TestR119_DetectCppWorkerParallelism_BatchedOnKeepsNParallel — с включённым
// планировщиком параллелизм настоящий, вместимость возвращается как есть.
func TestR119_DetectCppWorkerParallelism_BatchedOnKeepsNParallel(t *testing.T) {
	if got := detectWith(t, map[string]interface{}{
		"defaultNParallel":      2,
		"enableBatchedParallel": true,
	}); got != 2 {
		t.Errorf("detectCppWorkerParallelism() = %d, want 2 (batched ON — параллелизм реальный)", got)
	}
	if got := detectWith(t, map[string]interface{}{
		"defaultNParallel":      4,
		"enableBatchedParallel": true,
	}); got != 4 {
		t.Errorf("detectCppWorkerParallelism() = %d, want 4 (batched ON)", got)
	}
}

// TestR119_DetectCppWorkerParallelism_FieldAbsentKeepsOldBehaviour — старая
// сборка cppworker поля не отдаёт: поведение обязано остаться прежним (R83-fix),
// иначе апгрейд агента без апгрейда воркера молча срежет параллелизм.
func TestR119_DetectCppWorkerParallelism_FieldAbsentKeepsOldBehaviour(t *testing.T) {
	if got := detectWith(t, map[string]interface{}{"defaultNParallel": 2}); got != 2 {
		t.Errorf("detectCppWorkerParallelism() = %d, want 2: без поля enableBatchedParallel "+
			"ничего не меняем (старая сборка cppworker)", got)
	}
}

// TestR119_DetectCppWorkerParallelism_SingleSlotStaysOne — один слот при
// выключенном batched — по-прежнему 1 (граничное значение не «переворачиваем»).
func TestR119_DetectCppWorkerParallelism_SingleSlotStaysOne(t *testing.T) {
	if got := detectWith(t, map[string]interface{}{
		"defaultNParallel":      1,
		"enableBatchedParallel": false,
	}); got != 1 {
		t.Errorf("detectCppWorkerParallelism() = %d, want 1", got)
	}
}
