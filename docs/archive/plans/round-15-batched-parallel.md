# Round 15 — True Batched Parallel Inference

**Статус:** design (стадия #3.1) — implementation запланирована после approval
**Сложность:** 1-2 недели (senior, требует C-bridge + Go scheduler + интеграция)
**Целевой релиз:** v0.5.0 (после v0.4.12 baseline)

## TL;DR

Round 13 (v0.4.11) даёт **multi-slot state isolation** — два параллельных чата
работают без SIGABRT, но llama_decode всё равно сериализуется через `inst.mu`.
Эффективно: каждый чат ждёт, пока другой закончит forward pass → 0% speedup.

Round 15: **multiple sequences в ОДНОМ `llama_decode` call** — real speedup
на GPU (matmul shared между sequences). Pattern из
`c/llama.cpp/examples/parallel/parallel.cpp`.

## Почему сейчас

- Round 13 deployed, v0.4.12 в проде, slot state isolation работает.
- Qwen3-4B-Thinking на 22 GB VRAM загружен в 32 GPU layers — есть запас для batching.
- Пользователь жалуется что 2 параллельных чата = sequential по latency, не parallel.
- Llama.cpp `parallel.cpp` reference implementation уже написан — бери и адаптируй.

## Архитектура (3 слоя)

### Слой 1: C-bridge — `build_batched_batch` + `bridge_batched_decode`

**Новая функция `build_batched_batch`:**
```c
// Принимает массив sequences, каждая с (tokens, n_tokens, seq_id, start_pos).
// Возвращает ОДИН llama_batch, где все sequences уживаются.
// Каждый токен имеет свой seq_id, llama_decode обрабатывает их в ОДНОМ forward pass.
struct llama_batch build_batched_batch(
    const TokenChunk* chunks,   // массив: tokens[i], n_tokens[i], seq_id[i], start_pos[i]
    int32_t n_chunks,           // количество sequences
    int32_t max_total_tokens    // capacity hint (= n_ctx)
);
```

**Аналогия:** Round 13 `build_batch_with_seq` для одного seq_id, Round 15 —
multi-seq. Симметрично: тот же `llama_batch_init` (n_tokens, 0, 1), populate
loop добавляет токены из всех chunks.

**CRITICAL:** `llama_batch_init` по-прежнему инициализирует `n_tokens=0` (как
в Round 13 — bug aba9a90). После populate loop нужно явно
`batch.n_tokens = total_tokens`. Регрессионный тест в
`c/bridge/tests/test_batch_n_tokens.c` уже покрывает single-seq, добавим
multi-seq case.

**Поток в C-bridge `bridge_batched_decode`:**
1. Acquire: lock `inst.mu` (как Round 8 — llama_decode НЕ thread-safe).
2. Build batch через `build_batched_batch`.
3. `llama_decode(im->context, batch)` — ОДИН forward pass на все sequences.
4. Output: для каждого seq_id → указатель на logits (через `batch.logits[i]` →
   `llama_get_logits_ith(ctx, i)`).
5. Per-seq sampling — Go side делает через существующий `bridge_sample_token`.

**Locking:** `inst.mu` берётся ОДИН раз на весь `bridge_batched_decode`, не на
каждый sequence. SlotManager БОЛЬШЕ НЕ НУЖЕН в batched path (parallel.cpp
тоже не использует slot manager — все sequences в ОДНОМ batch).

### Слой 2: Go — `BatchedScheduler`

**Новый файл `internal/cppbackend/batched_scheduler.go`:**

```go
// BatchedScheduler — single-goroutine scheduler, collects N concurrent
// generation requests, calls bridge_batched_decode ОДИН раз, distributes
// tokens обратно в каналы каждого session.
type BatchedScheduler struct {
    mu       sync.Mutex
    sessions map[uint64]*SessionState // session_id → state
    bridge   *BridgeHandle
    interval time.Duration // 5-10ms batching window
}

type SessionState struct {
    ID         uint64
    SeqID      int32             // llama_seq_id (per-session stable)
    Sampler    *Sampler          // per-session sampler
    Prompt     []Token           // initial tokens
    Generated  []Token           // so far
    TokenCh    chan Token        // streamed output
    DoneCh     chan struct{}     // closed on completion
    Finished   bool
    n_past     int32             // current pos in KV-cache
}
```

**Цикл scheduler'а:**
```go
func (bs *BatchedScheduler) Run(ctx context.Context) {
    ticker := time.NewTicker(bs.interval)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            bs.tick(ctx)
        }
    }
}

func (bs *BatchedScheduler) tick(ctx context.Context) {
    bs.mu.Lock()
    active := bs.activeSessions() // exclude Finished
    if len(active) == 0 {
        bs.mu.Unlock()
        return
    }
    bs.mu.Unlock()

    // Собираем chunks: каждый session добавляет ОДИН токен (последний из
    // своего prompt при первом tick, или последний sampled — на последующих).
    chunks := bs.collectChunks(active)

    // ОДИН decode на все sequences.
    logits, err := bs.bridge.BatchedDecode(chunks)
    if err != nil {
        // Distribute error, mark sessions as failed
        return
    }

    // Per-session sampling → next token.
    for i, s := range active {
        next := s.Sampler.Sample(logits[i])
        s.Generated = append(s.Generated, next)
        s.n_past++
        select {
        case s.TokenCh <- next:
        case <-ctx.Done():
            return
        }
        if s.Sampler.IsEOG(next) || len(s.Generated) >= s.MaxTokens {
            s.Finished = true
            close(s.DoneCh)
        }
    }
}
```

**Batching window = 5-10ms** — компромисс:
- Меньше 5ms — пустой batch, нечего декодить, scheduler thrashing
- Больше 50ms — заметная latency на первый токен

**Cadence alignment:** scheduler **требует** чтобы все sessions двигались
синхронно (1 token per forward pass). Новая session может "догнать" batch на
следующем tick — её prompt обрабатывается сразу, и она присоединяется к
parallel decoding со 2-го тика.

**Cleanup:** scheduler НЕ вызывает `llama_memory_clear` — каждый session
имеет свой seq_id, KV-cache region выделяется автоматически. Когда session
finished → `llama_memory_seq_rm(seq_id)` освобождает регион.

### Слой 3: Backend integration — `internal/cppbackend/backend.go`

**Два пути в `Backend.Infer` / `Backend.InferStream`:**

```go
func (b *Backend) Infer(ctx context.Context, model string, req InferRequest) (*InferResult, error) {
    inst, err := b.getModelInstance(model)
    if err != nil { return nil, err }

    if inst.opts.Parallel > 1 && inst.batchedScheduler != nil {
        // Round 15 path: batched scheduler
        return b.batchedInfer(ctx, inst, req)
    }
    // Round 13 path: slot-based single-seq (legacy fallback)
    return b.slotInfer(ctx, inst, req)
}
```

**BatchedScheduler per-model:** при `LoadModelWithOpts` если
`opts.Parallel > 1 && opts.EnableBatched == true` (новый флаг), создаём
`BatchedScheduler` для этого instance. В Round 14.5+ добавить
`LlamaCppConfig.EnableBatchedParallel bool`.

**Когда активировать:** сначала за флагом `EnableBatchedParallel=true`,
default `false` для backward compat. После стабилизации — default `true`
для `n_parallel > 1`.

**Single-call latency trade-off:** first-tokens-latency +5-10ms (batching
window), throughput +50-200% (shared matmul). Это WIN для сценариев "несколько
одновременных чатов", и NEUTRAL/LOSS для "одиночные запросы". Решается
default off + per-model opt-in.

## Backward compat matrix

| Сценарий | Round 13 (v0.4.11) | Round 15 (v0.5.0) |
|---|---|---|
| `n_parallel=1`, single call | single-seq (1.0x latency) | **single-seq (same)** |
| `n_parallel=2..8`, `EnableBatched=false` | multi-slot serialized (1.0x throughput) | **multi-slot serialized (same)** |
| `n_parallel=2..8`, `EnableBatched=true` | n/a (feature missing) | **batched (1.5-2.0x throughput)** |
| Existing 1-2 одновременных chat | 1.0x latency | **+5-10ms latency, 1.0x throughput** (default off) |
| 4+ одновременных chat | 0.25x throughput (1/n) | **0.5-0.75x throughput (1/n, but shared matmul)** |

## План реализации (по шагам)

### #3.1 Design doc ← ВЫ ЗДЕСЬ
- [x] Round 15 architecture (C-bridge + Go scheduler + backend)
- [x] Backward compat matrix
- [x] Trade-off analysis (latency vs throughput)

### #3.2 C-bridge foundation
- [ ] `build_batched_batch(chunks, n_chunks, max_tokens)` — multi-seq version
- [ ] `bridge_batched_decode(im, chunks, n_chunks, logits_out, n_logits_per_seq)` — public API
- [ ] `c/bridge/tests/test_batch_n_tokens.c` — add multi-seq test case
- [ ] Docker `bridge-tests` stage + CI `test-c-bridge` job — already in place
- [ ] Backward compat: `build_batch_with_seq` остаётся для legacy single-seq

### #3.3 Go BatchedScheduler
- [ ] `internal/cppbackend/batched_scheduler.go` (~300 строк)
- [ ] `internal/cppbackend/batched_scheduler_test.go` — unit tests:
  - 2 sessions finish in same wall-time (parallel)
  - 4 sessions finish in <2x single-session time
  - EOG token closes session
  - ctx.Done() cancels all sessions
- [ ] Per-session sampler state — copy from `im->sampler` при регистрации
  (у каждого session своя, не общий)

### #3.4 Backend integration
- [ ] `Backend.InferStream` → `batchedInferStream` при `EnableBatched=true`
- [ ] `LoadModelWithOpts` создаёт BatchedScheduler per-instance
- [ ] LlamaCppConfig field: `EnableBatchedParallel bool`
- [ ] WebUI: toggle "Batched parallel (Round 15, experimental)" в Per-Backend form
- [ ] i18n keys: 2-3 новых ("EnableBatched", "Batched experimental", "Throughput vs latency")

### #3.5 Integration tests
- [ ] 4 параллельных `/v1/chat/completions` к gemma-4 (200 tokens each)
- [ ] Измерить wall-time vs sequential (target: <60% от sequential)
- [ ] Cline-style SSE streaming — каждый session получает свой поток
- [ ] Backward compat: 1 чат + `EnableBatched=true` — работает (1.0x latency)

### #3.6 CHANGELOG + tag v0.5.0
- [ ] CHANGELOG entry: Round 15 (batched parallel, opt-in)
- [ ] Tag v0.5.0 (после Round 14 → major bump, experimental feature)

## Риски и unknowns

**R1: GPU OOM при n_parallel=4 на Qwen3-4B.**
- KV-cache per sequence: 4 * 32 layers * 4 heads * 128 dim * n_ctx * 2 bytes (fp16) = ...
- Для 4K context: ~16 MB per sequence, 64 MB total. Trivial.
- Для 32K context: ~128 MB per sequence, 512 MB total. Fine на 22 GB.
- Mitigation: документировать memory math, warn UI если OOM.

**R2: Cadence skew — один session сильно отстаёт от других.**
- Prompt ingestion: новый session обрабатывает prompt на одном tick,
  догоняет batch на следующем. Latency = 1 batching window.
- Generation: все sessions синхронны (1 token per tick). EOG обрабатывается
  on-the-fly — session завершается, остальные продолжают.
- Если EOG в одном session происходит посередине batch — другие sessions
  получают свои logits нормально (разные i_batch index).

**R3: Batching window latency vs throughput trade-off.**
- 5ms window: latency cost +5ms, throughput ~1.5x для 2 sessions
- 10ms window: latency cost +10ms, throughput ~1.8x для 2 sessions
- Default: **5ms** (configurable через `LlamaCppConfig.BatchedWindowMs`)

**R4: API change для `LlamaCppConfig`.**
- `EnableBatchedParallel bool` — новое поле, default `false` (backward compat)
- `BatchedWindowMs int` — новое поле, default 5 (range 1-50)
- Сторонние клиенты НЕ ломаются (новые поля optional)

**R5: Cancellation.**
- ctx.Done() в Go прерывает session token reads → session помечается finished
- llama_decode на cancel mid-batch → tokens для cancelled sessions
  игнорируются, остальные дочитываются
- KV-cache для cancelled seq_id чистится через `llama_memory_seq_rm`

## Что НЕ входит в Round 15

- ❌ **Round 16 (crash recovery)** — отдельный раунд, не блокирует.
- ❌ **Persistent sessions** — `llama_state_save/load` для cross-restart.
- ❌ **Speculative decoding** — Round 17+ (потенциально 2-3x speedup
  per-sequence, но orthogonal к batching).
- ❌ **Auto-tuning batching window** — статический 5ms default, dynamic
  tuning в Round 18+.

## Метрики успеха

- [ ] 4 параллельных чата к gemma-4-Q4_K_M: < 60% wall-time от sequential
- [ ] Cline streaming работает для каждого session независимо
- [ ] Single-call latency overhead < 10ms при `EnableBatched=true`
- [ ] 0 SIGABRT / OOM за 100 integration tests с n_parallel=4
- [ ] Backward compat: Round 13 path работает без изменений при
      `EnableBatched=false` (default)

## Reference: llama.cpp parallel.cpp pattern

```cpp
// c/llama.cpp/examples/parallel/parallel.cpp:288
while (true) {
    common_batch_clear(batch);

    // Decode ongoing sequences (last sampled token)
    for (auto & client : clients) {
        if (client.seq_id == -1) continue;
        client.i_batch = batch.n_tokens;
        common_batch_add(batch, client.sampled, client.n_past++, { client.id + 1 }, true);
    }

    // Insert new sequences (prefill their prompts)
    for (auto & client : clients) {
        if (client.seq_id == -1 && g_seq_id < n_seq) {
            for (size_t i = 0; i < tokens_prompt.size(); ++i) {
                common_batch_add(batch, tokens_prompt[i], client.n_past++, { client.id + 1 }, false);
            }
        }
    }

    if (batch.n_tokens == 0) break;

    // Process in chunks of params.n_batch
    for (int32_t i = 0; i < batch.n_tokens; i = i_next) {
        llama_batch batch_view = { n_tokens, ... }; // view into batch
        llama_decode(ctx, batch_view);
        // Per-client sampling via client.i_batch
        for (auto & client : clients) {
            const llama_token id = common_sampler_sample(client.smpl, ctx, client.i_batch - i);
            client.response += common_token_to_piece(ctx, id);
        }
    }
}
```

**Ключевая идея:** ВСЕ клиенты декодятся синхронно в ОДНОМ `llama_decode`.
Каждый клиент знает свой `i_batch` индекс → может сэмплить СВОИ logits.

## Связанные документы

- Round 13 (v0.4.11): `CHANGELOG.md` (slot manager + state isolation)
- Round 14 (v0.4.12): `CHANGELOG.md` (native enable_thinking + batch.n_tokens fix)
- llama.cpp example: `c/llama.cpp/examples/parallel/parallel.cpp`
- SlotManager: `internal/cppbackend/slot_manager.go`
- Regression test: `c/bridge/tests/test_batch_n_tokens.c`
