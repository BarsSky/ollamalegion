// R60.29 (2026-09-10): TestMain watchdog removed.
//
// R52.4b добавил watchdog 50s как kill switch для pre-existing
// leaked goroutines. С тех пор watchdog:
//
//   - false-positive срабатывал в Windows + AV (тесты занимали 50-60s
//     из-за file system + Defender overhead, watchdog килил на 50s)
//   - false-positive срабатывал в CI ubuntu-latest после 50s на полном
//     test suite (даже в Linux leaked goroutines от R52.4 known issue
//     дают 60-90s общую длительность)
//
// Решение R60.29: убрать watchdog. Test runner завершается
// естественно (CI timeout 5min per job — реальная защита). Если
// реальный hang — упадёт по `go test -timeout 5m`.
//
// Pre-existing leaked goroutines НЕ блокируют CI при тестах с
// `-race -timeout 240s` (стандартный CI конфиг), они просто
// "noisy" в финальном log report.
//
// Если реальные hangs вернутся — добавить обратно watchdog
// с LB_TEST_WATCHDOG_SEC=180 env override (opt-in per run).
package balancer

// TestMain intentionally empty — R60.29 removes R52.4b watchdog.
// Tests run without kill switch; CI timeout (5min per job) is the
// real protection against infinite hangs.
