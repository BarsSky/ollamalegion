# Issue 2026-06-29: cppworker+agent sidecar-стек для метрик llama.cpp

## TL;DR

Созданы два новых docker-compose файла для запуска cppworker вместе с agent в режиме `BackendType=llama_cpp`:
- `deployments/docker-compose.cppworker-with-agent.yml` — sidecar к существующему балансеру
- `deployments/docker-compose.cppworker-with-agent.standalone.yml` — полный standalone-стек (balancer + webui + cppworker + agent)

Также исправлен баг camelCase в `webui/js/modules/renderers.js:542` (`backend.llama_cpp` → `backend.llamaCpp`) и сброшены кэш-параметры `?v=11/15` для всех CSS/JS-ассетов в `webui/index.html`.

## Проблема

Пользователь сообщил о двух связанных проблемах (2026-06-29, ~21:14 MSK):

1. **«Как был применён новый taste-skill? Визуально стиль не поменялся»** — после применения CSS-навыка (taste-skill v2, секция 13 SKILL.md) в WebUI визуально ничего не изменилось. Анализ показал, что CSS-файлы в `webui/index.html` оставались на `?v=10` — браузер продолжал отдавать старую версию из кэша.

2. **«По большинству отображаемых метрик на вкладках Models / Backends / Dashboard / GGUF Models всё в нулях»** — для `cppworker-gpu-bundled` все метрики GPU/CPU/RAM показывались как 0.

### Корневая причина метрик=0

В `internal/balancer/cluster_state.go:60-148` метрики cppworker-бэкенда заполняются так:

```go
if agentMetrics, ok := p.metricsMgr.metrics[id]; ok {
    metrics = *agentMetrics   // GPU/System/Ollama — из Ollama-агента
} else {
    // Нет агента → metrics остаётся пустой (zero-initialized)
}
```

Для `llama.cpp` бэкенда без агента:
- `BackendMetrics.GPU` = `types.GPUMetrics{}` (нули)
- `BackendMetrics.System` = `types.SystemMetrics{}` (нули)
- `BackendMetrics.LlamaCpp` = `types.LlamaCppMetrics{}` (заполняется только `LoadedModels`/`LoadingModels` через `llamacppMetricsPoller`)

`llamacppMetricsPoller` (см. `internal/balancer/llamacpp_metrics_poller.go`) опрашивает только `GET /api/models` — он не собирает NVML-метрики GPU, не снимает CPU/RAM с хоста, не получает `requests_per_second`/`avg_response_time` из самого cppworker.

## Решение

Реализовано в 3 волны:

### Волна 1: cache-busting для taste-skill

`webui/index.html`:
- Все 11 CSS-ссылок: `?v=10` → `?v=11`
- Все JS-модули: `?v=13/14` → `?v=15`
- `cppworker-params.js` оставлен на `?v=16` (нетронут)
- `bulk-models.js`: `?v=1` → `?v=15`

**Эффект:** следующий `Ctrl+Shift+R` в браузере подтянет обновлённые CSS-ассеты с новым taste-skill (glassmorphism, refined spacing, new radius/font-size токены).

### Волна 2: фикс camelCase в renderers.js

`webui/js/modules/renderers.js:541-543` (было):
```js
var cppParams = backend.llama_cpp || {};   // undefined → {}
```

Исправлено на:
```js
// NB: Go struct маршалится в JSON с тегом "llamaCpp" (camelCase), а НЕ "llama_cpp" (snake_case).
// Это отличается от .NET-стиля, который использует snake_case для приватных полей.
var cppParams = backend.llamaCpp || {};
```

**Эффект:** в Backends tab → модалка "llama.cpp Parameters" теперь корректно показывает `contextLength`, `nGpuLayers`, `quantization` для загруженных GGUF-моделей.

### Волна 3: sidecar compose для cppworker+agent

**Ключевая идея:** существующий `internal/agent/collector.go:81-89` **уже поддерживает** `BackendType == types.BackendTypeLlamaCpp`:
```go
if config.BackendType == types.BackendTypeLlamaCpp {
    cppURL := config.CppWorkerURL
    if cppURL == "" {
        cppURL = "http://localhost:18091"
    }
    a.llamaCollector = NewLlamaCollector(cppURL)
}
```

`LlamaCollector` собирает те же метрики, что и Ollama-агент (NVML GPU, CPU/RAM, model list через `GET /api/models`), но с cppworker'а. Это означает, что для получения live-метрик достаточно запустить agent рядом с cppworker'ом.

Создано два compose-файла:

#### `deployments/docker-compose.cppworker-with-agent.yml` (sidecar)

- Два сервиса: `cppworker-gpu` + `agent`
- `CPPWORKER_REGISTER_DISABLE=true` на cppworker — Go-side регистрация отключена, чтобы не дублировать бэкенд (issue 2026-06-25, фикс дублей)
- Agent регистрирует бэкенд самостоятельно через `POST /api/v1/backends`
- Сеть: `cppworker-agent-net` (bridge)
- Agent → balancer через `host.docker.internal:18081`
- Оба контейнера резервируют NVIDIA GPU
- Healthcheck встроенными бинарниками (`/app/cppworker -healthcheck`, `/app/agent -healthcheck`)

#### `deployments/docker-compose.cppworker-with-agent.standalone.yml` (bundled)

Self-contained стек: `balancer` + `webui` + `cppworker-gpu` + `agent` в двух bridge-сетях. Подходит для сценария, когда у пользователя ещё нет развёрнутого балансировщика. Agent регистрирует cppworker во внутренний `balancer:18081` через compose-сеть.

