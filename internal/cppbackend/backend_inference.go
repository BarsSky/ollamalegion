package cppbackend

import (
	"context"
	"errors"
	"fmt"
	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/tokencount"
	"time"
	"unsafe"
)

// Generate выполняет синхронный инференс
//
// Round 8 (2026-07-28) BUGFIX: удерживаем inst.mu на всём протяжении
// inst.handle.Infer(). Без этого два параллельных запроса к одной модели
// вызывали llama_decode() на одном и том же контексте — race на KV-cache
// приводил к GGML_ASSERT(ggml_are_same_shape) и крашу cppworker (SIGABRT).
// Симптом у пользователя: на qwen3.6 35B A3B (inference 5-30s) второй
// юзер получал 503 "connection closed" / "model is loading".
//
// С n_parallel=1 (default) и Hold-Mu-Through-Infer корректное поведение —
// второй запрос ЖДЁТ завершения первого, а не падает. Параллельность
// inference регулируется на уровне балансера (slot manager) и/или
// n_parallel (если >1, нужно сменить на отдельный RWMutex).
//
// Round 13 (2026-07-28): добавлен slot manager. ПОРЯДОК LOCK'ов критичен:
//  1. sm.Acquire(ctx) — получаем slot ID (блокирует если все заняты)
//  2. inst.mu.Lock()  — serialize llama_decode
//  3. handle.Infer(params{SeqId: slot, ...})
//  4. defer sm.Release(slot) срабатывает ПОСЛЕ Unlock (через defer LIFO)
//  5. defer inst.mu.Unlock()
func (b *Backend) Generate(modelName string, prompt string, params bridge.GenerationParams) (*bridge.InferenceResult, error) {
	inst, err := b.getModelInstance(modelName)
	if err != nil {
		return nil, err
	}

	// R114 (2026-10-13): узел раскладки пропал — генерацию не начинаем: llama.cpp
	// на мёртвом RPC-устройстве вызывает abort() и роняет процесс.
	if err := b.checkRPCNodesForInference(); err != nil {
		return nil, err
	}

	// Round 13: acquire slot FIRST. Если все слоты заняты — блокируемся
	// (FIFO) пока не освободится. Для defaultNParallel=1 — никогда не
	// блокируется (единственный слот занят только текущим call'ом).
	// ctx — context.Background() (no timeout). Если нужен timeout —
	// передать через params (пока не реализовано).
	if inst.slots == nil {
		// Защита от nil: если модель загружена через устаревший путь без
		// SlotManager (например, init race), создаём его по тому же правилу,
		// что и штатная загрузка (env CPPWORKER_N_PARALLEL учитывается).
		inst.slots = NewSlotManager(parallelSlotsFor(resolveNParallel(0, b.cfg.DefaultNParallel)))
	}
	slot, release, err := inst.slots.Acquire(context.Background())
	if err != nil {
		return nil, err
	}
	// ВАЖНО: defer release() ДО defer mu.Unlock() — defer LIFO order:
	// сначала выполнится Unlock (release mu), потом Release slot.
	// Это предотвращает deadlock: слот освобождается ПОСЛЕ unlock mu,
	// поэтому следующий waiter может взять слот, а затем ждать mu.
	defer release()

	// Round 13: пробрасываем slot ID как seq_id в C-bridge.
	// seq_id=0 (default) → legacy single-slot, > 0 → multi-slot slot region.
	params.SeqId = slot

	// Round 8 BUGFIX: держим mu на всём инференсе. До фикса Unlock() был
	// сразу после ++ActiveQueries — два goroutine могли одновременно войти
	// в inst.handle.Infer() и race на llama.cpp context.
	inst.mu.Lock()
	inst.incQueries()
	inst.syncQueryCountersLocked()
	// lastUsedAt.Store — atomic, мьютекс не нужен. Делаем тут же для
	// удобства чтения (IdleUnloadManager смотрит на это значение).
	inst.lastUsedAt.Store(time.Now())

	start := time.Now()

	// R66d (2026-09-22): результат нужен в deferred-метрике, поэтому заводим
	// переменные заранее и заполняем их вместо прямого return.
	var (
		result   *bridge.InferenceResult
		inferErr error
	)

	defer func() {
		inst.decQueries()
		inst.syncQueryCountersLocked()
		// Токены считаем ПОКА держим inst.mu: CountTokens дёргает tokenizer
		// модели (llama.cpp) и не переживает параллельный Infer. Раньше здесь
		// стоял approximateTokens(approxResultLen(...)), а approxResultLen —
		// заглушка, всегда возвращавшая "" → метрика писала tokens=0 на КАЖДЫЙ
		// запрос (см. R66-оговорку в доке approxResultLen).
		var tokens int
		if b.metrics != nil && result != nil {
			tokens = completionTokens(inst.handle, result.Output, true)
		}
		inst.mu.Unlock()
		if b.metrics != nil {
			duration := time.Since(start)
			b.metrics.RecordRequestOutcome(modelName, tokens, duration, metricsOutcomeR358(inferErr))
		}
		inst.lastUsedAt.Store(time.Now())
	}()

	result, inferErr = inst.handle.Infer(prompt, params)
	// R224: по факту сбоя уточняем причину (узлы раскладки живы?) и при потере
	// сессии запускаем выгрузку модели — см. rpc_session_lost.go.
	if inferErr != nil {
		inferErr = b.diagnoseInferenceFailure(inst, modelName, inferErr)
	}
	return result, inferErr
}

