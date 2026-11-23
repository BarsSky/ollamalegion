// gate_notices.go — R700 (2026-11-24): сработавший гейт — это УВЕДОМЛЕНИЕ с
// числами, а не только строка в логе и запись в теле 503.
//
// ЖИВАЯ ЖАЛОБА (второй стенд, A10 ×2, 50 ГБ, Qwen3.8-27B). Оператор видел, что
// заявка стоит в очереди, открывал монитор и не находил причины: диск, RAM,
// VRAM, окно и слоты отказывали МОЛЧА во всех местах выбора. Причины были
// известны (`why_queued` в логе и `backend_rejections` в теле 503), но:
//   - колокольчик WebUI их не показывал вовсе — там жили только провалы
//     загрузки (load_failure_events.go) и обрывы стрима;
//   - на страницах «Очередь»/«Монитор» причины не было ни строчкой;
//   - чтобы увидеть число «9480 МБ < 10240 МБ», надо было идти в лог, то есть
//     дефект обнаруживался только тем, у кого есть доступ к docker logs.
//
// ПРАВИЛО: у каждого гейта (диск → RAM → VRAM → окно → слоты) есть ОДНО
// уведомление на узел, с числами и действием. Оно же попадает в тело 503
// (`gate_notices`) и в снимок причин для страниц (`queueStats.gates`).
//
// ДЕДУПЛИКАЦИЯ ОБЯЗАТЕЛЬНА, и по той же причине, что в load_failure_events.go:
// список причин строится на КАЖДОМ заходе в очередь, а очередь переспрашивает
// условия шесть раз в секунду (admissionPollInterval). Без дедупа один
// заблокированный узел дал бы сотни одинаковых событий в час, SSE ring хранит
// 100 событий (internal/api/handlers_events.go) — полезная история была бы
// вытеснена, а оператор привык бы закрывать колокольчик не читая.
//
// СЕМАНТИКА ДЕДУПА — «эпизод»: ключ (гейт, узел), внутри которого сообщение
// публикуется ОДИН раз и повторно только если изменился текст (например,
// свободного места стало ещё меньше). Наблюдения схлопываются в счётчик
// Count («×N»), сам эпизод живёт 10 минут без наблюдений — после этого гейт
// считается исчезнувшим и следующий случай публикуется заново.
package balancer

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// Коды гейтов. Стабильны: по ним WebUI выбирает подпись строки и действие, а
// тесты — ожидаемый текст. Отдельные от кодов причин (backend_rejections.go):
// одна причина может быть одним гейтом, но у гейта есть человеческий слой
// (заголовок, действие), которого у машинного кода нет.
const (
	gateKindDiskR700   = "disk"
	gateKindRAMR700    = "ram"
	gateKindVRAMR700   = "vram"
	gateKindWindowR700 = "window"
	gateKindSlotsR700  = "slots"
)

// GateDiskR700 — экспортированный код дискового гейта для api-слоя.
//
// ЗАЧЕМ: PUT /api/v1/cluster/config, меняя minFreeDisk, обязан СНЯТЬ уже
// показанную причину по диску (ClearGateNoticesR700) — иначе страница и баннер
// ещё 10 минут рассказывают об исключённом узле, который по новому порогу
// проходит. Строковый литерал в двух пакетах однажды разошёлся бы; поэтому имя
// гейта живёт здесь одно.
const GateDiskR700 = gateKindDiskR700

// gateNoticeTTLR700 — сколько живёт эпизод без новых наблюдений.
//
// 10 минут — не «красивое число», а время, за которое живой стенд успевает
// показать и ожидание, и его исчезновение: дольше держать запись значило бы
// показывать в снимке причину, которой уже нет (тот же период выбран для
// провала загрузки — loadFailureTTL).
const gateNoticeTTLR700 = 10 * time.Minute

// gateNumbersR700 — числа сработавшего гейта. Одна структура на все гейты, а
// не пять разных: потребителей два (текст уведомления и JSON для страницы), и
// им нужен одинаковый набор полей «что сравнивали с чем».
//
// Порядок полей — под fieldalignment: сначала 8-байтные, затем 4-байтные.
type gateNumbersR700 struct {
	FreeMB    uint64  `json:"free_mb,omitempty"`
	MinFreeMB uint64  `json:"min_free_mb,omitempty"`
	NeedMB    uint64  `json:"need_mb,omitempty"`
	Percent   float64 `json:"percent,omitempty"`
	Threshold float64 `json:"threshold,omitempty"`
	// Окно: Have — что есть у кандидата, Need — что требуется живой сессии.
	WindowHave int `json:"window_have,omitempty"`
	WindowNeed int `json:"window_need,omitempty"`
	// Слоты: занято/вместимость (эффективная, R493).
	SlotsActive int `json:"slots_active,omitempty"`
	SlotsMax    int `json:"slots_max,omitempty"`
}

