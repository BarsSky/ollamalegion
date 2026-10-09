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

// backendDedupPort — порт, по которому бэкенд участвует в де-дупликации.
//
// R-MultiHost (2026-10-07). Дефект на живой паре из двух машин: image-бэкенд
// (image_cpp) НЕ имеет ни Ollama-, ни llama.cpp-поверхности, но в записи всё
// равно лежит ollamaPort=11434 — это дефолт, который подставляет
// agentRegisterHandler. Ключ де-дупликации получался (host, 11434), и на машине,
// где текстовый и image-бэкенд объявляют ОДИН И ТОТ ЖЕ host (так делает
// BACKEND_HOST=192.0.2.11 сразу для обоих воркеров), ключи совпадали.
//
// Следствие в WebUI: из двух записей оставалась одна (побеждал меньший ID —
// CPPWORKER-34), а image-бэкенд «мигал»: он появлялся на секунды, когда запись
// пересоздавалась саморегистрацией sdworker'а (там ollamaPort ещё 0 → ключ
// (host, 0) не совпадал), и исчезал после обновления от встроенного агента,
// который проставляет ollamaPort=11434.
//
// Для image-бэкенда ключ — его собственный порт (EffectiveImagePort), который
// не пересекается ни с 11434, ни с портом cppworker на том же хосте.
func backendDedupPort(b types.Backend) int {
	if b.Type == types.BackendTypeImage {
		return b.EffectiveImagePort()
	}
	return backendEffectivePort(b.OllamaPort, b.CppWorkerPort)
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
	return dedupBackendsByEndpoint(
		backends,
		func(t T) endpointKey { return endpointKey{Host: host(t), Port: port(t)} },
		hasAgent,
		preferAgent,
	)
}

// endpointKey — идентичность ФИЗИЧЕСКОГО эндпоинта.
//
// R-MultiHost (2026-10-07): к (host, port) добавлен NodeAddr — адрес узла, с
// которого пришла регистрация. Имя хоста идентичностью не является: на двух
// машинах с одинаковым compose контейнеры называются одинаково, поэтому
// `imageworker` (машина 1) и `IMAGEWORKER-34` (машина 2) дали один и тот же
// ключ (imageworker, 18093). Де-дупликация схлопывала их в одну запись, и
// половина кластера пропадала из WebUI — со стороны это выглядело как
// «карточка мигает»: какая из двух победит, зависело от того, у кого в этот
// момент выставлен hasAgent.
//
// Пустой NodeAddr (запись, созданная до этой ревизии, или созданная вручную из
// WebUI) остаётся отдельным значением ключа: показать лишний дубль на переходный
// период безопаснее, чем молча спрятать целую машину.
type endpointKey struct {
	Host     string
	Port     int
	NodeAddr string
}

// dedupBackendsByEndpoint — де-дупликация по полной идентичности эндпоинта.
// Правило выбора кандидата то же, что было в dedupBackendsByHostPort:
// preferAgent + hasAgent побеждает, иначе — первый встретившийся.
func dedupBackendsByEndpoint[T any](backends []T, key func(T) endpointKey, hasAgent func(T) bool, preferAgent bool) []T {
	if len(backends) == 0 {
		return backends
	}

	// Первый проход: для каждого ключа выбираем кандидата.
	candidateByKey := make(map[endpointKey]T)
	for _, b := range backends {
		k := key(b)
		if existing, ok := candidateByKey[k]; ok {
			// Приоритет: preferAgent + hasAgent > первый встретившийся.
			if preferAgent && !hasAgent(existing) && hasAgent(b) {
				candidateByKey[k] = b
			}
			continue
		}
		candidateByKey[k] = b
	}

	// Второй проход: восстанавливаем порядок исходного среза.
	seen := make(map[endpointKey]bool, len(candidateByKey))
	result := make([]T, 0, len(candidateByKey))
	for _, b := range backends {
		k := key(b)
		if seen[k] {
			continue
		}
		if cand, ok := candidateByKey[k]; ok {
			result = append(result, cand)
			seen[k] = true
		}
	}
	return result
}

// BackendMetricsDeDupKey — извлекает (host, port, hasAgent) из BackendMetrics
// для де-дупликации. Используется в helpers ниже.
func BackendMetricsDeDupKey(bm types.BackendMetrics) (host string, port int, hasAgent bool) {
	return bm.Host, bm.CppWorkerPort, bm.HasAgent
}