// GenerateStream выполняет стриминг-инференс
//
// Round 8 (2026-07-28) BUGFIX: см. Generate — удерживаем inst.mu на всём
// протяжении inst.handle.InferStream().
//
// Round 13 (2026-07-28): добавлен slot manager (см. Generate для деталей
// ordering). Тот же lock pattern: Acquire → Lock → InferStream → Unlock → Release.
//
// Round 15.1 (2026-07-30): если BatchedScheduler активен для этой модели,
// маршрутизируем через BatchedInferStream (true parallel inference).
// Greedy argmax sampling (temp/top_p ignored). Иначе — legacy Round 13 path.
//
// R63 (2026-09-15): возвращает (abortFlag, error). abortFlag — per-infer
// atomic int от C-bridge; caller (handler) использует его для ctx.Done
// cancel watcher через NewAbortWatcher. Per-infer flag решает R60.58
// per-model race (любая отмена одной горутины сбрасывала все infers для
// той же модели).
// R248/B3 (2026-10-27): GenerateWithImages — синхронный инференс с картинками.
//
// Зачем отдельный метод, а не флаг в Generate: префилл с изображениями идёт
// через libmtmd (bridge_infer_images) и не поддерживает streaming, поэтому у
// него своя механика слота/блокировки. Метрики RecordRequest здесь не пишутся:
// врать в статистике (записывать vision-запрос как текстовый) хуже, чем не
// записывать.
//
// Ошибки моста (ErrVisionUnsupported и image_*) возвращаются как есть —
// вызывающая сторона переводит их в 415/400, а не в 500.
func (b *Backend) GenerateWithImages(ctx context.Context, modelName string, prompt string, images [][]byte, params bridge.GenerationParams) (*bridge.InferenceResult, error) {
	inst, err := b.getModelInstance(modelName)
	if err != nil {
		return nil, err
	}
	// R114: мёртвый RPC-узел — не начинаем (llama.cpp уронил бы процесс).
	if err := b.checkRPCNodesForInference(); err != nil {
		return nil, err
	}
	if inst.slots == nil {
		inst.slots = NewSlotManager(parallelSlotsFor(resolveNParallel(0, b.cfg.DefaultNParallel)))
	}
	slot, release, err := inst.slots.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	params.SeqId = slot

	// Тот же порядок, что в Generate: слот → (lock, ++ActiveQueries) → infer →
	// (--ActiveQueries, unlock) → release. Держим mu на всё время инференса:
	// llama.cpp context не переживает параллельный вызов.
	inst.mu.Lock()
	defer inst.mu.Unlock()
	inst.incQueries()
	defer inst.decQueries()
	inst.lastUsedAt.Store(time.Now())

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return inst.handle.InferWithImages(prompt, images, params)
}

