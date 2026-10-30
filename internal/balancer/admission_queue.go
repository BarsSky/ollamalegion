// admission_queue.go — R67b (2026-09-23): очередь допуска (admission) для
// inference-запросов llama.cpp вместо немедленного 503.
//
// ЖАЛОБА (R67, дословно): «проблема на стороне балансера, он резко отбивает
// повторный запрос от клиента выдавая 503, а не поставляя в очередь, скорей
// всего причина в том что не различает разных пользователей клиента OpenWebUI,
// что может предоставлять модель сразу нескольким пользователям одновременно»
// и «даже если от одного пользователя идет новый запрос, балансер его не ставит
// в очередь на отработку и не делает как новую сессию».
//
// ДИАГНОЗ (подтверждён чтением кода + живым стендом). Inference-хендлеры
// llama.cpp (handleChat / handleGenerate / handleOpenAIChatCompletions) выбирают
// бэкенд и сразу проксируют запрос, НЕ беря слот: `tryAcquireSlot`
// (slot_manager.go) вызывается только из общего flow Proxy.ServeHTTP и из
// QueueManager. Поэтому:
//   - `maxConcurrentReqs` бэкенда (в живом стеке 4) на llama.cpp-путь не влиял
//     вообще: `activeRequests` в /api/v1/backends оставался 0 при трёх
//     параллельных генерациях, а WebUI показывал нулевую загрузку бэкенда;
//   - всё, что не помещалось в параллелизм cppworker'а, ждало ВНУТРИ cppworker
//     (его SlotManager), а балансер при этом не имел ни очереди, ни видимости
//     ожидающих; при таймауте ожидания клиент получал 503 «model is still
//     loading» / «all backends busy» без позиции в очереди.
//
// РЕШЕНИЕ: admission-очередь на стороне балансера.
//  1. Быстрый путь: слот свободен → берём его (`tryAcquireSlot`), как раньше.
//  2. Слот занят → запрос ВСТАЁТ В ОЧЕРЕДЬ и ждёт до LB_ADMISSION_WAIT_SEC
//     (default 300 c; 0 = прежнее поведение «сразу 503»), после чего получает
//     слот и обслуживается ПЕРВЫМ же запросом (без «повторите запрос»).
//  3. Справедливость по сессиям (per-user): пока у сессии есть активный запрос,
//     её следующие ожидающие запросы УСТУПАЮТ сессиям, которые ещё не
//     обслуживались. Один пользователь OpenWebUI не может занять все слоты
//     своими повторными запросами.
//  4. Каждый НОВЫЙ запрос пользователя — новая сессия/новая позиция в очереди
//     (см. requestSessionKey: X-User-Id / X-OpenWebUI-User-Id / chat_id из тела
//     и metadata), а не «слипание» всех пользователей в одну сессию.
//  5. 503 остаётся последним рубежом: только если ожидание истекло. Клиент
//     получает Retry-After + X-Queue-Position + X-Queue-Wait-Ms.
package balancer

import (
	"context"
	"errors"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/internal/settings"
	"ollama-loadbalancer/pkg/logger"
)

const (
	// admissionWaitDefaultSec — сколько ждать слот перед 503 (10 минут).
	// Достаточно, чтобы пережить длинную генерацию другого пользователя, и
	// меньше типичного таймаута OpenWebUI/Cline на «висит соединение».
	admissionWaitDefaultSec = 300

	// admissionPollInterval — период перепроверки слота ожидающим. Нужен как
	// страховка от потерянного пробуждения (broadcast) — основной сигнал всё же
	// мгновенный.
	admissionPollInterval = 250 * time.Millisecond

	// admissionRetryAfterSec — Retry-After для 503 при исчерпанном ожидании.
	admissionRetryAfterSec = 15

	// admissionFairnessWindow — окно «недавно обслуживался»: сессия, которая
	// получила слот в этом окне, уступает сессиям, которые ещё не обслуживались
	// (защита от «один пользователь забил очередь своими запросами»). Окно
	// конечно: через него пользователь снова считается «новым», иначе один
	// ранний запрос навсегда понижал бы его приоритет.
	admissionFairnessWindow = 30 * time.Second
)

// AdmissionWaitTimeout — R67b: сколько балансер ждёт свободный слот, прежде чем
// ответить 503.
//
//	LB_ADMISSION_WAIT_SEC=N — N секунд; 0 = не ждать (прежнее поведение);
//	отрицательное = ждать до отмены клиентом (ограничено разумным максимумом).
//	По умолчанию 300s.
//
// R278: значение из реестра настроек (override из WebUI → env → дефолт), то есть
// меняется без рестарта. Трактовка «отрицательное = до отмены, но не дольше
// 30 минут» оставлена здесь, а не в реестре: это смысл флага, а не валидация.
func AdmissionWaitTimeout() time.Duration {
	v, ok := settings.HotInt("LB_ADMISSION_WAIT_SEC")
	if !ok {
		return admissionWaitDefaultSec * time.Second
	}
	switch {
	case v == 0:
		return 0
	case v < 0:
		return 30 * time.Minute
	default:
		return time.Duration(v) * time.Second
	}
}

