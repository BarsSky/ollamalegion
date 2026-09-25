// invariants_test.go — свойства, которые обязаны выполняться при ЛЮБЫХ входных
// данных. Именно они делают класс ошибок «формула разошлась с реальностью»
// обнаружимым автоматически, а не через полгода на живом стенде.
package memfit

import (
	"strings"
	"testing"
)

// TestInvariant_MonotonicInCtx — если n_ctx не помещается, то и любой больший не
// помещается. На этой монотонности держится вся логика «рекомендуем меньше».
func TestInvariant_MonotonicInCtx(t *testing.T) {
	ctxs := []int{1024, 8192, 32768, 65536, 131072, 262144}
	for _, kv := range []KVType{KVF16, KVQ8, KVQ4} {
		for _, b := range []Budget{budget3070(), budgetA10()} {
			refused := false
			for _, ctx := range ctxs {
				v := Evaluate(qwen38, Request{Ctx: ctx, KVType: kv}, b, DefaultPolicy())
				if refused && v.Fits() {
					t.Errorf("немонотонность: kv=%s ctx=%d снова fits после отказа\n  %s", kv, ctx, v.String())
				}
				if !v.Fits() {
					refused = true
				}
			}
		}
	}
}

// TestInvariant_SplitIsConsistentAndWithinBudget — сумма частей равна целому, а
// каждая часть не выходит за свой бюджет. Это инвариант, нарушение которого
// означало бы «одна и та же память посчитана дважды».
func TestInvariant_SplitIsConsistentAndWithinBudget(t *testing.T) {
	for _, kv := range []KVType{KVF16, KVQ8, KVQ4} {
		for _, b := range []Budget{budget3070(), budgetA10()} {
			for _, ctx := range []int{4096, 32768, 65536} {
				v := Evaluate(qwen38, Request{Ctx: ctx, KVType: kv}, b, DefaultPolicy())

				if got := v.GPUWeights + v.CPUWeights; got != v.Weights {
					t.Errorf("веса не сходятся: gpu+cpu=%s, weights=%s (kv=%s ctx=%d)", got, v.Weights, kv, ctx)
				}
				if got := v.GPUKV + v.CPUKV; got != v.KVTotal {
					t.Errorf("KV не сходится: gpu+cpu=%s, kv_total=%s (kv=%s ctx=%d)", got, v.KVTotal, kv, ctx)
				}
				if !v.Fits() {
					continue
				}
				if v.GPUWeights+v.GPUKV > v.UsableVRAM {
					t.Errorf("GPU-часть %s превышает доступную VRAM %s\n  %s",
						v.GPUWeights+v.GPUKV, v.UsableVRAM, v.String())
				}
				if v.CPUWeights+v.CPUKV > v.UsableRAM {
					t.Errorf("CPU-часть %s превышает доступную RAM %s\n  %s",
						v.CPUWeights+v.CPUKV, v.UsableRAM, v.String())
				}
				// Стадия обязана соответствовать числу слоёв на GPU.
				switch {
				case v.GPULayers == 0 && v.Stage != StageCPUOnly:
					t.Errorf("gpu_layers=0, но stage=%s", v.Stage)
				case v.GPULayers > 0 && v.GPULayers < v.TotalLayers && v.Stage != StagePartial:
					t.Errorf("частичный оффлоад (%d/%d), но stage=%s", v.GPULayers, v.TotalLayers, v.Stage)
				}
			}
		}
	}
}

