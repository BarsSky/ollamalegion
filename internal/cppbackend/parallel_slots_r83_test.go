// parallel_slots_r83_test.go — R83 (2026-09-29).
//
// ЖАЛОБА ОПЕРАТОРА: «первый кто отправил получил ответ, второй получил ошибку
// 503 и то что модель не загрузить... надо проверить на малых моделях чтобы
// иметь возможность выделить кеш под несколько запросов, тестово хватит 2 в
// параллель».
//
// ПРИЧИНА (найдена на живой стойке): параллельность вычислялась ДВУМЯ разными
// правилами.
//   - C-bridge (llama_context_params.n_seq_max) получал значение из
//     resolveNParallel: явный opts.Parallel, иначе env CPPWORKER_N_PARALLEL.
//   - Go SlotManager получал просто max(1, opts.Parallel) — env не учитывался.
//
// При CPPWORKER_N_PARALLEL=2 получалось: llama.cpp выделяет KV-cache под 2
// последовательности и cppworker объявляет балансеру вместимость 2
// (cmd/cppworker/balancer_register.go: effectiveNParallel), но SlotManager
// пропускает по одному запросу — второй клиент молча висел в Acquire и
// получал 503 по таймауту. Ниже зафиксировано единое правило.
package cppbackend

import "testing"

// TestResolveNParallel_R83 — per-model запрос всегда побеждает env-дефолт,
// env учитывается при отсутствии запроса, «ничего не задано» остаётся нулём
// (ноль = оставить внутренний дефолт llama.cpp, а не «ноль слотов»).
func TestResolveNParallel_R83(t *testing.T) {
	tests := []struct {
		name       string
		perModel   int
		cfgDefault int
		want       int
	}{
		{"ничего не задано → 0 (дефолт bridge)", 0, 0, 0},
		{"только env → env", 0, 2, 2},
		{"только запрос → запрос", 3, 0, 3},
		{"запрос побеждает env", 4, 2, 4},
		{"env=1 остаётся единицей", 0, 1, 1},
		{"отрицательный запрос игнорируется", -1, 2, 2},
		{"отрицательный env игнорируется", 0, -5, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveNParallel(tc.perModel, tc.cfgDefault); got != tc.want {
				t.Errorf("resolveNParallel(%d, %d) = %d, want %d",
					tc.perModel, tc.cfgDefault, got, tc.want)
			}
		})
	}
}

// TestParallelSlotsFor_R83 — SlotManager никогда не получает < 1 слота:
// «не задано» — это одиночный слот (поведение Round 8), а не ноль.
func TestParallelSlotsFor_R83(t *testing.T) {
	tests := []struct {
		requested int
		want      int
	}{
		{0, 1},
		{-3, 1},
		{1, 1},
		{2, 2},
		{8, 8},
	}
	for _, tc := range tests {
		if got := parallelSlotsFor(tc.requested); got != tc.want {
			t.Errorf("parallelSlotsFor(%d) = %d, want %d", tc.requested, got, tc.want)
		}
	}
}

// TestResolveNParallel_EnvAndSlotsAgree_R83 — главное утверждение фикса:
// число, ушедшее в C-bridge, и число слотов SlotManager совпадают при любом
// источнике настройки. Именно расхождение этих двух значений давало «второй
// клиент получает 503, хотя воркер объявил 2 параллельных запроса».
func TestResolveNParallel_EnvAndSlotsAgree_R83(t *testing.T) {
	cases := []struct {
		name       string
		perModel   int
		cfgDefault int
	}{
		{"оба не заданы", 0, 0},
		{"env задаёт 2", 0, 2},
		{"профиль задаёт 2", 2, 0},
		{"профиль 3 побеждает env 2", 3, 2},
		{"env 4 побеждает отсутствие запроса", 0, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bridge := resolveNParallel(tc.perModel, tc.cfgDefault)
			slots := parallelSlotsFor(resolveNParallel(tc.perModel, tc.cfgDefault))

			// bridge==0 означает «оставить дефолт llama.cpp», а он равен 1.
			bridgeEffective := bridge
			if bridgeEffective < 1 {
				bridgeEffective = 1
			}
			if slots != bridgeEffective {
				t.Errorf("SlotManager=%d, а C-bridge получает n_parallel=%d (эффективно %d) — "+
					"балансер будет пускать столько запросов, сколько объявит воркер, "+
					"тогда как слот-менеджер пропустит только %d",
					slots, bridge, bridgeEffective, slots)
			}
		})
	}
}
