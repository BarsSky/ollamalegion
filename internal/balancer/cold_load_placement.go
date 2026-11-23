// cold_load_placement_r659.go — R659 (2026-11-20): ЗАГРУЗКА ВМЕСТО
// ПАССИВНОГО ОЖИДАНИЯ.
//
// ЖИВОЙ ДЕФЕКТ (второй стенд, 2026-11-20, архив балансера r597 + воркер r648).
// Qwen3.8-27B загружена на мастере (A10, окно 201216), мастер отдаёт одну
// генерацию. Второй запрос /api/chat встал в unified-очередь с маркером `any` на
// 300 с и был отменён клиентом. Рядом простаивали ОБА свободных узла (A10-126,
// 20.5 ГБ свободной VRAM; RTX 4000 Quadro), и в их логах — НОЛЬ попыток
// загрузки: балансер даже не начинал поднимать там модель.
//
// ПОЧЕМУ ТАК. Заявка была привязана к мастеру, и очередь R480/P1-7 не выдаёт
// слот на «холодной» копии: гейт R459 (session_move_window.go) отклоняет такого
// кандидата строкой «модель не загружена (переезд = выгрузка+загрузка)». Это
// правило верное — оно про ОКНО, — но оно же запрещало единственное действие,
// которое создаёт ёмкость: загрузку модели на свободном узле. Выбор под загрузку
// (этап 3b, pickLoadableFreeBackendR493) при этом срабатывал каждые 250 мс —
// 304 раза за прогон — и каждый раз отклонялся тем же гейтом.
//
// ЧТО ЗДЕСЬ. Заявка, которая НЕ получила слот ни на одной прогретой копии
// (retry-цикл по loaded-копиям исчерпан, слот не взят), запускает загрузку на
// подходящем свободном узле. Заявка при этом остаётся в очереди и обслуживается
// тем, что станет готово РАНЬШЕ: своя копия (её слот освободился) или новая
// (загрузка закончилась). Так заявка не платит за загрузку, если её собственный
// слот освободится через секунду, и не ждёт 300 с впустую, если стрим длинный.
//
// ПОЧЕМУ НЕ «ПЕРЕЕХАТЬ И ЖДАТЬ ЗАГРУЗКУ ТАМ». R139 живой: уход сессии на узел
// без модели — это минуты тишины в стриме клиента (pi-ai idle timeout 300 с,
// Cline 300 с), и именно на этом ломался агентский ход. Здесь переезда нет:
// заявка ждёт там же, где ждала, а узел тем временем поднимает модель.
//
// ОКНО. Загрузка идёт на «нужном окне» заявки (need — session_move_need.go,
// вариант C) и приходит к воркеру как contextPerSeq, то есть ровно как окно
// ОДНОГО клиента (R83: воркер сам считает суммарное = PAD256(per) × слоты).
// Никакого сжатия под живым клиентом (R294/R459) здесь нет и быть не может:
// новый слот не уже нужного окна по построению.
//
// ЧЕСТНОСТЬ ПРОВЕРКИ. «Влезет ли окно» — та же арифметика, что у AutoTune
// (R230/R498): веса + KV(окно, тип KV) + резерв на буферы против свободной VRAM
// узла, где KV считается единственной формулой стека (memfit, kvCacheBytesForContext).
// Если данных не хватает (нет ни одной загруженной копии для геометрии, нет
// метрик VRAM), загрузка всё равно запускается — как и этап 3b, который «VRAM
// неизвестна» не запрещает (стенд без метрик иначе не поднимал бы ничего), — но
// решение помечается `verified=false`, и это видно в логе.
package balancer

