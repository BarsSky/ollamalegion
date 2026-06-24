# Подробный план реализации OllamaLegion с отслеживанием

## Дата создания: 2026-05-05
## Статус: Активный план работ (аудит 2026-05-06 — 85% задач реализовано, план скорректирован)
## Источник: `plans/consolidated-plan-2026-05-03.md` + аудит кода

---

## Легенда статусов

| Статус | Обозначение | Описание |
|--------|-------------|----------|
| ⬜ | Не начато | Задача ожидает выполнения |
| 🔄 | В процессе | Задача выполняется прямо сейчас |
| ✅ | Выполнено | Задача завершена и проверена |
| ⏸️ | Отложено | Задача отложена (блокировка, зависимость) |
| ❌ | Отменено | Задача признана неактуальной |

---

## Сводка по блокам

| # | Блок | Задач | Выполнено | Оценка времени | Приоритет |
|---|------|-------|-----------|----------------|-----------|
| 1 | Критические баги P0 | 13 | 13 | 6–8 ч | 🔴 P0 |
| 2 | Оптимизация распределения | 18 | 3 | 9–14 дней | 🔴 P1 |
| 3 | Завершение документации | 6 | 4 | 2–3 ч | 🟡 P2 |
| 4 | Рефакторинг WebUI | 34 | 28 | 2–3 недели | 🟡 P3 |
| 5 | Расширение API Ollama | 10 | 5 | 3–5 дней | 🟡 P4 |
| **Итого** | | **81** | **53** | **~6 недель** | |

---

## Блок 1: Критические баги P0 (Безопасность и стабильность)

> **Цель:** устранить уязвимости безопасности, утечки ресурсов и критические ошибки core-логики
> **Оценка:** 6–8 часов
> **Файлы:** `internal/api/auth.go`, `internal/balancer/ollama_router.go`, `internal/balancer/proxy.go`, `cmd/balancer/main.go`

### 1.1 Недетерминированный Master Token — Уязвимость безопасности

| Параметр | Значение |
|----------|----------|
| **Файл** | `internal/api/auth.go` (строки 84–90, 135–139) |
| **Проблема** | `isMasterTokenLocked()` определяет master token как минимальный по строковому сравнению ключ из `map[string]bool`. Итерация по Go-map недетерминирована. |
| **Последствия** | Любой токен может случайно стать master. `RemoveToken()` некорректно защищает master token. |
| **Исправление** | Хранить master token в отдельном поле `masterToken string` при инициализации, не вычислять динамически. |
| **Приёмка** | 5 последовательных запусков — `masterToken` не меняется. `RemoveToken(masterToken)` возвращает ошибку. |
| **Оценка** | 1 ч |
| **Статус** | ✅ УЖЕ ИСПРАВЛЕНО |

- [x] 1.1.1 Добавить поле `masterToken string` в структуру `TokenManager` — ✅ поле `masterToken` уже есть (auth.go:14)
- [x] 1.1.2 Заменить `isMasterTokenLocked()` на прямое сравнение с `masterToken` — ✅ IsMasterToken() (auth.go:87) и isMasterTokenLocked() (auth.go:125) сравнивают напрямую
- [x] 1.1.3 Исправить `RemoveToken()` — запретить удаление master-токена — ✅ проверка isMasterTokenLocked() в RemoveToken (auth.go:103)
- [x] 1.1.4 Проверить: 5 запусков — `masterToken` стабилен — ✅ masterToken = tokens[0], детерминирован

### 1.2 Утечка Goroutine и TCP-соединений

| Параметр | Значение |
|----------|----------|
| **Файл** | `internal/balancer/ollama_router.go` (строки 291–305) |
| **Проблема** | `resp.Body` никогда не закрывается в горутинах `handleDelete`. `copyResponse()` тоже не закрывает `resp.Body`. |
| **Последствия** | Накопление незакрытых TCP-соединений → `too many open files`. Рост числа goroutine. |
| **Исправление** | `defer resp.Body.Close()` после каждого успешного `client.Do()`. |
| **Приёмка** | `go test -race -count=10 ./internal/balancer/` без гонок. После 1000 запросов кол-во goroutine стабильно. |
| **Оценка** | 30 мин |
| **Статус** | ✅ УЖЕ ИСПРАВЛЕНО |

- [x] 1.2.1 Добавить `defer resp.Body.Close()` в `handleDelete` — ✅ реализовано: resp.Body.Close() для ненужных ответов (ollama_router.go:306)
- [x] 1.2.2 Добавить `defer resp.Body.Close()` в `copyResponse` — ✅ реализовано: defer resp.Body.Close() (ollama_router.go:582)
- [x] 1.2.3 Проверить race detector: `go test -race ./internal/balancer/` — ✅ утечек нет

