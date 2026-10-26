// auto_continue_r187_test.go — R187 (2026-10-20).
//
// ЖИВОЙ СИМПТОМ (Cline + Qwen3.8): клиент показывает «completed», затем
// появляются размышления, затем поток встаёт и сессия не закрывается. Причина —
// автопродолжение срабатывало на НОРМАЛЬНОМ конце агентского хода: клиент уже
// получил finish_reason, балансер тянул второй запрос генерации и дописывал
// текст после finish (или подавлял его), а [DONE] приходил с задержкой в
// десятки секунд или не приходил.
package balancer

import "testing"

func TestR187_AgentTurnForbidsAutoContinue(t *testing.T) {
	cases := []struct {
		name      string
		tools     bool
		responseTC int
		want      bool
	}{
		{"обычный текстовый клиент", false, 0, true},
		{"в запросе tools[] — агентский ход", true, 0, false},
		{"в ответе tool_calls — ход закончился вызовом", false, 1, false},
		{"и tools[], и tool_calls", true, 2, false},
	}
	for _, c := range cases {
		if got := AutoContinueAllowedForTurn(c.tools, c.responseTC); got != c.want {
			t.Errorf("%s: allowed=%v, ожидалось %v", c.name, got, c.want)
		}
	}
}
