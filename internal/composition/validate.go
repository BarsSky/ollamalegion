// Package composition — валидатор композиции кластера (R95, S4 плана
// docs/plans/2026-10-07-model-distribution-multi-gpu-next-session.md §7).
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ПАКЕТ. Проверки «соберётся ли раскладка» нужны ДВУМ
// потребителям с разной природой данных:
//   - сухому прогону в WebUI (S5/S6): оператор вводит состав кластера и хочет
//     увидеть ошибки ДО сохранения;
//   - резолверу размещения (S3): он уже сам помечает решение Degraded, но не
//     объясняет оператору весь набор проблем (версии, RAM под KV, трафик).
//
// Поэтому проверки здесь — ЧИСТЫЕ функции над явным входом (Request). Живые
// данные (метрики, health, доступность портов) собирает адаптер в
// internal/balancer/composition_r95.go — так же, как ResolvePlacementDecision
// отделён от Proxy.ResolvePlacement.
//
// ПРИНЦИП «НЕ ГАДАЕМ». Если данных нет (n_kv_heads неизвестны, узел не сообщает
// версию llama.cpp), проверка не выдумывает вердикт: она возвращает
// warning/hint «не проверено» с причиной. Ложная ошибка в валидаторе хуже
// пропущенной: оператор перестаёт ему верить.
package composition

import (
	"fmt"
	"strings"
)

// Level — важность замечания (терминология плана §7).
type Level string

const (
	// LevelError — композицию собирать нельзя.
	LevelError Level = "error"
	// LevelWarning — собрать можно, но оператор должен понимать последствия.
	LevelWarning Level = "warning"
	// LevelHint — подсказка («достаточно single»).
	LevelHint Level = "hint"
)

// CheckID — машиночитаемый идентификатор проверки.
//
// Нужен потому, что текст локализуется (ru/en), а UI и тесты должны опираться на
// стабильный ключ, а не на формулировку: иначе любая правка текста ломает
// WebUI-подсказки.
type CheckID string

const (
	// CheckNodeUnreachable — узел не зарегистрирован/не healthy.
	CheckNodeUnreachable CheckID = "node_unreachable"
	// CheckRPCUnreachable — порт RPC-сервера закрыт.
	CheckRPCUnreachable CheckID = "rpc_unreachable"
	// CheckVersionMismatch — узлы собраны из разных коммитов llama.cpp.
	CheckVersionMismatch CheckID = "version_mismatch"
	// CheckVersionUnknown — версию не сообщает хотя бы один узел.
	CheckVersionUnknown CheckID = "version_unknown"
	// CheckVRAMSumInsufficient — сумма свободного VRAM узлов меньше весов с запасом.
	CheckVRAMSumInsufficient CheckID = "vram_sum_insufficient"
	// CheckKVRAMInsufficient — KV для запрошенного n_ctx не помещается.
	CheckKVRAMInsufficient CheckID = "kv_ram_insufficient"
	// CheckWeightsNeedRAMOffload — веса НЕ влезают в суммарную VRAM группы, но
	// влезают с учётом свободной RAM узлов (mmap / частичный offload).
	//
	// R249 (2026-10-28): живая жалоба оператора — «объединение бэкендов судит
	// только по свободной видеопамяти, а не по видеопамяти И оперативной»:
	// группа отвергалась как ошибка там, где модель реально запускается
	// (медленнее, но запускается). Теперь это Warning с явным текстом о
	// деградации, а не Error.
	CheckWeightsNeedRAMOffload CheckID = "weights_need_ram_offload"
	// CheckKVUnknown — KV проверить нечем (нет n_kv_heads/head_dim).
	CheckKVUnknown CheckID = "kv_unknown"
	// CheckTrafficRowHigh — row split дороже 10 % времени токена.
	CheckTrafficRowHigh CheckID = "traffic_row_high"
	// CheckFitsSingle — модель влезает в один узел.
	CheckFitsSingle CheckID = "fits_single"
	// CheckBackendType — тип бэкенда несовместим с моделью.
	CheckBackendType CheckID = "backend_type_mismatch"
	// CheckHomogeneous — неоднородные GPU там, где требуется однородность.
	CheckHomogeneous CheckID = "homogeneous"
)

