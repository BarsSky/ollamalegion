//go:build llama_stub

// model_copy_window_r118_test.go — R118.5 (2026-10-14).
//
// ЖИВОЙ ДЕФЕКТ (нашла сценарная проверка на стенде). Одна и та же модель была
// загружена на двух бэкендах:
//
//	мастер cppworker-gpu-bundled-agent : total 32768, 16K на слот, роль main
//	донор  cppworker-cyborg-bundled-agent: total 8192,  4K на слот, роль donor
//
// Клиент прислал num_ctx=8192. Балансер выбирал копию «наименее загруженную» —
// донор простаивал, и запрос ушёл на НЕГО:
//
//	num_ctx decision backend=cppworker-cyborg-bundled-agent
//	окно модели суммарное/на клиента: total_n_ctx=8192 context_per_seq=4096
//	detected n_ctx mismatch loaded_n_ctx=4096 requested_n_ctx=8192 → reload → 422
//	клиент: +33 с и окно меньше заготовленного оператором
//
// Тесты фиксируют правило выбора копии:
//  1. покрывающая окно копия важнее «свободной»;
//  2. при нескольких покрывающих — главный бэкенд;
//  3. если никто не покрывает — берём копию с САМЫМ БОЛЬШИМ окном;
//  4. без запрошенного окна (num_ctx не задан) — прежнее поведение (решение не
//     принимаем здесь);
//  5. окно на слот считается и без context_per_seq (делим суммарное на слоты).
package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

const (
	winMain  = "cppworker-gpu-bundled-agent"
	winDonor = "cppworker-cyborg-bundled-agent"
	winModel = "gemma-4-E4B-it-Q4_K_M"
)

// newCopyWindowProxy — прокси с двумя ЗАГРУЖЕННЫМИ копиями модели.
//
// mainPerSeq/donorPerSeq — окно НА СЛОТ (то, что сравнивается с num_ctx клиента).
func newCopyWindowProxy(t *testing.T, mainPerSeq, donorPerSeq int, mainActive, donorActive int) *Proxy {
	t.Helper()

	mk := func(id string, role string, perSeq int, active int) (*BackendState, *types.BackendMetrics, *types.LlamaCppMetrics) {
		total := perSeq * 2 // два слота у модели
		state := &BackendState{Backend: &types.Backend{
			ID: id, Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
			MaxConcurrentReqs: 2, Role: role,
		}}
		state.ActiveReqs = active
		metrics := &types.BackendMetrics{LlamaCpp: types.LlamaCppMetrics{
			LoadedModels: []types.LlamaCppModel{{Name: winModel, State: "loaded", ContextLength: total}},
		}}
		llama := &types.LlamaCppMetrics{LoadedModels: []types.LlamaCppModel{{
			Name: winModel, State: "loaded", ContextLength: total,
			ContextPerSeq: perSeq, MaxSlots: 2, Parallel: 2,
		}}}
		return state, metrics, llama
	}

	mainState, mainMetrics, mainLlama := mk(winMain, types.BackendRoleMain, mainPerSeq, mainActive)
	donorState, donorMetrics, donorLlama := mk(winDonor, types.BackendRoleDonor, donorPerSeq, donorActive)

	p := &Proxy{
		config:     &types.LoadBalancerConfig{},
		backends:   map[string]*BackendState{winMain: mainState, winDonor: donorState},
		metricsMgr: NewMetricsManager(),
	}
	p.metricsMgr.metrics[winMain] = mainMetrics
	p.metricsMgr.metrics[winDonor] = donorMetrics
	p.metricsMgr.llamaMetrics[winMain] = mainLlama
	p.metricsMgr.llamaMetrics[winDonor] = donorLlama
	return p
}

func allowedLlama() []types.BackendType {
	return []types.BackendType{types.BackendTypeLlamaCpp}
}

// TestR118_5_CoveringCopyWinsOverIdleCopy — главный случай живой жалобы: копия
// мастера покрывает окно клиента, копия донора — нет, а свободнее именно донор.
func TestR118_5_CoveringCopyWinsOverIdleCopy(t *testing.T) {
	// Мастер занят (1 из 2 слотов), донор полностью свободен — раньше выигрывал он.
	p := newCopyWindowProxy(t, 16384, 4096, 1, 0)

	// Предпосылка теста: ПРЕЖНИЙ выбор (наименее загруженная копия) — донор с
	// окном 4096. Именно это и было живым дефектом.
	if oldID := p.findLeastLoadedBackendWithModel(winModel, allowedLlama()); oldID != winDonor {
		t.Fatalf("предпосылка неверна: прежний выбор = %q, ожидался свободный донор %q", oldID, winDonor)
	}

	id, why := p.selectModelCopyForWindow(winModel, 8192, nil, allowedLlama())
	if id != winMain {
		t.Fatalf("выбрана копия %q (причина: %s), ожидалась покрывающая окно копия мастера %q — "+
			"это и есть живой дефект: запрос уходил на свободную копию с окном 4096",
			id, why, winMain)
	}
	if why == "" {
		t.Error("причина выбора не объяснена (нужна для лога и разбора)")
	}
}

