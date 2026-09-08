// proxy_timeout.go — Per-model adaptive timeout helpers (3-tier resolver) и
// AdvancedTiming helpers для Proxy. Вынесено из proxy.go для уменьшения файла.
package balancer

import (
	"os"
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// isStreamingNeverTimeout — ENV override для полного отключения streaming-таймаутов.
// R58.1 (2026-09-03): user pain — «балансер по жёстким таймаутам рубил
// работу клиентам, хотя по идее это не требуется так как если будет в
// этом необходимость пользователь сам передаст ответ через клиент по
// прекращению». Семантика: при LB_STREAMING_NEVER_TIMEOUT=1 balancer
// НЕ обрывает streaming-соединения по своему таймауту — ждёт cancel
// от клиента (r.Context().Done()) или EOF от backend.
//
// Принимает "1", "true", "yes" (case-insensitive) как enable.
// "0", "false", "no", "" → false.
func isStreamingNeverTimeout() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("LB_STREAMING_NEVER_TIMEOUT")))
	switch v {
	case "1", "true", "yes":
		return true
	}
	return false
}

// getEnvIdleTimeoutSec — R60.18 F1: helper для чтения LB_STREAMING_IDLE_TIMEOUT_SEC.
//
// До R60.18 этот env var был phantom — документирован в
// config/config.bundled.json:32 и упомянут в error message streaming.go:322/325,
// но НЕ читался кодом (silent no-op для оператора).
//
// Семантика: парсит целое число секунд из env. Возвращает (0, false) если:
//   - env пуст / невалидный / ноль
//   - LB_STREAMING_NEVER_TIMEOUT=1 (NEVER_TIMEOUT отключает всё — выше приоритет)
//
// Caller (getGlobalStreamingIdleTimeout, getModelStreamingIdleTimeout)
// использует (n, true) как break-glass override перед per-model profile и
// глобальным config (по симметрии с R60.17 LB_LLAMACPP_STREAM_TIMEOUT_SEC).
func getEnvIdleTimeoutSec() (int, bool) {
	if isStreamingNeverTimeout() {
		return 0, false
	}
	raw := strings.TrimSpace(os.Getenv("LB_STREAMING_IDLE_TIMEOUT_SEC"))
	if raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// maxConcurrentWarmups — пакетная функция для использования до создания Proxy.
func maxConcurrentWarmups(config *types.LoadBalancerConfig) int {
	n := config.Balancing.AdvancedTiming.MaxConcurrentWarmups
	if n <= 0 {
		n = 3
	}
	return n
}

// --- AdvancedTiming helpers (замена хардкодных магических чисел) ---

// getZombieSessionThreshold — порог зомби-сессий в секундах (default: 120).
func (p *Proxy) getZombieSessionThreshold() time.Duration {
	sec := p.config.Balancing.AdvancedTiming.ZombieSessionThresholdSec
	if sec <= 0 {
		sec = 120
	}
	return time.Duration(sec) * time.Second
}

// getStreamingRetryDelay — задержка перед retry streaming запроса (default: 500ms).
func (p *Proxy) getStreamingRetryDelay() time.Duration {
	ms := p.config.Balancing.AdvancedTiming.StreamingRetryDelayMs
	if ms <= 0 {
		ms = 500
	}
	return time.Duration(ms) * time.Millisecond
}

// getMaxConcurrentWarmups — макс. одновременных warmup (default: 3).
func (p *Proxy) getMaxConcurrentWarmups() int {
	n := p.config.Balancing.AdvancedTiming.MaxConcurrentWarmups
	if n <= 0 {
		n = 3
	}
	return n
}

// getHeartbeatInterval — интервал SSE heartbeat (default: 15s).
func (p *Proxy) getHeartbeatInterval() time.Duration {
	sec := p.config.Balancing.AdvancedTiming.HeartbeatIntervalSec
	if sec <= 0 {
		return 15 * time.Second
	}
	return time.Duration(sec) * time.Second
}

// getStreamingIdleTimeout — read-deadline для backend-стрима.
// Если в течение этого времени из бэкенда не приходит ни одного чанка, прокси
// считает, что бэкенд завис/SIGSEGV, и явно завершает стрим с ошибкой
// (вместо того, чтобы молча "завершать" по EOF).
// default: 120s, настройка — Balancing.StreamingIdleTimeout.
//
// R58.1: LB_STREAMING_NEVER_TIMEOUT=1 → 0 (= "no idle deadline").
func (p *Proxy) getStreamingIdleTimeout() time.Duration {
	if isStreamingNeverTimeout() {
		return 0
	}
	sec := p.config.Balancing.StreamingIdleTimeout
	if sec <= 0 {
		return 120 * time.Second
	}
	return time.Duration(sec) * time.Second
}

// --- Per-model adaptive timeout helpers (3-tier resolver) ---

// getModelStreamTimeout возвращает per-model общий таймаут стриминга.
// Приоритет:
//  1. Per-model profile (config.LlamaCppModelProfiles[modelName].StreamingTimeoutSec)
//  2. ModelLatencyTracker (автоматический расчёт на основе истории генерации)
//  3. Эвристика по размеру GGUF файла (для моделей без истории)
//  4. Глобальный config.Balancing.StreamTimeout
//  5. Дефолт 600 секунд
//
// R58.1 (2026-09-03): LB_STREAMING_NEVER_TIMEOUT=1 → 0 (= "no total stream timeout").
// Highest priority, overrides per-model profile and config.
//
// R60.18 F3 (2026-09-08): REMOVED LB_LLAMACPP_STREAM_TIMEOUT_SEC env override.
// Архитектурно total stream timeout — wrong abstraction для LLM streaming
// (см. docs/R60.18-env-flags-audit.md). Use:
//   - streamingIdleTimeout — hang detection (no chunks for N sec)
//   - firstByteTimeout — prefill hang (no first chunk for N sec)
//   - n_predict per-request/per-model — explicit response length cap
//
// Total stream timeout оставлен только как safety net для
// "process sending garbage chunks forever" (через ModelLatencyTracker).
func (p *Proxy) getModelStreamTimeout(modelName string) time.Duration {
	if isStreamingNeverTimeout() {
		return 0
	}

	if modelName == "" || p.modelLatencyTracker == nil {
		return p.getGlobalStreamTimeout()
	}

	// Tier 1: per-model profile
	profile, ok := p.GetModelProfile(modelName)
	if ok && profile.StreamingTimeoutSec > 0 {
		return time.Duration(profile.StreamingTimeoutSec) * time.Second
	}

	// Tier 2-3: ModelLatencyTracker + GGUF size heuristic
	globalTimeoutSec := p.config.Balancing.StreamTimeout
	if globalTimeoutSec <= 0 {
		globalTimeoutSec = 600
	}
	modelSize := p.getModelSizeBytes(modelName)
	return p.modelLatencyTracker.GetOrComputeTimeout(modelName, 0, globalTimeoutSec, modelSize)
}

// getModelStreamingIdleTimeout возвращает per-model idle-таймаут стриминга.
// Приоритет:
//  1. ENV override LB_STREAMING_IDLE_TIMEOUT_SEC (break-glass, R60.18 F1)
//  2. Per-model profile (config.LlamaCppModelProfiles[modelName].StreamingIdleTimeoutSec)
//  3. ModelLatencyTracker (автоматический расчёт на основе истории)
//  4. Эвристика по размеру GGUF файла (для моделей без истории)
//  5. Глобальный config.Balancing.StreamingIdleTimeout
//  6. Дефолт 120 секунд
//
// modelSizeBytes используется для tier-4 (эвристика по размеру). Для больших
// моделей на GPU с partial offload (не все слои в VRAM) idle между чанками
// может быть >120s → без эвристики стрим обрывался бы без диагностики.
//
// R58.1: LB_STREAMING_NEVER_TIMEOUT=1 → 0 (= "no idle deadline").
// R60.18 F1: LB_STREAMING_IDLE_TIMEOUT_SEC → break-glass override
// (по симметрии с LB_LLAMACPP_STREAM_TIMEOUT_SEC — env побеждает per-model
// profile для incident response, но НЕ для production defaults).
func (p *Proxy) getModelStreamingIdleTimeout(modelName string) time.Duration {
	if isStreamingNeverTimeout() {
		return 0
	}
	// R60.18 F1: env override (break-glass). Tier 1.
	if n, ok := getEnvIdleTimeoutSec(); ok {
		return time.Duration(n) * time.Second
	}
	if modelName == "" || p.modelLatencyTracker == nil {
		return p.getGlobalStreamingIdleTimeout()
	}

	// Tier 2: per-model profile
	profile, ok := p.GetModelProfile(modelName)
	if ok && profile.StreamingIdleTimeoutSec > 0 {
		return time.Duration(profile.StreamingIdleTimeoutSec) * time.Second
	}

	// Tier 3 + 4: ModelLatencyTracker (с modelSizeBytes для tier-4 fallback)
	globalIdleSec := p.config.Balancing.StreamingIdleTimeout
	if globalIdleSec <= 0 {
		globalIdleSec = 120
	}
	modelSize := p.getModelSizeBytes(modelName)
	return p.modelLatencyTracker.GetOrComputeIdleTimeout(modelName, 0, globalIdleSec, modelSize)
}

// getModelRequestTimeout возвращает per-model таймаут non-streaming запроса.
// Приоритет:
//  1. Per-model profile (config.LlamaCppModelProfiles[modelName].RequestTimeoutSec)
//  2. ModelLatencyTracker (автоматический расчёт)
//  3. Глобальный config.Balancing.RequestTimeout
//  4. Дефолт 120 секунд
//
// R58.1: LB_STREAMING_NEVER_TIMEOUT=1 → 0 (= "no request timeout").
func (p *Proxy) getModelRequestTimeout(modelName string) time.Duration {
	if isStreamingNeverTimeout() {
		return 0
	}
	if modelName == "" || p.modelLatencyTracker == nil {
		return p.getGlobalRequestTimeout()
	}

	// Tier 1: per-model profile
	profile, ok := p.GetModelProfile(modelName)
	if ok && profile.RequestTimeoutSec > 0 {
		return time.Duration(profile.RequestTimeoutSec) * time.Second
	}

	// Tier 2: ModelLatencyTracker
	globalReqSec := p.config.Balancing.RequestTimeout
	if globalReqSec <= 0 {
		globalReqSec = 120
	}
	return p.modelLatencyTracker.GetOrComputeRequestTimeout(modelName, 0, globalReqSec)
}

// getModelFirstByteTimeout возвращает per-model таймаут ожидания первого байта.
// Приоритет:
//  1. Per-model profile (config.LlamaCppModelProfiles[modelName].FirstByteTimeoutSec)
//  2. ModelLatencyTracker (автоматический расчёт на основе истории first-byte latency)
//  3. Эвристика по размеру GGUF файла (для моделей без истории)
//  4. Глобальный config.Balancing.FirstByteTimeout
//  5. Дефолт 120 секунд
//
// R58.1: LB_STREAMING_NEVER_TIMEOUT=1 → 0 (= "no first-byte timeout").
func (p *Proxy) getModelFirstByteTimeout(modelName string) time.Duration {
	if isStreamingNeverTimeout() {
		return 0
	}
	if modelName == "" || p.modelLatencyTracker == nil {
		return p.getGlobalFirstByteTimeout()
	}

	// Tier 1: per-model profile
	profile, ok := p.GetModelProfile(modelName)
	if ok && profile.FirstByteTimeoutSec > 0 {
		return time.Duration(profile.FirstByteTimeoutSec) * time.Second
	}

	// Tier 2-3: ModelLatencyTracker + GGUF size heuristic
	globalFbSec := p.config.Balancing.FirstByteTimeout
	if globalFbSec <= 0 {
		globalFbSec = 120
	}
	modelSize := p.getModelSizeBytes(modelName)
	return p.modelLatencyTracker.GetOrComputeFirstByteTimeout(modelName, 0, globalFbSec, modelSize)
}

// getGlobalStreamTimeout — глобальный таймаут стриминга из конфига (или дефолт 600s).
//
// R53.6 (2026-08-24): ENV override LB_LLAMACPP_STREAM_TIMEOUT_SEC. REMOVED R60.18 F3.
//
// R58.1 (2026-09-03): ENV override LB_STREAMING_NEVER_TIMEOUT=1
// отключает стриминг-таймаут полностью (returns 0). Caller (proxy_request.go)
// интерпретирует 0 как "skip context.WithTimeout".
//
// R60.18 F3: REMOVED LB_LLAMACPP_STREAM_TIMEOUT_SEC. Total stream timeout —
// wrong abstraction для LLM streaming (см. docs/R60.18-env-flags-audit.md).
// Используйте streamingIdleTimeout + firstByteTimeout + n_predict per-model.
func (p *Proxy) getGlobalStreamTimeout() time.Duration {
	if isStreamingNeverTimeout() {
		return 0
	}
	sec := p.config.Balancing.StreamTimeout
	if sec <= 0 {
		return 600 * time.Second
	}
	return time.Duration(sec) * time.Second
}

// getGlobalStreamingIdleTimeout — глобальный idle-таймаут стриминга (или дефолт 120s).
//
// R58.1: LB_STREAMING_NEVER_TIMEOUT=1 → 0 (= "no idle deadline").
// R60.18 F1: LB_STREAMING_IDLE_TIMEOUT_SEC — break-glass env override
// (документирован в config.bundled.json:32 и streaming.go error message;
// до R60.18 был phantom — silent no-op).
func (p *Proxy) getGlobalStreamingIdleTimeout() time.Duration {
	if isStreamingNeverTimeout() {
		return 0
	}
	if n, ok := getEnvIdleTimeoutSec(); ok {
		return time.Duration(n) * time.Second
	}
	sec := p.config.Balancing.StreamingIdleTimeout
	if sec <= 0 {
		return 120 * time.Second
	}
	return time.Duration(sec) * time.Second
}

// getGlobalRequestTimeout — глобальный таймаут non-streaming (или дефолт 120s).
//
// R58.1: LB_STREAMING_NEVER_TIMEOUT=1 → 0 (= "no request timeout").
func (p *Proxy) getGlobalRequestTimeout() time.Duration {
	if isStreamingNeverTimeout() {
		return 0
	}
	sec := p.config.Balancing.RequestTimeout
	if sec <= 0 {
		return 120 * time.Second
	}
	return time.Duration(sec) * time.Second
}

// getGlobalFirstByteTimeout — глобальный таймаут первого байта (или дефолт 900s).
//
// Round 42 (2026-08-19): bump default 120s → 900s для batched inference моделей.
// Cline на RTX 3070 с Qwen3.6-35B (22GB, 32K prompt prefill) требует 10-15 мин до
// первого токена. Старый default 120s приводил к 500 ошибке через 2 мин → Cline
// retry → restart loop. Новый default 900s = 15 мин покрывает:
//   - Q4_K_M 1-3B (быстрые): 10-30 сек prefill
//   - Q4_K_M 3-8B (средние): 1-3 мин prefill
//   - Q4_K_M 8-20B (большие): 3-7 мин prefill
//   - Q4_K_M 20-40B (MoE + partial offload): 10-15 мин prefill
//
// 15 мин = worst case + safety margin.
//
// R58.1: LB_STREAMING_NEVER_TIMEOUT=1 → 0 (= "no first-byte timeout").
func (p *Proxy) getGlobalFirstByteTimeout() time.Duration {
	if isStreamingNeverTimeout() {
		return 0
	}
	sec := p.config.Balancing.FirstByteTimeout
	if sec <= 0 {
		return 900 * time.Second
	}
	return time.Duration(sec) * time.Second
}

// getModelSizeBytes возвращает размер модели в байтах из per-model профиля
// или из метрик загруженных моделей на бэкендах.
// Приоритет:
//  1. Per-model profile (config.LlamaCppModelProfiles[modelName].SizeBytes)
//  2. LoadedModel size из llamaMetrics любого бэкенда (LlamaCppModel.Size)
//  3. 0 — если размер неизвестен
func (p *Proxy) getModelSizeBytes(modelName string) int64 {
	if modelName == "" {
		return 0
	}

	// Tier 1: per-model profile
	profile, ok := p.GetModelProfile(modelName)
	if ok && profile.SizeBytes > 0 {
		return profile.SizeBytes
	}

	// Tier 2: loadedModel size из llamaMetrics на любом бэкенде
	if p.metricsMgr != nil {
		p.metricsMgr.mu.RLock()
		for _, lm := range p.metricsMgr.llamaMetrics {
			if lm == nil {
				continue
			}
			for _, m := range lm.LoadedModels {
				if m.Name == modelName && m.Size > 0 {
					p.metricsMgr.mu.RUnlock()
					return int64(m.Size)
				}
			}
		}
		p.metricsMgr.mu.RUnlock()
	}

	return 0
}

// getWarmupSemaphoreTimeout — таймаут ожидания семафора warmup (default: 30s).
func (p *Proxy) getWarmupSemaphoreTimeout() time.Duration {
	sec := p.config.Balancing.AdvancedTiming.WarmupSemaphoreTimeoutSec
	if sec <= 0 {
		sec = 30
	}
	return time.Duration(sec) * time.Second
}
