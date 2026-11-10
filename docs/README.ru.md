# OllamaLegion - документация

> [English](README.md) | **Русский** (текущий)
>
> Оглавление всей документации проекта. Один документ - один файл в каталоге своей темы.
> Соглашение об именах: `<имя>.md` - английская версия, `<имя>.ru.md` - русская.

Русский язык - исходный для проекта: часть документов пока существует только на нём (прочерк `-` в колонке English означает, что перевод ещё не готов). Архивные материалы (аудиты, отчёты раундов, research-заметки) лежат в [`archive/`](archive/README.ru.md) на языке оригинала.

## Содержание

- [Начало работы](#начало-работы)
- [Архитектура](#архитектура)
- [Эксплуатация](#эксплуатация)
- [API и справочники](#api-и-справочники)
- [Возможности](#возможности)
- [Релизы](#релизы)
- [Разработка](#разработка)
- [Планы и отчёты сессий](#планы-и-отчёты-сессий)
- [Архив](#архив)

## Начало работы

Установка, настройка и запуск стека.

| Документ | Русский | English | О чём |
|---|---|---|---|
| **v0.7.245: размышления не в content** | [RU](releases/v0.7.245-release-notes.ru.md) | - | R485: сторож больше не снимает удержание, вызванное размышлениями — иначе размышления уходили в `content` и клиент сохранял их как прошлый ответ модели («замыкается в себя»). Поле `hold_kept_reasoning`, событие в кольце воркера, аварийный тумблер. |
| **v0.7.244: разбор «это стрим?» и транспорт** | [RU](releases/v0.7.244-release-notes.ru.md) | - | R484: один разбор поля `stream` с явным дефолтом поверхности и фактический транспорт (`stream`/`json`) в `request trace` и прокси-логах. |
| **v0.7.243: где копился ответ** | [RU](releases/v0.7.243-release-notes.ru.md) | - | R483: `first_byte_ms` в трассировке (воркер/балансер/клиент) и кольцо событий воркера без 5xx (`hold_released`, `client_gone`, `client_cancelled`, `unparsed_tool_markup`, `turn_retry`) в `failures.json`. |
| **v0.7.242: инструменты в reasoning и поток** | [RU](releases/v0.7.242-release-notes.ru.md) | - | R482: вызов инструмента ищется и внутри блока размышлений, удержание вывода получило сторож (20 с / 32 КБ), финальный хвост режется на части — «одним куском» больше не приходит. |
| **Установка и сборка** | [RU](getting-started/installation.ru.md) | [EN](getting-started/installation.md) | Требования, сборка в Docker, локальная сборка, stub-режим. |
| **Развёртывание** | [RU](getting-started/deployment.ru.md) | [EN](getting-started/deployment.md) | Развёртывание стека: профили, воркеры, окружения, порты. |
| **Конфигурация** | [RU](getting-started/configuration.ru.md) | [EN](getting-started/configuration.md) | config.json балансера, env-переопределения, nginx и настройки WebUI. |
| **Пресеты под железо** | [RU](getting-started/hardware-presets.ru.md) | [EN](getting-started/hardware-presets.md) | Готовые .env-пресеты для RTX 30xx/40xx/50xx и A10. |

## Архитектура

Как устроены маршрутизация, раскладка и вывод.

| Документ | Русский | English | О чём |
|---|---|---|---|
| **Жизненный цикл запроса (R402)** | [RU](architecture/request-lifecycle.ru.md) | [EN](architecture/request-lifecycle.md) | Обязательная схема: окно контекста (на слот и суммарное), причины завершения, usage, реестр отказов, карта кода. |
| **Балансировка и сессии** | [RU](architecture/balancing-guide.ru.md) | [EN](architecture/balancing-guide.md) | Скоринг, выбор бэкенда, привязка сессий и моделей. |
| **Изоляция типов бэкендов** | [RU](architecture/backend-type-isolation.ru.md) | [EN](architecture/backend-type-isolation.md) | Как бэкенды llama.cpp и Ollama живут в одном кластере. |
| **RPC-координатор** | [RU](architecture/rpc-coordinator.ru.md) | [EN](architecture/rpc-coordinator.md) | Распределённый вывод через llama.cpp RPC: роли, состояние, лимиты. |
| **Виртуальные модели** | [RU](architecture/virtual-router.ru.md) | [EN](architecture/virtual-router.md) | Виртуальные модели alias-on-pool и правила маршрутизации. |
| **Раскладка слоёв** | [RU](architecture/layer-sharding.ru.md) | [EN](architecture/layer-sharding.md) | Раскладка слоёв по бэкендам, контейнеры, параметры. |
| **Версия llama.cpp: что зафиксировано** | [RU](architecture/llama-cpp-pin.ru.md) | - | b10731 (2026-09-01) ↔ b11844 (2026-10-23, 1113 коммитов): что даст обновление, проверка совместимости моста, чек-лист. |
| **Правила RPC-раскладки** | [RU](architecture/rpc-layout-rules.ru.md) | [EN](architecture/rpc-layout-rules.md) | Правила, добытые живыми падениями: один коммит, архитектура, флаг. |
| **Канал между узлами** | [RU](architecture/rpc-link-bandwidth.ru.md) | [EN](architecture/rpc-link-bandwidth.md) | Как измеряется скорость канала между узлами и где хранится. |
| **C-bridge API** | [RU](architecture/bridge-api.ru.md) | [EN](architecture/bridge-api.md) | Мост Go <-> C к llama.cpp: функции, структуры, контракт. |
| **Контракт балансер <-> cppworker** | [RU](architecture/balancer-cppworker-contract.ru.md) | [EN](architecture/balancer-cppworker-contract.md) | Сетевой контракт балансера и воркера, поле за полем. |
| **Долги проекта (реестр)** | [RU](architecture/debts.ru.md) | - | Единый список того, что осознанно не сделано: P0/P1/P2, продуктовые решения, внешние проверки, инфраструктура — с file:line и тем, что закрывает. || **Рефакторинг: два мозга, четыре окна, четыре очереди** | [RU](architecture/refactoring-plan.ru.md) | - | План по итогам четырёх аудитов: почему `/api/chat`, `/v1/*` и `/ollama/*` обслуживаются разными машинами, этапы с флагами, тесты-предохранители, критерии отката. || **Агентский цикл: почему симптомы повторяются** | [RU](architecture/agent-loop-diagnostics.ru.md) | - | План диагностики без правок: гипотезы (образ, шаблон, окно, слоты, VRAM, набор инструментов), чем каждая подтверждается, что прислать со стенда; `X-Request-ID`, выгрузка логов через WebUI, флаги ведения логов. |

## Эксплуатация

Эксплуатация, обновление и диагностика живого кластера.

| Документ | Русский | English | О чём |
|---|---|---|---|
| **Compose-стек** | [RU](operations/deployment-stack.ru.md) | [EN](operations/deployment-stack.md) | Один compose-файл, профили, токены, реестр, диагностика. |
| **Воркер за удалённым балансером** | [RU](operations/deployment-remote-balancer.ru.md) | [EN](operations/deployment-remote-balancer.md) | cppworker + agent на одной машине, балансер на другой. |
| **Руководство по compose-файлам** | [RU](operations/docker-compose-guide.ru.md) | [EN](operations/docker-compose-guide.md) | Какой compose-файл канонический и что делают исторические. |
| **Развёртывание агента** | [RU](operations/agent-deployment.ru.md) | [EN](operations/agent-deployment.md) | Агент метрик: режимы GPU и CPU, systemd, Windows, WSL2. |
| **Обновление донора** | [RU](operations/donor-update.ru.md) | [EN](operations/donor-update.md) | Обновление и откат образа узла-донора. |
| **VRAM на узлах раскладки** | [RU](operations/rpc-donor-vram-lifecycle.ru.md) | [EN](operations/rpc-donor-vram-lifecycle.md) | Когда освобождается VRAM на узлах раскладки и что остаётся занятым. |
| **Troubleshooting** | [RU](operations/troubleshooting.ru.md) | [EN](operations/troubleshooting.md) | Симптомы, причины и исправления из живых инцидентов. |
| **Инциденты развёртывания** | [RU](operations/deployment-troubleshooting.ru.md) | [EN](operations/deployment-troubleshooting.md) | Ловушки развёртывания и разборы инцидентов. |
| **Runbook tools/tool_calls** | [RU](operations/runbook-tools.ru.md) | [EN](operations/runbook-tools.md) | Пошаговый дебаг сбросов tool_calls (сценарии A-G). |

## API и справочники

Справочники по эндпоинтам, переменным и метрикам.

| Документ | Русский | English | О чём |
|---|---|---|---|
| **Справочник REST API** | [RU](api/api.ru.md) | [EN](api/api.md) | Все эндпоинты балансера, авторизация, WebSocket, совместимость Ollama/OpenAI. |
| **Переменные окружения** | [RU](api/env-vars.ru.md) | [EN](api/env-vars.md) | Единый справочник переменных: значение по умолчанию и эффект. |
| **Справочник метрик** | [RU](api/metrics.ru.md) | [EN](api/metrics.md) | Метрики GPU, системы, Ollama и прокси: единицы и смысл. |
| **Сбор метрик воркера** | [RU](api/cppworker-metrics-collection.ru.md) | [EN](api/cppworker-metrics-collection.md) | Встроенный агент против внешнего поллера: кто что собирает. |

## Возможности

Что кластер даёт клиентам.

| Документ | Русский | English | О чём |
|---|---|---|---|
| **Генерация изображений** | [RU](features/image-generation.ru.md) | [EN](features/image-generation.md) | Image-бэкенд sd.cpp, модели, клиенты, профили VRAM. |
| **Параметры моделей** | [RU](features/cppworker-model-params.ru.md) | [EN](features/cppworker-model-params.md) | n_ctx resolver, профили моделей, RAM fallback, поверхности прокси. |
| **Замер RPC (27B)** | [RU](features/rpc-benchmark-qwen38.ru.md) | [EN](features/rpc-benchmark-qwen38.md) | Измеренный выигрыш раскладки на qwen3.8 27B: методика и результаты. |
| **Вход и роли WebUI** | [RU](features/webui-auth.ru.md) | [EN](features/webui-auth.md) | Сессии, роли, CSRF, аварийный токен, nginx-прокси. |
| **Демо-режим WebUI** | [RU](features/webui-demo.ru.md) | [EN](features/webui-demo.md) | Гостевая витрина сценария, включается только WEBUI_DEMO_MODE. |
| **Агентский цикл Cline** | [RU](features/cline-agent-loop.ru.md) | [EN](features/cline-agent-loop.md) | Контракт tool_calls, ожидание готовности, трассировка, проверка. |

## Релизы

Что вошло в релизы и как проверялось.

| Документ | Русский | English | О чём |
|---|---|---|---|
| **v0.7.241: WebUI — диагностика, пользователи, логи** | [RU](releases/v0.7.241-release-notes.ru.md) | - | R481: доступ к «Диагностике» и архиву логов, создание пользователей WebUI (путь users.json на записываемом томе), переработка страницы логов: поля события отдельно, поиск/фильтр, раскрытие ошибок, выгрузка CSV/JSON, локализация. |
| **v0.7.240: окно сессии по варианту C** | [RU](releases/v0.7.240-release-notes.ru.md) | - | R480: `need = max(потребность запроса, окно активной сессии в пределах TTL)`, TTL метрики окна, гейт R459 на обходных путях и в очереди, `window_too_small` в `why_queued` (P1-4…P1-8). |
| **v0.7.239: стадия очереди и чистка** | [RU](releases/v0.7.239-release-notes.ru.md) | - | R480: единая очередь ставит `admission`, generic-путь — `upstream`/`stream`/`client_write`; удалён мёртвый код (P1-9, P2-14). |
| **v0.7.238: паритет типов** | [RU](releases/v0.7.238-release-notes.ru.md) | - | R479: `/v1/*` доходит до Ollama (нативный passthrough), алиасы `/ollama` и `/openai` снимаются до диспетчеризации, `/v1/models` показывает оба пула, лимит тела и стадии доставки. || **v0.7.237: стадия сбоя и OpenWebUI** | [RU](releases/v0.7.237-release-notes.ru.md) | - | R477/R478: гейт R470 только на tools-пути (ответ OpenWebUI снова идёт), стадии в `request trace`, кольцо сбоев в сводке/архиве и блок «Последние сбои» в WebUI, отказы R459 на INFO, 502 вместо пустого 200. || **v0.7.236: диагностика стенда архивом** | [RU](releases/v0.7.236-release-notes.ru.md) | [EN](releases/v0.7.236-release-notes.md) | R476: `request trace` с `request_id`, `/api/v1/diag/summary`, архив `/api/v1/diag/bundle` (логи балансера + воркеров), страница «Диагностика» с копированием и скачиванием; флаги `LB_REQUEST_TRACE`, `CPPWORKER_LOG_BUFFER_ENTRIES`, `CPPWORKER_LOG_LEVEL`. || **v0.7.235: окно контекста — не догадка** | [RU](releases/v0.7.235-release-notes.ru.md) | [EN](releases/v0.7.235-release-notes.md) | R448–R473: точный подсчёт токенов вместо эвристики, отказ 413 только для окна клиента, рост окна без ложных потолков, честные причины отказов. |
| **v1.0: заметки о релизе** | [RU](releases/v1.0-release-notes.ru.md) | [EN](releases/v1.0-release-notes.md) | Что входит в v1.0: работает / частично / заглушка. |
| **v1.0-rc2: заметки о релизе** | [RU](releases/v1.0-rc2-release-notes.ru.md) | [EN](releases/v1.0-rc2-release-notes.md) | Изменения с rc1 и как они проверялись. |
| **v0.7.217: заметки о релизе** | [RU](releases/v0.7.217-release-notes.ru.md) | [EN](releases/v0.7.217-release-notes.md) | Гигиена репозитория, починка тестов и последние правки переводов; новых возможностей нет. |
| **v0.7.216: заметки о релизе** | [RU](releases/v0.7.216-release-notes.ru.md) | [EN](releases/v0.7.216-release-notes.md) | v0.7.216: demo-режим, рефакторинг и локализация документации, язык логов, ворота в CI. |
| **v1.0: чек-лист smoke** | [RU](releases/v1.0-smoke-checklist.ru.md) | [EN](releases/v1.0-smoke-checklist.md) | Чек-лист ручного smoke на A10 24 GB перед финальным тегом. |

## Разработка

Соглашения и CI для участников.

| Документ | Русский | English | О чём |
|---|---|---|---|
| **Стиль и синхронизация документации** | [RU](development/style-compliance.ru.md) | [EN](development/style-compliance.md) | Линт em-dash, паритет i18n, правила синхронизации документации. |
| **Язык логов и ошибок (R234)** | [RU](development/i18n-logs.ru.md) | [EN](development/i18n-logs.md) | Как выбирается язык логов и ошибок, как переключается в WebUI и синхронизируется с воркерами. |
| **Self-hosted CI runner** | [RU](development/ci-self-hosted-runner.ru.md) | [EN](development/ci-self-hosted-runner.md) | Настройка runner-а, лейблы, эксплуатация и диагностика. |

## Roadmap и отчёты сессий

Планы, ADR и рабочие отчёты сессий (русский, исторические).

- [Roadmap и индекс планов](plans/README.md)
- [2026-08-04-settings-busy-and-streaming-cutoff](plans/2026-08-04-settings-busy-and-streaming-cutoff.md)
- [2026-09-23-admission-queue-and-sessions](plans/2026-09-23-admission-queue-and-sessions.md)
- [2026-09-23-multi-backend-placement-policy](plans/2026-09-23-multi-backend-placement-policy.md)
- [2026-09-25-handoff-r83-next-session](plans/2026-09-25-handoff-r83-next-session.md)
- [2026-09-25-memory-fit-subsystem](plans/2026-09-25-memory-fit-subsystem.md)
- [2026-09-25-placement-p4-load-kpi](plans/2026-09-25-placement-p4-load-kpi.md)
- [2026-09-25-qwen38-load-context-dup-and-infra](plans/2026-09-25-qwen38-load-context-dup-and-infra.md)
- [2026-09-26-estimator-unification-protocol](plans/2026-09-26-estimator-unification-protocol.md)
- [2026-09-26-step1b-fit-refusal-proposal](plans/2026-09-26-step1b-fit-refusal-proposal.md)
- [2026-09-27-image-generation-backend-plan](plans/2026-09-27-image-generation-backend-plan.md)
- [2026-10-03-image-ui-gguf-style-unification](plans/2026-10-03-image-ui-gguf-style-unification.md)
- [2026-10-06-image-tool-catalog-autoload](plans/2026-10-06-image-tool-catalog-autoload.md)
- [2026-10-07-model-distribution-multi-gpu-next-session](plans/2026-10-07-model-distribution-multi-gpu-next-session.md)
- [2026-10-13-rpc-full-mode](plans/2026-10-13-rpc-full-mode.md)
- [2026-10-14-donor-update-and-layout](plans/2026-10-14-donor-update-and-layout.md)
- [2026-10-14-handoff-r115-r116](plans/2026-10-14-handoff-r115-r116.md)
- [2026-10-14-handoff-r117](plans/2026-10-14-handoff-r117.md)
- [2026-10-14-handoff-r118](plans/2026-10-14-handoff-r118.md)
- [2026-10-14-model-settings-refactor-r116](plans/2026-10-14-model-settings-refactor-r116.md)
- [2026-10-16-session-report](plans/2026-10-16-session-report.md)
- [2026-10-17-qwen38-flash-next-a10](plans/2026-10-17-qwen38-flash-next-a10.md)
- [2026-10-18-inworker-node-r172](plans/2026-10-18-inworker-node-r172.md)
- [2026-10-18-layout-on-reload-r174-check](plans/2026-10-18-layout-on-reload-r174-check.md)
- [2026-10-21-webui-auth](plans/2026-10-21-webui-auth.md)
- [2026-10-22-r220-r226-summary](plans/2026-10-22-r220-r226-summary.md)
- [2026-q3-production-ready-plan](plans/2026-q3-production-ready-plan.md)
- [2026-q3-roadmap](plans/2026-q3-roadmap.md)
- [2026-q3-session-f-quick-wins](plans/2026-q3-session-f-quick-wins.md)
- [b8-tensor-parallelism-plan](plans/b8-tensor-parallelism-plan.md)
- [bug-2026-07-31-reasoning-not-routed](plans/bug-2026-07-31-reasoning-not-routed.md)
- [pre-existing-test-failures](plans/pre-existing-test-failures.md)
- [round-17-multi-gpu-test-plan](plans/round-17-multi-gpu-test-plan.md)
- [round-18-balancer-architecture-audit](plans/round-18-balancer-architecture-audit.md)
- [round-18-p0.3-user-parallel](plans/round-18-p0.3-user-parallel.md)
- [round-20-ollama-api-coverage-audit](plans/round-20-ollama-api-coverage-audit.md)
- [round-22-balancer-general-audit](plans/round-22-balancer-general-audit.md)
- [round-35-cppworker-bundled-r35](plans/round-35-cppworker-bundled-r35.md)
- [round-35c-env-tunable-polling](plans/round-35c-env-tunable-polling.md)
- [rpc-model-distribution-plan](plans/rpc-model-distribution-plan.md)

## Архив

Исторические материалы на языке оригинала: аудиты, research-заметки, отчёты раундов, разборы инцидентов, ранние планы. В индекс актуальной документации не входят.

Полный перечень: [archive/README.ru.md](archive/README.ru.md)

---

Русский README проекта: [../README.ru.md](../README.ru.md). Английский: [../README.md](../README.md).
