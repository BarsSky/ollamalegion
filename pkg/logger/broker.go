package logger

import (
	"sync"
	"time"
)

// LogEntry — одна запись системного лога, отдаваемая в WebSocket stream.
//
// JSON-сериализация через json.Marshal (поля в верхнем регистре для совместимости
// с существующим frontend renderLogs() в webui/app.js, который ожидает keys:
// time, level, message, source).
type LogEntry struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"` // "debug" | "info" | "warn" | "error"
	Message string    `json:"message"`
	Source  string    `json:"source,omitempty"` // optional: "balancer" | "proxy" | "cppworker" | etc.
	// Fields — структурированные поля события (backend, model, request_id, …).
	//
	// R481. Раньше поля склеивались в Message строкой `{"backend":"x",…}`, и в
	// WebUI лог выглядел как «текст + нечитаемый JSON» — оператор не мог ни
	// прочитать сообщение, ни найти поле (жалоба «лишний текст, поля
	// некорректно заполняются»). Теперь Message — только текст, а Fields
	// отдаются отдельно: UI рисует раскрываемый блок «поля (N)», поиск и
	// экспорт работают с ними как с данными.
	Fields map[string]interface{} `json:"fields,omitempty"`

	// R715 (D3 плана 2026-11-24): схлопывание повторов.
	//
	// Repeated — сколько раз событие повторилось (1 = обычная запись).
	// FirstTime/LastTime — первое и последнее появление. Поля ДОБАВЛЕНЫ, а не
	// заменяют прежние: time/level/message/source/fields остаются на месте и с
	// прежним смыслом (time = Time, у схлопнутой записи — время ПЕРВОГО
	// появления), поэтому старые потребители JSON (логи WS, экспорт, WebUI)
	// читают записи как раньше, а новые видят счётчик.
	Repeated  int        `json:"repeated,omitempty"`
	FirstTime *time.Time `json:"first_time,omitempty"`
	LastTime  *time.Time `json:"last_time,omitempty"`
}

// LogSubscriber — одна подписка (один WebSocket-клиент).
type LogSubscriber struct {
	id      string
	channel chan LogEntry
	done    chan struct{}
}

// Channel возвращает приёмный канал для горутины рассылки.
func (ls *LogSubscriber) Channel() <-chan LogEntry { return ls.channel }

// Done возвращает канал закрытия подписки (контекст отменён / клиент отключился).
func (ls *LogSubscriber) Done() <-chan struct{} { return ls.done }

// Close безопасно закрывает каналы подписчика. Идемпотентно.
func (ls *LogSubscriber) Close() {
	defer func() { _ = recover() }() // защита от double-close
	close(ls.done)
	close(ls.channel)
}

// LogBroker — pub/sub для log-записей с ring buffer для replay при (re)connect.
//
// Потокобезопасен. Используется в internal/api/handlers_logs_ws.go для стриминга
// логов в WebSocket /ws/logs.
//
// Семантика ring buffer: при переполнении (>bufferSize) старые записи молча
// отбрасываются. Это нормально для live-tail: при reconnect клиент получает
// последние N записей, не всю историю с момента запуска.
type LogBroker struct {
	mu          sync.RWMutex
	subscribers map[string]*LogSubscriber
	buffer      []LogEntry
	bufferSize  int
	nextID      uint64
	stopCh      chan struct{}
	stopped     bool

	// R715 (D3): режим схлопывания повторов. См. aggregate.go — зачем.
	//
	// aggregate=false — историческое поведение (каждый Publish занимает запись);
	// aggregate=true  — повтор с тем же AggregateKeyR715 увеличивает Repeated у
	// уже лежащей записи. Буфер при этом хранит РАЗНЫЕ события, поэтому окно
	// логов растёт без роста памяти.
	aggregate bool
	// aggIndex — ключ повтора → позиция в buffer. aggKeys — ключ КАЖДОЙ
	// позиции (нужен, чтобы перестроить aggIndex после обрезки хвоста буфера).
	aggIndex map[string]int
	aggKeys  []string
	// trimChunk — сколько записей срезать за раз при переполнении. Без этого
	// каждая публикация после заполнения требовала бы перестроения aggIndex
	// (O(размера буфера)), то есть на типовой нагрузке 2000+ операций на
	// строку лога.
	trimChunk int
	// publishedTotal / collapsedTotal — счётчики ДО схлопывания: по ним видно,
	// сколько строк реально пришло и сколько из них были повторами (окно логов
	// в метаданных архива считается по ним, а не по остатку в буфере).
	publishedTotal uint64
	collapsedTotal uint64
}