var (
	// errAdmissionDisabled — ожидание выключено (LB_ADMISSION_WAIT_SEC=0) и
	// свободных слотов нет: вызывающий код отвечает прежним быстрым 503.
	errAdmissionDisabled = errors.New("admission queue disabled")
	// errAdmissionTimeout — ожидание истекло: 503 + Retry-After.
	errAdmissionTimeout = errors.New("no free slot within admission wait")
)

// admissionWaitBudget — R117 (2026-10-14): сколько запрос реально ждёт слот.
//
// Это НЕ то же самое, что LB_ADMISSION_WAIT_SEC: ожидание в очереди не имеет
// смысла быть короче ожидания авто-загрузки, за которой этот запрос стоит.
//
// ЖИВОЙ ЗАМЕР (стенд: LB_ADMISSION_WAIT_SEC по умолчанию 300,
// LB_AUTO_LOAD_WAIT_SEC=900). Два запроса к выгруженной модели: первый ушёл
// загружать модель (загрузка заняла ~345 с), второй встал в admission-очередь и
// получил 503 «waited in queue and timed out» ровно через 300 с — за 45 с ДО
// готовности модели, хотя был первым в очереди на освободившийся слот:
//
//	t+300s B_exit=True  model=loading  | 503 waitMaxSec=300
//	t+345s              model=loaded
//
// Правило: бюджет = max(LB_ADMISSION_WAIT_SEC, LB_AUTO_LOAD_WAIT_SEC). Оба
// значения читаются из env на каждом вызове; при выключенном ожидании слотов (0)
// бюджет тоже 0, то есть прежнее поведение «сразу 503» сохраняется.
func (p *Proxy) admissionWaitBudget() time.Duration {
	wait := p.admissionWaitTimeout()
	if wait <= 0 {
		return wait
	}
	if autoLoad := AutoLoadWaitTimeout(); autoLoad > wait {
		return autoLoad
	}
	return wait
}

// admissionWaiter — ожидающий слот запрос.
// Порядок полей — под fieldalignment (time.Time несёт указатель на Location).
type admissionWaiter struct {
	enqueued time.Time
	session  string
	seq      uint64
}

// Источники ключа сессии, который показывает очередь (R118, 2026-10-14).
//
// Ключ сессии порождается в client.go (getSessionIDWithModel / getClientFingerprint)
// и в RequestSessionKey (autoload_wait.go); здесь он только классифицируется —
// чтобы оператор видел, различает ли балансер двух клиентов с одной машины.
const (
	// sessionIdentityClient — ключ пришёл от клиента (заголовок, тело, cookie):
	// два разных запроса этого клиента получают РАЗНЫЕ ключи, если клиент их даёт.
	sessionIdentityClient = "client"
	// sessionIdentityDerived — ключ выведен балансером из окружения (IP+User-Agent
	// или общий токен). Два экземпляра клиента с одной машины получают ОДИН ключ —
	// именно этот случай оператор видит как «балансер не разделяет».
	sessionIdentityDerived = "derived"
	// sessionIdentityNone — идентификатора нет вовсе.
	sessionIdentityNone = "none"
)

// admissionWaiterInfo — один ожидающий запрос для UI (R91, 2026-10-08).
//
// ЗАЧЕМ. На странице «Очередь» показывалась ЛЕГАСИ-очередь
// (/api/v1/queue/details: current_size/processed_total), которая в этом
// развёртывании всегда пуста: реальное ожидание слота живёт в admission-очереди
// (LB_ADMISSION_WAIT_SEC). Оператор видел «Очередь пуста», «Выполнено 0» и
// «Среднее ожидание —» при том, что в admission-статистике было
// served_total=42, waited_total=42, avg_wait_ms=13926. То есть по странице нельзя
// было понять, работает ли очередь вообще. Здесь отдаём по каждому ожидающему
// то, что нужно таблице: кто ждёт, на каком бэкенде и сколько уже ждёт.
//
// R118: добавлены Position (позиция в очереди по порядку постановки — UI брал
// индекс строки), SessionSource и SessionKnown (различает ли балансер клиентов).
type admissionWaiterInfo struct {
	Session       string `json:"session"`
	SessionSource string `json:"session_source,omitempty"`
	Backend       string `json:"backend"`
	WaitedMs      int64  `json:"waited_ms"`
	Position      int    `json:"position"`
	SessionKnown  bool   `json:"session_known"`
	// R229: протокол запроса (openai/ollama) — из сессии, если она известна.
	Protocol string `json:"protocol,omitempty"`
}

