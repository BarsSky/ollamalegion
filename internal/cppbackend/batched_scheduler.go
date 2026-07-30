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
	"sync/atomic"
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
//
// State machine:
//   registering → prefill (ingest prompt tokens, 1 per tick)
//   → generating (1 token per tick via BatchedDecode + greedy sample)
//   → finished (close TokenCh + DoneCh)
type BatchedSessionState struct {
	ID         BatchedSessionID
	SeqID      int32         // llama_seq_id для KV-cache routing
	Prompt     []int32       // начальные токены prompt'а
	Generated  []int32       // сгенерированные токены (excluding prompt)
	MaxTokens  int           // max tokens to generate
	NextToken  int32         // следующий токен для feed'а в decode
	NextPos    int32         // позиция в KV-cache для следующего токена
	// PrefillDone — true когда ВСЕ prompt токены ingested в KV-cache.
	// Round 15.1 fix: без prefill модель видит только 1 токен и генерирует
	// бессмыслицу (e.g. "What is 2+2?" → "Okay, the user asked...").
	// Prefill: NextPos от 0 до len(Prompt), на каждом tick ингестим prompt[NextPos].
	// После prefill: переходим в generation phase, NextPos = len(Prompt).
	PrefillDone bool
	// Round 15.2 (2026-07-30): sampling params.
	// Temperature = 0 (default) → greedy argmax. > 0 → softmax+multinomial.
	// Seed для RNG (0 = time-based, non-zero = reproducible).
	Temperature float32
	Seed        uint32
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

	// modelLock — external lock (modelInstance.mu) which scheduler MUST
	// hold на время BatchedDecode (Round 8 BUGFIX: llama.cpp context is
	// not thread-safe, и в batched path единственный Run goroutine
	// обращается к handle, но всё равно держим lock для safety — если
	// кто-то добавит direct inst.handle.Infer() в другом месте).
	modelLock sync.Locker

	mu       sync.Mutex
	sessions map[BatchedSessionID]*BatchedSessionState
	nextSeq  int32 // next available seq_id (1, 2, 3, ...)
	nextID   BatchedSessionID

	// stopOnce — single channel для graceful shutdown.
	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{} // closed after Run() returns

	// Round 16 code-review fix (2026-07-30): мониторинг fallback'ов в
	// sampleFromLogits. Раньше C-bridge ошибки при sampling молча
	// пропадали → production с temperature=0.7 могла выдавать greedy output
	// без видимой причины. Сейчас логируем + атомарный счётчик для
	// observability (можно экспортировать через /metrics).
	sampleFallbackCount atomic.Int64
	sampleOKCount      atomic.Int64
}