// Node — то, что валидатор знает об узле композиции.
//
// Порядок полей — по требованию fieldalignment.
type Node struct {
	BackendID string
	Role      string
	Status    string
	// RPCEndpoint — host:port RPC-сервера (у воркера он обязателен).
	RPCEndpoint string
	// RPCError — почему порт не ответил (для текста).
	RPCError string
	// LlamaCppVersion — версия/коммит сборки, как её сообщает узел.
	LlamaCppVersion string
	// GPUIdentity — «личность» GPU для проверки однородности (sm_86, vram:8192).
	GPUIdentity string
	// GPUName — человекочитаемое имя карты (для текста), если известно.
	GPUName string
	// BackendType — ollama | llama_cpp | image_cpp.
	BackendType string
	VRAMFreeMB  int
	RAMFreeMB   int
	VRAMTotalMB int
	// Layers — сколько слоёв модели ляжет на этот узел (0 = неизвестно, делим
	// поровну между участниками раскладки).
	Layers     int
	Registered bool
	Healthy    bool
	// RPCChecked — пробовали ли вообще порт (у мастера он не нужен).
	RPCChecked   bool
	RPCReachable bool
	VRAMKnown    bool
	RAMKnown     bool
}

// Model — то, что валидатор знает о модели.
//
// Порядок полей — по требованию fieldalignment.
type Model struct {
	Name string
	// BackendType — тип бэкенда, который требует запрос ("" = любой текстовый).
	BackendType string
	// KV — параметры KV-кэша. Нули означают «неизвестно»: проверка n_ctx тогда
	// честно помечается как непроверенная, а не считается пройденной.
	KVLayers       int
	KVHeads        int
	KVHeadDim      int
	KVBytesPerElem int
	SizeBytes      int64
	HiddenSize     int
	Layers         int
	NCtx           int
	BytesPerElem   int
	SizeKnown      bool
}

// Group — проверяемая композиция (логический бэкенд).
type Group struct {
	ID       string
	Model    string
	Strategy string
	Nodes    []Node
	// RequireHomogeneous — правило auto.requireHomogeneous.
	RequireHomogeneous bool
}

// Request — вход сухого прогона.
//
// Порядок полей — по требованию fieldalignment.
type Request struct {
	Group Group
	Model Model
	// Lang — язык текстов ("ru"/"en"); неизвестный → ru.
	Lang string
	// PrefillTokens — длина префилла для оценки трафика (по умолчанию 512).
	PrefillTokens int
	// TokensPerSecond — скорость декода, от которой считается доля трафика
	// (0 = не считать долю, только абсолютную задержку).
	TokensPerSecond float64
	// BandwidthBytesPerSec — полезная пропускная способность канала
	// (1 Гбит/с ≈ 112 МБ/с по §3 плана; 0 = не считать трафик).
	BandwidthBytesPerSec float64
	// VRAMSafetyFactor — запас к весам (0 → 1.25, как в placement-fit).
	VRAMSafetyFactor float64
}

// Issue — одно замечание валидатора.
//
// Params несёт ЧИСЛА для машиночитаемых потребителей (UI рисует свои подписи по
// CheckID + Params), Text — уже отрендеренная строка для логов и CLI.
//
// Порядок полей — по требованию fieldalignment.
type Issue struct {
	Check  CheckID        `json:"check"`
	Level  Level          `json:"level"`
	Target string         `json:"target,omitempty"`
	Text   string         `json:"text"`
	Params map[string]any `json:"params,omitempty"`
}

// Report — результат сухого прогона.
type Report struct {
	Issues []Issue `json:"issues"`
	Lang   string  `json:"lang"`
	// GroupID/Model — эхо входа: UI показывает, к чему относится отчёт.
	GroupID string `json:"groupId,omitempty"`
	Model   string `json:"model"`
}

// Errors — замечания уровня error.
func (r Report) Errors() []Issue {
	out := make([]Issue, 0, len(r.Issues))
	for _, issue := range r.Issues {
		if issue.Level == LevelError {
			out = append(out, issue)
		}
	}
	return out
}

// Warnings — замечания уровня warning.
func (r Report) Warnings() []Issue {
	out := make([]Issue, 0, len(r.Issues))
	for _, issue := range r.Issues {
		if issue.Level == LevelWarning {
			out = append(out, issue)
		}
	}
	return out
}

