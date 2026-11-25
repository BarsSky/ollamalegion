// availability_first.go — R493 (2026-11-11): ДОСТУПНОСТЬ КАК ПРЕДИКАТ.
//
// ЖИВОЙ ДЕФЕКТ (разбор: docs/architecture/availability-first-r493.ru.md, по
// архивам второго стенда 2026-11-11). Мастер (A10, 27B загружена, 2.4 ГБ VRAM
// свободно) был занят одной генерацией, рядом простаивал A10-126 (20.5 ГБ
// свободно) — и запрос встал в ОЧЕРЕДЬ к мастеру на 300 с. В логах свободных
// узлов ни одной попытки загрузки: балансер даже не пытался поднять модель там.
//
// ПОЧЕМУ ТАК. Выбор бэкенда судил о занятости по КОЭФФИЦИЕНТУ
// `ActiveReqs / MaxConcurrentReqs` с порогом 0.8. В этом кластере у ВСЕХ узлов
// `maxConcurrentRequests = 1`, поэтому коэффициент принимал только 0 и 1 и
// ничего не говорил о реальном слоте; а «привязка сессии» действовала как замок
// (proxy.go передавал бэкенд сессии в очередь без проверки доступности).
//
// ПРАВИЛО R493. Кандидат «доступен сейчас» — это ПРЕДИКАТ (есть свободный слот),
// а не коэффициент. И если ни один узел с загруженной моделью не принимает
// сейчас, а подходящий свободный узел есть (health, ресурсы, VRAM хватает), то
// заявка едет туда и модель там поднимается — очередь остаётся последним
// вариантом, а не первым.
package balancer