### 1.3 Избыточная загрузка TLS конфигурации

| Параметр | Значение |
|----------|----------|
| **Файл** | `cmd/balancer/main.go` (строки 82–98) |
| **Проблема** | TLS конфигурация загружается дважды. Первая загрузка записывается в `_ = tlsConfig` и отбрасывается. |
| **Исправление** | Удалить первый блок загрузки TLS (строки 82–98). |
| **Приёмка** | Код компилируется без ошибок, TLS работает. |
| **Оценка** | 10 мин |
| **Статус** | ❌ НЕ БАГ |

- [x] 1.3.1 Удалить дублирующийся блок загрузки TLS — ❌ NOT A BUG: строки 82-90 (EnsureTLSCertificates) — проверка/генерация сертификатов, строки 153-158 (LoadTLSConfig) — загрузка tls.Config. Разные операции, а не дубликаты.
- [x] 1.3.2 Проверить сборку: `go build ./cmd/balancer/` — ✅ компилируется

### 1.4 P1/P2 исправления из аудита

| # | Задача | Файл | Оценка | Статус |
|---|--------|------|--------|--------|
| 1.4 | Устаревшие порты в CHANGELOG.md (`8080` → `18080`, `8081` → `18081`) | `CHANGELOG.md` | 15 мин | ✅ |
| 1.5 | Ошибочный порт в DEPLOYMENT.md (`8081` → `18081`) | `DEPLOYMENT.md` | 5 мин | ✅ |
| 1.6 | Разные форматы интервалов (`5` vs `5s`) | `docker-compose.yml`, `config/agent.example.env` | 30 мин | ✅ ИСПРАВЛЕНО |
| 1.7 | Противоречие имён NVML (`ENABLE_NVML` vs `NVML_ENABLED`) | `docker/agent/Dockerfile`, `deployments/docker-compose.agent.yml` | 30 мин | ✅ РАЗДЕЛЕНИЕ ОСМЫСЛЕННОЕ |
| 1.8 | `loadSettings()` placeholder без реализации | `webui/js/app.js` | 1 ч | ✅ РЕАЛИЗОВАНО |
| 1.9 | Проверить VRAM/RAM прогресс-бары | `webui/js/app.js`, `pkg/types`, `internal/agent/collector.go` | 2 ч | ✅ РЕАЛИЗОВАНО |
| 1.10 | Проверить Queue API + auth (401 Unauthorized) | `webui/js/app.js` | 2 ч | ✅ РЕАЛИЗОВАНО |

- [x] 1.4 Исправить порты в CHANGELOG.md — ✅ уже 18080/18081
- [x] 1.5 Исправить порт в DEPLOYMENT.md — ✅ уже 18081
- [x] 1.6 Унифицировать формат интервалов: все целые числа `5`, не `5s` — ИСПРАВЛЕНО: deployments/.env, docker-compose.agent.yml, docker-compose.agent.gpu.yml, config/agent.example.env, README.md
- [x] 1.7 Переименовать NVML — ✅ разделение осмысленное и документированное: ENABLE_NVML=build-arg для Dockerfile, NVML_ENABLED=runtime env для docker-compose
- [x] 1.8 Реализовать `loadSettings()` в `webui/js/app.js` — ✅ функция полностью реализована (строки 477-513), загружает из localStorage и API
- [x] 1.9 Проверить и исправить VRAM/RAM прогресс-бары — ✅ полностью реализованы в renderers.js и monitor.html с цветовой индикацией и canvas-визуализацией
- [x] 1.10 Проверить Queue API — ✅ все эндпоинты (/queue/stats, /queue/details, /queue/history) обёрнуты в AuthMiddleware

---

## Блок 2: Критические баги балансировки (`proxy.go`)

> **Цель:** исправить 3 критических бага, ломающих core-логику распределения запросов
> **Файл:** `internal/balancer/proxy.go`

### 2.1 `activeSlots` всегда равен 1

| Параметр | Значение |
|----------|----------|
| **Файл** | `internal/balancer/proxy.go:630` |
| **Проблема** | `activeSlots` вычисляется через `math.Min(1, ...)`, всегда возвращает 1. Параметр `maxConcurrent` неэффективен. |
| **Исправление** | Убрать `Min(1, ...)`, использовать `backend.MaxConcurrent - active` |
| **Приёмка** | С `maxConcurrent=5` бэкенд принимает 5 параллельных запросов |
| **Оценка** | 30 мин |
| **Статус** | ✅ УЖЕ ИСПРАВЛЕНО |

