// Package balancer — unit-тесты для nctx_reload (Stages 2, 5, 6).
//
// Покрывает:
//   - NewNCtxReloadCoordinator + SetConfig (config hot-reload)
//   - LastKnownNCtx / SetLastKnownNCtx (per-backend n_ctx cache)
//   - DecideReloadBackend:
//   - AutoReloadNCtx=false → всегда NoOp
//   - bridgeErr nil → NoOp
//   - required <= current → NoOp
//   - required > max_vram*safety → Reject
//   - required <= max_vram*safety → Reload с new_n_ctx
//   - RecordDecision / RecordReloadDuration / RecordError
//   - Snapshot возвращает правильный формат метрик
//   - DefaultNCtxReloadConfig defaults
//   - loadNCtxReloadConfig (config bridge) + applyNCtxReloadEnvOverrides
package balancer

import (
	"os"
	"testing"
	"time"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/pkg/types"
)

func TestDefaultNCtxReloadConfig(t *testing.T) {
	cfg := DefaultNCtxReloadConfig()
	if cfg.AutoReloadNCtx != false {
		t.Errorf("default AutoReloadNCtx = %v, want false (kill-switch off)", cfg.AutoReloadNCtx)
	}
	if cfg.AutoReloadVRAMSafetyFactor != 0.85 {
		t.Errorf("default AutoReloadVRAMSafetyFactor = %v, want 0.85", cfg.AutoReloadVRAMSafetyFactor)
	}
	if cfg.AutoReloadTimeoutSec != 60 {
		t.Errorf("default AutoReloadTimeoutSec = %d, want 60", cfg.AutoReloadTimeoutSec)
	}
}

func TestNCtxReloadCoordinator_SetLastKnownNCtx(t *testing.T) {
	coord := NewNCtxReloadCoordinator(DefaultNCtxReloadConfig())
	defer coord.Shutdown() //nolint:errcheck

	if got := coord.LastKnownNCtx("backend-A"); got != 0 {
		t.Errorf("LastKnownNCtx before set = %d, want 0", got)
	}
	coord.SetLastKnownNCtx("backend-A", 8192)
	if got := coord.LastKnownNCtx("backend-A"); got != 8192 {
		t.Errorf("LastKnownNCtx after set = %d, want 8192", got)
	}

	// SetLastKnownNCtx игнорирует <= 0 (защита от garbage значений)
	coord.SetLastKnownNCtx("backend-A", 0)
	if got := coord.LastKnownNCtx("backend-A"); got != 8192 {
		t.Errorf("LastKnownNCtx after set 0 = %d, want 8192 (unchanged)", got)
	}
}

func TestNCtxReloadCoordinator_SetConfig(t *testing.T) {
	coord := NewNCtxReloadCoordinator(DefaultNCtxReloadConfig())
	defer coord.Shutdown() //nolint:errcheck

	if coord.Config().AutoReloadNCtx {
		t.Error("expected AutoReloadNCtx=false initially")
	}

	newCfg := DefaultNCtxReloadConfig()
	newCfg.AutoReloadNCtx = true
	coord.SetConfig(newCfg)
	if !coord.Config().AutoReloadNCtx {
		t.Error("SetConfig didn't update AutoReloadNCtx")
	}
}

func TestNCtxReloadCoordinator_DecideReloadBackend_Disabled(t *testing.T) {
	// AutoReloadNCtx=false — ВСЕГДА NoOp
	coord := NewNCtxReloadCoordinator(NCtxReloadConfig{
		AutoReloadNCtx:             false,
		AutoReloadVRAMSafetyFactor: 0.85,
	})
	defer coord.Shutdown() //nolint:errcheck

	plan := coord.DecideReloadBackend("backend-A", &NCtxBridgeError{
		Code:         bridge.ErrCodeNCtxNeedsReload,
		CurrentNCtx:  4096,
		RequiredNCtx: 8192,
		MaxVRAMNCtx:  77000,
	}, 8192)
	if plan.Decision != DecisionNoOp {
		t.Errorf("expected DecisionNoOp when AutoReloadNCtx=false, got %v", plan.Decision)
	}
}