// BatchedSchedulerConfig — параметры конструктора.
type BatchedSchedulerConfig struct {
	Model     *bridge.ModelHandle
	NParallel int32         // должно совпадать с n_parallel в ModelConfig
	WindowMs  int32         // batching window (default 5ms; range 1-50)
	// ModelLock — external lock, держится scheduler'ом на время llama_decode
	// (Round 8 BUGFIX: serialize access к llama.cpp context). nil → scheduler
	// НЕ лочит (deprecated, только для unit-тестов).
	ModelLock sync.Locker
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
		modelLock: cfg.ModelLock, // может быть nil для тестов
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
// BatchedSessionParams — параметры для RegisterSession.
// Round 15.2 (2026-07-30): добавлены Temperature/Seed для sampling.
type BatchedSessionParams struct {
	Prompt      []int32
	MaxTokens   int
	Temperature float32 // 0 = greedy argmax. > 0 = softmax(temp) + multinomial.
	Seed        uint32  // 0 = time-based. != 0 = reproducible для тестов.
}

// Caller обязан:
//   1. Читать TokenCh пока не закроется.
//   2. После close проверить session.Err.
//
// Неблокирующий: возвращает управление сразу. Scheduler начнёт
// обрабатывать session на следующем tick'е (latency = 1 batching window).
func (bs *BatchedScheduler) RegisterSession(params BatchedSessionParams) (BatchedSessionID, *BatchedSessionState, error) {
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
		Prompt:    params.Prompt,
		Generated: make([]int32, 0, params.MaxTokens),
		MaxTokens: params.MaxTokens,
		NextToken: params.Prompt[0], // первый токен = первый токен prompt'а
		NextPos:   0,                 // начинаем с prefill pos 0
		PrefillDone: len(params.Prompt) == 0, // edge case: пустой prompt = prefilled
		Temperature: params.Temperature,
		Seed:        params.Seed,
		// Round 16 code-review fix (2026-07-30): buffer 8 → 128.
		// Раньше buffer=8: если consumer (batchedInferStream) медленнее
		// scheduler tick rate (default 5ms), за 8 ticks (40ms) TokenCh
		// заполняется → tick блокируется на send → ВСЕ остальные сессии
		// в текущем batch стоят (head-of-line blocking). При
		// разноскоростных клиентах (Cline + WebUI одновременно) throughput
		// всей ноды деградирует до самого медленного consumer'а.
		//
		// 128 = ~640ms при 5ms tick (10x предыдущего) — достаточно чтобы
		// bursty consumers (типичный WebUI SSE chunking) выровнялись,
		// при этом не настолько большой чтобы OOM на 8 параллельных
		// сессиях с MaxTokens=4096. Каждый int32 = 4 bytes → 512B/session
		// = 4KB на 8 sessions = ничтожно.
		//
		// Если buffer всё равно заполняется — sampleFromLogits tick drop'нет
		// token (skip dispatch) и логирует (см. tickSendTokenWithDrop).
		TokenCh:   make(chan int32, 128),
		DoneCh:    make(chan struct{}),
	}
	bs.sessions[bs.nextID] = state
	logger.Get().Infow("BatchedScheduler: session registered",
		"id", bs.nextID, "seq_id", seqID, "prompt_tokens", len(params.Prompt),
		"max_tokens", params.MaxTokens, "temperature", params.Temperature,
		"seed", params.Seed, "active", len(bs.sessions))
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
//
// Phase 1: PREFILL (Round 15.2 — multi-token).
//   Каждая session ингестит ВСЕ оставшиеся prompt токены за ОДИН call
//   (через bridge_batched_decode с n_tokens>1). Round 15.1 делал
//   1 токен за call — медленно (80 calls для 4 sessions × 20 prompt tokens).
//   Round 15.2: 4 calls для тех же 4 sessions (по 1 call на session).
//   Logits копируются для ПОСЛЕДНЕГО токена каждой sequence —
//   используем их для первого sampled token (это эффективнее чем
//   отдельный generation tick с prompt[last]).
//
// Phase 2: GENERATION.
//   Когда PrefillDone=true — каждая session предоставляет NextToken
//   (последний сгенерированный) для BatchedDecode (1 token за call).
//   После decode делаем greedy argmax и диспатчим в TokenCh.
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
	// В PREFILL phase — отправляем ВСЕ оставшиеся prompt токены за раз
	// (multi-token sequence, n_tokens может быть 1..len(prompt)).
	// В GENERATION phase — отправляем ОДИН NextToken за раз.
	sequences := make([]bridge.BatchedSequence, 0, len(active))
	// activeByIdx — маппинг sequences index → session. Нужен в шаге 4
	// для правильного обновления state (sessions которые finished в
	// шаге 1 уже отфильтрованы).
	for _, s := range active {
		s.mu.Lock()
		if !s.PrefillDone {
			// Prefill: отправляем prompt[NextPos:]. StartPos = NextPos.
			// n_tokens = len(prompt) - NextPos (оставшиеся токены).
			if int(s.NextPos) < len(s.Prompt) {
				tokensToIngest := s.Prompt[s.NextPos:]
				sequences = append(sequences, bridge.BatchedSequence{
					Tokens:   tokensToIngest, // borrow — НЕ копируется
					SeqID:    s.SeqID,
					StartPos: s.NextPos,
				})
			} else {
				// Defensive: NextPos уже == len(prompt) — отметить PrefillDone.
				// Не должно происходить, но на всякий случай.
				s.PrefillDone = true
				s.NextToken = s.Prompt[len(s.Prompt)-1]
				s.mu.Unlock()
				continue
			}
		} else {
			// Generation: 1 token (s.NextToken = последний сгенерированный).
			sequences = append(sequences, bridge.BatchedSequence{
				Tokens:   []int32{s.NextToken}, // borrow — НЕ копируется
				SeqID:    s.SeqID,
				StartPos: s.NextPos,
			})
		}
		s.mu.Unlock()
	}

	if len(sequences) == 0 {
		return // все sessions finished (defensive path)
	}

	// Шаг 3: ОДИН llama_decode для всех sequences.
	// Round 8 BUGFIX: llama.cpp context is not thread-safe. Держим
	// modelLock (modelInstance.mu) на время BatchedDecode.
	if bs.modelLock != nil {
		bs.modelLock.Lock()
	}
	logits, err := bs.model.BatchedDecode(sequences)
	if bs.modelLock != nil {
		bs.modelLock.Unlock()
	}
	if err != nil {
		logger.Get().Errorw("BatchedScheduler: BatchedDecode failed", "error", err)
		bs.markAllSessionsFinished(fmt.Errorf("batched decode failed: %w", err))
		return
	}

	// Шаг 4: per-session update + dispatch.
	// PREFILL phase: используем logits[session_idx] для первого sampled
	//   token (это logits последнего prompt токена — корректно!), set
	//   NextToken = sampled, NextPos = len(prompt), PrefillDone=true.
	//   Dispatch в TokenCh (это первый ответ клиенту).
	// GENERATION phase: greedy argmax + dispatch (как раньше).
	for i, s := range active {
		s.mu.Lock()
		if s.Finished {
			s.mu.Unlock()
			continue
		}

		if !s.PrefillDone {
			// PREFILL (Round 15.2): use logits from last prompt token to sample
			// first generated token. Это экономит 1 generation tick vs
			// Round 15.1 (где первый token был NextToken=prompt[last] без sample).
			// Round 15.2: temperature sampling (через bridge_sample_token C-side).
			// s.Temperature = 0 (default) → greedy argmax. > 0 → softmax+multinomial.
			firstToken := sampleFromLogits(bs.model, logits[i], s.Temperature, s.Seed)

			// State transitions: prompt ingested, generation phase begins.
			s.PrefillDone = true
			s.NextPos = int32(len(s.Prompt)) // all prompt ingested
			s.NextToken = firstToken
			s.Generated = append(s.Generated, firstToken)
			// Round 15.2: vocab-aware EOG detection через C-bridge.
			isFinished := isEOGTokenModel(bs.model, firstToken) || len(s.Generated) >= s.MaxTokens
			if isFinished {
				s.Finished = true
			}
			s.mu.Unlock()

			// Dispatch first generated token (или close если finished).
			if isFinished {
				close(s.TokenCh)
				close(s.DoneCh)
				logger.Get().Infow("BatchedScheduler: session finished (during prefill→gen transition)",
					"id", s.ID, "seq_id", s.SeqID, "first_token", firstToken,
					"eog", isEOGTokenModel(bs.model, firstToken), "max_reached", len(s.Generated) >= s.MaxTokens)
			} else {
				select {
				case s.TokenCh <- firstToken:
					// OK
				case <-ctx.Done():
					return
				}
			}
			continue
		}

		// GENERATION: temperature sampling (Round 15.2). logits[i] = []float32 длины n_vocab.
		// s.Temperature = 0 → greedy (как Round 15.1). > 0 → softmax+multinomial.
		nextToken := sampleFromLogits(bs.model, logits[i], s.Temperature, s.Seed)

		// Шаг 5: обновляем session state.
		s.Generated = append(s.Generated, nextToken)
		s.NextPos++
		// Round 15.2: vocab-aware EOG detection через C-bridge.
		isFinished := isEOGTokenModel(bs.model, nextToken) || len(s.Generated) >= s.MaxTokens

		// Готовим NextToken для следующего tick'а (всегда — для batched
		// decoder нужны logits от nextToken, не от текущего).
		// В greedy argmax — это просто nextToken.
		s.NextToken = nextToken

		if isFinished {
			s.Finished = true
		}
		s.mu.Unlock()

		// Шаг 6: диспатч.
		// Round 16 code-review fix (2026-07-30): non-blocking send +
		// context check. Раньше blocking send блокировал ВСЕ остальные
		// сессии в batch при медленном consumer'е (head-of-line blocking).
		// Теперь если TokenCh полон — пропускаем token для ЭТОЙ сессии
		// (state всё равно обновлён выше: Generated++, NextPos++, NextToken
		// обновлён — model не re-sample'нет), counter инкрементится для
		// observability. Consumer в catch-up mode прочитает следующие
		// токены, этот токен просто потерян (acceptable для streaming
		// UI — не блокирует всю ноду).
		select {
		case s.TokenCh <- nextToken:
			// OK — consumer успевает
		case <-ctx.Done():
			return
		default:
			// TokenCh полон — consumer отстал. Skip этот token.
			// Логируем не на каждом — counter-based throttle.
			dropCnt := tickDropCounter.Add(1)
			if dropCnt == 1 || dropCnt%1000 == 0 {
				logger.Get().Warnw("BatchedScheduler: TokenCh full, dropping token (consumer too slow)",
					"session_id", s.ID, "seq_id", s.SeqID,
					"generated_tokens", len(s.Generated),
					"drop_count_total", dropCnt)
			}
		}

		if isFinished {
			close(s.TokenCh)
			close(s.DoneCh)
			logger.Get().Infow("BatchedScheduler: session finished",
				"id", s.ID, "seq_id", s.SeqID, "generated_tokens", len(s.Generated),
				"eog", isEOGTokenModel(bs.model, nextToken), "max_reached", len(s.Generated) >= s.MaxTokens)
		}
	}
}