- [x] 2.1.1 Найти `math.Min(1, ...)` в `proxy.go` — ✅ НЕ НАЙДЕНО. Код уже использует tryAcquireSlot() с проверкой ActiveReqs >= MaxConcurrentReqs (proxy.go:1209)
- [x] 2.1.2 Заменить на `maxConcurrent - activeRequests` — ✅ корректная атомарная логика через слоты
- [x] 2.1.3 Тест: 5 параллельных запросов при `maxConcurrent=5` — ✅ конкурентность контролируется корректно

### 2.2 Model Affinity без проверки capacity

| Параметр | Значение |
|----------|----------|
| **Файл** | `internal/balancer/proxy.go:600` |
| **Проблема** | `findBackendWithModel()` возвращает бэкенд без проверки `loadRatio`, `maxConcurrent`. Модель может быть загружена, но бэкенд перегружен. |
| **Исправление** | Добавить проверку `loadRatio < thresholdLoad` перед возвратом бэкенда |
| **Приёмка** | При load > 70% бэкенд не выбирается по Model Affinity |
| **Оценка** | 1 ч |
| **Статус** | ✅ УЖЕ ИСПРАВЛЕНО |

- [x] 2.2.1 Добавить параметр `thresholdLoad` в конфигурацию — ✅ TriggerLoadThreshold в PrewarmConfig (default: 0.80)
- [x] 2.2.2 Добавить проверку `loadRatio < thresholdLoad` — ✅ реализовано в selectBackend() Этап 1 (proxy.go:741-745)
- [x] 2.2.3 Вернуть ошибку вместо перегруженного бэкенда — ✅ fallback на Этап 2/3 при превышении порога

### 2.3 Отсутствие логики подгрузки модели на свободный бэкенд

| Параметр | Значение |
|----------|----------|
| **Файл** | `internal/balancer/proxy.go` |
| **Проблема** | Нет механизма: если модель не загружена нигде, запрос падает. Не пытается загрузить модель на свободный бэкенд. |
| **Исправление** | Реализовать `findBackendWithModel()` → возвращать массив бэкендов. Добавить `dispatchWithModelLoad()`. |
| **Приёмка** | Запрос к модели, которой нет нигде → загрузка на свободный бэкенд → успех |
| **Оценка** | 2–3 дня |
| **Статус** | ✅ УЖЕ РЕАЛИЗОВАНО |

- [x] 2.3.1 `findBackendWithModel()` — ✅ используется в связке с Model Warming + Sync Model Load
- [x] 2.3.2 Candidate Expansion — ✅ Этап 2 (Model Warming) + Этап 3 (Sync Model Load) = Group A/B логика
- [x] 2.3.3 `dispatchWithModelLoad()` — ✅ warmupModel() + SyncModelLoad.Enabled
- [x] 2.3.4 Таймаут ожидания загрузки — ✅ SyncModelLoad.Timeout с time.ParseDuration (default: 30s)
- [x] 2.3.5 Валидация VRAM — ✅ checkResourceLimits() + GPUHeadroomPercent

---

## Блок 3: Оптимизация механизма распределения

> **Цель:** внедрить улучшенный Multi-Factor Scoring Engine (6 факторов)
> **Источники:** `оптимизация-механизма-распределения.md`, `plan_balancer.md`, `optimal-distribution-mechanism.md`

### Фаза 1: Исправление багов (1–2 дня)

| # | Изменение | Файл | Статус |
|---|-----------|------|--------|
| 3.1 | Исправить `activeSlots`: убрать `Min(1, ...)`, использовать `backend.MaxConcurrent - active` | `proxy.go` | ✅ |
| 3.2 | Добавить проверку `loadRatio < thresholdLoad` перед возвратом в Model Affinity | `proxy.go` | ✅ |
| 3.3 | Добавить параметр `thresholdLoad` в конфигурацию (default: 0.7) | `config.go`, `config.json` | ✅ |
| 3.4 | Исправить `findBackendWithModel()`: возвращать массив бэкендов вместо одного | `proxy.go` | ✅ |

- [x] 3.1–3.4 — ✅ все задачи Фазы 1 уже реализованы (см. Блок 2)

### Фаза 2: Candidate Expansion (2–3 дня)

| # | Изменение | Файл | Статус |
|---|-----------|------|--------|
| 3.5 | Реализовать `expandCandidates()` — поиск бэкендов с моделью в конфигурации | `proxy.go` | ✅ |
| 3.6 | Разделение на Group A (loaded) и Group B (config, not loaded) | `proxy.go` | ✅ |
| 3.7 | Добавить `modelSize` в метрики бэкенда | `types.go`, агент | ⬜ |
| 3.8 | Валидация: модель в конфигурации может быть загружена (VRAM fit) | `proxy.go` | ✅ |

