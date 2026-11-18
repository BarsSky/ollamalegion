package cppbackend

import (
	"time"
)

// LoadModelOpts — опции загрузки модели, передаваемые из WebUI/API
// через endpoint /api/models/load-with-params (cppworker).
//
// Базовые параметры (GPULayers, ContextSize и т.д.) использовались ещё
// в /api/models/load (legacy endpoint). Расширенные параметры ниже
// добавлены для тонкой настройки llama.cpp под конкретный workload:
//
//   - NThreads        — количество CPU-потоков для batch/generation
//     (по умолчанию физические ядра).
//   - Parallel         — число параллельных sequences (batched generation).
//     Слоты ДЕЛЯТ окно контекста (llama.cpp: n_ctx_seq = n_ctx / n_seq_max,
//     kv_unified=false), а не умножают KV-cache: 8192 + parallel=2 → по 4096
//     на клиента, суммарный KV остаётся как при 8192. Окно на клиента задаётся
//     ContextPerSeq.
//   - KVCacheType      — тип KV-cache quantization (0=F16, 1=Q8_0, 2=Q4_0).
//     Q8_0 экономит ~50% KV-cache VRAM с минимальной
//     потерей качества (perplexity delta < 0.1).
//   - SplitMode        — режим tensor split для multi-GPU (0=layer, 1=row).
//   - OverrideTensor   — переопределение dtype отдельных тензоров
//     (например "blk\\..*\\.ffn_.*_exps=CPU").
//
// LoadTimeRecord — запись о реальном времени загрузки модели.
// Round 25 (2026-08-04): используется в cppworker для динамической оценки
// estimateLoadTimeMs (compute avgSpeedBytesPerSec на основе последних
// N=20 успешных load'ов).
type LoadTimeRecord struct {
	SizeBytes   int64     // размер .gguf файла
	ContextSize int       // n_ctx (для KV-cache init factor)
	DurationMs  int64     // реальное время от LoadModelWithOpts до возврата
	Timestamp   time.Time // когда load завершился (для TTL)
}

// loadHistoryMaxSize — сколько последних записей хранить.
// 20 записей × ~50B = 1KB памяти. Достаточно для статистики и не растёт
// unbounded при долгой работе сервера.
const loadHistoryMaxSize = 20

// loadHistoryMaxAge — максимальный возраст записи. Старые записи (после
// GPU upgrade / disk defrag) могут быть misleading — игнорируем через 7 дней.
const loadHistoryMaxAge = 7 * 24 * time.Hour

// RecordLoadTime добавляет запись в историю. Вызывается при успешном
// load'е. Thread-safe (lock на write).
func (b *Backend) RecordLoadTime(record LoadTimeRecord) {
	b.loadHistoryMu.Lock()
	defer b.loadHistoryMu.Unlock()
	b.loadHistory = append(b.loadHistory, record)
	// Trim to max size (FIFO).
	if len(b.loadHistory) > loadHistoryMaxSize {
		// Drop oldest entries.
		b.loadHistory = b.loadHistory[len(b.loadHistory)-loadHistoryMaxSize:]
	}
}

// GetLoadHistory возвращает копию истории. Thread-safe (RLock).
// Caller не должен мутировать результат.
func (b *Backend) GetLoadHistory() []LoadTimeRecord {
	b.loadHistoryMu.RLock()
	defer b.loadHistoryMu.RUnlock()
	out := make([]LoadTimeRecord, len(b.loadHistory))
	copy(out, b.loadHistory)
	return out
}

// EstimatedBytesPerSec вычисляет среднюю скорость загрузки на основе
// истории. Используется в cppworker'е для динамической оценки.
//
// Стратегия: weighted average по records, weight ∝ 1/size_diff (closer
// size = higher weight). Возвращает 0 если история пустая.
//
// Round 25: gemma-4 5GB реально грузится 60-90s, что даёт ~55-83 MB/s.
// Хардкод 100MB/s в estimateLoadTimeMs ЗАНИЖАЕТ оценку для больших
// моделей → клиент думает "скоро загрузится" → 60-90s timeout.
// С measured cache estimate становится реалистичной.
func (b *Backend) EstimatedBytesPerSec() int64 {
	history := b.GetLoadHistory()
	if len(history) == 0 {
		return 0
	}
	// Фильтруем по age (отсекаем слишком старые записи).
	now := time.Now()
	var totalWeight float64
	var weightedSum float64
	for _, r := range history {
		if now.Sub(r.Timestamp) > loadHistoryMaxAge {
			continue
		}
		if r.SizeBytes <= 0 || r.DurationMs <= 0 {
			continue
		}
		// bytes per second
		bps := float64(r.SizeBytes) * 1000.0 / float64(r.DurationMs)
		// Weight: recent records (more recent = higher weight) AND
		// we want all records to count equally (simple average works
		// well for our use case; 5-20 records is enough for stable estimate).
		weight := 1.0
		totalWeight += weight
		weightedSum += bps * weight
	}
	if totalWeight == 0 {
		return 0
	}
	return int64(weightedSum / totalWeight)
}