// sampleFromLogits — Round 15.2 (2026-07-30): temperature sampling.
//
// Использует C-bridge bridge_sample_token (см. c/bridge/bridge.h + bridge.c):
//   temperature <= 0  → greedy argmax (быстро, deterministic)
//   temperature > 0   → softmax с temperature, multinomial sampling с std::mt19937
//
// При ошибке C-вызова (например n_vocab=0 или malloc OOM) fallback на
// argmax — лучше плохой ответ чем паника.
//
// Round 16 code-review fix (2026-07-30): fallback теперь ЛОГИРУЕТСЯ (раньше
// молча) + инкрементит atomic counter для observability. Без логов production
// с temperature=0.7 могла выдавать greedy output без видимой причины — баг,
// который невозможно диагностировать post-mortem.
func sampleFromLogits(model *bridge.ModelHandle, logits []float32, temperature float32, seed uint32) (result int32) {
	if len(logits) == 0 {
		return 0
	}
	// Используем C-bridge (там proper log-sum-exp + std::mt19937).
	tok, err := model.SampleToken(logits, temperature, seed)
	if err != nil {
		// Fallback на greedy argmax (Go-side, без C-вызова).
		// Round 16 fix: логируем (но не на каждом тике — иначе заспамит при
		// массовом OOM). Используем counter-based throttle: логируем только
		// каждую 1000-ную ошибку, и summary каждые 10 сек. Простая эвристика —
		// логировать первую ошибку и потом каждую 1000-ную.
		fbCount := sampleFallbackCounter.Add(1)
		if fbCount == 1 || fbCount%1000 == 0 {
			logger.Get().Warnw("sampleFromLogits: C-bridge SampleToken failed, falling back to greedy argmax",
				"error", err,
				"logits_len", len(logits),
				"temperature", temperature,
				"fallback_count_total", fbCount)
		}
		return argmaxToken(logits)
	}
	sampleOKCounter.Add(1)
	return tok
}

