// worker_token_fallback_r373_test.go — R373 (2026-11-01).
//
// Правило: админ-ручка воркера получает токен из записи бэкенда, а если его нет
// (узел добавлен вручную без cppWorkerApiToken) — из стекового окружения
// (IMAGEWORKER_API_TOKEN → CPPWORKER_API_TOKEN → LB_API_TOKEN).
//
// Живой факт, из которого правило выросло: `PUT …/cppworker-donor/role
// {"role":"standalone"}` давал в логе балансера
//
//	EnsureRPCNodeServing: узел не погашен, error: cppworker cppworker-donor: status 401
//
// каждые 15 секунд; роль на балансере сменилась, а донор продолжал считать себя
// донором и отвечал 409 на загрузку моделей.
package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR373_RecordTokenWins — токен записи главнее окружения.
func TestR373_RecordTokenWins(t *testing.T) {
	t.Setenv("CPPWORKER_API_TOKEN", "stack-token")
	if got := WorkerAPITokenOrEnvR373("record-token"); got != "record-token" {
		t.Errorf("токен = %q, want record-token (запись главнее окружения)", got)
	}
}

// TestR373_EnvFallback — без токена записи берём стековый.
func TestR373_EnvFallback(t *testing.T) {
	t.Setenv("CPPWORKER_API_TOKEN", "stack-token")
	if got := WorkerAPITokenOrEnvR373(""); got != "stack-token" {
		t.Errorf("токен = %q, want stack-token", got)
	}
	if got := WorkerAPITokenOrEnvR373("   "); got != "stack-token" {
		t.Errorf("токен = %q, want stack-token (пробелы — это «не задан»)", got)
	}
}

// TestR373_EnvPriority — порядок переменных как в gguf_backend_proxy.go.
func TestR373_EnvPriority(t *testing.T) {
	t.Setenv("IMAGEWORKER_API_TOKEN", "")
	t.Setenv("CPPWORKER_API_TOKEN", "worker-token")
	t.Setenv("LB_API_TOKEN", "lb-token")
	if got := WorkerAPITokenOrEnvR373(""); got != "worker-token" {
		t.Errorf("токен = %q, want worker-token (CPPWORKER_API_TOKEN раньше LB_API_TOKEN)", got)
	}

	t.Setenv("CPPWORKER_API_TOKEN", "")
	if got := WorkerAPITokenOrEnvR373(""); got != "lb-token" {
		t.Errorf("токен = %q, want lb-token (последний fallback)", got)
	}
}

// TestR373_NoTokenAnywhere — честная пустая строка: заголовок не ставится, и 401
// останется видимым.
func TestR373_NoTokenAnywhere(t *testing.T) {
	t.Setenv("IMAGEWORKER_API_TOKEN", "")
	t.Setenv("CPPWORKER_API_TOKEN", "")
	t.Setenv("LB_API_TOKEN", "")
	if got := WorkerAPITokenOrEnvR373(""); got != "" {
		t.Errorf("токен = %q, want пусто", got)
	}
}

// TestR373_BackendHelper — обёртка над записью бэкенда.
func TestR373_BackendHelper(t *testing.T) {
	t.Setenv("CPPWORKER_API_TOKEN", "stack-token")

	if got := workerAPITokenForBackendR373(nil); got != "stack-token" {
		t.Errorf("nil-запись: токен = %q, want stack-token", got)
	}
	if got := workerAPITokenForBackendR373(&types.Backend{CppWorkerApiToken: "own"}); got != "own" {
		t.Errorf("токен = %q, want own", got)
	}
	if got := workerAPITokenForBackendR373(&types.Backend{}); got != "stack-token" {
		t.Errorf("пустая запись: токен = %q, want stack-token", got)
	}
}
