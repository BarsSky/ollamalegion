//go:build llama_stub

// client_window_policy_r83_test.go — R83-политика (2026-10-01): загрузка
// «окном клиента».
//
// Требование пользователя: «если размер указан и модель не загружена — применять
// настройки клиента; дефолтные значения поломают всю работу». Проверяем
// пересчёт единиц: num_ctx клиента — окно НА КЛИЕНТА, а n_ctx модели —
// СУММАРНОЕ (llama.cpp делит его между слотами).
package main

import "testing"

// TestR83Policy_LoadTotalNCtx_PerClientToTotal — 32768 на клиента при двух
// слотах = 65536 суммарно; при одном слоте — столько же, сколько просили.
func TestR83Policy_LoadTotalNCtx_PerClientToTotal(t *testing.T) {
	cases := []struct {
		name       string
		perClient  int
		slots      int
		wantMin    int
		wantEquals int // 0 = проверять только нижнюю границу
	}{
		{"два слота — удваиваем", 32768, 2, 65536, 65536},
		{"один слот — как просили", 32768, 1, 32768, 32768},
		{"нулевые слоты трактуем как один", 8192, 0, 8192, 8192},
		{"нестандартное окно выравнивается вверх", 1000, 2, 2048, 0},
		{"клиент молчал — 0 (берём env/профиль)", 0, 2, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := loadTotalNCtxForRequest(tc.perClient, tc.slots)
			if got < tc.wantMin {
				t.Fatalf("loadTotalNCtxForRequest(%d, %d) = %d, ожидалось >= %d",
					tc.perClient, tc.slots, got, tc.wantMin)
			}
			if tc.wantEquals != 0 && got != tc.wantEquals {
				t.Errorf("loadTotalNCtxForRequest(%d, %d) = %d, ожидалось %d",
					tc.perClient, tc.slots, got, tc.wantEquals)
			}
			// Инвариант: клиент не должен получить меньше, чем просил.
			if tc.perClient > 0 {
				perSlot := got
				if tc.slots > 1 {
					perSlot = got / tc.slots
				}
				if perSlot < tc.perClient {
					t.Errorf("на клиента приходится %d при запросе %d — окно потеряно",
						perSlot, tc.perClient)
				}
			}
		})
	}
}
