// balancer_register_r70_test.go — R70 (2026-09-24): cppworker сообщает балансеру
// РЕАЛЬНУЮ параллельную вместимость, а не константу.
//
// ПРОБЛЕМА: в регистрации стояло `MaxConcurrentReqs: 4`, поэтому балансер пускал
// до 4 одновременных inference-запросов туда, где cppworker обслуживает 1
// (slot manager: maxSlots=1 при n_parallel=0): остальные три блокировались внутри
// cppworker (SlotManager.Acquire), клиент ждал молча — без позиции в
// admission-очереди, без keepalive и без отражения в метриках балансера.
//
// ФИКС R70: effectiveNParallel() берёт currentConfig.DefaultNParallel (Round 12),
// безопасный минимум — 1.
//
// R119 (2026-10-15): сама по себе n_parallel — ещё не способность. Без
// batched-планировщика legacy-путь держит inst.mu на весь инференс, поэтому
// llama_decode сериализуется (живой замер стенда: второй запрос получил первый
// байт ровно тогда, когда закончился первый — 11.76 c против 12.29 c). Заявка
// «2» в этом режиме снова приводит к молчаливому ожиданию внутри cppworker.
// Поэтому при выключенном EnableBatchedParallel вместимость = 1.
package main

import (
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

// TestEffectiveNParallel_R70 — конфиг задаёт параллелизм, 0/отсутствие → 1.
func TestEffectiveNParallel_R70(t *testing.T) {
	orig := currentConfig
	defer func() { currentConfig = orig }()

	cases := []struct {
		name string
		cfg  *cppbackend.Config
		want int
	}{
		{"nil config → 1", nil, 1},
		{"default (0) → 1", &cppbackend.Config{DefaultNParallel: 0}, 1},
		{"negative → 1", &cppbackend.Config{DefaultNParallel: -3}, 1},
		{"explicit 4 → 4", &cppbackend.Config{DefaultNParallel: 4, EnableBatchedParallel: true}, 4},
		{"explicit 1 → 1", &cppbackend.Config{DefaultNParallel: 1}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			currentConfig = tc.cfg
			if got := effectiveNParallel(); got != tc.want {
				t.Errorf("effectiveNParallel() = %d, ожидалось %d", got, tc.want)
			}
		})
	}
}

// TestEffectiveNParallel_R119_BatchedRequired — R119: n_parallel без
// batched-планировщика не даёт параллелизма, поэтому заявлять её балансеру
// нельзя: он пропустит двух клиентов, а cppworker обслужит их по очереди
// (второй — молча, без заголовков очереди).
func TestEffectiveNParallel_R119_BatchedRequired(t *testing.T) {
	orig := currentConfig
	defer func() { currentConfig = orig }()

	cases := []struct {
		name  string
		cfg   *cppbackend.Config
		want  int
		whyRU string
	}{
		{
			name:  "n_parallel=2, batched OFF (бой на стенде) → 1",
			cfg:   &cppbackend.Config{DefaultNParallel: 2, EnableBatchedParallel: false},
			want:  1,
			whyRU: "legacy-путь сериализует llama_decode (inst.mu на весь инференс)",
		},
		{
			name: "n_parallel=2, batched ON → 2",
			cfg:  &cppbackend.Config{DefaultNParallel: 2, EnableBatchedParallel: true},
			want: 2,
		},
		{
			name: "n_parallel=4, batched ON → 4",
			cfg:  &cppbackend.Config{DefaultNParallel: 4, EnableBatchedParallel: true},
			want: 4,
		},
		{
			name: "n_parallel=1, batched ON → 1 (нет второго слота)",
			cfg:  &cppbackend.Config{DefaultNParallel: 1, EnableBatchedParallel: true},
			want: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			currentConfig = tc.cfg
			if got := effectiveNParallel(); got != tc.want {
				t.Errorf("effectiveNParallel() = %d, ожидалось %d (%s)", got, tc.want, tc.whyRU)
			}
		})
	}
}
