package composition

// R95 (S4 плана 2026-10-07): единый валидатор композиции.
//
// Проверяется всё, что требует §7: 8 классов замечаний, уровень каждого
// (error/warning/hint), ЧИСЛА в тексте (а не «будет медленно»), и семантика
// СУММЫ VRAM по узлам, ради которой раскладка и существует.

import (
	"strings"
	"testing"
)

// validNodesR95 — два здоровых узла по 6 ГБ свободного VRAM.
// Для модели 8 ГиБ (need 10 ГиБ) суммы 12 ГиБ хватает, а каждый узел по
// отдельности — нет. Ровно тот случай, где раскладка уместна.
func validNodesR95() []Node {
	return []Node{
		{
			BackendID: "node-a", Role: "master", Status: "healthy",
			Registered: true, Healthy: true, VRAMKnown: true, RAMKnown: true,
			VRAMFreeMB: 6144, VRAMTotalMB: 8192, RAMFreeMB: 16384,
			LlamaCppVersion: "gguf-v0.19.0-1369-g1d2869c6e", GPUIdentity: "sm_86", BackendType: "llama_cpp",
		},
		{
			BackendID: "node-b", Role: "worker", Status: "healthy",
			RPCEndpoint: "192.0.2.11:50052", RPCChecked: true, RPCReachable: true,
			Registered: true, Healthy: true, VRAMKnown: true, RAMKnown: true,
			VRAMFreeMB: 6144, VRAMTotalMB: 8192, RAMFreeMB: 16384,
			LlamaCppVersion: "gguf-v0.19.0-1369-g1d2869c6e", GPUIdentity: "sm_120", BackendType: "llama_cpp",
		},
	}
}

func validRequestR95() Request {
	return Request{
		Lang: LangRU,
		Group: Group{
			ID:       "g1",
			Model:    "qwen3-14b",
			Strategy: StrategyLayer,
			Nodes:    validNodesR95(),
		},
		Model: Model{
			Name:         "qwen3-14b",
			BackendType:  "llama_cpp",
			SizeBytes:    8 << 30,
			SizeKnown:    true,
			HiddenSize:   4096,
			Layers:       48,
			BytesPerElem: 2,
		},
		BandwidthBytesPerSec: 112e6, // 1 Гбит/с: 112 МБ/с по §3
		TokensPerSecond:      30,
	}
}

func findIssue(report Report, check CheckID) (Issue, bool) {
	for _, issue := range report.Issues {
		if issue.Check == check {
			return issue, true
		}
	}
	return Issue{}, false
}

func TestValidate_ReadyGroup_NoErrors_R95(t *testing.T) {
	report := Validate(validRequestR95())
	if report.HasErrors() {
		t.Fatalf("готовая раскладка не должна давать ошибок: %s", report.String())
	}
	if len(report.Warnings()) != 0 {
		t.Errorf("неожиданные предупреждения: %s", report.String())
	}
	if report.GroupID != "g1" || report.Model != "qwen3-14b" {
		t.Errorf("отчёт должен эхо-повторять вход: %+v", report)
	}
}

func TestValidate_VRAMUsesSumAcrossNodes_R95(t *testing.T) {
	// Та же модель, но у каждого узла 6 ГиБ: 6 < 10 (need), а сумма 12 ≥ 10.
	// Проверка не должна ругаться — иначе раскладка «не собирается» там, где
	// она как раз и нужна.
	report := Validate(validRequestR95())
	if _, found := findIssue(report, CheckVRAMSumInsufficient); found {
		t.Fatalf("VRAM складывается по узлам: %s", report.String())
	}

	// Уменьшаем узлы до 2 ГиБ каждый (сумма 4 < 10) — теперь ошибка с числами.
	req := validRequestR95()
	for i := range req.Group.Nodes {
		req.Group.Nodes[i].VRAMFreeMB = 2048
	}
	report = Validate(req)
	issue, found := findIssue(report, CheckVRAMSumInsufficient)
	if !found {
		t.Fatalf("суммы 4 ГиБ на модель 8 ГиБ не хватает: %s", report.String())
	}
	if issue.Level != LevelError {
		t.Errorf("нехватка VRAM — ошибка, получено %q", issue.Level)
	}
	for _, want := range []string{"10.0", "4.0", "нужно", "доступно"} {
		if !strings.Contains(issue.Text, want) {
			t.Errorf("в тексте нет %q: %s", want, issue.Text)
		}
	}
	if got := issue.Params["needMB"]; got != 10240 {
		t.Errorf("needMB = %v, ожидалось 10240 (8 ГиБ × 1.25)", got)
	}
}

