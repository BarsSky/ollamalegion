package balancer

import (
	"path/filepath"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// R91 (2026-10-09). «Дублирование вывода о загруженных моделях» (скриншот
// оператора): на странице моделей одна и та же модель Qwen3-Instruct-2507-q4km
// показывалась ДВУМЯ карточками одного бэкенда cppworker-gpu-bundled-agent — одна
// с прочерками, «(сведения ещё не получены)» и «⌛ Истёк», вторая с реальными
// данными (2.3 GB, GGUF path, RAM 2620/25044).
//
// Причина: cluster state отдавал модель дважды —
//   * metrics.Ollama.RunningModels — Ollama-совместимая проекция, которую
//     updateRunningModelInMetrics добавляет при warmup (только имя, expiresAt =
//     0001-01-01 → UI показывал «Истёк»);
//   * metrics.LlamaCpp.LoadedModels — реальные данные от поллера.
//
// Правило: при непустом LoadedModels проекцию в ответ не отдаём (один источник);
// при пустом — отдаём, потому что других данных нет.

func newLoadedModelsProxy(t *testing.T) *Proxy {
	t.Helper()
	return newProxyWithCleanup(t, &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host: "localhost", Port: 8080, APIPort: 8081,
			StatePath: filepath.Join(t.TempDir(), "state.json"),
		},
		Backends: []types.Backend{{
			ID: "cppworker-gpu-bundled-agent", Name: "w", Host: "127.0.0.1", CppWorkerPort: 18092,
			Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy, MaxConcurrentReqs: 1,
		}},
		Balancing: types.BalancingSettings{Algorithm: types.AlgorithmResourceAware, OperatingMode: "standard"},
	})
}

// clusterBackend — бэкенд из cluster state по ID.
func clusterBackend(t *testing.T, p *Proxy, id string) types.BackendMetrics {
	t.Helper()
	for _, b := range p.GetClusterState().Backends {
		if b.ID == id {
			return b
		}
	}
	t.Fatalf("бэкенд %q не найден в cluster state", id)
	return types.BackendMetrics{}
}

// TestClusterState_LoadedModelsSuppressOllamaProjection — модель не отдаётся
// дважды: проекция в runningModels пуста, данные — в loadedModels, а список
// metrics.Models (его читают монитор и страница бэкендов) заполнен из loadedModels.
func TestClusterState_LoadedModelsSuppressOllamaProjection(t *testing.T) {
	p := newLoadedModelsProxy(t)
	const id = "cppworker-gpu-bundled-agent"

	// Так это и происходит живьём: warmup добавил проекцию, поллер — реальные данные.
	p.updateRunningModelInMetrics(id, "Qwen3-Instruct-2507-q4km")
	p.updateLlamaCppRunningModelInMetrics(id, "Qwen3-Instruct-2507-q4km")
	p.metricsMgr.mu.Lock()
	if lm := p.metricsMgr.llamaMetrics[id]; lm != nil {
		for i := range lm.LoadedModels {
			lm.LoadedModels[i].Path = "/app/models/Qwen3-Instruct-2507-q4km.gguf"
			lm.LoadedModels[i].Size = 2497281120
		}
	}
	p.metricsMgr.mu.Unlock()

	got := clusterBackend(t, p, id)
	if n := len(got.Ollama.RunningModels); n != 0 {
		t.Errorf("runningModels = %d записей, ожидалось 0: проекция дублирует loadedModels "+
			"и несёт нулевые поля с expiresAt=0001-01-01 («⌛ Истёк» на карточке)", n)
	}
	if n := len(got.LlamaCpp.LoadedModels); n != 1 {
		t.Fatalf("loadedModels = %d, ожидалась 1 запись (реальные данные)", n)
	}
	if got.LlamaCpp.LoadedModels[0].Path == "" {
		t.Error("в loadedModels потерялся путь к файлу — карточка снова будет пустой")
	}
	if len(got.Models) != 1 || got.Models[0] != "Qwen3-Instruct-2507-q4km" {
		t.Errorf("metrics.models = %v, ожидался список из loadedModels", got.Models)
	}
	// Проекция в metricsMgr обязана остаться: её читает /api/ps (Ollama-совместимость).
	if mm, ok := p.metricsMgr.SnapshotBackendMetrics(id); !ok || len(mm.Ollama.RunningModels) != 1 {
		t.Error("проекция RunningModels удалена из metricsMgr — /api/ps перестанет видеть модель")
	}
}

// TestClusterState_ProjectionKeptWhenNoLoadedModels — пока поллер не принёс
// данные, проекция остаётся единственным источником и должна доехать до UI.
func TestClusterState_ProjectionKeptWhenNoLoadedModels(t *testing.T) {
	p := newLoadedModelsProxy(t)
	const id = "cppworker-gpu-bundled-agent"

	p.updateRunningModelInMetrics(id, "Qwen3-Instruct-2507-q4km")

	got := clusterBackend(t, p, id)
	if len(got.Ollama.RunningModels) != 1 {
		t.Errorf("runningModels = %d, ожидалась 1 запись (loadedModels пуст, других данных нет)",
			len(got.Ollama.RunningModels))
	}
	if len(got.Models) != 1 {
		t.Errorf("metrics.models = %v, ожидалось одно имя", got.Models)
	}
}