import (
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

const (
	// coldLoadDedupTTLR659 — сколько помним, что загрузку на узле уже
	// запускали. Короче загрузки (140–485 с) намеренно: провалившуюся попытку
	// надо уметь повторить, а не залипнуть на ней до конца жизни процесса.
	coldLoadDedupTTLR659 = 120 * time.Second
	// coldLoadMinReserveMBR659 — минимум под compute-буферы, если total VRAM
	// узла неизвестен (та же роль, что autotuneMinReserveBytes).
	coldLoadMinReserveMBR659 = 256
	// coldLoadRetryEveryR659 — как часто заявка, стоящая в очереди, повторяет
	// попытку «начать загрузку». Слот на свободном узле мог освободиться.
	coldLoadRetryEveryR659 = 5 * time.Second
)

// coldLoadFitR659 — вердикт «модель поднимется на этом узле с окном window».
//
// Verified=false — проверить нечем (нет геометрии модели или метрик VRAM);
// решение принимается по данным R493 (размер весов), а не по выдумке.
type coldLoadFitR659 struct {
	KVType    string
	Reason    string
	NeedMB    int
	FreeMB    int
	WeightsMB int
	KVMB      int
	ReserveMB int
	Fits      bool
	Verified  bool
}

// coldLoadPlanR659 — что и где поднимать.
type coldLoadPlanR659 struct {
	BackendID string
	Fit       coldLoadFitR659
	Window    int
}

// coldLoadStateR659 — память о запущенных загрузках (дедупликация).
//
// ЗАЧЕМ СВОЯ ПАМЯТЬ, А НЕ WarmingUpModels: маркер прогрева живёт своей жизнью
// (prewarm ставит его на 30 с и снимает таймером), а здесь нужен ровно один
// факт — «эту загрузку уже просили N секунд назад», чтобы заявки, ждущие в
// очереди, не отправляли POST /load каждая.
type coldLoadStateR659 struct {
	started map[string]time.Time
	// probes — кэш живых ответов узлов (R663): один GET /api/models на узел и
	// модель раз в coldLoadProbeCacheTTL. Иначе каждая итерация ожидания (раз в
	// 5 с) дёргала бы воркер лишним запросом в его же лог.
	probes map[string]coldLoadProbeEntryR659
	mu     sync.Mutex
}

// coldLoadProbeEntryR659 — запись кэша живых проверок.
type coldLoadProbeEntryR659 struct {
	At    time.Time
	Probe coldLoadProbeR659
}

// markStartedR659 — true, если загрузку на этом узле можно просить (не просили
// последние coldLoadDedupTTLR659). Мутация и проверка — под одним замком,
// иначе две одновременные заявки обе решат, что они первые.
func (s *coldLoadStateR659) markStartedR659(backendID, model string, now time.Time) bool {
	key := backendID + "\x00" + model
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started == nil {
		s.started = make(map[string]time.Time, 4)
	}
	if at, ok := s.started[key]; ok && now.Sub(at) < coldLoadDedupTTLR659 {
		return false
	}
	// Чистим протухшие записи здесь же: карта маленькая (узлы × модели), но
	// расти без предела не должна.
	for k, at := range s.started {
		if now.Sub(at) >= coldLoadDedupTTLR659 {
			delete(s.started, k)
		}
	}
	s.started[key] = now
	return true
}

// startedAtR659 — когда загрузку просили (для диагностики «почему в очереди»:
// «загрузка начата N с назад» вместо «модель не загружена»).
func (s *coldLoadStateR659) startedAtR659(backendID, model string) (time.Time, bool) {
	key := backendID + "\x00" + model
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.started[key]
	if !ok || time.Since(at) >= coldLoadDedupTTLR659 {
		return time.Time{}, false
	}
	return at, true
}

// clearStartedR659 — забыть отметку о попытке (R663: живая проверка показала, что
// загрузка не подхватилась — отказ узла или провал; ждать полный TTL бессмысленно).
func (s *coldLoadStateR659) clearStartedR659(backendID, model string) {
	key := backendID + "\x00" + model
	s.mu.Lock()
	delete(s.started, key)
	s.mu.Unlock()
}

// cachedProbeR659 — живой ответ узла из кэша (не старше coldLoadProbeCacheTTL).
func (s *coldLoadStateR659) cachedProbeR659(backendID, model string, now time.Time) (coldLoadProbeR659, bool) {
	key := backendID + "\x00" + model
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.probes[key]
	if !ok || now.Sub(entry.At) >= coldLoadProbeCacheTTL {
		return coldLoadProbeR659{}, false
	}
	return entry.Probe, true
}

// putProbeR659 — запомнить живой ответ; протухшие записи чистятся здесь же.
func (s *coldLoadStateR659) putProbeR659(backendID, model string, now time.Time, probe coldLoadProbeR659) {
	key := backendID + "\x00" + model
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.probes == nil {
		s.probes = make(map[string]coldLoadProbeEntryR659, 4)
	}
	for k, entry := range s.probes {
		if now.Sub(entry.At) >= coldLoadProbeCacheTTL {
			delete(s.probes, k)
		}
	}
	s.probes[key] = coldLoadProbeEntryR659{At: now, Probe: probe}
}

// coldLoadAutoLoadAllowedR659 — разрешено ли вообще инициировать загрузку
// запросом. Тот же гейт, что у этапа 3b (backend_selector.go): async-автозагрузка
// по умолчанию включена, sync — флагом конфигурации. Запрет оператора не
// обходится: иначе «свободный узел» стал бы лазейкой вокруг выключателя.
func (p *Proxy) coldLoadAutoLoadAllowedR659() bool {
	if p == nil {
		return false
	}
	if p.lbAutoLoadAsync {
		return true
	}
	return p.config != nil && p.config.Balancing.SyncModelLoad.Enabled
}

// coldLoadGeometryR659 — архитектура модели из ЛЮБОЙ загруженной копии.
//
// Файл модели один и тот же на всех узлах, поэтому геометрия (слои, головы KV,
// head_dim) — свойство модели, а не узла. Вопрос «влезет ли окно на СВОБОДНОМ
// узле» этой геометрией и решается; берём копию с самым широким окном как
// наиболее представительную.
//
// Возвращает (профиль, тип KV, ok). ok=false — ни одной загруженной копии с
// метаданными нет: KV проверить нечем, и это честно сообщается наверх.
func (p *Proxy) coldLoadGeometryR659(model string) (*ModelProfileInfo, string, bool) {
	if p == nil || p.metricsMgr == nil || model == "" {
		return nil, "", false
	}
	p.metricsMgr.mu.RLock()
	defer p.metricsMgr.mu.RUnlock()
	var best *ModelProfileInfo
	bestWindow := -1
	kvType := ""
	for _, lm := range p.metricsMgr.llamaMetrics {
		if lm == nil {
			continue
		}
		for _, m := range lm.LoadedModels {
			if !modelNameMatches(m.Name, model) {
				continue
			}
			if m.NLayers <= 0 || m.NKvHeads <= 0 || m.HeadDimK <= 0 {
				continue
			}
			window := m.ContextPerSeq
			if window <= 0 {
				window = m.ContextLength
			}
			if best == nil || window > bestWindow {
				bestWindow = window
				best = &ModelProfileInfo{
					Name:                 m.Name,
					Quantization:         m.Quantization,
					Architecture:         m.Architecture,
					NLayers:              m.NLayers,
					NEmbd:                m.NEmbd,
					NKvHeads:             m.NKvHeads,
					HeadDimK:             m.HeadDimK,
					HeadDimV:             m.HeadDimV,
					SizeBytes:            m.Size,
					CurrentContextLength: m.ContextLength,
					CurrentKVCacheType:   m.KvCacheType,
				}
				// R707: раскладка KV — иначе вердикт «влезает ли окно» считался
				// по всем слоям и отклонял узел, где модель физически грузится.
				applyKVPlanToProfileR707(best, &m)
			}
			if kvType == "" && m.KvCacheType != "" {
				kvType = m.KvCacheType
			}
		}
	}
	if best == nil {
		return nil, "", false
	}
	if kvType == "" {
		// Тип KV = намерение профиля, иначе безопасный верхний предел f16
		// (KV больше → оценка консервативнее, ложного «влезает» не будет).
		kvType = p.reloadHintsFor("", model).KVCacheType
	}
	if kvType == "" {
		kvType = "f16"
	}
	return best, kvType, true
}

// coldLoadSlotsR659 — сколько слотов будет у новой копии.
//
// Нужно потому, что KV-кэш выделяется НА КАЖДЫЙ слот: при parallel=2 суммарный
// KV вдвое больше KV одного окна. Источник — профиль модели (то же число уедет
// в теле загрузки через applyReloadHintsToPayload), затем факт загруженной
// копии, затем 1.
func (p *Proxy) coldLoadSlotsR659(model string) int {
	if p == nil || model == "" {
		return 1
	}
	if prof, ok := profileForReloadHints(p.profilesSnapshotR423(), model); ok && prof.Parallel > 0 {
		return prof.Parallel
	}
	if p.metricsMgr != nil {
		p.metricsMgr.mu.RLock()
		defer p.metricsMgr.mu.RUnlock()
		for _, lm := range p.metricsMgr.llamaMetrics {
			if lm == nil {
				continue
			}
			for _, m := range lm.LoadedModels {
				if !modelNameMatches(m.Name, model) {
					continue
				}
				if m.MaxSlots > 0 {
					return m.MaxSlots
				}
				if m.Parallel > 0 {
					return m.Parallel
				}
			}
		}
	}
	return 1
}

// coldLoadReserveMBR659 — резерв под CUDA-контекст и compute-буферы.
//
// Одна политика с AutoTune (autotuneReserveBytes): max(256 МБ, 5% total VRAM).
// При неизвестном total берётся минимум — резерв не должен превращаться в
// «неизвестно», иначе проверка окна не сработает на стенде без телеметрии.
func (p *Proxy) coldLoadReserveMBR659(backendID string) int {
	totalMB := 0
	if p != nil && p.metricsMgr != nil {
		p.metricsMgr.mu.RLock()
		if lm := p.metricsMgr.llamaMetrics[backendID]; lm != nil {
			totalMB = int(lm.TotalVRAMMB)
		}
		p.metricsMgr.mu.RUnlock()
	}
	if totalMB <= 0 {
		return coldLoadMinReserveMBR659
	}
	reserve := int(autotuneReserveBytes(mbToBytes(uint64(totalMB))) / bytesPerMB)
	if reserve < coldLoadMinReserveMBR659 {
		reserve = coldLoadMinReserveMBR659
	}
	return reserve
}

// coldLoadWindowFitPartsR659 — чистая арифметика вердикта (без замков и сети).
//
// Вынесена отдельно, чтобы её звали и решение о загрузке, и список причин
// why_queued (backend_rejections.go) — там уже держится p.mu.RLock, и брать
// внутри ещё один RLock нельзя (R284: рекурсивный RLock при стоящем писателе
// вешает балансер навсегда).
func coldLoadWindowFitPartsR659(geo *ModelProfileInfo, kvType string, window, weightsMB, freeMB, reserveMB, slots int) coldLoadFitR659 {
	fit := coldLoadFitR659{
		KVType:    kvType,
		WeightsMB: weightsMB,
		FreeMB:    freeMB,
		ReserveMB: reserveMB,
	}
	if geo == nil {
		fit.Reason = "геометрия модели неизвестна (нет ни одной загруженной копии) — KV проверить нечем"
		return fit
	}
	if window <= 0 {
		fit.Reason = "окно не названо"
		return fit
	}
	if weightsMB <= 0 {
		fit.Reason = "размер модели неизвестен"
		return fit
	}
	kvBytes := kvCacheBytesForContext(geo, kvType, window)
	if kvBytes <= 0 {
		fit.Reason = "KV для окна не посчитать (нет nLayers/nKvHeads/headDim)"
		return fit
	}
	if slots < 1 {
		slots = 1
	}
	// KV выделяется на каждый слот модели, а window — окно ОДНОГО клиента.
	fit.KVMB = int(uint64(slots) * kvBytes / bytesPerMB)
	fit.NeedMB = fit.WeightsMB + fit.KVMB + fit.ReserveMB
	fit.Verified = true
	fit.Fits = fit.FreeMB >= fit.NeedMB
	if fit.Fits {
		fit.Reason = "влезает"
	} else {
		fit.Reason = "не влезает"
	}
	return fit
}

// coldLoadWindowFitR659 — вердикт по текущим метрикам узла.
func (p *Proxy) coldLoadWindowFitR659(backendID, model string, window int) coldLoadFitR659 {
	if p == nil || backendID == "" || model == "" || window <= 0 {
		return coldLoadFitR659{Reason: "окно или узел не названы"}
	}
	freeMB, freeKnown := p.backendVramFreeMB(backendID)
	weights := p.getModelSizeBytes(model)
	weightsMB := int(weights / bytesPerMB)
	geo, kvType, geoOK := p.coldLoadGeometryR659(model)
	if !geoOK {
		geo = nil
	}
	if !freeKnown {
		// Метрики VRAM нет — проверить нечем. Не блокируем (этап 3b поступает так
		// же), но и «влезает» не утверждаем: Verified=false.
		return coldLoadFitR659{
			KVType:    kvType,
			WeightsMB: weightsMB,
			Reason:    "свободная VRAM узла неизвестна",
		}
	}
	reserveMB := p.coldLoadReserveMBR659(backendID)
	slots := p.coldLoadSlotsR659(model)
	return coldLoadWindowFitPartsR659(geo, kvType, window, weightsMB, freeMB, reserveMB, slots)
}

// coldLoadCandidateIsColdR659 — узел ДОСТОВЕРНО без этой модели.
//
// «Неизвестно» (метрика устарела / узла ещё не опрашивали) сюда не входит
// намеренно: если модель там на самом деле загружена с ШИРОКИМ окном, наша
// загрузка перезагрузила бы её с более узким — ровно то сжатие под живым
// клиентом, которое запрещено (R294/R459). Ждём опрос поллера (≤30 с).
func (p *Proxy) coldLoadCandidateIsColdR659(backendID, model string) bool {
	perSeq, known := p.loadedPerSeqNCtxKnownR480(backendID, model)
	return known && perSeq <= 0
}

// planColdLoadR659 — выбрать узел под загрузку с окном window.
//
// Кандидаты — тот же набор, что у этапа 3b (свободные узлы + узлы без метрик),
// и тот же предикат «можно грузить сейчас» (canLoadModelNowR493): свободный слот,
// ресурсы, файл модели на диске, отсутствие image-генерации, веса влезают в
// свободную VRAM. Поверх него — проверка ОКНА (KV под нужное окно), которой у
// 3b не было: там окно неизвестно, здесь оно известно и обязано влезть.
//
// Приоритет: сначала ВЕРИФИЦИРОВАННЫЙ кандидат (по score), и только если таких
// нет — неверифицированный (нет метрик VRAM/геометрии): на стенде без
// телеметрии «не грузить никогда» хуже, чем «попробовать» (3b-паритет).
func (p *Proxy) planColdLoadR659(model string, bt types.BackendType, window int) (coldLoadPlanR659, bool) {
	if p == nil || model == "" || window <= 0 {
		return coldLoadPlanR659{}, false
	}
	if !p.coldLoadAutoLoadAllowedR659() {
		return coldLoadPlanR659{}, false
	}
	allowed := p.getAllowedTypesList(bt)
	// ГЕЙТ «ПРОГРЕТАЯ КОПИЯ СУЩЕСТВУЕТ». R659 — не холодный старт: его предмет —
	// «живая копия занята, создадим вторую». Если модели нет нигде, решает обычный
	// выбор (этап 3b/3c), и подменять его нельзя: у R493 уже был живой регресс,
	// когда «выбери свободный узел» забрал заявки у нормального распределения
	// (TestReplication_Scenario_LiveRequestReplicated). Заодно этот гейт
	// гарантирует геометрию для проверки окна: у прогретой копии она есть.
	if !p.modelLoadedSomewhereR493(model, allowed) {
		if logger.Get() != nil {
			logger.Get().Debugw("R659: «загрузка вместо ожидания» не применима — прогретой копии в кластере нет",
				"model", model, "reason", "no_warm_copy")
		}
		return coldLoadPlanR659{}, false
	}
	candidates := p.loadableCandidateIDsR493(model, allowed)
	if len(candidates) == 0 {
		return coldLoadPlanR659{}, false
	}
	var bestVerified, bestUnverified coldLoadPlanR659
	bestVerifiedScore, bestUnverifiedScore := -1.0, -1.0
	for _, backendID := range p.orderCandidatesForSelectionR493(candidates, model) {
		if ok, _ := p.canLoadModelNowR493(backendID, model); !ok {
			continue
		}
		// Только llama.cpp: окно задаётся contextPerSeq, и его понимает только
		// воркер. У Ollama-узла «окно загрузки» не выбирается — это был бы
		// переезд без гарантии окна (R459).
		if backend := p.GetBackend(backendID); backend == nil ||
			types.ResolveEngine(backend.Engine, backend.Type) != types.EngineLlamaCPP {
			continue
		}
		if !p.coldLoadCandidateIsColdR659(backendID, model) {
			continue
		}
		// R663: ЖИВАЯ ПРОВЕРКА КАНДИДАТА.
		//
		// Кэш метрик отстаёт на интервал поллера (30 с), поэтому «по кэшу холодный»
		// не значит «сейчас холодный»: узел мог уже поднять модель (в том числе по
		// этой же причине R659) или быть донором раскладки, который собственных
		// копий не держит и отвечает 409. Оба случая стоили лишнего POST /load на
		// живом стенде — здесь они отсекаются до отправки.
		probe := p.probeColdLoadCandidateR659(backendID, model)
		if probe.Known {
			if !probe.CanLoadOwn {
				if logger.Get() != nil {
					logger.Get().Debugw("R663: кандидат пропущен — узел не держит собственных копий модели",
						"backend", backendID, "model", model, "reason", probe.Reason)
				}
				continue
			}
			if probe.ModelPresent {
				// Узел уже держит или грузит модель — просить загрузку нечего;
				// заявку обслужит очередь, когда копия будет готова.
				continue
			}
		}
		// Дедупликация загрузки: одна попытка на узел, но не «навсегда по таймеру».
		// Если живая проверка показала, что модели нет и она не грузится, значит
		// попытка не подхватилась (отказ, провал) — после короткой паузы повторяем.
		if at, started := p.coldLoadStateR659.startedAtR659(backendID, model); started {
			if probe.Known && !probe.ModelPresent {
				if time.Since(at) < coldLoadRetryAfterFailedR659 {
					continue
				}
				p.coldLoadStateR659.clearStartedR659(backendID, model)
			} else {
				continue
			}
		}
		fit := p.coldLoadWindowFitR659(backendID, model, window)
		if fit.Verified && !fit.Fits {
			if logger.Get() != nil {
				logger.Get().Debugw("R659: свободный узел пропущен — нужное окно не влезает",
					"backend", backendID, "model", model, "window", window,
					"need_mb", fit.NeedMB, "free_mb", fit.FreeMB,
					"weights_mb", fit.WeightsMB, "kv_mb", fit.KVMB,
					"reserve_mb", fit.ReserveMB, "kv_type", fit.KVType)
			}
			continue
		}
		score := p.calculateScore(backendID)
		plan := coldLoadPlanR659{BackendID: backendID, Window: window, Fit: fit}
		if fit.Verified {
			if score > bestVerifiedScore {
				bestVerified, bestVerifiedScore = plan, score
			}
			continue
		}
		if score > bestUnverifiedScore {
			bestUnverified, bestUnverifiedScore = plan, score
		}
	}
	if bestVerified.BackendID != "" {
		return bestVerified, true
	}
	if bestUnverified.BackendID != "" {
		return bestUnverified, true
	}
	return coldLoadPlanR659{}, false
}

// startColdLoadR659 — запустить загрузку модели на выбранном узле.
//
// Загрузка идёт через штатный warmupModel: он проверяет health узла, резолвит
// путь к файлу через /api/models/files (иначе воркер сканирует каталог и может
// не найти имя), добавляет хинты профиля (kvCacheType/parallel/enableReasoning)
// и предъявляет admin-токен воркера (R307). Здесь добавлено ровно одно —
// ОКНО: NumCtx/ContextPerSeq = plan.Window, то есть окно на КЛИЕНТА (R83),
// а не суммарное (суммарное воркер считает сам как PAD256(per) × слоты).
func (p *Proxy) startColdLoadR659(plan coldLoadPlanR659, model string) bool {
	if p == nil || plan.BackendID == "" || model == "" || plan.Window <= 0 {
		return false
	}
	backend := p.GetBackend(plan.BackendID)
	if backend == nil {
		return false
	}
	// Breaker: узел, который уже проваливал загрузку этой модели, не долбим —
	// иначе каждая заявка в очереди повторяла бы заведомо неудачную попытку.
	if lr := p.llamaCppRouter; lr != nil && lr.loadBackoff != nil {
		if skip, reason, until := lr.loadBackoff.shouldSkip(plan.BackendID, model); skip {
			if logger.Get() != nil {
				logger.Get().Infow("R659: загрузка не начата — узел недавно проваливал загрузку",
					"backend", plan.BackendID, "model", model,
					"reason", reason, "breaker_until", until.Format(time.RFC3339))
			}
			return false
		}
	}
	if !p.coldLoadStateR659.markStartedR659(plan.BackendID, model, time.Now()) {
		return false
	}
	fit := plan.Fit
	// АСИНХРОННО, ХОТЯ warmupModel И САМ УЖЕ ЗАГРУЖАЕТ АСИНХРОННО. Его
	// синхронная часть — это HTTP-запросы к воркеру (health, /api/models/files),
	// а p.client собран с Timeout:0 (таймаут задаётся контекстом запроса). Узел,
	// который принимает TCP и молчит, подвесил бы обработчик клиента ещё до
	// очереди. Здесь ждать нечего: решение принято, дальше — работа воркера.
	go p.warmupModel(plan.BackendID, backend.Host, backend.OllamaPort, model,
		warmupOptions{NumCtx: plan.Window, ContextPerSeq: plan.Window})
	if logger.Get() != nil {
		logger.Get().Infow("R659: загрузка вместо ожидания — на свободном узле начата загрузка модели",
			"backend", plan.BackendID, "model", model,
			"window", plan.Window, "window_role", "context_per_seq",
			"need_mb", fit.NeedMB, "free_mb", fit.FreeMB,
			"weights_mb", fit.WeightsMB, "kv_mb", fit.KVMB,
			"reserve_mb", fit.ReserveMB, "kv_type", fit.KVType,
			"verified", fit.Verified, "reason", fit.Reason,
			"effect", "заявка продолжает ждать слот; обслужится тем, что станет готово раньше")
	}
	return true
}

// maybeStartColdLoadR659 — решить и, если можно, запустить загрузку. Возвращает
// план (для логов/тестов) и признак «запустили».
func (p *Proxy) maybeStartColdLoadR659(model string, bt types.BackendType, window int) (coldLoadPlanR659, bool) {
	plan, ok := p.planColdLoadR659(model, bt, window)
	if !ok {
		return coldLoadPlanR659{}, false
	}
	if !p.startColdLoadR659(plan, model) {
		return plan, false
	}
	return plan, true
}

// startColdLoadBeforeQueueR659 — точка входа из ServeHTTP: заявка не получила
// слот ни на одной прогретой копии (retry-цикл по loaded исчерпан).
//
// МЕСТО В ПОТОКЕ ВАЖНО. Вызывается ПОСЛЕ решения R301 о росте окна и ПЕРЕД
// очередью: если окно можно поднять на прогретой копии (R385/R447), то это и
// есть выбранное действие, и параллельная загрузка второй копии была бы лишней
// работой. Если роста не требуется, заявка узнаёт, что слот не взят нигде, — и
// тогда создаём ёмкость.
//
// Заявка при этом НЕ переезжает: она остаётся в очереди и обслуживается тем, что
// станет готово раньше — своя копия (слот освободился) или новая (загрузка
// закончилась). R139 в силе: уход живой сессии на холодный узел и ожидание там
// стоили живого агентского хода (pi-ai idle timeout).
//
// Окно для загрузки — «нужное окно» переезда (session_move_need.go, вариант C):
// max(потребность запроса, окно активной сессии, названное клиентом окно). Меньше
// любого из них поднимать нельзя — это сжало бы окно под живым клиентом
// (R294/R459).
func (p *Proxy) startColdLoadBeforeQueueR659(
	model string, bt types.BackendType, parsed *parsedRequest, fromBackend, sessionID, path string,
) {
	if p == nil || parsed == nil || model == "" {
		return
	}
	need, _ := p.sessionMoveNeedR480(fromBackend, model, sessionID, parsed.NumCtx,
		p.promptTokensForMoveR480(parsed.RawBody, path))
	if need <= 0 {
		return
	}
	p.maybeStartColdLoadR659(model, bt, need)
}
