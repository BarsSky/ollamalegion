// capacity_echo_r119_test.go — R119 (2026-10-15).
//
// ЖИВОЙ ДЕФЕКТ (стенд). cppworker честно отдаёт `max_slots=1` при
// `enableBatchedParallel=false`, агент в логе пишет «сообщаем балансеру 1» — а
// `/api/v1/backends` показывает `maxConcurrentRequests=2 src=static`, и второй
// клиент снова ждёт молча внутри cppworker вместо честной очереди балансера.
//
// Петля (та же «эхо», что описана в R83, но её источник не был закрыт):
//
//	state.json (старое 2) → heartbeat отдаёт 2 →
//	агент кладёт 2 в a.config.MaxConcurrentRequests →
//	регистрация заявляет 2 → балансер сохраняет 2 → heartbeat снова 2 → …
//
// Тесты фиксируют правило: операторский env > фактическая способность узла >
// эхо. Тест падает до правки: заявка бралась из a.config (эхо) и 1 не доезжала.
package agent

import (
	"os"
	"strings"
	"testing"
)

// TestR119_ClaimMaxConcurrent_NodeTruthBeatsEcho — главное: эхо из heartbeat не
// должно перебивать фактическую способность узла.
func TestR119_ClaimMaxConcurrent_NodeTruthBeatsEcho(t *testing.T) {
	cases := []struct {
		name                       string
		operator, detected, echoed int
		want                       int
	}{
		{
			name:     "эхо 2 при фактической способности 1 (живой стенд)",
			detected: 1, echoed: 2,
			want: 1,
		},
		{
			name:     "операторский лимит важнее всего",
			operator: 3, detected: 1, echoed: 2,
			want: 3,
		},
		{
			name:     "нет эха — берём способность",
			detected: 2,
			want:     2,
		},
		{
			name: "узел не ответил — остаётся эхо как последний fallback",
			// 0 значит «не знаю»; лучше сказать балансеру прежнее значение, чем 0
			// (0 = «не задано» → балансер применит свой дефолт по типу бэкенда).
			echoed: 2,
			want:   2,
		},
		{
			name: "ничего не известно — 0 (балансер решит сам)",
			want: 0,
		},
		{
			name:     "способность 1 не затирается нулём эха",
			detected: 1,
			want:     1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := claimMaxConcurrent(tc.operator, tc.detected, tc.echoed); got != tc.want {
				t.Errorf("claimMaxConcurrent(%d, %d, %d) = %d, want %d",
					tc.operator, tc.detected, tc.echoed, got, tc.want)
			}
		})
	}
}

// TestR119_OperatorEnvIsReadFromEnvironment — операторский лимит читается из
// ПЕРЕМЕННОЙ, а не из a.config: в a.config то же поле перезаписывает heartbeat.
func TestR119_OperatorEnvIsReadFromEnvironment(t *testing.T) {
	t.Setenv("AGENT_MAX_CONCURRENT_REQUESTS", "3")
	if got := operatorMaxConcurrentRequests(); got != 3 {
		t.Errorf("operatorMaxConcurrentRequests() = %d, want 3 (env задан)", got)
	}

	t.Setenv("AGENT_MAX_CONCURRENT_REQUESTS", "")
	if got := operatorMaxConcurrentRequests(); got != 0 {
		t.Errorf("operatorMaxConcurrentRequests() = %d, want 0 (env не задан)", got)
	}

	for _, bad := range []string{"0", "-1", "many", "  "} {
		t.Setenv("AGENT_MAX_CONCURRENT_REQUESTS", bad)
		if got := operatorMaxConcurrentRequests(); got != 0 {
			t.Errorf("env=%q → %d, want 0 (невалидное/нулевое значение = «не задано»)", bad, got)
		}
	}
}

// TestR119_RegisterUsesClaimHelper — защита от отката: регистрация обязана
// считать заявку через claimMaxConcurrent, а не брать a.config напрямую.
func TestR119_RegisterUsesClaimHelper(t *testing.T) {
	raw, err := os.ReadFile("collector_register.go")
	if err != nil {
		t.Fatalf("read collector_register.go: %v", err)
	}
	src := string(raw)
	if !strings.Contains(src, "claimMaxConcurrent(") {
		t.Error("register() больше не использует claimMaxConcurrent — вернулась петля «эхо»: " +
			"заявка берётся из поля, которое перезаписывает heartbeat")
	}
	if strings.Contains(src, "maxConcurrent := a.config.MaxConcurrentRequests") {
		t.Error("register() снова берёт заявку напрямую из a.config.MaxConcurrentRequests " +
			"(это и есть источник эха)")
	}
}