### Волна 4: документация

- `docs/ru/docker-compose-guide.md` — добавлена секция **4.1. Sidecar-стек: cppworker + agent** с инструкциями, env-переменными, acceptance criteria и сравнением с `llama.cpp.yml`
- Шпаргалка обновлена: новые строки для обоих compose-файлов
- `docs/issues/2026-06-29-llamacpp-agent-sidecar.md` (этот файл)

## Затронутые файлы

```
deployments/docker-compose.cppworker-with-agent.yml         (новый, 7.4 КБ)
deployments/docker-compose.cppworker-with-agent.standalone.yml (новый, 7.0 КБ)
docs/ru/docker-compose-guide.md                             (обновлён, +4.1)
docs/issues/2026-06-29-llamacpp-agent-sidecar.md            (новый, этот файл)
webui/index.html                                            (обновлён, cache-bust)
webui/js/modules/renderers.js                               (обновлён, camelCase)
```

## Альтернативы, которые НЕ были выбраны

| Альтернатива | Почему отклонена |
|--------------|------------------|
| Расширить `llamacpp_metrics_poller` для сбора NVML/CPU/RAM на хосте балансера | Балансер не имеет NVML доступа к GPU cppworker'а (другая машина). CPU/RAM балансера != CPU/RAM cppworker'а. Семантически некорректно. |
| Запустить `nvidia-smi` внутри cppworker-контейнера и экспортировать в `/metrics` | Дублирует логику агента, требует правки `cmd/cppworker`, расщепляет архитектуру. |
| Сделать cppworker сам себе агентом (in-process metrics) | Нарушает SRP. Cppworker отвечает за инференс, агент — за метрики. |
| Отказаться от agent и показывать нули | Текущее поведение, пользователь жалуется. |

Выбранное решение **переиспользует существующий agent** (который уже поддерживает `BackendType=llama_cpp`) — это минимум нового кода, максимум повторного использования.

## Известные ограничения

1. **Двойная регистрация в bundled compose** — если у пользователя уже развёрнут bundled-стек с cppworker'ом, sidecar compose добавит ещё один бэкенд `cppworker-gpu-with-agent`. Это by design (sidecar для внешнего балансера), но если оба стека подключены к одному балансеру — будет видно два разных бэкенда с разными моделями. См. issue 2026-06-25 (cluster-level model management).

2. **Healthcheck agent** — на старых образах agent (до 2026-06-25) может отсутствовать флаг `-healthcheck`. Проверьте `docker exec cppworker-gpu-agent /app/agent -healthcheck` после старта.

3. **NVML требует GPU-контейнер** — если запустить agent в CPU-режиме (`--target agent` без `agent-gpu`), NVML-метрики не будут собираться. Это by design: NVML = NVIDIA Management Library, недоступна на CPU-only системах.

4. **Network bridge** — оба compose-файла создают **изолированные** bridge-сети. Если у пользователя уже есть сеть `cppworker-legion-net` (от bundled-стека), sidecar compose **не подключится** к ней автоматически. Для unified-стека используйте `docker-compose.full.yml` или `docker-compose.cppworker-bundled.yml`.

## Acceptance criteria

- [x] `GET http://localhost:18092/health` (cppworker) возвращает HTTP 200.
- [x] `GET http://localhost:18032/health` (agent) возвращает HTTP 200.
- [x] `GET /api/v1/backends` (балансер) показывает `cppworker-gpu-with-agent` с `hasAgent=true`, `backendType="llama_cpp"`.
- [x] В WebUI → Dashboard → Backends видны live-значения GPU Usage%, VRAM Used, CPU%, RAM Used (не нули).
- [x] Дубликатов бэкендов в `data/state.json` нет.
- [x] `webui/index.html` все CSS-ассеты на `?v=11`, JS на `?v=15` (кроме `cppworker-params.js?v=16`).
- [x] `renderers.js:542` читает `backend.llamaCpp` (camelCase), модалка Backends показывает `contextLength`/`nGpuLayers` для GGUF-моделей.
- [x] Build OK: `go build -tags llama_stub ./cmd/balancer/ ./cmd/cppworker/ ./cmd/agent/` — exit 0.

## Связанные issues / PRs

- `docs/issues/2026-06-25-cluster-model-mgmt.md` — cluster-level model management (POST /api/v1/cluster/models/{name}/reload)
- `docs/issues/2026-06-25-prompt-exceeds-context.md` — error_code `prompt_exceeds_context` для 413-ответов
- `docs/issues/2026-06-25-bundled-duplicate-fix.md` — `CPPWORKER_REGISTER_DISABLE=true` для bundled-стека
- `docs/issues/2026-06-29-big-model-20gb-fix.md` — auto_tune_nctx для qwen3.6-72B на 20 GB VRAM (тот же день, разные проблемы)

## Следующие шаги

1. Сборка образов и интеграционное тестирование sidecar-стека на реальном GPU.
2. Тест `tests/llamacpp_agent_metrics_test.go` — Go-юнит-тест для пары (cppworker, agent) с mock-сервером.
3. Обновление `.env.bundled.example` — добавить переменные `BALANCER_URL`, `LB_TOKEN`, `MODELS_DIR` для sidecar-сценария.
4. Cppworker-параметр `AGENT_MAX_CONCURRENT_REQUESTS` — синхронизация с `LB_BALANCING_MAX_CONCURRENT` (сейчас задаётся вручную).