// SessionIdentitySource — R118: чем является ключ сессии, который показывает
// очередь: client (различает клиентов), derived (выведен из IP+User-Agent или
// общего токена — НЕ различает), none (идентификатора нет).
//
// Префиксы заданы ровно там, где ключ порождается (client.go,
// autoload_wait.go), поэтому список закрыт и проверяется тестом.
func SessionIdentitySource(session string) string {
	s := strings.TrimSpace(session)
	switch {
	case s == "":
		return sessionIdentityNone
	case strings.HasPrefix(s, "fp:"), strings.HasPrefix(s, "tkn:"):
		// fp: — хеш IP+User-Agent, tkn: — хеш Authorization: у двух экземпляров
		// клиента на одной машине (или под одним токеном) они совпадают.
		return sessionIdentityDerived
	case strings.HasPrefix(s, "conv:"):
		// R118.9 (2026-10-14): ключ выведен балансером из НАЧАЛА РАЗГОВОРА. Он не
		// пришёл от клиента, но РАЗЛИЧАЕТ экземпляры: два клиента с одной машины
		// получают разные ключи, пока работают над разными задачами. Для счётчика
		// «неразличимых ожидающих» это идентифицированный ожидающий.
		return sessionIdentityClient
	default:
		return sessionIdentityClient
	}
}

// queueAdmissionStats — R67b: срез admission-очереди для /api/v1/queue/stats.
//
// Порядок полей — от «тяжёлых» к лёгким: golangci-lint включает govet
// fieldalignment, поэтому map/slice (единственные поля с указателями) идут
// первыми — иначе анализатор требует переставить их (pointer data prefix).
type queueAdmissionStats struct {
	ByBackend     map[string]int        `json:"waiting_by_backend,omitempty"`
	Sessions      []string              `json:"sessions,omitempty"`
	WaitingDetail []admissionWaiterInfo `json:"waiting_detail,omitempty"`
	Served        int64                 `json:"served_total"`
	Timeouts      int64                 `json:"timeout_total"`
	WaitedTotal   int64                 `json:"waited_total"`
	AvgWaitMs     int64                 `json:"avg_wait_ms"`
	WaitSec       int                   `json:"wait_max_sec"`
	Waiting       int                   `json:"waiting"`
	// R118: сколько ожидающих балансер НЕ может различить между собой —
	// ключ выведен из окружения (IP+User-Agent, общий токен) или отсутствует.
	// На странице «Очередь» это повод сказать оператору прямо: «клиент не
	// различает сессии», а не показывать одинаковые строки как разные клиенты.
	Unidentified int  `json:"unidentified_waiting"`
	ActiveByUser int  `json:"active_sessions"`
	Enabled      bool `json:"enabled"`
}

// admissionQueue — очередь ожидающих слотов по бэкендам.
//
// Модель данных намеренно простая: срез ожидающих на бэкенд + счётчик активных
// запросов по сессии. Право «попробовать взять слот» выдаётся ровно одному
// ожидающему (самому приоритетному), остальные спят на broadcast-канале.
type admissionQueue struct {
	waiters    map[string][]*admissionWaiter
	inflight   map[string]int
	lastServed map[string]time.Time
	notify     chan struct{}
	mu         sync.Mutex
	seq        uint64
	// wait — предел ожидания слота (LB_ADMISSION_WAIT_SEC). Хранится здесь, а не
	// в Proxy: так структура Proxy не растёт (allocator size class, см. govet
	// fieldalignment) — очередь и её настройка живут одним указателем.
	wait time.Duration
	// waitOverridden — R326 (2026-10-30): явный override (тесты, Shutdown).
	//
	// Пока он не выставлен, предел ожидания читается из реестра настроек НА
	// КАЖДОМ вызове. LB_ADMISSION_WAIT_SEC — «горячая» настройка (override из
	// WebUI → env → дефолт), но до R326 значение снималось один раз при создании
	// Proxy: правка в WebUI писалась в реестр, показывалась как применённая и не
	// влияла на поведение до перезапуска балансера.
	waitOverridden bool
	// R229: как узнать протокол сессии для waiting_detail (nil = неизвестен).
	protocolOf func(string) string
	// стата
	waitedTotal  int64
	timeoutTotal int64
	waitedSumMs  int64
	servedTotal  int64
}

func newAdmissionQueue(wait time.Duration) *admissionQueue {
	return &admissionQueue{
		waiters:    make(map[string][]*admissionWaiter),
		inflight:   make(map[string]int),
		lastServed: make(map[string]time.Time),
		notify:     make(chan struct{}),
		wait:       wait,
	}
}

// broadcast — разбудить всех ожидающих (закрываем текущий канал и создаём новый).
func (aq *admissionQueue) broadcast() {
	if aq == nil {
		return
	}
	aq.mu.Lock()
	close(aq.notify)
	aq.notify = make(chan struct{})
	aq.mu.Unlock()
}

// waitChannel — канал текущего «поколения» пробуждений.
func (aq *admissionQueue) waitChannel() <-chan struct{} {
	aq.mu.Lock()
	defer aq.mu.Unlock()
	return aq.notify
}

