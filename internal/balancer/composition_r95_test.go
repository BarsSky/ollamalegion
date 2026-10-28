package balancer

// R95 (S4 плана 2026-10-07): сухой прогон композиции на живых данных балансера.
//
// Проверяем, что адаптер действительно берёт состояние из балансера (health,
// VRAM, RAM, тип, «личность» GPU), пробует RPC-порт и НИЧЕГО не меняет: dry-run
// обязан оставаться dry-run.

import (
	"net"
	"strings"
	"testing"

	"ollama-loadbalancer/internal/composition"
	"ollama-loadbalancer/pkg/types"
)

const (
	r95MasterID = "r95-master"
	r95WorkerID = "r95-worker"
)

// compositionProxyR95 — прокси с двумя здоровыми llama.cpp-узлами и метриками.
// Группы в конфиге НЕТ: сухой прогон как раз и проверяет предлагаемую.
func compositionProxyR95(t *testing.T, vramFreeMB, ramFreeMB int) *Proxy {
	t.Helper()
	cfg := createTestConfig()
	cfg.Backends = []types.Backend{
		{
			ID: r95MasterID, Name: "master", Host: "127.0.0.1", CppWorkerPort: 18092,
			Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
			MaxConcurrentReqs: 1, Weight: 1, Labels: []string{"gpu:sm_86"},
		},
		{
			ID: r95WorkerID, Name: "worker", Host: "192.0.2.11", CppWorkerPort: 18092,
			Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
			MaxConcurrentReqs: 1, Weight: 1, Labels: []string{"gpu:sm_120"},
		},
	}
	p := newProxyWithCleanup(t, cfg)
	for _, id := range []string{r95MasterID, r95WorkerID} {
		p.UpdateMetrics(id, &types.BackendMetrics{
			GPU:    types.GPUMetrics{MemoryTotal: 8192, MemoryFree: uint64(vramFreeMB)},
			System: types.SystemMetrics{MemoryTotal: 32768, MemoryFree: uint64(ramFreeMB)},
		})
	}
	return p
}