// TestR118_5_BothCovering_PreferMain — покрывают обе копии: выбираем главный
// бэкенд (его готовил оператор, и его VRAM не занята отдачей слоёв донору).
func TestR118_5_BothCovering_PreferMain(t *testing.T) {
	p := newCopyWindowProxy(t, 16384, 8192, 0, 0)

	id, _ := p.selectModelCopyForWindow(winModel, 8192, nil, allowedLlama())
	if id != winMain {
		t.Fatalf("выбрана копия %q, ожидался главный бэкенд %q", id, winMain)
	}
}

// TestR118_5_NobodyCovers_TakeLargestWindow — никто не покрывает: берём копию с
// самым большим окном (растить окно дешевле от 16K, чем от 4K), а не самую
// свободную.
func TestR118_5_NobodyCovers_TakeLargestWindow(t *testing.T) {
	p := newCopyWindowProxy(t, 16384, 4096, 1, 0)

	id, why := p.selectModelCopyForWindow(winModel, 32768, nil, allowedLlama())
	if id != winMain {
		t.Fatalf("выбрана копия %q (причина: %s), ожидалась копия с самым большим окном %q", id, why, winMain)
	}
}

// TestR118_5_NoRequestedWindow_NoDecision — клиент окно не задал: правило не
// вмешивается (прежнее поведение выбора бэкенда).
func TestR118_5_NoRequestedWindow_NoDecision(t *testing.T) {
	p := newCopyWindowProxy(t, 16384, 4096, 1, 0)
	if id, why := p.selectModelCopyForWindow(winModel, 0, nil, allowedLlama()); id != "" {
		t.Fatalf("без окна клиента выбор не должен приниматься здесь: id=%q why=%q", id, why)
	}
}

// TestR118_5_PerSeqFromTotalAndSlots — если cppworker не отдал context_per_seq,
// окно на слот считаем делением суммарного на число слотов.
func TestR118_5_PerSeqFromTotalAndSlots(t *testing.T) {
	p := newCopyWindowProxy(t, 16384, 4096, 0, 0)

	// Подменяем запись мастера: context_per_seq нет, есть max_slots=2.
	p.metricsMgr.llamaMetrics[winMain] = &types.LlamaCppMetrics{LoadedModels: []types.LlamaCppModel{{
		Name: winModel, State: "loaded", ContextLength: 32768, MaxSlots: 2,
	}}}
	perSeq, total := p.loadedPerSeqWindowFromCache(winMain, winModel)
	if total != 32768 {
		t.Errorf("суммарное окно = %d, want 32768", total)
	}
	if perSeq != 16384 {
		t.Errorf("окно на слот = %d, want 16384 (32768 / 2 слота)", perSeq)
	}
}

// TestR118_5_ModelCopyCoversWindow — проверка покрытия окна по бэкенду (её
// использует ветка привязки сессии: привязанная копия может не покрывать запрос).
func TestR118_5_ModelCopyCoversWindow(t *testing.T) {
	p := newCopyWindowProxy(t, 16384, 4096, 0, 0)

	if !p.modelCopyCoversWindow(winMain, winModel, 8192) {
		t.Error("копия с 16384 на слот обязана покрывать 8192")
	}
	if p.modelCopyCoversWindow(winDonor, winModel, 8192) {
		t.Error("копия с 4096 на слот НЕ покрывает 8192")
	}
	if !p.modelCopyCoversWindow(winDonor, winModel, 4096) {
		t.Error("копия с 4096 на слот покрывает ровно 4096")
	}
	// Клиент окно не задавал — покрытие не ограничивает.
	if !p.modelCopyCoversWindow(winDonor, winModel, 0) {
		t.Error("без запрошенного окна ограничений быть не должно")
	}
}

// TestR118_5_SelectBackendPreferWindow_FallsBackWithoutCopies — если покрывающей
// ГОТОВОЙ копии нет, решение остаётся прежнему пути (не ломаем загрузку модели).
func TestR118_5_SelectBackendPreferWindow_FallsBackWithoutCopies(t *testing.T) {
	p := newCopyWindowProxy(t, 16384, 4096, 0, 0)
	p.metricsMgr.metrics[winMain] = &types.BackendMetrics{}
	p.metricsMgr.metrics[winDonor] = &types.BackendMetrics{}

	// Модель нигде не загружена: window-путь не должен ничего выбирать сам.
	if id, why := p.selectModelCopyForWindow(winModel, 8192, nil, allowedLlama()); id != "" || why != "" {
		t.Fatalf("window-путь выбрал бэкенд без загруженной модели: id=%q why=%q", id, why)
	}
}

// TestR118_5_NoModel_NoCandidates — модель нигде не загружена: правило молчит,
// решение принимает прежний путь (загрузка на подходящий узел).
func TestR118_5_NoModel_NoCandidates(t *testing.T) {
	p := newCopyWindowProxy(t, 16384, 4096, 0, 0)
	p.metricsMgr.metrics[winMain] = &types.BackendMetrics{}
	p.metricsMgr.metrics[winDonor] = &types.BackendMetrics{}
	if cands := p.collectModelCopyCandidates(winModel, nil, allowedLlama()); len(cands) != 0 {
		t.Fatalf("кандидаты без загруженной модели: %+v", cands)
	}
	if id, _ := p.selectModelCopyForWindow(winModel, 8192, nil, allowedLlama()); id != "" {
		t.Fatalf("без загруженной модели выбор не должен приниматься: %q", id)
	}
}
