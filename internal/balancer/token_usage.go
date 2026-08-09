// token_usage.go — Round 31 #7 (2026-08-09): per-model token usage tracking.
//
// cppworker возвращает OpenAI usage: {prompt_tokens, completion_tokens, total_tokens}.
// Ollama format: {prompt_eval_count, eval_count}.
// Балансер пробрасывает их в response клиенту (translateOpenAIChatToOllama и т.д.),
// но НЕ агрегирует для мониторинга. Этот файл добавляет:
//
//   1. ModelTokenUsage — struct для хранения per-model aggregates.
//   2. recordTokenUsage(model, prompt, completion) — вызывается из response translator'ов.
//   3. GetTokenUsageSnapshot() / ResetTokenUsage() — для /api/v1/stats/tokens endpoint.
//
// Atomic инкремент под RLock для минимизации contention.
package balancer

import (
	"sync/atomic"
	"time"
)

// ModelTokenUsage — per-model token aggregates.
type ModelTokenUsage struct {
	Model             string    `json:"model"`
	PromptTokens      int64     `json:"prompt_tokens"`
	CompletionTokens  int64     `json:"completion_tokens"`
	TotalTokens       int64     `json:"total_tokens"`
	RequestCount      int64     `json:"request_count"`
	LastUpdated       time.Time `json:"last_updated"`
	firstSeenUnixNano int64     // для отладки; не экспортируется в JSON
}

// getOrCreateTokenUsage — thread-safe accessor.
func (p *Proxy) getOrCreateTokenUsage(model string) *ModelTokenUsage {
	p.tokensByModelMu.RLock()
	u, ok := p.tokensByModel[model]
	p.tokensByModelMu.RUnlock()
	if ok {
		return u
	}
	p.tokensByModelMu.Lock()
	defer p.tokensByModelMu.Unlock()
	// Double-check после upgrade lock
	if u, ok = p.tokensByModel[model]; ok {
		return u
	}
	u = &ModelTokenUsage{
		Model:             model,
		firstSeenUnixNano: time.Now().UnixNano(),
	}
	p.tokensByModel[model] = u
	return u
}

// recordTokenUsage — атомарно инкрементирует per-model counters.
// Безопасно вызывать из любой горутины.
func (p *Proxy) recordTokenUsage(model string, promptTokens, completionTokens int64) {
	if p == nil || model == "" {
		return
	}
	u := p.getOrCreateTokenUsage(model)
	atomic.AddInt64(&u.PromptTokens, promptTokens)
	atomic.AddInt64(&u.CompletionTokens, completionTokens)
	atomic.AddInt64(&u.TotalTokens, promptTokens+completionTokens)
	atomic.AddInt64(&u.RequestCount, 1)
	// LastUpdated — non-atomic; допустим небольшой race для метки времени.
	u.LastUpdated = time.Now()
}

// GetTokenUsageSnapshot — возвращает snapshot всех моделей.
// Caller может безопасно итерировать без lock.
func (p *Proxy) GetTokenUsageSnapshot() []ModelTokenUsage {
	if p == nil {
		return nil
	}
	p.tokensByModelMu.RLock()
	defer p.tokensByModelMu.RUnlock()
	out := make([]ModelTokenUsage, 0, len(p.tokensByModel))
	for _, u := range p.tokensByModel {
		out = append(out, ModelTokenUsage{
			Model:            u.Model,
			PromptTokens:     atomic.LoadInt64(&u.PromptTokens),
			CompletionTokens: atomic.LoadInt64(&u.CompletionTokens),
			TotalTokens:      atomic.LoadInt64(&u.TotalTokens),
			RequestCount:     atomic.LoadInt64(&u.RequestCount),
			LastUpdated:      u.LastUpdated,
		})
	}
	return out
}

// ResetTokenUsage — обнуляет все счётчики (для тестов / admin reset).
func (p *Proxy) ResetTokenUsage() {
	if p == nil {
		return
	}
	p.tokensByModelMu.Lock()
	defer p.tokensByModelMu.Unlock()
	for _, u := range p.tokensByModel {
		atomic.StoreInt64(&u.PromptTokens, 0)
		atomic.StoreInt64(&u.CompletionTokens, 0)
		atomic.StoreInt64(&u.TotalTokens, 0)
		atomic.StoreInt64(&u.RequestCount, 0)
	}
}
