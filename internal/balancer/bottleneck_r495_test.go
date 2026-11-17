//go:build llama_stub

// bottleneck_r495_test.go — R495 шаг S2 (2026-11-18).
//
// ЗАЧЕМ. Классификатор — это то место, где «наблюдаемость» превращается в
// утверждение о реальности: memory-bound карта больше не должна выглядеть
// свободной. Цена ошибки высока в обе стороны, поэтому проверяются не только
// вердикты, но и ПРАВИЛА, из-за которых можно навредить:
//
//   - unknown НЕ наказание: без данных (старый агент, N/A у драйвера) вердикт не
//     должен ни повышать, ни понижать выбор — иначе старая сборка агента начнёт
//     получать штраф (тест §8 п.4 плана);
//   - гистерезис: одиночный выброс не меняет вердикт, серия из K — меняет
//     (иначе на границе порога дёргается и score, и подсказки, §4.1 п.1);
//   - ничего не решаем: при `off` вердикт не считается, при `shadow` публикуется
//     и не влияет (S3 отдельным коммитом).
package balancer

import (
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// TestR495_ClassifyVerdicts — 6 вердиктов §4.1 на числах из таблицы плана.
func TestR495_ClassifyVerdicts(t *testing.T) {
	cases := []struct {
		name string
		in   BottleneckInputR495
		want BottleneckKindR495
	}{
		{
			name: "memory: память 96 %, SM 62 %, питание 0.4",
			in: BottleneckInputR495{SM: 62, MemBusy: 96, MemBusyMeasured: true,
				PowerUsage: 60, PowerLimit: 150, ActiveRequests: 1},
			want: BottleneckMemoryR495,
		},
		{
			name: "power: SM 95 %, питание 0.98, троттлинг sw_power_cap",
			in: BottleneckInputR495{SM: 95, MemBusy: 70, MemBusyMeasured: true,
				PowerUsage: 147, PowerLimit: 150, ThrottleKnown: true, ThrottleMask: 4, ActiveRequests: 1},
			want: BottleneckPowerR495,
		},
		{
			name: "compute: SM 94 %, память 55 %",
			in: BottleneckInputR495{SM: 94, MemBusy: 55, MemBusyMeasured: true,
				PowerUsage: 80, PowerLimit: 150, ActiveRequests: 2},
			want: BottleneckComputeR495,
		},
		{
			name: "stall: SM 20 %, память 25 %, но запрос идёт",
			in: BottleneckInputR495{SM: 20, MemBusy: 25, MemBusyMeasured: true,
				PowerUsage: 40, PowerLimit: 150, ActiveRequests: 1},
			want: BottleneckStallR495,
		},
		{
			name: "idle: SM 2 %, запросов нет",
			in:   BottleneckInputR495{SM: 2, MemBusy: 3, MemBusyMeasured: true, ActiveRequests: 0},
			want: BottleneckIdleR495,
		},
		{
			// Граница порога: 60 % памяти — это НЕ memory-bound (порог §4.1 — 85).
			// Без этого случая понижение порога до 50 % не краснит ни один тест:
			// существующие случаи далеки от границы.
			name: "граница: память 60 % при SM 62 % — ещё не memory",
			in: BottleneckInputR495{SM: 62, MemBusy: 60, MemBusyMeasured: true,
				PowerUsage: 60, PowerLimit: 150, ActiveRequests: 1},
			want: BottleneckUnknownR495,
		},
		{
			name: "unknown: данных нет (старый агент)",
			in:   BottleneckInputR495{SM: 0, MemBusy: 0, MemBusyMeasured: false, ActiveRequests: 1},
			want: BottleneckUnknownR495,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyBottleneckR495(c.in); got != c.want {
				t.Fatalf("вердикт = %q, ожидалось %q (вход: %+v)", got, c.want, c.in)
			}
		})
	}
}

// TestR495_UnknownIsNotPunishment — карта без измерений не получает вердикта
// вообще, даже если SM=0 при активных запросах («нечего показывать» ≠ «плохо»).
func TestR495_UnknownIsNotPunishment(t *testing.T) {
	in := BottleneckInputR495{SM: 0, MemBusyMeasured: false, ActiveRequests: 3}
	if got := classifyBottleneckR495(in); got != BottleneckUnknownR495 {
		t.Fatalf("без данных вердикт = %q, ожидалось unknown", got)
	}
}

// TestR495_Hysteresis — одиночный выброс не меняет вердикт, серия из K меняет.
func TestR495_Hysteresis(t *testing.T) {
	tr := newBottleneckTrackerR495()
	now := time.Now()

	idle := BottleneckInputR495{SM: 2, MemBusy: 3, MemBusyMeasured: true}
	// Первый вердикт публикуется сразу: подтверждать нечего.
	state, changed := tr.update("b1", idle, false, now)
	if state.Kind != BottleneckIdleR495 || changed {
		t.Fatalf("первый вердикт = %q (changed=%v), ожидалось idle без «смены»", state.Kind, changed)
	}

	// Один выброс memory — вердикт прежний.
	memory := BottleneckInputR495{SM: 62, MemBusy: 96, MemBusyMeasured: true, ActiveRequests: 1}
	if st, ch := tr.update("b1", memory, false, now); st.Kind != BottleneckIdleR495 || ch {
		t.Fatalf("одиночный выброс сменил вердикт на %q (changed=%v): гистерезиса нет", st.Kind, ch)
	}

	// Серия K=2 — вердикт меняется и это видно как «смена» (для лога).
	st, ch := tr.update("b1", memory, false, now)
	if st.Kind != BottleneckMemoryR495 || !ch {
		t.Fatalf("после K подряд вердикт = %q (changed=%v), ожидалось memory со сменой", st.Kind, ch)
	}
	if st.MemBusy != 96 || st.SM != 62 {
		t.Errorf("в состоянии нет чисел решения: %+v — оператор не поймёт, почему memory", st)
	}
}

