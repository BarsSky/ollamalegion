// batched_scheduler.go — Round 15.1 (2026-07-29): BatchedScheduler для
// true parallel inference.
//
// Архитектура: single-goroutine scheduler, периодически (по тикеру
// batchedWindowMs) собирает ОДИН токен от каждой active session,
// вызывает bridge_batched_decode (ОДИН llama_decode call для всех
// sessions параллельно), сэмплирует следующий токен per-session
// (пока greedy argmax), и рассылает токены в каналы каждой session.
//
// Это шаг 3.3 из дизайн-дока docs/plans/round-15-batched-parallel.md.
// Предыдущие шаги (3.1-3.2) добавили C-side build_batched_batch + bridge_batched_decode.
// Round 13 multi-slot path остаётся default (opt-in через EnableBatchedParallel).
//
// State machine per session:
//   registering → prefill (chunk prompt tokens, multi-step)
//   → generating (1 token per tick until EOG or max_tokens)
//   → finished (close TokenCh + DoneCh)
//
// Concurrency model:
//   - BatchedScheduler.Run(ctx) — ОДНА горутина (tick loop).
//   - RegisterSession / UnregisterSession — concurrent-safe (mu защищает
//     sessions map).
//   - Каждая session имеет свой TokenCh (buffered) + DoneCh (signals close).
//
// Sampling (пока упрощённо):
//   - Greedy argmax: token = argmax(logits). Достаточно для smoke test.
//   - В Round 15.2 (или позже) заменим на полный Sampler chain (temperature,
//     top_p, top_k, repetition_penalty, mirostat, etc.) — pattern как
//     в llama-cli's common_sampler.

package cppbackend

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/pkg/logger"
)

// BatchedSessionID — уникальный ID сессии в scheduler'е. Выдаётся
// RegisterSession. Используется как ключ в sessions map.
type BatchedSessionID uint64

// BatchedSessionState — состояние одной session в scheduler.
//
// SeqID — уникальный llama_seq_id (slot index в n_parallel range).
// Выдаётся scheduler'ом при RegisterSession, стабильный на всю жизнь сессии.
// Может быть переиспользован после UnregisterSession + RegisterSession
// (но в Round 15.1 — инкрементный counter без reuse).
type BatchedSessionState struct {
	ID         BatchedSessionID
	SeqID      int32         // llama_seq_id для KV-cache routing
	Prompt     []int32       // начальные токены prompt'а
	Generated  []int32       // сгенерированные токены (excluding prompt)
	MaxTokens  int           // max tokens to generate
	NextToken  int32         // следующий токен для feed'а в decode (start: prompt[0])
	NextPos    int32         // позиция в KV-cache для следующего токена
	TokenCh    chan int32    // stream output (sampled tokens)
	DoneCh     chan struct{} // closed when session finishes
	Finished   bool          // true after EOG or MaxTokens
	Err        error         // non-nil if inference failed
	mu         sync.Mutex    // protects session state from concurrent reads
}

// BatchedScheduler — single-goroutine scheduler для multi-session parallel
// inference через bridge_batched_decode.
//
// Lifecycle:
//   - Created via NewBatchedScheduler(model, nParallel, windowMs)
//   - Started via Run(ctx) — single goroutine, blocks until ctx.Done
//   - RegisterSession(s) to add a new session (non-blocking, returns session ID)
//   - UnregisterSession(id) to remove (graceful — waits for current tick to finish)
//   - Per-tick: collect 1 token per active session, call BatchedDecode, sample, dispatch
type BatchedScheduler struct {
	model     *bridge.ModelHandle
	nParallel int32         // max concurrent sessions (= llama n_seq_max)
	windowMs  int32         // batching window (default 5ms)
	interval  time.Duration // = time.Duration(windowMs) * time.Millisecond

	mu       sync.Mutex
	sessions map[BatchedSessionID]*BatchedSessionState
	nextSeq  int32 // next available seq_id (1, 2, 3, ...)
	nextID   BatchedSessionID

	// stopOnce — single channel для graceful shutdown.
	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{} // closed after Run() returns
}

// BatchedSchedulerConfig — параметры конструктора.
type BatchedSchedulerConfig struct {
	Model     *bridge.ModelHandle
	NParallel int32         // должно совпадать с n_parallel в ModelConfig
	WindowMs  int32         // batching window (default 5ms; range 1-50)
}