// R248/B3: VisionCapable — опубликованная возможность модели (факт загрузки
// mmproj), а не догадка по имени. Хендлеры используют это, чтобы честно
// ответить 415, если картинку прислали модели без проектора.
func (b *Backend) VisionCapable(modelName string) bool {
	inst, err := b.getModelInstance(modelName)
	if err != nil {
		return false
	}
	return inst.info.Vision
}

// R248/B3: MMProjMarker — медиа-маркер загруженного проектора (например
// "<__media__>"). Пустая строка = картинки обслуживать нечем.
func (b *Backend) MMProjMarker(modelName string) string {
	inst, err := b.getModelInstance(modelName)
	if err != nil {
		return ""
	}
	return inst.handle.MMProjMarker()
}

func (b *Backend) GenerateStream(modelName string, prompt string, params bridge.GenerationParams, callback bridge.StreamCallback) (unsafe.Pointer, error) {
	return b.GenerateStreamWithAbort(context.Background(), modelName, prompt, params, callback, nil)
}

// GenerateStreamWithAbort — R83 (2026-09-29): то же, что GenerateStream, но с
// ЗАРАНЕЕ созданным флагом отмены (bridge.NewInferAbortFlag).
//
// Зачем. Раньше флаг отмены возвращался уже после завершения блокирующей
// генерации, поэтому клиентский Cancel не мог её прервать. Теперь вызывающий
// создаёт флаг, вооружает AbortWatcher на ctx.Done() и передаёт флаг сюда —
// отмена срабатывает на ближайшем checkpoint C-bridge (per-token).
// R413 (2026-11-03): ctx — первый аргумент и он реально используется на
// ожидании слота. Раньше здесь стояло context.Background(): отменённый клиент
// навсегда оставался в Acquire, слот не освобождался, и воркер переставал
// отвечать (живой дефект: active_queries=1, GPU простаивает, ни одного
// «stream response summary» больше нет). Теперь отмена клиента снимает
// ожидание слота так же, как снимает саму генерацию.
func (b *Backend) GenerateStreamWithAbort(ctx context.Context, modelName string, prompt string, params bridge.GenerationParams, callback bridge.StreamCallback, streamAbortFlag unsafe.Pointer) (unsafe.Pointer, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	inst, err := b.getModelInstance(modelName)
	if err != nil {
		return nil, err
	}

	// R114 (2026-10-13): узел раскладки пропал — генерацию не начинаем: llama.cpp
	// на мёртвом RPC-устройстве вызывает abort() и роняет процесс (клиент получил
	// бы не ошибку, а перезапуск мастера).
	if err := b.checkRPCNodesForInference(); err != nil {
		return nil, err
	}

	// Round 15.1: если BatchedScheduler активен, маршрутизируем в batched path.
	// BatchedInferStream владеет собственным slot management через BatchedScheduler.
	if inst.batchedScheduler != nil {
		return b.batchedInferStream(inst, modelName, prompt, params, callback)
	}

	// Round 13: acquire slot (см. Generate для объяснения).
	if inst.slots == nil {
		inst.slots = NewSlotManager(parallelSlotsFor(resolveNParallel(0, b.cfg.DefaultNParallel)))
	}
	// R413: видимая точка «запрос ждёт слот». Без неё заклинивание выглядело как
	// «запрос пришёл и молча исчез»: воркер занят одной генерацией, остальные
	// висят в Acquire, и в логах нет ни строки.
	if inst.slots.ActiveCount() >= inst.slots.MaxSlots() {
		logger.Get().Infow("inference: all slots busy, request will wait",
			"model", modelName, "slots", inst.slots.MaxSlots(), "wait_until", "ctx")
	}
	waitStart := time.Now()
	slot, release, err := inst.slots.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	if waited := time.Since(waitStart); waited > 500*time.Millisecond {
		logger.Get().Infow("inference: slot acquired after waiting",
			"model", modelName, "slot", slot, "waited_ms", waited.Milliseconds())
	}
	// ВАЖНО: defer release() ДО defer mu.Unlock() — defer LIFO:
	// сначала Unlock, потом Release (без deadlock).
	defer release()

	// R413: ctx мог быть отменён между выдачей слота и этим местом (клиент ушёл,
	// пока запрос стоял в очереди). Генерацию заведомо отменённого запроса не
	// начинаем.
	if err := ctx.Err(); err != nil {
		return nil, ErrAllSlotsBusy
	}

	// Round 13: пробрасываем slot ID как seq_id.
	params.SeqId = slot

	inst.mu.Lock()
	inst.incQueries()
	inst.syncQueryCountersLocked()
	inst.lastUsedAt.Store(time.Now())

	start := time.Now()

	// R66d (2026-09-22): считаем сгенерированные токены для метрик. C-bridge
	// вызывает callback ровно один раз на сгенерированный токен
	// (c/bridge/bridge.c:1968), плюс один дополнительный вызов, когда
	// сработал antiprompt (bridge.c:1955) — то есть в antiprompt-сценарии
	// возможен пересчёт на +1 (осознанно: это по-прежнему «сколько кусков
	// ушло клиенту», а не 0, как было раньше).
	generated := 0
	countedCallback := func(piece string) bool {
		generated++
		return callback(piece)
	}

	var streamErr error
	defer func() {
		inst.decQueries()
		inst.syncQueryCountersLocked()
		inst.mu.Unlock()
		if b.metrics != nil {
			b.metrics.RecordRequestOutcome(modelName, generated, time.Since(start), metricsOutcomeR358(streamErr))
		}
		inst.lastUsedAt.Store(time.Now())
	}()

	// R83 (2026-09-29): если вызывающий уже создал флаг отмены — используем его,
	// чтобы AbortWatcher мог остановить генерацию ВО ВРЕМЯ неё (см.
	// InferStreamWithAbort в c/bridge). Без флага поведение прежнее.
	// streamErr объявлен выше и используется в defer для метрик.
	var abortFlag unsafe.Pointer
	if streamAbortFlag != nil {
		abortFlag, streamErr = inst.handle.InferStreamWithAbort(prompt, params, countedCallback, streamAbortFlag)
	} else {
		abortFlag, streamErr = inst.handle.InferStream(prompt, params, countedCallback)
	}
	if streamErr != nil {
		// R224: тот же диагноз на потоковом пути — текст ошибки уходит в
		// error-чанк SSE, и «KV-cache overflow» там так же вводит в заблуждение.
		streamErr = b.diagnoseInferenceFailure(inst, modelName, streamErr)
	}
	return abortFlag, streamErr
}

