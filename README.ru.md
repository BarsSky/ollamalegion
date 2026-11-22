# OllamaLegion — балансировщик для llama.cpp / Ollama-стека

[English version](README.md)

OllamaLegion собирает одну или несколько машин с потребительскими GPU в единый пул вывода
за одним адресом. Балансировщик распределяет запросы между бэкендами `llama.cpp` (текст) и
`stable-diffusion.cpp` (картинки), сам выбирает узел по свободным ресурсам, поднимает модель
там, где её нет в памяти, и отдаёт WebUI для управления кластером. Клиенты работают через
привычные API: **Ollama**, **OpenAI** и A1111 (`/sdapi/v1/*`).

Что берёт на себя балансировщик:

- **Маршрутизация** — выбор бэкенда по GPU/VRAM/CPU/RAM, привязка модели и сессии клиента.
- **Очередь допуска** — вместо мгновенного 503 ждём слот (`LB_ADMISSION_WAIT_SEC`) или бэкенд (`LB_BACKEND_WAIT_SEC`).
- **Окно контекста на слот** — при нескольких слотах суммарное окно делится между ними.
- **Холодная загрузка** — модель, которой нет в памяти, поднимается на свободном бэкенде.
- **WebUI** — узлы и раскладка, очередь, сессии, модели, логи, диагностика, роли `admin`/`viewer`.

Архитектура коротко: клиент -> балансер (`:18080` Ollama, `:18079` OpenAI) -> выбор бэкенда,
preflight окна и очередь -> `cppworker` (llama.cpp, `:18092`) или `imageworker` (sd.cpp,
`:18093`); часть слоёв может уйти на донорный узел по llama.cpp RPC. Подробнее —
[жизненный цикл запроса](docs/architecture/request-lifecycle.ru.md) и
[балансировка и сессии](docs/architecture/balancing-guide.ru.md).

## Быстрый старт

### Требования

- Docker 24+ с Compose v2; для GPU-воркера — драйвер NVIDIA и `nvidia-container-toolkit`.
- Git с сабмодулями: `c/llama.cpp` — фиксированный сабмодуль, без него `cppworker` не собрать.
- Минимальный готовый пресет — 8 ГБ VRAM (`rtx30-8gb`); балансеру хватает ~512 МБ ОЗУ.
- Команды даны для Windows/PowerShell 7 (`pwsh`), bash-эквиваленты — в комментариях.

### Окружение (один раз)

```powershell
git clone --recurse-submodules https://github.com/BarsSky/ollamalegion.git
cd ollamalegion
Copy-Item deployments/.env.example deployments/.env              # bash: cp deployments/.env.example deployments/.env
Copy-Item deployments/.env.bundled-with-agent.example deployments/.env.bundled-with-agent
```

Обязательное в `deployments/.env`:

| Переменная | Зачем |
|---|---|
| `CPPWORKER_API_TOKEN` | единый токен стека (балансер, воркеры, WebUI); публичный дефолт `changeme-bundled-with-agent-token` — смените |
| `LB_ADMIN_USER`, `LB_ADMIN_PASSWORD` | первый администратор WebUI (логин живёт в балансере, не в WebUI) |
| `BALANCER_TAG`, `WEBUI_TAG`, `CPPWORKER_GPU_TAG`, `IMAGE_WORKER_TAG`, `AGENT_TAG` | теги образов; скрипт выпуска пишет их сам |

`CUDA_ARCH` (в примере `75;86;120`) задаётся там же и определяет архитектуры сборки `cppworker`.

### Запуск

```powershell
pwsh -File scripts/release-all.ps1 -Tag <тег>   # сборка с идентичностью, теги в .env, раскатка
```

Когда образы нужных тегов уже есть, достаточно compose:

```bash
docker compose -f deployments/docker-compose.stack.yml --profile full up -d
```