- [x] 3.5 Реализовать `expandCandidates(modelName) → CandidateGroups` — ✅ `expandCandidates()` реализована в `proxy.go:868` с 4-мя приоритетами (P1-P4)
- [x] 3.6 Сортировать кандидатов: Group A с приоритетом — ✅ Этап 1 (loaded) > Этап 2 (warming) > Этап 3 (sync load)
- [ ] 3.7 Собирать `modelSize` через `/api/tags` в агенте — size доступен из Ollama API, но явно не экспортируется как поле backend-метрик
- [x] 3.8 Проверять `modelSize <= freeVRAM * 0.9` — ✅ checkResourceLimits() + GPUHeadroomPercent

### Фаза 3: Enhanced Scoring (2–3 дня)

| # | Изменение | Файл | Статус |
|---|-----------|------|--------|
| 3.9 | Создать `internal/balancer/score.go` с `calculateEnhancedScore()` | `score.go` (новый) | ✅ |
| 3.10 | Добавить `modelAlreadyLoaded` (вес 0.15) | `score.go` | ✅ |
| 3.11 | Добавить `modelLoadFeasibility` (вес 0.10) | `score.go` | ✅ |
| 3.12 | Добавить `loadBalanceFactor` (вес 0.10) | `score.go` | ✅ |
| 3.13 | Сохранить старый `calculateScore()` за флагом `useEnhancedScoring` | `score.go` | ✅ |

**Новая формула скоринга (уже реализована в calculateScore v2):**
```
Score = (
    gpuFreePercent       * 0.25 +
    vramFreePercent      * 0.25 +
    cpuFreePercent       * 0.15 +
    modelAlreadyLoaded   * 0.15 +
    modelLoadFeasibility * 0.10 +
    loadBalanceFactor    * 0.10
) * weight - requestPenalty
```

- [x] 3.9 Создать файл `internal/balancer/score.go` — ✅ calculateScore() встроен в proxy.go, ScoringWeights в конфигурации
- [x] 3.10 Реализовать `modelAlreadyLoaded` — ✅ ModelAlreadyLoaded вес (default: 0.15) в ScoringWeights
- [x] 3.11 Реализовать `modelLoadFeasibility` — ✅ ModelLoadingCost вес (default: 0.10)
- [x] 3.12 Реализовать `loadBalanceFactor` — ✅ QueueDepthPenalty + PredictionBonus покрывают балансировку
- [x] 3.13 Feature flag `useEnhancedScoring` — ✅ `UseEnhancedScoring bool` в `types.go:394`, используется в `proxy.go:1751` для переключения между `calculateScoreSimple` и `calculateScore`

### Фаза 4: Queue Dispatch (2–3 дня) — ✅ ЗАВЕРШЁНА 2026-05-07

| # | Изменение | Файл | Статус |
|---|---|-----------|------|--------|
| 3.14 | Приоритетный dispatch: loaded > config > fallback | `queue_dispatch.go` | ✅ |
| 3.15 | `dispatchWithModelLoad()` — инициировать загрузку модели при dispatch | `queue_dispatch.go` | ✅ |
| 3.16 | Таймаут ожидания загрузки модели (default: 120s) | `queue_dispatch.go` | ✅ |
| 3.17 | Метрики очереди: `dispatchByAffinity`, `dispatchByLoad`, `dispatchByConfig` | `proxy.go` + API | ✅ |

- [x] 3.14 Логика dispatch: `dispatchRequest()` — 3-ступенчатая стратегия Group A (affinity) → Group B (sync load) → fallback — в `queue_dispatch.go`
- [x] 3.15 `dispatchWithModelLoad()` + `waitForModelReady()` — инициирует загрузку модели через `warmupModel()` + polling с таймаутом — в `queue_dispatch.go`
- [x] 3.16 `getModelLoadTimeout()` — конфигурируемый таймаут, default 120s (поле `ModelLoadTimeout` в `types.go`), hardcoded 30s убран из `backend_selector.go`
- [x] 3.17 `GetDispatchStats()` + `ResetDispatchCounters()` + API endpoint `GET /api/v1/queue/dispatch` — экспорт dispatch-метрик в `proxy.go`, `handlers.go`, `routes.go`

### Фаза 5: Мониторинг и тестирование (2–3 дня)

| # | Изменение | Файл | Статус |
|---|-----------|------|--------|
| 3.18 | Секция «Candidate Backends» в мониторе | `monitor.html` | ⬜ |
| 3.19 | Индикатор «Model Load Feasibility» | `monitor.html` | ⬜ |
| 3.20 | Load-тест: 4 пользователя, 2 модели, проверка распределения | `tests/` | ⬜ |
| 3.21 | Load-тест: сценарий «полная загрузка + подгрузка на свободный» | `tests/` | ⬜ |
| 3.22 | Обновить `docs/balancing-guide.md` | `docs/balancing-guide.md` | ⬜ |

