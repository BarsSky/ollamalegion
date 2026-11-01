// worker_token_fallback_r373.go — R373 (2026-11-01): стековый токен воркера для
// записей, зарегистрированных вручную.
//
// ЖИВОЙ ДЕФЕКТ (двухузловой стенд). Донор добавлен руками:
//
//	POST /api/v1/backends {"id":"cppworker-donor","host":"100.64.0.7", ...}
//
// В теле не было `cppWorkerApiToken`, поэтому запись осталась без токена. Дальше
// балансер не смог выполнить НИ ОДНОЙ админ-ручки на этом узле:
//
//	PUT /api/v1/backends/cppworker-donor/role {"role":"standalone"}
//	→ лог каждые 15 с: «EnsureRPCNodeServing: узел не погашен,
//	                   error: cppworker cppworker-donor: status 401»
//
// Следствие: роль на балансере сменилась, а воркер донора продолжал считать себя
// донором (rpc_server_enabled=true) и отказывал в загрузке моделей с
// `409 donor_cannot_host_models` — оператор снял роль в панели, а узел остался
// занятым. Токен при этом в системе ЕСТЬ (стековый CPPWORKER_API_TOKEN), он
// просто не доехал до записи.
//
// ПОЧЕМУ ENV-ЦЕПОЧКА, А НЕ ОДНА ПЕРЕМЕННАЯ. Ровно такую же цепочку уже
// использует internal/api/gguf_backend_proxy.go (токен бэкенда →
// IMAGEWORKER_API_TOKEN → CPPWORKER_API_TOKEN → LB_API_TOKEN): в стенде один
// стековый токен на все воркеры, и разводить вторую логику выбора нельзя.
package balancer

import (
	"os"
	"strings"

	"ollama-loadbalancer/pkg/types"
)

// workerTokenEnvChainR373 — переменные окружения, из которых берётся стековый
// токен воркера, в порядке приоритета.
var workerTokenEnvChainR373 = []string{"IMAGEWORKER_API_TOKEN", "CPPWORKER_API_TOKEN", "LB_API_TOKEN"}

// WorkerAPITokenOrEnvR373 — токен для админ-ручек воркера: сначала токен записи
// бэкенда, затем стековый из окружения.
//
// Пустая строка означает «токена нет»: тогда заголовок не ставится, и воркер без
// авторизации запрос пропустит, а воркер с авторизацией ответит 401 — это
// останется видимым, а не проглоченным.
func WorkerAPITokenOrEnvR373(backendToken string) string {
	if t := strings.TrimSpace(backendToken); t != "" {
		return t
	}
	for _, name := range workerTokenEnvChainR373 {
		if t := strings.TrimSpace(os.Getenv(name)); t != "" {
			return t
		}
	}
	return ""
}

// workerAPITokenForBackendR373 — то же для записи бэкенда.
func workerAPITokenForBackendR373(b *types.Backend) string {
	if b == nil {
		return WorkerAPITokenOrEnvR373("")
	}
	return WorkerAPITokenOrEnvR373(b.CppWorkerApiToken)
}