func TestValidate_NodeUnreachable_R95(t *testing.T) {
	req := validRequestR95()
	req.Group.Nodes[1].Registered = false

	report := Validate(req)
	issue, found := findIssue(report, CheckNodeUnreachable)
	if !found || issue.Level != LevelError {
		t.Fatalf("незарегистрированный узел — ошибка: %s", report.String())
	}
	if !strings.Contains(issue.Text, "node-b") || !strings.Contains(issue.Text, "не зарегистрирован") {
		t.Errorf("текст должен называть узел и причину: %s", issue.Text)
	}

	// Не healthy — тот же класс, но с указанием статуса.
	req = validRequestR95()
	req.Group.Nodes[0].Healthy = false
	req.Group.Nodes[0].Status = "offline"
	issue, found = findIssue(Validate(req), CheckNodeUnreachable)
	if !found || !strings.Contains(issue.Text, "offline") {
		t.Errorf("текст должен содержать статус: %v", issue.Text)
	}
}

func TestValidate_RPCUnreachable_R95(t *testing.T) {
	req := validRequestR95()
	req.Group.Nodes[1].RPCReachable = false
	req.Group.Nodes[1].RPCError = "connection refused"

	issue, found := findIssue(Validate(req), CheckRPCUnreachable)
	if !found || issue.Level != LevelError {
		t.Fatal("закрытый порт RPC — ошибка")
	}
	if !strings.Contains(issue.Text, "192.0.2.11:50052") {
		t.Errorf("текст должен называть endpoint: %s", issue.Text)
	}
	if !strings.Contains(issue.Text, "connection refused") {
		t.Errorf("текст должен нести причину отказа: %s", issue.Text)
	}

	// У мастера порт RPC не проверяется — замечания быть не должно.
	req = validRequestR95()
	req.Group.Nodes[0].RPCChecked = false
	req.Group.Nodes[0].RPCReachable = false
	if _, found := findIssue(Validate(req), CheckRPCUnreachable); found {
		t.Error("для узла без обязательного RPC-порта замечание не нужно")
	}
}

func TestValidate_VersionMismatch_R95(t *testing.T) {
	req := validRequestR95()
	req.Group.Nodes[1].LlamaCppVersion = "gguf-v0.19.0-1500-gdeadbeef"

	issue, found := findIssue(Validate(req), CheckVersionMismatch)
	if !found || issue.Level != LevelError {
		t.Fatal("разные коммиты llama.cpp — ошибка (§0.6)")
	}
	if !strings.Contains(issue.Text, "1d2869c6e") || !strings.Contains(issue.Text, "deadbeef") {
		t.Errorf("текст должен показывать обе версии: %s", issue.Text)
	}
}

func TestValidate_VersionUnknown_IsWarning_R95(t *testing.T) {
	// Сегодня узел версию не сообщает вовсе (её нет в метриках балансера).
	// Это НЕ ошибка: иначе любой существующий стенд блокировался бы валидатором.
	req := validRequestR95()
	for i := range req.Group.Nodes {
		req.Group.Nodes[i].LlamaCppVersion = ""
	}

	report := Validate(req)
	issue, found := findIssue(report, CheckVersionUnknown)
	if !found || issue.Level != LevelWarning {
		t.Fatalf("неизвестная версия — предупреждение: %s", report.String())
	}
	if report.HasErrors() {
		t.Errorf("неизвестная версия не должна блокировать сборку: %s", report.String())
	}
}

func TestValidate_KVCache_R95(t *testing.T) {
	req := validRequestR95()
	req.Model.NCtx = 65536
	req.Model.KVLayers = 48
	req.Model.KVHeads = 8
	req.Model.KVHeadDim = 128
	// perToken = 2*48*8*128*2 = 196 608 Б → на 65536 токенов ≈ 12.0 ГиБ
	// Свободно: RAM 32 ГиБ → влезает.
	if _, found := findIssue(Validate(req), CheckKVRAMInsufficient); found {
		t.Fatal("KV должен помещаться в 32 ГиБ RAM")
	}

	// Сжимаем RAM так, что KV не влезает.
	for i := range req.Group.Nodes {
		req.Group.Nodes[i].RAMFreeMB = 512
	}
	issue, found := findIssue(Validate(req), CheckKVRAMInsufficient)
	if !found || issue.Level != LevelError {
		t.Fatalf("нехватка памяти под KV — ошибка: %s", issue.Text)
	}
	if !strings.Contains(issue.Text, "65536") {
		t.Errorf("текст должен называть n_ctx: %s", issue.Text)
	}
	if !strings.Contains(issue.Text, "свободно") {
		t.Errorf("текст должен называть доступное: %s", issue.Text)
	}
}

