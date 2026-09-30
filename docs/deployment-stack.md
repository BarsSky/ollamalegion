# Развёртывание: один compose, три сценария

**Дата:** 2026-09-30 · Актуально для тегов `r83-submodule-v24+`
**Образы:** `ollama-legion/balancer:r83-submodule-v24+`,
`ollama-legion/agent:r83-submodule-v24+`,
`ollama-legion/webui:r83-submodule-v24+`,
`ollama-legion/cppworker:gpu-r83-submodule-v25+` (вместимость/параллельность —
см. п. 7 и 7.2; `cppworker v25` нужен, чтобы `/api/models` честно отдавал
`max_slots`).

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

## 5.3 Свой репозиторий образов: одна переменная `IMAGE_REGISTRY`

По умолчанию образы берутся по стандартному пути (`ollama-legion/balancer:<тег>`,
локально собранные). Если у вас свой registry, его адрес задаётся **одной**
переменной в `deployments/.env`:

```bash
# deployments/.env
IMAGE_REGISTRY=                       # стандартный путь (по умолчанию)
IMAGE_REGISTRY=local-docker-hub:5000/ # свой registry — СЛЭШ НА КОНЦЕ
```

Тогда compose подставит префикс во все четыре образа:

```
local-docker-hub:5000/ollama-legion/balancer:r83-submodule-v23
local-docker-hub:5000/ollama-legion/cppworker:gpu-r83-submodule-v23
local-docker-hub:5000/ollama-legion/agent:r83-submodule-v23
local-docker-hub:5000/ollama-legion/webui:r83-submodule-v23
```

Проверить, что получится, **не запуская** ничего:

```bash
cd deployments
docker compose -f docker-compose.stack.yml --profile full config | grep 'image:'
```

> ⚠️ **Слэш на конце обязателен.** Compose не нормализует пути: он склеивает
> строки буквально, и без слэша получится
> `local-docker-hub:5000ollama-legion/balancer` — невалидная ссылка.

**Сборка читает ту же переменную.** Скрипты добавляют слэш сами и превращают
заглушки (`local`, `docker.io`, `index.docker.io`) в пустой префикс, поэтому
собранный образ получает ровно то имя, которое ищет compose:

```powershell
# все образы с релизным тегом, в ваш registry (адрес — из deployments/.env)
powershell -File scripts/build-containers.ps1 -Tag r83-submodule-v23

# разовая сборка в другой registry без правки .env
$env:IMAGE_REGISTRY='other-reg:5000'; powershell -File scripts/build-containers.ps1 -Balancer -Tag test
```

Полный выпуск (`scripts/release-all.ps1`) и точечные пересборки
(`rebuild_balancer_r66a.ps1`, `rebuild_cppworker_r66a.ps1`, `rebuild_webui_r66d.ps1`)
тоже учитывают префикс — иначе `up -d` пошёл бы в registry за образом, который
только что собран локально под другим именем.

Проверка нормализации префикса (без Docker):

```powershell
powershell -File scripts/lib-image-registry.tests.ps1
```

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

## 7. Вместимость: сколько параллельных запросов примет бэкенд

Источник истины — сам cppworker:

1. cppworker знает свою параллельность (`CPPWORKER_N_PARALLEL`);
2. агент, если `AGENT_MAX_CONCURRENT_REQUESTS` не задан (в compose теперь
   дефолт `0` = AUTO), читает её у cppworker
   (`GET /api/v1/cppworker/config` → `config.defaultNParallel`) и сообщает
   балансеру;
3. балансер пропускает ровно столько одновременных запросов, остальные ждут
   в admission-очереди.

Поэтому настраивать надо **одно место**. `AGENT_MAX_CONCURRENT_REQUESTS`
нужен только как **операторский лимит** (имеет приоритет, автоматикой не
перезаписывается):

| `CPPWORKER_N_PARALLEL` | Что видит балансер | `AGENT_MAX_CONCURRENT_REQUESTS` |
|---|---|---|
| 0 или 1 | 1 | `0` (AUTO — значение по умолчанию) |
| 2 | 2 | `0` (AUTO) или `2` как явный лимит |