// listenR95 — реальный слушающий порт (для успешной пробы RPC).
func listenR95(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("не удалось занять порт: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String()
}

func compositionRequestR95(rpcEndpoint string, sizeBytes int64) CompositionRequestR95 {
	return CompositionRequestR95{
		Group: types.BackendGroupConfig{
			ID: "g-new", Model: "m-14b", Strategy: "layer",
			Members: []types.BackendGroupMemberConfig{
				{BackendID: r95MasterID, Role: "master"},
				{BackendID: r95WorkerID, Role: "worker", RPCEndpoint: rpcEndpoint},
			},
		},
		Model: CompositionModelR95{
			Name: "m-14b", BackendType: "llama_cpp",
			SizeBytes: sizeBytes, HiddenSize: 4096, Layers: 32, BytesPerElem: 2,
		},
		Lang:            composition.LangRU,
		TokensPerSecond: 30,
	}
}

func issueForR95(report composition.Report, check composition.CheckID) (composition.Issue, bool) {
	for _, issue := range report.Issues {
		if issue.Check == check {
			return issue, true
		}
	}
	return composition.Issue{}, false
}

func TestValidateCompositionR95_ReadyProposal(t *testing.T) {
	p := compositionProxyR95(t, 6144, 16384)
	endpoint := listenR95(t)

	report := p.ValidateCompositionR95(compositionRequestR95(endpoint, 8<<30))
	if report.HasErrors() {
		t.Fatalf("готовая композиция не должна давать ошибок: %s", report.String())
	}
	if report.GroupID != "g-new" {
		t.Errorf("groupId = %q, ожидалось g-new", report.GroupID)
	}
	// Dry-run: конфиг балансера не изменился.
	if len(p.config.Balancing.BackendGroups) != 0 {
		t.Error("сухой прогон не должен записывать группы в конфиг")
	}
	if p.BackendGroupsR92() == nil || p.BackendGroupsR92().Configured() {
		t.Error("сухой прогон не должен регистрировать реестр групп")
	}
}

func TestValidateCompositionR95_ProbesRPCPort(t *testing.T) {
	p := compositionProxyR95(t, 6144, 16384)

	// Порт закрыт (порт 1 на loopback): «узел не отвечает на …» — ошибка §7.
	report := p.ValidateCompositionR95(compositionRequestR95("127.0.0.1:1", 8<<30))
	issue, found := issueForR95(report, composition.CheckRPCUnreachable)
	if !found {
		t.Fatalf("закрытый RPC-порт должен ловиться: %s", report.String())
	}
	if issue.Level != composition.LevelError {
		t.Errorf("уровень = %q, ожидалось error", issue.Level)
	}
	if !strings.Contains(issue.Text, "127.0.0.1:1") {
		t.Errorf("текст должен называть endpoint: %s", issue.Text)
	}

	// Открытый порт — замечания нет.
	endpoint := listenR95(t)
	report = p.ValidateCompositionR95(compositionRequestR95(endpoint, 8<<30))
	if _, found := issueForR95(report, composition.CheckRPCUnreachable); found {
		t.Errorf("на слушающем порту замечания быть не должно: %s", report.String())
	}

	// SkipProbes — проба не делается вовсе (тесты/заведомо закрытая сеть).
	req := compositionRequestR95("127.0.0.1:1", 8<<30)
	req.SkipProbes = true
	if _, found := issueForR95(p.ValidateCompositionR95(req), composition.CheckRPCUnreachable); found {
		t.Error("при skipProbes проба не выполняется")
	}
}

// TestValidateCompositionR95_UsesLiveVRAMAndRAM — живые метрики узлов реально
// участвуют в оценке группы.
//
// R249 (2026-10-28) ИЗМЕНИЛ КОНТРАКТ, и этот тест обязан его догонять: раньше
// «суммарной VRAM не хватает» было ОШИБКОЙ (vram_sum_insufficient). Но веса,
// которые не влезают в VRAM, влезают в VRAM+RAM (mmap / частичный offload), и
// модель реально запускается — медленнее, но запускается. Поэтому теперь два
// разных исхода:
//   - VRAM не хватает, а VRAM+RAM хватает → warning weights_need_ram_offload
//     (деградация, оператор видит, что будет медленно);
//   - не хватает и VRAM+RAM → error vram_sum_insufficient (жёсткий отказ).
//
// Тест держит ОБА исхода: иначе легко вернуться к «отказ там, где всё работает»
// (живая жалоба, из-за которой R249 и делался) или потерять сам отказ.
func TestValidateCompositionR95_UsesLiveVRAMAndRAM(t *testing.T) {
	// Узлы по 4 ГиБ VRAM и 16 ГиБ RAM: VRAM (8 ГиБ) не хватает на модель 8 ГиБ
	// (need 10 ГиБ), а VRAM+RAM (40 ГиБ) хватает.
	p := compositionProxyR95(t, 4096, 16384)
	report := p.ValidateCompositionR95(compositionRequestR95(listenR95(t), 8<<30))

	issue, found := issueForR95(report, composition.CheckWeightsNeedRAMOffload)
	if !found {
		t.Fatalf("VRAM+RAM узлов учитывается: %s", report.String())
	}
	if issue.Level != composition.LevelWarning {
		t.Errorf("уровень = %q, ожидалось warning: с RAM модель запускается, это деградация, а не отказ",
			issue.Level)
	}
	if !strings.Contains(issue.Text, "8.0") || !strings.Contains(issue.Text, "10.0") {
		t.Errorf("в тексте должны быть числа из живых метрик (сумма 8.0, нужно 10.0): %s", issue.Text)
	}

	// Жёсткий отказ остаётся там, где он уместен: RAM неизвестна (узел не
	// сообщает системные метрики), значит гарантировать offload нечем.
	unknown := compositionProxyR95(t, 4096, 0)
	report = unknown.ValidateCompositionR95(compositionRequestR95(listenR95(t), 8<<30))
	if _, found := issueForR95(report, composition.CheckWeightsNeedRAMOffload); found {
		t.Errorf("при неизвестной RAM обещать offload нельзя: %s", report.String())
	}
	hard, found := issueForR95(report, composition.CheckVRAMSumInsufficient)
	if !found {
		t.Fatalf("без RAM нехватка VRAM — это Error: %s", report.String())
	}
	if hard.Level != composition.LevelError {
		t.Errorf("уровень = %q, ожидалось error", hard.Level)
	}

	// Та же пара узлов, но модель 2 ГиБ — влезает (need 2.5 ГиБ < 4 ГиБ),
	// и появляется подсказка «достаточно single».
	report = p.ValidateCompositionR95(compositionRequestR95(listenR95(t), 2<<30))
	if report.HasErrors() {
		t.Fatalf("модель 2 ГиБ влезает: %s", report.String())
	}
	if _, found := issueForR95(report, composition.CheckFitsSingle); !found {
		t.Errorf("должна быть подсказка «достаточно single»: %s", report.String())
	}
}

func TestValidateCompositionR95_TakesSizeFromProfile(t *testing.T) {
	// Размер в запросе не задан — берём из профиля балансера.
	p := compositionProxyR95(t, 6144, 16384)
	p.config.LlamaCppModelProfiles = map[string]types.LlamaCppModelProfile{
		"m-14b": {SizeBytes: 40 << 30}, // 40 ГиБ: не влезает никуда
	}

	report := p.ValidateCompositionR95(compositionRequestR95(listenR95(t), 0))
	if _, found := issueForR95(report, composition.CheckVRAMSumInsufficient); !found {
		t.Fatalf("размер должен браться из профиля: %s", report.String())
	}
}

func TestValidateCompositionR95_UnregisteredNode(t *testing.T) {
	p := compositionProxyR95(t, 6144, 16384)
	req := compositionRequestR95(listenR95(t), 8<<30)
	req.Group.Members[1].BackendID = "нет-такого-узла"

	issue, found := issueForR95(p.ValidateCompositionR95(req), composition.CheckNodeUnreachable)
	if !found {
		t.Fatal("несуществующий узел должен ловиться")
	}
	if !strings.Contains(issue.Text, "нет-такого-узла") {
		t.Errorf("текст должен называть узел: %s", issue.Text)
	}
}

func TestValidateCompositionR95_HomogeneousFromLabels(t *testing.T) {
	// «Личность» GPU берётся из меток бэкенда (gpu:sm_86 / gpu:sm_120).
	p := compositionProxyR95(t, 6144, 16384)
	req := compositionRequestR95(listenR95(t), 8<<30)
	req.RequireHomogeneous = true

	issue, found := issueForR95(p.ValidateCompositionR95(req), composition.CheckHomogeneous)
	if !found {
		t.Fatal("sm_86 + sm_120 при requireHomogeneous — предупреждение")
	}
	if !strings.Contains(issue.Text, "sm_86") || !strings.Contains(issue.Text, "sm_120") {
		t.Errorf("текст должен называть обе карты: %s", issue.Text)
	}
}

func TestValidateCompositionR95_TrafficUsesDefaultBandwidth(t *testing.T) {
	// Канал не измерен → берём 112 МБ/с (§3) и считаем трафик row split.
	p := compositionProxyR95(t, 6144, 16384)
	req := compositionRequestR95(listenR95(t), 8<<30)
	req.Group.Strategy = "row"
	req.Model.Layers = 48 // row: 4096×2×48 = 393 216 Б/токен → ~10.5 %

	issue, found := issueForR95(p.ValidateCompositionR95(req), composition.CheckTrafficRowHigh)
	if !found {
		t.Fatal("row split должен предупреждать о трафике")
	}
	gbps, _ := issue.Params["gbps"].(float64)
	// 112 МБ/с полезной нагрузки = 896 Мбит/с ≈ 0.9 Гбит/с (ориентир §3).
	if gbps < 0.85 || gbps > 0.95 {
		t.Errorf("по умолчанию должен использоваться 1 Гбит/с (≈0.9 Гбит/с полезной), получено %v", gbps)
	}
}

func TestValidateCompositionR95_BackendTypeFromBackends(t *testing.T) {
	// Тип узла берётся из зарегистрированного бэкенда: image_cpp не обслужит
	// текстовую модель, даже если оператор описал его в группе по ошибке.
	p := compositionProxyR95(t, 6144, 16384)
	p.mu.Lock()
	p.backends[r95WorkerID].Backend.Type = types.BackendTypeImage
	p.mu.Unlock()

	report := p.ValidateCompositionR95(compositionRequestR95(listenR95(t), 8<<30))
	issue, found := issueForR95(report, composition.CheckBackendType)
	if !found {
		t.Fatalf("image_cpp в текстовой группе — ошибка: %s", report.String())
	}
	if !strings.Contains(issue.Text, "image_cpp") {
		t.Errorf("текст должен называть тип узла: %s", issue.Text)
	}
}

func TestValidateCompositionR95_EmptyGroupIsReported(t *testing.T) {
	p := compositionProxyR95(t, 6144, 16384)
	report := p.ValidateCompositionR95(CompositionRequestR95{Lang: composition.LangEN})
	if report.Lang != composition.LangEN {
		t.Errorf("язык отчёта = %q", report.Lang)
	}
	if len(report.Issues) != 0 {
		t.Errorf("пустой состав не даёт замечаний (структурные ошибки — у конфиг-валидатора): %v", report.Issues)
	}
}

func TestCompositionConfigWarningsR95(t *testing.T) {
	// Структурные ошибки предлагаемой группы отдаются отдельным списком.
	cfg := types.BackendGroupConfig{
		ID: "broken", Model: "m", Strategy: "layer",
		Members: []types.BackendGroupMemberConfig{
			{BackendID: "a", Role: "worker", RPCEndpoint: "h:1"},
			{BackendID: "b", Role: "worker", RPCEndpoint: "h:2"},
		},
	}
	warnings := CompositionConfigWarningsR95(cfg)
	if len(warnings) == 0 {
		t.Fatal("группа без мастера должна давать предупреждения конфига")
	}
	joined := strings.Join(warnings, " | ")
	if !strings.Contains(joined, "master") {
		t.Errorf("должно быть сказано про мастера: %s", joined)
	}

	if got := CompositionConfigWarningsR95(types.BackendGroupConfig{}); got != nil {
		t.Errorf("пустая группа = нет предупреждений, получено %v", got)
	}
}

func TestValidateCompositionR95_NilProxy(t *testing.T) {
	var p *Proxy
	report := p.ValidateCompositionR95(CompositionRequestR95{})
	if !report.HasErrors() {
		t.Error("без балансера отчёт не может быть успешным")
	}
}