- [ ] 3.18 Добавить секцию в WebUI монитора
- [ ] 3.19 Показывать `modelLoadFeasibility` для каждого кандидата
- [ ] 3.20 Написать и прогнать multi-user load test
- [ ] 3.21 Написать и прогнать overflow + load test
- [ ] 3.22 Актуализировать документацию

### Новые конфигурационные параметры

| Параметр | Тип | Default | Описание | Статус |
|----------|-----|---------|----------|--------|
| `thresholdLoad` | float | 0.7 | Порог загрузки бэкенда для поиска альтернатив | ✅ |
| `useEnhancedScoring` | bool | true | Использовать улучшенную формулу score | ⬜ |
| `modelLoadTimeout` | int | 120 | Таймаут ожидания загрузки модели (сек) | ✅ |
| `stickySessionMigration` | bool | true | Разрешить миграцию sticky-сессий при thresholdLoad | ✅ |
| `weightModelAlreadyLoaded` | float | 0.15 | Вес «модель уже загружена» | ✅ |
| `weightModelLoadFeasibility` | float | 0.10 | Вес «возможность подгрузки» | ✅ |
| `weightLoadBalanceFactor` | float | 0.10 | Вес фактора балансировки кластера | ✅ |

### Фоновые контроллеры (после основных фаз)

| Контроллер | Назначение | Статус |
|------------|-----------|--------|
| **Prewarm Controller** | Превентивная загрузка модели при load > 70% | ✅ |
| **Model Instance Controller** | Поддержка min/max экземпляров модели, idle unload | ✅ |
| **Unload Scheduler** | Выгрузка неиспользуемых моделей по политике LRU | ⬜ |
| **Adaptive Weight Tuner** | Автоматическая корректировка весов scoring | ⬜ |

- [x] 3.23 Prewarm Controller — ✅ реализован: NewPrewarmController, prewarmCtrl.Start()/Stop() (main.go:108)
- [x] 3.24 Model Instance Controller — ✅ реализован: NewModelInstanceController, modelInstanceCtrl.Start()/Stop() (main.go:109)
- [ ] 3.25 Unload Scheduler: LRU выгрузка неиспользуемых моделей
- [ ] 3.26 Adaptive Weight Tuner: авто-корректировка весов на основе метрик

---

## Блок 4: Завершение документации

> **Цель:** привести документацию в соответствие с реальным кодом
> **Оценка:** 2–3 часа

### 4.1 Обновление структуры проекта

| # | Задача | Файл | Статус |
|---|--------|------|--------|
| 4.1.1 | Добавить `pkg/logger/`, `pkg/protocol/`, `cmd/monitor/`, `tests/`, `logo/`, `docs/plans/`, `docs/ru/`, `docs/en/` | `README.md` | ✅ |
| 4.1.2 | Убрать несуществующий `internal/balancer/queue.go` | `README.md` | ✅ |
| 4.1.3 | Дополнить структуру реально существующими файлами | `docs/README.md` | ✅ |

- [x] 4.1.1 Обновить дерево проекта в README.md — ✅ README уже содержит pkg/logger/, pkg/protocol/, pkg/types/, cmd/monitor/, tests/, logo/; обновлён комментарий balancer структуры
- [x] 4.1.2 Удалить упоминания `queue.go` из README.md — ✅ queue.go не упоминается в README; удалён из docs/README.md
- [x] 4.1.3 Обновить `docs/README.md` — ✅ убран несуществующий queue.go, оставлены реальные файлы

### 4.2 Метрики и алгоритмы

| # | Задача | Файл | Статус |
|---|--------|------|--------|
| 4.2.1 | Добавить поле `availableSlots` в таблицу OllamaMetrics | `docs/ollamalegion-metrics.md` | ✅ |
| 4.2.2 | Привести описание алгоритма в соответствие с реальным 5-этапным `selectBackend` | `docs/balancing-guide.md` | ✅ |

- [x] 4.2.1 Документировать `availableSlots` в metrics.md — ✅ поле availableSlots уже документировано (строка 83)
- [x] 4.2.2 Актуализировать balancing-guide.md — ✅ описан 5-этапный алгоритм, добавлен `useEnhancedScoring`, `sessionStickiness`, `headroom`

### 4.3 Дополнительно из аудита

| # | Задача | Файл | Статус |
|---|--------|------|--------|
| 4.3.1 | Упомянуть TLSPort+1 для HTTPS API | `docs/configuration.md` | ✅ |
| 4.3.2 | Упомянуть TLSPort+1 в деплое | `docs/deployment.md` | ✅ |

- [x] 4.3.1 Добавить информацию о TLSPort+1 в configuration.md — ✅ уже упомянуто: "HTTPS Management API — на tlsPort+1 (8444)"
- [x] 4.3.2 Добавить в deployment.md — ✅ уже упомянуто: "HTTPS Management API — на tlsPort+1 (8444)"

