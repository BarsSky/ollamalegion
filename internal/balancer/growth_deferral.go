// growth_deferral.go — R664 (шаг A), исправление живого дефекта стенда
// `balancer:r261120-r679`: заявка клиента на большее окно исполняется ПОСЛЕ его
// хода, а не в preflight.
//
// ЖИВОЙ ФАКТ (двухузловой стенд, 2026-11-20). Запрос: 15 токенов промпта,
// `options.num_ctx=16384` при загруженных 8192 на слот, `stream=true`. Ответ 200,
// но первый байт — через 46.9 с, `load_duration=46.925 s`: модель выгрузилась и
// загрузилась ПОД ЭТИМ ЖЕ запросом. Лог при этом утверждал обратное —
// «проактивная заявка клиента — поднимаем окно в фоне, запрос обслуживается
// сейчас».
//
// ПОЧЕМУ ТАК ВЫХОДИЛО. Решение «расти в фоне» принимал preflight
// (growth_stream_guard.go → triggerBackgroundGrowthR664) и проверял только
// `active_streams`/`reload_pending`. Текущий запрос стримом ещё НЕ учтён:
// beginClientStreamR271 стоит в транспорте, после admission и после ответа
// апстрима. Счётчик был 0, `deferDestructiveOpR291` видел 0 — unload+load шёл
// немедленно, и клиент платил за него своим первым байтом.
//
// ЧТО ЗДЕСЬ.
//
//  1. preflight заявку только СТАВИТ В ОЧЕРЕДЬ (deferGrowthAfterTurnR664);
//  2. очередь разбирается на ЗАВЕРШЕНИИ хода клиента (beginClientTurnR664 /
//     drainDeferredGrowthR664) — когда на бэкенде нет ни одного хода;
//  3. занятость проверяется теми же growthBlockersR288, что и раньше, плюс
//     учёт «ход клиента идёт» (turns): стрим регистрируется ПОЗЖЕ решения, и
//     одного счётчика стримов для этого окна не хватало — ровно из-за этого
//     дефект и был живым.
//
// ПОЧЕМУ БЕЗ ТАЙМЕРА И ФОНОВОГО ЦИКЛА. Рост нужен МЕЖДУ ходами: следующий ход
// уже должен получить новое окно. «Когда-нибудь по таймеру» — это то же
// обещание без срока, а фоновый цикл пришлось бы останавливать вместе с
// процессом. Разбор очереди в точке «ход закончился» — единственный момент, про
// который известно, что модель свободна.
//
// ЧЕГО ЭТО НЕ МЕНЯЕТ. Немедленный рост (growthRequiredR664: данные не влезают и
// клиент заявил покрывающее окно) остаётся немедленным — запрос всё равно не
// обслуживается, ждать ему нечего. Здесь только «проактивная» заявка, данные
// которой влезают в текущее окно.
package balancer

