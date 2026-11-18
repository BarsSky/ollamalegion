//go:build llama_stub

// window_reserve_clamp_r502_test.go — R502 (2026-11-19), аудит S4, пункт N-9 со
// стороны воркера: жёсткий пол n_predict берётся из ОБЩЕЙ константы, а не из
// литерала, иначе балансер и воркер снова разойдутся (балансер считал 256,
// воркер требует 512 → HTTP 413 при живом запасе окна).
package main

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR502_WorkerClampIsSharedConstant — N-9: воркер берёт пол из того же
// значения, что и балансер.
func TestR502_WorkerClampIsSharedConstant(t *testing.T) {
	if minNPredictClamp != types.ContextBudgetMinAnswerTokensR502 {
		t.Fatalf("пол n_predict у воркера = %d, общая константа = %d: два конца "+
			"снова считают по-разному (аудит N-9)",
			minNPredictClamp, types.ContextBudgetMinAnswerTokensR502)
	}
	if minNPredictClamp != 512 {
		t.Fatalf("пол n_predict = %d, ожидалось 512 (значение зафиксировано "+
			"аудитом: ниже воркер не опускает ответ)", minNPredictClamp)
	}
}
