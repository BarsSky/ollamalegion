package balancer

// R96 (S5 плана 2026-10-07): предлагаемый план размещения.
//
// Проверяем матрицу §8: «влезает в один» → single/replicated, «влезает в сумму» →
// sharded с предложенной группой, «не влезает никуда» → честный degraded с
// числами. И то, что план — это ГОТОВЫЙ фрагмент balancing.placement, а не
// применённая настройка.

import (
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/internal/composition"
	"ollama-loadbalancer/pkg/types"
)

// planProxyR96 — прокси с N llama.cpp-узлами и заданным свободным VRAM каждого.
func planProxyR96(t *testing.T, vramFreeMB ...int) *Proxy {
	t.Helper()
	cfg := createTestConfig()
	cfg.Backends = make([]types.Backend, 0, len(vramFreeMB))
	for i := range vramFreeMB {
		cfg.Backends = append(cfg.Backends, types.Backend{
			ID:                "r96-node-" + string(rune('a'+i)),
			Name:              "node",
			Host:              "127.0.0.1",
			CppWorkerPort:     18092,
			Type:              types.BackendTypeLlamaCpp,
			Status:            types.StatusHealthy,
			MaxConcurrentReqs: 1,
			Weight:            1,
			Labels:            []string{"gpu:sm_86"},
		})
	}
	p := newProxyWithCleanup(t, cfg)
	for i, free := range vramFreeMB {
		p.UpdateMetrics("r96-node-"+string(rune('a'+i)), &types.BackendMetrics{
			GPU:    types.GPUMetrics{MemoryTotal: 24576, MemoryFree: uint64(free)},
			System: types.SystemMetrics{MemoryTotal: 32768, MemoryFree: 16384},
		})
	}
	// R232: изоляция от РЕАЛЬНОГО стенда. Тестовые бэкенды смотрят на
	// 127.0.0.1:18092, где на машине разработчика может быть живой cppworker:
	// фоновый поллер каталога успевает наполнить ModelCatalogSnapshot файлами
	// стенда, и discoverModelsR96 находит лишние модели — тест падал только при
	// поднятом стенде (Discovered=4 вместо 2). Останавливаем поллер и стартуем
	// с пустым каталогом, чтобы discovery зависел только от профилей.
	if p.llamaCppMetricsPoller != nil {
		p.llamaCppMetricsPoller.Stop()
		p.llamaCppMetricsPoller.WaitStopped(2 * time.Second)
	}
	p.modelCatalog = newModelCatalog()
	return p
}

func planRequestR96(model string, sizeBytes int64) SetupPlanRequestR96 {
	return SetupPlanRequestR96{
		Models: []SetupPlanModelR96{{
			Name: model, SizeBytes: sizeBytes,
			HiddenSize: 4096, Layers: 48, NCtx: 32768,
		}},
		Lang:            composition.LangRU,
		TokensPerSecond: 30,
	}
}

func TestPlacementPlanR96_SingleWhenOnlyOneNodeWouldFit(t *testing.T) {
	// Модель 8 ГиБ (need 10 ГиБ) влезает в 12 ГиБ одного узла, но оператор просит
	// без копий (singleOnly) → single.
	p := planProxyR96(t, 12288, 12288)
	req := planRequestR96("m-8b", 8<<30)
	req.SingleOnly = true

	plan := p.PlacementPlanR96(req)
	if len(plan.Plans) != 1 {
		t.Fatalf("планов = %d, ожидался 1", len(plan.Plans))
	}
	item := plan.Plans[0]
	if item.Strategy != string(types.PlacementSingle) {
		t.Fatalf("strategy = %q, ожидалось single (reason=%s)", item.Strategy, item.Reason)
	}
	if !item.KnownSize || item.Degraded {
		t.Errorf("модель известна и влезает: %+v", item)
	}
	if !strings.Contains(item.Reason, "влезает в один узел") {
		t.Errorf("причина должна объяснять выбор: %s", item.Reason)
	}
	if !containsNoteR96(item.Notes, "раскладка не нужна") {
		t.Errorf("нужна подсказка про ненужность раскладки: %v", item.Notes)
	}
}