Завышенное значение даёт ложные «свободные слоты»: балансер считает, что можно
слать несколько запросов параллельно, хотя cppworker обслуживает один — клиент
получает отказы и зависания. Значение `<= 0` означает «не задано»: агент берёт
значение воркера, а если и его узнать не удалось — балансер применит дефолт по
типу бэкенда (`llama_cpp → 1`).

> ℹ️ До R83-fix вместимость задавалась в ДВУХ местах, и `environment:` в compose
> ВСЕГДА выставлял `AGENT_MAX_CONCURRENT_REQUESTS=1` (дефолт), поэтому агент
> никогда не брал значение из cppworker. При `CPPWORKER_N_PARALLEL=2` это давало
> ровно жалобу «первый клиент получил ответ, второй — 503»: воркер держал два
> слота, а балансер ставил второй запрос в очередь.

> ℹ️ До R83 параллельность считалась **двумя разными правилами**: C-bridge
> (llama.cpp `n_seq_max`) учитывал `CPPWORKER_N_PARALLEL`, а Go `SlotManager`
> брал только явное поле `parallel` из запроса загрузки. При
> `CPPWORKER_N_PARALLEL=2` это давало рассогласование: cppworker объявлял
> балансеру вместимость 2, а обслуживал по одному запросу — второй клиент молча
> ждал в `SlotManager.Acquire` и получал 503. Теперь оба места считает одна
> функция (`internal/cppbackend` → `resolveNParallel`), и она же питает
> регистрацию; в `cmd/cppworker` есть тест на согласованность.

> ⚠️ **Параллельность > 1: слоты ДЕЛЯТ окно, а не удваивают память.** Проверено
> на живом стенде (RTX 3070 8 ГБ, `Qwen3-Instruct-2507-q4km`, `c/llama.cpp`
> `gguf-v0.19.0-1369-g1d2869c6e`, `kv_unified=false` — дефолт этой сборки):
> * `parallel=1` — один запрос обслуживается, остальные **встают в очередь**.
>   Замер: два одновременных запроса, ~3.4 с и ~3.8 с, суммарно 7.2 с.
> * `parallel=2` — оба запроса выполняются **действительно одновременно**
>   (финишируют в один момент), оба 200, ответы не путаются.
> * `parallel=3` — проверено: оба запроса 200, падений нет.
>
> **Цена в VRAM — её нет.** Один и тот же `ctx=8192`, `gpuLayers=20`, `q4_0`:
>
> | parallel | свободно VRAM, МБ | `llama_context: n_ctx` | `n_ctx_seq` (на клиента) |
> |---|---|---|---|
> | 1 | 5197 | 8192 | **8192** |
> | 2 | 5197 | 8192 | **4096** |
> | 3 | 5191 | 8448 | **2816** |
>
> Причина — в `c/llama.cpp/src/llama-context.cpp`: при `kv_unified=false`
> `n_ctx_seq = GGML_PAD(n_ctx / n_seq_max, 256)`, а суммарный KV-кэш считается
> от `n_ctx` (при `parallel=3` llama.cpp сам подгоняет `n_ctx` до 8448 = 2816 × 3).
> Поэтому `contextSize` в API и в WebUI — **СУММАРНОЕ** окно: `16384 + parallel=2`
> означает **по 8192 на клиента**, и нигде об этом не было сказано.
>
> ✅ **Как задать окно НА КЛИЕНТА (R83, 2026-09-30).** Есть поле `contextPerSeq`:
> если оно > 0, суммарный `n_ctx = PAD256(contextPerSeq) × слоты`, и каждый клиент
> получает ровно запрошенное (memfit при этом проверяет суммарный объём). То же
> самое можно задать вручную: суммарное окно = окно × слоты. В WebUI поле
> «окно НА КЛИЕНТА» есть в блоке «Настройки по умолчанию», и рядом с `n_ctx`
> всегда показывается вторая строка — «n_ctx на клиента».
>
> ℹ️ Предыдущая версия этого раздела утверждала «второй слот стоил ~3.7 ГБ
> (вторая копия KV-cache)». Это было **неверно**: то измерение шло после
> перезагрузки, которая сменила раскладку слоёв (gpuLayers 20 → 36), и разницу
> в VRAM дали веса, а не KV. Контрольный замер с одинаковыми `gpuLayers`
> приведён в таблице выше.
>
> ✅ **Ранее наблюдавшийся `SIGABRT` (2026-09-29) больше не воспроизводится.**
> Тогда: `parallel=3` + три одновременных запроса **через балансер** к ЕЩЁ НЕ
> загруженной модели → падение cppworker. Сейчас тот же сценарий проходит:
> три параллельные загрузки подряд (`load-with-params`, `parallel=2`) приняты
> и дедуплицированы, `restarts=0`, в логах нет `assert`/`abort`; три
> одновременных клиентских запроса к незагруженной модели обслуживаются.
> Причина тогда была в контексте загрузки: она привязывалась к клиенту,
> который её инициировал, и обрыв этого клиента убивал общую загрузку
> (`model load aborted by user ... BRIDGE_ERR_ABORTED`) — теперь контекст
> загрузки всегда отвязан (`loadContextForSharedLoad`).
>
> Вместимость балансера берётся из факта: cppworker отдаёт `max_slots`
> в `/api/models` и объявляет свою параллельность при регистрации, поэтому
> балансер пропускает ровно столько одновременных запросов, сколько слотов
> заведено у модели (остальные ждут в очереди — в логе
> `admission: request queued, waiting for free slot`). Операторский лимит
> (`PUT /api/v1/backends/{id}/limits` или `AGENT_MAX_CONCURRENT_REQUESTS`) имеет
> приоритет и автоматикой не перезаписывается.