// Hints — подсказки уровня hint.
func (r Report) Hints() []Issue {
	out := make([]Issue, 0, len(r.Issues))
	for _, issue := range r.Issues {
		if issue.Level == LevelHint {
			out = append(out, issue)
		}
	}
	return out
}

// allCheckIDs — полный список проверок §7 (для UI-легенды и тестов покрытия).
var allCheckIDs = []CheckID{
	CheckNodeUnreachable,
	CheckRPCUnreachable,
	CheckVersionMismatch,
	CheckVersionUnknown,
	CheckVRAMSumInsufficient,
	CheckWeightsNeedRAMOffload,
	CheckKVRAMInsufficient,
	CheckKVUnknown,
	CheckTrafficRowHigh,
	CheckFitsSingle,
	CheckBackendType,
	CheckHomogeneous,
}

// AllChecks возвращает идентификаторы всех проверок валидатора.
//
// UI по нему может показать оператору, ЧТО именно проверяется, даже если сейчас
// замечаний нет: «проверок 11, ошибок 0» читается лучше, чем пустой блок.
func AllChecks() []CheckID {
	out := make([]CheckID, len(allCheckIDs))
	copy(out, allCheckIDs)
	return out
}

// HasErrors — можно ли собирать композицию.
func (r Report) HasErrors() bool {
	for _, issue := range r.Issues {
		if issue.Level == LevelError {
			return true
		}
	}
	return false
}

// Validate прогоняет все проверки §7 по явному входу.
//
// Порядок замечаний детерминирован (проверки идут фиксированным списком), потому
// что UI показывает их как список, а тесты сверяют состав.
func Validate(req Request) Report {
	report := Report{
		Lang:    NormalizeLang(req.Lang),
		GroupID: req.Group.ID,
		Model:   req.Model.Name,
	}
	if req.VRAMSafetyFactor <= 0 {
		req.VRAMSafetyFactor = defaultVRAMSafetyFactor
	}
	if req.PrefillTokens <= 0 {
		req.PrefillTokens = defaultPrefillTokens
	}

	report.Issues = append(report.Issues, checkNodes(req)...)
	report.Issues = append(report.Issues, checkVersions(req)...)
	report.Issues = append(report.Issues, checkBackendTypes(req)...)
	report.Issues = append(report.Issues, checkVRAMSum(req)...)
	report.Issues = append(report.Issues, checkKV(req)...)
	report.Issues = append(report.Issues, checkTraffic(req)...)
	report.Issues = append(report.Issues, checkFitsSingle(req)...)
	report.Issues = append(report.Issues, checkHomogeneous(req)...)
	return report
}

// checkNodes — узел недоступен / порт RPC закрыт (§7, строки 1).
func checkNodes(req Request) []Issue {
	var issues []Issue
	for _, node := range req.Group.Nodes {
		if !node.Registered {
			issues = append(issues, newIssue(req, CheckNodeUnreachable, LevelError, node.BackendID, map[string]any{
				"backend": node.BackendID,
				"role":    node.Role,
				"reason":  "unregistered",
			}))
			continue
		}
		if !node.Healthy {
			issues = append(issues, newIssue(req, CheckNodeUnreachable, LevelError, node.BackendID, map[string]any{
				"backend": node.BackendID,
				"role":    node.Role,
				"status":  node.Status,
				"reason":  "unhealthy",
			}))
			continue
		}
		if node.RPCChecked && !node.RPCReachable {
			issues = append(issues, newIssue(req, CheckRPCUnreachable, LevelError, node.BackendID, map[string]any{
				"backend":  node.BackendID,
				"endpoint": node.RPCEndpoint,
				"error":    node.RPCError,
			}))
		}
	}
	return issues
}