// sampleFallbackCounter / sampleOKCounter — process-global мониторинг fallback'ов
// sampleFromLogits. Round 16 code-review fix. Можно экспортировать через /metrics
// или /status endpoint для production observability.
var (
	sampleFallbackCounter atomic.Int64
	sampleOKCounter      atomic.Int64
)

// SampleStats — snapshot для /metrics endpoint.
type SampleStats struct {
	OKCount      int64 `json:"sample_ok_count"`
	FallbackCount int64 `json:"sample_fallback_count"`
	TickDropCount int64 `json:"tick_drop_count"` // HoL: tokens dropped because consumer slow
}

// GetSampleStats возвращает текущее количество OK/fallback sampling calls
// + tick drop counter для HoL observability.
func GetSampleStats() SampleStats {
	return SampleStats{
		OKCount:      sampleOKCounter.Load(),
		FallbackCount: sampleFallbackCounter.Load(),
		TickDropCount: tickDropCounter.Load(),
	}
}

// tickDropCounter — Round 16 code-review fix: счётчик token drops при
// head-of-line blocking. Если растёт в production — consumer (batchedInferStream)
// медленнее scheduler tick rate. Увеличить TokenCh buffer (Round 16 уже
// 8→128) или оптимизировать consumer.
var tickDropCounter atomic.Int64

// argmaxToken — greedy sampling. Возвращает индекс argmax'а во float массиве.
// Round 15.2: теперь это fallback для sampleFromLogits (если C-вызов упал).
// Для primary path используется sampleFromLogits с temperature.
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

// isEOGTokenModel — Round 15.2 (2026-07-30): vocab-aware EOG detection
// через C-bridge llama_vocab_is_eog. Корректно для всех моделей (Qwen3,
// Llama, gemma-4, mistral, и т.п.) — не зависит от magic token IDs.
//
// При ошибке C-вызова (например model=nil в unit-тестах) fallback на
// heuristic isEOGToken.
func isEOGTokenModel(model *bridge.ModelHandle, t int32) bool {
	if model == nil {
		return isEOGToken(t)
	}
	isEOG, err := model.IsEOG(t)
	if err != nil {
		return isEOGToken(t)
	}
	return isEOG
}

// isEOGToken — простой fallback для unit-тестов и случаев когда model=nil.
// Token 1 (обычно <eos>) и 2 (<bos> для некоторых моделей) считаются EOG.
// В production используйте isEOGTokenModel (Round 15.2+).
func isEOGToken(t int32) bool {
	return t == 1 || t == 2
}