import (
	"fmt"
	"strings"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// hasFreeSlotNowR493 — есть ли на бэкенде свободный слот ПРЯМО СЕЙЧАС.
//
// Отличие от canAcceptRequest: тот смотрит коэффициент с порогом 0.8 и мягкие
// ресурсные пороги (для прогрева/скоринга это правильно). Здесь нужен именно
// ответ «займём ли слот, не встав в очередь».
func (p *Proxy) hasFreeSlotNowR493(backendID string) bool {
	if p == nil || strings.TrimSpace(backendID) == "" {
		return false
	}
	p.mu.RLock()
	state, ok := p.backends[backendID]
	p.mu.RUnlock()
	if !ok || state == nil || state.Backend == nil {
		return false
	}
	if state.Backend.Status != types.StatusHealthy && state.Backend.Status != "" {
		return false
	}
	if p.backendDisabled(backendID) {
		return false
	}
	state.mu.Lock()
	active := state.ActiveReqs
	maxReqs := state.Backend.EffectiveMaxConcurrentRequests()
	state.mu.Unlock()
	// R501 / аудит C-3: одна трактовка числа (types.CapacityFromValueR501).
	// -1 документирован как «без лимита» (handlers_backends.go) — здесь он больше
	// НЕ превращается в «лимит 1»: бэкенд с «без лимита» доступен. Ноль = «не
	// задано» → консервативный дефолт R117: один слот (пока не известны
	// фактические слоты модели), а не «сколько угодно».
	if cap := types.CapacityFromValueR501(maxReqs); cap.Unlimited {
		return true
	} else if !cap.Known {
		maxReqs = 1
	} else {
		maxReqs = cap.Value
	}
	return active < maxReqs
}

// stickyBackendIfAvailableR493 — бэкенд привязки сессии, но только если он может
// принять запрос СЕЙЧАС.
//
// R493: привязка — ПРЕДПОЧТЕНИЕ, а не замок. Раньше backend сессии безусловно
// уезжал в ожидание слота (R480/P1-7), и заявка ждала занятый узел, пока рядом
// простаивал подходящий. Правило R480 остаётся в силе для случая «слот есть»:
// прогретая копия побеждает; при занятости — обычный выбор.
func (p *Proxy) stickyBackendIfAvailableR493(sessionKey string) string {
	if p == nil || sessionKey == "" || p.sessionMgr == nil {
		return ""
	}
	s, ok := p.sessionMgr.GetSnapshot(sessionKey)
	if !ok || s.BackendID == "" {
		return ""
	}
	if !p.hasFreeSlotNowR493(s.BackendID) {
		if logger.Get() != nil {
			logger.Get().Infow("R493: привязка сессии не действует — у бэкенда нет свободного слота, решает доступность",
				"backend", s.BackendID, "session", sessionKey)
		}
		return ""
	}
	return s.BackendID
}

// pickLoadableFreeBackendR493 — подходящий СВОБОДНЫЙ бэкенд, где модель можно
// поднять (health + свободный слот + ресурсы + VRAM хватает).
//
// Раньше загрузку на свободном узле запускал только этап P3 и только при
// `SyncModelLoad.Enabled`; при срабатывании P1 (модель загружена у занятого
// узла) до P3 дело не доходило. Здесь — отдельный выбор, который вызывается
// ПОСЛЕ того, как выяснилось, что принимающего узла среди loaded нет.
//
// КОГО РАССМАТРИВАЕМ:
//   - P3 (FREE): узел без модели в памяти и со свободным слотом — обычный случай;
//   - узлы ТОЛЬКО из P4 (FALLBACK): это, как правило, узлы БЕЗ МЕТРИК. Именно они
//     дали живой дефект: computeNoMetricsScore даёт такому узлу score 1, занятый
//     мастер выглядит лучше по score, и заявка встаёт в очередь к мастеру, хотя
//     свободная машина простаивает. Загруженные узлы, не попавшие в P1 из-за
//     порога прогрева, НЕ рассматриваются: их выбирать «вместо очереди» нельзя,
//     иначе порог LB_PREWARM_TRIGGER_LOAD перестал бы действовать.
//   - и узел обязан ЗНАТЬ эту модель: если каталог узла прочитан и файла модели
//     там нет, узел пропускается (долг 11b; см. комментарий в цикле).
//
// КАК ВЫБИРАЕМ: R248 (VRAM-fit) — узел, где модель не влезает, отбрасывается
// (27B на 8-гигабайтной карте не поднимется, R493 п.5). Среди оставшихся —
// обычный score того же вида, что в P4: свободный узел не должен выигрывать у
// более подходящего только потому, что перебор шёл по map. «VRAM неизвестна» не
// запрещает выбор — иначе стенд без метрик не поднимал бы ничего.
// stickyBackendForGateR493 — бэкенд сессии как ИСТОЧНИК ОКНА для гейта R459.
//
// R493-уточнение (2026-11-12, решение оператора по архиву стенда A10 ×2):
//   - если копия сессии СВОБОДНА — работает предпочтение (мгновенный слот);
//   - если ЗАНЯТА, а бэкенд с нужным окном есть — заявка ждёт слот ИМЕННО у него,
//     а не уезжает на свободную копию с МЕНЬШИМ окном и не пытается грузить модель
//     на узле, где её нет.
//
// Поэтому «предпочтение» (stickyBackendIfAvailableR493) и «источник окна»
// (эта функция) — РАЗНЫЕ вещи. Первое отдаёт бэкенд только при свободном слоте;
// второе отдаёт его всегда, чтобы очередь построила гейт «кандидат покрывает
// окно сессии» и встала в очередь к подходящей копии.
//
// Живой дефект, который это лечит (архив 2026-11-12T11:21): мастер держал 27B с
// окном 198656 и был занят, A10-126 держал ту же модель с окном 131072 и был
// свободен. После R493 гейт не строился вовсе (from=""), очередь взяла свободный
// A10-126 с меньшим окном, дальше пошли загрузки на 8-гигабайтные узлы и заявка
// получила 503 на стадии select_backend.
func (p *Proxy) stickyBackendForGateR493(sessionKey string) string {
	if p == nil || sessionKey == "" || p.sessionMgr == nil {
		return ""
	}
	s, ok := p.sessionMgr.GetSnapshot(sessionKey)
	if !ok || s.BackendID == "" {
		return ""
	}
	return s.BackendID
}

// vramNeedVsFreeR493 — сколько нужно под модель и сколько свободно у узла.
//
// Одна и та же арифметика для двух мест, чтобы они не разошлись: выбор узла под
// загрузку (pickLoadableFreeBackendR493) и причина отказа в why_queued
// (backendRejectionsR460). Правило R248/R493: потребность = размер файла × 1.05
// (KV-cache и буферы контекста), «нет данных» не считается отказом.
//
// known=false, если размер модели неизвестен ИЛИ у узла нет данных о VRAM:
// тогда причину называть нечем, и вызывающий оставляет прежнюю формулировку.
func (p *Proxy) vramNeedVsFreeR493(backendID, model string) (needMB float64, freeMB int, known bool) {
	if p == nil {
		return 0, 0, false
	}
	sizeBytes := p.getModelSizeBytes(model)
	if sizeBytes <= 0 {
		return 0, 0, false
	}
	needMB = float64(sizeBytes) / (1024 * 1024) * vramFitSlack
	freeMB, vrOK := p.backendVramFreeMB(backendID)
	if !vrOK {
		return needMB, 0, false
	}
	return needMB, freeMB, true
}

// formatGiBR493 — МБ в «N.N GiB» для сообщений оператору. Гибибайты, потому что
// и размер модели, и свободная VRAM считаются в МиБ; «ГБ» в подписи путало бы.
func formatGiBR493(mb float64) string {
	return fmt.Sprintf("%.1f GiB", mb/1024)
}

// modelLoadedSomewhereR493 — есть ли модель в памяти ХОТЯ БЫ на одном узле,
// который вообще рассматривается для этой заявки.
//
// ЗАЧЕМ ЭТОТ ПРЕДИКАТ. R493 решает узкий случай: модель УЖЕ загружена, но её
// узел занят, а рядом простаивает свободный. Без такой проверки этап 3b
// превращался в «выбери первый свободный по score» и забирал заявки у обычного
// распределения: живой регресс — TestReplication_Scenario_LiveRequestReplicated
// (10 запросов к двум узлам БЕЗ метрик уходили на один, второй получал 0, а
// алгоритм round-robin переставал действовать). Модель не загружена нигде —
// значит, никакого «занятого узла с прогретой копией» нет, и решать должен
// обычный выбор, а не этот этап.
func (p *Proxy) modelLoadedSomewhereR493(model string, allowedTypes []types.BackendType) bool {
	if p == nil || model == "" || p.metricsMgr == nil {
		return false
	}
	p.mu.RLock()
	ids := make([]string, 0, len(p.backends))
	for id, state := range p.backends {
		if state == nil || state.Backend == nil {
			continue
		}
		if isDisabledBackendState(state) || p.cannotHostModelR304(id) {
			continue
		}
		if !isBackendTypeAllowed(normalizeBackendType(state.Backend.Type), allowedTypes) {
			continue
		}
		ids = append(ids, id)
	}
	p.mu.RUnlock()
	for _, id := range ids {
		if p.backendHasModelByID(id, model) {
			return true
		}
	}
	return false
}

// modelHasOnDiskSomewhereR493 — R552 (F8): лежит ли файл модели на диске ХОТЯ БЫ
// одного узла, который вообще рассматривается для этой заявки.
//
// ЧТО ЗНАЕТ КАТАЛОГ. Список файлов модели на узле приходит из воркера (R93) и
// обновляется фоном (R514); `presence` возвращает (has, known, files, dirError).
// «Известно и есть» — единственный факт, на который здесь можно опираться. Пока
// каталог не опрошен (`known=false`) или его чтение падало (`dirError != ""`),
// функция отвечает «нет»: догадка «наверное, файл там есть» ломала бы обычное
// распределение ровно так, как это было в регрессе R493.
func (p *Proxy) modelHasOnDiskSomewhereR493(model string, allowedTypes []types.BackendType) bool {
	if p == nil || model == "" || p.modelCatalog == nil {
		return false
	}
	p.mu.RLock()
	ids := make([]string, 0, len(p.backends))
	for id, state := range p.backends {
		if state == nil || state.Backend == nil {
			continue
		}
		if isDisabledBackendState(state) || p.cannotHostModelR304(id) {
			continue
		}
		if !isBackendTypeAllowed(normalizeBackendType(state.Backend.Type), allowedTypes) {
			continue
		}
		ids = append(ids, id)
	}
	p.mu.RUnlock()
	for _, id := range ids {
		has, known, _, dirErr, _ := p.backendCatalogPresenceR493(id, model)
		if known && dirErr == "" && has {
			return true
		}
	}
	return false
}

// Коды причины переезда R757 (для лога и тестов).
const (
	// reasonFileAbsentEverywhereR757 — файла модели нет нигде: переезжать некуда.
	reasonFileAbsentEverywhereR757 = "file_absent_everywhere_in_catalog"
	// reasonMovedToFileNodeR757 — файл есть на другом узле: поехали туда.
	reasonMovedToFileNodeR757 = "selected_backend_file_absent"
)

// relocateToFileNodeR757 — если на выбранном узле НЕТ файла модели, а на другом
// узле он есть, вернуть этот другой узел.
//
// ЖИВАЯ ЖАЛОБА (архив второго стенда, 06:10): «она не стартанула с запроса от
// пользователя, а поднятая вручную работала». Разбор: выбор отдал A10-126, где
// ФАЙЛА модели нет (каталог это знал — `R696: привязка сессии не создана ...
// reason=model_absent_on_backend`), а сам файл лежал на
// cppworker-gpu-bundled-agent. Гейт автозагрузки честно попытался поднять модель
// на A10-126, воркер ответил `model ... not found in ./models`, и клиент получил
// терминальный 404 (R325.2). Ручная загрузка на узле с файлом подняла модель за
// 8.2 с.
//
// ПОЧЕМУ ЭТОГО НЕ ЛОВИЛИ РАНЬШЕ. R303 спасает только когда модель УЖЕ загружена
// на другом узле (живая копия). Здесь живых копий не было: модель только что
// выгрузили или пересобрали, и «правильный» узел определялся наличием ФАЙЛА на
// диске, а не содержимым VRAM. R552 учит смотреть в каталог выбор ПОД ЗАГРУЗКУ
// (этапы очереди), но к уже выбранному узлу это не применялось.
//
// Условия намеренно узкие, чтобы не переписывать обычное распределение:
//   - каталог выбранного узла ПРОЧИТАН (known) и без ошибки чтения (dirErr);
//   - файла там НЕТ;
//   - модель на этом узле НЕ загружена (иначе каталог устарел, а копия в памяти
//     рабочая и переезд только что-то сломает);
//   - файл есть хотя бы на одном другом узле.
//
// Возвращает ("", "") когда делать нечего (каталог неизвестен/файл на месте) и
// ("", reasonFileAbsentEverywhereR757) когда файла нет нигде — это отдельный факт
// для лога: ожидание загрузки не поможет, надо копировать GGUF.
func (p *Proxy) relocateToFileNodeR757(currentBackend, model string, allowedTypes []types.BackendType) (string, string) {
	if p == nil || currentBackend == "" || model == "" {
		return "", ""
	}
	if p.backendHasModelByID(currentBackend, model) {
		// Модель уже в памяти этого узла: каталог разошёлся с реальностью (файл
		// унесли после загрузки) либо кэш метрик отстал. Переезд не нужен.
		return "", ""
	}
	hasFile, known, _, dirErr, _ := p.backendCatalogPresenceR493(currentBackend, model)
	if !known || dirErr != "" {
		// Каталог не опрошен или его чтение падало: «файла нет» здесь было бы
		// догадкой, а догадка ломает обычное распределение (регресс R493).
		return "", ""
	}
	if hasFile {
		return "", ""
	}
	// Сначала — узел, который МОЖЕТ принять заявку сейчас (слот, ресурсы, VRAM,
	// файл по каталогу). Это лучший исход: модель поднимется без ожидания.
	if alt := p.pickLoadableFreeBackendR493(model, allowedTypes); alt != "" && alt != currentBackend {
		return alt, reasonMovedToFileNodeR757
	}
	// Свободного узла с файлом нет — годится любой наименее загруженный узел с
	// файлом: даже если он занят, это узел, где загрузка ВОЗМОЖНА, а выбранный
	// гарантированно даёт 404 model_not_found.
	if alt := p.findLeastLoadedBackendWithModelOnDisk(model, allowedTypes); alt != "" && alt != currentBackend {
		return alt, reasonMovedToFileNodeR757
	}
	return "", reasonFileAbsentEverywhereR757
}

// backendsWithModelOnDiskR757 — узлы, где файл модели ЛЕЖИТ на диске (по каталогу).
//
// ЗАЧЕМ. Терминальный 404 `model_not_found` (R325.2) раньше не говорил оператору
// НИЧЕГО о том, устранимо ли это: «модели нет» на выбранном узле и «модели нет в
// кластере» — разные ситуации. Живой разбор второго стенда: файл лежал на другом
// узле, а клиент получил 404 и оператор пошёл искать причину в логи воркера.
// Теперь ответ называет узлы, где файл есть, чтобы вопрос «куда переехать» решался
// по ответу API.
//
// Отдаются только узлы, каталог которых ПРОЧИТАН и без ошибки каталога: «файл,
// возможно, есть» здесь было бы ложью, а такие узлы уже отсекаются в остальных
// проверках.
func (p *Proxy) backendsWithModelOnDiskR757(model string) []string {
	if p == nil || model == "" || p.modelCatalog == nil {
		return nil
	}
	p.mu.RLock()
	ids := make([]string, 0, len(p.backends))
	for id, state := range p.backends {
		if state == nil || state.Backend == nil {
			continue
		}
		if isDisabledBackendState(state) {
			continue
		}
		ids = append(ids, id)
	}
	p.mu.RUnlock()
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if has, known, _, dirErr, _ := p.backendCatalogPresenceR493(id, model); known && dirErr == "" && has {
			out = append(out, id)
		}
	}
	return out
}