// gateNoticeR700 — одна причина, готовая и для колокольчика, и для страницы.
type gateNoticeR700 struct {
	Numbers   *gateNumbersR700 `json:"numbers,omitempty"`
	FirstSeen time.Time        `json:"first_seen"`
	LastSeen  time.Time        `json:"last_seen"`
	Gate      string           `json:"gate"`
	BackendID string           `json:"backend_id,omitempty"`
	Model     string           `json:"model,omitempty"`
	Severity  string           `json:"severity"`
	Message   string           `json:"message"`
	Detail    string           `json:"detail,omitempty"`
	Action    string           `json:"action,omitempty"`
	// Count — сколько наблюдений схлопнуто в это уведомление («×N»).
	Count int `json:"count"`
}

// gateNoticeStateR700 — состояние эпизода в Proxy (ключ — гейт + узел).
//
// sig — текст уведомления на момент публикации. Сравнивается с новым: пока
// числа не меняются, событие не повторяется.
type gateNoticeStateR700 struct {
	firstSeen time.Time
	lastSeen  time.Time
	sig       string
	notice    gateNoticeR700
}

// gateKindForReasonR700 — код причины R460 → код гейта.
//
// ВТОРОЕ ЗНАЧЕНИЕ — «показывать ли это оператору как причину ОЖИДАНИЯ».
// model_not_loaded, file_absent, disabled_by_operator и прочие сюда не входят:
// это не гейты, а состояния, и у них своя аудитория (страница «Бэкенды»,
// уведомления о загрузке). Смешивать их значило бы вернуть шум, из-за которого
// и затевался R460.
func gateKindForReasonR700(reason string) (string, bool) {
	switch reason {
	case rejectDiskLowR460:
		return gateKindDiskR700, true
	case rejectRAMCriticalR460:
		return gateKindRAMR700, true
	case rejectVRAMInsufficientR460, rejectWindowDoesNotFitR460:
		// «С окном N не влезает» — это нехватка VRAM, а не малое окно у готовой
		// копии: действие у него то же (освободить VRAM / уменьшить n_ctx).
		return gateKindVRAMR700, true
	case rejectWindowTooSmallR460, rejectWindowFitUnknownR460:
		return gateKindWindowR700, true
	case rejectSlotsBusyR460, rejectOverLoadThresholdR460, rejectNoSlotsConfigR460:
		return gateKindSlotsR700, true
	}
	return "", false
}

// gatePriorityR700 — порядок гейтов в снимке и в тексте: сначала то, что
// оператор ЛЕЧИТ (диск, память), затем то, что ждёт само (окно, слоты).
func gatePriorityR700(gate string) int {
	switch gate {
	case gateKindDiskR700:
		return 10
	case gateKindRAMR700:
		return 20
	case gateKindVRAMR700:
		return 30
	case gateKindWindowR700:
		return 40
	case gateKindSlotsR700:
		return 50
	}
	return 90
}

// gateSeverityR700 — цвет уведомления. Диск и RAM — warning/critical: это
// реальные условия узла, и часть из них (порог диска) регулируется настройкой.
// Слоты — info: «все слоты заняты» штатно и рассасывается само, пугать
// оператора красным в нормальной очереди нельзя.
func gateSeverityR700(gate string) types.EventSeverity {
	switch gate {
	case gateKindDiskR700:
		return types.SeverityWarning
	case gateKindRAMR700:
		return types.SeverityCritical
	case gateKindVRAMR700, gateKindWindowR700:
		return types.SeverityWarning
	case gateKindSlotsR700:
		return types.SeverityInfo
	}
	return types.SeverityWarning
}