### 7.0 Что может «сбить» настройки, выставленные в WebUI

Проверено на живом стенде (2026-09-29). Настройки задаются один раз — при загрузке
модели, — поэтому важно знать, что именно их меняет:

| Действие | Влияет на настройки? |
|---|---|
| Загрузка/перезагрузка модели из WebUI (`contextSize`, `parallel`, `kvCacheType`, …) | ✅ применяется (это и есть настройка) |
| Клиентский запрос **вообще без `num_ctx`** (клиент не указал окно) | ❌ **нет** — если модель уже загружена, она не перезагружается: работаем с тем, что загружено. Если модель ещё не загружена — загрузка идёт значениями из настроек (профиль модели → «настройки по умолчанию» → env cppworker) |
| Клиентский запрос, которому **хватает** текущего контекста (`num_ctx` меньше загруженного) | ❌ нет — проверено: загружено 16384, запрос с `num_ctx=2048` → ctx остался 16384 |
| Клиентский запрос, которому нужно **больше** контекста (`num_ctx` > загруженного) | ⚠️ да — балансер запускает auto-reload и поднимает контекст. Проверено: 16384 → запрос с 32768 → ctx стал 32768 |
| AutoTune (авто-оптимизация загрузки) | ⚠️ может перезагрузить модель по своим рекомендациям; есть cooldown 5 мин после успеха и 60 с после ошибки |

> ℹ️ Правило «клиент молчит → не трогаем» добавлено R83 (2026-09-30). До него при
> `body_num_ctx=0` preflight подставлял `contextLength` профиля (это HINT) и при
> несовпадении с загруженным запускал async reload: оператор грузил модель с
> ctx=8192, а первый же запрос без `num_ctx` перезагружал её в 16384. В логе это
> видно как `preflightNCtxReload: клиент не задал num_ctx, модель уже загружена —
> reload не нужен` (раньше — `detected n_ctx mismatch, scheduling async reload`).

Чтобы клиентские запросы **не могли раздувать** контекст выше вашей настройки,
задайте потолок auto-reload в `deployments/.env`:

```bash
LB_NCTX_RELOAD_MAX_N_CTX=16384   # = вашей настройке contextSize
```

Тогда запрос с большим `num_ctx` получит понятный отказ (413 с указанием потолка)
вместо тихой перезагрузки модели. Совсем запретить auto-reload:
`LB_NCTX_RELOAD_ENABLED=false`.

