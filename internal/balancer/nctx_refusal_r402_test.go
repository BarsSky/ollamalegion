// nctx_refusal_r402_test.go — R402 (2026-11-05).
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА: «запрос на увеличение приходит с ошибкой, что он не
// может выделить объём больше, и сделайте вручную». Отказ по окну отправлял
// клиента «загрузить модель явно с нужным contextSize», хотя балансер уже
// пробовал вырасти сам (R400) и отказывает только когда расти некуда.
//
// Новый контракт текста (I7): назвать связывающий потолок и объяснить единицы —
// окно на слот, parallel, что слоты делят окно; дать выполнимые рычаги.
package balancer

import (
	"strings"
	"testing"
)

// TestR402_RefusalExplainsUnits — в отказе есть числа, объяснение слотов и нет
// совета «загрузить вручную».
func TestR402_RefusalExplainsUnits(t *testing.T) {
	cases := []struct {
		name                          string
		requested, operator, physical int
		wantContains                  []string
	}{
		{
			name: "связывает операторский потолок",
			requested: 65536, operator: 32768, physical: 0,
			wantContains: []string{"num_ctx=65536", "auto_reload_max_n_ctx=32768",
				"slots divide the window", "parallel=1", "declared window"},
		},
		{
			name: "связывает физика",
			requested: 262144, operator: 131072, physical: 8192,
			wantContains: []string{"max_vram_n_ctx=8192", "slots divide the window", "parallel=1"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := formatNCtxCeilingRefusal(c.requested, c.operator, c.physical, "gemma-4")
			for _, want := range c.wantContains {
				if !strings.Contains(msg, want) {
					t.Errorf("в отказе нет %q:\n%s", want, msg)
				}
			}
			if strings.Contains(msg, "load the model explicitly") {
				t.Errorf("отказ снова отправляет «загрузить вручную»:\n%s", msg)
			}
			if !strings.Contains(msg, "cannot grow it automatically") {
				t.Errorf("отказ должен прямо говорить, что автоматический рост уже невозможен:\n%s", msg)
			}
		})
	}
}