// TestInvariant_CeilingsMatchEvaluate — потолки и вердикт обязаны быть согласованы:
// на n_ctx = потолок помещается, на потолок+1 — уже нет. Именно это свойство
// означает, что «рекомендуем n_ctx ≤ X» и «отказываем при n_ctx > X» считаются по
// одной формуле, а не по двум разным (как было: гейт по RAM, совет по VRAM).
func TestInvariant_CeilingsMatchEvaluate(t *testing.T) {
	p := DefaultPolicy()
	checked := 0
	// Проверяем на обоих бюджетах: на A10 потолок упирается в обучающий контекст
	// модели (тогда случай пропускается), на 3070 — в железо.
	for _, b := range []Budget{budgetA10(), budget3070()} {
		for _, kv := range []KVType{KVF16, KVQ8, KVQ4} {
			exact, hard := Ceilings(qwen38, kv, b, p)
			if exact > hard {
				t.Errorf("kv=%s: exact=%d > hard=%d", kv, exact, hard)
			}
			if hard <= 0 || (qwen38.TrainCtx > 0 && hard >= qwen38.TrainCtx) {
				continue // потолок ограничен моделью, а не железом — проверяется отдельно
			}
			if v := Evaluate(qwen38, Request{Ctx: hard, KVType: kv}, b, p); !v.Fits() {
				t.Errorf("kv=%s: n_ctx=потолок(%d) обязан помещаться\n  %s", kv, hard, v.String())
			}
			if v := Evaluate(qwen38, Request{Ctx: hard + 1, KVType: kv}, b, p); v.Fits() {
				t.Errorf("kv=%s: n_ctx=потолок+1(%d) не должен помещаться\n  %s", kv, hard+1, v.String())
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("ни одного случая не проверено — тест выродился, надо пересмотреть бюджеты")
	}
}

// TestInvariant_KVOrdering — квантизация KV обязана уменьшать объём строго:
// q4_0 < q8_0 < f16. Ловит ситуацию, когда тип KV «принимается», но в расчёт не
// попадает (именно так жил хардкод 256 байт/токен).
func TestInvariant_KVOrdering(t *testing.T) {
	f16 := KVBytesPerToken(qwen38, KVF16)
	q8 := KVBytesPerToken(qwen38, KVQ8)
	q4 := KVBytesPerToken(qwen38, KVQ4)
	if !(q4 < q8 && q8 < f16) {
		t.Fatalf("нарушен порядок: q4=%s q8=%s f16=%s", q4, q8, f16)
	}
	if q8.Scale(2, 1) < f16 || q4.Scale(4, 1) < f16 {
		t.Errorf("q8_0 должен быть примерно вдвое, q4_0 — вчетверо меньше f16: q4=%s q8=%s f16=%s", q4, q8, f16)
	}
}

// TestInvariant_UsableNeverExceedsAvailable — санитарная проверка бюджета.
func TestInvariant_UsableNeverExceedsAvailable(t *testing.T) {
	b := budget3070()
	p := DefaultPolicy()
	if got, max := b.UsableRAM(p), b.RAMAvail.Sub(b.RAMReserve); got > max {
		t.Errorf("UsableRAM=%s больше доступного минус резерв (%s)", got, max)
	}
	if got, max := b.UsableVRAM(p), b.VRAMFree.Sub(b.VRAMReserve); got > max {
		t.Errorf("UsableVRAM=%s больше свободного минус резерв (%s)", got, max)
	}
}

// TestProbeRAM_EnvOverride — ридер системной памяти один на проект и слушается
// явного override (нужен и тестам, и оператору на нестандартных стендах).
func TestProbeRAM_EnvOverride(t *testing.T) {
	const sixteenGiB = 17179869184
	t.Setenv("CPPWORKER_AVAILABLE_RAM_BYTES", "17179869184")
	pr := ProbeRAM()
	if !pr.Known || int64(pr.Available) != sixteenGiB {
		t.Errorf("ProbeRAM с override: known=%v available=%s, want %d байт", pr.Known, pr.Available, sixteenGiB)
	}
	if !strings.Contains(pr.Source, "env") {
		t.Errorf("источник обязан указывать env, получено %q", pr.Source)
	}

	// Некорректное значение нельзя принимать за правду (в прежнем коде оно тихо
	// превращалось в 0 = «проверку RAM пропускаем», то есть в fail-open).
	t.Setenv("CPPWORKER_AVAILABLE_RAM_BYTES", "not-a-number")
	pr2 := ProbeRAM()
	if pr2.Known && strings.Contains(pr2.Source, "env") {
		t.Errorf("некорректный override принят как источник: %q", pr2.Source)
	}
}
