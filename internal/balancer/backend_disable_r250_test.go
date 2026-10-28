// backend_disable_r250_test.go — R250 (2026-10-28): «не использовать бэкенд».
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА (R248/R249). Единственным способом исключить узел из
// выбора было остановить контейнер или снять роль, и оба варианта плохи: узел
// пропадает из панели (а его надо настраивать), раскладка ломается, вернуть в
// работу можно только новым деплоем. Флаг `Disabled` решает ровно это: узел
// остаётся в списке, в метриках и в раскладке, но новых запросов не получает.
//
// Тесты фиксируют:
//   - выключенный бэкенд не кандидат (expandCandidates/selectBackend/
//     selectByResources/«ручные» поиски узлов) и не получает загрузку модели;
//   - включение одним переключением возвращает его в выбор;
//   - все выключены — пустой выбор и понятная причина в логе (прежнее поведение
//     «нет кандидатов», а не паника и не «выбери любой ценой»);
//   - выключенный не исчезает из GetAllBackends и переживает SaveState/LoadState
//     вместе с причиной и моментом выключения.
package balancer

import (
	"path/filepath"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// newDisableTestProxy — прокси с двумя здоровыми узлами и метриками.
//
// b1 держит модель и потому выигрывает P1; b2 модель не держит. Так видно
// главное: после выключения b1 запрос уходит на b2, а не «в никуда».
func newDisableTestProxy(t *testing.T) *Proxy {
	t.Helper()
	p := &Proxy{
		backends: map[string]*BackendState{
			"b1": {Backend: &types.Backend{ID: "b1", Name: "b1", Host: "h1", MaxConcurrentReqs: 10, Weight: 100, Status: types.StatusHealthy}},
			"b2": {Backend: &types.Backend{ID: "b2", Name: "b2", Host: "h2", MaxConcurrentReqs: 10, Weight: 50, Status: types.StatusHealthy}},
		},
		metricsMgr: NewMetricsManager(),
		config: &types.LoadBalancerConfig{Balancing: types.BalancingSettings{
			Prewarm: types.PrewarmConfig{TriggerLoadThreshold: 0.80},
		}},
		queueMgr:  NewQueueManager(nil, 10, 1, 30),
		statePath: filepath.Join(t.TempDir(), "state.json"),
	}

	p.metricsMgr.mu.Lock()
	p.metricsMgr.metrics["b1"] = &types.BackendMetrics{
		ID:  "b1",
		GPU: types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 2000, MemoryFree: 22576},
		Ollama: types.OllamaMetrics{
			MaxConcurrentRequests: 10, ActiveRequests: 1,
			RunningModels: []types.RunningModel{{Name: "llama3.1:8b"}},
		},
	}
	p.metricsMgr.metrics["b2"] = &types.BackendMetrics{
		ID:  "b2",
		GPU: types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 2000, MemoryFree: 22576},
		Ollama: types.OllamaMetrics{
			MaxConcurrentRequests: 10, ActiveRequests: 1,
			RunningModels: []types.RunningModel{},
		},
	}
	p.metricsMgr.mu.Unlock()
	return p
}

