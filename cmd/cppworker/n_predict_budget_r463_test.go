// n_predict_budget_r463_test.go — R463/R464 (2026-11-06).
//
// ЖИВОЙ ДЕФЕКТ (второй стенд: A10 ×2, Qwen3.8-27B, Cline). Клиент не присылает
// num_predict, присылает max_output_tokens (например 27392). Политика R411
// подставляла дефолт reasoning-модели 8192, а граница клиента могла только
// уменьшать. Размышления съедали 8192 → done_reason=length без видимого ответа →
// клиентская overflow-recovery (compacting) → «text part … not found» и потеря
// части переписки. В логе при этом было «R188 повтор ограничен … клиент задал
// длину ответа n_predict=8192», хотя клиент длину НЕ задавал.
package main

import (
	"testing"
)

func ptrInt(v int) *int { return &v }

// TestR463_UpperBoundRaisesDefault — главный случай: граница клиента поднимает
// дефолт модели, но не выше предохранителя.
func TestR463_UpperBoundRaisesDefault(t *testing.T) {
	budget, source := resolveOutputBudgetR463(0, 27392, "qwen3.8-27b")
	if source != "client_upper_bound+ceiling" {
		t.Fatalf("source = %q, want client_upper_bound+ceiling", source)
	}
	if budget != defaultMaxNPredictR463 {
		t.Fatalf("budget = %d, want %d (потолок)", budget, defaultMaxNPredictR463)
	}
	if budget <= DefaultNPredictReasoning {
		t.Fatalf("budget %d не больше дефолта reasoning %d — граница не подняла бюджет",
			budget, DefaultNPredictReasoning)
	}
}

// TestR463_ExplicitClientValueWins — явный num_predict приоритетнее границы.
func TestR463_ExplicitClientValueWins(t *testing.T) {
	budget, source := resolveOutputBudgetR463(4096, 27392, "qwen3.8-27b")
	if source != "client_num_predict" || budget != 4096 {
		t.Fatalf("budget=%d source=%q, want 4096/client_num_predict", budget, source)
	}

	// Но и явное значение упирается в потолок: иначе клиент «65536» получит
	// многочасовой ход на 27B с CPU-оффлоадом.
	budget, source = resolveOutputBudgetR463(65536, 0, "qwen3.8-27b")
	if source != "client_num_predict+ceiling" || budget != defaultMaxNPredictR463 {
		t.Fatalf("budget=%d source=%q, want %d/client_num_predict+ceiling",
			budget, source, defaultMaxNPredictR463)
	}
}

// TestR463_NoClientData — ни длины, ни границы: дефолт политики без изменений.
func TestR463_NoClientData(t *testing.T) {
	budget, source := resolveOutputBudgetR463(0, 0, "qwen3.8-27b")
	if source != "model_default" || budget != DefaultNPredictReasoning {
		t.Fatalf("budget=%d source=%q, want %d/model_default", budget, source, DefaultNPredictReasoning)
	}
}

// TestR463_SmallerUpperBoundDoesNotRaise — граница МЕНЬШЕ дефолта не поднимает.
func TestR463_SmallerUpperBoundDoesNotRaise(t *testing.T) {
	budget, source := resolveOutputBudgetR463(0, 2048, "qwen3.8-27b")
	if source != "model_default" || budget != DefaultNPredictReasoning {
		t.Fatalf("budget=%d source=%q: граница меньше дефолта не должна его менять", budget, source)
	}
}

// TestR463_CeilingEnv — предохранитель настраивается, мусор не ломает.
func TestR463_CeilingEnv(t *testing.T) {
	if got := maxNPredictCeilingR463(); got != defaultMaxNPredictR463 {
		t.Fatalf("по умолчанию %d, want %d", got, defaultMaxNPredictR463)
	}
	t.Setenv("CPPWORKER_MAX_N_PREDICT", "32768")
	if got := maxNPredictCeilingR463(); got != 32768 {
		t.Fatalf("env=32768 → %d", got)
	}
	budget, _ := resolveOutputBudgetR463(0, 60000, "qwen3.8-27b")
	if budget != 32768 {
		t.Fatalf("budget=%d, want 32768 (из env)", budget)
	}
	t.Setenv("CPPWORKER_MAX_N_PREDICT", "чепуха")
	if got := maxNPredictCeilingR463(); got != defaultMaxNPredictR463 {
		t.Fatalf("мусор в env → %d, want дефолт %d", got, defaultMaxNPredictR463)
	}
	t.Setenv("CPPWORKER_MAX_N_PREDICT", "0")
	if got := maxNPredictCeilingR463(); got != defaultMaxNPredictR463 {
		t.Fatalf("0 в env → %d, want дефолт %d", got, defaultMaxNPredictR463)
	}
}

// TestR463_OllamaPathBudget — через реальный buildGenerationParams (/api/chat,
// построенный из chat-запроса).
func TestR463_OllamaPathBudget(t *testing.T) {
	req := generateRequest{
		Model:     "qwen3.8-9b",
		Prompt:    "hi",
		MaxTokens: 0,
	}
	req._upperBoundNumPredict = 27392
	if got := buildGenerationParams(req).NPredict; got != defaultMaxNPredictR463 {
		t.Fatalf("NPredict=%d, want %d (граница клиента подняла дефолт)", got, defaultMaxNPredictR463)
	}

	explicit := generateRequest{Model: "qwen3.8-9b", Prompt: "hi", MaxTokens: 4096}
	explicit._upperBoundNumPredict = 27392
	if got := buildGenerationParams(explicit).NPredict; got != 4096 {
		t.Fatalf("явный num_predict 4096 → NPredict=%d", got)
	}

	plain := generateRequest{Model: "qwen3.8-9b", Prompt: "hi"}
	if got := buildGenerationParams(plain).NPredict; got != DefaultNPredictReasoning {
		t.Fatalf("без клиентских данных NPredict=%d, want %d", got, DefaultNPredictReasoning)
	}
}

// TestR464_ClientLimitDetection — R464: признак «клиент ограничил длину».
func TestR464_ClientLimitDetection(t *testing.T) {
	if !clientSetOutputLimit(4096) {
		t.Fatal("явный num_predict=4096 обязан считаться клиентским лимитом")
	}
	if clientSetOutputLimit(0) {
		t.Fatal("num_predict=0 (не задано) не является клиентским лимитом")
	}

	// /api/chat: max_tokens указателем, num_predict в options.
	if clientSetOutputLimitChat(ptrInt(0), 0) {
		t.Fatal("explicit max_tokens=0 и num_predict=0 — клиент длину не задавал")
	}
	if !clientSetOutputLimitChat(nil, 8192) {
		t.Fatal("num_predict=8192 в options — клиентский лимит")
	}
	if !clientSetOutputLimitChat(ptrInt(4096), 0) {
		t.Fatal("max_tokens=4096 — клиентский лимит")
	}
}

// TestR464_RetryBudgetUsesClientFlag — при нашем дефолте повтор не урезается.
func TestR464_RetryBudgetUsesClientFlag(t *testing.T) {
	// Наш дефолт (клиент ничего не задал) — полный бюджет повторов.
	if got := emptyOutputRetryBudget(clientSetOutputLimit(0)); got != maxEmptyOutputRetries {
		t.Fatalf("при нашем дефолте повторов %d, want %d: наш дефолт — не клиентский лимит",
			got, maxEmptyOutputRetries)
	}
	// Клиент реально ограничил — один повтор.
	if got := emptyOutputRetryBudget(clientSetOutputLimit(4096)); got != 1 {
		t.Fatalf("при клиентском лимите повторов %d, want 1", got)
	}
}
