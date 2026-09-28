# Развёртывание: один compose, три сценария

**Дата:** 2026-09-28 · Актуально для тегов `r83-submodule-v22+`
**Балансер:** для этого документа нужен образ `ollama-legion/balancer:r83-submodule-v23+`
(исправлено применение вместимости, см. п. 7); остальные сервисы — `v22`.

> **Канонический файл — `deployments/docker-compose.stack.yml`.** В папке лежат
> ещё 18 compose-файлов (исторические и узкоспециальные: RPC, cocoindex,
> test-stub, fanout-стенд). Новым стекам нужен только этот; остальные оставлены
> для воспроизведения старых прогонов и помечены в конце документа.

---

## 1. Три сценария — выбор профиля

| Сценарий | Профиль | Что поднимается | Когда нужен |
|---|---|---|---|
| **1. Всё на одной машине** | `--profile full` | cppworker + agent + balancer + webui | одна GPU-машина, к ней подключаются клиенты |
| **2. Только бэкенд** | `--profile worker` | cppworker + agent | балансер уже работает на другой машине |
| **3. Только управление** | `--profile balancer` | balancer + webui | бэкенды регистрируются сами с других машин |

```bash
cd deployments
# первый раз:
cp .env.bundled-with-agent.example .env.bundled-with-agent
# в deployments/.env задать единый токен стека:
#   CPPWORKER_API_TOKEN=<ваш-токен>

# сценарий 1
docker compose -f docker-compose.stack.yml --profile full up -d
# сценарий 2
docker compose -f docker-compose.stack.yml --profile worker up -d
# сценарий 3
docker compose -f docker-compose.stack.yml --profile balancer up -d
```

Проверить, какие сервисы попадут в профиль, **не запуская** их:

```bash
docker compose -f docker-compose.stack.yml --profile full --services
# → agent, cppworker-gpu, loadbalancer, webui
```

---

## 2. Сценарий 1: всё на одной машине

```bash
docker compose -f docker-compose.stack.yml --profile full up -d
docker ps --format '{{.Names}}\t{{.Status}}' | grep ol-stack
```

| Адрес | Что это |
|---|---|
| `http://<хост>:18080` | клиентский API (OpenAI + Ollama) — сюда смотрит OpenWebUI/Cline |
| `http://<хост>:18081` | admin API (регистрация бэкендов, метрики, WebUI) |
| `http://<хост>:18083` | дашборд оператора |
| `http://<хост>:18092` | порт cppworker (нужен только внутри сети, но проброшен) |

**Проверка после старта:**

```bash
TOKEN=$(grep '^CPPWORKER_API_TOKEN=' .env | cut -d= -f2)
curl -s -H "X-API-Token: $TOKEN" http://127.0.0.1:18081/api/v1/backends | jq '.backends[] | {id,host,cppWorkerPort,status,maxConcurrentRequests}'
```

Ожидается **ровно одна** запись (`cppworker-gpu-bundled-agent`, `status=healthy`,
`maxConcurrentRequests` = ваш `AGENT_MAX_CONCURRENT_REQUESTS`).

Выбор модели и загрузка (так же, как это делает клиент):

```bash
curl -s http://127.0.0.1:18080/api/tags | jq -r '.models[].name'       # что доступно
curl -s -H "X-API-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d '{"model":"<имя>","messages":[{"role":"user","content":"hi"}],"stream":false}' \
  http://127.0.0.1:18080/api/chat -D - | head -20
# → 200 и заголовок X-Backend-Id
```

**Первое обращение к незагруженной модели** занимает столько, сколько длится
загрузка (27B Q4 на A10 — минуты). Балансер ждёт её `LB_AUTO_LOAD_WAIT_SEC`
(в этом compose дефолт **900 с**); если не дождался — отдаёт `503` с
`Retry-After`, и клиент повторяет запрос. Это нормальное поведение, а не сбой.

### 2.1 Первая загрузка 27B и таймауты клиента (важно для Cline)

Загрузка 27B на A10 занимает **3–8 минут** (наблюдалось 311 с для
`Qwen3.8-27B-UD-Q4_K_M`). Из этого следуют три практических правила:

1. **Клиент должен переживать долгий первый запрос.** Балансер честно ждёт до
   `LB_AUTO_LOAD_WAIT_SEC`, но если у клиента таймаут меньше (Cline по умолчанию
   рвёт соединение раньше), клиент увидит «не работает», хотя балансер и
   cppworker работают. Для первого запроса поднимайте таймаут клиента или
   загружайте модель заранее.