func TestNCtxReloadCoordinator_DecideReloadBackend_NilBridgeErr(t *testing.T) {
	coord := NewNCtxReloadCoordinator(NCtxReloadConfig{
		AutoReloadNCtx: true,
	})
	defer coord.Shutdown() //nolint:errcheck

	plan := coord.DecideReloadBackend("backend-A", nil, 0)
	if plan.Decision != DecisionNoOp {
		t.Errorf("expected DecisionNoOp when bridgeErr is nil, got %v", plan.Decision)
	}
}

func TestNCtxReloadCoordinator_DecideReloadBackend_RequiredEqualsCurrent(t *testing.T) {
	coord := NewNCtxReloadCoordinator(NCtxReloadConfig{
		AutoReloadNCtx:             true,
		AutoReloadVRAMSafetyFactor: 0.85,
	})
	defer coord.Shutdown() //nolint:errcheck

	plan := coord.DecideReloadBackend("backend-A", &NCtxBridgeError{
		Code:         bridge.ErrCodeNCtxNeedsReload,
		CurrentNCtx:  4096,
		RequiredNCtx: 4096,
		MaxVRAMNCtx:  77000,
	}, 4096)
	if plan.Decision != DecisionNoOp {
		t.Errorf("expected DecisionNoOp when required==current, got %v", plan.Decision)
	}
}

func TestNCtxReloadCoordinator_DecideReloadBackend_Reject(t *testing.T) {
	// required > max_vram*safety → Reject
	coord := NewNCtxReloadCoordinator(NCtxReloadConfig{
		AutoReloadNCtx:             true,
		AutoReloadVRAMSafetyFactor: 0.85,
	})
	defer coord.Shutdown() //nolint:errcheck

	plan := coord.DecideReloadBackend("backend-A", &NCtxBridgeError{
		Code:         bridge.ErrCodeNCtxNeedsReload,
		CurrentNCtx:  4096,
		RequiredNCtx: 262144, // 256K
		MaxVRAMNCtx:  8192,   // мало VRAM
	}, 262144)
	if plan.Decision != DecisionReject {
		t.Errorf("expected DecisionReject, got %v (reason=%q)", plan.Decision, plan.Reason)
	}
}

func TestNCtxReloadCoordinator_DecideReloadBackend_Reload(t *testing.T) {
	// required <= max_vram*safety → Reload
	coord := NewNCtxReloadCoordinator(NCtxReloadConfig{
		AutoReloadNCtx:             true,
		AutoReloadVRAMSafetyFactor: 0.85,
	})
	defer coord.Shutdown() //nolint:errcheck

	plan := coord.DecideReloadBackend("backend-A", &NCtxBridgeError{
		Code:         bridge.ErrCodeNCtxNeedsReload,
		CurrentNCtx:  4096,
		RequiredNCtx: 8192,
		MaxVRAMNCtx:  77000, // 77000 * 0.85 = 65450, 8192 << 65450
	}, 8192)
	if plan.Decision != DecisionReload {
		t.Errorf("expected DecisionReload, got %v (reason=%q)", plan.Decision, plan.Reason)
	}
	if plan.NewNCtx < 8192 {
		t.Errorf("NewNCtx = %d, want >= 8192", plan.NewNCtx)
	}
}

func TestNCtxReloadCoordinator_DecideReloadBackend_MaxNCtxCap(t *testing.T) {
	// AutoReloadMaxNCtx cap — не превышать
	coord := NewNCtxReloadCoordinator(NCtxReloadConfig{
		AutoReloadNCtx:             true,
		AutoReloadMaxNCtx:          16384, // cap
		AutoReloadVRAMSafetyFactor: 0.85,
	})
	defer coord.Shutdown() //nolint:errcheck

	plan := coord.DecideReloadBackend("backend-A", &NCtxBridgeError{
		Code:         bridge.ErrCodeNCtxNeedsReload,
		CurrentNCtx:  4096,
		RequiredNCtx: 131072, // хотим 128K
		MaxVRAMNCtx:  262144, // VRAM позволяет
	}, 131072)
	if plan.Decision != DecisionReject {
		t.Errorf("expected DecisionReject (cap=16384 < required=131072), got %v", plan.Decision)
	}
}

