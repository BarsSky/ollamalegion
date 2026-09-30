// context_per_seq_r83_test.go — R83 (2026-09-30).
//
// ФАКТ, КОТОРЫЙ ЗДЕСЬ ЗАФИКСИРОВАН (проверен на живом стенде, c/llama.cpp
// gguf-v0.19.0-1369-g1d2869c6e, kv_unified=false по умолчанию):
//
//	src/llama-context.cpp: n_ctx_seq = GGML_PAD(n_ctx / n_seq_max, 256)
//
// то есть contextSize в API — СУММАРНОЕ окно, и слоты его ДЕЛЯТ между клиентами.
// В логе cppworker это видно буквально:
//
//	contextSize=8192, parallel=2 → llama_context: n_ctx = 8192, n_ctx_seq = 4096
//	contextSize=8192, parallel=3 → llama_context: n_ctx = 8448, n_ctx_seq = 2816
//	contextSize=8192, parallel=1 → llama_context: n_ctx = 8192, n_ctx_seq = 8192
//
// Оператор, выставив 16K и 2 слота, получал по 8K на клиента и не видел этого
// ни в API, ни в WebUI. Здесь — обе стороны решения: показать «на клиента» и
// дать способ запросить окно именно на клиента (ContextPerSeq).
package cppbackend

import "testing"

// TestR83_ContextPerSlot_MatchesLlamaCpp — повторяет правило llama.cpp.
func TestR83_ContextPerSlot_MatchesLlamaCpp(t *testing.T) {
	tests := []struct {
		name  string
		total int
		slots int
		want  int
	}{
		// Живые значения из логов llama_context (см. шапку файла).
		{"8192/1 = 8192", 8192, 1, 8192},
		{"8192/2 = 4096", 8192, 2, 4096},
		{"8192/3 → 8448/3 = 2816", 8448, 3, 2816},
		{"16384/2 = 8192", 16384, 2, 8192},
		{"16384/1 = 16384", 16384, 1, 16384},
		// Выравнивание вверх до 256 (GGML_PAD).
		{"5000/3 → PAD(1666)=1792", 5000, 3, 1792},
		{"1000/3 → PAD(333)=512", 1000, 3, 512},
		// Защита от мусора.
		{"0 слотов трактуется как 1", 4096, 0, 4096},
		{"без окна — 0", 0, 2, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ContextPerSlot(tc.total, tc.slots); got != tc.want {
				t.Errorf("ContextPerSlot(%d, %d) = %d, want %d",
					tc.total, tc.slots, got, tc.want)
			}
		})
	}
}

// TestR83_EffectiveContextSize_PerSeqWins — «окно на клиента» превращается в
// суммарный n_ctx = PAD256(per) × слотов: именно это уходит в memfit и в C-bridge.
func TestR83_EffectiveContextSize_PerSeqWins(t *testing.T) {
	tests := []struct {
		name      string
		total     int
		perSeq    int
		slots     int
		wantTotal int
		wantPer   int
	}{
		{"16384 на клиента × 2 слота = 32768", 0, 16384, 2, 32768, 16384},
		{"16384 на клиента × 1 слот = 16384", 0, 16384, 1, 16384, 16384},
		{"perSeq вытесняет суммарное", 4096, 8192, 2, 16384, 8192},
		{"выравнивание perSeq вверх", 0, 5000, 2, 10240, 5120}, // PAD(5000)=5120
		// Прежняя семантика, когда perSeq не задан.
		{"только суммарное: показываем на клиента", 16384, 0, 2, 16384, 8192},
		{"суммарное, один слот", 8192, 0, 1, 8192, 8192},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			total, per := EffectiveContextSize(tc.total, tc.perSeq, tc.slots)
			if total != tc.wantTotal {
				t.Errorf("total = %d, want %d", total, tc.wantTotal)
			}
			if per != tc.wantPer {
				t.Errorf("perSeq = %d, want %d", per, tc.wantPer)
			}
		})
	}
}

// TestR83_EffectiveContextSize_NoSlotsMultiplierWithoutPerSeq — регресс на
// прежнее (ошибочное) представление «слоты умножают KV»: при незаданном
// perSeq суммарное окно НЕ растёт с числом слотов, оно делится.
func TestR83_EffectiveContextSize_NoSlotsMultiplierWithoutPerSeq(t *testing.T) {
	total1, per1 := EffectiveContextSize(8192, 0, 1)
	total2, per2 := EffectiveContextSize(8192, 0, 2)

	if total1 != 8192 || total2 != 8192 {
		t.Errorf("суммарное окно зависит от слотов: %d и %d (ожидалось 8192 и 8192)",
			total1, total2)
	}
	if per1 != 8192 || per2 != 4096 {
		t.Errorf("окно на клиента: %d и %d (ожидалось 8192 и 4096)", per1, per2)
	}
}

// TestR83_PadContext_Step256 — выравнивание совпадает с GGML_PAD(_, 256).
func TestR83_PadContext_Step256(t *testing.T) {
	cases := map[int]int{0: 0, 1: 256, 256: 256, 257: 512, 2730: 2816, 4096: 4096, 8192: 8192}
	for in, want := range cases {
		if got := padContext(in); got != want {
			t.Errorf("padContext(%d) = %d, want %d", in, got, want)
		}
	}
}