2. **Загружайте модель заранее** — тогда клиентский запрос обслуживается сразу:

   ```bash
   curl -s -H "X-API-Token: $TOKEN" -H 'Content-Type: application/json' \
     -d '{"name":"Qwen3.8-27B-UD-Q4_K_M","contextSize":32768}' \
     'http://127.0.0.1:18092/api/models/load-with-params?wait=false'
   # прогресс: /api/models/load/progress?model=Qwen3.8-27B-UD-Q4_K_M
   ```

   `wait=false` (или без `wait`) обязателен: синхронное ожидание с дефолтным
   `waitTimeoutMs=60000` обрывает загрузку раньше, чем она закончится.
3. **Не обрывайте загрузку на полпути.** Если HTTP-запрос загрузки отменён по
   таймауту, cppworker прерывает загрузку («load aborted via watcher»), модель
   остаётся в состоянии `loading`, а её слот занят — последующие запросы к этой
   модели будут ждать. Лечится перезагрузкой модели (п. 2) или рестартом
   `cppworker-gpu`.

---

## 3. Сценарий 2: только бэкенд, балансер на другой машине

Это самый частый случай, когда «cppworker не коннектится». Нужно **две**
переменные, и обе — в `.env.bundled-with-agent`:

```bash
# deployments/.env.bundled-with-agent
BALANCER_URL=http://<хост-балансера>:18081     # ADMIN API, НЕ 18080!
BACKEND_HOST=<адрес ЭТОЙ машины, видимый С балансера>
```

- `BALANCER_URL` читает **agent** (он регистрирует бэкенд и шлёт метрики).
  Порт **18081** — admin API; на 18080 регистрация не работает.
- `BACKEND_HOST` попадает в балансер как адрес бэкенда: он будет проксировать
  инференс на `BACKEND_HOST:18092`. Имя compose-сервиса (`cppworker-gpu`) здесь
  **не подойдёт** — оно резолвится только внутри этой сети. Варианты: IP машины
  в LAN, DNS-имя, `host.docker.internal` (если балансер на том же хосте).

```bash
docker compose -f docker-compose.stack.yml --profile worker up -d
```

Проверка, что бэкенд виден **с балансера**:

```bash
curl -H "X-API-Token: $TOKEN" http://<хост-балансера>:18081/api/v1/backends
```

И что балансер реально доедет до инференса:

```bash
curl -H "X-API-Token: $TOKEN" http://<хост-балансера>:18092/health   # с машины балансера
```

Если последняя команда не отвечает — проблема в сети/файрволе или в
`BACKEND_HOST`, а не в cppworker.

---

## 4. Сценарий 3: только управление

```bash
docker compose -f docker-compose.stack.yml --profile balancer up -d
```

Бэкенды на других машинах регистрируются сами (у них в `.env.bundled-with-agent`
должен быть `BALANCER_URL=http://<этот-хост>:18081`). Пока их нет,
`/api/v1/backends` пуст — это нормально.

---

## 5. Ловушка с именами переменных (главная причина «агент не коннектится»)

Код читает **не те** имена, которые исторически стоят в старых compose-файлах.
Если скопировать блок из старого файла, сервис запустится, но параметр не
подействует.

| Задано в старых файлах | Читает код | Последствие ошибки |
|---|---|---|
| `AGENT_BACKEND_TYPE` | **`BACKEND_TYPE`** | бэкенд регистрируется как **Ollama**: балансер идёт по Ollama-пути, llama.cpp-возможности (KV-типы, offload, n_ctx-потолки) не используются |
| `AGENT_CPPWORKER_URL` | **`CPPWORKER_URL`** | agent не знает адрес cppworker для метрик |
| `AGENT_NODE_LABELS` | **`NODE_LABELS`** | метки не доезжают (в UI — авто-метки хоста) |
| `AGENT_MODE` | **`GPU_MODE`** | режим GPU определяется автоматически |
| `AGENT_COLLECT_INTERVAL` / `AGENT_HEARTBEAT_INTERVAL` | **`COLLECT_INTERVAL`** / **`HEARTBEAT_INTERVAL`** | интервалы берутся из дефолтов агента |
| `AGENT_BACKEND_ID`, `AGENT_REGISTER_GPU_MODE` | — | не читаются вовсе |
| `CPPWORKER_ADVERTISED_PORT` | — | порт для балансера задаётся через `AGENT_CPPWORKER_PORT` |
| `BALANCER_PORT`, `BALANCER_API_PORT` | — | порты балансера берутся из `config/config.json` |

В `docker-compose.stack.yml` используются правильные имена.

---

## 5.1 Токен: одно место, откуда он расходится по всем сервисам