Чтобы AutoTune не трогал параметры: в `config/config.json` либо
`balancing.autoTune: false` (глобально), либо у конкретного профиля
`"autoTune": false`.

> ℹ️ Известная дыра (исправлено в коде, ждёт раскатки): потолок
> `LB_NCTX_RELOAD_MAX_N_CTX` проверял только координатор reload, а второй путь
> (`preflightNCtxReloadIfNeeded`) его игнорировал — поэтому в текущей запущенной
> сборке клиентский запрос всё ещё может поднять контекст выше лимита.
> Исправление в коммите `b9ec496`: при `requested num_ctx > LB_NCTX_RELOAD_MAX_N_CTX`
> возвращается 413, модель остаётся с прежним `n_ctx`.

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

### 7.2 Как включить 2 параллельных запроса (пошагово)

Параллельность задаётся **в одном месте** — сколько слотов поднимает cppworker;
балансер узнаёт это сам. Порядок:

```bash
cd deployments
# 1. В .env:
#    CPPWORKER_N_PARALLEL=2
# 2. Перезапустить cppworker (переменная читается при старте):
docker compose -f docker-compose.stack.yml --profile full up -d --force-recreate cppworker-gpu
# 3. Убедиться, что cppworker поднял 2 слота (после того как модель загружена):
curl -s http://127.0.0.1:18092/api/models | jq '.models[] | {name, context_size, max_slots}'
#    → "max_slots": 2
# 4. Убедиться, что балансер видит ту же вместимость:
curl -s -H "X-API-Token: <токен>" http://127.0.0.1:18081/api/v1/backends \
  | jq '.backends[] | {id, maxConcurrentReqs}'
#    → "maxConcurrentReqs": 2
```

> ℹ️ Уже загруженная модель сохраняет свои слоты: после смены
> `CPPWORKER_N_PARALLEL` её надо перезагрузить (unload + запрос, либо
> «Применить» в WebUI). Иначе в `/api/models` останется прежний `max_slots`.

Три способа задать параллельность (приоритет сверху вниз):

| Способ | Когда использовать | Где |
|---|---|---|
| Поле `parallel` в запросе загрузки | разовая загрузка с нужным числом слотов | WebUI (страница моделей GGUF) / `POST /api/models/load-with-params` |
| Поле `parallel` в профиле модели | если у профиля `ignoreDefaults: false` | `config/config.json` → `llamaCppModelProfiles.<имя>.parallel` |
| `CPPWORKER_N_PARALLEL` | **основной способ**: инфраструктурная настройка контейнера | `deployments/.env` |

> ⚠️ Если у профиля стоит `"ignoreDefaults": true` (в этом стенде — у всех
> профилей), профиль **не** подставляет параметры загрузки, включая `parallel`.
> Это осознанно: `ignoreDefaults` означает «env контейнера выигрывает у профиля»
> (см. §7.0). Параллельность в этом режиме задаётся только `CPPWORKER_N_PARALLEL`.

> ℹ️ Раньше `parallel` из профиля доезжал до cppworker **только** через
> `POST /api/v1/cppworker/model-profiles/{name}/apply` и терялся при обычной
> загрузке (auto-load по `/api/chat`, `POST /api/models/load*`) — оператор
> сохранял 2 слота в WebUI, а модель поднималась с одним (`max_slots=1`), и
> второй клиент получал 503. Исправлено в `internal/balancer/model_management.go`
> (`applyProfileLoadParams`), там же есть тесты.

> ℹ️ Сохранение профиля в WebUI **не перезагружает** модель: кнопка
> «Сохранить» пишет настройки, применяет их «Применить (reload)». R83: после
> сохранения WebUI спрашивает, применить ли сразу, и объясняет, что иначе
> изменения вступят в силу при следующей загрузке модели.

### 7.3 Настройки загрузки: что доступно и как назначить их модели из WebUI

R83 (2026-09-30). В настройках llama.cpp появились два блока (страница
«Настройки» → llama.cpp → ниже Per-Model Profiles):

