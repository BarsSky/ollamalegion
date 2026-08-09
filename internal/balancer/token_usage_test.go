// token_usage_test.go — tests for Round 31 #7 token usage tracking.
package balancer

import (
	"sync"
	"testing"
)

// TestTokenUsage_RecordAndSnapshot — basic record + snapshot.
func TestTokenUsage_RecordAndSnapshot(t *testing.T) {
	p := &Proxy{tokensByModel: make(map[string]*ModelTokenUsage)}
	p.recordTokenUsage("gemma-4", 100, 50)
	p.recordTokenUsage("gemma-4", 200, 75)
	p.recordTokenUsage("qwen3.6", 1000, 500)

	snap := p.GetTokenUsageSnapshot()
	if len(snap) != 2 {
		t.Fatalf("expected 2 models, got %d", len(snap))
	}
	byModel := map[string]ModelTokenUsage{}
	for _, u := range snap {
		byModel[u.Model] = u
	}
	g := byModel["gemma-4"]
	if g.PromptTokens != 300 {
		t.Errorf("gemma-4 prompt: want 300, got %d", g.PromptTokens)
	}
	if g.CompletionTokens != 125 {
		t.Errorf("gemma-4 completion: want 125, got %d", g.CompletionTokens)
	}
	if g.TotalTokens != 425 {
		t.Errorf("gemma-4 total: want 425, got %d", g.TotalTokens)
	}
	if g.RequestCount != 2 {
		t.Errorf("gemma-4 request_count: want 2, got %d", g.RequestCount)
	}
	q := byModel["qwen3.6"]
	if q.PromptTokens != 1000 {
		t.Errorf("qwen3.6 prompt: want 1000, got %d", q.PromptTokens)
	}
	if q.RequestCount != 1 {
		t.Errorf("qwen3.6 request_count: want 1, got %d", q.RequestCount)
	}
}

// TestTokenUsage_Reset — reset обнуляет счётчики.
func TestTokenUsage_Reset(t *testing.T) {
	p := &Proxy{tokensByModel: make(map[string]*ModelTokenUsage)}
	p.recordTokenUsage("m1", 100, 50)
	p.ResetTokenUsage()
	snap := p.GetTokenUsageSnapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 model entry, got %d", len(snap))
	}
	if snap[0].PromptTokens != 0 || snap[0].CompletionTokens != 0 {
		t.Errorf("after Reset, counters should be 0, got prompt=%d completion=%d",
			snap[0].PromptTokens, snap[0].CompletionTokens)
	}
}

// TestTokenUsage_Concurrent — race-free под параллельной нагрузкой.
func TestTokenUsage_Concurrent(t *testing.T) {
	p := &Proxy{tokensByModel: make(map[string]*ModelTokenUsage)}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				p.recordTokenUsage("gemma-4", 10, 5)
			}
		}()
	}
	wg.Wait()

	snap := p.GetTokenUsageSnapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 model, got %d", len(snap))
	}
	wantPrompt := int64(100 * 100 * 10)
	if snap[0].PromptTokens != wantPrompt {
		t.Errorf("prompt_tokens: want %d, got %d (race condition?)", wantPrompt, snap[0].PromptTokens)
	}
	wantCompletion := int64(100 * 100 * 5)
	if snap[0].CompletionTokens != wantCompletion {
		t.Errorf("completion_tokens: want %d, got %d", wantCompletion, snap[0].CompletionTokens)
	}
	wantCount := int64(100 * 100)
	if snap[0].RequestCount != wantCount {
		t.Errorf("request_count: want %d, got %d", wantCount, snap[0].RequestCount)
	}
}

// TestTokenUsage_NilSafety — nil proxy и empty model не паникуют.
func TestTokenUsage_NilSafety(t *testing.T) {
	var p *Proxy
	p.recordTokenUsage("m", 1, 1) // должно быть no-op, не panic
	if snap := p.GetTokenUsageSnapshot(); snap != nil {
		t.Errorf("nil proxy should return nil snapshot, got %v", snap)
	}

	p = &Proxy{tokensByModel: make(map[string]*ModelTokenUsage)}
	p.recordTokenUsage("", 1, 1) // empty model = no-op
	if snap := p.GetTokenUsageSnapshot(); len(snap) != 0 {
		t.Errorf("empty model should not create entry, got %d", len(snap))
	}
}