// gateActionR700 — что именно сделать. Живёт рядом с текстом, потому что
// уведомление без действия оператор читает как «сломалось».
func gateActionR700(gate string) string {
	switch gate {
	case gateKindDiskR700:
		return "освободите место на узле или снизьте порог «Min Free Disk» в «Настройках → Лимиты ресурсов»"
	case gateKindRAMR700:
		return "освободите память на узле или поднимите «RAM Max Usage» в «Настройках → Лимиты ресурсов»"
	case gateKindVRAMR700:
		return "уменьшите n_ctx/квант модели или освободите VRAM на узле"
	case gateKindWindowR700:
		return "заявка ждёт слот своей копии: копию с меньшим окном под живую сессию не подставляем (R459)"
	case gateKindSlotsR700:
		return "ждём освобождения слота — отдельное действие не требуется"
	}
	return ""
}

// gateNoticeMessageR700 — человеческая фраза с числами.
//
// ЧИСЛА БЕРУТСЯ ИЗ gateNumbersR700, а не выдёргиваются из Detail: Detail —
// диагностическая строка для лога (key=value), и её формат менялся. Если чисел
// нет (гейт сработал по неизвестной метрике), честно отдаём Detail — врать
// «0 МБ» вместо «неизвестно» нельзя.
func gateNoticeMessageR700(gate, backendID, model string, nums *gateNumbersR700, detail string) string {
	node := backendID
	if node == "" {
		node = "узел"
	}
	switch gate {
	case gateKindDiskR700:
		if nums != nil && nums.MinFreeMB > 0 {
			return fmt.Sprintf(
				"Запрос ждёт: свободный узел %s заблокирован по диску (свободно %d МБ, минимум %d МБ) — освободите место или снизьте порог в настройках",
				node, nums.FreeMB, nums.MinFreeMB)
		}
	case gateKindRAMR700:
		if nums != nil && nums.Threshold > 0 {
			return fmt.Sprintf(
				"Запрос ждёт: узел %s исключён из выбора — RAM занята %.0f%% (порог %.0f%%): %s",
				node, nums.Percent, nums.Threshold, gateActionR700(gate))
		}
	case gateKindVRAMR700:
		if nums != nil && nums.NeedMB > 0 {
			target := "модель"
			if model != "" {
				target = "модель " + model
			}
			return fmt.Sprintf(
				"Узел %s пропущен: %s не влезает в свободную VRAM (нужно %d МБ, свободно %d МБ) — %s",
				node, target, nums.NeedMB, nums.FreeMB, gateActionR700(gate))
		}
	case gateKindWindowR700:
		if nums != nil && nums.WindowNeed > 0 && nums.WindowHave > 0 {
			return fmt.Sprintf(
				"Ожидание: окно свободной копии на узле %s меньше заявленного (%d < %d) — %s",
				node, nums.WindowHave, nums.WindowNeed, gateActionR700(gate))
		}
	case gateKindSlotsR700:
		if nums != nil && nums.SlotsMax > 0 {
			return fmt.Sprintf(
				"Ожидание: заняты все слоты узла %s (%d/%d) — %s",
				node, nums.SlotsActive, nums.SlotsMax, gateActionR700(gate))
		}
	}
	// Нет чисел — причина словами из списка отказов (там числа тоже есть, но в
	// формате лога). Пустой Detail не оставляем: «гейт сработал» без причины
	// хуже, чем отсутствие уведомления.
	if detail != "" {
		return fmt.Sprintf("Запрос ждёт: узел %s — %s", node, detail)
	}
	return fmt.Sprintf("Запрос ждёт: узел %s не прошёл гейт (%s)", node, gate)
}