// checkVersions — одинаковый коммит llama.cpp на обеих сторонах RPC (§0.6, §7).
//
// Разные CMAKE_CUDA_ARCHITECTURES — норма (sm_86 у 3070, sm_120 у 5060), а разные
// ИСХОДНИКИ — нет: протокол RPC версионно-чувствителен, и несовпадение даёт
// таймаут загрузки вместо понятной ошибки.
func checkVersions(req Request) []Issue {
	byVersion := map[string][]string{}
	var unknown []string
	for _, node := range req.Group.Nodes {
		if !node.Registered || !node.Healthy {
			continue
		}
		version := strings.TrimSpace(node.LlamaCppVersion)
		if version == "" {
			unknown = append(unknown, node.BackendID)
			continue
		}
		byVersion[version] = append(byVersion[version], node.BackendID)
	}

	var issues []Issue
	if len(byVersion) > 1 {
		versions := make([]string, 0, len(byVersion))
		for version := range byVersion {
			versions = append(versions, version)
		}
		sortStrings(versions)
		parts := make([]string, 0, len(versions))
		for _, version := range versions {
			ids := byVersion[version]
			sortStrings(ids)
			parts = append(parts, version+"="+strings.Join(ids, ","))
		}
		issues = append(issues, newIssue(req, CheckVersionMismatch, LevelError, req.Group.ID, map[string]any{
			"versions": parts,
			"detail":   strings.Join(parts, "; "),
		}))
	}
	if len(unknown) > 0 && len(req.Group.Nodes) > 1 {
		sortStrings(unknown)
		// Уровень warning, а не error: сегодня узел вообще не сообщает версию
		// (в метриках балансера её нет), и error заблокировал бы любой стенд.
		issues = append(issues, newIssue(req, CheckVersionUnknown, LevelWarning, req.Group.ID, map[string]any{
			"backends": unknown,
			"detail":   strings.Join(unknown, ", "),
		}))
	}
	return issues
}

// checkBackendTypes — тип бэкенда несовместим с моделью (§7, строка 7).
func checkBackendTypes(req Request) []Issue {
	want := strings.TrimSpace(req.Model.BackendType)
	if want == "" {
		return nil
	}
	var issues []Issue
	for _, node := range req.Group.Nodes {
		nodeType := NormalizeBackendType(node.BackendType)
		if nodeType == want {
			continue
		}
		issues = append(issues, newIssue(req, CheckBackendType, LevelError, node.BackendID, map[string]any{
			"backend":   node.BackendID,
			"nodeType":  nodeType,
			"modelType": want,
		}))
	}
	return issues
}

// checkVRAMSum — сумма свободного VRAM узлов против весов с запасом (§7).
//
// Именно СУММА: при layer/row split веса лежат на разных картах. Для сравнения
// кладём в Params и максимум по узлу — оператору нужно видеть, что дело не в
// одной карте (иначе он пойдёт покупать вторую такую же зря).
func checkVRAMSum(req Request) []Issue {
	if !req.Model.SizeKnown || req.Model.SizeBytes <= 0 {
		return nil
	}
	sumFree := 0
	maxFree := 0
	known := 0
	ramFree := 0
	ramKnown := false
	for _, node := range req.Group.Nodes {
		if !node.Registered || !node.Healthy {
			continue
		}
		if node.RAMKnown {
			ramFree += node.RAMFreeMB
			ramKnown = true
		}
		if !node.VRAMKnown {
			continue
		}
		known++
		sumFree += node.VRAMFreeMB
		if node.VRAMFreeMB > maxFree {
			maxFree = node.VRAMFreeMB
		}
	}
	if known == 0 {
		return nil
	}
	needMB := int(float64(req.Model.SizeBytes) / (1024 * 1024) * req.VRAMSafetyFactor)
	if sumFree >= needMB {
		return nil
	}
	// R249 (2026-10-28): раньше сравнивалось ТОЛЬКО с суммарной VRAM, и группа
	// признавалась нерабочей там, где веса помещаются в VRAM+RAM (mmap/частичный
	// offload) и модель реально запускается — медленнее, но запускается.
	// Теперь это разные исходы: с RAM — Warning (деградация), без RAM — Error.
	if ramKnown && sumFree+ramFree >= needMB {
		return []Issue{newIssue(req, CheckWeightsNeedRAMOffload, LevelWarning, req.Group.ID, map[string]any{
			"needMB":      needMB,
			"needBytes":   int64(float64(req.Model.SizeBytes) * req.VRAMSafetyFactor),
			"weightsMB":   int(req.Model.SizeBytes / (1024 * 1024)),
			"availableMB": sumFree,
			"ramFreeMB":   ramFree,
			"combinedMB":  sumFree + ramFree,
			"maxNodeMB":   maxFree,
			"nodeCount":   known,
			"safety":      req.VRAMSafetyFactor,
		})}
	}
	return []Issue{newIssue(req, CheckVRAMSumInsufficient, LevelError, req.Group.ID, map[string]any{
		"needMB":      needMB,
		"needBytes":   int64(float64(req.Model.SizeBytes) * req.VRAMSafetyFactor),
		"weightsMB":   int(req.Model.SizeBytes / (1024 * 1024)),
		"availableMB": sumFree,
		"maxNodeMB":   maxFree,
		"nodeCount":   known,
		"safety":      req.VRAMSafetyFactor,
		"ramFreeMB":   ramFree,
		"ramKnown":    ramKnown,
	})}
}

