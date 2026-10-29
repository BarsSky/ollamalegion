// Package balancer — R96 (S5 плана 2026-10-07): предлагаемый план размещения.
//
// План §6 (S5): «Заменить шаг „выбор режима“ на „состав кластера“: обнаружение
// бэкендов → распределение моделей → проверка → сохранение политики» и
// «(3) предлагаемый план (single/replicated/sharded/pool) с числами,
// (4) предупреждения, (5) запись balancing.placement».
//
// ПОЧЕМУ ЛОГИКА ЗДЕСЬ, А НЕ В JS МАСТЕРА. Правило стенда — «WebUI только
// предлагает, никогда не применяет». Но ЧТО предлагать — это решение о
// размещении, и оно должно считаться там же, где исполняется: в балансере. Иначе
// мастер и резолвер разойдутся (мастер посчитает «влезает» по своим формулам, а
// резолвер откажет 503-й). Здесь используется тот же VRAM-fit, что у auto
// (placementFitForModel), те же формулы трафика (§3, internal/composition) и тот
// же валидатор композиции (§7). JS остаётся рисование экранов и запись ответа.
//
// ЭНДПОИНТ НИЧЕГО НЕ МЕНЯЕТ: он возвращает готовый фрагмент `balancing.placement`
// (+ `balancing.backendGroups`), который мастер запишет только после подтверждения
// оператора.
package balancer