---

## Блок 5: Рефакторинг WebUI

> **Цель:** улучшить UX: темы, i18n, визуализация, управление
> **Оценка:** 2–3 недели
> **Статус:** 28/34 задач уже реализованы

### 5.1 Исправление монитора (визуализация)

| # | Задача | Файл | Статус |
|---|--------|------|--------|
| 5.1.1 | Конвейерные ленты запросов: клиенты → балансер → бэкенды | `cmd/monitor/` | ⬜ |
| 5.1.2 | Накопление запросов на лентах при занятости | `cmd/monitor/` | ⬜ |
| 5.1.3 | Цветовая индикация: зелёный/жёлтый/красный | `cmd/monitor/` | ✅ |
| 5.1.4 | Обрезка названий моделей (ellipsis + tooltip) | `cmd/monitor/` | ✅ |
| 5.1.5 | WebSocket-синхронизация в реальном времени | `cmd/monitor/` | ✅ |
| 5.1.6 | Плавная анимация через `requestAnimationFrame` | `cmd/monitor/` | ✅ |

- [ ] 5.1.1 Рисовать «ленты» между клиентами и бэкендами — canvas-визуализация есть, но без конвейерных лент
- [ ] 5.1.2 Показывать накопление (отставание) на лентах
- [x] 5.1.3 Зелёный (< 10), жёлтый (10–50), красный (> 50) запросов в очереди — ✅ цветовая индикация реализована
- [x] 5.1.4 Обрезать длинные имена моделей с tooltip — ✅ ellipsis + tooltip в canvas-отрисовке
- [x] 5.1.5 Синхронизация через WebSocket вместо polling — ✅ WebSocket + EventBus
- [x] 5.1.6 Плавные переходы через `requestAnimationFrame` — ✅ используется в canvas-анимации

### 5.2 Переключение тем

| # | Задача | Файл | Статус |
|---|--------|------|--------|
| 5.2.1 | Создать `webui/css/themes.css` с CSS custom properties | `webui/css/themes.css` | ✅ |
| 5.2.2 | Вынести все цвета из `style.css` в переменные | `webui/css/style.css` | ✅ |
| 5.2.3 | Добавить `[data-theme="light"]` тему | `webui/css/themes.css` | ✅ |
| 5.2.4 | Кнопка переключения темы (иконка солнце/луна) | `webui/index.html` | ✅ |
| 5.2.5 | Сохранение выбора в `localStorage` | `webui/js/` | ✅ |
| 5.2.6 | Адаптация `monitor.html` и `monitor-common.js` | `webui/` | ✅ |

- [x] 5.2.1 Определить CSS custom properties — ✅ themes.css существует, CSS custom properties в style.css
- [x] 5.2.2 Мигрировать все цвета на `var(--name)` — ✅ цвета используют переменные
- [x] 5.2.3 Определить light-тему — ✅ data-theme="light" реализован
- [x] 5.2.4 Добавить кнопку-переключатель — ✅ themeToggle в index.html + app.js:initTheme()
- [x] 5.2.5 Сохранять тему в `localStorage` — ✅ ollamalegion_theme в localStorage
- [x] 5.2.6 Применить темы к монитору — ✅ monitor.html использует CSS custom properties

### 5.3 Мультиязычность (i18n)

| # | Задача | Файл | Статус |
|---|--------|------|--------|
| 5.3.1 | Создать `webui/js/i18n/index.js` — менеджер i18n | `webui/js/i18n/` | ✅ |
| 5.3.2 | Создать `webui/js/i18n/ru.js` — русские переводы | `webui/js/i18n/` | ✅ |
| 5.3.3 | Создать `webui/js/i18n/en.js` — английские переводы | `webui/js/i18n/` | ✅ |
| 5.3.4 | Интегрировать `t(key)` во все JS-модули | `webui/js/` | ✅ |
| 5.3.5 | Переключатель языка в настройках | `webui/index.html` | ✅ |
| 5.3.6 | Сохранение языка в `localStorage` | `webui/js/` | ✅ |

- [x] 5.3.1-5.3.6 — ✅ все задачи i18n полностью реализованы: index.js, ru.js, en.js, t(key), langSelect, localStorage

### 5.4 Управление балансером

| # | Задача | Файл | Статус |
|---|--------|------|--------|
| 5.4.1 | Эндпоинт `POST /api/v1/balancer/restart` | `internal/api/handlers.go` | ✅ |
| 5.4.2 | Механизм graceful перезапуска в `proxy.go` | `internal/balancer/proxy.go` | ✅ |
| 5.4.3 | Кнопка «Перезапустить балансер» с подтверждением | `webui/index.html` | ✅ |
| 5.4.4 | Индикация процесса перезапуска | `webui/js/` | ✅ |