func TestValidate_KVUnknown_IsHint_R95(t *testing.T) {
	req := validRequestR95()
	req.Model.NCtx = 65536 // параметры KV не заданы

	report := Validate(req)
	issue, found := findIssue(report, CheckKVUnknown)
	if !found || issue.Level != LevelHint {
		t.Fatalf("без n_kv_heads/head_dim проверка честно помечается непроверенной: %s", report.String())
	}
	if report.HasErrors() {
		t.Errorf("непроверенный KV не должен блокировать: %s", report.String())
	}
}

func TestValidate_TrafficRowWarning_R95(t *testing.T) {
	req := validRequestR95()
	req.Group.Strategy = StrategyRow
	req.Model.Layers = 48
	// row: 4096×2×48 = 393 216 Б/токен → 3.51 мс при 112 МБ/с →
	// +10.5 % к 33.3 мс на токен (30 tok/s) → предупреждение.
	issue, found := findIssue(Validate(req), CheckTrafficRowHigh)
	if !found {
		t.Fatal("row split дороже 10 % времени токена — предупреждение (§7)")
	}
	if issue.Level != LevelWarning {
		t.Errorf("уровень = %q, ожидалось warning", issue.Level)
	}
	for _, want := range []string{"row split", "10.5", "Гбит/с"} {
		if !strings.Contains(issue.Text, want) {
			t.Errorf("в тексте нет %q: %s", want, issue.Text)
		}
	}

	// layer split при тех же данных — предупреждения нет (это рабочий вариант).
	req.Group.Strategy = StrategyLayer
	if _, found := findIssue(Validate(req), CheckTrafficRowHigh); found {
		t.Error("layer split на 1 Гбит/с не должен предупреждать")
	}
}

func TestValidate_TrafficUnknownBandwidth_NoWarning_R95(t *testing.T) {
	req := validRequestR95()
	req.Group.Strategy = StrategyRow
	req.BandwidthBytesPerSec = 0 // канал не измерен

	if _, found := findIssue(Validate(req), CheckTrafficRowHigh); found {
		t.Error("без измеренной пропускной способности предупреждать нечем — не гадаем")
	}
}

func TestValidate_FitsSingle_Hint_R95(t *testing.T) {
	req := validRequestR95()
	req.Model.SizeBytes = 2 << 30 // 2 ГиБ → need 2.5 ГиБ, а у узла 6 ГиБ свободно

	issue, found := findIssue(Validate(req), CheckFitsSingle)
	if !found || issue.Level != LevelHint {
		t.Fatalf("модель, влезающая в узел, должна получать подсказку: %v", issue.Text)
	}
	if !strings.Contains(issue.Text, "single") {
		t.Errorf("подсказка должна предлагать single: %s", issue.Text)
	}
}

func TestValidate_BackendTypeMismatch_R95(t *testing.T) {
	req := validRequestR95()
	req.Group.Nodes[1].BackendType = "image_cpp"

	issue, found := findIssue(Validate(req), CheckBackendType)
	if !found || issue.Level != LevelError {
		t.Fatal("image_cpp не обслуживает текстовые модели — ошибка")
	}
	if !strings.Contains(issue.Text, "image_cpp") || !strings.Contains(issue.Text, "llama_cpp") {
		t.Errorf("текст должен называть оба типа: %s", issue.Text)
	}

	// Пустой тип у модели — проверка не применяется (запрос не называет тип).
	req = validRequestR95()
	req.Model.BackendType = ""
	req.Group.Nodes[1].BackendType = "image_cpp"
	if _, found := findIssue(Validate(req), CheckBackendType); found {
		t.Error("без требуемого типа проверка бессмысленна")
	}
}

func TestValidate_Homogeneous_R95(t *testing.T) {
	req := validRequestR95()
	req.Group.RequireHomogeneous = true // sm_86 + sm_120

	issue, found := findIssue(Validate(req), CheckHomogeneous)
	if !found || issue.Level != LevelWarning {
		t.Fatal("неоднородные GPU при requireHomogeneous — предупреждение")
	}
	if !strings.Contains(issue.Text, "sm_86") || !strings.Contains(issue.Text, "sm_120") {
		t.Errorf("текст должен называть обе «личности» GPU: %s", issue.Text)
	}

	// Правило не требует однородности — молчим (разные карты это норма).
	req.Group.RequireHomogeneous = false
	if _, found := findIssue(Validate(req), CheckHomogeneous); found {
		t.Error("без requireHomogeneous замечаний об однородности быть не должно")
	}
}