import (
	"fmt"
	"sort"
	"strings"

	"ollama-loadbalancer/internal/composition"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// defaultRPCPortR96 — порт RPC-сервера по умолчанию (§5 плана: «RPC-канал (50052)»).
//
// Именно УГАДЫВАЕТСЯ в предложении и помечается note: реальный порт знает только
// оператор/агент, и валидатор композиции скажет, если он закрыт.
const defaultRPCPortR96 = 50052

// SetupPlanModelR96 — модель в запросе плана. Обязательно только имя: остальное
// (размер, размерности) балансер возьмёт из профиля/метрик, если знает.
//
// Порядок полей — по требованию fieldalignment.
type SetupPlanModelR96 struct {
	Name       string `json:"name"`
	SizeBytes  int64  `json:"sizeBytes"`
	HiddenSize int    `json:"hiddenSize"`
	Layers     int    `json:"layers"`
	NCtx       int    `json:"nCtx"`
	KVLayers   int    `json:"kvLayers"`
	KVHeads    int    `json:"kvHeads"`
	KVHeadDim  int    `json:"kvHeadDim"`
}

// SetupPlanRequestR96 — тело POST /api/v1/placement/plan.
//
// Порядок полей — по требованию fieldalignment.
type SetupPlanRequestR96 struct {
	// Models — что планировать; пусто = все известные балансеру модели.
	Models []SetupPlanModelR96 `json:"models"`
	// Lang — язык текстов замечаний (ru/en).
	Lang string `json:"lang"`
	// BandwidthMBps — измеренная пропускная способность (0 → 112 МБ/с, §3).
	BandwidthMBps float64 `json:"bandwidthMBps"`
	// PrefillTokens — длина префилла для оценки трафика (0 → 512).
	PrefillTokens int `json:"prefillTokens"`
	// TokensPerSecond — скорость декода для доли времени токена.
	TokensPerSecond float64 `json:"tokensPerSecond"`
	// RequireHomogeneous — требовать одинаковые GPU в группе.
	RequireHomogeneous bool `json:"requireHomogeneous"`
	// AllowDegraded — писать в политику auto.allowDegraded.
	AllowDegraded bool `json:"allowDegraded"`
	// SingleOnly — не предлагать копии (replicated) даже когда модель влезает в
	// несколько узлов. Дефолт false: §8 плана — «replicated даст ×2 пропускной
	// способности», и мастер должен это предлагать. Отдельное поле, а не
	// PreferReplicated, потому что у bool нулевое значение false, и «дефолт true»
	// через него не выражается без указателя.
	SingleOnly bool `json:"singleOnly"`
}

// SetupPlanTrafficR96 — оценка трафика раскладки для обеих стратегий: мастер
// показывает обе, чтобы оператор выбирал осознанно (§3).
type SetupPlanTrafficR96 struct {
	Layer composition.TrafficEstimate `json:"layer"`
	Row   composition.TrafficEstimate `json:"row"`
}

// SetupPlanItemR96 — предложение по одной модели.
//
// Порядок полей — по требованию fieldalignment (срезы и указатели, затем строки
// и структуры, затем числа, затем bool).
type SetupPlanItemR96 struct {
	Issues    []composition.Issue       `json:"issues"`
	Notes     []string                  `json:"notes"`
	Group     *types.BackendGroupConfig `json:"backendGroup,omitempty"`
	Traffic   *SetupPlanTrafficR96      `json:"traffic,omitempty"`
	Model     string                    `json:"model"`
	Strategy  string                    `json:"strategy"`
	Source    string                    `json:"source"`
	Reason    string                    `json:"reason"`
	GroupID   string                    `json:"groupId,omitempty"`
	Rule      types.PlacementModelRule  `json:"rule"`
	SizeGB    float64                   `json:"sizeGB"`
	NeedGB    float64                   `json:"needGB"`
	MaxNodeGB float64                   `json:"maxNodeGB"`
	SumFreeGB float64                   `json:"sumFreeGB"`
	KnownSize bool                      `json:"knownSize"`
	Degraded  bool                      `json:"degraded"`
}

// SetupPlanR96 — ответ целиком: предложения + готовый фрагмент конфига.
//
// Порядок полей — по требованию fieldalignment.
type SetupPlanR96 struct {
	Lang          string                     `json:"lang"`
	Plans         []SetupPlanItemR96         `json:"plans"`
	Placement     types.PlacementSettings    `json:"placement"`
	BackendGroups []types.BackendGroupConfig `json:"backendGroups"`
	// Warnings — структурные ошибки предложенных групп (нет rpcEndpoint и т.п.).
	Warnings []string `json:"warnings"`
	// Hint — что мастер должен показать внизу экрана.
	Hint string `json:"hint"`
	// Discovered — сколько моделей найдено на стенде (когда models[] не задан).
	Discovered int `json:"discoveredModels"`
}

// PlacementPlanR96 строит предложение по составу кластера.
func (p *Proxy) PlacementPlanR96(req SetupPlanRequestR96) SetupPlanR96 {
	result := SetupPlanR96{
		Lang:          composition.NormalizeLang(req.Lang),
		Plans:         []SetupPlanItemR96{},
		BackendGroups: []types.BackendGroupConfig{},
		Hint: "Это предложение, а не применение: политика записывается отдельной " +
			"операцией конфига после подтверждения. pool для виртуальных алиасов " +
			"мастер настраивает в разделе виртуальных моделей.",
	}
	if p == nil || p.config == nil {
		result.Warnings = append(result.Warnings, "балансер недоступен")
		return result
	}

	models := p.planModelsR96(req.Models)
	result.Discovered = len(models)

	settings := types.PlacementSettings{
		Enabled:  true,
		Fallback: types.PlacementFallbackError,
	}
	for _, planned := range models {
		item := p.planItemR96(planned, req)
		if item == nil {
			continue
		}
		settings.Models = append(settings.Models, item.Rule)
		if item.Group != nil {
			result.BackendGroups = append(result.BackendGroups, *item.Group)
		}
		result.Plans = append(result.Plans, *item)
	}
	result.Placement = settings

	if len(result.BackendGroups) > 0 {
		result.Warnings = append(result.Warnings,
			types.ValidateBackendGroups(result.BackendGroups)...)
	}

	logger.Get().Infow("placement plan built (dry proposal)",
		"models", len(result.Plans), "groups", len(result.BackendGroups),
		"warnings", len(result.Warnings))
	return result
}

// planItemR96 — предложение по одной модели.
//
// Порядок решений (§8 «матрица что даёт какой вариант»):
//  1. размер неизвестен → single + note (не гадаем);
//  2. влезает в один узел → replicated, если влезает в ≥2 (пропускная способность
//     ×N), иначе single;
//  3. не влезает ни в один, но влезает в СУММУ ≥2 узлов → sharded + предложенная
//     группа (мастер + воркеры);
//  4. не влезает никуда → single с числами и degraded (честный отказ увидит
//     резолвер, а не оператор потом).
func (p *Proxy) planItemR96(planned SetupPlanModelR96, req SetupPlanRequestR96) *SetupPlanItemR96 {
	model := strings.TrimSpace(planned.Name)
	if model == "" {
		return nil
	}
	sizeBytes := planned.SizeBytes
	if sizeBytes <= 0 {
		sizeBytes = p.getModelSizeBytes(model)
	}
	lang := composition.NormalizeLang(req.Lang)

	item := &SetupPlanItemR96{
		Model:     model,
		Source:    "proposal",
		Notes:     []string{},
		Issues:    []composition.Issue{},
		KnownSize: sizeBytes > 0,
	}
	const gb = 1024 * 1024 * 1024
	needBytes := float64(sizeBytes) * placementVRAMSafetyFactor
	item.SizeGB = float64(sizeBytes) / gb
	item.NeedGB = needBytes / gb

	candidates := p.planCandidatesR96()
	sumFree, maxFree := 0, 0
	for _, cand := range candidates {
		sumFree += cand.vramFreeMB
		if cand.vramFreeMB > maxFree {
			maxFree = cand.vramFreeMB
		}
	}
	item.SumFreeGB = float64(sumFree) / 1024
	item.MaxNodeGB = float64(maxFree) / 1024

	fitting := 0
	for _, cand := range candidates {
		if float64(cand.vramFreeMB)*1024*1024 >= needBytes {
			fitting++
		}
	}
	fitsOneNode := item.KnownSize && float64(maxFree)*1024*1024 >= needBytes
	fitsSum := item.KnownSize && float64(sumFree)*1024*1024 >= needBytes

	switch {
	case !item.KnownSize:
		item.Strategy = string(types.PlacementSingle)
		item.Degraded = true
		item.Reason = "размер модели неизвестен — предложить раскладку нельзя " +
			"(нужен профиль sizeBytes или метрики загруженной модели)"
		item.Notes = append(item.Notes, noteR96(lang, "size_unknown"))

	case fitsOneNode && !req.SingleOnly && fitting >= 2:
		item.Strategy = string(types.PlacementReplicated)
		item.Reason = fmt.Sprintf(
			"модель влезает в %d узла(ов) по отдельности (нужно %s, максимум свободно %s) — "+
				"копии дают кратную пропускную способность",
			fitting, humanBytesR75(int64(needBytes)), humanBytesR75(int64(maxFree)*1024*1024))
		item.Notes = append(item.Notes, noteR96(lang, "single_would_do"))

	case fitsOneNode:
		item.Strategy = string(types.PlacementSingle)
		item.Reason = fmt.Sprintf("модель влезает в один узел: нужно %s, свободно %s",
			humanBytesR75(int64(needBytes)), humanBytesR75(int64(maxFree)*1024*1024))
		item.Notes = append(item.Notes, noteR96(lang, "single_enough"))

	case len(candidates) >= 2 && fitsSum:
		group := p.planGroupR96(item, model, candidates, lang, req)
		item.Strategy = string(types.PlacementSharded)
		item.GroupID = group.ID
		item.Group = group
		item.Reason = fmt.Sprintf(
			"ни один узел не вмещает модель (нужно %s, максимум свободно %s), "+
				"а сумма по %d узлам (%s) — вмещает: раскладка слоями",
			humanBytesR75(int64(needBytes)), humanBytesR75(int64(maxFree)*1024*1024),
			len(group.Members), humanBytesR75(int64(sumFree)*1024*1024))
		item.Notes = append(item.Notes, noteR96(lang, "rpc_endpoint_guessed"))

	default:
		item.Strategy = string(types.PlacementSingle)
		item.Degraded = true
		item.Reason = fmt.Sprintf(
			"модель не влезает ни в один узел и не влезает в сумму: нужно %s, "+
				"свободно максимум %s, суммарно %s",
			humanBytesR75(int64(needBytes)), humanBytesR75(int64(maxFree)*1024*1024),
			humanBytesR75(int64(sumFree)*1024*1024))
		item.Notes = append(item.Notes, noteR96(lang, "no_fit"))
	}

	// Трафик считаем только там, где есть раскладка: для single/replicated
	// межсетевого обмена между узлами нет (реплики независимы).
	if item.Strategy == string(types.PlacementSharded) {
		item.Traffic = planTrafficR96(p, planned, req)
		item.Issues = p.planIssuesR96(item, planned, req)
	}

	item.Rule = types.PlacementModelRule{
		Model:    model,
		Strategy: string(types.PlacementAuto),
		Reason:   item.Reason,
		Auto: &types.PlacementAutoRule{
			// Политика пишется как auto, а не как «застывшая» стратегия: состав
			// кластера и свободный VRAM меняются, и решение должно следовать за
			// ними. Предложенная стратегия — то, что auto выберет СЕЙЧАС, и она
			// же видна оператору в плане.
			Prefer:             []string{item.Strategy, string(types.PlacementSingle)},
			AllowDegraded:      req.AllowDegraded,
			RequireHomogeneous: req.RequireHomogeneous,
		},
	}
	if item.Strategy == string(types.PlacementReplicated) {
		item.Rule.Auto.MinBackends = 2
	}
	return item
}

// planCandidateR96 — узел-кандидат для раскладки: healthy llama.cpp с метриками
// VRAM. Воркеры уже собранных групп исключены (их GPU занят существующей
// раскладкой), image-узлы исключены (другой тип бэкенда).
type planCandidateR96 struct {
	backendID  string
	host       string
	vramFreeMB int
	gpuID      string
}

func (p *Proxy) planCandidatesR96() []planCandidateR96 {
	p.mu.RLock()
	ids := make([]string, 0, len(p.backends))
	for id, state := range p.backends {
		if state == nil || state.Backend == nil {
			continue
		}
		if state.Backend.Status != types.StatusHealthy {
			continue
		}
		if normalizeBackendType(state.Backend.Type) != types.BackendTypeLlamaCpp {
			continue
		}
		ids = append(ids, id)
	}
	p.mu.RUnlock()
	sort.Strings(ids)

	out := make([]planCandidateR96, 0, len(ids))
	for _, id := range ids {
		// R304: сюда же донор — его VRAM отдана мастеру, свои копии ему нельзя.
		if p.cannotHostModelR304(id) {
			continue // GPU уже отдан существующей раскладке
		}
		info := p.groupMemberInfoR92(id)
		if !info.VRAMKnown {
			continue // без метрик VRAM раскладку не посчитать — не гадаем
		}
		host := ""
		p.mu.RLock()
		if state := p.backends[id]; state != nil && state.Backend != nil {
			host = state.Backend.Host
		}
		p.mu.RUnlock()
		out = append(out, planCandidateR96{
			backendID:  id,
			host:       host,
			vramFreeMB: info.VRAMFreeMB,
			gpuID:      p.backendGPUIdentity(id),
		})
	}
	// Мастером предлагаем узел с наибольшим свободным VRAM: на нём осядет
	// большая часть слоёв и KV, и именно он принимает HTTP.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].vramFreeMB != out[j].vramFreeMB {
			return out[i].vramFreeMB > out[j].vramFreeMB
		}
		return out[i].backendID < out[j].backendID
	})
	return out
}