**Источник истины — ровно один: `deployments/.env` → `CPPWORKER_API_TOKEN`.**
Всё остальное compose выводит из него:

| Сервис | Переменная в контейнере | Откуда |
|---|---|---|
| loadbalancer | `LB_API_TOKEN` | `${CPPWORKER_API_TOKEN}` |
| cppworker | `API_TOKEN`, `CPPWORKER_API_TOKEN` | `${CPPWORKER_API_TOKEN}` |
| agent | `BALANCER_TOKEN` | `${CPPWORKER_API_TOKEN}` |
| agent | `CPPWORKER_API_TOKEN` | `${CPPWORKER_API_TOKEN}` |
| webui | `API_TOKEN` | `${CPPWORKER_API_TOKEN}` |

Правите токен — правите **только** эту строку и пересоздаёте стек:

```bash
# deployments/.env
CPPWORKER_API_TOKEN=<новый-токен>

docker compose -f docker-compose.stack.yml --profile full up -d --force-recreate
```

### Что менять НЕ надо (и почему это ловушка)

`auth.tokens` в `config/config.json` содержит плейсхолдер `bundled-default`, и
при заданном `LB_API_TOKEN` список токенов из файла **полностью заменяется**.
Правка `config.json` не даст никакого эффекта — вы будете искать причину 401
там, где её нет. Балансер теперь говорит об этом прямо в стартовой шапке и
предупреждением в логе:

```
║ Auth tokens:   LB_API_TOKEN (len=33) ЗАМЕНЯЕТ config.json    ║
...
WARN auth.tokens из config.json НЕ ДЕЙСТВУЕТ: список полностью переопределён
     переменной окружения. Меняйте токен в deployments/.env ..., правка
     config/config.json результата не даст.  winner=LB_API_TOKEN config_token_count=1
```

То же самое видно у остальных сервисов в их шапках:

```
cppworker: "apiTokenSource":"API_TOKEN(len=33)"        # или CPPWORKER_API_TOKEN
agent:     ║API token:     CPPWORKER_API_TOKEN/API_TOKEN (len=33)        ║
```

### Токен cppworker доезжает через агента

Cppworker защищает свои эндпоинты (`/api/models/reload`,
`/api/v1/cppworker/config/update`) и ждёт `Authorization: Bearer <token>` или
`X-API-Token`. Балансер проксирует эти запросы от WebUI, но **не передаёт
клиентский токен**: он подставляет серверный токен бэкенда, а при его отсутствии
удаляет заголовок целиком.

До исправления в bundled-стеке этот токен не попадал в балансер вообще:
Go-side регистрация cppworker там выключена (`CPPWORKER_REGISTER_DISABLE=true`),
бэкенд создаёт агент, а токен умела отдавать только cppworker-регистрация. Итог —
правка параметров модели в WebUI через агента падала с 401 «invalid or missing
API token», хотя токен задан во всех `.env` и одинаков. Теперь агент передаёт
`cppWorkerApiToken` в регистрации, и балансер принимает его на всех трёх путях
(создание, повторная регистрация, attach).

Проверка, что путь живой:

```bash
TOKEN=$(grep '^CPPWORKER_API_TOKEN=' .env | cut -d= -f2)
curl -s -o /dev/null -w '%{http_code}\n' -X PUT \
  -H "X-API-Token: $TOKEN" -H 'Content-Type: application/json' -d '{}' \
  "http://127.0.0.1:18080/api/v1/gguf/backends/cppworker-gpu-bundled-agent/proxy/api/v1/cppworker/config/update"
# → 200 (а не 401 «invalid or missing API token»)
```

---

## 5.2 Версия в шапке логов: проверка «а точно ли новая сборка?»

Все три сервиса печатают версию запущенного образа первой строкой шапки:

```
╔═════════════════════════════════════════════════════════════╗
║               Ollama Load Balancer - Starting               ║
╠═════════════════════════════════════════════════════════════╣
║Version:       r83-submodule-v23 (commit 5731414, 2026-09-28)║
```

Значение берётся из `APP_VERSION` (compose подставляет тег образа) плюс
`GIT_COMMIT`/`BUILD_DATE`, вшитые при сборке. Отсюда сразу видно, применилась ли
правка:

```bash
docker logs ol-stack-balancer 2>&1 | head -10 | grep Version
docker logs ol-stack-agent    2>&1 | head -12 | grep Version
docker logs ol-stack-cppworker-gpu 2>&1 | grep -m1 'Worker starting'
#   → "version":"r83-submodule-v22 (commit …, …)","apiTokenSource":"API_TOKEN(len=33)"
```

Если версия в логе не та, которую вы собирали, — в контейнере старый образ, и
искать дефект в коде бессмысленно.

