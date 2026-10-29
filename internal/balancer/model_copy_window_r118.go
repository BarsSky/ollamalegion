package balancer

import (
	"sort"
	"strings"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// model_copy_window_r118.go — R118.5 (2026-10-14): выбор КОПИИ модели с учётом
// окна клиента.
//
// ЖИВОЙ ДЕФЕКТ (нашла сценарная проверка на стенде). Одна и та же модель была
// загружена на двух бэкендах: на мастере с окном 32768 (16K на слот) и на доноре
// с окном 8192 (4K на слот). Клиент прислал num_ctx=8192. Балансер выбирал копию
// по «наименее загруженному» — донор простаивал, и запрос ушёл на НЕГО:
//
//	num_ctx decision backend=cppworker-cyborg-bundled-agent
//	окно модели суммарное/на клиента: total_n_ctx=8192 context_per_seq=4096
//	detected n_ctx mismatch loaded_n_ctx=4096 requested_n_ctx=8192
//	  → reload target 16384 → cppworker: 422 n_ctx_infeasible
//	клиент: +33 с и окно МЕНЬШЕ заготовленного оператором
//
// То есть подготовленная копия (32768 на мастере) игнорировалась в пользу копии,
// которая окно клиента не покрывает. Здесь — правило выбора:
//
//  1. сначала копии, чьё окно НА СЛОТ ≥ запрошенного клиентом (покрывают запрос);
//  2. внутри группы — главный бэкенд (мастер): его VRAM не занята отдачей слоёв
//     донору, и именно его готовил оператор;
//  3. затем — наименьшая загрузка, затем лучший score, затем меньший ID
//     (детерминизм: обход карты в Go случаен).
//
// Если НИ ОДНА копия окно не покрывает — берём копию с САМЫМ БОЛЬШИМ окном (а не
// самую свободную): рост окна — задача preflight-reload'а, и растить его дешевле
// от 16K, чем от 4K.

// modelCopyCandidate — кандидат-копия модели с известным окном на слот.
type modelCopyCandidate struct {
	ID        string
	PerSeq    int
	Total     int
	Role      string
	LoadRatio float64
	Score     float64
}

// loadedPerSeqWindowFromCache — окно НА СЛОТ для модели по кэшу метрик.
// Возвращает 0, если модель в кэше не найдена.
//
// Логика та же, что в loadedPerSeqNCtx (num_ctx_resolver.go): сначала
// context_per_seq от cppworker, иначе делим суммарное окно на слоты. Отдельная
// функция нужна потому, что здесь мы читаем кэш ОДНИМ захватом metricsMgr.mu
// (внутри цикла по бэкендам) — вложенные RLock на том же мьютексе запрещены.
func (p *Proxy) loadedPerSeqWindowFromCache(backendID, modelName string) (perSeq, total int) {
	if p == nil || p.metricsMgr == nil || backendID == "" {
		return 0, 0
	}
	p.metricsMgr.mu.RLock()
	defer p.metricsMgr.mu.RUnlock()
	lm := p.metricsMgr.llamaMetrics[backendID]
	if lm == nil {
		return 0, 0
	}
	for _, m := range lm.LoadedModels {
		if modelName != "" && m.Name != modelName &&
			!containsFold(m.Name, modelName) && !containsFold(modelName, m.Name) {
			continue
		}
		if m.ContextLength <= 0 {
			continue
		}
		total = m.ContextLength
		if m.ContextPerSeq > 0 {
			return m.ContextPerSeq, total
		}
		if m.MaxSlots > 1 {
			return cppbackend.ContextPerSlot(m.ContextLength, m.MaxSlots), total
		}
		if m.Parallel > 1 {
			return cppbackend.ContextPerSlot(m.ContextLength, m.Parallel), total
		}
		return m.ContextLength, total
	}
	return 0, 0
}

// collectModelCopyCandidates — какие бэкенды ГОТОВЫ принять запрос к этой модели
// (модель загружена, бэкенд healthy, лимиты и загрузка позволяют).
//
// Фильтры намеренно те же, что у findLeastLoadedBackendWithModel: иначе
// «предпочтение по окну» отдавало бы запрос узлу, который текущий код считает
// непригодным (перегружен, воркер группы раскладки и т.п.).
func (p *Proxy) collectModelCopyCandidates(modelName string, exclude map[string]bool, allowedTypes []types.BackendType) []modelCopyCandidate {
	if p == nil || modelName == "" {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()

	out := make([]modelCopyCandidate, 0, len(p.backends))
	for id, state := range p.backends {
		if p.isGroupWorkerR92(id) {
			continue
		}
		if exclude != nil && exclude[id] {
			continue
		}
		if !isBackendTypeAllowed(normalizeBackendType(state.Backend.Type), allowedTypes) {
			continue
		}
		if state.Backend.Status != types.StatusHealthy {
			continue
		}
		if !p.checkResourceLimitsLocked(id) {
			continue
		}
		state.mu.Lock()
		active := state.ActiveReqs
		maxReqs := state.Backend.MaxConcurrentReqs
		role := state.Backend.Role
		state.mu.Unlock()
		if maxReqs <= 0 {
			continue
		}
		metrics, hasMetrics := p.metricsMgr.SnapshotBackendMetrics(id)
		if !hasMetrics || !p.backendHasModel(metrics, modelName) {
			continue
		}
		perSeq, total := p.loadedPerSeqWindowFromCache(id, modelName)
		out = append(out, modelCopyCandidate{
			ID:        id,
			PerSeq:    perSeq,
			Total:     total,
			Role:      strings.TrimSpace(role),
			LoadRatio: float64(active) / float64(maxReqs),
			Score:     p.calculateScore(id),
		})
	}
	return out
}

// selectModelCopyForWindow — выбрать копию модели под окно клиента.
//
// Возвращает (backendID, причина для лога). Пустая строка — решать прежним
// способом (например, когда ни одна копия ещё не загружена).
func (p *Proxy) selectModelCopyForWindow(modelName string, requestedNCtx int, exclude map[string]bool, allowedTypes []types.BackendType) (string, string) {
	if p == nil || modelName == "" || requestedNCtx <= 0 {
		return "", ""
	}
	cands := p.collectModelCopyCandidates(modelName, exclude, allowedTypes)
	if len(cands) == 0 {
		return "", ""
	}

	covering := make([]modelCopyCandidate, 0, len(cands))
	for _, c := range cands {
		if c.PerSeq > 0 && c.PerSeq >= requestedNCtx {
			covering = append(covering, c)
		}
	}
	pool := covering
	reason := "копия покрывает окно клиента"
	if len(pool) == 0 {
		// Никто не покрывает: берём САМОЕ БОЛЬШОЕ окно (растить его дешевле),
		// а не самую свободную копию.
		pool = cands
		reason = "ни одна копия не покрывает окно клиента — берём копию с самым большим окном"
	}

	sort.SliceStable(pool, func(i, j int) bool {
		a, b := pool[i], pool[j]
		if len(covering) == 0 && a.PerSeq != b.PerSeq {
			// Никто не покрывает — решает размер окна.
			return a.PerSeq > b.PerSeq
		}
		aMain := a.Role == types.BackendRoleMain
		bMain := b.Role == types.BackendRoleMain
		if aMain != bMain {
			return aMain
		}
		if a.LoadRatio != b.LoadRatio {
			return a.LoadRatio < b.LoadRatio
		}
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		return a.ID < b.ID
	})

	best := pool[0]
	if best.Role == types.BackendRoleMain {
		reason += " (главный бэкенд)"
	}
	return best.ID, reason
}

// modelCopyCoversWindow — покрывает ли ЗАГРУЖЕННАЯ на бэкенде копия модели
// запрошенное клиентом окно (сравнение по окну НА СЛОТ).
func (p *Proxy) modelCopyCoversWindow(backendID, modelName string, requestedNCtx int) bool {
	if requestedNCtx <= 0 {
		return true
	}
	perSeq, _ := p.loadedPerSeqWindowFromCache(backendID, modelName)
	return perSeq >= requestedNCtx
}

// selectBackendPreferWindow — выбор бэкенда с учётом окна клиента: если среди
// УЖЕ загруженных копий есть покрывающая запрос — берём её, иначе прежний путь.
//
// Нужен там, где бэкенд выбирается ДО llama.cpp-транспорта (Ollama-поверхность:
// ServeHTTP → selectBackend). OpenAI-поверхность выбирает бэкенд в
// llamacpp_handlers_inference.go (selectLlamaCppBackendForModelWithNCtx).
func (p *Proxy) selectBackendPreferWindow(model string, bt types.BackendType, requestedNCtx int) string {
	if p != nil && requestedNCtx > 0 && model != "" {
		if id, why := p.selectModelCopyForWindow(model, requestedNCtx, nil, p.getAllowedTypesList(bt)); id != "" {
			logger.Get().Infow(i18n.T("balancer.model_copy_window.select_backend_info"),
				"model", model, "backend", id, "requested_n_ctx", requestedNCtx, "reason", why)
			return id
		}
	}
	return p.selectBackend(model, bt, false)
}

// selectLlamaCppBackendForModelWithNCtx — то же, что selectLlamaCppBackendForModel,
// но с окном клиента: при нескольких ГОТОВЫХ копиях предпочитается та, что
// покрывает запрошенное окно (см. model_copy_window_r118.go).
//
// Группы репликации не трогаем: у них собственный учёт инстансов и счётчик
// выбора (Select меняет состояние), поэтому для моделей группы работает прежний
// путь целиком.
func (lr *LlamaCppRouter) selectLlamaCppBackendForModelWithNCtx(model string, requestedNCtx int) string {
	if lr == nil || lr.proxy == nil || model == "" || requestedNCtx <= 0 {
		if lr == nil {
			return ""
		}
		return lr.selectLlamaCppBackendForModel(model)
	}
	if lr.proxy.replicationSelector != nil && lr.proxy.replicationSelector.IsGroupModel(model) {
		return lr.selectLlamaCppBackendForModel(model)
	}
	allowed := lr.proxy.getAllowedTypesList(types.BackendTypeLlamaCpp)
	if id, why := lr.proxy.selectModelCopyForWindow(model, requestedNCtx, nil, allowed); id != "" {
		logger.Get().Infow(i18n.T("balancer.model_copy_window.llamacpp_info"),
			"model", model, "backend", id, "requested_n_ctx", requestedNCtx, "reason", why)
		return id
	}
	return lr.selectLlamaCppBackendForModel(model)
}