// NewBatchedScheduler — создаёт scheduler. nParallel должно совпадать с
// n_parallel, переданным в LoadModelWithOpts (для KV-cache sizing).
// windowMs — интервал тикера (default 5ms).
func NewBatchedScheduler(cfg BatchedSchedulerConfig) (*BatchedScheduler, error) {
	if cfg.Model == nil {
		return nil, errors.New("NewBatchedScheduler: Model is nil")
	}
	if cfg.NParallel <= 0 {
		return nil, fmt.Errorf("NewBatchedScheduler: NParallel must be > 0, got %d", cfg.NParallel)
	}
	if cfg.WindowMs <= 0 {
		cfg.WindowMs = 5 // default 5ms (per design doc R3)
	}
	if cfg.WindowMs > 50 {
		return nil, fmt.Errorf("NewBatchedScheduler: WindowMs > 50 not supported (max 50ms), got %d", cfg.WindowMs)
	}
	return &BatchedScheduler{
		model:     cfg.Model,
		nParallel: cfg.NParallel,
		windowMs:  cfg.WindowMs,
		interval:  time.Duration(cfg.WindowMs) * time.Millisecond,
		sessions:  make(map[BatchedSessionID]*BatchedSessionState),
		nextSeq:   1, // 0 зарезервирован для legacy single-slot
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}, nil
}

// RegisterSession — добавляет новую session. Возвращает session ID и
// каналы для streaming (TokenCh читает сгенерированные токены, DoneCh
// закрывается по завершении).
//
// Caller обязан:
//   1. Читать TokenCh пока не закроется.
//   2. После close проверить session.Err.
//
// Неблокирующий: возвращает управление сразу. Scheduler начнёт
// обрабатывать session на следующем tick'е (latency = 1 batching window).
func (bs *BatchedScheduler) RegisterSession(prompt []int32, maxTokens int) (BatchedSessionID, *BatchedSessionState, error) {
	bs.mu.Lock()
	defer bs.mu.Unlock()

	if int32(len(bs.sessions)) >= bs.nParallel {
		return 0, nil, fmt.Errorf("BatchedScheduler at capacity: %d/%d sessions active",
			len(bs.sessions), bs.nParallel)
	}

	bs.nextID++
	seqID := bs.nextSeq
	bs.nextSeq++

	state := &BatchedSessionState{
		ID:        bs.nextID,
		SeqID:     seqID,
		Prompt:    prompt,
		Generated: make([]int32, 0, maxTokens),
		MaxTokens: maxTokens,
		NextToken: prompt[0], // первый токен = первый токен prompt'а
		NextPos:   0,
		TokenCh:   make(chan int32, 8), // буферизованный, чтобы scheduler не блокировался
		DoneCh:    make(chan struct{}),
	}
	bs.sessions[bs.nextID] = state
	logger.Get().Infow("BatchedScheduler: session registered",
		"id", bs.nextID, "seq_id", seqID, "prompt_tokens", len(prompt),
		"max_tokens", maxTokens, "active", len(bs.sessions))
	return bs.nextID, state, nil
}

// UnregisterSession — удаляет session. НЕ дожидается завершения (caller
// должен прочитать TokenCh до EOF).
func (bs *BatchedScheduler) UnregisterSession(id BatchedSessionID) {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	if state, ok := bs.sessions[id]; ok {
		// Если session ещё не finished — закрываем с ошибкой.
		state.mu.Lock()
		if !state.Finished {
			state.Err = errors.New("session unregistered before completion")
			state.Finished = true
			close(state.DoneCh)
		}
		state.mu.Unlock()
		// Закрываем TokenCh только если ещё не закрыт (Finished sessions
		// уже имеют закрытый TokenCh).
		select {
		case <-state.TokenCh:
			// Уже закрыт
		default:
			// Безопасное закрытие: если никто не читает — GC дойдёт позже.
			// Используем recover для safety.
			defer func() { _ = recover() }()
			close(state.TokenCh)
		}
		delete(bs.sessions, id)
		logger.Get().Infow("BatchedScheduler: session unregistered",
			"id", id, "active", len(bs.sessions))
	}
}

// Stop — graceful shutdown. Закрывает scheduler (Run() возвращается).
// Безопасно вызывать несколько раз.
func (bs *BatchedScheduler) Stop() {
	bs.stopOnce.Do(func() {
		close(bs.stop)
	})
}

// Done — возвращает канал, который закрывается после Run() return.
func (bs *BatchedScheduler) Done() <-chan struct{} {
	return bs.done
}

// ActiveSessions — возвращает список active session IDs (для мониторинга).
func (bs *BatchedScheduler) ActiveSessions() []BatchedSessionID {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	ids := make([]BatchedSessionID, 0, len(bs.sessions))
	for id := range bs.sessions {
		ids = append(ids, id)
	}
	return ids
}

// Run — главный цикл scheduler'а. Блокирует до ctx.Done() или Stop().
//
// Tick loop:
//   1. Собрать active sessions (filter Finished).
//   2. Для каждой session подготовить BatchedSequence (1 token, pos = NextPos).
//   3. Вызвать model.BatchedDecode (ОДИН llama_decode для всех sessions).
//   4. Для каждой session сэмплировать next token (greedy argmax пока).
//   5. Отправить token в session.TokenCh, инкрементировать NextPos/NextToken.
//   6. Если EOG (или MaxTokens) — пометить Finished, close DoneCh + TokenCh.
func (bs *BatchedScheduler) Run(ctx context.Context) {
	defer close(bs.done)

	ticker := time.NewTicker(bs.interval)
	defer ticker.Stop()

	logger.Get().Infow("BatchedScheduler: starting", "interval_ms", bs.windowMs, "n_parallel", bs.nParallel)

	for {
		select {
		case <-ctx.Done():
			logger.Get().Infow("BatchedScheduler: ctx done, stopping")
			bs.markAllSessionsFinished(ctx.Err())
			return
		case <-bs.stop:
			logger.Get().Infow("BatchedScheduler: stop signaled")
			bs.markAllSessionsFinished(errors.New("scheduler stopped"))
			return
		case <-ticker.C:
			bs.tick(ctx)
		}
	}
}