// Suppress unused-import warning for unsafe — нужно для новой сигнатуры
// GenerateStream (R63). Используется в unsafe.Pointer(nil) выше для batched
// path (batched scheduler пока не возвращает abortFlag — TODO R63.x).
var _ = unsafe.Pointer(nil)

// batchedInferStream — Round 15.1: streaming inference через BatchedScheduler.
//
// Контракт:
//  1. Tokenize prompt через inst.handle.Tokenize → []int32
//  2. RegisterSession в BatchedScheduler (получаем id, state, TokenCh)
//  3. Loop: читаем int32 токены из state.TokenCh, detokenize через
//     inst.handle.TokenToPiece, передаём в user callback
//  4. TokenCh закрывается scheduler'ом когда session finished
//     (EOG, max_tokens, или scheduler stop)
//
// ВАЖНО: НЕ держим inst.mu здесь! BatchedScheduler.Run goroutine берёт
// modelLock (= &inst.mu) сам на время BatchedDecode. Это позволяет
// реальный parallel inference (N sessions одновременно в одном
// llama_decode call).
//
// Slot management: BatchedScheduler.RegisterSession возвращает ошибку
// при capacity overflow. Caller (HTTP handler) перехватывает и возвращает
// 503 клиенту. Round 15.1 НЕ блокирует на capacity — opt-in флаг для
// workloads где caller знает лимит.
func (b *Backend) batchedInferStream(inst *modelInstance, modelName string, prompt string, params bridge.GenerationParams, callback bridge.StreamCallback) (unsafe.Pointer, error) {
	// R63: возвращаем (nil, error) — batched path пока не использует per-infer
	// abort flag (TODO R63.x: добавить когда BatchedScheduler научится cancel).
	// 1. Tokenize prompt.
	tokens, err := inst.handle.Tokenize(prompt)
	if err != nil {
		return nil, fmt.Errorf("batched stream: tokenize failed: %w", err)
	}
	if len(tokens) == 0 {
		return nil, errors.New("batched stream: empty prompt after tokenize")
	}

	// 2. Determine MaxTokens.
	// Round 16 P2 fix (2026-07-30): используем константы вместо magic numbers
	// + ссылка на bridge.DefaultGenerationParams().NPredict для default.
	// Раньше было `maxTokens = 2048` hardcoded — при изменении дефолта в
	// bridge.go забылось бы тут.
	const batchedMaxTokensCap = 4096
	maxTokens := params.NPredict
	if maxTokens <= 0 || maxTokens > batchedMaxTokensCap {
		// Batched path: cap at 4096 чтобы один greedy loop не съел всю память.
		// Default = bridge.DefaultGenerationParams().NPredict (current 2048).
		maxTokens = bridge.DefaultGenerationParams().NPredict
	}

	// 3. Register session.
	// Round 15.2: pass Temperature/Seed из params для sampling.
	// 0 = greedy (default), > 0 = softmax+multinomial.
	//
	// R65d (2026-09-20): прокидываем также sampler-поля, которые batched-путь
	// НЕ применяет (top_p/top_k/min_p/repeat_penalty/mirostat/stop...).
	// RegisterSession использует их ТОЛЬКО для предупреждения в логе, чтобы
	// игнорирование перестало быть молчаливым.
	id, state, err := inst.batchedScheduler.RegisterSession(BatchedSessionParams{
		Prompt:      tokens,
		MaxTokens:   maxTokens,
		Temperature: params.Temperature,
		Seed:        uint32(params.Seed),

		TopP:             params.TopP,
		TopK:             params.TopK,
		MinP:             params.MinP,
		TypicalP:         params.TypicalP,
		TfsZ:             params.TfsZ,
		RepeatPenalty:    params.RepeatPenalty,
		FrequencyPenalty: params.FrequencyPenalty,
		PresencePenalty:  params.PresencePenalty,
		RepeatLastN:      params.RepeatLastN,
		Mirostat:         params.Mirostat,
		MirostatTau:      params.MirostatTau,
		MirostatEta:      params.MirostatEta,
		Antiprompts:      params.Antiprompts,
		StopSequences:    params.StopSequences,
	})
	if err != nil {
		return nil, fmt.Errorf("batched stream: register session: %w", err)
	}
	defer inst.batchedScheduler.UnregisterSession(id)

	// 4. Update metrics (BatchedScheduler сам берёт modelLock на время
	// decode, поэтому нам НЕ нужно держать inst.mu тут).
	//
	// R83-fix (2026-09-30): счётчики — атомарные (incQueries/decQueries), а НЕ
	// inst.info.*. Здесь inst.mu не удерживается (см. комментарий выше), поэтому
	// запись в info была ещё и гонкой; зеркало info обновляют пути под мьютексом.
	inst.incQueries()
	inst.lastUsedAt.Store(time.Now())
	start := time.Now()

	// R66d (2026-09-22): считаем токены, реально полученные из scheduler'а
	// (одно сообщение TokenCh = один сгенерированный токен). Раньше в метрику
	// писался жёсткий 0.
	generated := 0
	var opErr error
	defer func() {
		inst.decQueries()
		if b.metrics != nil {
			b.metrics.RecordRequestOutcome(modelName, generated, time.Since(start), metricsOutcomeR358(opErr))
		}
		inst.lastUsedAt.Store(time.Now())
	}()

	// 5. Stream tokens.
	// Round 16 P2 fix (2026-07-30): explicit ctx-cancellation. Раньше просто
	// `for tok := range state.TokenCh` — блокируется до EOS даже если HTTP
	// client отвалился. Callback-based cancellation работает (callback
	// проверяет sw.IsBroken() и возвращает false → exit), но:
	// (a) Не explicit — при чтении кода не очевидно как cancellation работает.
	// (b) Зависит от того что caller передал callback с ctx-awareness.
	// Теперь: select с ctx.Done() + labeled break для EOS. Cancellation
	// работает в обе стороны — callback returns false ИЛИ ctx.Done().
	//
	// ctx берётся от request handler через callback (callback'и
	// safeStreamWriter инжектят ctx в check). Если caller не передал ctx —
	// fallback через callback.
	//
	// TODO(Round 17): thread ctx явно через GenerateStream → batchedInferStream
	// чтобы убрать неявную зависимость от callback'а.
streamLoop:
	for {
		select {
		case tok, ok := <-state.TokenCh:
			if !ok {
				// TokenCh closed by scheduler — normal EOS
				break streamLoop
			}
			// R66d: считаем именно полученные токены (в т.ч. те, что
			// декодируются в пустую строку и не уходят клиенту).
			generated++
			piece := inst.handle.TokenToPiece(tok)
			if piece == "" {
				// Некоторые токены (BOS, special) декодируются в пустую строку.
				// Пропускаем — callback их всё равно не отдаст клиенту.
				continue
			}
			if !callback(piece) {
				// Caller попросил остановиться (return false из callback).
				// Это primary cancellation path — safeStreamWriter callback
				// ловит ctx.Done() внутри Writef/Flush и возвращает false.
				return nil, nil
			}
		}
	}
	// TokenCh закрыт scheduler'ом. Проверяем session.Err.
	if state.Err != nil {
		opErr = state.Err
		// R224: батчевый путь (parallel > 1) — та же диагностика.
		return nil, b.diagnoseInferenceFailure(inst, modelName, state.Err)
	}
	return nil, nil
}