func (p *Proxy) pickLoadableFreeBackendR493(model string, allowedTypes []types.BackendType) string {
	if p == nil {
		return ""
	}
	// R552 (F8 плана 2026-11-19-webui-and-balancer-fixes, §2.8): «загрузка вместо
	// очереди» доступна не только когда прогретая копия где-то ЕСТЬ и занята, но и
	// когда моделей в VRAM нет вовсе, а ФАЙЛ МОДЕЛИ лежит на диске узла.
	//
	// ЖИВАЯ ЖАЛОБА: «балансер не грузит модель на свободный узел, а ждёт». Разбор:
	// гейт ниже требовал `modelLoadedSomewhereR493`, то есть прогретой копии в
	// кластере. Стенд после рестарта/выгрузки: в VRAM никого, файл на диске есть,
	// свободный узел есть — а балансер не начинал загрузку и ставил клиента в
	// очередь ждать копию, которой никто не создаст.
	//
	// Почему нельзя было просто снять гейт: тогда этап 3b «уводил» бы заявки при
	// ЛЮБОМ запросе (живой регресс R493: два узла без метрик, round-robin перестал
	// действовать). Поэтому условие расширено ровно на факт «файл есть на диске»,
	// который видит каталог (R93/R514), а не на догадку. Если каталог не опрошен —
	// поведение прежнее.
	if !p.modelLoadedSomewhereR493(model, allowedTypes) && !p.modelHasOnDiskSomewhereR493(model, allowedTypes) {
		if logger.Get() != nil {
			// Debug, а не Info: функция зовётся на каждом шаге ожидания (живой
			// прогон R493 — до 4 строк в секунду). Оператору адресована строка
			// `why_queued`, которая называет эту же причину однократно.
			logger.Get().Debugw("R552: «загрузка на свободный узел» не применима — модели нет ни в VRAM, ни в каталоге диска",
				"model", model, "reason", "no_warm_copy_and_no_catalog_file")
		}
		return ""
	}
	// R659 (2026-11-20): отбор и предикат вынесены в отдельные функции, чтобы
	// тот же выбор узла под загрузку использовала «загрузка вместо ожидания»
	// (cold_load_placement_r659.go). Двух копий условий быть не должно: они
	// разъезжаются ровно так, как это уже случалось в аудите (R493/R553).
	return p.pickLoadableFromCandidatesR493(p.loadableCandidateIDsR493(model, allowedTypes), model)
}