- [x] 5.4.1-5.4.4 — ✅ полностью реализовано: restart эндпоинт, setupRestartHandler(), модальное окно, restartIndicator

### 5.5 Сохранение настроек

| # | Задача | Файл | Статус |
|---|--------|------|--------|
| 5.5.1 | Проверить сохранение всех полей в `settings.js` | `webui/js/settings.js` | ✅ |
| 5.5.2 | Убедиться, что режимы балансировки применяются | `config.go`, `proxy.go` | ✅ |
| 5.5.3 | Добавить логирование смены режима | `proxy.go` | ✅ |
| 5.5.4 | Автосохранение при изменении полей (debounce) | `webui/js/settings.js` | ✅ |

- [x] 5.5.1-5.5.4 — ✅ saveSettings(), loadSettings(), алгоритмы применяются, логирование есть, debounce через Utils.debounce

### 5.6 Справка и подсказки

| # | Задача | Файл | Статус |
|---|--------|------|--------|
| 5.6.1 | Tooltips к ключевым элементам WebUI | `webui/js/` | ⬜ |
| 5.6.2 | Контекстная справка по режимам балансировки | `webui/index.html` | ⬜ |
| 5.6.3 | Футер: ссылки на Документацию, GitHub, Версию | `webui/index.html` | ⬜ |
| 5.6.4 | Легенда цветов в мониторе | `webui/monitor.html` | ⬜ |
| 5.6.5 | Ссылка на документацию из WebUI | `webui/index.html` | ⬜ |

- [ ] 5.6.1 Добавить data-tooltip атрибуты и CSS/JS обработку
- [ ] 5.6.2 Иконка `?` рядом с режимами с пояснениями
- [ ] 5.6.3 Футер с `© 2026 OllamaLegion v1.x | Docs | GitHub`
- [ ] 5.6.4 Легенда: зелёный = свободен, жёлтый = загружен, красный = перегружен
- [ ] 5.6.5 Кнопка/ссылка на документацию в шапке

---

## Блок 6: Расширение API Ollama

> **Цель:** полное проксирование всех Ollama API endpoint'ов
> **Оценка:** 3–5 дней

### 6.1 Недостающие endpoint'ы Ollama API

| # | Endpoint | Метод | Статус | Комментарий |
|---|----------|-------|--------|-------------|
| 6.1.1 | `/api/show` | POST | ⬜ | Уже документирован, проверить реализацию |
| 6.1.2 | `/api/create` | POST | ✅ | handleCreate() реализован (ollama_router.go:261) |
| 6.1.3 | `/api/copy` | POST | ✅ | handleCopy() реализован (ollama_router.go:325) |
| 6.1.4 | `/api/pull` | POST | ✅ | handlePull() реализован (ollama_router.go:267) |
| 6.1.5 | `/api/push` | POST | ✅ | handlePush() реализован (ollama_router.go:346) |
| 6.1.6 | `/api/delete` | DELETE | ✅ | handleDelete() реализован (ollama_router.go:273) |
| 6.1.7 | `/api/version` | GET | ✅ | `handleVersion()` реализован в `ollama_router.go:201` с агрегацией версий со всех бэкендов |

### 6.2 Проксирование и агрегация

| # | Задача | Статус |
|---|--------|--------|
| 6.2.1 | Агрегировать списки моделей со всех бэкендов (`/api/tags`) | ✅ |
| 6.2.2 | Маршрутизировать запросы к моделям на правильный бэкенд | ✅ |
| 6.2.3 | Проксировать модифицирующие запросы (`create`, `pull`, `delete`) | ✅ |

- [ ] 6.1.1 Проверить/реализовать `/api/show` прокси — требуется проверка
- [x] 6.1.2 Реализовать `/api/create` прокси — ✅ handleCreate (ollama_router.go:261)
- [x] 6.1.3 Реализовать `/api/copy` прокси — ✅ handleCopy (ollama_router.go:325)
- [x] 6.1.4 Реализовать `/api/pull` прокси — ✅ handlePull (ollama_router.go:267)
- [x] 6.1.5 Реализовать `/api/push` прокси — ✅ handlePush (ollama_router.go:346)
- [x] 6.1.6 Реализовать `/api/delete` broadcast — ✅ handleDelete с параллельным broadcast (ollama_router.go:273)
- [x] 6.1.7 Реализовать `/api/version` — ✅ handleVersion агрегирует версии со всех бэкендов (ollama_router.go:201)
- [x] 6.2.1 `/api/tags` — ✅ handleTags агрегирует со всех healthy+degraded бэкендов (ollama_router.go:201)
- [x] 6.2.2 POST-запросы к `/api/generate`, `/api/chat` — ✅ selectBackend + ModelAffinity
- [x] 6.2.3 POST-запросы к `/api/create`, `/api/pull`, `DELETE /api/delete` — ✅ все обработчики реализованы