// planGroupR96 — предложенная группа под модель.
func (p *Proxy) planGroupR96(item *SetupPlanItemR96, model string, candidates []planCandidateR96, lang string, req SetupPlanRequestR96) *types.BackendGroupConfig {
	group := &types.BackendGroupConfig{
		ID:       "group-" + sanitizeGroupIDR96(model),
		Model:    model,
		Strategy: string(types.BackendGroupStrategyLayer),
		Reason:   "предложение мастера: модель не влезает в один узел",
		Members:  make([]types.BackendGroupMemberConfig, 0, len(candidates)),
	}
	for i, cand := range candidates {
		if i == 0 {
			group.Members = append(group.Members, types.BackendGroupMemberConfig{
				BackendID: cand.backendID,
				Role:      types.BackendGroupRoleMaster,
			})
			continue
		}
		endpoint := fmt.Sprintf("%s:%d", cand.host, defaultRPCPortR96)
		if strings.TrimSpace(cand.host) == "" {
			endpoint = ""
		}
		group.Members = append(group.Members, types.BackendGroupMemberConfig{
			BackendID:   cand.backendID,
			Role:        types.BackendGroupRoleWorker,
			RPCEndpoint: endpoint,
		})
	}
	if req.RequireHomogeneous {
		// Однородность влияет на трафик и на предсказуемость: отмечаем в note,
		// а сам факт проверяет валидатор композиции (checkHomogeneous).
		item.Notes = append(item.Notes, noteR96(lang, "homogeneous_check"))
	}
	return group
}