// loadableCandidateIDsR493 — КОГО вообще рассматриваем под загрузку модели.
//
// R493: свободные узлы (P3) и узлы, попавшие ТОЛЬКО в fallback (P4, обычно без
// метрик). Загруженные узлы, не прошедшие порог прогрева, сюда не входят: их
// выбирать «вместо очереди» нельзя, иначе порог LB_PREWARM_TRIGGER_LOAD перестал
// бы действовать.
func (p *Proxy) loadableCandidateIDsR493(model string, allowedTypes []types.BackendType) []string {
	if p == nil {
		return nil
	}
	var freeGroup, fallbackGroup []string
	loadedOrWarming := make(map[string]bool, 8)
	for _, group := range p.expandCandidates(model, allowedTypes) {
		for _, id := range group.BackendIDs {
			switch group.Priority {
			case 1, 2:
				loadedOrWarming[id] = true
			case 3:
				freeGroup = append(freeGroup, id)
			case 4:
				fallbackGroup = append(fallbackGroup, id)
			}
		}
	}
	freeSet := make(map[string]bool, len(freeGroup))
	for _, id := range freeGroup {
		freeSet[id] = true
	}
	candidates := make([]string, 0, len(freeGroup)+len(fallbackGroup))
	candidates = append(candidates, freeGroup...)
	for _, id := range fallbackGroup {
		if loadedOrWarming[id] || freeSet[id] {
			continue
		}
		candidates = append(candidates, id)
	}
	return candidates
}

