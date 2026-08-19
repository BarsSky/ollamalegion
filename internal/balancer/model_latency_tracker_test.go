package balancer

import (
	"errors"
	"net"
	"testing"
	"time"
)

// errorsAs — алиас для errors.As (чтобы не импортировать "errors" в каждом тесте).
// Используется для проверки, что ошибка реализует интерфейс net.Error.
func errorsAs(err error, target interface{}) bool {
	return errors.As(err, target)
}

// TestEstimateIdleTimeoutFromModelSize — эвристика idle timeout по размеру модели.
// Round 32 #11 (2026-08-10): bumped 2-5GB 300s → 600s and 5-12GB 600s → 1200s
// (30-40 min reasoning prompts на 4-8B Q4_K_M).
// Round 32 #9 (2026-08-10): bumped 2-5GB 180s → 300s and 5-12GB 300s → 600s
// (reasoning-capable models: gemma-4, Qwen3-Instruct with reasoning=on).
func TestEstimateIdleTimeoutFromModelSize(t *testing.T) {
	cases := []struct {
		name     string
		sizeGB   float64
		wantMin  time.Duration // минимум (включительно)
		wantZero bool          // true если ожидаем 0
	}{
		{"zero size", 0, 0, true},
		{"negative size", -1, 0, true},
		{"tiny 1GB", 1, 120 * time.Second, false},
		{"small 3GB Q4_K_M 7B", 3.5, 600 * time.Second, false},
		{"medium 8GB Q4_K_M 13B", 8, 1200 * time.Second, false},
		{"large 16GB Q4_K_M 27B", 16, 1800 * time.Second, false},
		{"xl 40GB Q4_K_M 70B", 40, 2400 * time.Second, false},
		{"huge 100GB Q2_K 200B", 100, 2400 * time.Second, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := EstimateIdleTimeoutFromModelSize(int64(c.sizeGB * 1024 * 1024 * 1024))
			if c.wantZero {
				if got != 0 {
					t.Errorf("expected 0 for invalid/zero size, got %v", got)
				}
				return
			}
			if got < c.wantMin {
				t.Errorf("size=%.1fGB: got %v, want >= %v", c.sizeGB, got, c.wantMin)
			}
		})
	}
}

// TestEstimateStreamTimeoutFromModelSize — эвристика общего stream timeout.
// Round 32 #11 (2026-08-10): bumped 2-5GB 900s → 1800s (30 min) and 5-12GB
// 1200s → 2400s (40 min). Live test показал что gemma-4 с длинным
// reasoning + HTML output требует 30+ мин на 4B Q4_K_M @ RTX 3070.
// Round 32 #9 (2026-08-10): bumped 2-5GB 300s → 900s and 5-12GB 600s → 1200s
// for reasoning-capable models. gemma-4 4.64GB was generating 16+ min on
// reasoning prompts; 5 min cap truncated mid-stream.
func TestEstimateStreamTimeoutFromModelSize(t *testing.T) {
	cases := []struct {
		name    string
		sizeGB  float64
		wantMin time.Duration
	}{
		{"small 1GB", 1, 120 * time.Second},
		{"reasoning 4.6GB gemma-4", 4.6, 1800 * time.Second},
		{"medium 8GB", 8, 2400 * time.Second},
		{"large 16GB", 16, 3600 * time.Second},
		{"xl 40GB", 40, 5400 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := EstimateStreamTimeoutFromModelSize(int64(c.sizeGB * 1024 * 1024 * 1024))
			if got < c.wantMin {
				t.Errorf("size=%.1fGB: got %v, want >= %v", c.sizeGB, got, c.wantMin)
			}
		})
	}
}