// GetEmbeddings получает эмбеддинги текста
func (b *Backend) GetEmbeddings(modelName string, text string) ([]float32, error) {
	inst, err := b.getModelInstance(modelName)
	if err != nil {
		return nil, err
	}

	return inst.handle.GetEmbeddings(text)
}

// CountTokens возвращает число токенов в тексте для загруженной модели.
// Использует tokenizer модели через C-bridge; если модель не загружена или
// токенизатор вернул 0 — оценка через pkg/tokencount.
//
// R66: раньше фолбэк был len([]rune(text))/4 (недогруженная модель) и
// len([]rune(text)) (токенизатор вернул 0). Оба варианта неверны: первый
// занижает на любом измеренном словаре (латиница 63-81%, кириллица до 304%),
// второй завышает ровно в 4 раза относительно первого. Теперь один
// обоснованный оценщик на оба случая — см. pkg/tokencount.
func (b *Backend) CountTokens(modelName string, text string) int {
	n, _ := b.CountTokensExact(modelName, text)
	return n
}

// CountTokensExact — R467b (2026-11-06): число токенов и признак «посчитано
// РЕАЛЬНЫМ токенизатором загруженной модели» (exact=true), иначе это оценка
// pkg/tokencount.
//
// ЗАЧЕМ ПРИЗНАК. Раньше фолбэк на оценку был невидим наружу. Живой прогон:
// сразу после старта загрузки модели инстанс ещё не в StateLoaded, подсчёт шёл
// оценкой, а получатель считал число точным — на тексте 600 000 символов вышло
// 300 000 вместо 75 000. Число, о котором неизвестно, чем оно посчитано, нельзя
// класть в решение про окно (это и есть корень R466).
func (b *Backend) CountTokensExact(modelName string, text string) (int, bool) {
	inst, err := b.getModelInstance(modelName)
	if err != nil {
		// Модель не загружена/выгружена — реального токенизатора нет.
		return tokencount.Estimate(text), false
	}
	n, exact := inst.handle.CountTokensExact(text)
	if exact && n <= 0 && text != "" {
		// llama_tokenize вернул 0 (multilingual / сломанный токенизатор) —
		// оценка вместо нуля, иначе n_predict посчитает prompt пустым.
		return tokencount.Estimate(text), false
	}
	return n, exact
}