// noteGateRejectionsR700 — превратить список отказов в уведомления (с дедупом)
// и вернуть СНИМОК всех активных причин для страниц и тела 503.
//
// Вызывается из мест, где заявка встаёт в очередь (unified_queue.go,
// unified_queue_gate.go), и из ответа 503 (admission_queue.go). Возвращает
// снимок даже когда публиковать нечего: страница должна видеть причину, пока
// она жива, а не только в момент первого события.
func (p *Proxy) noteGateRejectionsR700(model string, rejections []backendRejectionR460) []gateNoticeR700 {
	if p == nil {
		return nil
	}
	now := time.Now()

	var publish []gateNoticeR700
	seen := make(map[string]bool, len(rejections))
	p.gatesMu.Lock()
	if p.gatesSeen == nil {
		p.gatesSeen = make(map[string]*gateNoticeStateR700)
	}
	for _, r := range rejections {
		gate, ok := gateKindForReasonR700(r.Reason)
		if !ok {
			continue
		}
		key := gate + "|" + r.BackendID
		if seen[key] {
			// Один узел может отказать по нескольким причинам одного гейта
			// (например, model_loading и disk_low) — оператору нужна одна
			// строка на узел, поэтому берём ПЕРВУЮ по порядку отказов.
			continue
		}
		seen[key] = true
		msg := gateNoticeMessageR700(gate, r.BackendID, model, r.Numbers, r.Detail)
		st := p.gatesSeen[key]
		// Эпизод истёк — это НОВЫЙ случай, а не продолжение: иначе после
		// ночного затишья оператор не увидел бы вернувшуюся проблему.
		if st == nil || now.Sub(st.lastSeen) > gateNoticeTTLR700 {
			n := gateNoticeR700{
				Numbers:   r.Numbers,
				FirstSeen: now,
				LastSeen:  now,
				Gate:      gate,
				BackendID: r.BackendID,
				Model:     model,
				Severity:  string(gateSeverityR700(gate)),
				Message:   msg,
				Detail:    r.Detail,
				Action:    gateActionR700(gate),
				Count:     1,
			}
			p.gatesSeen[key] = &gateNoticeStateR700{firstSeen: now, lastSeen: now, sig: msg, notice: n}
			publish = append(publish, n)
			continue
		}
		st.lastSeen = now
		st.notice.Count++
		st.notice.LastSeen = now
		if st.sig != msg {
			// Числа изменились (места стало меньше, окно поднялось) — это
			// новое состояние, о нём надо сообщить: оператор принимает
			// решения по числам, а не по факту «что-то не так».
			st.sig = msg
			st.notice.Message = msg
			st.notice.Detail = r.Detail
			st.notice.Numbers = r.Numbers
			st.notice.Model = model
			publish = append(publish, st.notice)
		}
	}
	// Наблюдение — это снимок «здесь и сейчас» для ЭТОЙ модели: если гейт в нём
	// больше не сработал, причина исчезла и её нельзя держать на странице до
	// истечения TTL. Живой случай: оператор снизил порог диска — узел снова
	// проходит, а баннер «исключён по диску» висел бы ещё 10 минут и выглядел
	// как «настройка не применилась». Записи ДРУГИХ моделей не трогаем: их
	// наблюдения к этому проходу не относятся.
	p.dropUnobservedGatesLocked(model, seen)
	p.pruneGatesLocked(now)
	p.gatesMu.Unlock()

	// Публикация — ВНЕ мьютекса: EventBus.Publish не блокирует, но держать
	// замок на время рассылки подписчикам не нужно, а порядок блокировок в
	// проекте проверяется отдельно (lockorder_r284_test.go).
	for _, n := range publish {
		p.publishGateNoticeR700(n)
	}
	return p.GateNoticesR700()
}

// GateNoticesR700 — снимок активных причин ожидания (для API и тестов).
//
// Экспортирован, потому что api-слой кладёт его в /api/v1/queue/stats
// (queue_manager.go: QueueStats.Gates) и в diag-summary. Порядок — по
// приоритету гейта, затем по узлу: страница рисует сверху то, что лечится.
func (p *Proxy) GateNoticesR700() []gateNoticeR700 {
	if p == nil {
		return nil
	}
	now := time.Now()
	p.gatesMu.Lock()
	defer p.gatesMu.Unlock()
	p.pruneGatesLocked(now)
	if len(p.gatesSeen) == 0 {
		return nil
	}
	out := make([]gateNoticeR700, 0, len(p.gatesSeen))
	for _, st := range p.gatesSeen {
		out = append(out, st.notice)
	}
	sort.Slice(out, func(i, j int) bool {
		pi, pj := gatePriorityR700(out[i].Gate), gatePriorityR700(out[j].Gate)
		if pi != pj {
			return pi < pj
		}
		return out[i].BackendID < out[j].BackendID
	})
	return out
}

// ClearGateNoticesR700 — снять активные причины указанного гейта.
//
// ЗАЧЕМ. Оператор меняет настройку, которая И ЕСТЬ причина (порог диска), и
// ждёт, что страница и баннер это отразят. Без снятия запись жила бы до TTL
// (10 минут) или до следующего захода в очередь: на странице «Очередь» висел бы
// баннер «узел исключён по диску» сразу после сохранения нового порога — то
// есть выглядело бы как «настройка не применилась». Живой сценарий проверки:
// minFreeDisk 10240 → 9000 при свободных 9480 МБ.
//
// Уведомление о снятии НЕ публикуется: «причина исчезла» — не инцидент, а
// следствие действия самого оператора (он видит тост «Настройки сохранены»).
// Если гейт всё ещё срабатывает по другой причине/узлу, следующий проход
// очереди создаст уведомление заново.
func (p *Proxy) ClearGateNoticesR700(gate string) {
	if p == nil || gate == "" {
		return
	}
	p.gatesMu.Lock()
	defer p.gatesMu.Unlock()
	for key, st := range p.gatesSeen {
		if st.notice.Gate == gate {
			delete(p.gatesSeen, key)
		}
	}
}

