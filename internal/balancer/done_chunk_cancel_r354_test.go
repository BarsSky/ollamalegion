// done_chunk_cancel_r354_test.go — R354 (2026-10-31).
//
// ЖИВОЙ ФАКТ (проверка R353 на стенде). Клиент запустил стрим /api/generate и нажал
// Stop; балансер разослал POST /api/cancel, воркер прекратил генерацию и прислал
// финальный кадр:
//
//	{"done":true,"done_reason":"error",
//	 "error":"stream inference cancelled (code -100): bridge: inference aborted by user (BRIDGE_ERR_ABORTED)"}
//
// Балансер отдавал его клиенту как есть. Клиент видел done_reason="error" — «сервер
// сломался» вместо «я сам остановил»; в журнале это был Warn про ошибку upstream, а
// текст ошибки попадал в upstreamErrorText, по которому работают auto-continue и
// эвристики n_ctx.
//
// R354: отмена пользователем — штатное завершение. Кадр остаётся ОДНИМ терминатором,
// но с done_reason="cancelled" (реестр R328) и БЕЗ error.
//
// Проверяется:
//  1. признаки отмены опознаются (все три формулировки, регистр не важен);
//  2. настоящая ошибка (n_ctx) отменой НЕ считается;
//  3. нормализация меняет только причину и убирает error, сохраняя model/created_at;
//  4. не-done кадр и не-JSON остаются как были (чужие кадры не трогаем);
//  5. повторная нормализация ничего не портит (идемпотентность).
package balancer

import (
	"encoding/json"
	"testing"
)

// TestR354_IsUserCancelDoneChunk — опознание отмены по тексту.
func TestR354_IsUserCancelDoneChunk(t *testing.T) {
	positives := []string{
		"stream inference cancelled (code -100): bridge: inference aborted by user (BRIDGE_ERR_ABORTED)",
		"bridge: inference aborted by user",
		"inference cancelled",
		"STREAM INFERENCE CANCELLED (CODE -100)", // регистр не важен: текст идёт из двух слоёв
	}
	for _, s := range positives {
		if !isUserCancelDoneChunkR354(s) {
			t.Errorf("текст %q не опознан как отмена пользователем — клиент получит "+
				"done_reason=error вместо cancelled", s)
		}
	}

	negatives := []string{
		"",
		"   ",
		"code 2: requested n_ctx=128000 exceeds model's effective n_ctx=32768",
		"upstream returned HTTP 500",
		"context deadline exceeded",
	}
	for _, s := range negatives {
		if isUserCancelDoneChunkR354(s) {
			t.Errorf("текст %q ошибочно принят за отмену пользователем — настоящая ошибка "+
				"будет замаскирована под штатное завершение", s)
		}
	}
}

// TestR354_NormalizeCancelDoneChunk — кадр отмены приводится к контракту.
func TestR354_NormalizeCancelDoneChunk(t *testing.T) {
	line := `{"created_at":"2026-10-31T09:54:15Z","done":true,"done_reason":"error",` +
		`"error":"stream inference cancelled (code -100): bridge: inference aborted by user (BRIDGE_ERR_ABORTED)",` +
		`"model":"gemma-4-E4B-it-Q4_K_M","response":""}`

	out, changed := normalizeCancelDoneChunkR354(line)
	if !changed {
		t.Fatal("кадр отмены не нормализован — клиент снова увидит done_reason=error")
	}

	var chunk map[string]interface{}
	if err := json.Unmarshal([]byte(out), &chunk); err != nil {
		t.Fatalf("результат не JSON: %v (%s)", err, out)
	}
	if reason, _ := chunk["done_reason"].(string); reason != string(DoneReasonCancelled) {
		t.Errorf("done_reason = %q, want %q (реестр R328)", reason, DoneReasonCancelled)
	}
	if _, has := chunk["error"]; has {
		t.Errorf("поле error осталось: клиент покажет ошибку вместо штатной остановки (%s)", out)
	}
	if done, _ := chunk["done"].(bool); !done {
		t.Error("done больше не true: терминатор обязан закрывать ход")
	}
	if model, _ := chunk["model"].(string); model != "gemma-4-E4B-it-Q4_K_M" {
		t.Errorf("model потерян: %q", model)
	}
	if created, _ := chunk["created_at"].(string); created != "2026-10-31T09:54:15Z" {
		t.Errorf("created_at потерян: %q", created)
	}
}

// TestR354_RealErrorNotMasked — настоящая ошибка остаётся ошибкой.
func TestR354_RealErrorNotMasked(t *testing.T) {
	line := `{"done":true,"done_reason":"error","model":"m",` +
		`"error":"code 2: requested n_ctx=128000 exceeds model's effective n_ctx=32768"}`

	out, changed := normalizeCancelDoneChunkR354(line)
	if changed {
		t.Fatalf("настоящая ошибка переписана в отмену: %s (клиент и оператор потеряют причину)", out)
	}
	if out != line {
		t.Errorf("строка изменена без признаков отмены: %s", out)
	}
}

// TestR354_NonTerminalAndNonJSONUntouched — чужие кадры не трогаем.
func TestR354_NonTerminalAndNonJSONUntouched(t *testing.T) {
	cases := []string{
		`{"done":false,"model":"m","response":"фрагмент"}`,
		`{"model":"m","response":"без done"}`,
		`not json at all`,
		`{"done":true,"done_reason":"stop","model":"m","response":"ok"}`,
	}
	for _, line := range cases {
		out, changed := normalizeCancelDoneChunkR354(line)
		if changed || out != line {
			t.Errorf("кадр %q изменён, хотя признаков отмены нет: %q", line, out)
		}
	}
}

// TestR354_NormalizeIsIdempotent — повторная нормализация уже нормализованного кадра.
func TestR354_NormalizeIsIdempotent(t *testing.T) {
	line := `{"done":true,"done_reason":"error","model":"m",` +
		`"error":"bridge: inference aborted by user (BRIDGE_ERR_ABORTED)"}`
	first, changed := normalizeCancelDoneChunkR354(line)
	if !changed {
		t.Fatal("первая нормализация не сработала")
	}
	second, changedAgain := normalizeCancelDoneChunkR354(first)
	if changedAgain {
		t.Errorf("вторая нормализация снова изменила кадр: %q → %q", first, second)
	}
	if second != first {
		t.Errorf("идемпотентность нарушена: %q != %q", second, first)
	}
}