// TestR250_DisabledBackendIsNotChosen — главный сценарий: галочка «не
// использовать» убирает узел из выбора, а не из кластера.
func TestR250_DisabledBackendIsNotChosen(t *testing.T) {
	p := newDisableTestProxy(t)

	if got := p.selectBackend("llama3.1:8b", ""); got != "b1" {
		t.Fatalf("до выключения выбран %q, want b1 (модель загружена на b1)", got)
	}

	if !p.SetBackendDisabled("b1", true, "плановое обслуживание") {
		t.Fatal("SetBackendDisabled не нашёл b1")
	}

	// Кандидаты: b1 обязан исчезнуть из всех приоритетов.
	for _, group := range p.expandCandidates("llama3.1:8b", nil) {
		for _, id := range group.BackendIDs {
			if id == "b1" {
				t.Fatalf("выключенный b1 попал в кандидаты P%d", group.Priority)
			}
		}
	}

	// Запрос уходит на живой b2, а не «в никуда».
	if got := p.selectBackend("llama3.1:8b", ""); got != "b2" {
		t.Fatalf("после выключения b1 выбран %q, want b2", got)
	}
	if got := p.selectByResources(nil); got != "b2" {
		t.Fatalf("selectByResources = %q, want b2", got)
	}
	// «Ручные» поиски узлов (переезд/подбор под модель) тоже не должны отдавать
	// выключенный узел — иначе флаг обходится в обход основного селектора.
	if got := p.findBackendWithModel("llama3.1:8b", nil); got == "b1" {
		t.Fatal("findBackendWithModel вернул выключенный b1")
	}
	if got := p.findLeastLoadedBackendWithModel("llama3.1:8b", nil); got == "b1" {
		t.Fatal("findLeastLoadedBackendWithModel вернул выключенный b1")
	}
	if got := p.findLessLoadedBackendAny("llama3.1:8b", "b2", nil); got == "b1" {
		t.Fatal("findLessLoadedBackendAny вернул выключенный b1")
	}
	if got := p.selectFreeBackendAny(nil); got == "b1" {
		t.Fatal("selectFreeBackendAny вернул выключенный b1")
	}
	if got := p.findFreeBackendForModelUnsafe("другая-модель", nil); got == "b1" {
		t.Fatal("findFreeBackendForModelUnsafe вернул выключенный b1")
	}

	// И это НЕ удаление: узел остаётся в кластере, с причиной.
	all := p.GetAllBackends()
	var found *types.Backend
	for i := range all {
		if all[i].ID == "b1" {
			found = &all[i]
		}
	}
	if found == nil {
		t.Fatal("выключенный бэкенд исчез из GetAllBackends — оператор не найдёт его в панели")
	}
	if !found.Disabled {
		t.Error("флаг Disabled потерян в записи")
	}
	if found.DisabledReason != "плановое обслуживание" {
		t.Errorf("причина = %q, want «плановое обслуживание»", found.DisabledReason)
	}
	if found.DisabledAt == nil {
		t.Error("момент выключения не записан — в панели не будет «выключен N назад»")
	}
}

// TestR250_EnableReturnsBackendToOneToggle — «вернуть в работу» одним
// переключением: причина и момент снимаются, узел снова кандидат.
func TestR250_EnableReturnsBackendToOneToggle(t *testing.T) {
	p := newDisableTestProxy(t)

	p.SetBackendDisabled("b1", true, "сеть")
	p.SetBackendDisabled("b1", false, "сеть")

	disabled, reason, since, ok := p.BackendDisabledInfo("b1")
	if !ok {
		t.Fatal("бэкенд не найден")
	}
	if disabled {
		t.Fatal("флаг не снят")
	}
	if reason != "" {
		t.Errorf("причина осталась у работающего узла: %q (в панели это ложь)", reason)
	}
	if since != nil {
		t.Errorf("момент выключения остался: %v", since)
	}
	if got := p.selectBackend("llama3.1:8b", ""); got != "b1" {
		t.Fatalf("после включения выбран %q, want b1", got)
	}
}

// TestR250_AllDisabledKeepsPreviousBehaviour — все выключены: выбора нет, и это
// НЕ паника и не «выбери любой ценой». Запрос уйдёт в существующую
// admission-очередь/503, как и при полном отсутствии кандидатов.
func TestR250_AllDisabledKeepsPreviousBehaviour(t *testing.T) {
	p := newDisableTestProxy(t)
	p.SetBackendDisabled("b1", true, "")
	p.SetBackendDisabled("b2", true, "")

	if groups := p.expandCandidates("llama3.1:8b", nil); len(groups) != 0 {
		t.Fatalf("кандидаты непустые при выключенных узлах: %+v", groups)
	}
	if got := p.selectBackend("llama3.1:8b", ""); got != "" {
		t.Fatalf("selectBackend = %q, want пусто", got)
	}
	if got := p.selectByResources(nil); got != "" {
		t.Fatalf("selectByResources = %q, want пусто", got)
	}
	// warnIfAllCandidatesDisabled не должен паниковать на пустых конфигах.
	p.warnIfAllCandidatesDisabled("llama3.1:8b", nil)
}