import (
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// growthRunR664 — исход попытки исполнить отложенный рост.
type growthRunR664 int

const (
	// growthDeferredR664 — сейчас нельзя (идёт чужой стрим или перезагрузка):
	// заявку надо оставить в очереди и попробовать на следующем завершении хода.
	growthDeferredR664 growthRunR664 = iota
	// growthStartedR664 — перезагрузка запущена, цель заявки исполняется.
	growthStartedR664
	// growthNotNeededR664 — заявка больше не нужна (цель уже покрыта загруженным
	// окном, рост выключен оператором или модель пропала): из очереди её убираем.
	growthNotNeededR664
)

// queuedGrowthR664 — одна отложенная заявка на рост окна.
type queuedGrowthR664 struct {
	backendID string
	model     string
	// target — окно НА КЛИЕНТА: РОВНО заявленное клиентом (R664), не оценка
	// промпта и не профиль. Иначе окно уползало бы к числу, которого клиент не
	// просил.
	target int
	// streaming — заявка пришла от стримингового запроса. Нужна, чтобы
	// выключатель LB_NCTX_GROW_FOR_STREAM (R385/R472) действовал и на
	// отложенное исполнение: оператор выключил рост на стриминге — значит его
	// нет ни в preflight, ни после хода.
	streaming bool
	// since — когда заявка поставлена (для лога: «сколько ход уже длится»).
	since time.Time
}

// growthDeferralR664 — очередь отложенного роста и учёт идущих ходов клиента.
//
// Отдельная структура со своим мьютексом, а не поля Proxy под p.mu: ход
// начинается и заканчивается на горячем пути, а p.mu держат селекторы и реестр
// бэкендов. Нулевое значение пригодно к использованию (карты создаются лениво):
// Proxy в тестах собирается как &Proxy{...} и не проходит через NewProxy.
type growthDeferralR664 struct {
	mu sync.Mutex
	// pending — заявки по ключу backend+model (как у учёта стримов R271).
	pending map[string]queuedGrowthR664
	// turns — сколько ходов клиента идёт на бэкенде ПРЯМО СЕЙЧАС.
	turns map[string]int
	// draining — по бэкенду уже разбирается очередь (второй разбор не нужен:
	// два горутина на одну модель запустили бы две перезагрузки).
	draining map[string]bool
}

// deferGrowthAfterTurnR664 — поставить заявку в очередь отложенного роста.
//
// Возвращает true, если заявка в очереди (и рост будет исполнен после хода).
// НИЧЕГО не перезагружает и не ждёт: это и есть весь смысл правки — preflight
// больше не запускает unload+load под обслуживаемым запросом.
func (p *Proxy) deferGrowthAfterTurnR664(e queuedGrowthR664) bool {
	if p == nil || e.backendID == "" || e.model == "" || e.target <= 0 {
		return false
	}
	if e.since.IsZero() {
		e.since = time.Now()
	}
	q := &p.growthDeferralR664
	key := streamRefKeyR271(e.backendID, e.model)
	q.mu.Lock()
	if q.pending == nil {
		q.pending = make(map[string]queuedGrowthR664, 4)
	}
	prev, existed := q.pending[key]
	if existed && prev.target > e.target {
		// R294/R448: окно не уменьшаем. Более ранняя заявка просила больше —
		// цель очереди не понижается, иначе «рост» стал бы «сужением».
		e.target = prev.target
	}
	q.pending[key] = e
	queued := len(q.pending)
	q.mu.Unlock()

	logger.Get().Infow("R664: заявка клиента на большее окно отложена до конца хода клиента",
		"backend", e.backendID, "model", e.model,
		"requested_n_ctx", e.target, "streaming", e.streaming,
		"already_queued", existed, "queue_len", queued,
		"effect", "текущий ход не ждёт перезагрузки; окно поднимается между ходами")
	return true
}

// beginClientTurnR664 — учесть начало хода клиента на бэкенде.
//
// Возвращает функцию завершения: она снимает учёт и разбирает очередь
// отложенного роста (асинхронно — ответ клиенту не задерживается). Идемпотентна
// и безопасна при p == nil (тесты, «пустой» прокси).
//
// ЗАЧЕМ УЧЁТ ХОДОВ, ЕСЛИ ЕСТЬ УЧЁТ СТРИМОВ. Стрим регистрируется в транспорте
// ПОСЛЕ ответа апстрима, а preflight — до; между ними есть окно, в котором
// «активных стримов 0», хотя ход уже идёт. Именно в это окно живой дефект
// r679 и запускал перезагрузку. Учёт ходов начинается ДО preflight — то есть
// покрывает ровно тот промежуток.
func (p *Proxy) beginClientTurnR664(backendID string) func() {
	if p == nil || backendID == "" {
		return func() {}
	}
	q := &p.growthDeferralR664
	q.mu.Lock()
	if q.turns == nil {
		q.turns = make(map[string]int, 4)
	}
	q.turns[backendID]++
	q.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			q.mu.Lock()
			if q.turns[backendID] > 0 {
				q.turns[backendID]--
				if q.turns[backendID] == 0 {
					delete(q.turns, backendID)
				}
			}
			q.mu.Unlock()
			// Ход закончился — момент, когда модель свободна. Рост запускаем,
			// не задерживая ответ: у клиента стрим уже закрыт.
			p.scheduleDeferredGrowthDrainR664(backendID)
		})
	}
}

// turnsInFlightR664 — сколько ходов клиента идёт на бэкенде.
func (p *Proxy) turnsInFlightR664(backendID string) int {
	if p == nil || backendID == "" {
		return 0
	}
	q := &p.growthDeferralR664
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.turns[backendID]
}

// pendingGrowthTargetR664 — окно, до которого рост отложен для этой модели
// (0 — заявки нет). Нужно логу и проверкам («заявка не потерялась»).
func (p *Proxy) pendingGrowthTargetR664(backendID, model string) int {
	if p == nil || backendID == "" {
		return 0
	}
	q := &p.growthDeferralR664
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.pending[streamRefKeyR271(backendID, model)].target
}