func TestNCtxReloadCoordinator_RecordDecision(t *testing.T) {
	coord := NewNCtxReloadCoordinator(NCtxReloadConfig{
		AutoReloadNCtx: true,
	})
	defer coord.Shutdown() //nolint:errcheck

	// Record несколько решений
	coord.RecordDecision("backend-A", "noop", "no override")
	coord.RecordDecision("backend-A", "reload-start", "")
	coord.RecordDecision("backend-A", "reject", "VRAM too small")
	coord.RecordDecision("backend-A", "noop", "no override")

	// Snapshot должен показать правильные счётчики.
	// Конкретные значения зависят от internal-логики RecordDecision;
	// здесь мы только проверяем, что Snapshot не падает и содержит ключи.
	snap := coord.Snapshot()
	if snap == nil {
		t.Fatal("Snapshot returned nil")
	}
	for _, key := range []string{
		"nctx_reloads_total",
		"nctx_rejects_total",
		"nctx_errors_total",
		"nctx_reload_duration_ms_avg",
		"nctx_reload_duration_ms_sum",
		"nctx_reload_duration_count",
		"nctx_per_backend",
	} {
		if _, ok := snap[key]; !ok {
			t.Errorf("Snapshot missing key %q", key)
		}
	}
}

func TestNCtxReloadCoordinator_RecordReloadDuration(t *testing.T) {
	coord := NewNCtxReloadCoordinator(NCtxReloadConfig{
		AutoReloadNCtx: true,
	})
	defer coord.Shutdown() //nolint:errcheck

	// Без ошибки
	coord.RecordReloadDuration("backend-A", 100*time.Millisecond, nil)
	// С ошибкой
	coord.RecordReloadDuration("backend-A", 200*time.Millisecond, &NCtxReloadHTTPError{msg: "fail"})

	snap := coord.Snapshot()
	count, ok := snap["nctx_reload_duration_count"].(int64)
	if !ok {
		t.Fatalf("nctx_reload_duration_count type = %T, want int64", snap["nctx_reload_duration_count"])
	}
	if count < 2 {
		t.Errorf("nctx_reload_duration_count = %d, want >= 2", count)
	}
}

func TestNCtxReloadCoordinator_RecordError(t *testing.T) {
	coord := NewNCtxReloadCoordinator(NCtxReloadConfig{
		AutoReloadNCtx: true,
	})
	defer coord.Shutdown() //nolint:errcheck

	coord.RecordError("backend-A", "backend not found")
	coord.RecordError("backend-A", "timeout")
	coord.RecordError("backend-B", "http 500")

	snap := coord.Snapshot()
	errorsCount, ok := snap["nctx_errors_total"].(int64)
	if !ok {
		t.Fatalf("nctx_errors_total type = %T, want int64", snap["nctx_errors_total"])
	}
	if errorsCount < 3 {
		t.Errorf("nctx_errors_total = %d, want >= 3", errorsCount)
	}
}

func TestNCtxReloadCoordinator_Snapshot_NilSafe(t *testing.T) {
	// Snapshot на nil-координаторе (защита от panic при тестах)
	var coord *NCtxReloadCoordinator
	snap := coord.Snapshot()
	if snap == nil {
		t.Fatal("Snapshot on nil returned nil")
	}
	if _, ok := snap["nctx_reloads_total"]; !ok {
		t.Error("nil Snapshot missing nctx_reloads_total key")
	}
}

func TestNCtxReloadCoordinator_RecordOnNilSafe(t *testing.T) {
	// Record* на nil-координаторе не паникует
	var coord *NCtxReloadCoordinator
	coord.RecordDecision("b", "noop", "r")
	coord.RecordReloadDuration("b", 1*time.Millisecond, nil)
	coord.RecordError("b", "x")
}