// NewLogBroker создаёт брокер с ring buffer указанного размера (default 100 при <=0).
func NewLogBroker(bufferSize int) *LogBroker {
	if bufferSize <= 0 {
		bufferSize = 100
	}
	return &LogBroker{
		subscribers: make(map[string]*LogSubscriber),
		buffer:      make([]LogEntry, 0, bufferSize),
		bufferSize:  bufferSize,
		stopCh:      make(chan struct{}),
	}
}

// NewAggregatingLogBroker — R715 (D3): тот же кольцевой буфер, но одинаковые
// события схлопываются (см. aggregate.go).
//
// Отдельный конструктор, а не флаг у NewLogBroker: схлопывание меняет смысл
// Snapshot (длина среза = число РАЗНЫХ событий, а не число строк), и включать
// его молча для всех потребителей было бы скрытой сменой контракта. Балансер и
// image-воркер включают его явно; тестовые брокеры остаются историческими.
func NewAggregatingLogBroker(bufferSize int) *LogBroker {
	b := NewLogBroker(bufferSize)
	b.aggregate = true
	b.aggIndex = make(map[string]int, bufferSize)
	b.trimChunk = bufferSize / 10
	if b.trimChunk < 1 {
		b.trimChunk = 1
	}
	return b
}

// Aggregates сообщает, включено ли схлопывание повторов.
func (b *LogBroker) Aggregates() bool { return b.aggregate }

// Publish добавляет запись в ring buffer и рассылает всем подписчикам.
//
// Неблокирующая: если канал подписчика заполнен — запись для него отбрасывается
// (медленный клиент не должен блокировать producer).
func (b *LogBroker) Publish(entry LogEntry) {
	if entry.Time.IsZero() {
		entry.Time = time.Now()
	}
	if entry.Level == "" {
		entry.Level = "info"
	}

	b.mu.Lock()
	b.publishedTotal++
	if b.aggregate {
		if b.publishAggregatedLocked(entry) {
			// Запись стала повтором: строка в буфере не появилась, но лив-тейл
			// обязан её показать — подписчик ждёт каждое событие.
			b.collapsedTotal++
		}
	} else {
		// Ring buffer: append + truncate если превышен размер.
		b.buffer = append(b.buffer, entry)
		if len(b.buffer) > b.bufferSize {
			// Удаляем самые старые, оставляем последние bufferSize записей.
			excess := len(b.buffer) - b.bufferSize
			b.buffer = b.buffer[excess:]
		}
	}
	// Snapshot подписчиков под RLock — чтобы не удерживать write lock при send.
	subs := make([]*LogSubscriber, 0, len(b.subscribers))
	for _, s := range b.subscribers {
		subs = append(subs, s)
	}
	b.mu.Unlock()

	// Неблокирующая рассылка.
	for _, s := range subs {
		select {
		case s.channel <- entry:
		case <-s.done:
			// Подписчик отвалился — удалим его позже (Close от loop).
		default:
			// Канал заполнен (медленный клиент) — пропускаем запись для этого sub.
			// Логируем только при первом drop, чтобы не спамить.
			// TODO: метрика dropped_total.
		}
	}
}