**С нуля (образов ещё нет):** вместо прямого `docker compose up -d` — `pwsh -File scripts/up-stack.ps1`
(или `./scripts/up-stack.sh`). Скрипт вычисляет идентичность сборки из git (её требует стража R486 в
Dockerfile'ах), собирает только недостающие образы и поднимает стек; подробнее —
[up-from-scratch.ru.md](docs/operations/up-from-scratch.ru.md).

Прямой `up -d` на машине без образов соберёт их сам, но сборке нужны build-arg'и идентичности
(`GIT_COMMIT` и др.) из окружения процесса — без них стража в Dockerfile останавливает сборку.

### Адреса после старта

| Адрес | Что это |
|---|---|
| `http://localhost:18080` | Ollama-native: `/api/chat`, `/api/generate`, `/api/tags`, `/api/ps` |
| `http://localhost:18079` | OpenAI: `/v1/chat/completions`, `/v1/models`, `/v1/images/*`, `/sdapi/v1/*` |
| `http://localhost:18081` | admin API: бэкенды, очередь, метрики, WebSocket; наружу не публикуйте |
| `http://localhost:18083` | WebUI |
| `:18092`, `:18093` | `cppworker` и `imageworker` — служебные, для внутренней сети |

Внешние порты — `LB_PORT`, `LB_OPENAI_PORT`, `LB_ADMIN_PORT`, `WEBUI_PORT`, `CPPWORKER_PORT_HOST`,
`SDWORKER_PORT`.

### Проверка и первая модель

```bash
TOKEN=$(grep '^CPPWORKER_API_TOKEN=' deployments/.env | cut -d= -f2)
# PowerShell: $TOKEN = ((Get-Content deployments/.env | ? { $_ -match '^CPPWORKER_API_TOKEN=' }) -split '=',2)[1]
curl -s http://localhost:18081/api/v1/ping                                # 200 — балансер жив
curl -s http://localhost:18081/api/v1/health                              # здоровье без токена
curl -s -H "X-API-Token: $TOKEN" http://localhost:18081/api/v1/backends   # бэкенды и статусы
curl -s http://localhost:18080/api/tags | jq -r '.models[].name'          # модели на диске
```

WebUI — `http://localhost:18083`, вход парой `LB_ADMIN_USER`/`LB_ADMIN_PASSWORD`. Первый запрос
к незагруженной модели обслуживается столько, сколько длится загрузка (27B Q4 на A10 — минуты):
балансер ждёт до `LB_AUTO_LOAD_WAIT_SEC` (в этом compose 900 с), затем отдаёт `503` с `Retry-After`.

```bash
curl -s -H "X-API-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d '{"model":"<имя>","messages":[{"role":"user","content":"hi"}],"stream":false}' \
  http://localhost:18080/api/chat
```

### Остановка и обновление

```bash
docker compose -f deployments/docker-compose.stack.yml --profile full down     # тома сохраняются
docker compose -f deployments/docker-compose.stack.yml --profile full down -v  # вместе с состоянием
```

Обновление — новый выпуск через `scripts/release-all.ps1 -Tag <новый-тег>`; только перепин тегов
без сборки и раскатки — тот же скрипт с `-SkipBuild -NoDeploy`.

## Документация

Оглавление всей документации — [docs/README.ru.md](docs/README.ru.md).

| Документ | О чём |
|---|---|
| [installation.ru.md](docs/getting-started/installation.ru.md) | требования к железу, сборка, stub-режим |
| [deployment.ru.md](docs/getting-started/deployment.ru.md) | развёртывание: профили, окружения, порты |
| [configuration.ru.md](docs/getting-started/configuration.ru.md) | `config.json` балансера и env-переопределения |
| [hardware-presets.ru.md](docs/getting-started/hardware-presets.ru.md) | пресеты под RTX 30xx/40xx/50xx и A10 |
| [deployment-stack.ru.md](docs/operations/deployment-stack.ru.md) | профили, порты, удалённые воркеры, проверки |
| [docker-compose-guide.ru.md](docs/operations/docker-compose-guide.ru.md) | какой compose-файл за что отвечает |
| [deployment-troubleshooting.ru.md](docs/operations/deployment-troubleshooting.ru.md) | живые аварии и ловушки окружения |
| [troubleshooting.ru.md](docs/operations/troubleshooting.ru.md) | общий разбор отказов по симптомам |
| [deployments/README.md](deployments/README.md) | что лежит в `deployments/`, какой файл канонический |
| [request-lifecycle.ru.md](docs/architecture/request-lifecycle.ru.md) | жизненный цикл запроса и его стадии |
| [balancing-guide.ru.md](docs/architecture/balancing-guide.ru.md) | выбор бэкенда и привязка сессий |
| [context-numbers.ru.md](docs/architecture/context-numbers.ru.md) | окно контекста: откуда берутся числа |
| [availability-first-r493.ru.md](docs/architecture/availability-first-r493.ru.md) | почему запрос ждёт при свободных узлах |
| [window-align-r494.ru.md](docs/architecture/window-align-r494.ru.md) | выравнивание окон копий одной модели |
| [feature-switches.ru.md](docs/architecture/feature-switches.ru.md) | флаги: что и чем включается |
| [env-vars.ru.md](docs/api/env-vars.ru.md) | все переменные окружения с дефолтами |
| [api.ru.md](docs/api/api.ru.md) | REST API управления |
| [rpc-benchmark-qwen38.ru.md](docs/features/rpc-benchmark-qwen38.ru.md) | замер раскладки слоёв по RPC на 27B |
| [webui-auth.ru.md](docs/features/webui-auth.ru.md) | вход, роли, CSRF, сессии |
| [webui-demo.ru.md](docs/features/webui-demo.ru.md) | демо-режим: WebUI без кластера |
| [image-generation.ru.md](docs/features/image-generation.ru.md) | генерация изображений на том же балансере |
| [debts.ru.md](docs/architecture/debts.ru.md) | реестр осознанно не сделанного |

История выпусков и тегов образов — [CHANGELOG.md](CHANGELOG.md).

## Управление и эксплуатация

Ключевые группы переменных (полный список — [env-vars.ru.md](docs/api/env-vars.ru.md)):

| Группа | Переменные |
|---|---|
| Маршрутизация и очередь | `LB_MODEL_AFFINITY`, `LB_SESSION_STICKINESS`, `LB_ADMISSION_WAIT_SEC`, `LB_BACKEND_WAIT_SEC`, `LB_UNIFIED_ADMISSION` |
| Окно контекста | `LB_NCTX_RELOAD_ENABLED`, `LB_NCTX_RELOAD_MAX_N_CTX`, `LB_NCTX_PREFLIGHT_ENABLED`, `LB_PREFLIGHT_EXACT_TOKENS`, `LB_R459_NEED_RESERVE`, `LB_WINDOW_ALIGN_MAX_N_CTX` |
| Доступ | `CPPWORKER_API_TOKEN`, `LB_ADMIN_USER`, `LB_ADMIN_PASSWORD`, `LB_ADMIN_ENV_SYNC`, `LB_SESSIONS_FILE` |
| Воркер | `CPPWORKER_CTX_SIZE`, `CPPWORKER_GPU_LAYERS`, `CPPWORKER_N_PARALLEL`, `CPPWORKER_VRAM_OVERHEAD_MB` |
| Теги образов | `BALANCER_TAG`, `WEBUI_TAG`, `CPPWORKER_GPU_TAG`, `IMAGE_WORKER_TAG`, `AGENT_TAG`, `CUDA_ARCH` |

Логи и диагностика (в WebUI — страницы «Бэкенды», «Очередь», «Логи», «Диагностика»):

```bash
docker compose -f deployments/docker-compose.stack.yml logs -f loadbalancer cppworker-gpu
curl -s -H "X-API-Token: $TOKEN" http://localhost:18081/api/v1/queue/stats
curl -s -H "X-API-Token: $TOKEN" -o diag.zip http://localhost:18081/api/v1/diag/bundle
```

Выпуск и проверки:

| Скрипт | Что делает |
|---|---|
| `scripts/release-all.ps1` | собрать, протегировать, раскатать; пишет `deployments/.env` и манифест |
| `scripts/check-image-tags.ps1` | согласованность тегов: compose, `.env`, манифест (docker не нужен) |
| `scripts/check_webui_assets.py`, `scripts/check_webui_i18n_keys.py`, `scripts/check_webui_refs.js` | статика WebUI: ассеты без CDN, i18n-ключи, вызовы функций |
| `scripts/check_docs_claims.py` | документация не ссылается на несуществующие файлы и переменные |

## Ограничения и известные особенности

- **Рост окна — только под заявку клиента.** Промпт не влез, а `options.num_ctx` не прислан
  (Cline не присылает) — модель не перезагружается, клиент получает 413
  `context_window_exhausted_no_growth_request` с числами `prompt_tokens`,
  `loaded_window_tokens`, `required_window_tokens`, `requested_n_ctx` и потолками; заявка окна
  выше загруженного растит окно в фоне ([context-numbers.ru.md](docs/architecture/context-numbers.ru.md)).
- **Не-стриминговый preflight — прежний контракт:** заявленное окно выше загруженного —
  синхронная перезагрузка (или `413`, если авто-reload выключен), фонового роста там нет.
- **Слоты делят окно:** `CPPWORKER_N_PARALLEL=1` по умолчанию, при `parallel=2` сессия получает
  половину суммарного окна.
- **Вход принадлежит балансеру:** пользователи и сессии — файлы на томе `balancer_data`
  (`LB_USERS_FILE`, `LB_SESSIONS_FILE`); `LB_ADMIN_*` перезаписывают пароль при
  `LB_ADMIN_ENV_SYNC=1`, удаление тома означает потерю входа.
- **Таймауты по умолчанию выключены:** инференс не ограничен (`LB_REQUEST_TIMEOUT_SEC=0`),
  зависший поток ловит `LB_STREAM_STALL_ABORT_SEC`, первый байт — `LB_FIRST_BYTE_TIMEOUT_DEFAULT_SEC`.

## Статус и лицензия

- Последняя запись [CHANGELOG.md](CHANGELOG.md) — 0.7.477 (R679, 2026-11-21).
- Лицензия MIT — [LICENSE.md](LICENSE.md); документация ведётся на русском, часть файлов — только на нём.