func TestValidate_EnglishTexts_R95(t *testing.T) {
	req := validRequestR95()
	req.Lang = LangEN
	req.Group.Nodes[1].Registered = false
	req.Model.SizeBytes = 40 << 30 // заведомо не влезает

	report := Validate(req)
	if report.Lang != LangEN {
		t.Fatalf("lang = %q", report.Lang)
	}
	ruIssue, _ := findIssue(Validate(func() Request { r := req; r.Lang = LangRU; return r }()), CheckNodeUnreachable)
	for _, check := range []CheckID{CheckNodeUnreachable, CheckVRAMSumInsufficient} {
		issue, found := findIssue(report, check)
		if !found {
			t.Fatalf("нет замечания %s", check)
		}
		if issue.Text == "" {
			t.Errorf("%s: пустой текст", check)
		}
		if strings.Contains(issue.Text, "узел") || strings.Contains(issue.Text, "нужно") {
			t.Errorf("%s: в en-тексте остался русский: %s", check, issue.Text)
		}
	}
	if ruIssue.Text == "" || strings.Contains(ruIssue.Text, "node ") {
		t.Errorf("ru-текст должен быть русским: %s", ruIssue.Text)
	}

	// Неизвестный язык → ru (не падаем и не отдаём пустой текст).
	req.Lang = "de"
	if got := Validate(req).Lang; got != LangRU {
		t.Errorf("неизвестный язык должен давать ru, получено %q", got)
	}
}

func TestValidate_Defaults_R95(t *testing.T) {
	// Defaults: safety factor 1.25 и prefill 512 подставляются, если не заданы.
	req := validRequestR95()
	req.VRAMSafetyFactor = 0
	req.PrefillTokens = 0
	req.Group.Strategy = StrategyRow
	req.Model.Layers = 48

	issue, found := findIssue(Validate(req), CheckTrafficRowHigh)
	if !found {
		t.Fatal("предупреждение о трафике должно считаться с prefill по умолчанию")
	}
	if got := issue.Params["prefillTokens"]; got != defaultPrefillTokens {
		t.Errorf("prefillTokens = %v, ожидалось %d", got, defaultPrefillTokens)
	}
	// 8 ГиБ × 1.25 = 10 ГиБ > суммы 12 ГиБ? Нет: 12 ≥ 10, ошибки быть не должно.
	if _, found := findIssue(Validate(req), CheckVRAMSumInsufficient); found {
		t.Error("без явного safety factor должен применяться 1.25 (сумма 12 ГиБ ≥ need 10 ГиБ)")
	}
}

func TestKVCacheBytes_R95(t *testing.T) {
	// GQA: 48 слоёв, 8 KV-голов, head_dim 128, f16 → 196 608 Б на токен.
	m := Model{KVLayers: 48, KVHeads: 8, KVHeadDim: 128, NCtx: 1024, KVBytesPerElem: 2}
	got, known := KVCacheBytes(m)
	if !known {
		t.Fatal("параметры заданы — размер должен считаться")
	}
	if want := int64(196608 * 1024); got != want {
		t.Errorf("KVCacheBytes = %d, ожидалось %d", got, want)
	}

	// Наивная формула по n_embd завысила бы на порядок — проверяем, что её нет:
	// при 8 KV-головах и 32 головах внимания размер должен отличаться в 4 раза.
	m2 := m
	m2.KVHeads = 32
	got2, _ := KVCacheBytes(m2)
	if got2 != got*4 {
		t.Errorf("KV должен считаться по KV-головам: %d vs %d", got2, got*4)
	}

	for _, broken := range []Model{
		{KVHeads: 8, KVHeadDim: 128, NCtx: 1024},   // нет слоёв
		{KVLayers: 48, KVHeadDim: 128, NCtx: 1024}, // нет голов
		{KVLayers: 48, KVHeads: 8, NCtx: 1024},     // нет head_dim
		{KVLayers: 48, KVHeads: 8, KVHeadDim: 128}, // нет n_ctx
	} {
		if _, known := KVCacheBytes(broken); known {
			t.Errorf("неполные параметры должны давать known=false: %+v", broken)
		}
	}
}

func TestReport_Helpers_R95(t *testing.T) {
	req := validRequestR95()
	req.Group.Nodes[1].Registered = false
	req.Group.Nodes[0].LlamaCppVersion = "" // → warning version_unknown
	req.Model.SizeBytes = 40 << 30          // → error vram_sum_insufficient
	report := Validate(req)

	if !report.HasErrors() {
		t.Fatal("отчёт должен содержать ошибки")
	}
	if len(report.Errors()) == 0 || len(report.Warnings()) == 0 {
		t.Errorf("нужны оба уровня: %+v", report.Issues)
	}
	if report.Errors()[0].Level != LevelError {
		t.Errorf("Errors() должен отдавать только error: %+v", report.Errors())
	}
	if !strings.Contains(report.String(), "composition g1") {
		t.Errorf("String() должен быть читаемым: %s", report.String())
	}
}
