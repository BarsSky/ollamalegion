package api

import (
	"ollama-loadbalancer/pkg/types"
)

// backendEffectivePort — возвращает port, по которому выполняется де-дупликация
// для бэкенда. Приоритет: OllamaPort (если > 0), иначе CppWorkerPort.
//
// Зачем: тестовые бэкенды задают только OllamaPort (11434/11435), а bundled-compose
// cppworker задаёт только CppWorkerPort (18092). Использование ОДНОГО порта как
// ключа dedup приводит к тому, что бэкенды без заданного этого порта схлопываются
// в один (host, 0). Эта функция выбирает непустой порт, чтобы де-дуп работал
// корректно в обоих сценариях.
//
// Используется в Backend-типах (types.Backend), где есть и OllamaPort, и CppWorkerPort.
// Для BackendMetrics (cppworker) — берётся только CppWorkerPort.
func backendEffectivePort(ollamaPort, cppWorkerPort int) int {
	if ollamaPort > 0 {
		return ollamaPort
	}
	return cppWorkerPort
}

// dedupBackendsByHostPort — де-дупликация бэкендов по (host, port).
//
// Проблема (2026-06-30): один физический бэкенд (cppworker) может быть
// зарегистрирован в балансировщике несколько раз:
//   - shell-script register-with-balancer.sh (id="cppworker-gpu-bundled")
//   - Go-side balancer_register.go cppworker'а (id="cppworker-gpu")
//   - agent внутри compose-стека (id="<agent-id>")
//
// Все три указывают на host="cppworker-gpu", port="18092" — это один
// и тот же физический контейнер. WebUI показывал 2-3 записи в списке
// бэкендов/агентов, что сбивало пользователя с толку и вызывало race
// condition в selectBackend.
//
// Логика:
//  1. Группируем бэкенды по ключу (host, port) — это уникальный
//     идентификатор физического эндпоинта.
//  2. Для каждой группы оставляем кандидата с наивысшим приоритетом:
//     (a) preferAgent=true И кандидат с hasAgent=true → он побеждает
//     (агент даёт реальные GPU/VRAM метрики).
//     (b) иначе первый встретившийся — порядок итерации переданного
//     среза сохраняется (вызывающий код контролирует порядок
//     сортировкой).
//
// Возвращает НОВЫЙ срез; порядок = порядок первого вхождения ключа
// в исходном срезе.
//
// Используется в:
//   - listBackends (GET /api/v1/backends)
//   - agentStatsHandler (GET /api/v1/agents/stats)
//   - listLoadedClusterModels (GET /api/v1/cluster/models/loaded)
//   - handleGgufBackends (GET /api/v1/gguf/backends) — рефактор существующего кода.
func dedupBackendsByHostPort[T any](backends []T, host func(T) string, port func(T) int, hasAgent func(T) bool, preferAgent bool) []T {
	if len(backends) == 0 {
		return backends
	}

	type bmKey struct {
		host string
		port int
	}
	keyOf := func(t T) bmKey { return bmKey{host: host(t), port: port(t)} }

	// Первый проход: для каждого ключа выбираем кандидата.
	candidateByKey := make(map[bmKey]T)
	for _, b := range backends {
		key := keyOf(b)
		if existing, ok := candidateByKey[key]; ok {
			// Приоритет: preferAgent + hasAgent > первый встретившийся.
			if preferAgent && !hasAgent(existing) && hasAgent(b) {
				candidateByKey[key] = b
			}
			continue
		}
		candidateByKey[key] = b
	}

	// Второй проход: восстанавливаем порядок исходного среза.
	seen := make(map[bmKey]bool, len(candidateByKey))
	result := make([]T, 0, len(candidateByKey))
	for _, b := range backends {
		key := keyOf(b)
		if seen[key] {
			continue
		}
		if cand, ok := candidateByKey[key]; ok {
			result = append(result, cand)
			seen[key] = true
		}
	}
	return result
}

// BackendMetricsDeDupKey — извлекает (host, port, hasAgent) из BackendMetrics
// для де-дупликации. Используется в helpers ниже.
func BackendMetricsDeDupKey(bm types.BackendMetrics) (host string, port int, hasAgent bool) {
	return bm.Host, bm.CppWorkerPort, bm.HasAgent
}