---

## 6. Ловушка с порядком переменных: `environment:` сильнее `env_file`

Compose сначала подставляет `env_file`, а затем **накладывает** `environment`.
Поэтому строка `- LB_FOO=${LB_FOO:-}` в `environment` **затирает** значение из
`.env.bundled-with-agent` пустой строкой: интерполяция `${...}` читает только
`deployments/.env`, где таких переменных нет.

Правило: переключатель поведения задаётся **либо** в `.env.bundled-with-agent`
(и не упоминается в `environment`), **либо** в `environment` с дефолтом из
`.env`. В `docker-compose.stack.yml` переключатели (`LB_OPENAI_AUTO_STREAM`,
`LB_AUTO_LOAD_ASYNC`, `LB_AUTO_CONTINUE_*`, `LB_GPU_HEADROOM_PERCENT`)
намеренно не дублируются.

---

## 7. Вместимость: `AGENT_MAX_CONCURRENT_REQUESTS` = реальный n_parallel

Значение должно совпадать с параллелизмом воркера:

| n_parallel cppworker | `AGENT_MAX_CONCURRENT_REQUESTS` |
|---|---|
| 1 (по умолчанию) | 1 |
| 2 | 2 |

Завышенное значение даёт ложные «свободные слоты»: балансер считает, что можно
слать несколько запросов параллельно, хотя cppworker обслуживает один — клиент
получает отказы и зависания. Значение `<= 0` означает «не задано», и балансер
применит дефолт по типу бэкенда (`llama_cpp → 1`).

### 7.1 Куда именно попадает вместимость (и почему её бывает «не слышно»)

Агент отдаёт `maxConcurrentRequests` в теле регистрации, а балансер принимает
его **тремя разными путями** — и до `r83-submodule-v23` обновлял вместимость
только в одном из них:

| Путь регистрации | Когда срабатывает | Старое поведение |
|---|---|---|
| создание нового бэкенда | первого бэкенда на этом `host:18092` ещё нет | ✅ дефолт по типу (`llama_cpp → 1`) |
| повторная регистрация того же `agentId` | агент перезапущен | ❌ сохранял прежнее значение |
| **attach** к уже зарегистрированному cppworker | bundled-режим (агент приходит вторым) | ❌ сохранял прежнее значение |

Дополнительно значение сохраняется в `state.json` (том `ol-balancer-data`) и
восстанавливается при старте. Поэтому на стенде, где раньше работала сборка с
жёсткой константой `10`, балансер продолжал отдавать `10`, даже когда агент уже
был пересоздан с `AGENT_MAX_CONCURRENT_REQUESTS=1`: значение бралось из
`state.json`, heartbeat отдавал его агенту, агент «применял» его у себя и
возвращал в следующей регистрации. Самоподдерживающаяся петля.

Что делать, если в `/api/v1/backends` видите `maxConcurrentRequests` больше
вашего `AGENT_MAX_CONCURRENT_REQUESTS`:

```bash
# 1. Убедиться, что балансер именно v23+ (в нём вместимость применяется на всех путях)
docker inspect ol-stack-balancer --format '{{.Config.Image}}'

# 2. Посмотреть, что реально лежит в state.json
docker run --rm -v ol-balancer-data:/d alpine cat /d/state.json | grep -A2 maxConcurrent

# 3. Убрать унаследованное значение и перезапустить связку
docker exec -u root ol-stack-balancer rm -f /app/data/state.json
docker restart ol-stack-balancer && docker restart ol-stack-agent

# 4. Проверить: ожидается maxConcurrentRequests = 1
curl -s -H "X-API-Token: $TOKEN" http://127.0.0.1:18081/api/v1/backends \
  | jq '.backends[] | {id, maxConcurrentRequests}'
```

В логе балансера исправление видно явно:

```
"msg":"AdoptCapacityFromNode: вместимость бэкенда приведена к n_parallel узла",
"oldMaxConcurrentReqs":10,"newMaxConcurrentReqs":1
```

На чистой установке (`ol-balancer-data` пуст) шаги 2–3 не нужны: бэкенд
создаётся уже с правильным значением.

---

## 8. Диагностика: что смотреть при проблемах