// checkKV — хватает ли памяти под KV-кэш при заданном n_ctx (§7).
//
// Доступная память под KV: свободная RAM всех узлов плюс остаток VRAM после
// весов. Для раскладки это корректно: KV живёт на тех же узлах, что и слои, а
// mmap позволяет вынести его в RAM.
func checkKV(req Request) []Issue {
	m := req.Model
	if m.NCtx <= 0 {
		return nil
	}
	kvBytes, known := KVCacheBytes(m)
	if !known {
		return []Issue{newIssue(req, CheckKVUnknown, LevelHint, req.Group.ID, map[string]any{
			"nCtx": m.NCtx,
		})}
	}

	ramFree, ramKnown := 0, false
	vramFree, vramKnown := 0, false
	for _, node := range req.Group.Nodes {
		if !node.Registered || !node.Healthy {
			continue
		}
		if node.VRAMKnown {
			vramFree += node.VRAMFreeMB
			vramKnown = true
		}
		if node.RAMKnown {
			ramFree += node.RAMFreeMB
			ramKnown = true
		}
	}
	if !ramKnown && !vramKnown {
		return []Issue{newIssue(req, CheckKVUnknown, LevelHint, req.Group.ID, map[string]any{
			"nCtx": m.NCtx,
		})}
	}

	needWeightsMB := int64(0)
	if m.SizeKnown {
		needWeightsMB = int64(float64(m.SizeBytes) / (1024 * 1024) * req.VRAMSafetyFactor)
	}
	vramLeftMB := int64(vramFree) - needWeightsMB
	if vramLeftMB < 0 {
		vramLeftMB = 0
	}
	availableBytes := (int64(ramFree)+vramLeftMB)*1024*1024 + 0
	if availableBytes >= kvBytes {
		return nil
	}
	return []Issue{newIssue(req, CheckKVRAMInsufficient, LevelError, req.Group.ID, map[string]any{
		"nCtx":           m.NCtx,
		"kvBytes":        kvBytes,
		"availableBytes": availableBytes,
		"ramFreeMB":      ramFree,
		"vramLeftMB":     vramLeftMB,
		"weightsMB":      needWeightsMB,
	})}
}

// checkTraffic — оценка сетевого бюджета раскладки (§3, §7).
//
// Warning только для row split: layer split на 1 Гбит/с даёт <0.5 % времени
// токена, и предупреждать там не о чем.
func checkTraffic(req Request) []Issue {
	strategy := NormalizeStrategy(req.Group.Strategy)
	if strategy != StrategyRow {
		return nil
	}
	estimate := EstimateTraffic(TrafficInput{
		Strategy:             strategy,
		HiddenSize:           req.Model.HiddenSize,
		Layers:               req.Model.Layers,
		BytesPerElem:         req.Model.BytesPerElem,
		PrefillTokens:        req.PrefillTokens,
		BandwidthBytesPerSec: req.BandwidthBytesPerSec,
		TokensPerSecond:      req.TokensPerSecond,
	})
	if !estimate.Known {
		return nil
	}
	if estimate.TokenOverheadPercent <= rowSplitWarnPercent || estimate.TokenOverheadPercent < 0 {
		return nil
	}
	return []Issue{newIssue(req, CheckTrafficRowHigh, LevelWarning, req.Group.ID, map[string]any{
		"strategy":      strategy,
		"overheadMs":    estimate.TokenOverheadMs,
		"overheadPct":   estimate.TokenOverheadPercent,
		"prefillMs":     estimate.PrefillMs,
		"prefillTokens": req.PrefillTokens,
		"bytesPerToken": estimate.BytesPerToken,
		"gbps":          req.BandwidthBytesPerSec * 8 / 1e9,
	})}
}