// ResetLoadHistory очищает историю. Для тестов и admin endpoint'а.
func (b *Backend) ResetLoadHistory() {
	b.loadHistoryMu.Lock()
	defer b.loadHistoryMu.Unlock()
	b.loadHistory = nil
}

// Если значение 0/empty — используется llama.cpp default (без override).
type LoadModelOpts struct {
	GPULayers     int
	ContextSize   int
	BatchSize     int
	FlashAttnType int
	NUMA          bool
	UseMmap       bool
	TensorSplit   []float32

	// Extended parameters (для /api/models/load-with-params)
	NThreads       int    // 0 = auto (use physical cores)
	Parallel       int    // 0 = 1 (no batching)
	KVCacheType    string // "" = inherit (default F16), "f16"/"q8_0"/"q4_0" — explicit type
	SplitMode      int    // 0=layer, 1=row
	OverrideTensor string // legacy single-string override, empty = no override

	// ContextPerSeq — R83 (2026-09-30): окно контекста НА КЛИЕНТА (на слот).
	//
	// ЗАЧЕМ. В этой сборке llama.cpp (см. src/llama-context.cpp: при
	// kv_unified=false) считает n_ctx_seq = PAD256(n_ctx / n_seq_max), то есть
	// contextSize в API — СУММАРНОЕ окно, которое слоты ДЕЛЯТ между собой.
	// Оператор, выставив 16384 и parallel=2, получал по 8192 на клиента и не
	// видел этого нигде.
	//
	// С ContextPerSeq > 0 итоговое (суммарное) окно считается как
	// PAD256(ContextPerSeq) × slots, поэтому «16384 на клиента + 2 слота» даёт
	// ровно 16384 каждому (суммарно 32768 — и memfit проверяет именно 32768).
	// 0 = выключено, ContextSize трактуется как суммарное (прежнее поведение).
	ContextPerSeq int
	// Round 7: parallel arrays for per-tensor override-tensors.
	// Each entry is (regex-pattern, buft-name) where buft-name is one of
	//   "CPU", "CUDA0", "CUDA1", ...
	// For MoE: keep attention on GPU, expert tensors in RAM.
	OverrideTensors     []string
	OverrideTensorBufts []string

	// Round 15.1: per-model override для batched parallel inference.
	// - nil (default) → use cfg.EnableBatchedParallel (Backend global toggle)
	// - non-nil → explicit per-model choice (overrides cfg.EnableBatchedParallel)
	// CAVEAT: BatchedScheduler пока greedy argmax (no temp/top_p).
	EnableBatchedParallel *bool

	// R83 §3.2/§3.4 (2026-09-26): числа KV-кэша для C-оценки свободной VRAM.
	// Заполняются в checkVRAMForModel из метаданных GGUF (KVLayersFor /
	// KVHeadDimFor) и уходят в bridge.ModelConfig. 0 = неизвестно.
	// Нужны, чтобы C-оценка «сколько n_ctx влезает» считала те же слои и тот же
	// head_dim, что и план memfit: наивная формула завышала KV почти в 10 раз.
	KVLayers  int
	KVHeadDim int

	// Round 17 (2026-07-31): per-model override для reasoning parser routing.
	// Решает баг 2026-07-31 (см. docs/plans/bug-2026-07-31-reasoning-not-routed.md):
	// SOFT prompt injection работает для ЛЮБОЙ модели при cfg.EnableReasoning=true,
	// но парсер SplitReasoningContent срабатывал ТОЛЬКО для моделей из
	// IsReasoningModel() whitelist (qwen3.5, qwen3.6, deepseek-r1, ...).
	// Для qwen3-instruct (которой нет в whitelist) reasoning text попадал
	// в `content` вместо `reasoning_content` — OpenWebUI не показывал reasoning.
	//
	// С этим флагом:
	//   nil (default) → use cfg.DefaultEnableReasoning (для всех моделей)
	//   *true / *false → explicit per-model choice (override default)
	// После LoadModel этот флаг резолвится в modelInstance.reasoningEnabled.
	EnableReasoning *bool

	// R248/B3 (2026-10-27): путь к mmproj-проектору (vision). Пусто = модель
	// текстовая. Путь НЕ угадывается рядом с моделью: только явный запрос
	// оператора/профиля, иначе «нашёлся похожий файл» даёт ложную capability.
	MmprojPath string
}