// deferredGrowthCountR664 — сколько заявок ждёт исполнения на бэкенде.
func (p *Proxy) deferredGrowthCountR664(backendID string) int {
	if p == nil || backendID == "" {
		return 0
	}
	q := &p.growthDeferralR664
	q.mu.Lock()
	defer q.mu.Unlock()
	n := 0
	for _, e := range q.pending {
		if e.backendID == backendID {
			n++
		}
	}
	return n
}

// scheduleDeferredGrowthDrainR664 — запустить разбор очереди В ФОНЕ.
//
// ПОЧЕМУ ГОРУТИН, А НЕ ПРЯМОЙ ВЫЗОВ. executeAsyncReload — это HTTP-вызов
// воркеру. Даже при ответе 202 он занимает время, а в худшем случае воркер
// ответит только после загрузки модели (десятки секунд). Держать столько
// клиентское соединение после последнего байта ответа нельзя: клиент ждёт
// закрытия потока.
func (p *Proxy) scheduleDeferredGrowthDrainR664(backendID string) {
	if p == nil || backendID == "" {
		return
	}
	if p.deferredGrowthCountR664(backendID) == 0 {
		return
	}
	go p.drainDeferredGrowthR664(backendID)
}

// drainDeferredGrowthR664 — исполнить отложенный рост, если бэкенд свободен.
//
// Условия те же, что были у triggerBackgroundGrowthR664, плюс учёт ходов:
//   - на бэкенде не идёт ни один ход клиента (turns == 0);
//   - ни одного активного стрима и ни одной идущей перезагрузки (R271/R288);
//   - цель заявки всё ещё больше загруженного окна (иначе рост не нужен).
//
// Не исполнилось — заявка остаётся в очереди и будет повторена на следующем
// завершении хода. Исполняется ОДНА заявка за разбор: перезагрузки на одном
// воркере не должны идти пачкой (следующая заявка увидит reload_pending).
func (p *Proxy) drainDeferredGrowthR664(backendID string) {
	if p == nil || backendID == "" {
		return
	}
	q := &p.growthDeferralR664
	q.mu.Lock()
	if q.draining == nil {
		q.draining = make(map[string]bool, 4)
	}
	if q.draining[backendID] {
		q.mu.Unlock()
		return
	}
	q.draining[backendID] = true
	q.mu.Unlock()
	defer func() {
		q.mu.Lock()
		delete(q.draining, backendID)
		q.mu.Unlock()
	}()

	if n := p.turnsInFlightR664(backendID); n > 0 {
		logger.Get().Infow("R664: отложенный рост ждёт завершения хода клиента",
			"backend", backendID, "turns_in_flight", n)
		return
	}
	entry, ok := p.takeDeferredGrowthR664(backendID)
	if !ok {
		return
	}
	if active, pending := growthBlockersR288(p, entry.backendID, entry.model); active > 0 || pending {
		logger.Get().Infow("R664: отложенный рост ждёт — на узле идёт стрим или перезагрузка (R271/R288)",
			"backend", entry.backendID, "model", entry.model,
			"active_streams", active, "reload_pending", pending,
			"requested_n_ctx", entry.target)
		p.requeueDeferredGrowthR664(entry)
		return
	}
	if outcome := p.triggerBackgroundGrowthR664(entry); outcome == growthDeferredR664 {
		p.requeueDeferredGrowthR664(entry)
	}
}

// takeDeferredGrowthR664 — забрать одну заявку на бэкенде (порядок не важен:
// ключ — модель, а на одном воркере обычно одна модель).
func (p *Proxy) takeDeferredGrowthR664(backendID string) (queuedGrowthR664, bool) {
	q := &p.growthDeferralR664
	q.mu.Lock()
	defer q.mu.Unlock()
	for key, e := range q.pending {
		if e.backendID != backendID {
			continue
		}
		delete(q.pending, key)
		return e, true
	}
	return queuedGrowthR664{}, false
}

// requeueDeferredGrowthR664 — вернуть заявку в очередь (рост отложен занятостью).
func (p *Proxy) requeueDeferredGrowthR664(e queuedGrowthR664) {
	q := &p.growthDeferralR664
	key := streamRefKeyR271(e.backendID, e.model)
	q.mu.Lock()
	if q.pending == nil {
		q.pending = make(map[string]queuedGrowthR664, 4)
	}
	if prev, ok := q.pending[key]; ok && prev.target > e.target {
		e.target = prev.target // R294: цель не понижаем
	}
	q.pending[key] = e
	q.mu.Unlock()
}
