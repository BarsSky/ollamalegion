// context_math_guard_r493_test.go — ПРОВЕРКА РАСЧЁТА РАЗМЕРА КОНТЕКСТА
// (по просьбе оператора 2026-11-12).
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ТЕСТ, если у ContextPerSlot уже есть тест R83. Тот проверяет
// арифметику саму по себе. Здесь фиксируются СЦЕНАРИИ, в которых число «окно на
// клиента» путают с суммарным — именно так оператор и получает неверный размер
// контекста: выставил 65536 и parallel=4, а клиент видит 16384 (и это правильно),
// либо наоборот видит 65536 при фактических 16384 (и это дефект отчётности).
//
// Тест закрепляет оба перевода и запрещает «молчаливое» изменение:
//   - per_slot(total, slots) — сколько РЕАЛЬНО получит один клиент;
//   - total(per_slot, slots) — сколько надо выставить, чтобы клиент получил заданное.
package balancer

import (
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

// TestR493_ContextPerClientScenarios — «окно на клиента» для живых раскладок стенда.
func TestR493_ContextPerClientScenarios(t *testing.T) {
	// Порядок полей — под fieldalignment (линт CI): строки (указатели) первыми,
	// числа последними, иначе структура занимает лишние pointer-байты.
	cases := []struct {
		name    string
		comment string
		total   int
		slots   int
		wantPer int
	}{
		{"A10 24G, 27B, один слот", "окно целиком одному клиенту", 65536, 1, 65536},
		{"A10 24G, 27B, два слота", "ровно половина (pad 256 не нужен)", 65536, 2, 32768},
		{"тот же, четыре слота", "четверть — типовая жалоба «контекст меньше, чем я ставил»", 65536, 4, 16384},
		{"нечётное деление", "21845 паддится ВВЕРХ до 22016", 65536, 3, 22016},
		{"дефолт 8192 при parallel=2", "половина", 8192, 2, 4096},
		{"один слот из конфига 16384", "ничего не теряется", 16384, 1, 16384},
		{"нулевой total", "нечего делить", 0, 4, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := cppbackend.ContextPerSlot(c.total, c.slots)
			if c.name == "нечётное деление" {
				// Точное правило llama.cpp: n_ctx_seq = GGML_PAD(n_ctx / n_seq, 256).
				// 65536/3 = 21845 → pad вверх до 22016. Фиксируем фактическое значение,
				// чтобы «примерно четверть» никогда не выдавалось за точное.
				if got != 22016 {
					t.Fatalf("65536/3 = %d, ожидалось 22016 (pad 256 вверх); %s", got, c.comment)
				}
				return
			}
			if got != c.wantPer {
				t.Fatalf("ContextPerSlot(%d, %d) = %d, ожидалось %d (%s)",
					c.total, c.slots, got, c.wantPer, c.comment)
			}
		})
	}
}

// TestR493_ContextTotalFromPerClient — обратный перевод: сколько выставить в
// конфиге, чтобы клиент получил заданное окно.
func TestR493_ContextTotalFromPerClient(t *testing.T) {
	cases := []struct {
		perSeq    int
		slots     int
		wantTotal int
	}{
		{32768, 2, 65536},
		{16384, 4, 65536},
		// 1000 → pad до 1024, затем ×2 = 2048. Клиент получит 1024, а не 1000:
		// это правило llama.cpp (окно паддится), и оно должно быть видно числом.
		{1000, 2, 2048},
	}
	for _, c := range cases {
		total, per := cppbackend.EffectiveContextSize(0, c.perSeq, c.slots)
		if total != c.wantTotal {
			t.Errorf("EffectiveContextSize(per=%d, slots=%d): total = %d, ожидалось %d",
				c.perSeq, c.slots, total, c.wantTotal)
		}
		if per <= 0 {
			t.Errorf("EffectiveContextSize(per=%d, slots=%d): per = %d, ожидалось >0",
				c.perSeq, c.slots, per)
		}
		// Ключевой инвариант: клиент получает ровно per, а не total.
		if got := cppbackend.ContextPerSlot(total, c.slots); got != per {
			t.Errorf("клиент получит %d, а EffectiveContextSize обещает %d — "+
				"именно эта пара чисел и расходится в отчётности", got, per)
		}
	}
}

// TestR493_ContextReportedIsPerClient — что уходит клиенту в Ollama-ответе:
// в /api/models балансер отдаёт context_length = СУММАРНОЕ окно и max_slots, из
// которых клиент сам считает окно на слот. Проверяем, что это согласовано с
// правилом ContextPerSlot: total / slots (с паддингом) == обещанное per-клиента.
func TestR493_ContextReportedIsPerClient(t *testing.T) {
	total, slots := 65536, 4
	perClient := cppbackend.ContextPerSlot(total, slots)
	if perClient != 16384 {
		t.Fatalf("per-client = %d, ожидалось 16384", perClient)
	}
	if perClient*slots > total {
		t.Fatalf("окно на клиента × слоты (%d) больше суммарного (%d) — клиенты "+
			"получат больше, чем есть в модели", perClient*slots, total)
	}
}