// pruneGatesLocked — убрать истёкшие эпизоды. Вызывается под p.gatesMu.
func (p *Proxy) pruneGatesLocked(now time.Time) {
	for key, st := range p.gatesSeen {
		if now.Sub(st.lastSeen) > gateNoticeTTLR700 {
			delete(p.gatesSeen, key)
		}
	}
}

// dropUnobservedGatesLocked — убрать причины ЭТОЙ модели, переставшие
// срабатывать. Вызывается под p.gatesMu.
//
// Почему по модели, а не «всё, чего нет в списке»: проход строится для одной
// заявки, и гейты другой модели в нём не участвуют. Удалять их значило бы
// гасить баннер чужой заявки при каждом запросе соседа.
func (p *Proxy) dropUnobservedGatesLocked(model string, seen map[string]bool) {
	for key, st := range p.gatesSeen {
		if st.notice.Model != model {
			continue
		}
		if seen[key] {
			continue
		}
		delete(p.gatesSeen, key)
	}
}

// publishGateNoticeR700 — низкоуровневая публикация (nil-safe по eventBus).
//
// Source="gate" — чтобы WebUI и логи отличали причину ожидания от провала
// загрузки (source="load") и от обрыва стрима. Data повторяет поля, нужные
// колокольчику и странице: gate/backend/numbers/action/count.
func (p *Proxy) publishGateNoticeR700(n gateNoticeR700) {
	if p == nil || p.eventBus == nil {
		return
	}
	data := map[string]interface{}{
		"event_kind": "gate_notice",
		"gate":       n.Gate,
		"backend_id": n.BackendID,
		"action":     n.Action,
		"count":      n.Count,
	}
	if n.Numbers != nil {
		data["numbers"] = n.Numbers
	}
	p.eventBus.Publish(types.Event{
		Type:      types.EventNotification,
		Timestamp: time.Now(),
		BackendID: n.BackendID,
		Model:     n.Model,
		Severity:  types.EventSeverity(n.Severity),
		Source:    "gate",
		Message:   n.Message,
		Data:      data,
	})
	logger.Get().Infow("gate notice published",
		"gate", n.Gate, "backend", n.BackendID, "model", n.Model,
		"severity", n.Severity, "message", n.Message)
}

// gateNoticesSummaryR700 — одна строка для тела 503 и лога:
// «disk_low(A10-126: …); window(B: …)».
func gateNoticesSummaryR700(list []gateNoticeR700) string {
	if len(list) == 0 {
		return ""
	}
	parts := make([]string, 0, len(list))
	for _, n := range list {
		parts = append(parts, n.Message)
	}
	return strings.Join(parts, " | ")
}

// queueGateStatsR700 — блок gates в /api/v1/queue/stats.
//
// Отдельная структура (а не []gateNoticeR700 напрямую), потому что рядом с
// причинами странице нужен предел ожидания и порог диска — иначе она
// показывает причину без контекста «сколько ещё ждать».
type queueGateStatsR700 struct {
	Notices []gateNoticeR700 `json:"notices"`
	// MinFreeDiskMB — действующий порог диска (resources.disk.minFreeMB).
	// Нужен на странице «Настройки» и в снимке причин: оператор должен видеть,
	// что именно он может изменить, а не только «мало места».
	MinFreeDiskMB uint64 `json:"min_free_disk_mb"`
	WaitMaxSec    int    `json:"wait_max_sec"`
}

// queueGateStatsR700 — снимок для /queue/stats. nil — причин нет (нулевой блок
// только путал бы; так же сделано для image и backend_wait).
func (p *Proxy) queueGateStatsR700() *queueGateStatsR700 {
	if p == nil {
		return nil
	}
	notices := p.GateNoticesR700()
	if len(notices) == 0 {
		return nil
	}
	out := &queueGateStatsR700{Notices: notices}
	if p.config != nil {
		out.MinFreeDiskMB = p.config.Resources.Disk.MinFreeMB
	}
	out.WaitMaxSec = int(p.admissionWaitBudget().Seconds())
	return out
}
