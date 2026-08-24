// Round 52.4 (2026-08-24): TestMain watchdog для предотвращения hang.
//
// Проблема: многие pre-existing тесты в internal/balancer запускают
// background goroutines (llamaCppMetricsPoller, AdaptiveWeightTuner,
// SessionManager, AutoPullManager, PrewarmController, и т.д.) и не
// cleanup'ят их. R52.4 partial fix (newProxyWithCleanup) покрывает
// только NewProxy() callsites — другие пути (smoke, e2e, scenario tests)
// всё ещё утекают.
//
// Эффект: 1000+ leaked goroutines → test runner ждёт их завершения
// 60-120s → CI timeout.
//
// Fix: TestMain с watchdog `time.AfterFunc(timeout, os.Exit(1))` который
// ФОРСИРОВАННО завершает процесс через 50s. Это "kill switch" для
// hung тестов — даже если 1000 goroutines висят, через 50s процесс
// убьётся и CI выдаст exit code 1 (failure), но НЕ будет висеть
// вечно на 5-минутном GitHub Actions timeout.
//
// Компромисс: после kill тесты что работали, могут не иметь clean
// artifacts. Но CI не висит. Это лучший trade-off.
package balancer

import (
	"os"
	"testing"
	"time"
)

// TestMain — entry point для всех тестов в этом пакете.
//
// Round 52.4: добавляем watchdog чтобы test runner не висел в случае
// leaked goroutines. Срабатывает через 50s после старта тестов.
func TestMain(m *testing.M) {
	// 50s watchdog — на 10s меньше чем типичный GitHub Actions job timeout
	// (60s для short tests, 180s для полных). Если watchdog сработал,
	// значит что-то зависло — лучше fail быстро чем висеть вечно.
	const watchdogTimeout = 50 * time.Second

	// Запускаем watchdog в отдельной goroutine. После timeout форсируем
	// выход с code 1 (test failure).
	timer := time.AfterFunc(watchdogTimeout, func() {
		//nolint:errcheck // os.Exit не возвращает
		os.Stderr.WriteString("WATCHDOG: test timeout after 50s, forcing exit. Leaked goroutines suspected.\n")
		os.Exit(1)
	})

	// Run tests
	code := m.Run()

	// Останавливаем watchdog (тесты завершились вовремя).
	timer.Stop()

	os.Exit(code)
}
