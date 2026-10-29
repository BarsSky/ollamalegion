package balancer

// placement_over_reload_r293.go — R293: «разместить на другом узле» вместо
// «перезагрузить этот».
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА. На втором стенде два узла: на одном крутится агент
// (модель загружена, стрим идёт), второй полностью свободен и файл модели у него
// есть. Второй агент приходит с ДРУГИМ окном (или другими параметрами загрузки) —
// и балансер вместо загрузки на свободный узел ПЕРЕЗАГРУЖАЕТ модель на первом,
// вырывая её из-под живого стрима: первый агент получает
// «stream ended by upstream before completion».
//
// ПОЧЕМУ ТАК БЫЛО. Решение принималось в два независимых шага, и ни один не
// смотрел на второй узел:
//  1. выбор бэкенда (model_copy_window_r118.go:234 →
//     selectModelCopyForWindow) рассматривает только УЖЕ ЗАГРУЖЕННЫЕ копии
//     (collectModelCopyCandidates требует backendHasModel). Пустой, но готовый
//     узел кандидатом не становится никогда;
//  2. preflight (preflight_helper.go:53 → RunPreflight) получает уже
//     выбранный backendID и сравнивает требуемое окно с загруженным — то есть
//     может только «перезагрузить здесь» или отказать.
// Механизм «загрузить на узле с файлом» существует
// (findLeastLoadedBackendWithModelOnDisk, model_catalog.go:534), но применяется
// ТОЛЬКО когда модель не загружена нигде.
//
// ПРАВИЛО ЗДЕСЬ. Если селектор выбрал узел, где загруженная копия НЕ покрывает
// требуемое окно, и уходить с этого узла есть причина (на нём идут чужие стримы
// либо его физический потолок меньше требуемого), сначала ищем ДРУГОЙ здоровый
// узел, который:
//   - не воркер раскладки, не disabled, healthy, llama.cpp;
//   - имеет файл модели на диске (каталог опрошен и модель есть);
//   - не занят стримами;
//   - физически вмещает требуемое окно (GGUF/feasible, если узел их сообщает);
//   - имеет свободной VRAM не меньше оценки модели.
// Нашли — возвращаем его (обработчик загрузит модель туда с окном клиента).
// Не нашли — прежнее поведение (перезагрузка выбранного узла под guard'ом R271).
//
// ЧЕГО ЭТО НЕ ДЕЛАЕТ: не трогает узлы с активными стримами, не выбирает узел без
// файла модели (иначе 503 model_not_found), не «переезжает» при отсутствии
// причины (свободный узел дешевле перезагрузить, чем грузить копию на другой:
// модель уже в памяти). Флаг оператора — LB_PLACE_OVER_RELOAD (по умолчанию ON),
// горячий: меняется из WebUI/API без рестарта.

