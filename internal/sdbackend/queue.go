package sdbackend

import (
	"context"
	"sync"
)

// ============================================================
// Очередь генераций
// ============================================================
//
// ПОЧЕМУ СВОЯ ОЧЕРЕДЬ, А НЕ ОЧЕРЕДЬ sd-server:
// движок исполняет генерации строго по одной (один sd_ctx, один мьютекс),
// а его внутренняя FIFO на 64 слота (AsyncJobManager) не даёт ни приоритетов,
// ни честного queue_position для клиента балансера. Поэтому воркер держит
// собственную очередь и использует sd-server как односоставный исполнитель
// (см. docs/research-sdcpp-lowvram-integration.md §4.4).
//
// Реализация — счётный семафор на канале. Acquire НЕ блокируется: при
// заполненной очереди немедленно возвращает ErrQueueFull, а вызывающий отдаёт
// HTTP 429 с Retry-After. Осознанно: блокирующее ожидание слота превращало бы
// 429 в «висящий» запрос без позиции в очереди и без обратной связи клиенту —
// ровно та проблема, которую пришлось чинить в cppworker (R70).

// Queue — счётный семафор с неблокирующим захватом.
type Queue struct {
	slots   chan struct{}
	mu      sync.Mutex
	waiting int // сколько запросов получили ErrQueueFull (для метрик)
}

// NewQueue создаёт очередь на size одновременных генераций.
func NewQueue(size int) *Queue {
	if size < 1 {
		size = 1
	}
	return &Queue{slots: make(chan struct{}, size)}
}

// Capacity — размер очереди.
func (q *Queue) Capacity() int { return cap(q.slots) }

// Acquire занимает слот. ErrQueueFull — если свободных слотов нет; решение,
// что ответить клиенту (429 + Retry-After), принимает HTTP-слой.
// ctx используется только для логической проверки отмены до захвата.
func (q *Queue) Acquire(ctx context.Context) (release func(), err error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	select {
	case q.slots <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-q.slots }) }, nil
	default:
		q.mu.Lock()
		q.waiting++
		q.mu.Unlock()
		return nil, ErrQueueFull
	}
}

// TryAcquire — то же, что Acquire, но без контекста (async-джобы).
func (q *Queue) TryAcquire() (release func(), ok bool) {
	select {
	case q.slots <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-q.slots }) }, true
	default:
		return nil, false
	}
}

// InFlight — сколько слотов занято сейчас.
func (q *Queue) InFlight() int { return len(q.slots) }

// Rejected — сколько запросов получили ErrQueueFull за жизнь процесса.
func (q *Queue) Rejected() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.waiting
}