// planTrafficR96 — оценка трафика для layer и row при текущих данных модели.
func planTrafficR96(p *Proxy, planned SetupPlanModelR96, req SetupPlanRequestR96) *SetupPlanTrafficR96 {
	if planned.HiddenSize <= 0 || planned.Layers <= 0 {
		return nil
	}
	input := composition.TrafficInput{
		HiddenSize:           planned.HiddenSize,
		Layers:               planned.Layers,
		BytesPerElem:         2,
		PrefillTokens:        req.PrefillTokens,
		BandwidthBytesPerSec: p.bandwidthBytesPerSecR166(req.BandwidthMBps),
		TokensPerSecond:      req.TokensPerSecond,
	}
	layer := input
	layer.Strategy = composition.StrategyLayer
	row := input
	row.Strategy = composition.StrategyRow
	return &SetupPlanTrafficR96{
		Layer: composition.EstimateTraffic(layer),
		Row:   composition.EstimateTraffic(row),
	}
}

// planIssuesR96 — предполётные замечания по предложенной раскладке (§7).
func (p *Proxy) planIssuesR96(item *SetupPlanItemR96, planned SetupPlanModelR96, req SetupPlanRequestR96) []composition.Issue {
	if item.Group == nil {
		return []composition.Issue{}
	}
	report := p.ValidateCompositionR95(CompositionRequestR95{
		Group: *item.Group,
		Model: CompositionModelR95{
			Name:        planned.Name,
			BackendType: "llama_cpp",
			SizeBytes:   planned.SizeBytes,
			HiddenSize:  planned.HiddenSize,
			Layers:      planned.Layers,
			NCtx:        planned.NCtx,
			KVLayers:    planned.KVLayers,
			KVHeads:     planned.KVHeads,
			KVHeadDim:   planned.KVHeadDim,
		},
		Lang:               req.Lang,
		BandwidthMBps:      req.BandwidthMBps,
		PrefillTokens:      req.PrefillTokens,
		TokensPerSecond:    req.TokensPerSecond,
		RequireHomogeneous: req.RequireHomogeneous,
	})
	if report.Issues == nil {
		return []composition.Issue{}
	}
	return report.Issues
}