// TestR495_UnknownDoesNotResetVerdict — пропажа данных не должна выглядеть как
// «стало хорошо»: подтверждённый memory остаётся.
func TestR495_UnknownDoesNotResetVerdict(t *testing.T) {
	tr := newBottleneckTrackerR495()
	now := time.Now()
	memory := BottleneckInputR495{SM: 62, MemBusy: 96, MemBusyMeasured: true, ActiveRequests: 1}
	tr.update("b1", memory, false, now)
	tr.update("b1", memory, false, now)

	unknown := BottleneckInputR495{SM: 0, MemBusyMeasured: false, ActiveRequests: 1}
	if st, ch := tr.update("b1", unknown, false, now); st.Kind != BottleneckMemoryR495 || ch {
		t.Fatalf("пропажа данных сменила вердикт на %q (changed=%v), ожидалось сохранение memory", st.Kind, ch)
	}
}

// TestR495_ModeDefaultShadow — дефолт S2: shadow (диагноз есть, решения прежние).
func TestR495_ModeDefaultShadow(t *testing.T) {
	cases := []struct {
		env  string
		want BottleneckModeR495
	}{
		{"", BottleneckModeShadowR495},
		{"off", BottleneckModeOffR495},
		{"shadow", BottleneckModeShadowR495},
		{"on", BottleneckModeOnR495},
		{"banana", BottleneckModeShadowR495}, // неизвестное → shadow, не молчание
	}
	for _, c := range cases {
		resetBottleneckModeR495()
		t.Setenv(bottleneckEnvR495, c.env)
		if got := bottleneckModeFromEnvR495(); got != c.want {
			t.Errorf("LB_GPU_BOTTLENECK=%q → %q, ожидалось %q", c.env, got, c.want)
		}
	}
	resetBottleneckModeR495()
}

// TestR495_OffPublishesNothing — при off в /api/metrics вердиктов нет вообще:
// включение должно быть осознанным шагом, а не «незаметным улучшением».
func TestR495_OffPublishesNothing(t *testing.T) {
	resetBottleneckModeR495()
	t.Cleanup(func() { resetBottleneckModeR495() })
	t.Setenv(bottleneckEnvR495, "off")

	p := &Proxy{metricsMgr: NewMetricsManager()}
	p.metricsMgr.metrics["b1"] = &types.BackendMetrics{
		GPU: types.GPUMetrics{UsagePercent: 62, MemBusyPercent: 96},
	}
	if snap := p.bottleneckSnapshotR495(); snap != nil {
		t.Fatalf("при off опубликованы вердикты: %+v", snap)
	}
}

// TestR495_ShadowPublishesVerdict — при shadow вердикт есть в снимке, с числами.
func TestR495_ShadowPublishesVerdict(t *testing.T) {
	resetBottleneckModeR495()
	t.Cleanup(func() { resetBottleneckModeR495() })
	t.Setenv(bottleneckEnvR495, "shadow")

	p := &Proxy{metricsMgr: NewMetricsManager()}
	p.metricsMgr.metrics["b1"] = &types.BackendMetrics{
		GPU: types.GPUMetrics{UsagePercent: 62, MemBusyPercent: 96, PowerUsage: 60, PowerLimit: 150},
	}
	p.observeBottlenecksR495()
	snap := p.bottleneckSnapshotR495()
	st, ok := snap["b1"]
	if !ok {
		t.Fatalf("при shadow вердикта нет: %+v", snap)
	}
	if st.Kind != BottleneckMemoryR495 {
		t.Fatalf("вердикт = %q, ожидалось memory (SM 62, память 96)", st.Kind)
	}
	if st.Since == "" {
		t.Error("нет since: оператор не отличит «только что» от «висит час»")
	}
}

// TestR495_InputFromMetrics — маппинг метрик в правило не теряет «измерено ли».
func TestR495_InputFromMetrics(t *testing.T) {
	gpu := types.GPUMetrics{UsagePercent: 55, MemBusyPercent: 0, PowerUsage: 100, PowerLimit: 150,
		ThrottleKnown: true, ThrottleReasons: 8}
	in := bottleneckInputFromMetricsR495(gpu, 2)
	if in.MemBusyMeasured {
		t.Error("MemBusyMeasured=true при нулевой занятости памяти: отсутствие данных выдано за измерение")
	}
	if !in.ThrottleKnown || in.ThrottleMask != 8 {
		t.Errorf("троттлинг потерян в маппинге: known=%v mask=%d", in.ThrottleKnown, in.ThrottleMask)
	}
	if in.ActiveRequests != 2 {
		t.Errorf("ActiveRequests = %d, ожидалось 2", in.ActiveRequests)
	}
}