// TestGetOrComputeIdleTimeout_Heuristic — Tier 3 (эвристика по размеру)
// для моделей без per-model profile и без истории генерации.
// Round 32 #11 (2026-08-10): bumped 20GB from ≥900s to ≥1800s, 8GB from ≥600s to ≥1200s.
func TestGetOrComputeIdleTimeout_Heuristic(t *testing.T) {
	tracker := NewModelLatencyTracker()

	// 20GB модель без profile → должна получить ≥1800s по эвристике (Round 32 #11).
	modelSize := int64(20 * 1024 * 1024 * 1024)
	got := tracker.GetOrComputeIdleTimeout("big-model", 0, 120, modelSize)
	if got < 1800*time.Second {
		t.Errorf("20GB model heuristic idle timeout: got %v, want >= 1800s", got)
	}

	// 8GB модель → ≥1200s (Round 32 #11: 600s → 1200s).
	got = tracker.GetOrComputeIdleTimeout("medium-model", 0, 120, int64(8*1024*1024*1024))
	if got < 1200*time.Second {
		t.Errorf("8GB model heuristic: got %v, want >= 1200s", got)
	}

	// 1GB модель → ≥120s (default).
	got = tracker.GetOrComputeIdleTimeout("tiny-model", 0, 120, int64(1*1024*1024*1024))
	if got != 120*time.Second {
		t.Errorf("1GB model: got %v, want 120s", got)
	}

	// Per-model profile > 0 — используем его.
	got = tracker.GetOrComputeIdleTimeout("any-model", 900, 120, int64(20*1024*1024*1024))
	if got != 900*time.Second {
		t.Errorf("per-model profile override: got %v, want 900s", got)
	}

	// Без размера — fallback на global.
	got = tracker.GetOrComputeIdleTimeout("unknown-model", 0, 300)
	if got != 300*time.Second {
		t.Errorf("global fallback: got %v, want 300s", got)
	}
}

// TestGetOrComputeIdleTimeout_StatsHistory — Tier 2 (статистика) имеет
// приоритет над Tier 3 (эвристика), если есть история.
func TestGetOrComputeIdleTimeout_StatsHistory(t *testing.T) {
	tracker := NewModelLatencyTracker()

	// Записываем сэмпл с MaxInterTokenGapMs = 60s → P95=60s × idleMult(2.0 для heavy) × 1.5 = 180s
	tracker.RecordSample("heavy-model", ModelSample{
		TokensGenerated:    100,
		DurationMs:         10000,
		FirstByteLatencyMs: 5000,
		MaxInterTokenGapMs: 60000,
		Error:              false,
		NumGPULayers:       0, // CPU-only → isHeavy=true → heavyMultiplier=2.0
	})

	stats := tracker.GetStats("heavy-model")
	if stats.RecommendedIdleTimeoutSec <= 0 {
		t.Fatalf("expected RecommendedIdleTimeoutSec > 0, got %d", stats.RecommendedIdleTimeoutSec)
	}
	t.Logf("RecommendedIdleTimeoutSec=%d (MaxInterTokenGapMs=60000, isHeavy=true)",
		stats.RecommendedIdleTimeoutSec)

	// 60s × 2.0 × 1.5 = 180s (recommended by stats)
	got := tracker.GetOrComputeIdleTimeout("heavy-model", 0, 120, int64(20*1024*1024*1024))
	if got < 180*time.Second {
		t.Errorf("expected stats-based idle timeout >= 180s, got %v", got)
	}
}

// timeoutErr — реализация net.Error с Timeout()=true, используемая в
// streaming.go для детекции idle-timeout от бэкенда. Создаём свой тип,
// чтобы тесты не зависели от внутреннего состояния Go runtime.
type timeoutErr struct{}

func (e *timeoutErr) Error() string   { return "i/o timeout" }
func (e *timeoutErr) Timeout() bool   { return true }
func (e *timeoutErr) Temporary() bool { return true }

// TestIsIdleTimeoutDetection — verify that streaming.go корректно различает
// idle-timeout от реального EOF. Используем errors.As + net.Error.Timeout()
// (реальный Go API, не зависит от внутренностей streaming.go).
//
// Контракт, который должен соблюдать streaming.go:
//   - timeoutErr (net.Error с Timeout()=true) → isIdleTimeout=true.
//   - io.EOF или другие ошибки без Timeout() → НЕ считать timeout.
//
// Это unit-тест логики errors.As + Timeout(). Интеграционное тестирование
// с реальным SetReadDeadline → "i/o timeout" проводится отдельно.
func TestIsIdleTimeoutDetection(t *testing.T) {
	// timeoutErr (реализует net.Error с Timeout()=true).
	testErr := &timeoutErr{}
	var netErr net.Error
	if !errorsAs(testErr, &netErr) {
		t.Fatal("errors.As should extract net.Error from *timeoutErr")
	}
	if !netErr.Timeout() {
		t.Error("expected netErr.Timeout()=true for *timeoutErr")
	}
}