func TestPlacementPlanR96_SingleWhenOnlyOneNodeFits(t *testing.T) {
	// Влезает ровно в один узел (второй мал) — копий быть не может.
	p := planProxyR96(t, 12288, 1024)

	item := p.PlacementPlanR96(planRequestR96("m-8b", 8<<30)).Plans[0]
	if item.Strategy != string(types.PlacementSingle) {
		t.Fatalf("strategy = %q, ожидалось single (reason=%s)", item.Strategy, item.Reason)
	}
	if item.Degraded {
		t.Errorf("в один узел модель влезает — деградации нет: %+v", item)
	}
}

func TestPlacementPlanR96_ReplicatedWhenTwoNodesFit(t *testing.T) {
	// Модель влезает в оба узла → предлагаем копии (пропускная способность).
	p := planProxyR96(t, 12288, 12288)

	plan := p.PlacementPlanR96(planRequestR96("m-8b", 8<<30))
	item := plan.Plans[0]
	if item.Strategy != string(types.PlacementReplicated) {
		t.Fatalf("strategy = %q, ожидалось replicated (reason=%s)", item.Strategy, item.Reason)
	}
	if item.Rule.Auto == nil || item.Rule.Auto.MinBackends != 2 {
		t.Errorf("для репликации нужен auto.minBackends=2: %+v", item.Rule.Auto)
	}
	if !containsNoteR96(item.Notes, "single тоже подошёл бы") {
		t.Errorf("оператор должен знать, что single достаточно: %v", item.Notes)
	}
	if item.Group != nil {
		t.Error("для репликации группа не нужна: копии независимы")
	}
}

func TestPlacementPlanR96_ShardedWhenOnlySumFits(t *testing.T) {
	// Ни один узел не вмещает 10 ГиБ, а сумма 12 ГиБ — вмещает.
	p := planProxyR96(t, 6144, 6144)

	plan := p.PlacementPlanR96(planRequestR96("m-14b", 8<<30))
	item := plan.Plans[0]
	if item.Strategy != string(types.PlacementSharded) {
		t.Fatalf("strategy = %q, ожидалось sharded (reason=%s)", item.Strategy, item.Reason)
	}
	if item.Group == nil {
		t.Fatal("раскладка без предложенной группы бессмысленна")
	}
	if item.GroupID != item.Group.ID || item.GroupID == "" {
		t.Errorf("groupId = %q, группа = %+v", item.GroupID, item.Group)
	}
	if item.Group.Strategy != "layer" {
		t.Errorf("по умолчанию предлагаем layer (§3: row дорог на префилле): %q", item.Group.Strategy)
	}
	if len(item.Group.Members) != 2 {
		t.Fatalf("участников = %d, ожидалось 2", len(item.Group.Members))
	}
	// Мастер — узел с большим свободным VRAM; при равенстве — меньший id.
	if item.Group.Members[0].Role != "master" || item.Group.Members[0].BackendID != "r96-node-a" {
		t.Errorf("мастер выбран неверно: %+v", item.Group.Members[0])
	}
	if item.Group.Members[1].Role != "worker" {
		t.Errorf("второй участник должен быть воркером: %+v", item.Group.Members[1])
	}
	if !strings.HasSuffix(item.Group.Members[1].RPCEndpoint, ":50052") {
		t.Errorf("порт RPC по умолчанию должен быть предложен: %q", item.Group.Members[1].RPCEndpoint)
	}
	if !containsNoteR96(item.Notes, "порт RPC воркеров предложен по умолчанию") {
		t.Errorf("догадка про порт должна быть помечена: %v", item.Notes)
	}
	if !strings.Contains(item.Reason, "сумма") {
		t.Errorf("причина должна объяснять, что решает сумма: %s", item.Reason)
	}
	// Замечания §7 приложены к предложению (порт 50052 на 127.0.0.1 закрыт).
	if item.Issues == nil {
		t.Error("issues должны присутствовать (хотя бы пустым списком)")
	}
	// Трафик считается для обеих стратегий — оператору нужно сравнение.
	if item.Traffic == nil || !item.Traffic.Layer.Known || !item.Traffic.Row.Known {
		t.Fatalf("оценка трафика обязательна для раскладки: %+v", item.Traffic)
	}
	if item.Traffic.Row.BytesPerToken <= item.Traffic.Layer.BytesPerToken {
		t.Error("row должен быть дороже layer при одном числе слоёв")
	}
}

