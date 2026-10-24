package balancer

// R104 (S6 плана 2026-10-07): список ЗАГРУЖЕННОГО на узлах группы.
//
// Зачем: при «Разъединить» модели на узлах группы будут выгружены, и оператор
// обязан увидеть, что именно потеряет, ДО подтверждения. Проверяем путь данных:
// метрики бэкенда → узел группы → состояние группы (то, что уходит в API).

import (
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

func TestBackendGroupState_TokensPerSecond_R104(t *testing.T) {
	p := shardedProxyR92(t, shardedGroupR92(), 6144, 6144, true)

	// Измерений ещё не было: карточка обязана показать «н/д», а не 0 ток/с.
	state, _ := p.BackendGroupStateForModel("m-sharded")
	if state.TokensKnown {
		t.Fatalf("без измерений скорость неизвестна: %+v", state)
	}

	// Трекер латентности — единственный источник скорости в балансере.
	p.modelLatencyTracker.RecordSample("m-sharded", ModelSample{
		Timestamp: time.Now(), TokensGenerated: 100, DurationMs: 5000,
	})

	state, _ = p.BackendGroupStateForModel("m-sharded")
	if !state.TokensKnown || state.TokensPerSecond <= 0 {
		t.Fatalf("измеренная скорость должна попасть в состояние группы: %+v", state)
	}
	if state.TokensPerSecond < 15 || state.TokensPerSecond > 25 {
		t.Errorf("скорость = %.1f, ожидалось около 20 ток/с (100 токенов за 5 с)", state.TokensPerSecond)
	}
}

func TestBackendGroupState_TokensPerSecondMatchesMask_R104(t *testing.T) {
	// Модель группы — маска: в трекере лежит КОНКРЕТНОЕ имя. Без сопоставления по
	// маске карточка показывала бы «н/д» на стенде, где скорость уже измерена.
	groups := shardedGroupR92()
	groups[0].Model = "qwen3-14b*"
	p := shardedProxyR92(t, groups, 6144, 6144, true)

	p.modelLatencyTracker.RecordSample("qwen3-14b-Q4_K_M", ModelSample{
		Timestamp: time.Now(), TokensGenerated: 60, DurationMs: 2000,
	})

	state, _ := p.BackendGroupStateForModel("qwen3-14b-Q4_K_M.gguf")
	if !state.TokensKnown || state.TokensPerSecond <= 0 {
		t.Fatalf("скорость должна находиться по маске группы: %+v", state)
	}
}

func TestBackendGroupState_LoadedModelsFromMetrics_R104(t *testing.T) {
	p := shardedProxyR92(t, shardedGroupR92(), 6144, 6144, true)

	// cppworker отдаёт загруженные модели в LlamaCpp.LoadedModels.
	p.UpdateMetrics(r92MasterID, &types.BackendMetrics{
		GPU:      types.GPUMetrics{MemoryTotal: 8192, MemoryFree: 2048},
		LlamaCpp: types.LlamaCppMetrics{LoadedModels: []types.LlamaCppModel{{Name: "m-sharded"}}},
	})
	p.UpdateMetrics(r92WorkerID, &types.BackendMetrics{
		GPU:      types.GPUMetrics{MemoryTotal: 8192, MemoryFree: 2048},
		LlamaCpp: types.LlamaCppMetrics{LoadedModels: []types.LlamaCppModel{{Name: "gemma-3-4b"}}},
	})

	state, ok := p.BackendGroupStateForModel("m-sharded")
	if !ok {
		t.Fatal("группа должна быть найдена")
	}
	if len(state.LoadedModels) != 2 {
		t.Fatalf("LoadedModels = %v, ожидались модели обоих узлов", state.LoadedModels)
	}
	// Объединение по узлам (для «что выгрузится всего»).
	if state.LoadedModels[0] != "gemma-3-4b" || state.LoadedModels[1] != "m-sharded" {
		t.Errorf("LoadedModels = %v, ожидался детерминированный порядок", state.LoadedModels)
	}
	// И по узлам отдельно (для «что выгрузится с этого узла»).
	var master, worker string
	for _, member := range state.Members {
		switch member.Role {
		case types.BackendGroupRoleMaster:
			master = member.BackendID
		case types.BackendGroupRoleWorker:
			worker = member.BackendID
		}
	}
	if master != r92MasterID || worker != r92WorkerID {
		t.Fatalf("роли перепутаны: master=%q worker=%q", master, worker)
	}
	for _, member := range state.Members {
		if member.BackendID == r92MasterID && len(member.LoadedModels) != 1 {
			t.Errorf("у мастера загружено %v", member.LoadedModels)
		}
		if member.BackendID == r92WorkerID && len(member.LoadedModels) != 1 {
			t.Errorf("у воркера загружено %v", member.LoadedModels)
		}
	}
}

func TestBackendGroupState_NoMetricsNoLoadedModels_R104(t *testing.T) {
	// Метрик нет — списка нет. Выдумывать «что выгрузится» нельзя: оператор
	// примет решение по ложным данным.
	p := shardedProxyR92(t, shardedGroupR92(), 0, 0, true)

	state, _ := p.BackendGroupStateForModel("m-sharded")
	if len(state.LoadedModels) != 0 {
		t.Errorf("без метрик список должен быть пуст: %v", state.LoadedModels)
	}
}

func TestBackendGroupState_EmptyNamesIgnored_R104(t *testing.T) {
	// Пустые имена в метриках (бывает при частичном снапшоте) не должны
	// превращаться в пустые строки в списке выгрузки.
	p := shardedProxyR92(t, shardedGroupR92(), 6144, 6144, true)
	p.UpdateMetrics(r92MasterID, &types.BackendMetrics{
		LlamaCpp: types.LlamaCppMetrics{LoadedModels: []types.LlamaCppModel{{Name: "  "}, {Name: "real-model"}}},
	})

	state, _ := p.BackendGroupStateForModel("m-sharded")
	if len(state.LoadedModels) != 1 || state.LoadedModels[0] != "real-model" {
		t.Errorf("LoadedModels = %v, ожидался только real-model", state.LoadedModels)
	}
}