// Snapshot возвращает копию ring buffer (для initial backlog при reconnect).
// Возвращает slice в порядке oldest -> newest.
func (b *LogBroker) Snapshot() []LogEntry {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]LogEntry, len(b.buffer))
	copy(out, b.buffer)
	return out
}

// publishAggregatedLocked — R715: положить запись в буфер со схлопыванием.
//
// Возвращает true, если запись оказалась ПОВТОРОМ уже лежащего события (тогда
// новая строка в буфере не появилась, а у существующей вырос Repeated и
// сдвинулся LastTime). Вызывается под b.mu.
func (b *LogBroker) publishAggregatedLocked(entry LogEntry) bool {
	key := AggregateKeyR715(entry)
	if key != "" {
		if pos, ok := b.aggIndex[key]; ok && pos >= 0 && pos < len(b.buffer) &&
			b.aggKeys[pos] == key {
			slot := &b.buffer[pos]
			if slot.Repeated == 0 {
				// Первый повтор: фиксируем границы события. Время первого
				// появления — это Time самой записи (её никто не переписывает).
				first := slot.Time
				slot.FirstTime = &first
				slot.Repeated = 1
			}
			slot.Repeated++
			last := entry.Time
			slot.LastTime = &last
			// Текст и поля остаются от ПЕРВОГО появления: это представитель
			// события, а меняющиеся числа (disk_free_mb и т.п.) в ключ не входят
			// — заменять их на последние значило бы показать не то, что случилось
			// первым. Последнее время есть отдельным полем.
			return true
		}
	}
	b.buffer = append(b.buffer, entry)
	b.aggKeys = append(b.aggKeys, key)
	if len(b.buffer) > b.bufferSize {
		// trimLocked ПЕРЕСТРОИТ индекс целиком: позиции сместились.
		b.trimLocked()
	} else if key != "" {
		b.aggIndex[key] = len(b.buffer) - 1
	}
	return false
}

// trimLocked — срезать голову буфера и перестроить индекс ключей.
//
// Срезается не ровно 1 запись, а trimChunk (10% ёмкости): перестроение индекса
// стоит O(числа записей), и делать его на КАЖДУЮ строку лога — это те самые
// «2000 операций на строку», которых не должно быть. Размер буфера при этом
// никогда не превышает bufferSize.
func (b *LogBroker) trimLocked() {
	excess := len(b.buffer) - b.bufferSize + b.trimChunk
	if excess > len(b.buffer) {
		excess = len(b.buffer)
	}
	if excess <= 0 {
		return
	}
	b.buffer = append(b.buffer[:0:0], b.buffer[excess:]...)
	b.aggKeys = append(b.aggKeys[:0:0], b.aggKeys[excess:]...)
	for k := range b.aggIndex {
		delete(b.aggIndex, k)
	}
	for pos, k := range b.aggKeys {
		if k != "" {
			b.aggIndex[k] = pos
		}
	}
}

// LogBufferStats — снимок состояния буфера для диагностики (R715).
//
// PublishedTotal — сколько строк реально пришло (до схлопывания);
// CollapsedTotal — сколько из них стали повторами; Entries — сколько РАЗНЫХ
// событий лежит в буфере. Первое/последнее время — границы ОКНА, которое архив
// реально покрывает (то самое «13 минут», из-за которых заведён D3).
type LogBufferStats struct {
	Aggregating    bool      `json:"aggregating"`
	Capacity       int       `json:"capacity"`
	Entries        int       `json:"entries"`
	PublishedTotal uint64    `json:"published_total"`
	CollapsedTotal uint64    `json:"collapsed_total"`
	FirstTime      time.Time `json:"first_time"`
	LastTime       time.Time `json:"last_time"`
	ApproxBytes    int       `json:"approx_bytes"`
}