// checkFitsSingle — подсказка «достаточно single» (§7).
//
// Модель, влезающая в ОДИН узел, не нуждается в раскладке: раскладка добавит
// сетевую задержку и хрупкость (падение узла = падение модели) без выигрыша.
func checkFitsSingle(req Request) []Issue {
	if !req.Model.SizeKnown || len(req.Group.Nodes) < 2 {
		return nil
	}
	needMB := float64(req.Model.SizeBytes) / (1024 * 1024) * req.VRAMSafetyFactor
	bestID := ""
	bestFree := 0
	for _, node := range req.Group.Nodes {
		if !node.Registered || !node.Healthy || !node.VRAMKnown {
			continue
		}
		if float64(node.VRAMFreeMB) >= needMB && node.VRAMFreeMB > bestFree {
			bestID, bestFree = node.BackendID, node.VRAMFreeMB
		}
	}
	if bestID == "" {
		return nil
	}
	return []Issue{newIssue(req, CheckFitsSingle, LevelHint, bestID, map[string]any{
		"backend":     bestID,
		"availableMB": bestFree,
		"needMB":      int(needMB),
	})}
}

// checkHomogeneous — неоднородные GPU при requireHomogeneous (§7).
func checkHomogeneous(req Request) []Issue {
	if !req.Group.RequireHomogeneous || len(req.Group.Nodes) < 2 {
		return nil
	}
	groups := map[string][]string{}
	for _, node := range req.Group.Nodes {
		if !node.Registered || !node.Healthy {
			continue
		}
		groups[node.GPUIdentity] = append(groups[node.GPUIdentity], node.BackendID)
	}
	if len(groups) <= 1 {
		return nil
	}
	// Пары «личность GPU → узлы» передаём структурой, а не готовой строкой:
	// подпись «неизвестно» локализуется в renderText, и собирать её здесь
	// значило бы зашить язык в валидатор.
	identities := make([]string, 0, len(groups))
	for identity := range groups {
		identities = append(identities, identity)
	}
	sortStrings(identities)
	pairs := make([]gpuGroupParam, 0, len(identities))
	rawParts := make([]string, 0, len(identities))
	for _, identity := range identities {
		ids := groups[identity]
		sortStrings(ids)
		pairs = append(pairs, gpuGroupParam{Identity: identity, Backends: ids})
		rawParts = append(rawParts, identity+"="+strings.Join(ids, ","))
	}
	return []Issue{newIssue(req, CheckHomogeneous, LevelWarning, req.Group.ID, map[string]any{
		"groups": pairs,
		"detail": strings.Join(rawParts, "; "),
	})}
}

// newIssue собирает замечание с уже отрендеренным текстом.
func newIssue(req Request, check CheckID, level Level, target string, params map[string]any) Issue {
	lang := NormalizeLang(req.Lang)
	return Issue{
		Check:  check,
		Level:  level,
		Target: target,
		Params: params,
		Text:   renderText(check, lang, params),
	}
}

// KVCacheBytes — размер KV-кэша для n_ctx.
//
// Формула: 2 (K и V) × слоёв × токенов × KV-голов × head_dim × байт на элемент.
// GQA-модели хранят KV только по KV-головам, поэтому «наивная» формула по
// n_embd завышает размер на порядок (та же ошибка, что описана в R83 §3.2 для
// cppworker). known=false — если хотя бы один множитель неизвестен.
func KVCacheBytes(m Model) (int64, bool) {
	if m.NCtx <= 0 || m.KVLayers <= 0 || m.KVHeads <= 0 || m.KVHeadDim <= 0 {
		return 0, false
	}
	bytesPerElem := m.KVBytesPerElem
	if bytesPerElem <= 0 {
		bytesPerElem = 2 // f16 — дефолт llama.cpp
	}
	perToken := int64(2) * int64(m.KVLayers) * int64(m.KVHeads) * int64(m.KVHeadDim) * int64(bytesPerElem)
	return perToken * int64(m.NCtx), true
}

// sortStrings — локальная сортировка, чтобы не тянуть "sort" в каждый вызов
// (списки крошечные, зато порядок замечаний детерминирован).
func sortStrings(list []string) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j] < list[j-1]; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}

// String — компактное представление отчёта (логи, CLI).
func (r Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "composition %s / %s: issues=%d", r.GroupID, r.Model, len(r.Issues))
	for _, issue := range r.Issues {
		fmt.Fprintf(&b, "\n  [%s] %s: %s", issue.Level, issue.Check, issue.Text)
	}
	return b.String()
}
