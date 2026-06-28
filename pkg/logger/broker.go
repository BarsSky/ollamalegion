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
	// Ring buffer: append + truncate если превышен размер.
	b.buffer = append(b.buffer, entry)
	if len(b.buffer) > b.bufferSize {
		// Удаляем самые старые, оставляем последние bufferSize записей.
		excess := len(b.buffer) - b.bufferSize
		b.buffer = b.buffer[excess:]
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