// enqueue — поставить ожидающего в очередь бэкенда.
func (aq *admissionQueue) enqueue(backendID, session string) *admissionWaiter {
	aq.mu.Lock()
	defer aq.mu.Unlock()
	aq.pruneLocked()
	aq.seq++
	w := &admissionWaiter{session: session, enqueued: time.Now(), seq: aq.seq}
	aq.waiters[backendID] = append(aq.waiters[backendID], w)
	return w
}

// remove — убрать ожидающего из очереди (по таймауту/отмене/выдаче слота).
func (aq *admissionQueue) remove(backendID string, w *admissionWaiter) {
	aq.mu.Lock()
	defer aq.mu.Unlock()
	aq.removeLocked(backendID, w)
}

func (aq *admissionQueue) removeLocked(backendID string, w *admissionWaiter) {
	list := aq.waiters[backendID]
	for i, cand := range list {
		if cand == w {
			aq.waiters[backendID] = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(aq.waiters[backendID]) == 0 {
		delete(aq.waiters, backendID)
	}
}

// position — позиция ожидающего в очереди бэкенда (1-based, для заголовков).
func (aq *admissionQueue) position(backendID string, w *admissionWaiter) int {
	aq.mu.Lock()
	defer aq.mu.Unlock()
	for i, cand := range aq.waiters[backendID] {
		if cand == w {
			return i + 1
		}
	}
	return 0
}

// waitingCount — R73: сколько запросов ждут слот (по всем бэкендам, включая
// ключ «any» единой очереди). Нужен для backpressure (см. admissionOverloaded).
func (aq *admissionQueue) waitingCount() int {
	if aq == nil {
		return 0
	}
	aq.mu.Lock()
	defer aq.mu.Unlock()
	total := 0
	for _, list := range aq.waiters {
		total += len(list)
	}
	return total
}

// markInflight — изменить счётчик активных запросов сессии.
func (aq *admissionQueue) markInflight(session string, delta int) {
	if aq == nil || session == "" {
		return
	}
	aq.mu.Lock()
	defer aq.mu.Unlock()
	next := aq.inflight[session] + delta
	if next <= 0 {
		delete(aq.inflight, session)
		return
	}
	aq.inflight[session] = next
}

// inflightOf — сколько запросов ЭТОЙ сессии сейчас в полёте.
//
// R139 (2026-10-16): нужен, чтобы отличить «backend занят МОИМ же длинным
// стримом» от «backend занят чужими запросами». Первое — не повод уводить
// сессию на другой backend (см. proxy.go, rebalanceIfNeeded): там модель
// придётся грузить заново, клиент видит тишину. Второе — повод, ради этой
// справедливости ребалансировка и сделана.
func (aq *admissionQueue) inflightOf(session string) int {
	if aq == nil || session == "" {
		return 0
	}
	aq.mu.Lock()
	defer aq.mu.Unlock()
	return aq.inflight[session]
}

// canProceed — имеет ли ПРАВО этот ожидающий попытаться взять слот.
//
// Приоритет: (1) сессии, которые не обслуживались (нет активного запроса и не
// получали слот в admissionFairnessWindow), (2) сессии без активного запроса, но
// недавно обслуженные, (3) повторные запросы активной сессии. Внутри группы —
// FIFO. Так повторные запросы одного пользователя уступают новым пользователям.
func (aq *admissionQueue) canProceed(backendID string, w *admissionWaiter) bool {
	aq.mu.Lock()
	defer aq.mu.Unlock()
	best := aq.bestLocked(backendID)
	return best == w
}

// priorityLocked — группа приоритета ожидающего (меньше = раньше).
func (aq *admissionQueue) priorityLocked(w *admissionWaiter) int {
	if aq.inflight[w.session] > 0 {
		return 2 // сессия уже обслуживается — её повторный запрос уступает
	}
	if served, ok := aq.lastServed[w.session]; ok && time.Since(served) < admissionFairnessWindow {
		return 1 // недавно обслуживалась — уступает тем, кто ещё не обслуживался
	}
	return 0
}

// bestLocked — самый приоритетный ожидающий (mu удерживается).
func (aq *admissionQueue) bestLocked(backendID string) *admissionWaiter {
	list := aq.waiters[backendID]
	if len(list) == 0 {
		return nil
	}
	var best *admissionWaiter
	bestGroup := 0
	for _, cand := range list {
		group := aq.priorityLocked(cand)
		switch {
		case best == nil, group < bestGroup:
			best, bestGroup = cand, group
		case group == bestGroup && cand.seq < best.seq:
			best = cand
		}
	}
	return best
}

// markServed — сессия получила слот (обновляет приоритет очереди).
func (aq *admissionQueue) markServed(session string) {
	if aq == nil || session == "" {
		return
	}
	aq.mu.Lock()
	aq.lastServed[session] = time.Now()
	aq.mu.Unlock()
}

// pruneLocked — забыть сессии, не обслуживавшиеся дольше окна справедливости
// (иначе map растёт бесконечно на долгом uptime).
func (aq *admissionQueue) pruneLocked() {
	for session, at := range aq.lastServed {
		if time.Since(at) > admissionFairnessWindow {
			delete(aq.lastServed, session)
		}
	}
}

// stats — снимок состояния очереди для /api/v1/queue/stats.
func (aq *admissionQueue) stats(wait time.Duration) queueAdmissionStats {
	if aq == nil {
		return queueAdmissionStats{}
	}
	aq.mu.Lock()
	defer aq.mu.Unlock()
	out := queueAdmissionStats{
		Enabled:      wait > 0,
		WaitSec:      int(wait.Seconds()),
		ByBackend:    make(map[string]int, len(aq.waiters)),
		Served:       aq.servedTotal,
		Timeouts:     aq.timeoutTotal,
		WaitedTotal:  aq.waitedTotal,
		AvgWaitMs:    0,
		ActiveByUser: len(aq.inflight),
	}
	// Waiting — число ожидающих ЗАПРОСОВ (не бэкендов): на одном бэкенде их
	// может быть несколько.
	//
	// R91: кроме счётчиков собираем поимённый список. Порядок внутри бэкенда —
	// как в очереди (по seq), поэтому UI показывает реальные позиции. Бэкенды
	// обходим в порядке ключей, чтобы выдача была стабильной между запросами.
	backendIDs := make([]string, 0, len(aq.waiters))
	for backendID := range aq.waiters {
		backendIDs = append(backendIDs, backendID)
	}
	sort.Strings(backendIDs)
	now := time.Now()
	for _, backendID := range backendIDs {
		list := aq.waiters[backendID]
		out.Waiting += len(list)
		out.ByBackend[backendID] = len(list)
		for i, w := range list {
			if w.session != "" {
				out.Sessions = append(out.Sessions, w.session)
			}
			// R118: позиция считается здесь, а не в UI по индексу строки: порядок
			// отрисовки и порядок очереди — разные вещи, и оператору нужна позиция
			// В ОЧЕРЕДИ (1-based, по seq постановки).
			source := SessionIdentitySource(w.session)
			known := source == sessionIdentityClient
			if !known {
				out.Unidentified++
			}
			out.WaitingDetail = append(out.WaitingDetail, admissionWaiterInfo{
				Protocol:      aq.protocolOfSession(w.session),
				Session:       w.session,
				SessionSource: source,
				Backend:       backendID,
				WaitedMs:      now.Sub(w.enqueued).Milliseconds(),
				Position:      i + 1,
				SessionKnown:  known,
			})
		}
	}
	if aq.waitedTotal > 0 {
		out.AvgWaitMs = aq.waitedSumMs / aq.waitedTotal
	}
	return out
}

// recordServed — статистика успешного ожидания.
func (aq *admissionQueue) recordServed(waited time.Duration) {
	if aq == nil {
		return
	}
	aq.mu.Lock()
	aq.servedTotal++
	aq.waitedTotal++
	aq.waitedSumMs += waited.Milliseconds()
	aq.mu.Unlock()
}

// recordTimeout — статистика исчерпанного ожидания.
func (aq *admissionQueue) recordTimeout() {
	if aq == nil {
		return
	}
	aq.mu.Lock()
	aq.timeoutTotal++
	aq.mu.Unlock()
}

// admissionLease — выданный слот (то же, что слот tryAcquireSlot).
// Порядок полей — под fieldalignment (release-функция и строки несут указатели).
type admissionLease struct {
	release   func()
	BackendID string
	Session   string
	Waited    time.Duration
	Position  int
}

// Release — освободить слот. Идемпотентно (sync.Once внутри release).
func (l *admissionLease) Release() {
	if l == nil || l.release == nil {
		return
	}
	l.release()
}

// admissionEnabled — включено ли ожидание (для метрик/WebUI).
func (p *Proxy) admissionEnabled() bool {
	return p.admissionWaitTimeout() > 0
}

// admissionWaitTimeout — предел ожидания слота (0 = ожидание выключено).
// nil-safe: Proxy без admission-очереди (юнит-тесты) → 0.
func (p *Proxy) admissionWaitTimeout() time.Duration {
	if p == nil || p.admission == nil {
		return 0
	}
	// R326 (2026-10-30): явный override (тесты, Shutdown) важнее настройки,
	// иначе берём АКТУАЛЬНОЕ значение из реестра: LB_ADMISSION_WAIT_SEC меняется
	// без рестарта (override из WebUI → env → дефолт), и «горячесть» иначе
	// оставалась бы только на бумаге — правка в WebUI применялась к копии,
	// снятой при создании Proxy.
	if p.admission.waitOverridden {
		return p.admission.wait
	}
	return AdmissionWaitTimeout()
}

// setAdmissionWait — изменить предел ожидания (используется тестами и Shutdown).
func (p *Proxy) setAdmissionWait(wait time.Duration) {
	if p == nil || p.admission == nil {
		return
	}
	p.admission.mu.Lock()
	p.admission.wait = wait
	p.admission.waitOverridden = true
	p.admission.mu.Unlock()
}

// acquireInferenceSlotNow — R70: быстрый путь без очереди (слот свободен?).
// Нужен вызывающему коду, чтобы решить, включать ли keepalive для streaming-
// клиента ДО того, как заголовки будут отправлены.
// Возвращает (nil, errAdmissionDisabled), если слот занят.
func (p *Proxy) acquireInferenceSlotNow(backendID, session string) (*admissionLease, error) {
	if p == nil || backendID == "" {
		return nil, errAdmissionDisabled
	}
	if p.tryAcquireSlot(backendID) {
		return p.newAdmissionLease(backendID, session, 0, 0), nil
	}
	return nil, errAdmissionDisabled
}

// acquireInferenceSlot — R67b: получить слот бэкенда, при необходимости встав в
// admission-очередь. Возвращает lease (release обязателен), позицию в очереди на
// момент постановки и ошибку:
//
//	nil                   — слот получен (Waited>0, если пришлось ждать);
//	errAdmissionDisabled  — ожидание выключено и слотов нет (legacy 503);
//	errAdmissionTimeout   — ожидание истекло (503 + Retry-After);
//	ctx.Err()             — клиент отвалился/отменил запрос.
func (p *Proxy) acquireInferenceSlot(
	ctx context.Context, backendID, session string,
) (*admissionLease, int, error) {
	if p == nil || backendID == "" {
		return nil, 0, errAdmissionDisabled
	}
	// R117: бюджет ожидания — max(admission, auto-load), см. admissionWaitBudget.
	wait := p.admissionWaitBudget()

	if p.tryAcquireSlot(backendID) {
		return p.newAdmissionLease(backendID, session, 0, 0), 0, nil
	}
	if wait <= 0 || p.admission == nil {
		return nil, 0, errAdmissionDisabled
	}

	aq := p.admission
	w := aq.enqueue(backendID, session)
	position := aq.position(backendID, w)
	deadline := time.Now().Add(wait)
	logger.Get().Infow("admission: request queued, waiting for free slot",
		"backend", backendID, "session", session, "position", position,
		"wait_max_sec", int(wait.Seconds()))
	defer aq.remove(backendID, w)

	for {
		// Канал берём ДО попытки захвата: иначе пробуждение между попыткой и
		// select потерялось бы и запрос ждал лишний poll-интервал.
		notify := aq.waitChannel()
		if aq.canProceed(backendID, w) && p.tryAcquireSlot(backendID) {
			waited := time.Since(w.enqueued)
			aq.recordServed(waited)
			logger.Get().Infow("admission: slot acquired after waiting",
				"backend", backendID, "session", session,
				"waited_ms", waited.Milliseconds(), "position", position)
			return p.newAdmissionLease(backendID, session, waited, position), position, nil
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			aq.recordTimeout()
			logger.Get().Warnw("admission: wait timeout, returning 503",
				"backend", backendID, "session", session,
				"waited_ms", time.Since(w.enqueued).Milliseconds(),
				"wait_max_sec", int(wait.Seconds()))
			return nil, position, errAdmissionTimeout
		}
		nap := admissionPollInterval
		if remaining < nap {
			nap = remaining
		}
		timer := time.NewTimer(nap)
		select {
		case <-notify:
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			logger.Get().Infow("admission: client canceled while waiting for slot",
				"backend", backendID, "session", session,
				"waited_ms", time.Since(w.enqueued).Milliseconds())
			return nil, position, ctx.Err()
		}
		timer.Stop()
	}
}

// newAdmissionLease — lease вокруг tryAcquireSlot/releaseSlot с учётом
// per-session inflight (нужен для справедливости очереди).
func (p *Proxy) newAdmissionLease(backendID, session string, waited time.Duration, position int) *admissionLease {
	aq := p.admission
	aq.markInflight(session, 1)
	aq.markServed(session)
	var once sync.Once
	return &admissionLease{
		BackendID: backendID,
		Session:   session,
		Waited:    waited,
		Position:  position,
		release: func() {
			once.Do(func() {
				// inflight уменьшаем ДО broadcast (внутри releaseSlot), иначе
				// ожидающие той же сессии увидят её «занятой» ещё один круг.
				aq.markInflight(session, -1)
				p.releaseSlot(backendID)
			})
		},
	}
}

// admissionKeepaliveDefaultSec — R70: период keepalive для streaming-клиента,
// который ждёт свободный слот (0 = выключить).
const admissionKeepaliveDefaultSec = 5

// AdmissionKeepaliveInterval — R70 (2026-09-24): период keepalive при ожидании
// слота в admission-очереди.
//
//	LB_ADMISSION_KEEPALIVE_SEC=N — раз в N секунд; 0 = выключено (прежнее
//	поведение: клиент ждёт молча и может отвалиться по своему таймауту).
//	По умолчанию 5 c.
func AdmissionKeepaliveInterval() time.Duration {
	v := strings.TrimSpace(os.Getenv("LB_ADMISSION_KEEPALIVE_SEC"))
	if v == "" {
		return admissionKeepaliveDefaultSec * time.Second
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return admissionKeepaliveDefaultSec * time.Second
	}
	return time.Duration(n) * time.Second
}

// admissionStreamKind — определяет, какой keepalive безопасен для этого
// streaming-запроса.
//
//   - OpenAI-пути (/v1/chat/completions, /v1/completions) отдают SSE:
//     keepalive = комментарий `: keepalive` (парсеры SSE его игнорируют);
//   - нативные Ollama-пути (/api/chat, /api/generate) отдают NDJSON: keepalive =
//     пустая строка (построчные парсеры её пропускают), писать туда `: …`
//     нельзя — это сломает JSON-строки;
//   - non-stream запросы keepalive не получают (клиенту нужен цельный ответ,
//     а заголовки/статус ещё могут измениться на 503).
//
// Возвращает ("", nil, false), если keepalive неприменим.
func admissionStreamKind(path string, body []byte) (contentType string, keepalive []byte, ok bool) {
	if !isStreamingFromBody(path, body) {
		return "", nil, false
	}
	switch {
	case strings.HasPrefix(path, "/v1/"):
		return "text/event-stream", []byte(": keepalive\n\n"), true
	case strings.HasPrefix(path, "/api/"):
		return "application/x-ndjson", []byte("\n"), true
	default:
		return "", nil, false
	}
}

// ADMISSION-HONEST (2026-10-15): функция startAdmissionKeepalive удалена вместе
// с преждевременной коммитой заголовков. Keepalive во время ожидания слота
// невозможен без отправки статуса 200, а именно он и был живым дефектом
// («OpenWebUI получает 200 + `: keepalive` 30-50 c»). Клиент теперь ждёт молча
// и получает либо 200 со слотом и X-Queue-*, либо честный 503.

// acquireInferenceAdmission — R67b: точка входа для inference-хендлеров
// llama.cpp. Берёт слот бэкенда через admission-очередь (с ожиданием) и
// возвращает release-функцию.
//
// Возвращает (release, ok):
//   - (lease.Release, true)  — слот получен, обработку можно продолжать;
//   - (nil, true)            — ожидание выключено (LB_ADMISSION_WAIT_SEC=0):
//     работаем как до R67b, без учёта слотов на этом пути;
//   - (nil, false)           — клиенту уже отправлен ответ (503 при таймауте или
//     ничего при отмене): хендлер обязан сделать return.
//
// Заголовки X-Queue-Wait-Ms / X-Queue-Position выставляются ДО того, как
// транспорт пишет свои заголовки, поэтому клиент видит факт ожидания в очереди.
//
// ADMISSION-HONEST (2026-10-15): раньше (R70) для streaming-клиента заголовки
// коммитились СРАЗУ при занятом слоте (200 + Content-Type) и во время ожидания
// слались keepalive'ы. Живой дефект стенда: второй клиент OpenWebUI на
// /v1/chat/completions мгновенно получал 200 + `X-Queue-Keepalive: 1` и 30-50 c
// пустого тела (`: keepalive`), а честные X-Queue-Wait-Ms/X-Queue-Position и
// 503+Retry-After при исчерпании ожидания были НЕДОСТИЖИМЫ — статус уже
// отправлен, и 503-ветка не могла его изменить. Теперь заголовки не коммитятся
// до выдачи слота: клиент ждёт молча и получает либо 200 с ожиданием в
// заголовках и настоящим ответом, либо 503 + Retry-After + X-Queue-Position.
// Поведение /api/chat и /v1/chat/completions на этом пути одинаково.
func (lr *LlamaCppRouter) acquireInferenceAdmission(
	w http.ResponseWriter, r *http.Request, backendID, model string, body []byte,
) (func(), bool) {
	if lr == nil || lr.proxy == nil {
		return nil, true
	}
	session := RequestSessionKey(r, body)
	// R229: запомнить протокол для ключа очереди (ключ != ID сессии — см.
	// admissionProtocols: без этой записи waiting_detail.protocol был пустым).
	lr.proxy.noteAdmissionProtocol(session, RequestProtocol(r.URL.Path))

	// Быстрый путь: слот свободен — очередь не нужна.
	if lease, err := lr.proxy.acquireInferenceSlotNow(backendID, session); err == nil {
		return lease.Release, true
	}

	// Слот занят. ADMISSION-HONEST (2026-10-15): НЕ коммитим заголовки и не шлём
	// keepalive'ы до выдачи слота (см. комментарий к функции) — иначе 200 уходит
	// клиенту раньше, чем он получил слот, и ни X-Queue-*, ни 503 при таймауте
	// до него уже не доедут.
	//
	// R329 (2026-10-30): ждущий запрос ОБЯЗАН быть виден в мониторе.
	//
	// ЖИВАЯ ЖАЛОБА: «запрос крутится, а на мониторе его нет». Строки ожидания в
	// панели очереди никто не писал с R78: qm.pending оставался мёртвым срезом, и
	// оператор видел только агрегат. Регистрируем строку «ждёт слот» ЗДЕСЬ — до
	// того, как запрос получит или не получит слот, и снимаем её при выходе.
	pendingRow := lr.proxy.noteAdmissionPendingR329(model, backendID)
	defer lr.proxy.clearAdmissionPendingR329(pendingRow)
	lease, position, err := lr.proxy.acquireInferenceSlot(r.Context(), backendID, session)
	switch {
	case err == nil:
		if lease.Waited > 0 {
			// Заголовки ещё не отправлены — клиент увидит, что запрос стоял в
			// очереди, и на /api/chat, и на /v1/chat/completions.
			w.Header().Set("X-Queue-Wait-Ms", strconv.FormatInt(lease.Waited.Milliseconds(), 10))
			w.Header().Set("X-Queue-Position", strconv.Itoa(position))
			logger.Get().Infow("admission: slot granted after waiting",
				"backend", backendID, "model", model, "session", session,
				"waited_ms", lease.Waited.Milliseconds())
		}
		return lease.Release, true
	case errors.Is(err, errAdmissionDisabled):
		// Ожидание выключено — сохраняем поведение до R67b (без гейта).
		return nil, true
	case errors.Is(err, errAdmissionTimeout):
		// R117: бюджет, а не голый LB_ADMISSION_WAIT_SEC — см. admissionWaitBudget.
		// waited == предела ожидания: запрос действительно стоял в очереди весь
		// разрешённый интервал и только потом получил 503. Заголовки не
		// отправлены, поэтому статус честный (до ADMISSION-HONEST здесь был 200
		// с keepalive-телом или с chunk с ошибкой внутри стрима).
		maxWait := lr.proxy.admissionWaitBudget()
		writeAdmissionUnavailable(w, model, session, position, maxWait, maxWait)
		return nil, false
	default:
		// Клиент отменил запрос, пока ждал слот: отвечать уже некому.
		logger.Get().Infow("admission: request canceled before slot was granted",
			"backend", backendID, "model", model, "session", session)
		return nil, false
	}
}

// writeAdmissionUnavailable — R67b: 503 «нет свободных слотов» с понятными
// заголовками (клиент видит, что запрос стоял в очереди, и когда повторить).
//
// Phase 6 follow-up (2026-10-02): лимит ожидания бывает сужен per-request —
// когда единственная причина отсутствия кандидата это активный image-лок
// (см. imageLockWaitCap / waitForInferenceBackend). Признак этого — waited
// СТРОГО меньше настроенного предела: из цикла ожидания errAdmissionTimeout
// возвращается только при remaining <= 0, то есть waited >= применённого
// лимита всегда. Поэтому здесь достаточно сравнения, без протаскивания флага
// через сигнатуру (лишний параметр сломал бы всех вызывающих).
func writeAdmissionUnavailable(w http.ResponseWriter, model, session string, position int, waited, maxWait time.Duration) {
	capped := waited > 0 && waited < maxWait
	if capped {
		// Отчитываемся ФАКТИЧЕСКИ применённым лимитом: иначе клиент и оператор
		// увидели бы LB_ADMISSION_WAIT_SEC, которого в этом запросе не было.
		maxWait = waited
	}
	w.Header().Set("Retry-After", strconv.Itoa(admissionRetryAfterSec))
	if position > 0 {
		w.Header().Set("X-Queue-Position", strconv.Itoa(position))
	}
	w.Header().Set("X-Queue-Wait-Ms", strconv.FormatInt(waited.Milliseconds(), 10))
	w.Header().Set("X-Queue-Wait-Max-Sec", strconv.Itoa(int(maxWait.Seconds())))
	payload := map[string]interface{}{
		"error": "all inference slots are busy, request waited in queue and timed out: " +
			"model '" + model + "'",
		"queued":        true,
		"queuePosition": position,
		"waitedMs":      waited.Milliseconds(),
		"waitMaxSec":    int(maxWait.Seconds()),
		"retryAfterMs":  admissionRetryAfterSec * 1000,
		"session":       session,
	}
	if capped {
		// Причина отказа машинночитаема: слот не освободился, потому что GPU
		// занята image-генерацией (exclusive-политика), а не потому что очередь
		// переполнена текстовыми запросами.
		w.Header().Set("X-Queue-Wait-Reason", imageLockWaitReason)
		payload["waitReason"] = imageLockWaitReason
		payload["waitReasonHint"] = "the text backends are on a GPU held by an image generation " +
			"(balancing.image.coexistence=exclusive); retry after it finishes or lower balancing.image.queueWaitTimeoutSec expectations"
	}
	writeJSON(w, http.StatusServiceUnavailable, payload)
}