// canLoadModelNowR493 — предикат «на этом узле модель можно ПОДНЯТЬ сейчас».
//
// Один предикат на все пути: этап 3b (выбор узла под загрузку), R659 (загрузка
// вместо ожидания) и why_queued. Второе значение — стабильный код причины
// отказа ("" = подходит); числа причины (сколько нужно/свободно) печатаются
// здесь же, чтобы вызывающий не собирал их второй раз.
func (p *Proxy) canLoadModelNowR493(backendID, model string) (bool, string) {
	if p == nil || backendID == "" {
		return false, rejectNoMetricsR460
	}
	if !p.hasFreeSlotNowR493(backendID) {
		return false, rejectSlotsBusyR460
	}
	if !p.canAcceptRequest(backendID) {
		return false, rejectOverLoadThresholdR460
	}
	state := p.backends[backendID]
	if state == nil || state.Backend == nil {
		return false, rejectNoMetricsR460
	}
	// R493 (долг 11b): «свободен» — ещё не «свободен для этой модели». Без
	// проверки каталога узел выбирался по VRAM, загрузка на нём падала
	// `model_not_found`, и клиент получал 503, хотя готовая копия стояла рядом (в
	// занятом узле) и достаточно было подождать слот. Условие исключения
	// намеренно узкое: только когда каталог узла ПРОЧИТАН (known) и воркер не
	// сообщил о недоступности каталога (dirErr). «Не опрашивали» и «каталог
	// отвалился» узлы не исключают: при выключенном поллере
	// (LB_MODEL_CATALOG_REFRESH_SEC=0) каталог пуст у всех, и строгая проверка
	// выкинула бы из выбора весь кластер.
	if hasOnDisk, catalogKnown, _, dirErr, _ := p.backendCatalogPresenceR493(backendID, model); catalogKnown && dirErr == "" && !hasOnDisk {
		// Уровень Debug, а не Info: функция вызывается КАЖДЫМ шагом ожидания в
		// очереди (живой прогон 2026-11-14 — 4 строки в секунду), а оператору
		// адресована одна строка `why_queued`, которая называет ту же причину по
		// каждому кандидату однократно.
		if logger.Get() != nil {
			logger.Get().Debugw("R493: свободный узел пропущен — файла модели нет на диске узла",
				"backend", backendID, "model", model, "reason", rejectFileAbsentR460)
		}
		return false, rejectFileAbsentR460
	}
	// Phase 6: пока на хосте идёт image-генерация, текстовую модель там не
	// поднимаем — GPU занят картинкой, и загрузка (140-260 с) всё равно встанет
	// в очередь за локом.
	if p.imageGateBlocksTextBackend(state) {
		return false, imageLockWaitReason
	}
	// R493/R248: размер файла × 1.05 против свободной VRAM. Это грубая проверка
	// «вообще влезает ли», окно сюда не входит — его проверяет R659
	// (coldLoadWindowFitR659), потому что у этапа 3b нужное окно неизвестно.
	sizeBytes := p.getModelSizeBytes(model)
	if sizeBytes > 0 {
		needMB := float64(sizeBytes) / (1024 * 1024) * vramFitSlack
		if freeMB, known := p.backendVramFreeMB(backendID); known && float64(freeMB) < needMB {
			if logger.Get() != nil {
				logger.Get().Infow("R493: свободный узел пропущен — модель не влезает в свободную VRAM",
					"backend", backendID, "model", model,
					"need_mb", int(needMB), "free_mb", freeMB)
			}
			return false, rejectVRAMInsufficientR460
		}
	}
	return true, ""
}

// pickLoadableFromCandidatesR493 — лучший по score среди прошедших предикат.
//
// R493 (12n, «на будущее»): порядок обхода — через шов selection_order.go. По
// умолчанию это прежний fit по VRAM; `LB_SELECTION_ORDER=leastconn` переставляет
// уже допущенных кандидатов так, что первым идёт менее занятый узел. Фильтры
// (слот, image-gate, каталог, fit) не меняются.
func (p *Proxy) pickLoadableFromCandidatesR493(candidates []string, model string) string {
	if p == nil || len(candidates) == 0 {
		return ""
	}
	var bestID string
	bestScore := -1.0
	for _, backendID := range p.orderCandidatesForSelectionR493(candidates, model) {
		if ok, _ := p.canLoadModelNowR493(backendID, model); !ok {
			continue
		}
		score := p.calculateScore(backendID)
		if score > bestScore {
			bestScore = score
			bestID = backendID
		}
	}
	if bestID != "" && logger.Get() != nil {
		logger.Get().Infow("R493: свободный узел выбран под загрузку модели",
			"backend", bestID, "model", model, "score", bestScore)
	}
	return bestID
}
