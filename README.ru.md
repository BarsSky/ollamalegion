# OllamaLegion - адаптивный кластер вывода LLM

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE.md)
[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8.svg)](https://go.dev/)
[![Docker](https://img.shields.io/badge/Docker-Ready-2496ED.svg)](https://www.docker.com/)
[![CI test](https://github.com/BarsSky/ollamalegion/actions/workflows/test.yml/badge.svg?branch=centurion)](https://github.com/BarsSky/ollamalegion/actions/workflows/test.yml)

> [English](README.md) | **Русский** (текущий)

OllamaLegion собирает одну или несколько машин с потребительскими GPU в единый
самонастраивающийся кластер вывода за одним адресом. Балансировщик распределяет запросы
между бэкендами `llama.cpp` (текст) и `stable-diffusion.cpp` (картинки), сам подбирает
безопасные параметры модели (контекст, тип KV-кэша, число слоёв на GPU), раскладывает
часть слоёв на донорный узел по `llama.cpp` RPC и отдаёт WebUI с дашбордами, мониторингом,
ролями и демо-режимом.

Клиенты продолжают работать через привычные API: **Ollama**, **OpenAI** и A1111
(`/sdapi/v1/*`).

## Содержание

- [Зачем нужен OllamaLegion](#зачем-нужен-ollamalegion)
- [Быстрый старт](#быстрый-старт)
- [Архитектура](#архитектура)
- [Порты](#порты)
- [Возможности](#возможности)
- [Производительность](#производительность)
- [Сборка](#сборка)
- [Сценарии развёртывания](#сценарии-развёртывания)
- [Конфигурация](#конфигурация)
- [Структура проекта](#структура-проекта)
- [Документация](#документация)
- [Безопасность](#безопасность)
- [Коммиты и авторство](#коммиты-и-авторство)
- [Лицензия](#лицензия)
- [Последние изменения](#последние-изменения)

## Зачем нужен OllamaLegion

- **Один адрес вместо N бэкендов.** Клиенты (Cline, Open WebUI, curl, SDK) видят один хост,
  а балансер выбирает бэкенд по ресурсам, а не по кругу. Привязка сессий и моделей уже
  внутри.
- **Ручная настройка параметров не нужна.** AutoTune читает фактическую VRAM/RAM и
  архитектуру модели (`/api/gguf`, `/api/show`), выставляет `n_ctx`, тип KV-кэша и
  `num_gpu_layers` по тому, что реально влезает, и сверяет применённый план с фактом.
- **Больше модели на том же железе.** Раскладка слоёв отправляет часть из них на донорный
  узел по `llama.cpp` RPC: замерено **20/66 -> 48/66 слоёв на GPU** и **~2.9x скорости
  генерации** на 27B Q4 с двумя картами по 8 ГБ
  ([замер](docs/rpc-benchmark-qwen38.md)). Пропал донор - гард возвращает локальную загрузку
  вместо отказа.
- **Текст и картинки за одним балансером.** Воркер `image_cpp` (`sd-server`) регистрируется
  рядом с текстовыми; маршрутизация идёт по типу бэкенда и по явным признакам запроса
  (`/v1/images/*`, `/sdapi/v1/*`, префикс модели `sd:`).
- **Деградирует, а не падает.** RAM-fallback, лестница KV-кэша (`f16 -> q8_0 -> q4_0`),
  частичный offload, запасной путь reload->load, кламп `n_ctx` и watchdog, который
  объясняет, почему загрузка была снята.
- **Всё видно и есть что показать.** WebUI: дашборды GPU/CPU/RAM, очередь, сессии, операции
  с моделями, история AutoTune, вход с ролями (администратор / гость) и CSRF, плюс
  **демо-режим** - синтетический сценарий работы кластера для гостя без доступа к стенду.
- **Без облака, аккаунта и вендора.** Всё работает на ваших машинах, лицензия MIT. Режим
  CPU/stub (`-tags llama_stub`) позволяет собирать, тестировать и показывать без GPU.

## Быстрый старт

Требуется: Docker 24+ с Compose v2, Git, NVIDIA драйвер + NVIDIA Container Toolkit (для
GPU-режима). Сборка без GPU возможна, см. [Сборка](#сборка).

```bash
# 1. Клонировать. --recurse-submodules ОБЯЗАТЕЛЕН: c/llama.cpp - закреплённый сабмодуль.
git clone --recurse-submodules https://github.com/BarsSky/ollamalegion.git
cd ollamalegion
# если уже клонировали без флага: git submodule update --init --recursive

# 2. Подготовить env-файлы (один раз)
cp deployments/.env.example deployments/.env
cp deployments/.env.bundled-with-agent.example deployments/.env.bundled-with-agent
```

```bash
# 3. Задать API-токен - ровно в одном месте:
#    deployments/.env -> CPPWORKER_API_TOKEN
#    compose выводит из него LB_API_TOKEN / API_TOKEN / BALANCER_TOKEN.

# 4. Собрать и поднять всё на одной машине
export DOCKER_BUILDKIT=1
export CUDA_ARCH=86          # 61 = GTX 10xx, 75 = RTX 20xx, 86 = RTX 30xx, 89 = RTX 40xx, 90 = RTX 50xx
docker compose -f deployments/docker-compose.stack.yml --profile full up -d
```

В PowerShell: `$env:DOCKER_BUILDKIT=1`, `$env:CUDA_ARCH=86`, затем та же команда
`docker compose ... up -d`. Флаг `--env-file` **не** нужен: compose сам читает
`deployments/.env`, а флаг подменяет его целиком (теряются `CUDA_ARCH` и токен).

После запуска:

| Адрес | Что это |
|---|---|
| http://localhost:18080 | Ollama-совместимый API (сюда смотрят клиенты) |
| http://localhost:18079 | OpenAI-совместимый API + image API |
| http://localhost:18081 | Management API (только админ, наружу не выставлять) |
| http://localhost:18083 | WebUI |

Проверка:

```bash
curl -s localhost:18081/api/v1/backends | jq '.backends[] | {id, host, type, status}'
```

Новое железо или миграция? Начните с пресета под GPU (RTX 30xx/40xx/50xx, A10):
`python scripts/apply-hardware-preset.py <name>` - [RU](docs/ru/hardware-presets.md) /
[EN](docs/en/hardware-presets.md).

## Архитектура

```
Клиент (Cline / Open WebUI / SDK / curl)
  -> Балансер :18080 (Ollama) / :18079 (OpenAI, картинки) / :18081 (управление)
       |-- маршрутизация: resource-aware scoring, привязка сессий и моделей
       |-- preflight n_ctx: 4 фазы, auto-reload с запасом по VRAM
       |-- раскладка слоёв: RPC-координатор -> донорный узел (llama.cpp RPC :50052)
       |-- CppWorker :18092  вывод llama.cpp (адаптивный загрузчик, KV-fallback, reload/load)
       |     + встроенный агент метрик :18034
       |-- ImageWorker :18093  sd.cpp (image_cpp) + встроенный агент метрик :18033
       +-- WebSocket :18081/ws  живые события (очередь, бэкенды, autotune)
  -> WebUI :18083 дашборды, мониторинг, очередь, сессии, настройки, демо-режим
```

Метрики для чистых **Ollama**-бэкендов собирает отдельный контейнер на `:18032`
(профиль `legacy-agent`) - см. [Порты](#порты).

## Порты

| Порт | Компонент | Назначение |
|---|---|---|
| 18080 | Балансер | Прокси Ollama API (+ поверхность `/v1/*`) |
| 18079 | Балансер | OpenAI-поверхность (`/v1/chat/completions`, `/v1/images/generations`, `/sdapi/v1/*`) |
| 18081 | Балансер | Management API + WebSocket (наружу не выставлять) |
| 18092 | CppWorker | Вывод llama.cpp |
| 18093 | ImageWorker | API sdworker (`/api/image/*`, `/api/hf/*`) |
| 18034 | CppWorker | Метрики **встроенного** агента (`CPPWORKER_AGENT_EMBEDDED=on`) |
| 18033 | ImageWorker | Метрики **встроенного** агента (`IMAGE_WORKER_AGENT_EMBEDDED=on`) |
| 18032 | Agent (внешний) | Метрики только для **Ollama**-бэкендов |
| 18083 | WebUI | Дашборд |

Порты встроенных агентов разнесены специально: два процесса не должны слушать один порт, а
балансер опрашивает `agentPort` конкретной записи. Внешний агент и встроенный на одном
бэкенде одновременно не запускаются - метрики «мигают», в частности `gpu.uuids`, по которому
балансер понимает, что два бэкенда делят одну физическую GPU.

## Возможности

### AutoTune - параметры по факту

Определяет sub-optimal состояние загруженных моделей и применяет лучший план через
асинхронный reload (ручное управление сохраняется per-model). Бюджет = свободная VRAM +
occupancy самой модели (перезагрузка её выгружает), единицы байты/МБ не путаются,
архитектура (`n_kv_heads`, `head_dim`) берётся из `/api/show`, тип KV выбирается по
фактическому `n_ctx`, а «success» без изменения конфигурации невозможен. Есть глобальный и
per-model тумблеры, живые обновления по WebSocket, история событий и таймлайн в WebUI.
Ручное применение:

```bash
curl -s -X POST localhost:18081/api/v1/admin/autotune/<backendID>/apply
```

Документация: [API](docs/api.md#autotune-api-r541--r552-2026-08-24), живая проверка
`pwsh -File scripts/verify-autotune-r230.ps1`.

### Раскладка слоёв по RPC (донорный узел)

Слои делятся между GPU мастера, донорным узлом и CPU. Обе стороны обязаны иметь один коммит
`llama.cpp` и одну архитектуру; у режима есть выключатель и свой учёт ёмкости. Правила,
добытые живыми падениями: [docs/rpc-layout-rules.md](docs/rpc-layout-rules.md); жизненный
цикл: [docs/rpc-donor-vram-lifecycle.md](docs/rpc-donor-vram-lifecycle.md); обновление и
откат донора: [docs/donor-update.md](docs/donor-update.md).

### Генерация изображений на том же балансере

`imageworker` (stable-diffusion.cpp / `sd-server`, сборка на Vulkan - работает и на старых
картах без CUDA) регистрируется как бэкенд `image_cpp`. Клиенты используют OpenAI images,
A1111 и sdworker API. Модели: GGUF/safetensors с Hugging Face, per-model профили.
Документация: [docs/image-generation.md](docs/image-generation.md).

### WebUI

Дашборды (GPU/CPU/RAM, очередь, сессии, операции с моделями), панель топологии и раскладки
RPC, живые логи, настройки, управление пользователями с ролями (`admin` / `viewer`) и CSRF,
плюс **демо-режим** (`WEBUI_DEMO_MODE=1`, только через env): детерминированный синтетический
сценарий - клиенты -> балансер -> очередь -> донор RPC -> AutoTune - для гостя без входа и без
единого запроса к реальному кластеру.
Документация: [вход и роли](docs/webui-auth.md), [демо-режим](docs/webui-demo.md).

### Совместимость с клиентами

Ollama, OpenAI (chat/completions, инструменты, стрим) и A1111 обслуживает один балансер.
Стрим следует одному контракту: размышления не дублируются в `content`, маркеры вида
`</think>` не доезжают до клиента, `[DONE]` приходит ровно один раз и последним событием.
Проверки: `node scripts/verify-stream-r231.js`, `node scripts/clients/contract-suite.js`,
агентский цикл: [docs/cline-agent-loop.md](docs/cline-agent-loop.md).

## Производительность

Раскладка слоёв по RPC, один запрос (58 токенов промпта, `num_predict=96`, `temperature=0`),
модель `Qwen3.8-27B-UD-Q4_K_M` (15.33 GiB), `ctx=16384`, KV `q8_0`. Мастер: 8 ГБ VRAM.
Донор: второй узел, 8 ГБ VRAM, RPC `:50052`, канал ~8.9 Гбит/с.

| | RPC выкл | RPC вкл | Дельта |
|---|---|---|---|
| слоёв на GPU | 20/66 (46 в RAM) | **26 CUDA + 22 RPC = 48/66**, CPU 18 | +28 слоёв |
| загрузка модели | 278 с | 285 с | +2.5 % |
| 96 токенов | 126-147 с | **45-49 с** | - |
| скорость | **0.65-0.77 tok/s** | **1.95-2.15 tok/s** | **~2.9x** |
| средняя задержка | 136 с | **46 с** | **-66 %** |

Методика и воспроизведение: [docs/rpc-benchmark-qwen38.md](docs/rpc-benchmark-qwen38.md),
`pwsh -File scripts/bench-rpc-qwen38.ps1 -Mode both`.

> Большие модели: загрузка 15 ГБ с медленного диска дольше дефолтного watchdog - выставьте
> `CPPWORKER_LOAD_STALL_SEC=1200` в `deployments/.env`, иначе на 300 с загрузка снимается с
> «model load cancelled».

## Сборка

Все команды выполняются из корня репозитория. `DOCKER_BUILDKIT=1` важен: Go-only изменения
собираются за ~30 с благодаря кэшу CUDA-слоёв.

```bash
export DOCKER_BUILDKIT=1
export CUDA_ARCH=86   # 61 = Pascal (GTX 10xx), 75, 86, 89, 90
docker compose -f deployments/docker-compose.stack.yml --profile full build            # всё
docker compose -f deployments/docker-compose.stack.yml --profile full build cppworker-gpu
docker compose -f deployments/docker-compose.stack.yml --profile full build imageworker
docker compose -f deployments/docker-compose.stack.yml --profile full build loadbalancer
docker compose -f deployments/docker-compose.stack.yml --profile full build webui
```

У каждого сервиса есть `build:`, поэтому `up -d` сам соберёт отсутствующий образ - локальный
реестр не обязателен. Скрипт-помощник (сам подставляет тег и префикс реестра):
`pwsh -File scripts/build-containers.ps1 -CppWorker -CudaArch 86 -Tag <тег>`;
`imageworker` собирается через compose (в скрипте для него флага нет).

Замечания:

- **CUDA 13 снял поддержку Pascal** (GTX 10xx). Для 1070/1080 оставайтесь на CUDA 12.x и
  собирайте с `CUDA_ARCH=61`; картинки идут через Vulkan и от CUDA-архитектуры не зависят.
- **Свой реестр образов** - одна переменная `IMAGE_REGISTRY` в `deployments/.env` (адрес
  **со слэшем на конце**). Скрипты сборки читают её же, поэтому собранный образ получает
  ровно то имя, которое ищет compose.
- CPU/stub-сборка без GPU: `-tags llama_stub` (используется юнит-тестами и CI).

## Сценарии развёртывания

Один compose-файл `deployments/docker-compose.stack.yml`, четыре сценария
([подробно](docs/deployment-stack.md)):

| Сценарий | Команда | Что поднимается |
|---|---|---|
| Всё на одной машине | `--profile full` | cppworker + imageworker + balancer + webui |
| Только бэкенды, балансер на другой машине | `--profile worker` | cppworker + imageworker |
| Только управление, бэкенды регистрируются сами | `--profile balancer` | balancer + webui |
| Откат: внешний сборщик метрик | `--profile worker --profile legacy-agent` | набор worker + контейнер `agent` |

Удалённым воркерам нужны две переменные в `deployments/.env.bundled-with-agent`:
`BALANCER_URL=http://<хост-балансера>:18081` (management API, не 18080) и
`BACKEND_HOST=<адрес, видимый с балансера>`.

```bash
# только текстовый воркер, на своей машине
docker compose -f deployments/docker-compose.stack.yml --profile worker up -d cppworker-gpu
# только image-воркер, к тому же балансеру
SDWORKER_BALANCER_URL=http://<хост-балансера>:18081 \
  docker compose -f deployments/docker-compose.stack.yml --profile worker up -d imageworker
```

Оба воркера - независимые сервисы: свой порт, свой каталог моделей, свой движок. Любой из них
работает без второго, и оба могут жить на разных машинах за одним балансером.

> **Имена переменных агента.** Агент читает `BACKEND_TYPE`, `CPPWORKER_URL`, `NODE_LABELS`,
> `GPU_MODE`, `COLLECT_INTERVAL`, `HEARTBEAT_INTERVAL`. Имена вида `AGENT_BACKEND_TYPE`
> **не читаются**: бэкенд зарегистрируется как Ollama. В каноническом compose имена верные.
>
> **Вместимость.** `AGENT_MAX_CONCURRENT_REQUESTS` должен совпадать с реальным
> `n_parallel` воркера (по умолчанию 1). Завышенное значение даёт ложные «свободные слоты» и
> зависания у клиента (Cline).

## Конфигурация

Дефолты - из `deployments/.env.bundled-with-agent.example` и
`config/cppworker-defaults.json` (`defaultCtxSize=32768`).

```bash
CPPWORKER_CTX_SIZE=32768            # контекст по умолчанию
CPPWORKER_GPU_LAYERS=20             # слоёв на GPU (-1 = все, -2 = авто)
CPPWORKER_AUTO_OFFLOAD=true         # авто gpuLayers
CPPWORKER_AUTO_TUNE_NCTX=true       # авто n_ctx
CPPWORKER_AUTO_KV_CACHE=true        # авто kvCacheType (q4_0/q8_0/f16)
CPPWORKER_RAM_FALLBACK_N_CTX=true   # RAM fallback при нехватке VRAM
CPPWORKER_LOAD_STALL_SEC=1200       # watchdog загрузки для 15-ГБ моделей
```

Конфиг балансера (`config/config.example.json`), авто-reload `n_ctx`:

```json
{
  "balancing": {
    "firstByteTimeout": 30,
    "nctxReload": {
      "auto_reload_n_ctx": true,
      "auto_reload_max_n_ctx": 131072,
      "auto_reload_vram_safety_factor": 0.85,
      "auto_reload_timeout_sec": 120
    }
  }
}
```

ENV-overrides для `nctxReload` важнее файла: `LB_NCTX_RELOAD_ENABLED`,
`LB_NCTX_RELOAD_MAX_N_CTX`, `LB_NCTX_RELOAD_VRAM_SAFETY_FACTOR`,
`LB_NCTX_RELOAD_TIMEOUT_SEC`.

Полный справочник переменных: [docs/ENV_VARS.md](docs/ENV_VARS.md).

## Структура проекта

```
cmd/
  balancer/      HTTP-прокси + management API (18080/18081/18079)
  cppworker/     сервер вывода llama.cpp (18092): адаптивный загрузчик, lazyload, кламп n_ctx
  agent/         метрики GPU/CPU/RAM (18032) для Ollama-бэкендов
  imageworker/   image-бэкенд sd.cpp (18093) + встроенный агент метрик
  rpcworker/     RPC-воркер для распределённого вывода
internal/
  api/           REST-хендлеры, роуты, авторизация, rate limit
  balancer/      маршрутизация, scoring, сессии, reload n_ctx, RPC-диспетчер
  cppbackend/    Go-обёртка над C-мостом (параметры, KV-кэш)
  rpccoordinator/ rpcworker/ rptensor/  распределённый вывод
  modelreplication/ virtualmodel/       группы репликации, alias-on-pool
c/               C-мост к llama.cpp; llama.cpp и ggml - сабмодули
deployments/     docker-compose.stack.yml + примеры env (профили full/worker/balancer)
config/          конфиг балансера, дефолты cppworker, примеры env
scripts/         сборка, бенчмарки, e2e, доктора, проверка демо
docs/            документация (RU + EN)
plans/           roadmap и отчёты по фазам
```

## Документация

- [docs/README.md](docs/README.md) - индекс русской документации (исходный язык)
- [docs/en/README.md](docs/en/README.md) - English documentation index
- [Установка](docs/installation.md) / [Развёртывание](docs/deployment.md) /
  [API](docs/api.md) / [Метрики](docs/metrics.md) /
  [Troubleshooting](docs/troubleshooting.md)
- [Генерация изображений](docs/image-generation.md) /
  [Пресеты под железо](docs/ru/hardware-presets.md)
- [ENV_VARS.md](docs/ENV_VARS.md) - все переменные с дефолтами и эффектом
- Глубокие разборы: [правила RPC-раскладки](docs/rpc-layout-rules.md),
  [замер RPC на 27B](docs/rpc-benchmark-qwen38.md), [демо-режим WebUI](docs/webui-demo.md),
  [вход и роли WebUI](docs/webui-auth.md), [агентский цикл Cline](docs/cline-agent-loop.md)
- [plans/README.md](plans/README.md) - roadmap

## Безопасность

- **Смените API-токен.** Значение по умолчанию (`changeme-bundled-with-agent-token`)
  известно всем: любой, кто дотянется до admin-порта (18081), получает полный контроль над
  кластером. Задайте `CPPWORKER_API_TOKEN` в `deployments/.env` и пересоздайте контейнеры.
- **18081 держите закрытым.** Клиентам нужны только 18079 (OpenAI/картинки) и 18080
  (Ollama).
- **Не публикуйте реальные адреса и токены.** В документации используются адреса из RFC 5737
  (`192.0.2.x`); свои значения держите в `deployments/.env`, файл в `.gitignore`.
- **Личные данные в git не попадают.** `.env`, `_diag/`, `debug/` (дампы сессий, трейсы)
  игнорируются; в примерах - placeholder'ы (`example.com`, `192.0.2.x`, `<user>@<host>`).
- **У WebUI настоящая авторизация.** Сессии, роли и CSRF:
  [docs/webui-auth.md](docs/webui-auth.md). Аварийный токен вводит оператор, в образ он не
  вшивается.

## Коммиты и авторство

Автор коммита берётся из git-конфига: локальный `user.name`/`user.email` в этом репозитории
важнее глобального.

```bash
git config --local user.name  "Your Name"
git config --local user.email "you@example.com"
```

GitHub считает контрибьюторов **по полю author**, поэтому каждая новая identity с отдельной
почтой становится отдельным контрибьютором. Исторические служебные идентичности сводятся к
владельцу репозитория файлом [`.mailmap`](.mailmap) без переписывания истории:
`git shortlog -sne github/centurion | head`.

Если коммитит ассистент или скрипт, задайте ему ту же или осознанно выбранную служебную
идентичность.

## Лицензия

MIT - см. [LICENSE.md](LICENSE.md).

## Последние изменения

Полная история с датами, тегами образов и дефектом за каждым исправлением:
[CHANGELOG.md](CHANGELOG.md). Три последних:

- **R232 (v0.7.211, 2026-10-24) - демо-режим WebUI.** `WEBUI_DEMO_MODE=1` (только env)
  превращает панель в гостевую read-only витрину: детерминированный сценарий на 150 с
  (клиенты -> балансер -> очередь -> донор RPC -> AutoTune) с баннером, счётчиками и живой
  лентой событий. Все `/api/`-, WebSocket- и SSE-запросы перехватываются в браузере, до
  реального балансера не доходит ничего. [Документация](docs/webui-demo.md)
- **v1.0-rc2 (v0.7.210, 2026-10-24) - релиз исходников.** README с замером RPC, документация
  приведена к фактическому состоянию (AutoTune R230, контракт стрима Cline R231, ENV,
  troubleshooting), из дерева убраны личные данные и артефакты стенда.
  [Release notes](docs/v1.0-rc2-release-notes.md)
- **R230 / R231 (v0.7.208-209) - корректный AutoTune и контракт стрима Cline.** AutoTune
  перестал видеть «свободно 0 GB» и советовать `-2/q4_0` (единицы, reload-бюджет,
  архитектура из `/api/show`, проверка применения); стрим больше не дублирует размышления в
  `content`, не пропускает маркеры `</think>` и отправляет ровно один финальный `[DONE]`.