func TestPlacementPlanR96_NoFitAtAllDegrades(t *testing.T) {
	p := planProxyR96(t, 2048, 2048)

	item := p.PlacementPlanR96(planRequestR96("m-70b", 40<<30)).Plans[0]
	if item.Strategy != string(types.PlacementSingle) || !item.Degraded {
		t.Fatalf("не влезает никуда — single с degraded: %+v", item)
	}
	for _, want := range []string{"нужно", "свободно максимум", "суммарно"} {
		if !strings.Contains(item.Reason, want) {
			t.Errorf("в причине должны быть ЧИСЛА (%q): %s", want, item.Reason)
		}
	}
	if !containsNoteR96(item.Notes, "нужен узел с большей VRAM") {
		t.Errorf("нужна подсказка, что делать: %v", item.Notes)
	}
	if item.Group != nil {
		t.Error("группа без вместимости не предлагается")
	}
}

func TestPlacementPlanR96_UnknownSizeIsHonest(t *testing.T) {
	p := planProxyR96(t, 12288)
	req := planRequestR96("m-unknown", 0) // размера нет ни в запросе, ни в профиле

	item := p.PlacementPlanR96(req).Plans[0]
	if item.KnownSize {
		t.Fatal("размер неизвестен — KnownSize должен быть false")
	}
	if !item.Degraded {
		t.Error("без размера предложение не гарантировано: degraded=true")
	}
	if !strings.Contains(item.Reason, "размер модели неизвестен") {
		t.Errorf("причина должна быть честной: %s", item.Reason)
	}
}

func TestPlacementPlanR96_PolicyFragmentIsReadyButNotApplied(t *testing.T) {
	p := planProxyR96(t, 6144, 6144)

	plan := p.PlacementPlanR96(planRequestR96("m-14b", 8<<30))

	// 1. Готовый фрагмент политики: включён, fallback=error, по правилу на модель.
	if !plan.Placement.Enabled || plan.Placement.Fallback != types.PlacementFallbackError {
		t.Errorf("фрагмент политики должен быть готов к записи: %+v", plan.Placement)
	}
	if len(plan.Placement.Models) != 1 {
		t.Fatalf("правил = %d, ожидалось 1", len(plan.Placement.Models))
	}
	rule := plan.Placement.Models[0]
	if rule.Model != "m-14b" {
		t.Errorf("правило не про ту модель: %+v", rule)
	}
	// Политика пишется как auto: состав кластера меняется, решение должно следовать.
	if rule.Strategy != string(types.PlacementAuto) {
		t.Errorf("стратегия правила = %q, ожидалось auto", rule.Strategy)
	}
	if rule.Auto == nil || len(rule.Auto.Prefer) == 0 || rule.Auto.Prefer[0] != string(types.PlacementSharded) {
		t.Errorf("auto.prefer должен начинаться с предложенной стратегии: %+v", rule.Auto)
	}
	if len(plan.BackendGroups) != 1 {
		t.Errorf("предложенная группа должна попасть во фрагмент: %+v", plan.BackendGroups)
	}

	// 2. Ничего не применено: конфиг и реестр групп пусты.
	if len(p.config.Balancing.Placement.Models) != 0 {
		t.Error("план не должен менять placement-политику балансера")
	}
	if len(p.config.Balancing.BackendGroups) != 0 {
		t.Error("план не должен записывать группы в конфиг")
	}
	if p.BackendGroupsR92() != nil && p.BackendGroupsR92().Configured() {
		t.Error("план не должен регистрировать группы в реестре")
	}
	if plan.Hint == "" {
		t.Error("мастеру нужна подсказка, что это предложение, а не применение")
	}
}