// planModelsR96 — что планировать: явный список или обнаружение по стенду.
func (p *Proxy) planModelsR96(requested []SetupPlanModelR96) []SetupPlanModelR96 {
	if len(requested) > 0 {
		out := make([]SetupPlanModelR96, 0, len(requested))
		seen := map[string]bool{}
		for _, m := range requested {
			name := strings.TrimSpace(m.Name)
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, m)
		}
		return out
	}
	return p.discoverModelsR96()
}

// discoverModelsR96 — модели, о которых знает балансер: профили + каталог файлов
// на узлах (R91). Стенд «пустой» ровно до первого запроса — обнаружение даёт
// мастеру что показать на шаге «модели и их вес».
func (p *Proxy) discoverModelsR96() []SetupPlanModelR96 {
	names := map[string]bool{}
	for name := range p.config.LlamaCppModelProfiles {
		if strings.TrimSpace(name) != "" {
			names[name] = true
		}
	}
	for _, snapshot := range p.ModelCatalogSnapshot() {
		for _, file := range snapshot.Files {
			name := strings.TrimSpace(strings.TrimSuffix(file, ".gguf"))
			if name != "" {
				names[name] = true
			}
		}
	}
	out := make([]SetupPlanModelR96, 0, len(names))
	for name := range names {
		out = append(out, SetupPlanModelR96{Name: name})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// sanitizeGroupIDR96 — id группы из имени модели (буквы/цифры/дефис).
func sanitizeGroupIDR96(model string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(model) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.' || r == '/':
			b.WriteRune('-')
		}
	}
	id := strings.Trim(b.String(), "-")
	if id == "" {
		id = "model"
	}
	if len(id) > 48 {
		id = id[:48]
	}
	return id
}

// noteR96 — короткие пояснения к предложению (ru/en).
func noteR96(lang, key string) string {
	ru := map[string]string{
		"size_unknown":         "размер модели неизвестен: подсказать раскладку нельзя, пока нет профиля или метрик",
		"single_would_do":      "single тоже подошёл бы: копии нужны ради пропускной способности, а не ради вместимости",
		"single_enough":        "модель влезает в один узел — раскладка не нужна и только добавит хрупкости",
		"rpc_endpoint_guessed": fmt.Sprintf("порт RPC воркеров предложен по умолчанию (%d) — проверьте его в валидаторе", defaultRPCPortR96),
		"no_fit":               "модель не влезает в кластер: нужен узел с большей VRAM или модель меньшего размера",
		"homogeneous_check":    "требование одинаковых GPU: проверено валидатором композиции",
	}
	en := map[string]string{
		"size_unknown":         "model size is unknown: no plan can be suggested until a profile or metrics exist",
		"single_would_do":      "single would also work: replicas are for throughput, not for capacity",
		"single_enough":        "the model fits one node — sharding is unnecessary and only adds fragility",
		"rpc_endpoint_guessed": fmt.Sprintf("worker RPC port is a default guess (%d) — verify it in the validator", defaultRPCPortR96),
		"no_fit":               "the model does not fit the cluster: a node with more VRAM or a smaller model is required",
		"homogeneous_check":    "identical GPUs required: checked by the composition validator",
	}
	if lang == composition.LangEN {
		return en[key]
	}
	return ru[key]
}