import (
	"sort"

	"ollama-loadbalancer/internal/settings"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// placeOverReloadEnabledR293 — флаг «размещать на другом узле вместо перезагрузки».
func placeOverReloadEnabledR293() bool {
	if v, ok := settings.HotInt("LB_PLACE_OVER_RELOAD"); ok {
		return v != 0
	}
	return true
}

// placementCandidateR293 — узел-кандидат для размещения модели с нужным окном.
type placementCandidateR293 struct {
	ID         string
	FreeVRAMMB uint64
	LoadRatio  float64
	Score      float64
}

// findPlacementOverReloadR293 — узел для размещения вместо перезагрузки.
//
// currentBackendID — узел, который выбрал селектор (там загружена копия модели,
// не покрывающая requiredPerSeq). Возвращает ("", "") если переезд не нужен или
// невозможен — тогда работает прежний путь с перезагрузкой.
func (p *Proxy) findPlacementOverReloadR293(model string, requiredPerSeq int, currentBackendID string) (string, string) {
	if p == nil || model == "" || requiredPerSeq <= 0 {
		return "", ""
	}
	if !placeOverReloadEnabledR293() {
		return "", ""
	}

	// Есть ли причина уходить с текущего узла.
	reason := ""
	if n := p.activeStreamsOnBackend(currentBackendID); n > 0 {
		reason = "current backend has active streams"
	} else if ceiling := p.backendWindowCeilingR293(currentBackendID, model); ceiling > 0 && requiredPerSeq > ceiling {
		reason = "current backend cannot fit the required window"
	}
	if reason == "" {
		// Узел свободен и физически вмещает окно: перезагрузка на нём дешевле
		// (модель уже в памяти/на диске) — прежнее поведение.
		return "", ""
	}

	cands := p.collectPlacementCandidatesR293(model, requiredPerSeq, currentBackendID)
	if len(cands) == 0 {
		return "", ""
	}
	best := cands[0]
	return best.ID, reason
}

// collectPlacementCandidatesR293 — узлы, готовые принять модель с нужным окном.
//
// Детерминированный порядок: больше свободной VRAM, затем меньше загрузка, затем
// выше score, затем меньший ID. Порядок фиксирован, иначе два одинаковых запроса
// подряд могли бы уехать на разные узлы (пинг-понг) — ровно тот класс дефекта,
// который уже ловили в R91.
func (p *Proxy) collectPlacementCandidatesR293(model string, requiredPerSeq int, currentBackendID string) []placementCandidateR293 {
	p.mu.RLock()
	defer p.mu.RUnlock()

	needVRAM := estimateModelVRAM(model)
	out := make([]placementCandidateR293, 0, len(p.backends))
	for id, state := range p.backends {
		if state == nil || state.Backend == nil {
			continue
		}
		if id == currentBackendID {
			continue
		}
		// Воркер раскладки не самостоятелен (R92) — грузить туда копию нельзя.
		// R304: сюда же донор — его VRAM отдана мастеру, свои копии ему нельзя.
		if p.cannotHostModelR304(id) {
			continue
		}
		if normalizeBackendType(state.Backend.Type) != types.BackendTypeLlamaCpp {
			continue
		}
		if state.Backend.Status != types.StatusHealthy {
			continue
		}
		// R250: узел, исключённый оператором, целью не становится.
		if state.Backend.Disabled {
			continue
		}
		if !p.checkResourceLimitsLocked(id) {
			continue
		}
		// Чужие стримы на кандидате — стоп: загрузка его же и оборвёт.
		if p.activeStreamsOnBackend(id) > 0 {
			continue
		}
		// Файл модели на диске обязателен: иначе загрузка вернёт model_not_found.
		has, known := p.BackendModelOnDisk(id, model)
		if !known || !has {
			continue
		}
		// Физический потолок узла (если сообщает) должен вмещать окно.
		if ceiling := p.backendWindowCeilingR293(id, model); ceiling > 0 && requiredPerSeq > ceiling {
			continue
		}
		// Свободная VRAM: берём тот же источник, что и остальные проверки
		// размещения (метрики поллера), не дёргая сеть в горячем пути.
		freeVRAM, ok := p.freeVRAMMBForPlacementR293(id)
		if !ok {
			continue
		}
		if needVRAM > 0 && freeVRAM < needVRAM {
			continue
		}
		state.mu.Lock()
		active := state.ActiveReqs
		maxReqs := state.Backend.EffectiveMaxConcurrentRequests()
		state.mu.Unlock()
		if maxReqs <= 0 {
			continue
		}
		out = append(out, placementCandidateR293{
			ID:         id,
			FreeVRAMMB: freeVRAM,
			LoadRatio:  float64(active) / float64(maxReqs),
			Score:      p.calculateScore(id),
		})
	}

	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.FreeVRAMMB != b.FreeVRAMMB {
			return a.FreeVRAMMB > b.FreeVRAMMB
		}
		if a.LoadRatio != b.LoadRatio {
			return a.LoadRatio < b.LoadRatio
		}
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		return a.ID < b.ID
	})
	return out
}

// backendWindowCeilingR293 — физический потолок окна узла для модели (0 = неизвестно).
//
// Источник — снапшот поллера: GGUF-потолок модели и/или feasible, посчитанный
// воркером. Неизвестно → 0, и тогда решение принимается по VRAM-оценке: гадать
// «не влезет» без данных значило бы отказывать в размещении там, где всё влезает.
func (p *Proxy) backendWindowCeilingR293(backendID, model string) int {
	if p == nil || p.metricsMgr == nil || backendID == "" {
		return 0
	}
	lm, ok := p.metricsMgr.SnapshotLlamaCppMetrics(backendID)
	if !ok || lm == nil {
		return 0
	}
	ceiling := lm.GGUFMaxContext
	if lm.MaxFeasibleContext > 0 && (ceiling <= 0 || lm.MaxFeasibleContext < ceiling) {
		ceiling = lm.MaxFeasibleContext
	}
	_ = model
	return ceiling
}

// freeVRAMMBForPlacementR293 — свободная VRAM узла в МБ по метрикам поллера.
func (p *Proxy) freeVRAMMBForPlacementR293(backendID string) (uint64, bool) {
	if p == nil || p.metricsMgr == nil {
		return 0, false
	}
	if metrics, ok := p.metricsMgr.SnapshotBackendMetrics(backendID); ok && metrics != nil {
		if metrics.GPU.MemoryTotal > 0 {
			if metrics.GPU.MemoryFree > 0 {
				return metrics.GPU.MemoryFree, true
			}
			if metrics.GPU.MemoryUsed < metrics.GPU.MemoryTotal {
				return metrics.GPU.MemoryTotal - metrics.GPU.MemoryUsed, true
			}
		}
	}
	if lm, ok := p.metricsMgr.SnapshotLlamaCppMetrics(backendID); ok && lm != nil && lm.VramKnown && lm.AvailableVRAMMB > 0 {
		return uint64(lm.AvailableVRAMMB), true
	}
	return 0, false
}

// logPlacementOverReloadR293 — одна строка: что и почему уехало на другой узел.
func logPlacementOverReloadR293(model, from, to, reason string, requiredPerSeq int) {
	logger.Get().Infow("model placed on alternative backend (R293)",
		"model", model, "from", from, "to", to,
		"reason", reason, "required_per_seq", requiredPerSeq)
}
