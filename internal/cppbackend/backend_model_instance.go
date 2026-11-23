package cppbackend

import (
	"context"
	"ollama-loadbalancer/c/bridge"
	"sync"
	"sync/atomic"
)

// modelInstance — экземпляр загруженной модели
type modelInstance struct {
	info     ModelInfo
	handle   *bridge.ModelHandle
	metadata *bridge.ModelMetadata
	mu       sync.Mutex
	// tokenMu — R714 (D2, 2026-11-25): замок ЖИЗНИ СЛОВАРЯ, отдельный от inst.mu.
	//
	// ЗАЧЕМ ОТДЕЛЬНЫЙ. inst.mu удерживается ВСЮ генерацию (см. Generate: «держим
	// mu на всём инференсе»), поэтому любой код, бравший его только чтобы
	// убедиться «модель загружена и handle жив», вставал в очередь за чужим
	// inference. Живой дефект: POST /api/tokenize отвечал 46.155 s там, где
	// соседний узел отдавал 45-47 мс, — балансер писал `exact prompt tokens
	// unavailable … timeout` и работал «по оценке».
	//
	// Токенизатору нужен только словарь модели (в C-мосте это im->vocab,
	// bridge_count_tokens) — он неизменен, пока модель загружена, и не зависит
	// от занятости слотов. Поэтому два разных вопроса получают два разных замка:
	//
	//	inst.mu     — «можно ли сейчас декодировать» (эксклюзивно, на всю генерацию);
	//	tokenMu     — «жив ли handle/словарь» (unload берёт на запись, токенизация — на чтение).
	//
	// ПОРЯДОК ЗАХВАТА: inst.mu → tokenMu (а также b.mu → tokenMu в публикации
	// загрузки). Обратного порядка нет нигде: читатель токенизации не берёт ни
	// inst.mu, ни b.mu, удерживая tokenMu, — поэтому дедлока быть не может.
	tokenMu sync.RWMutex
	// Round 13 (2026-07-28): slot manager для multi-slot batched inference.
	// maxSlots = max(1, inst.info.Parallel). При Parallel=1 (default) —
	// семантика идентична Round 8 (mu сериализует llama_decode, slot
	// acquisition не блокирует). При Parallel>1 — до N concurrent calls
	// на разных seq_id, каждый держит свой регион KV-cache.
	// Инициализируется в LoadModelWithOpts после успешной загрузки.
	slots *SlotManager

	// Round 15.1 (2026-07-30): BatchedScheduler для true parallel inference.
	// nil = не используется (default, Round 13 path). Если не-nil — GenerateStream
	// маршрутизирует через RegisterSession + чтение из TokenCh (с per-token
	// detokenize через ModelHandle.TokenToPiece). Создаётся в LoadModelWithOpts
	// только если (opts.EnableBatchedParallel ?? cfg.EnableBatchedParallel) = true
	// И Parallel >= 2 (нужен реальный slot pool).
	batchedScheduler *BatchedScheduler
	// batchedSchedulerCancel — отмена Run() goroutine при UnloadModel.
	// cancel != nil означает scheduler активен; вызов cancel() останавливает
	// tick loop и закрывает все активные sessions с ошибкой.
	batchedSchedulerCancel context.CancelFunc

	// Round 17 (2026-07-31): reasoningEnabled — resolved в LoadModelWithOpts
	// из opts.EnableReasoning ?? cfg.DefaultEnableReasoning. Используется
	// парсером (SplitReasoningContent / ReasoningStreamState) чтобы split'ить
	// <think> блоки для моделей ВНЕ IsReasoningModel() whitelist (например,
	// qwen3-instruct с включённым SOFT prompt reasoning).
	//
	// Source of truth для "эта модель эмитит reasoning — нужно split'ить".
	// cfg.DefaultEnableReasoning и per-model override оба резолвятся сюда
	// ОДИН раз при LoadModel — нет рассинхрона между settings и parser.
	//
	// Связь: handler.responds.IsReasoningModel(name) || inst.reasoningEnabled.
	// Читается из handler через *(inst) или снимок в ModelInfo.
	reasoningEnabled bool

	// lastUsedAt — момент последнего обращения к модели (генерация/стрим).
	// Используется IdleUnloadManager'ом для решения о выгрузке неактивных
	// моделей: считаем idle от LastUsedAt, а не от LoadedAt — иначе модель,
	// которая периодически обслуживает запросы, будет выгружена после
	// idleTimeout от момента загрузки, а не от момента последнего использования.
	// atomic.Value позволяет читать без мьютекса на горячем пути ListModels.
	lastUsedAt atomic.Value // time.Time

	// activeQueries / totalQueries — R83-fix (2026-09-30): счётчики запросов
	// БЕЗ inst.mu.
	//
	// Зачем. inst.mu удерживается на ВСЁ время генерации (см. Generate:
	// "держим mu на всём инференсе"). Пока счётчик читался под тем же мьютексом
	// (getActiveQueries), КАЖДЫЙ читающий эндпоинт вставал в очередь за
	// генерацией: на живом стенде GET /api/info и GET /api/models висели
	// 2 м 51 с — ровно столько, сколько шёл запрос Cline с 16k prompt. Это
	// ломало сразу всё: балансер не мог обновить метрики (WebUI показывал
	// устаревшие «сколько памяти занято / какая модель активна»), агент не
	// мог отдать heartbeat, а /api/models/active-queries (busy-badge) врал.
	//
	// Живой лог (2026-09-30, gemma-4 на RTX 3070): двенадцать GET /api/info
	// с duration 2m51s/2m36s/…/6s, все завершились в одну миллисекунду с
	// окончанием генерации.
	//
	// Теперь счётчики — атомарные: читаются lock-free, пишутся в том же месте,
	// где и раньше (+ зеркало в info для снимков, см. syncQueryCountersLocked).
	activeQueries atomic.Int64
	totalQueries  atomic.Int64
}

// incQueries — +1 активный запрос (и +1 всего). Не требует inst.mu.
func (inst *modelInstance) incQueries() {
	inst.activeQueries.Add(1)
	inst.totalQueries.Add(1)
}

// decQueries — -1 активный запрос (всего не меняется). Не требует inst.mu.
func (inst *modelInstance) decQueries() {
	inst.activeQueries.Add(-1)
}

// activeQueriesCount — текущее число активных запросов БЕЗ взятия inst.mu.
func (inst *modelInstance) activeQueriesCount() int {
	return int(inst.activeQueries.Load())
}

// totalQueriesCount — всего запросов к модели БЕЗ взятия inst.mu.
func (inst *modelInstance) totalQueriesCount() int64 {
	return inst.totalQueries.Load()
}

// syncQueryCountersLocked переносит атомарные счётчики в снимок info.
//
// Вызывать ТОЛЬКО удерживая inst.mu: info — обычные поля, их пишут и другие
// места (загрузка/выгрузка). Нужно для тестов и для кода, который читает
// inst.info напрямую; все внешние снимки (ListModels/GetModel/GetMetrics)
// берут значения из атомиков, а не из info.
func (inst *modelInstance) syncQueryCountersLocked() {
	inst.info.ActiveQueries = inst.activeQueriesCount()
	inst.info.TotalQueries = inst.totalQueriesCount()
}

// ============================================================
// Создание Backend
// ============================================================
