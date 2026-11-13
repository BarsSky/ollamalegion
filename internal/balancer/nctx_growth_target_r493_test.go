package balancer

// nctx_growth_target_r493_test.go — R493-12h: цель роста окна.
//
// Все числа в таблице — из архива второго стенда 09:53 (образы r261113-0822 /
// gpu-r261112-2146, Qwen3.8-27B). Проверяются ровно те исходы, которые в живом
// прогоне стоили 200+ секунд простоя на каждую перезагрузку.

import "testing"

func TestR493_GrowthTarget(t *testing.T) {
	cases := []struct {
		name    string
		needed  int
		loaded  int
		ceiling int
		want    int
		grow    bool
	}{
		// ── Главное: перезагрузка ВНИЗ исчезает ──────────────────────────────
		{
			name:   "архив 09:50:20: загружено 150016, просят 149809 — роста нет",
			needed: 149809, loaded: 150016,
			want: 0, grow: false,
		},
		{
			name:   "A10-126: загружено 160768, просят 160674 — роста нет",
			needed: 160674, loaded: 160768,
			want: 0, grow: false,
		},
		{
			name:   "просят ровно столько же",
			needed: 131072, loaded: 131072,
			want: 0, grow: false,
		},

		// ── Рост ступенью, а не «+1 токен» ───────────────────────────────────
		{
			name:   "архив 08:45: загружено 131072, просят 137109 → ступень 8k",
			needed: 137109, loaded: 131072,
			want: 139264, grow: true,
		},
		{
			name:   "архив: загружено 137216, просят 143671 → следующая ступень",
			needed: 143671, loaded: 137216,
			want: 147456, grow: true,
		},
		{
			name:   "рост с неизвестным загруженным окном тоже кратен ступени",
			needed: 137109, loaded: 0,
			want: 139264, grow: true,
		},

		// ── Потолок ──────────────────────────────────────────────────────────
		{
			// Нужное окно выше потолка: перезагрузка на потолок запрос всё равно
			// не обслужит, а стоит выгрузки 27B. Прежнее правило отказывало так
			// же — оно сохранено.
			name:   "нужно больше потолка — роста нет",
			needed: 300000, loaded: 131072, ceiling: 262144,
			want: 0, grow: false,
		},
		{
			name:   "потолок не больше загруженного — роста нет",
			needed: 262000, loaded: 262144, ceiling: 262144,
			want: 0, grow: false,
		},
		{
			// Ступень (8k) вывела бы цель за потолок — режем ровно до потолка,
			// выровненного вниз по 256 (воркер выравнивает вверх, и окно вышло бы
			// за объявленный предел).
			name:   "ступень выше потолка — цель урезана до потолка",
			needed: 137100, loaded: 131072, ceiling: 137216,
			want: 137216, grow: true,
		},
		{
			name:   "цель урезана до потолка, но он выше загруженного",
			needed: 200000, loaded: 196608, ceiling: 200192,
			want: 200192, grow: true,
		},

		// ── Нет данных для решения ───────────────────────────────────────────
		{"нужно 0", 0, 131072, 0, 0, false},
		{"нужно отрицательное", -1, 131072, 0, 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, grow := growthTargetR493(tc.needed, tc.loaded, tc.ceiling)
			if grow != tc.grow {
				t.Fatalf("grow=%v, ожидалось %v (target=%d)", grow, tc.grow, got)
			}
			if grow && got != tc.want {
				t.Fatalf("target=%d, ожидалось %d", got, tc.want)
			}
			if grow && tc.loaded > 0 && got <= tc.loaded {
				t.Fatalf("рост до %d не больше загруженного окна %d — это и есть "+
					"перезагрузка вниз из архива", got, tc.loaded)
			}
		})
	}
}

// TestR493_GrowthTargetNeverBelowLoaded — общее свойство, не зависящее от чисел:
// ни при каком входе функция не возвращает цель меньше загруженного окна.
func TestR493_GrowthTargetNeverBelowLoaded(t *testing.T) {
	for loaded := 0; loaded <= 262144; loaded += 8192 {
		for _, needed := range []int{1, 255, 256, 257, 8191, 8192, 8193, 131071, 131072, 131073, 200000, 262144, 262145, 400000} {
			for _, ceiling := range []int{0, 131072, 262144} {
				target, grow := growthTargetR493(needed, loaded, ceiling)
				if !grow {
					continue
				}
				if loaded > 0 && target <= loaded {
					t.Fatalf("needed=%d loaded=%d ceiling=%d → target=%d: рост вниз",
						needed, loaded, ceiling, target)
				}
				if ceiling > 0 && target > ceiling {
					t.Fatalf("needed=%d loaded=%d ceiling=%d → target=%d: цель выше потолка",
						needed, loaded, ceiling, target)
				}
				if target%256 != 0 {
					t.Fatalf("needed=%d loaded=%d → target=%d: цель не выровнена по 256",
						needed, loaded, target)
				}
			}
		}
	}
}
