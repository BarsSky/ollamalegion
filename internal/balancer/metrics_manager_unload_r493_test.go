// metrics_manager_unload_r493_test.go — R493 (2026-11-14): событие выгрузки модели
// обязано убирать её из ВСЕХ источников «модель загружена».
//
// ЖИВОЙ ДЕФЕКТ, который это сообщение чинит (нашёл CI-прогон fe3a9d99,
// `TestLlamaModelLoadedConfirmsReplica_R81` в internal/api): обработчик
// `POST /api/v1/internal/llama-model-unloaded` снимал инстанс группы
// (`MarkReplicationInstanceUnloaded` → `DropInstance`), но следом запускал
// `go p.ensurePlacementInstances()`, который «усыновляет» копии по метрикам через
// `backendHasModelByID`. Та смотрит ТРИ источника, а `UpdateLlamaCppModelUnloaded`
// чистил только `llamaMetrics.LoadedModels` — объединённый `metrics.Models`
// продолжал утверждать «загружена», и инстанс возвращался в группу сразу после
// того, как его убрали.
//
// Почему это был именно флейк на слабом раннере: горутина
// `ensurePlacementInstances` успевала дойти до усыновления только при медленном
// исполнении. На быстрой машине проверка случалась раньше — тест был зелёным.
package balancer

import (
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// TestUnloadEventClearsAllLoadedSources — выгрузка чистит llamaMetrics, объединённый
// список metrics.Models и Ollama.RunningModels; чужие модели остаются.
func TestUnloadEventClearsAllLoadedSources(t *testing.T) {
	mm := NewMetricsManager()
	const id, model, other = "b1", "r81-model", "other-model"

	mm.llamaMetrics[id] = &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{{Name: model, State: "loaded"}, {Name: other, State: "loaded"}},
	}
	mm.metrics[id] = &types.BackendMetrics{
		ID:     id,
		Models: []string{model, other},
		Ollama: types.OllamaMetrics{
			RunningModels: []types.RunningModel{{Name: model}, {Name: other}},
		},
	}
	// Контроль условия: до выгрузки модель видна как загруженная.
	p := &Proxy{metricsMgr: mm}
	if !p.backendHasModelByID(id, model) {
		t.Fatal("условие теста не выполнено: модель должна считаться загруженной до выгрузки")
	}

	mm.UpdateLlamaCppModelUnloaded(id, model)

	if p.backendHasModelByID(id, model) {
		t.Fatalf("после события выгрузки модель всё ещё считается загруженной: "+
			"инстанс группы будет «усыновлён» обратно (живой CI-дефект R493)\n"+
			"llamaMetrics=%v metrics.Models=%v running=%v",
			mm.llamaMetrics[id].LoadedModels, mm.metrics[id].Models, mm.metrics[id].Ollama.RunningModels)
	}
	// Чужая модель не должна пострадать.
	if !p.backendHasModelByID(id, other) {
		t.Fatal("выгрузка одной модели убрала другую — данные метрик испорчены")
	}
}

// TestUnloadEventUnknownBackendIsNoop — события по неизвестному бэкенду не паникуют.
func TestUnloadEventUnknownBackendIsNoop(t *testing.T) {
	mm := NewMetricsManager()
	mm.UpdateLlamaCppModelUnloaded("nope", "m") // не должно паниковать
	mm.metrics["x"] = nil
	mm.UpdateLlamaCppModelUnloaded("x", "m")
	_ = time.Now()
}