func TestPlacementPlanR96_DiscoveryFromProfiles(t *testing.T) {
	p := planProxyR96(t, 6144, 6144)
	p.config.LlamaCppModelProfiles = map[string]types.LlamaCppModelProfile{
		"m-alpha": {SizeBytes: 4 << 30},
		"m-beta":  {SizeBytes: 8 << 30},
	}

	plan := p.PlacementPlanR96(SetupPlanRequestR96{Lang: composition.LangRU})
	if plan.Discovered != 2 {
		t.Fatalf("обнаружено моделей = %d, ожидалось 2 (%+v)", plan.Discovered, plan.Plans)
	}
	if len(plan.Plans) != 2 {
		t.Fatalf("планов = %d, ожидалось 2", len(plan.Plans))
	}
	// Порядок детерминирован (по имени) — UI не должен прыгать между запусками.
	if plan.Plans[0].Model != "m-alpha" || plan.Plans[1].Model != "m-beta" {
		t.Errorf("порядок планов = %q, %q", plan.Plans[0].Model, plan.Plans[1].Model)
	}
	if plan.Plans[0].Strategy != string(types.PlacementReplicated) {
		t.Errorf("модель 4 ГиБ влезает в два узла по 6 ГиБ: %q", plan.Plans[0].Strategy)
	}
}

func TestPlacementPlanR96_ExcludesExistingGroupWorkers(t *testing.T) {
	// Второй узел уже отдан существующей раскладке — предлагать его повторно
	// нельзя (GPU занят), поэтому раскладка не собирается: кандидат один.
	p := planProxyR96(t, 6144, 6144)
	p.SetBackendGroupsR92(types.BalancingSettings{
		BackendGroups: []types.BackendGroupConfig{{
			ID: "g-existing", Model: "другая-модель", Strategy: "layer",
			Members: []types.BackendGroupMemberConfig{
				{BackendID: "r96-node-a", Role: "master"},
				{BackendID: "r96-node-b", Role: "worker", RPCEndpoint: "127.0.0.1:50052"},
			},
		}},
	})

	plan := p.PlacementPlanR96(planRequestR96("m-14b", 8<<30))
	item := plan.Plans[0]
	if item.Strategy == string(types.PlacementSharded) {
		t.Fatalf("занятый воркерами GPU не должен попадать в предложение: %+v", item)
	}
	if !item.Degraded {
		t.Errorf("свободного кандидата не осталось — это честный отказ: %+v", item)
	}
	// Отдельно проверим, что воркер не попал ни в одну предложенную группу.
	for _, g := range plan.BackendGroups {
		for _, m := range g.Members {
			if m.BackendID == "r96-node-b" {
				t.Error("воркер существующей группы предложен повторно")
			}
		}
	}
}

func TestPlacementPlanR96_ExcludesNonLlamaBackends(t *testing.T) {
	// image-узел не участвует в раскладке текстовой модели.
	p := planProxyR96(t, 6144)
	p.mu.Lock()
	p.backends["r96-node-a"].Backend.Type = types.BackendTypeImage
	p.mu.Unlock()

	plan := p.PlacementPlanR96(planRequestR96("m-14b", 8<<30))
	if plan.Plans[0].Strategy == string(types.PlacementSharded) {
		t.Error("image-узел не может быть частью раскладки моделей llama.cpp")
	}
}

func TestPlacementPlanR96_NilProxy(t *testing.T) {
	var p *Proxy
	plan := p.PlacementPlanR96(SetupPlanRequestR96{})
	if len(plan.Warnings) == 0 {
		t.Error("без балансера план обязан сообщить о проблеме")
	}
}

func TestSanitizeGroupIDR96(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Qwen3-14B", "qwen3-14b"},
		{"hf.co/user/model", "hf-co-user-model"},
		{"...", "model"},
		{"", "model"},
	}
	for _, tc := range cases {
		if got := sanitizeGroupIDR96(tc.in); got != tc.want {
			t.Errorf("sanitizeGroupIDR96(%q) = %q, ожидалось %q", tc.in, got, tc.want)
		}
	}
	long := sanitizeGroupIDR96(strings.Repeat("a", 100))
	if len(long) != 48 {
		t.Errorf("длинный id должен обрезаться до 48: %d", len(long))
	}
}

func containsNoteR96(notes []string, substr string) bool {
	for _, note := range notes {
		if strings.Contains(note, substr) {
			return true
		}
	}
	return false
}