**«Настройки по умолчанию для новых моделей»** — тот самый конфиг, который
раньше играл роль «инициализации при старте балансера», а теперь редактируется
из WebUI. Для каждого поля показано значение и **источник**:

| Источник | Что это значит |
|---|---|
| «настройки по умолчанию» | значение задано оператором (config → `defaultModelProfile`) |
| «env контейнера» | значение берётся из env cppworker (`CPPWORKER_CTX_SIZE`, `CPPWORKER_N_PARALLEL`, …) |
| «не задано» | ни там, ни там — cppworker применит свой внутренний дефолт |

Приоритет при загрузке модели: **явный запрос → профиль модели → настройки по
умолчанию → env cppworker**. Кнопка «Изменить» пишет значения в конфиг
балансера (`PUT /api/v1/cppworker/load-defaults`) — перезапуск контейнеров не
нужен. Чекбокс «Не подставлять эти значения (ignoreDefaults)» возвращает
управление окружению контейнера.

**«Модели в папке»** — все GGUF-файлы, включая ещё не загруженные: размер,
статус, есть ли свой профиль (и не отключён ли он через `ignoreDefaults`),
какие `n_ctx`/`parallel`/`kv` к ним применятся и откуда взяты. Кнопка
«Настроить» открывает обычный визард профиля, предзаполненный текущими
значениями: сохранив профиль, вы тем самым **назначаете настройки файлу** — они
применятся при следующей загрузке или по «Применить (reload)».

```bash
TOKEN=$(grep '^CPPWORKER_API_TOKEN=' deployments/.env | cut -d= -f2)
# что доступно для загрузки (значения + источники + env контейнера)
curl -s -H "X-API-Token: $TOKEN" http://127.0.0.1:18081/api/v1/cppworker/load-defaults | jq
# модели в папке с эффективными настройками
curl -s -H "X-API-Token: $TOKEN" http://127.0.0.1:18081/api/v1/cppworker/model-catalog | jq '.files[] | {name, loaded, hasProfile, effective, source}'
# задать настройки по умолчанию (пример: 16K, q4_0, 20 слоёв, 2 слота)
curl -s -X PUT -H "X-API-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d '{"contextLength":16384,"batchSize":512,"numGpuLayers":20,"kvCacheType":"q4_0","parallel":2}' \
  http://127.0.0.1:18081/api/v1/cppworker/load-defaults | jq
```

Проверено на живом стенде: после `PUT` (16K/q4_0/20 слоёв/2 слота) модель,
загруженная БЕЗ явных параметров, поднялась именно так —
`effective n_ctx=16384`, `slot manager initialized maxSlots:2 parallel:2`, а
`/api/v1/cppworker/model-catalog` показал источник `defaultProfile` для всех
четырёх полей.

> ⚠️ Значения из «настроек по умолчанию» применяются к моделям, у которых нет
> своего профиля. Если раньше такая модель грузилась «как настроен контейнер»
> (например, `gpuLayers=20` из env), а в `defaultModelProfile` лежит другое
> значение (например, 30) — поведение изменится: теперь выигрывает конфиг.
> Проверьте блок «Настройки по умолчанию» перед первой загрузкой новой модели.

### 7.4 Почему обычный запрос Cline «не отрабатывал» (R83, 2026-09-30)

Симптом на локальном стенде (RTX 3070 8 GB, gemma-4, `parallel=2`): обычный
запрос Cline (system-промпт + `tools[]`, prompt ≈16k токенов, `max_tokens=8192`)
отвечал через **4.5 минуты**, WebUI не показывал ни занятую память, ни активную
модель, а в логах cppworker один раз был `double free or corruption` с
`SIGABRT`. Разобрано четыре независимых дефекта — все исправлены.

**1. Читающие эндпоинты ждали генерацию.** `inst.mu` удерживается на весь
инференс, а счётчик активных запросов (`getActiveQueries`) читался под тем же
мьютексом; его вызывают `GetMetrics` (→ `/api/info`) и `ListModels`
(→ `/api/models`). Живой лог: двенадцать `GET /api/info` с
`duration 2m51s / 2m36s / … / 6s` — все завершились в одну миллисекунду с
окончанием генерации; `GET /api/models` не отвечал вовсе (таймаут 25 s).
Именно поэтому WebUI и метрики балансера «замирали», а агент не отдавал
heartbeat. Теперь счётчики атомарные, чтение без мьютекса.

