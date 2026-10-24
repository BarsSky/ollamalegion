// llamacpp_selection_balance_r91_test.go — R91 (2026-10-08): выбор наименее
// загруженного узла среди тех, где модель уже загружена.
//
// ЖИВОЙ ПРОГОН, из-за которого это появилось. Две машины, обе с загруженной
// gemma-4-E4B-it-Q4_K_M, ёмкость по 1 слоту (`LB_CAPACITY_FROM_MODEL_SLOTS=true`),
// 6 клиентов:
//
//	распределение запросов: cppworker-gpu-bundled-agent = 14, CPPWORKER-34 = 2
//	admission: queued 14, granted 14, waited avg 21.6 с (до 32 с), timeouts 0
//
// То есть почти всё ушло на первый узел, на нём выстроилась очередь, а второй
// узел простаивал (0 запросов). Причина: selectLlamaCppBackendForModel брал
// ПЕРВЫЙ бэкенд с моделью из карты metricsMgr.llamaMetrics (порядок обхода карты
// в Go случаен) и не смотрел на загрузку.
package balancer

import (
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// balanceTestRouter — два healthy llama.cpp бэкенда с моделью и заданной
// загрузкой. Активная нагрузка задаётся так же, как её видит боевой код:
// state.ActiveReqs и state.Backend.MaxConcurrentReqs.
func balanceTestRouter(t *testing.T, loadA, loadB int, modelOnA, modelOnB bool) *LlamaCppRouter {
	t.Helper()
	const model = "gemma-4-E4B-it-Q4_K_M"

	p := &Proxy{
		config:     &types.LoadBalancerConfig{},
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
	}
	for _, spec := range []struct {
		id     string
		load   int
		hasMod bool
	}{{"bk-a", loadA, modelOnA}, {"bk-b", loadB, modelOnB}} {
		p.backends[spec.id] = &BackendState{
			Backend: &types.Backend{
				ID:                spec.id,
				Host:              "127.0.0.1",
				Status:            types.StatusHealthy,
				Type:              types.BackendTypeLlamaCpp,
				CppWorkerPort:     18092,
				MaxConcurrentReqs: 1,
			},
			ActiveReqs: spec.load,
		}
		m := &types.BackendMetrics{
			ID:                    spec.id,
			Status:                types.StatusHealthy,
			BackendType:           types.BackendTypeLlamaCpp,
			MaxConcurrentRequests: 1,
			GPU:                   types.GPUMetrics{MemoryTotal: 8192, MemoryUsed: 1000},
			System:                types.SystemMetrics{MemoryTotal: 24576, MemoryUsed: 2048},
		}
		if spec.hasMod {
			m.LlamaCpp = types.LlamaCppMetrics{LoadedModels: []types.LlamaCppModel{{Name: model}}}
		}
		// SetBackendMetrics синхронизирует и llamaMetrics — ровно то, что делает
		// боевой UpdateMetrics.
		p.SetBackendMetrics(spec.id, m)
	}
	return NewLlamaCppRouter(p)
}

// TestSelectLlamaCppBackendForModel_PrefersIdleNode — главная проверка: при двух
// загруженных копиях выбирается СВОБОДНЫЙ узел, а не первый по карте.
func TestSelectLlamaCppBackendForModel_PrefersIdleNode(t *testing.T) {
	const model = "gemma-4-E4B-it-Q4_K_M"

	// Узел A занят (1/1), узел B свободен (0/1).
	lr := balanceTestRouter(t, 1, 0, true, true)
	if got := lr.selectLlamaCppBackendForModel(model); got != "bk-b" {
		t.Fatalf("выбран %q, ожидался свободный bk-b — иначе запрос встанет в очередь к занятому узлу", got)
	}

	// Обратная расстановка: занят B, свободен A.
	lr = balanceTestRouter(t, 0, 1, true, true)
	if got := lr.selectLlamaCppBackendForModel(model); got != "bk-a" {
		t.Fatalf("выбран %q, ожидался свободный bk-a", got)
	}
}

// TestSelectLlamaCppBackendForModel_IgnoresNodesWithoutModel — узел без модели не
// должен попадать в выбор только потому, что он свободен: его слот бесполезен.
func TestSelectLlamaCppBackendForModel_IgnoresNodesWithoutModel(t *testing.T) {
	const model = "gemma-4-E4B-it-Q4_K_M"

	// Модель есть только на занятом A; B свободен, но модели у него нет.
	lr := balanceTestRouter(t, 1, 0, true, false)
	if got := lr.selectLlamaCppBackendForModel(model); got != "bk-a" {
		t.Fatalf("выбран %q, ожидался bk-a (единственный узел с моделью)", got)
	}
}

// TestSelectLlamaCppBackendForModel_BothIdlePicksOne — при равной загрузке выбор
// детерминирован и не пустой.
func TestSelectLlamaCppBackendForModel_BothIdlePicksOne(t *testing.T) {
	const model = "gemma-4-E4B-it-Q4_K_M"
	lr := balanceTestRouter(t, 0, 0, true, true)
	first := lr.selectLlamaCppBackendForModel(model)
	if first != "bk-a" && first != "bk-b" {
		t.Fatalf("выбран %q, ожидался один из узлов с моделью", first)
	}
	for i := 0; i < 20; i++ {
		if got := lr.selectLlamaCppBackendForModel(model); got != first {
			t.Fatalf("выбор не детерминирован: %q, затем %q (при равной загрузке клиент будет прыгать между узлами)", first, got)
		}
	}
}

// TestSelectLlamaCppBackendForModel_FallsBackWithoutMetrics — если метрик нет
// вовсе (агент не отчитался), выбор не должен ломаться: возвращается healthy узел.
func TestSelectLlamaCppBackendForModel_FallsBackWithoutMetrics(t *testing.T) {
	const model = "gemma-4-E4B-it-Q4_K_M"
	lr := balanceTestRouter(t, 0, 0, false, false)
	got := lr.selectLlamaCppBackendForModel(model)
	if got == "" {
		t.Fatal("без метрик выбор пуст — запрос получит 503 «no llama.cpp backend available»")
	}
}

// balanceTestRouterStoredName — H8: два healthy llama.cpp-бэкенда, модель
// загружена ТОЛЬКО у второго (bk-b), причём под именем, которое клиент никогда
// не присылает дословно (другой регистр + расширение .gguf).
func balanceTestRouterStoredName(t *testing.T, storedName string) *LlamaCppRouter {
	t.Helper()

	p := &Proxy{
		config:     &types.LoadBalancerConfig{},
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
	}
	for _, spec := range []struct {
		id     string
		hasMod bool
	}{{"bk-a", false}, {"bk-b", true}} {
		p.backends[spec.id] = &BackendState{
			Backend: &types.Backend{
				ID:                spec.id,
				Host:              "127.0.0.1",
				Status:            types.StatusHealthy,
				Type:              types.BackendTypeLlamaCpp,
				CppWorkerPort:     18092,
				MaxConcurrentReqs: 1,
			},
			ActiveReqs: 0,
		}
		m := &types.BackendMetrics{
			ID:                    spec.id,
			Status:                types.StatusHealthy,
			BackendType:           types.BackendTypeLlamaCpp,
			MaxConcurrentRequests: 1,
			GPU:                   types.GPUMetrics{MemoryTotal: 8192, MemoryUsed: 1000},
			System:                types.SystemMetrics{MemoryTotal: 24576, MemoryUsed: 2048},
		}
		if spec.hasMod {
			m.LlamaCpp = types.LlamaCppMetrics{LoadedModels: []types.LlamaCppModel{{Name: storedName}}}
		}
		// SetBackendMetrics синхронизирует и llamaMetrics — ровно то, что делает
		// боевой UpdateMetrics.
		p.SetBackendMetrics(spec.id, m)
	}
	return NewLlamaCppRouter(p)
}

// TestSelectLlamaCppBackendForModel_MatchesNormalizedStoredName_H8 — H8
// (2026-10-09): тёплый узел должен находиться и когда cppworker отдаёт имя
// модели в другом регистре/с расширением .gguf, а клиент зовёт её Ollama-тегом.
//
// ЖИВОЙ ДЕФЕКТ. findModelOnLlamaCppBackend сравнивал имена СЫРО
// (`m.Name == model || strings.TrimSuffix(m.Name, ".gguf") == model`), тогда как
// рядом (IsLlamaCppModelLoaded) уже был нормализующий modelNameMatches. Запрос
// "gemma-4-e4b-it-q4_k_m:latest" при загруженной "Gemma-4-E4B-it-Q4_K_M.gguf"
// не находил узел с моделью в VRAM: выбор уходил в selectAnyLlamaCppHealthy —
// произвольный healthy узел (в т.ч. холодный), что давало лишний unload+load
// (десятки секунд — минуты на модель).
func TestSelectLlamaCppBackendForModel_MatchesNormalizedStoredName_H8(t *testing.T) {
	const (
		storedName  = "Gemma-4-E4B-it-Q4_K_M.gguf"
		requestName = "gemma-4-e4b-it-q4_k_m:latest"
	)
	if !modelNameMatches(storedName, requestName) {
		t.Fatalf("предпосылка теста нарушена: modelNameMatches(%q, %q) == false", storedName, requestName)
	}
	// Сырое сравнение (прежнее поведение) эту пару НЕ ловит — иначе тест ничего
	// не проверял бы.
	if storedName == requestName || strings.TrimSuffix(storedName, ".gguf") == requestName {
		t.Fatalf("пара %q / %q совпадает сырым сравнением — тест не воспроизводит дефект H8", storedName, requestName)
	}

	lr := balanceTestRouterStoredName(t, storedName)

	// (1) Точка правки: поиск узла с уже загруженной моделью.
	if got := lr.findModelOnLlamaCppBackend(requestName); got != "bk-b" {
		t.Fatalf("findModelOnLlamaCppBackend(%q) = %q, ожидался bk-b — единственный узел с моделью в VRAM (иначе запрос уйдёт на холодный узел)", requestName, got)
	}

	// (2) Боевой выбор бэкенда: без нормализации он падал в
	// selectAnyLlamaCppHealthy, который отдаёт узел из обхода карты — то есть
	// мог вернуть холодный bk-a. Повторяем, чтобы поймать недетерминированный
	// фолбэк, а не разовое совпадение.
	for i := 0; i < 20; i++ {
		if got := lr.selectLlamaCppBackendForModel(requestName); got != "bk-b" {
			t.Fatalf("selectLlamaCppBackendForModel(%q) = %q (попытка %d), ожидался тёплый bk-b", requestName, got, i+1)
		}
	}
}
