package backendgroup

// R92 (S2 плана 2026-10-07): логический бэкенд — реестр групп.
//
// Что проверяем (это и есть acceptance criteria S2):
//   - группа видна как ОДИН бэкенд: у неё один мастер, суммарный VRAM и слоты;
//   - слоты группы = лимит мастера, а НЕ сумма по узлам (грабли R70/R71:
//     сложив слоты, очередь решит, что вместимость удвоилась);
//   - воркеры не являются самостоятельными целями маршрутизации;
//   - группа жива, пока жив мастер; выпавший воркер делает её degraded, а не
//     «тихо односторонней»;
//   - при отсутствии метрик VRAM числа не выдумываются.

import (
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func infoMapR92(m map[string]MemberInfo) MemberInfoFunc {
	return func(backendID string) MemberInfo {
		if info, ok := m[backendID]; ok {
			return info
		}
		return MemberInfo{} // не зарегистрирован
	}
}

func healthyR92(freeMB, totalMB, slots int) MemberInfo {
	return MemberInfo{
		Registered:        true,
		Healthy:           true,
		Status:            "healthy",
		VRAMKnown:         true,
		VRAMFreeMB:        freeMB,
		VRAMTotalMB:       totalMB,
		MaxConcurrentReqs: slots,
	}
}

func testGroupR92() []types.BackendGroupConfig {
	return []types.BackendGroupConfig{{
		ID:       "g1",
		Model:    "qwen3-14b*",
		Strategy: "layer",
		Reason:   "14B не влезает в 8 ГБ одной карты",
		Members: []types.BackendGroupMemberConfig{
			{BackendID: "master-a", Role: "master"},
			{BackendID: "worker-b", Role: "worker", RPCEndpoint: "192.0.2.11:50052"},
		},
	}}
}

func TestRegistry_LoadedModelsUnion_R104(t *testing.T) {
	// Что загружено на узлах группы: нужно окну объединения, чтобы показать, ЧТО
	// будет выгружено при «Разъединить», до подтверждения.
	master := healthyR92(3000, 8192, 1)
	master.LoadedModels = []string{"qwen3-14b", "gemma-3-4b"}
	worker := healthyR92(3000, 8192, 1)
	worker.LoadedModels = []string{"qwen3-14b", "llama3"}

	reg := New(testGroupR92(), infoMapR92(map[string]MemberInfo{
		"master-a": master,
		"worker-b": worker,
	}))

	state, _ := reg.StateForModel("qwen3-14b")
	// Объединение без дублей и в детерминированном порядке: список показывается
	// оператору, и «прыгающий» порядок читался бы как разные наборы.
	want := []string{"gemma-3-4b", "llama3", "qwen3-14b"}
	if len(state.LoadedModels) != len(want) {
		t.Fatalf("LoadedModels = %v, ожидалось %v", state.LoadedModels, want)
	}
	for i := range want {
		if state.LoadedModels[i] != want[i] {
			t.Fatalf("LoadedModels = %v, ожидалось %v", state.LoadedModels, want)
		}
	}
	// И по каждому узлу отдельно: «что выгрузится с этого узла».
	if len(state.Members[0].LoadedModels) != 2 {
		t.Errorf("у мастера загружено %v", state.Members[0].LoadedModels)
	}
	if len(state.Members[1].LoadedModels) != 2 {
		t.Errorf("у воркера загружено %v", state.Members[1].LoadedModels)
	}
}

func TestRegistry_NoLoadedModelsIsEmpty_R104(t *testing.T) {
	reg := New(testGroupR92(), infoMapR92(map[string]MemberInfo{
		"master-a": healthyR92(3000, 8192, 1),
		"worker-b": healthyR92(3000, 8192, 1),
	}))
	state, _ := reg.StateForModel("qwen3-14b")
	if len(state.LoadedModels) != 0 {
		t.Errorf("без загруженных моделей список должен быть пуст: %v", state.LoadedModels)
	}
	if state.Members[0].LoadedModels != nil {
		t.Errorf("у узла без загруженных моделей поля быть не должно: %v", state.Members[0].LoadedModels)
	}
}

func TestRegistry_LoadedModelsAreCopied_R104(t *testing.T) {
	// Список отдаётся в API: владелец метрик не должен зависеть от того, кто
	// читает его срез.
	source := []string{"qwen3-14b", "gemma-3-4b"}
	info := healthyR92(3000, 8192, 1)
	info.LoadedModels = source
	reg := New(testGroupR92(), infoMapR92(map[string]MemberInfo{"master-a": info, "worker-b": info}))

	state, _ := reg.StateForModel("qwen3-14b")
	source[0] = "изменено-снаружи"
	for _, model := range state.LoadedModels {
		if model == "изменено-снаружи" {
			t.Fatal("состояние группы делит срез с источником метрик")
		}
	}
}

func TestRegistry_ModelTokensPerSecond_R104(t *testing.T) {
	// Скорость декода меряется по МОДЕЛИ (у раскладки она одна на всех узлов), и
	// 0 без TokensKnown означает «не измеряли», а не «ноль токенов в секунду».
	cfgs := []types.BackendGroupConfig{{
		ID: "g1", Model: "qwen3-14b", Strategy: "layer",
		Members: []types.BackendGroupMemberConfig{
			{BackendID: "a", Role: "master"},
			{BackendID: "b", Role: "worker", RPCEndpoint: "h:1"},
		},
	}}
	infos := infoMapR92(map[string]MemberInfo{
		"a": healthyR92(3000, 8192, 1),
		"b": healthyR92(3000, 8192, 1),
	})

	withMetrics := NewWithModelInfo(cfgs, infos, func(model string) ModelInfo {
		if model == "qwen3-14b" {
			return ModelInfo{TokensPerSecond: 42.5, TokensKnown: true}
		}
		return ModelInfo{}
	})
	state, _ := withMetrics.StateForModel("qwen3-14b")
	if !state.TokensKnown || state.TokensPerSecond != 42.5 {
		t.Fatalf("скорость модели не доехала до состояния: %+v", state)
	}

	// Без источника — честное «неизвестно», а не нулевая скорость.
	withoutMetrics := New(cfgs, infos)
	state, _ = withoutMetrics.StateForModel("qwen3-14b")
	if state.TokensKnown || state.TokensPerSecond != 0 {
		t.Errorf("без трекера скорость должна быть неизвестна: %+v", state)
	}

	// Источник есть, но по модели измерений нет.
	unknown := NewWithModelInfo(cfgs, infos, func(string) ModelInfo { return ModelInfo{} })
	state, _ = unknown.StateForModel("qwen3-14b")
	if state.TokensKnown {
		t.Errorf("без измерений TokensKnown должен быть false: %+v", state)
	}
}

func TestRegistry_MasterAndWorkers_R92(t *testing.T) {
	reg := New(testGroupR92(), infoMapR92(map[string]MemberInfo{}))

	if reg.Len() != 1 || !reg.Configured() {
		t.Fatalf("реестр должен знать одну группу: len=%d configured=%v", reg.Len(), reg.Configured())
	}
	g, ok := reg.GroupForModel("qwen3-14b-Q4_K_M.gguf")
	if !ok {
		t.Fatal("группа должна находиться по маске модели")
	}
	master, ok := g.Master()
	if !ok || master.BackendID != "master-a" {
		t.Fatalf("мастер определён неверно: %+v ok=%v", master, ok)
	}
	workers := g.Workers()
	if len(workers) != 1 || workers[0].BackendID != "worker-b" || workers[0].RPCEndpoint != "192.0.2.11:50052" {
		t.Fatalf("воркеры определены неверно: %+v", workers)
	}

	if !reg.IsWorker("worker-b") {
		t.Error("worker-b должен считаться воркером (исключается из выбора бэкенда)")
	}
	if reg.IsWorker("master-a") {
		t.Error("мастер не воркер — он и обслуживает запросы группы")
	}
	if reg.IsWorker("unknown") {
		t.Error("посторонний узел не должен считаться воркером")
	}
	if got := reg.GroupIDForWorker("worker-b"); got != "g1" {
		t.Errorf("GroupIDForWorker = %q, ожидалось g1", got)
	}
}

func TestRegistry_StateSumsVRAMButNotSlots_R92(t *testing.T) {
	reg := New(testGroupR92(), infoMapR92(map[string]MemberInfo{
		"master-a": healthyR92(3000, 8192, 1), // мастер: 1 слот — это ёмкость ВСЕЙ группы
		"worker-b": healthyR92(5000, 8192, 8), // 8 слотов воркера в ёмкость не идут
	}))

	state, ok := reg.StateForModel("qwen3-14b")
	if !ok {
		t.Fatal("состояние группы не найдено")
	}
	if !state.Healthy || state.Degraded || !state.Usable {
		t.Fatalf("группа должна быть готова: %+v", state)
	}
	// VRAM складывается: при layer split слои лежат на разных картах.
	if state.VRAMFreeMB != 8000 || state.VRAMTotalMB != 16384 || state.VRAMKnownMems != 2 {
		t.Errorf("VRAM группы = free %d / total %d / known %d, ожидалось 8000/16384/2",
			state.VRAMFreeMB, state.VRAMTotalMB, state.VRAMKnownMems)
	}
	// Слоты — ёмкость мастера, НЕ 1+8.
	if state.Slots != 1 {
		t.Errorf("Slots = %d, ожидалось 1 (емкость мастера, не сумма по узлам)", state.Slots)
	}
	if state.MasterBackendID != "master-a" {
		t.Errorf("MasterBackendID = %q, ожидалось master-a", state.MasterBackendID)
	}
	if got := reg.UsableMaster("qwen3-14b"); got != "master-a" {
		t.Errorf("UsableMaster = %q, ожидалось master-a", got)
	}
}

func TestRegistry_MasterDown_GroupUnusable_R92(t *testing.T) {
	// Мастер не зарегистрирован — «группа жива, пока жив мастер»: она мертва.
	reg := New(testGroupR92(), infoMapR92(map[string]MemberInfo{
		"worker-b": healthyR92(5000, 8192, 8),
	}))

	state, _ := reg.StateForModel("qwen3-14b")
	if state.Healthy {
		t.Error("без мастера группа не может быть healthy")
	}
	if !state.Degraded || state.Usable {
		t.Errorf("группа без мастера должна быть degraded и неиспользуемой: %+v", state)
	}
	if !strings.Contains(state.DegradeReason, "master-a") {
		t.Errorf("причина должна называть узел: %s", state.DegradeReason)
	}
	if reg.UsableMaster("qwen3-14b") != "" {
		t.Error("мастер не должен выбираться для маршрутизации без части слоёв")
	}
}

func TestRegistry_WorkerDown_DegradedButMasterAlive_R92(t *testing.T) {
	reg := New(testGroupR92(), infoMapR92(map[string]MemberInfo{
		"master-a": healthyR92(3000, 8192, 1),
		"worker-b": {Registered: true, Healthy: false, Status: "offline"},
	}))

	state, _ := reg.StateForModel("qwen3-14b")
	if !state.Healthy {
		t.Error("мастер жив — группа должна остаться healthy")
	}
	if !state.Degraded {
		t.Error("выпавший воркер обязан делать группу degraded: иначе раскладка молча останется неполной")
	}
	if state.Usable {
		t.Error("degraded-группа не маршрутизируется: запрос на мастера без слоёв воркера = ошибка инференса")
	}
	if !strings.Contains(state.DegradeReason, "worker-b") {
		t.Errorf("причина должна называть выпавший узел: %s", state.DegradeReason)
	}
	// VRAM выпавшего узла в сумму не попадает.
	if state.VRAMKnownMems != 1 || state.VRAMFreeMB != 3000 {
		t.Errorf("в сумму попал недоступный узел: %+v", state)
	}
}

func TestRegistry_WorkerNotRegistered_Degraded_R92(t *testing.T) {
	reg := New(testGroupR92(), infoMapR92(map[string]MemberInfo{
		"master-a": healthyR92(3000, 8192, 1),
	}))

	state, _ := reg.StateForModel("qwen3-14b")
	if !state.Degraded || state.Usable {
		t.Fatalf("незарегистрированный воркер должен деградировать группу: %+v", state)
	}
	if !strings.Contains(state.DegradeReason, "не зарегистрирован") {
		t.Errorf("причина должна быть про регистрацию: %s", state.DegradeReason)
	}
}

func TestRegistry_NoVRAMMetrics_NotInvented_R92(t *testing.T) {
	reg := New(testGroupR92(), infoMapR92(map[string]MemberInfo{
		"master-a": {Registered: true, Healthy: true, Status: "healthy", MaxConcurrentReqs: 2},
		"worker-b": {Registered: true, Healthy: true, Status: "healthy", MaxConcurrentReqs: 2},
	}))

	state, _ := reg.StateForModel("qwen3-14b")
	if state.VRAMKnownMems != 0 || state.VRAMFreeMB != 0 || state.VRAMTotalMB != 0 {
		t.Errorf("без метрик VRAM числа должны остаться нулевыми, получено %+v", state)
	}
	if !state.Usable {
		t.Error("отсутствие метрик VRAM не делает группу нерабочей (как и в VRAM-fit auto)")
	}
	if state.Slots != 2 {
		t.Errorf("Slots = %d, ожидалось 2 (лимит мастера)", state.Slots)
	}
}

func TestRegistry_ConfigVRAMBudgetFallback_R92(t *testing.T) {
	// Метрик ещё нет, но оператор задал бюджет — используем его как оценку.
	cfgs := testGroupR92()
	cfgs[0].Members[0].VRAMBudgetMB = 8000
	cfgs[0].Members[1].VRAMBudgetMB = 8000
	reg := New(cfgs, infoMapR92(map[string]MemberInfo{
		"master-a": {Registered: true, Healthy: true, Status: "healthy", MaxConcurrentReqs: 1},
		"worker-b": {Registered: true, Healthy: true, Status: "healthy", MaxConcurrentReqs: 1},
	}))

	state, _ := reg.StateForModel("qwen3-14b")
	if state.VRAMFreeMB != 16000 || state.VRAMKnownMems != 2 {
		t.Errorf("бюджет из конфига должен использоваться как fallback: %+v", state)
	}
}

func TestRegistry_NoInfo_AllUnregistered_R92(t *testing.T) {
	// info == nil: конфиг есть, узлов нет — состояние должно быть честным.
	reg := New(testGroupR92(), nil)
	state, ok := reg.StateForModel("qwen3-14b")
	if !ok {
		t.Fatal("группа должна находиться даже без данных об узлах")
	}
	if state.Usable || state.Healthy {
		t.Errorf("без данных об узлах группа не может быть готова: %+v", state)
	}
	if state.Members[0].Problem == "" {
		t.Error("у участника должна быть причина недоступности")
	}
}

func TestRegistry_InvalidGroup_NotUsable_R92(t *testing.T) {
	// Мастера нет вовсе — группа не должна маршрутизироваться ни при каких
	// условиях, даже если все узлы healthy.
	cfgs := []types.BackendGroupConfig{{
		ID:       "broken",
		Model:    "m",
		Strategy: "layer",
		Members: []types.BackendGroupMemberConfig{
			{BackendID: "a", Role: "worker", RPCEndpoint: "host:1"},
			{BackendID: "b", Role: "worker", RPCEndpoint: "host:2"},
		},
	}}
	reg := New(cfgs, infoMapR92(map[string]MemberInfo{
		"a": healthyR92(1000, 8000, 1),
		"b": healthyR92(1000, 8000, 1),
	}))

	state, _ := reg.StateForModel("m")
	if state.Usable || state.Healthy {
		t.Errorf("группа без мастера не может быть исполнимой: %+v", state)
	}
	if !strings.Contains(state.DegradeReason, "master") {
		t.Errorf("причина должна говорить про мастера: %s", state.DegradeReason)
	}
	if reg.UsableMaster("m") != "" {
		t.Error("UsableMaster должен быть пуст для некорректной группы")
	}
}

func TestRegistry_EmptyAndUnknownModel_R92(t *testing.T) {
	reg := New(nil, nil)
	if reg.Configured() || reg.Len() != 0 {
		t.Error("пустой конфиг — пустой реестр")
	}
	if _, ok := reg.GroupForModel("anything"); ok {
		t.Error("в пустом реестре нечего находить")
	}
	if reg.UsableMaster("anything") != "" {
		t.Error("в пустом реестре нет исполнителя")
	}
	if _, ok := reg.StateForModel("anything"); ok {
		t.Error("состояния нет для неописанной модели")
	}

	reg2 := New(testGroupR92(), infoMapR92(nil))
	if _, ok := reg2.GroupForModel(""); ok {
		t.Error("пустое имя модели не должно матчиться")
	}
	if _, ok := reg2.GroupForModel("другая-модель"); ok {
		t.Error("чужая модель не должна попадать в группу")
	}
}