**2. Адаптивная стратегия считала бюджет без памяти самой модели.** Стратегию
спрашивают перед reload'ом модели, которая прямо сейчас занимает VRAM; в
`env.FreeVRAM` её памяти нет, поэтому для gemma-4 (4.0 GiB весов, 19 слоёв на
GPU, 4629 MB свободных) стратегия отвечала `stage=cpu_only, gpuLayers=0`.
Теперь `handleAdaptiveStrategy` возвращает в бюджет VRAM перезагружаемой модели
(она освобождается в момент reload'а) и логирует
`бюджет reload'а учитывает память самой модели`.

**3. Reload, который ничего не мог изменить.** `preflight` сравнивал ОЦЕНКУ
промпта (`tokencount`, консервативная верхняя граница) с окном слота: 44 230
против 32 768 → async reload «на 131072». Стратегия возвращала `nCtx=65536`
(ровно текущее окно) и `gpuLayers=0`, модель выгружалась и грузилась заново
2–3 минуты — без единого выигрыша. Измерено, насколько оценка расходится с
реальным токенизатором gemma-4 (`/api/v1/cppworker/debug/last-prompt`,
`prompt_tokens`):

| Текст | `tokencount.Estimate` | Реальных токенов | Расхождение |
|---|---|---|---|
| латиница | 622 | 204 | ×3.05 |
| JSON/tools | 459 | 229 | ×2.0 |
| кириллица | 1058 | 264 | ×4.0 |

Поэтому рост окна теперь **ограничен проверкой достижимости**: перед 503
балансер спрашивает ту же стратегию и пропускает reload, если окно не вырастет
или раскладка ухудшится (`preflight: рост окна пропущен — reload не дал бы
выигрыша`). Запрос в этом случае обслуживается в текущем окне, а если prompt
действительно не влезает — cppworker отвечает честным 413 с причиной.

**4. `double free` в C-мосте при prompt > 64 KB.** Буфер шаблона растёт через
`C.realloc` (`ret == -4` — обычный Cline/OpenWebUI-запрос с `tools[]`), а
`defer C.free(unsafe.Pointer(outBuf))` запоминает указатель в момент `defer`;
после переезда блока `free` освобождал уже освобождённую память → glibc
`double free or corruption (out)` → `SIGABRT` всего процесса (стек
`ApplyChatTemplateWithThinking → handleChat → /api/chat`). `recover()` в
`handlers_chat.go` такой abort не ловит — backend просто перезапускался, а
клиент получал `connection refused`. Освобождение теперь через замыкание.

Проверка после раскатки (те же Cline-подобные запросы):

```bash
TOKEN=$(grep '^CPPWORKER_API_TOKEN=' deployments/.env | cut -d= -f2)
# 1. /api/models отвечает ВО ВРЕМЯ генерации (раньше не отвечал вовсе)
curl -s -o /dev/null -w '%{time_total}s\n' -H "X-API-Token: $TOKEN" \
  http://127.0.0.1:18092/api/models
# 2. в логе балансера при тяжёлом запросе не должно быть "triggering ASYNC reload"
docker logs --since 10m ol-stack-balancer 2>&1 | grep -E 'reload|пропущен'
# 3. в логе cppworker не должно быть "double free"
docker logs --since 1h ol-stack-cppworker-gpu 2>&1 | grep -c 'double free'
```

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
| Второй одновременный клиент получает 503 «model is not loaded» / «model load aborted by user» | загрузку инициировал первый клиент, и её контекст был привязан к нему; либо модель поднята с `max_slots=1` | R83: контекст загрузки отвязан (`loadContextForSharedLoad`), `parallel` из профиля доезжает до cppworker; для 2 слотов — `CPPWORKER_N_PARALLEL=2`, см. §7.2 |
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