// TestEstimateFirstByteTimeoutFromModelSize — Round 42 (2026-08-19).
// Эвристика таймаута первого байта по размеру .gguf файла.
//
// Round 42: bump всех tiers для batched inference (32K prompt prefill на MoE
// модели с partial offload = 10-15 мин на 8GB VRAM).
// Round 42: ВАЖНО — size=0 возвращает 900s default (НЕ 0 → fallthrough к global
// 120s → Cline aborts через 2 мин).
func TestEstimateFirstByteTimeoutFromModelSize(t *testing.T) {
	cases := []struct {
		name    string
		sizeGB  float64
		wantMin time.Duration
	}{
		// Round 42: size=0 теперь возвращает 900s (15 min) default,
		// а НЕ 0 (который раньше приводил к fallthrough на global 120s).
		{"zero size (unknown model)", 0, 900 * time.Second},
		{"negative size", -1, 900 * time.Second},
		// Round 42 tier mappings:
		{"tiny 1GB Q4_K_M 1-2B", 1, 300 * time.Second},
		{"small 2.5GB Qwen3-Instruct-2507", 2.5, 600 * time.Second},
		{"medium 4.6GB gemma-4", 4.6, 600 * time.Second},
		{"large 8GB Q4_K_M 13B", 8, 1200 * time.Second},
		{"xl 16GB Q4_K_M 27B", 16, 1800 * time.Second},
		// Qwen3.6-35B-A3B-UD-Q4_K_M = 22GB, на котором тестировался Round 42.
		// Cline retry loop с 5-min timeout прерывал 32K prompt prefill на этом размере.
		{"xxl 22GB Qwen3.6-35B-A3B", 22, 1800 * time.Second},
		{"xxxl 40GB Q4_K_M 70B", 40, 2400 * time.Second},
		{"huge 100GB Q2_K 200B", 100, 2400 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := EstimateFirstByteTimeoutFromModelSize(int64(c.sizeGB * 1024 * 1024 * 1024))
			if got < c.wantMin {
				t.Errorf("size=%.1fGB: got %v, want >= %v", c.sizeGB, got, c.wantMin)
			}
		})
	}
}

// TestGetOrComputeFirstByteTimeout_Heuristic — Tier 3 (эвристика по размеру GGUF)
// для моделей без per-model profile и без истории генерации.
//
// Round 42 (2026-08-19): bumped tiers для batched inference + size=0 → 900s default.
func TestGetOrComputeFirstByteTimeout_Heuristic(t *testing.T) {
	tracker := NewModelLatencyTracker()

	// 22GB модель (Qwen3.6-35B) → ≥1800s (Round 42: 600s → 1800s).
	got := tracker.GetOrComputeFirstByteTimeout("qwen36-35b", 0, 120, int64(22*1024*1024*1024))
	if got < 1800*time.Second {
		t.Errorf("22GB Qwen3.6-35B: got %v, want >= 1800s", got)
	}

	// 4.6GB модель (gemma-4) → ≥600s (Round 42: 120s → 600s для 2-5GB tier).
	got = tracker.GetOrComputeFirstByteTimeout("gemma-4", 0, 120, int64(4*1024*1024*1024))
	if got < 600*time.Second {
		t.Errorf("4GB gemma-4: got %v, want >= 600s", got)
	}

	// 1GB модель → ≥300s (Round 42: 60s → 300s).
	got = tracker.GetOrComputeFirstByteTimeout("tiny", 0, 120, int64(1*1024*1024*1024))
	if got < 300*time.Second {
		t.Errorf("1GB model: got %v, want >= 300s", got)
	}

	// Per-model profile > 0 — используем его (highest priority).
	got = tracker.GetOrComputeFirstByteTimeout("any", 900, 120, int64(22*1024*1024*1024))
	if got != 900*time.Second {
		t.Errorf("per-model profile override: got %v, want 900s", got)
	}

	// Round 42 КРИТИЧНО: size=0 (unknown) → НЕ fallthrough к global 120s.
	// Должен вернуть 900s default (Tier 5 default для batched inference).
	got = tracker.GetOrComputeFirstByteTimeout("unknown-model", 0, 120, 0)
	if got < 900*time.Second {
		t.Errorf("size=0 unknown model: got %v, want >= 900s (Round 42 critical fix — was falling through to 120s global)", got)
	}
}