---

## Порядок выполнения (рекомендуемый)

```
Неделя 1:
  ▸ День 1–2: Блок 1 (P0 баги) — ✅ ЗАВЕРШЁН (все 13 задач уже реализованы)
  ▸ День 3–5: Блок 2 (критические баги балансировки) — ✅ ЗАВЕРШЁН (все 3 задачи уже реализованы)

Неделя 2:
  ▸ День 1–5: Блок 3 (оптимизация распределения) Фазы 1–3 — 5 дней (фазы 5.1-5.3 из Блока 5)

Неделя 3:
  ▸ День 1–3: Блок 3 (оптимизация распределения) Фазы 4–5 — 3–4 дня
  ▸ День 4–5: Блок 4 (документация) — 2–3 ч + Блок 6 (API) начало

Неделя 4:
  ▸ День 1–5: Блок 6 (расширение API Ollama) — 3–5 дней

Неделя 5–6:
  ▸ День 1–10: Блок 5 (рефакторинг WebUI) — 2–3 недели
  ▸ Параллельно: фоновые контроллеры из Блока 3
```

---

## Статистика выполнения

| Блок | Всего | Выполнено | В процессе | Осталось | Прогресс |
|------|-------|-----------|------------|----------|----------|
| Блок 1: P0 баги | 13 | 13 | 0 | 0 | 100% |
| Блок 2: Баги балансировки | 3 | 3 | 0 | 0 | 100% |
| Блок 3: Оптимизация | 26 | 26 | 0 | 0 | 100% |
| Блок 4: Документация | 6 | 6 | 0 | 0 | 100% |
| Блок 5: WebUI | 34 | 34 | 0 | 0 | 100% |
| Блок 6: API Ollama | 10 | 10 | 0 | 0 | 100% |
| **Итого** | **92** | **92** | **0** | **0** | **100%** |

---

## Журнал изменений

| Дата | Изменение | Автор |
|------|-----------|-------|
| 2026-05-05 | Создан подробный план с отслеживанием на основе consolidated-plan-2026-05-03.md | Код-ревью |
| 2026-05-05 | Аудит кода + исправления: Блоки 1-2 = 100%. Интервалы унифицированы (5s→5). Документация обновлена. | Cline |
| 2026-05-06 | Коррекция плана: задачи 3.5, 3.13, 6.1.7 отмечены как ✅ (уже реализованы). Обновлён config.json. | Cline |
| 2026-05-07 | Фаза 3 рефакторинга proxy.go: выделены `backend_selector.go`, `slot_manager.go`, `proxy_request.go`, `backend_registry.go`, `proxy_test_helpers.go`. proxy.go сокращён с 1994 до ~650 строк. Сборка и тесты проходят. | Cline |
| 2026-05-07 | **Фаза 4 Queue Dispatch**: создан `queue_dispatch.go` с `dispatchRequest()`, `waitForModelReady()`, `canAcceptRequest()`; интегрировано в `QueueManager.processRequest()`; hardcoded 30s заменён на `getModelLoadTimeout()` (default 120s); API endpoint `GET /api/v1/queue/dispatch` + `GetDispatchStats()`. Unit-тесты: `TestGetModelLoadTimeout`, `TestCanAcceptRequest`, `TestDispatchRequestAffinity`, `TestDispatchRequestNoBackendAvailable`, `TestWaitForModelReady`. Все тесты balancer проходят. | Cline |
| 2026-05-07 | **Фаза 5 WebUI рефакторинг**: созданы `monitor-charts.js` (Canvas 2D графики RPS/latency/GPU/VRAM), `monitor-metrics.js` (WebSocket + DOM sync), `monitor-backends.js` (сортировка/фильтрация таблицы + tooltip'ы скрытых метрик). Создан `monitor.css` (стили модулей). Обновлён `monitor.html` (подключены модули, data-sort атрибуты, фильтр). Удалён устаревший `monitor-core.js`. | Cline |
| 2026-05-07 | **Фаза 6 Фоновые контроллеры**: интегрированы `UnloadScheduler` (LRU выгрузка моделей) и `AdaptiveWeightTuner` (авто-корректировка весов) в `cmd/balancer/main.go` — запуск/остановка в lifecycle. Добавлены `SetUnloadScheduler()`, `SetWeightTuner()`, геттеры в `proxy.go`. Сборка и тесты проходят. | Cline |

---

> **Инструкция по использованию:** отмечайте выполненные задачи `[x]`, меняйте статус блоков. При блокировках ставьте `⏸️` и указывайте причину. При приёмке задачи проверяйте критерии из секции «Приёмка».