// Stats — снимок состояния буфера (см. LogBufferStats).
func (b *LogBroker) Stats() LogBufferStats {
	b.mu.RLock()
	defer b.mu.RUnlock()
	st := LogBufferStats{
		Aggregating:    b.aggregate,
		Capacity:       b.bufferSize,
		Entries:        len(b.buffer),
		PublishedTotal: b.publishedTotal,
		CollapsedTotal: b.collapsedTotal,
		ApproxBytes:    LogEntriesApproxBytesR715(b.buffer),
	}
	for _, e := range b.buffer {
		first, last := e.FirstOccurrence(), e.LastOccurrence()
		if !first.IsZero() && (st.FirstTime.IsZero() || first.Before(st.FirstTime)) {
			st.FirstTime = first
		}
		if last.After(st.LastTime) {
			st.LastTime = last
		}
	}
	return st
}

// Subscribe регистрирует нового подписчика и возвращает его handle.
// Подписчик автоматически удаляется из брокера при вызове subscriber.Close().
func (b *LogBroker) Subscribe() *LogSubscriber {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.stopped {
		// Брокер уже остановлен — отдаём no-op подписчика с закрытыми каналами.
		// Оба канала закрываем сразу, чтобы read на channel() сразу возвращал (zero, false),
		// а read на done() сразу возвращал closed signal.
		ls := &LogSubscriber{id: "stopped", channel: make(chan LogEntry), done: make(chan struct{})}
		close(ls.channel)
		close(ls.done)
		return ls
	}

	b.nextID++
	id := time.Now().Format("20060102T150405.000") + "-" + itoa(b.nextID)
	ls := &LogSubscriber{
		id:      id,
		channel: make(chan LogEntry, 100),
		done:    make(chan struct{}),
	}
	b.subscribers[id] = ls

	// Запускаем авто-удаление: когда done закрывается — убираем из списка.
	go func(id string) {
		<-ls.done
		b.mu.Lock()
		delete(b.subscribers, id)
		b.mu.Unlock()
	}(id)

	return ls
}

// SubscriberCount возвращает текущее число подписчиков (для debug / health-check).
func (b *LogBroker) SubscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subscribers)
}

// Stop закрывает брокер: все подписчики получают сигнал done.
// После Stop повторные Publish/Panic-safe, Subscribe возвращает no-op подписчика.
func (b *LogBroker) Stop() {
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return
	}
	b.stopped = true
	subs := make([]*LogSubscriber, 0, len(b.subscribers))
	for _, s := range b.subscribers {
		subs = append(subs, s)
	}
	b.subscribers = make(map[string]*LogSubscriber)
	b.mu.Unlock()

	close(b.stopCh)
	for _, s := range subs {
		s.Close()
	}
}

// itoa — локальная утилита (избегаем strconv.Itoa ради минимальных зависимостей).
// Простая реализация для положительных uint64.
func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	digits := make([]byte, 0, 20)
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// Глобальный broker — инициализируется через SetBroker из main.go.
var (
	globalBrokerMu sync.RWMutex
	globalBroker   *LogBroker
)

// SetBroker устанавливает глобальный broker (обычно из main.go после logger.Init).
func SetBroker(b *LogBroker) {
	globalBrokerMu.Lock()
	defer globalBrokerMu.Unlock()
	globalBroker = b
}

// GetBroker возвращает текущий глобальный broker или nil (если не инициализирован).
// Используется в WebSocket handler'е для доступа к тому же broker'у.
func GetBroker() *LogBroker {
	globalBrokerMu.RLock()
	defer globalBrokerMu.RUnlock()
	return globalBroker
}

// Publish — глобальный helper для записи в broker. No-op если broker не инициализирован
// или остановлен. Удобно вызывать из любого места кода:
//
//	logger.Publish(logger.LogEntry{Level: "warn", Source: "balancer", Message: "..."})
//
// После этого запись попадает во все активные WebSocket подписки на /ws/logs.
func Publish(entry LogEntry) {
	b := GetBroker()
	if b == nil {
		return
	}
	b.Publish(entry)
}