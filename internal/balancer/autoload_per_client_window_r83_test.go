//go:build llama_stub

// autoload_per_client_window_r83_test.go — R83-фикс (2026-10-01): авто-загрузка
// от балансера передаёт окно клиента как «на клиента», а не как суммарное.
//
// Живой дефект. Балансер складывал client num_ctx в `contextSize`, который
// cppworker трактует как СУММАРНОЕ окно модели. При CPPWORKER_N_PARALLEL=2
// клиент получал ровно половину: попросил 32768 → слот 16384 → его же запрос
// отвергался («model is loaded with n_ctx=32768 … required 32768» на слот 16384),
// а клиент с окном 32768 не мог работать вовсе. Живое состояние стенда после
// такого auto-load: context_size=32768, context_per_seq=16384, max_slots=2.
package balancer

import "testing"

// TestR83Policy_AutoLoadRequestUsesPerClientWindow — клиентское окно уходит в
// contextPerSeq (cppworker сам умножит на слоты), а не в contextSize.
func TestR83Policy_AutoLoadRequestUsesPerClientWindow(t *testing.T) {
	perSeq := 32768
	req := buildAutoLoadRequest("gemma-4-E4B-it-Q4_K_M", nil, &perSeq, nil)

	if req.ContextPerSeq == nil || *req.ContextPerSeq != 32768 {
		t.Fatalf("ContextPerSeq = %v, ожидалось 32768 (окно на клиента)", req.ContextPerSeq)
	}
	if req.ContextSize != nil {
		t.Errorf("ContextSize = %v, ожидалось nil: суммарное окно считает cppworker "+
			"(иначе при parallel=2 клиент получит половину)", *req.ContextSize)
	}
	if req.Operation != "load" || req.ModelName != "gemma-4-E4B-it-Q4_K_M" {
		t.Errorf("operation/model потерялись: %+v", req)
	}
}

// TestR83Policy_AutoLoadRequest_ProfileWindowStillSupported — суммарное окно из
// профиля (contextSize) продолжает работать: оно уходит как есть.
func TestR83Policy_AutoLoadRequest_ProfileWindowStillSupported(t *testing.T) {
	total := 65536
	req := buildAutoLoadRequest("m", &total, nil, nil)
	if req.ContextSize == nil || *req.ContextSize != 65536 {
		t.Fatalf("ContextSize = %v, ожидалось 65536 (суммарное из профиля)", req.ContextSize)
	}
	if req.ContextPerSeq != nil {
		t.Errorf("ContextPerSeq = %v, ожидалось nil", *req.ContextPerSeq)
	}
}