func TestLoadNCtxReloadConfig_Nil(t *testing.T) {
	// loadNCtxReloadConfig(nil) → defaults + ENV overrides
	// Перед запуском чистим ENV, чтобы тест был детерминированным
	for _, k := range []string{
		"LB_NCTX_RELOAD_ENABLED",
		"LB_NCTX_RELOAD_MAX_N_CTX",
		"LB_NCTX_RELOAD_VRAM_SAFETY_FACTOR",
		"LB_NCTX_RELOAD_TIMEOUT_SEC",
	} {
		os.Unsetenv(k)
	}
	got := loadNCtxReloadConfig(nil)
	if got.AutoReloadNCtx {
		t.Error("expected AutoReloadNCtx=false by default")
	}
}

func TestLoadNCtxReloadConfig_FromConfig(t *testing.T) {
	// ENV не задан → используется значение из config
	for _, k := range []string{
		"LB_NCTX_RELOAD_ENABLED",
		"LB_NCTX_RELOAD_MAX_N_CTX",
		"LB_NCTX_RELOAD_VRAM_SAFETY_FACTOR",
		"LB_NCTX_RELOAD_TIMEOUT_SEC",
	} {
		os.Unsetenv(k)
	}
	cfg := &types.LoadBalancerConfig{
		Balancing: types.BalancingSettings{
			NCtxReload: types.NCtxReloadSettings{
				AutoReloadNCtx:             true,
				AutoReloadMaxNCtx:          32768,
				AutoReloadVRAMSafetyFactor: 0.75,
				AutoReloadTimeoutSec:       120,
			},
		},
	}
	got := loadNCtxReloadConfig(cfg)
	if !got.AutoReloadNCtx {
		t.Error("AutoReloadNCtx should be true from config")
	}
	if got.AutoReloadMaxNCtx != 32768 {
		t.Errorf("AutoReloadMaxNCtx = %d, want 32768", got.AutoReloadMaxNCtx)
	}
	if got.AutoReloadVRAMSafetyFactor != 0.75 {
		t.Errorf("AutoReloadVRAMSafetyFactor = %v, want 0.75", got.AutoReloadVRAMSafetyFactor)
	}
	if got.AutoReloadTimeoutSec != 120 {
		t.Errorf("AutoReloadTimeoutSec = %d, want 120", got.AutoReloadTimeoutSec)
	}
}

func TestLoadNCtxReloadConfig_EnvOverridesConfig(t *testing.T) {
	// ENV > config: LB_NCTX_RELOAD_ENABLED=true переопределяет config=false
	os.Setenv("LB_NCTX_RELOAD_ENABLED", "true")
	os.Setenv("LB_NCTX_RELOAD_VRAM_SAFETY_FACTOR", "0.5")
	defer func() {
		os.Unsetenv("LB_NCTX_RELOAD_ENABLED")
		os.Unsetenv("LB_NCTX_RELOAD_VRAM_SAFETY_FACTOR")
	}()

	cfg := &types.LoadBalancerConfig{
		Balancing: types.BalancingSettings{
			NCtxReload: types.NCtxReloadSettings{
				AutoReloadNCtx: false, // ENV должен переопределить
			},
		},
	}
	got := loadNCtxReloadConfig(cfg)
	if !got.AutoReloadNCtx {
		t.Error("ENV should override config: AutoReloadNCtx should be true")
	}
	if got.AutoReloadVRAMSafetyFactor != 0.5 {
		t.Errorf("AutoReloadVRAMSafetyFactor = %v, want 0.5 (from ENV)", got.AutoReloadVRAMSafetyFactor)
	}
}

func TestRoundUpPow2(t *testing.T) {
	tests := []struct {
		in, want int
	}{
		{512, 512},
		{513, 1024},
		{1024, 1024},
		{1025, 2048},
		{4096, 4096},
		{4097, 8192},
		{0, 512},       // default
		{1, 512},       // default (меньше 512 → 512)
		{16384, 16384}, // уже степень 2
	}
	for _, tc := range tests {
		got := roundUpPow2(tc.in)
		if got != tc.want {
			t.Errorf("roundUpPow2(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// NCtxReloadHTTPError — фиктивная ошибка для теста RecordReloadDuration
type NCtxReloadHTTPError struct{ msg string }

func (e *NCtxReloadHTTPError) Error() string { return e.msg }