```bash
# 1. Все ли контейнеры поднялись
docker ps --format '{{.Names}}\t{{.Status}}' | grep ol-stack

# 2. Зарегистрировался ли агент (главная проверка связки)
docker logs ol-stack-agent 2>&1 | grep -E 'Register|regist|error|refus'
#   ждём: "Registering llama_cpp backend at <host>:18092" + "... registered successfully"

# 3. Видит ли балансер бэкенд и правильную вместимость
TOKEN=$(grep '^CPPWORKER_API_TOKEN=' .env | cut -d= -f2)
curl -s -H "X-API-Token: $TOKEN" http://127.0.0.1:18081/api/v1/backends | jq '.backends[] | {id,status,maxConcurrentRequests}'

# 4. Доступен ли cppworker напрямую
curl -s http://127.0.0.1:18092/health
curl -s http://127.0.0.1:18092/api/models | jq '{count, models: [.models[].name]}'

# 5. Почему запрос не прошёл (логи балансера)
docker logs --since 5m ol-stack-balancer 2>&1 | grep -E 'preflight|auto-load|error|503|reject'

# 6. Какой бэкенд обслужил запрос
curl -s -D - -o /dev/null ... http://127.0.0.1:18080/api/chat | grep -i x-backend-id
```

| Симптом | Причина | Что делать |
|---|---|---|
| `X-Backend-Id` отсутствует, 503 сразу | модель не загружена и загрузка не началась | см. п. 2 и 5; проверить `CPPWORKER_MODELS_DIR` и наличие GGUF |
| 503 «model load started, retry» | идёт загрузка (минуты для 27B) | повторить запрос; при необходимости поднять `LB_AUTO_LOAD_WAIT_SEC` |
| Клиент (Cline) «не работает», запрос висит и рвётся | таймаут клиента меньше времени загрузки 27B | см. п. 2.1: грузить модель заранее, поднять таймаут клиента |
| `400 unknown field "options"` на `/v1/chat/completions` | клиент шлёт Ollama-поле в OpenAI-формат | `options` — только для `/api/chat`; в OpenAI-формате используйте `max_tokens`/`temperature` |
| Правка параметров модели из WebUI: 401 «invalid or missing API token» | балансер не знает токен cppworker (или у бэкенда пустой `cppWorkerApiToken`) | см. п. 5.1; проверить шапку агента (`API token`) и балансера |
| Токен изменён в `config/config.json`, но 401 остался | список токенов переопределён `LB_API_TOKEN` | см. п. 5.1: менять надо `deployments/.env` |
| Бэкенд `unhealthy` | балансер не может достучаться до `host:18092` | проверить `BACKEND_HOST`/сеть/файрвол (сценарий 2) |
| Бэкенд зарегистрирован, но `type=ollama` | задан `AGENT_BACKEND_TYPE` вместо `BACKEND_TYPE` | см. п. 5 |
| `maxConcurrentRequests=10` при `AGENT_MAX_CONCURRENT_REQUESTS=1` | унаследованное значение в `state.json` + старый образ балансера | см. п. 7.1 |
| Сомнение «применилась ли правка / какая сборка в контейнере» | неизвестна версия образа | см. п. 5.2: версия печатается в шапке логов |
| Два бэкенда на один endpoint | включена Go-регистрация cppworker и agent | `CPPWORKER_REGISTER_DISABLE=true` |

---

## 9. Остальные compose-файлы в папке

| Файл | Назначение | Статус |
|---|---|---|
| `docker-compose.yml` | самый ранний минимальный вариант (только balancer + том) | исторический |
| `docker-compose.full.yml` | полный стек с Ollama-бэкендами | исторический |
| `docker-compose.bundled-full.yml` | bundled + Ollama + webui | исторический |
| `docker-compose.cppworker-bundled.yml` | cppworker + balancer + webui (без агента) | рабочий, но без метрик GPU |
| `docker-compose.cppworker-bundled-with-agent.yml` | то же + агент | **есть дефекты имён переменных**; заменён на stack |
| `docker-compose.cppworker-with-agent.yml`, `…standalone.yml` | варианты пары cppworker+agent | исторические |
| `docker-compose.cppworker-remote.yml` | cppworker+agent к внешнему балансеру | рабочий (см. п. 3), но stack покрывает и это |
| `docker-compose.cppworker.yml`, `…llama.cpp.yml`, `…llama.cpu.yml` | разные варианты запуска cppworker | исторические |
| `docker-compose.agent.yml`, `…agent.gpu.yml` | запуск только агента | исторические |
| `docker-compose.rpc.yml` | RPC-координатор (этап P2) | не исполняется, см. /api/v1/placement |
| `docker-compose.cocoindex.yml` | внешний сервис cocoindex | не относится к ядру |
| `docker-compose.test-stub.yml`, `…test-stub.override.yml`, `…test-stub-fast.yml` | стаб-стенды для тестов | тестовые |
| `docker-compose.fanout-test.yml` | стенд проверки распределения запросов | тестовый (см. `scripts/test-backend-fanout.ps1`) |