// TestR250_DispatchWithModelLoadSkipsDisabled — загрузку модели не получает
// выключенный узел: иначе он занимает VRAM, которая оператору не нужна.
func TestR250_DispatchWithModelLoadSkipsDisabled(t *testing.T) {
	p := newDisableTestProxy(t)
	// Оба узла без модели: P3 сформируется из свободных. Выключаем b1.
	p.metricsMgr.mu.Lock()
	p.metricsMgr.metrics["b1"].Ollama.RunningModels = []types.RunningModel{}
	p.metricsMgr.metrics["b2"].Ollama.RunningModels = []types.RunningModel{}
	p.metricsMgr.mu.Unlock()
	p.SetBackendDisabled("b1", true, "")

	backendID, _ := p.dispatchWithModelLoad("новая-модель")
	if backendID == "b1" {
		t.Fatal("загрузка модели поставлена на выключенный узел")
	}
	if backendID != "b2" {
		t.Fatalf("загрузка поставлена на %q, want b2", backendID)
	}
}

// TestR250_LoadedModelNames — список моделей на узле собирается из обоих
// источников (Ollama runningModels + llama_cpp loadedModels).
func TestR250_LoadedModelNames(t *testing.T) {
	p := newDisableTestProxy(t)

	names := p.LoadedModelNames("b1")
	if len(names) != 1 || names[0] != "llama3.1:8b" {
		t.Fatalf("LoadedModelNames(b1) = %v, want [llama3.1:8b]", names)
	}
	if got := p.LoadedModelNames("b2"); len(got) != 0 {
		t.Fatalf("LoadedModelNames(b2) = %v, want пусто", got)
	}
	// Неизвестный бэкенд и nil-метрики не должны паниковать.
	if got := p.LoadedModelNames("нет-такого"); len(got) != 0 {
		t.Fatalf("LoadedModelNames(нет-такого) = %v, want пусто", got)
	}
	var nilProxy *Proxy
	if got := nilProxy.LoadedModelNames("b1"); len(got) != 0 {
		t.Fatalf("nil-proxy вернул %v", got)
	}
}

// TestR250_MissingBackendIsNotDisabled — «нет бэкенда» и «бэкенд выключен» —
// разные вещи: API отвечает 404, а не «enabled=false».
func TestR250_MissingBackendIsNotDisabled(t *testing.T) {
	p := newDisableTestProxy(t)
	if p.SetBackendDisabled("нет-такого", true, "") {
		t.Fatal("SetBackendDisabled вернул true для несуществующего бэкенда")
	}
	if _, _, _, ok := p.BackendDisabledInfo("нет-такого"); ok {
		t.Fatal("BackendDisabledInfo нашёл несуществующий бэкенд")
	}
}

// TestR250_DisabledSurvivesStateRoundTrip — решение оператора переживает
// сохранение/загрузку состояния (в bundled-развёртывании дополнительно есть
// sidecar; здесь проверяем штатный путь state.json).
func TestR250_DisabledSurvivesStateRoundTrip(t *testing.T) {
	p := newDisableTestProxy(t)
	p.SetBackendDisabled("b1", true, "греется")
	if err := p.SaveState(); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	// Новая «сессия» балансера: config приходит из config.json (роли/флагов там
	// нет), состояние — из state.json.
	fresh := &Proxy{
		backends: map[string]*BackendState{},
		config: &types.LoadBalancerConfig{
			Backends: []types.Backend{
				{ID: "b1", Host: "h1", MaxConcurrentReqs: 10, Status: types.StatusHealthy},
				{ID: "b2", Host: "h2", MaxConcurrentReqs: 10, Status: types.StatusHealthy},
			},
			Balancing: types.BalancingSettings{Prewarm: types.PrewarmConfig{TriggerLoadThreshold: 0.80}},
		},
		metricsMgr: NewMetricsManager(),
		queueMgr:   NewQueueManager(nil, 10, 1, 30),
		statePath:  p.statePath,
	}
	for i := range fresh.config.Backends {
		b := fresh.config.Backends[i]
		fresh.backends[b.ID] = &BackendState{Backend: &b}
	}
	if err := fresh.LoadState(); err != nil {
		t.Fatalf("LoadState: %v", err)
	}

	disabled, reason, since, ok := fresh.BackendDisabledInfo("b1")
	if !ok || !disabled {
		t.Fatalf("флаг не пережил рестарт: disabled=%v ok=%v", disabled, ok)
	}
	if reason != "греется" {
		t.Errorf("причина = %q, want «греется»", reason)
	}
	if since == nil || time.Since(*since) > time.Hour {
		t.Errorf("момент выключения странный: %v", since)
	}
	if got := fresh.selectBackend("llama3.1:8b", ""); got == "b1" {
		t.Fatal("после рестарта выключенный b1 снова получает запросы")
	}
}