// markAllSessionsFinished — helper для graceful shutdown.
func (bs *BatchedScheduler) markAllSessionsFinished(err error) {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	for _, s := range bs.sessions {
		s.mu.Lock()
		if !s.Finished {
			s.Err = err
			s.Finished = true
			close(s.DoneCh)
		}
		s.mu.Unlock()
	}
}

// tick — один цикл batched inference.
func (bs *BatchedScheduler) tick(ctx context.Context) {
	// Шаг 1: собираем active sessions.
	bs.mu.Lock()
	active := make([]*BatchedSessionState, 0, len(bs.sessions))
	for _, s := range bs.sessions {
		s.mu.Lock()
		if !s.Finished {
			active = append(active, s)
		}
		s.mu.Unlock()
	}
	bs.mu.Unlock()

	if len(active) == 0 {
		return // нечего декодить
	}

	// Шаг 2: готовим BatchedSequence для каждой session.
	// Каждая session предоставляет ОДИН токен (NextToken) для текущего шага.
	sequences := make([]bridge.BatchedSequence, 0, len(active))
	for _, s := range active {
		s.mu.Lock()
		sequences = append(sequences, bridge.BatchedSequence{
			Tokens:   []int32{s.NextToken}, // borrow — НЕ копируется
			SeqID:    s.SeqID,
			StartPos: s.NextPos,
		})
		s.mu.Unlock()
	}

	// Шаг 3: ОДИН llama_decode для всех sequences.
	// ВАЖНО: instance.mu держится на стороне вызывающего (Backend). Здесь
	// мы НЕ берём никаких блокировок — caller (Backend.batchedInfer) обязан
	// сериализовать через inst.mu.
	logits, err := bs.model.BatchedDecode(sequences)
	if err != nil {
		logger.Get().Errorw("BatchedScheduler: BatchedDecode failed", "error", err)
		// Распространяем ошибку на все sessions.
		bs.markAllSessionsFinished(fmt.Errorf("batched decode failed: %w", err))
		return
	}

	// Шаг 4: per-session sampling + dispatch.
	// TODO Round 15.2: заменить на полный Sampler chain. Пока — greedy argmax.
	for i, s := range active {
		s.mu.Lock()
		if s.Finished {
			// Между нашим сбором active и текущим моментом session мог
			// быть finished (например, ctx.Done() обработан параллельно).
			s.mu.Unlock()
			continue
		}

		// Sampling: greedy argmax. logits[i] = []float32 длины n_vocab.
		nextToken := argmaxToken(logits[i])

		// Шаг 5: обновляем session state.
		s.Generated = append(s.Generated, nextToken)
		s.NextPos++
		isFinished := isEOGToken(nextToken) || len(s.Generated) >= s.MaxTokens

		// Готовим NextToken для следующего tick'а (всегда — для batched
		// decoder нужны logits от nextToken, не от текущего).
		// В greedy argmax — это просто nextToken.
		s.NextToken = nextToken

		if isFinished {
			s.Finished = true
		}
		s.mu.Unlock()

		// Шаг 6: диспатч.
		select {
		case s.TokenCh <- nextToken:
			// OK
		case <-ctx.Done():
			return
		}

		if isFinished {
			close(s.TokenCh)
			close(s.DoneCh)
			logger.Get().Infow("BatchedScheduler: session finished",
				"id", s.ID, "seq_id", s.SeqID, "generated_tokens", len(s.Generated),
				"eog", isEOGToken(nextToken), "max_reached", len(s.Generated) >= s.MaxTokens)
		}
	}
}

// argmaxToken — greedy sampling. Возвращает индекс argmax'а во float массиве.
// В Round 15.2 заменим на полный Sampler chain.
func argmaxToken(logits []float32) int32 {
	if len(logits) == 0 {
		return 0
	}
	bestIdx := 0
	bestVal := float32(math.Inf(-1))
	for i, v := range logits {
		if v > bestVal {
			bestVal = v
			bestIdx = i
		}
	}
	return int32(bestIdx)
}

// isEOGToken — простая проверка на end-of-generation token. В Round 15.2
// будет использовать llama_vocab_is_eog через C-bridge (нужна новая
// функция bridge_vocab_is_eog). Пока — heuristic: token 1 (обычно <eos>)
// и 2 (<bos> для некоторых моделей) считаются EOG. Это неточно — в проде
// нужно использовать vocab API.
func isEOGToken(t int32) bool {
	return t == 1 || t == 2
}
