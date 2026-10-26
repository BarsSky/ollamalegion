// handlers_chat_retry_r188_test.go — R188 (2026-10-20).
//
// Смысл: клиентский лимит длины ответа не должен превращаться в цикл полных
// перегенераций (на 27B-модели один проход — минуты, слот занят, клиент видит
// «зависание»).
package main

import "testing"

func TestR188_RetryBudgetRespectsClientCap(t *testing.T) {
	if got := emptyOutputRetryBudget(true); got != 1 {
		t.Fatalf("при клиентском лимите допускается 1 повтор, получено %d", got)
	}
	if got := emptyOutputRetryBudget(false); got != maxEmptyOutputRetries {
		t.Fatalf("без лимита бюджет должен остаться прежним (%d), получено %d", maxEmptyOutputRetries, got)
	}
}
