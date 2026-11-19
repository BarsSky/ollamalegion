package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// R551 (T-12 аудита docs/plans/2026-11-19-consistency-audit.md §1.4).
//
// ЖИВОЙ ДЕФЕКТ. Одна величина — два значения. `config.Load` заполняет
// `Resources.CPU.MaxUsagePercent` = 80 и `Resources.Memory.MaxUsagePercent` = 85
// (env `LB_CPU_MAX_USAGE` / `LB_MEMORY_MAX_USAGE`), а `checkResourceLimits` при
// нуле в конфиге подставлял СВОИ фолбэки: CPU 95, RAM 98. То есть на стенде с
// конфигом без этих ключей «порог памяти 85 %» не ограничивал ничего, а
// документация (`docs/architecture/balancing-guide.ru.md:224-225`) обещала именно
// 80/85.
//
// После правки фолбэк равен дефолту конфига: одна величина — один источник.
// Проверки идут ЧЕРЕЗ РЕАЛЬНЫЙ путь допуска (`checkResourceLimits`), а не через
// копию формулы в тесте: копия не покраснела бы при возврате 98/95.

// resourceMetricsR551 — метрики бэкенда с заданной загрузкой CPU и RAM.
func resourceMetricsR551(cpuPercent float64, ramUsedMB uint64) *types.BackendMetrics {
	return &types.BackendMetrics{
		System: types.SystemMetrics{
			CPUUsagePercent: cpuPercent,
			MemoryTotal:     100_000, // 100 ГБ в МБ — процент считается из этих чисел
			MemoryUsed:      ramUsedMB,
		},
	}
}

func setMetricsR551(p *Proxy, m *types.BackendMetrics) {
	p.metricsMgr.mu.Lock()
	p.metricsMgr.metrics["backend-1"] = m
	p.metricsMgr.mu.Unlock()
}

// TestR551_RamFallbackMatchesConfigDefault — конфиг без порога RAM: действует 85 %
// (как в config.Load и в документации), а не прежние 98 %.
func TestR551_RamFallbackMatchesConfigDefault(t *testing.T) {
	cfg := createTestConfig()
	cfg.Resources.Memory.MaxUsagePercent = 0 // ключа нет — работает фолбэк
	proxy := newProxyWithCleanup(t, cfg)

	// 90 % RAM: выше документированных 85, но ниже прежних 98.
	setMetricsR551(proxy, resourceMetricsR551(10, 90_000))
	if proxy.checkResourceLimits("backend-1") {
		t.Fatal("при 90 % RAM и документированном пороге 85 % бэкенд обязан блокироваться: " +
			"прежний фолбэк 98 % делал настройку «85 %» неработающей")
	}

	// 80 % RAM — ниже порога: блокировки быть не должно (иначе тест ловит не то).
	setMetricsR551(proxy, resourceMetricsR551(10, 80_000))
	if !proxy.checkResourceLimits("backend-1") {
		t.Fatal("при 80 % RAM блокировки быть не должно")
	}
}

// TestR551_CpuFallbackMatchesConfigDefault — конфиг без порога CPU: действует 80 %
// (config.Load + документация), а не прежние 95 %.
//
// Порог CPU — SOFT (R118: не исключает бэкенд из кандидатов), поэтому наблюдаем
// его в ЖЁСТКОМ режиме (`LB_RESOURCE_LIMITS_HARD_BLOCK=1`) — там превышение
// порога действительно блокирует.
func TestR551_CpuFallbackMatchesConfigDefault(t *testing.T) {
	t.Setenv("LB_RESOURCE_LIMITS_HARD_BLOCK", "1")

	cfg := createTestConfig()
	cfg.Resources.CPU.MaxUsagePercent = 0
	cfg.Resources.Memory.MaxUsagePercent = 85
	proxy := newProxyWithCleanup(t, cfg)

	// 90 % CPU: выше 80 (дефолт конфига), но ниже прежних 95.
	setMetricsR551(proxy, resourceMetricsR551(90, 10_000))
	if proxy.checkResourceLimits("backend-1") {
		t.Fatal("в жёстком режиме при 90 % CPU и пороге 80 % бэкенд обязан блокироваться: " +
			"прежний фолбэк 95 % пропускал такую загрузку")
	}

	// 70 % CPU — ниже порога: блокировки быть не должно.
	setMetricsR551(proxy, resourceMetricsR551(70, 10_000))
	if !proxy.checkResourceLimits("backend-1") {
		t.Fatal("при 70 % CPU блокировки быть не должно")
	}
}

// TestR551_ConfiguredValueWins — явная настройка по-прежнему главнее фолбэка
// (правка не должна «прибить» порог к дефолту).
func TestR551_ConfiguredValueWins(t *testing.T) {
	cfg := createTestConfig()
	cfg.Resources.Memory.MaxUsagePercent = 96
	proxy := newProxyWithCleanup(t, cfg)

	// 90 % RAM при настроенных 96 % — блокировки нет.
	setMetricsR551(proxy, resourceMetricsR551(10, 90_000))
	if !proxy.checkResourceLimits("backend-1") {
		t.Fatal("оператор настроил порог 96 % — 90 % блокировать нельзя")
	}
	// 97 % RAM при настроенных 96 % — блокируется.
	setMetricsR551(proxy, resourceMetricsR551(10, 97_000))
	if proxy.checkResourceLimits("backend-1") {
		t.Fatal("97 % RAM выше настроенных 96 % — должно блокироваться")
	}
}
