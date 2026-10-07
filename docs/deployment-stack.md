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
| **1. Всё на одной машине** | `--profile full` | cppworker + imageworker + balancer + webui | одна GPU-машина, к ней подключаются клиенты |
| **2. Только бэкенды** | `--profile worker` | cppworker + imageworker | балансер уже работает на другой машине |
| **3. Только управление** | `--profile balancer` | balancer + webui | бэкенды регистрируются сами с других машин |
| 4. Внешний агент (Ollama / откат) | `--profile legacy-agent` | agent | Ollama-бэкенд, у которого нет своего кода, или откат к внешнему сборщику метрик |

> **R-Image follow-up (2026-10-07): где теперь живёт агент метрик.**
> Сборщик метрик для llama.cpp-воркеров встроен В cppworker
> (`AGENT_EMBEDDED=on`, порт 18034) и пишет в ту же запись бэкенда. Поэтому в
> профиле `full` отдельного контейнера `agent` больше НЕТ. Внешний агент
> остаётся для **Ollama**-бэкендов и только для них — он вынесен в ОТДЕЛЬНЫЙ
> профиль `legacy-agent`: у Ollama нет нашего Go-кода, метрики собрать некому.
>
> Два сборщика на одном бэкенде ставить НЕЛЬЗЯ: метрики приходят по очереди, и
> значения «мигают» — в частности `gpu.uuids`, по которому балансер понимает,
> что несколько бэкендов делят одну физическую GPU (иначе суммарная VRAM
> удваивается: 16 GB на карте 8 GB).

```bash
cd deployments
# первый раз:
cp .env.bundled-with-agent.example .env.bundled-with-agent
# в deployments/.env задать единый токен стека:
#   CPPWORKER_API_TOKEN=<ваш-токен>

# сценарий 1
docker compose -f docker-compose.stack.yml --profile full up -d
# сценарий 2 (адрес внешнего балансера — BALANCER_URL в .env!)
docker compose -f docker-compose.stack.yml --profile worker up -d
# сценарий 3
docker compose -f docker-compose.stack.yml --profile balancer up -d
# сценарий 4 (откат): внешний агент вместо встроенного
#   в .env: CPPWORKER_AGENT_EMBEDDED=off  и  BALANCER_URL=<внешний>
docker compose -f docker-compose.stack.yml --profile worker --profile legacy-agent up -d
```

Проверить, какие сервисы попадут в профиль, **не запуская** их:

```bash
docker compose -f docker-compose.stack.yml --profile full --services
# → cppworker-gpu, imageworker, loadbalancer, webui
docker compose -f docker-compose.stack.yml --profile worker --services
# → agent, cppworker-gpu, imageworker
```

**Сценарий 2 требует BALANCER_URL.** Без него воркеры пойдут на
`http://loadbalancer:18081` — имя сервиса, которого в этом профиле нет
(он живёт на другой машине), и регистрация будет вечно повторяться с ошибкой.
Указывается ОДИН раз в `deployments/.env`:

```bash
BALANCER_URL=http://192.168.1.10:18081     # машина с балансером
CPPWORKER_API_TOKEN=<тот же токен, что у балансера>
```

Значение уезжает во все три места: `CPPWORKER_BALANCER_URL` (сам воркер),
`SDWORKER_BALANCER_URL` (image-воркер) и `BALANCER_URL` (встроенный агент).

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

## 4.1 Смешанный режим: Ollama и llama.cpp одновременно

Балансер обслуживает **оба типа текстовых бэкендов одновременно** — это штатный
режим, а не эксперимент. Разведены они по поверхностям (портам), поэтому клиент
сам выбирает, каким путём идти, и ничего настраивать в клиенте не нужно.

| Внешний порт | Поверхность | Что принимает | Куда маршрутизирует |
|---|---|---|---|
| `18079` | OpenAI | `/v1/chat/completions`, `/v1/models`, `/v1/images/*` | llama.cpp- и image-бэкенды |
| `18080` | Ollama | `/api/chat`, `/api/generate`, `/api/tags`, `/api/ps` | Ollama- и llama.cpp-бэкенды |
| `18081` | Admin | `/api/v1/*` (регистрация, метрики, WebUI) | — (клиентских поверхностей нет) |

Проверено на живом стенде: `18079/v1/models` отвечает в формате OpenAI
(`{"data":[{"id":…,"object":"model"}]}`), `18080/api/tags` — в формате Ollama,
а `18081/v1/models` отдаёт 404: это админ-API, клиенты на него не ходят.

**Как выбирается бэкенд.** У каждого типа — свой роутер: `OllamaRouter`
(`BackendType=ollama`) и `LlamaCppRouter` (`BackendType=llama_cpp`), а image-путь
идёт в `ImageRouter`. Роутер отбирает бэкенды СВОЕГО типа
(`isBackendTypeAllowed`), поэтому запрос на Ollama-поверхность не уедет в
llama.cpp-воркер «по ошибке» и наоборот. Плюс есть осознанный кросс-фолбэк: если
на Ollama-поверхность пришла модель, которой у Ollama-бэкендов нет, а у
llama.cpp она загружена, запрос уйдёт в llama.cpp (см. `ollama_router.go`).

**Что это даёт на практике.** На одной машине можно держать «горячий» llama.cpp
(GGUF, свой n_ctx, KV-типы) и рядом Ollama с её моделями — клиент ходит и туда, и
туда через ОДИН адрес, а балансер учитывает оба пула.

### Поднять оба типа

```bash
cd deployments

# 1. llama.cpp-пул (cppworker + imageworker + балансер + webui)
docker compose -f docker-compose.stack.yml --profile full up -d

# 2. Ollama — на этой же машине, со своим портом (11434 по умолчанию).
#    Её регистрирует АГЕНТ: у Ollama нет нашего Go-кода, метрики собрать некому.
#    Для llama.cpp-воркеров агент не нужен — там он встроен (AGENT_EMBEDDED=on).
docker run -d --name ollama --gpus all -p 11434:11434 ollama/ollama

# 3. Агент под Ollama. Две переменные в deployments/.env:
#      AGENT_BACKEND_TYPE=ollama
#      OLLAMA_URL=http://host.docker.internal:11434
#    (обратите внимание: в .env имя AGENT_BACKEND_TYPE, а внутри контейнера оно
#     превращается в BACKEND_TYPE — так устроен compose-файл; см. §5)
docker compose -f docker-compose.stack.yml --profile legacy-agent up -d agent
```

> Внешний контейнер `agent` — **единственный** способ подключить Ollama-бэкенд:
> он регистрирует его в балансере и поставляет метрики. Для llama.cpp-воркеров
> агент не нужен — там он встроен (`AGENT_EMBEDDED=on`).
>
> `AGENT_BACKEND_TYPE` по умолчанию `llama_cpp` — прежнее поведение, поэтому
> стенды без Ollama ничего не замечают.

### Проверка, что оба типа видны

```bash
# оба типа в реестре
curl -s localhost:18081/api/v1/backends | jq '.backends[] | {id, type}'
#   → {"id":"cppworker-gpu-bundled-agent","type":"llama_cpp"}
#     {"id":"ollama-1","type":"ollama"}

# смешанный кластер: effectiveBackendType ПУСТ — это НОРМА, а не ошибка
curl -s localhost:18081/api/v1/cluster | jq '{effectiveBackendType, backendTypeCounts}'
#   → {"effectiveBackendType":"","backendTypeCounts":{"llama_cpp":1,"ollama":1}}

# какие роутеры задействованы (в логе балансера)
docker logs ol-stack-balancer 2>&1 | grep 'routeRequest: backend type resolved'
```

**Ловушка смешанного режима: `backendEngine`.** Если в `config/config.json`
(балансер) стоит `backendEngine: ollama_api`, то `getEffectiveBackendType()`
вернёт `ollama`, и `filterBackendsByEffectiveType()` выбросит ВСЕ llama_cpp-бэкенды
из выдачи `/api/v1/metrics` и `/api/ps`. Симптомы: монитор пуст, Ollama-клиенты
получают 503 «no healthy backends». Для стека с cppworker значение обязано быть
**`llama_cpp`**; при настоящей смеси оно остаётся `llama_cpp`, а
`effectiveBackendType` в `/api/v1/cluster` становится пустым — это признак
«показываем все типы».

---

## 4.2 Несколько машин: что суммируется, а что нет

Это главный вопрос при добавлении второй/третьей машины, и ответ различается
для текста и картинок.

| Что | Складывается ли VRAM нескольких машин | Как распределяется |
|---|---|---|
| **Текст** (llama.cpp) | **Да**, если модель разбита между машинами | послойно через RPC (см. ниже) или независимыми репликами |
| **Картинки** (stable-diffusion.cpp) | **Нет** | ОДИН воркер = ОДНА карта: генерация целиком на одной машине |

**Почему картинки не складываются.** `sd-server` — один процесс на одну модель,
`sd_ctx` создаётся один раз до `listen()`, а `ggml` внутри пропатчен, поэтому
разнести слои по сети нельзя (см. `docs/research-sdcpp-lowvram-integration.md`
§5.3). 24 ГБ «в трёх хостах» для генерации картинки **не объединяются**: каждая
генерация идёт целиком на карте одного воркера. Три машины дают три независимых
пула, а балансер распределяет между ними ЗАПРОСЫ (и не даёт двум генерациям
занять одну карту — политика `coexistence`).

**Что это значит на практике.** Модель картинок, которой нужно больше VRAM, чем
есть на одной карте, развернуть по сети нельзя. Обходится квантованием
(меньший GGUF) и offload (`--offload-to-cpu`, `--vae-tiling`).

### Распределение ТЕКСТОВОЙ модели по сети: статус в проекте

| Механизм | Файлы | Статус |
|---|---|---|
| Отдельные llama.cpp-воркеры на каждой машине, балансер раскидывает запросы | `cmd/cppworker`, `internal/balancer` | ✅ работает (штатный `--profile worker` на каждой машине) |
| RPC-координатор + RPC-воркеры (конвейер по слоям) | `internal/rpccoordinator`, `internal/rpcworker` | ⚠️ каркас: HTTP-слой, health, split/merge, KV-шарды и 66 тестов есть, но реальный инференс — **stub** (`stubMode`, «Реальная llama.cpp интеграция — B1.real»). Стек: `deployments/docker-compose.rpc.yml` |
| Tensor Parallelism (части матриц внутри слоя, `rptensor`) | `internal/rptensor` | ⬜ не начато: см. `plans/b8-tensor-parallelism-plan.md` — «НЕ НАЧАТО, 20–30 дней» |
| Интеграция `llama_rpc` / `ggml_backend_rpc` | — | ❌ в коде отсутствует |

**Вывод, который важно знать ДО покупки/подключения железа.** Сейчас 24 ГБ в
трёх машинах для ОДНОЙ большой текстовой модели штатным способом не собрать —
RPC-инференс в проекте ещё заглушка, а нативная поддержка `--rpc` у llama.cpp в
код не заведена. Реально работают два варианта:

1. **Реплики**: одна и та же модель грузится на каждой машине (влезает ли — по
   VRAM каждой), балансер распределяет запросы. Плюс — линейная
   производительность; минус — модель должна влезать в ОДНУ карту.
2. **Своя сборка с `--rpc`**: llama.cpp умеет `rpc-server` на каждой машине +
   `--rpc` у клиента, слои идут по сети. Это даёт «одна модель на трёх картах»,
   но требует своей сборки/конфигурации и **быстрой сети**: активации гоняются
   между узлами на каждом слое, поэтому 1 Гбит/с убивает выигрыш (ориентир —
   10 Гбит/с и выше, а лучше InfiniBand).

---

## 4.3 Карты прошлых поколений (GTX 1070 и родственные)

Короткий ответ: **для генерации картинок — да, подходит; для текста — да, но с
оговорками по скорости.**

**Почему подходит.** Обе сборки проекта считают Pascal (`sm_61`) поддерживаемым:

- `cppworker` (текст) собирается на **CUDA 12.2** (`docker/cppworker/Dockerfile.gpu`:
  `FROM ${CUDA_BASE}:12.2.0-devel-ubuntu22.04`). CUDA 12.x поддерживает Pascal;
  достаточно собрать с `CUDA_ARCH=61` (в `.env`/`build-containers.ps1`).
  Важно: CUDA **13** поддержку Pascal снял — не поднимайте версию toolkit выше 12.x.
- `imageworker` (картинки) собирается на **Vulkan** (`SD_SERVER_ASSET=...-vulkan.zip`)
  и на Vulkan работает независимо от CUDA-архитектуры. Vulkan ICD приезжает с
  драйвером, поэтому 1070 — рабочая карта для `sd-server`.

**Оговорки, которые стоит учесть:**

- **FP16 на Pascal медленный.** `1070` не умеет быстрый fp16 (он эмулируется),
  поэтому SD-модели на fp16 могут считаться медленнее, чем ожидается по числу
  ядер. На практике это значит «дольше на картинку», а не «не работает».
- **SDXL/FLUX на 8 ГБ** — по тем же правилам, что и на RTX 3070: нужен offload
  (`--offload-to-cpu`), тайлинг VAE и меньший квант. Ориентиры — в
  `docs/research-sdcpp-lowvram-quants.md`.
- **Разные карты в одном пуле — это нормально.** Балансер различает физические
  карты по UUID (`gpu.uuids`), поэтому «3070 + 1070» не будут перепутаны, а
  суммарная VRAM в Monitor не удвоится. Каждый воркер всё равно считает на своей
  карте; объединения памяти между ними НЕТ (см. §4.2).
- **Скорость пула задаёт самая медленная карта** в режиме реплик: запрос уйдёт на
  ту машину, где модель свободна, поэтому общая пропускная способность
  складывается, но отдельный запрос на 1070 будет медленнее, чем на 3070.

---

## 4.4 Вторая машина с воркерами: что задать и что проверить

Проверено на живой паре: балансер на `192.168.13.20`, воркеры на `192.168.13.34`.

### Что задать в `deployments/.env` НА УДАЛЁННОЙ МАШИНЕ

```bash
BALANCER_URL=http://192.168.13.20:18081     # ADMIN API балансера (:18081, НЕ :18080)
CPPWORKER_API_TOKEN=<тот же токен, что на балансере>
BACKEND_HOST=192.168.13.34                  # адрес ЭТОЙ машины, видимый с балансера

# ID записей — ОБЯЗАТЕЛЬНО уникальные между машинами (см. ниже про 409).
# На worker-хосте текстовый бэкенд регистрирует АГЕНТ, поэтому его ID — это
# AGENT_ID, а не CPPWORKER_REGISTER_NAME (cppworker сам не регистрируется:
# CPPWORKER_REGISTER_DISABLE=true).
AGENT_ID=cppworker-34
SDWORKER_BACKEND_ID=imageworker-34
```

`BACKEND_HOST` уезжает сразу в четыре места: `SDWORKER_ADVERTISE_HOST` (адрес для
`response_format:"url"` и проксирования), `AGENT_CPPWORKER_HOST` (адрес, под
которым балансер видит текстовый бэкенд), `AGENT_PUBLIC_HOST` (адрес `/health`
агента) и `CPPWORKER_ADVERTISE_HOST`. Раньше он подставлялся только в два из них,
поэтому даже успешная регистрация давала недостижимый адрес.

### ⚠️ Имя контейнера — НЕ адрес: проверьте поле HOST у каждого бэкенда

Если `BACKEND_HOST` на удалённой машине не задан (или контейнер не пересоздан
после его добавления), воркер регистрируется под именем своего контейнера —
`imageworker` / `cppworker-gpu`. Внутри docker-сети балансера эти имена
разрешаются в **его собственные** контейнеры, поэтому:

* запись удалённой машины выглядит живой и «своей»;
* метрики (CPU/GPU/VRAM) приезжают от неё честно — их шлёт её же агент;
* а вот **HTTP-запросы уходят на первую машину**, потому что URL строится из
  имени хоста.

Проверка на живой паре: `/api/v1/image/backends/IMAGEWORKER-34/models` возвращал
ровно тот же список из двух моделей, что и локальный `imageworker`, тогда как у
настоящего воркера второй машины моделей было **0**.

Как поймать:

```bash
# 1. В WebUI у каждого бэкенда поле HOST должно быть адресом (192.168.13.34),
#    а не именем контейнера. В API:
curl -s -H "X-API-Token: $TOKEN" http://localhost:18081/api/v1/backends \
  | jq '.backends[] | {id, host, nodeAddr}'

# 2. В логе балансера не должно быть предупреждения об одинаковом host:
docker logs ol-stack-balancer 2>&1 | grep 'advertise the SAME host'
#    registration guard: two different nodes advertise the SAME host —
#    host=imageworker backend=IMAGEWORKER-34 node=172.23.0.1
#    otherBackend=imageworker otherNode=172.23.0.5

# 3. На удалённой машине — что реально видит контейнер:
docker exec <imageworker-контейнер> env | grep -E 'BACKEND_HOST|ADVERTISE_HOST'
```

Лечение — задать `BACKEND_HOST=192.168.13.34` в `deployments/.env` на удалённой
машине и **пересоздать** воркеры (правка `.env` сама по себе контейнер не
трогает):

```bash
docker compose -f docker-compose.stack.yml --profile worker up -d --force-recreate cppworker-gpu imageworker
```


### Запуск на удалённой машине

```bash
cd deployments
docker compose -f docker-compose.stack.yml --profile worker up -d cppworker-gpu imageworker
```

Профиль `worker` поднимает **только** `cppworker-gpu` и `imageworker`. Внешний
агент в него больше не входит: у обоих воркеров свой встроенный сборщик метрик
(`AGENT_EMBEDDED=on`), а внешний нужен лишь для **Ollama**-бэкендов и запускается
отдельно:

```bash
docker compose -f docker-compose.stack.yml --profile legacy-agent up -d agent
```

⚠️ Если внешний агент остался запущенным на worker-хосте (например, поднимался
старой версией compose до того, как его вынесли в профиль `legacy-agent`),
**остановите его**:

```bash
docker compose -f docker-compose.stack.yml stop agent
```

Два сборщика на одной записи недопустимы в любом случае (значения метрик
начинают «мигать»), а в multi-host это ещё и второй претендент на тот же ID:
встроенный агент cppworker и внешний агент читают `AGENT_ID` из одного места и
будут отбирать запись друг у друга.

### Метрики собирает встроенный агент воркера

Оба воркера поднимают сборщик **в своём процессе** и регистрируются под ТЕМ ЖЕ
ID, что и сам воркер — отдельный контейнер не нужен:

| Воркер | Переменная | По умолчанию | Порт |
|---|---|---|---|
| `cppworker-gpu` | `CPPWORKER_AGENT_EMBEDDED` | `on` | `CPPWORKER_AGENT_PORT=18034` |
| `imageworker` | `IMAGE_WORKER_AGENT_EMBEDDED` | `on` | `IMAGE_WORKER_AGENT_PORT=18033` |

⚠️ **До 0.7.32 у imageworker это было `off`.** На удалённой машине, которая
следовала `.env.example`, получалась запись бэкенда **без телеметрии вообще**:
`hasAgent=false`, `agentPort=18032` (порт ЧУЖОГО контейнера) и
`gpuMemory`/`vramUsagePercent` в нулях. В WebUI это выглядит так:

```
IMAGEWORKER-34    GPU 0.0%   VRAM 0.0%   POWER 0W   0 MB / 1 MB
```

Проверка на воркере — в логе должно быть видно запуск встроенного агента:

```bash
docker logs <контейнер imageworker> 2>&1 | grep -iE 'embedded agent|Agent registered'
# ожидается:
#   [..] Agent registered successfully: imageworker-34
#   {"msg":"embedded agent started","agentId":"imageworker-34",...,"metricsPort":18033}
```

Если этих строк нет — задайте `IMAGE_WORKER_AGENT_EMBEDDED=on` в `deployments/.env`
на этой машине и пересоздайте воркер:

```bash
docker compose -f docker-compose.stack.yml --profile worker up -d --force-recreate imageworker
```

Выключать встроенный агент имеет смысл только тогда, когда метрики этого бэкенда
собирает внешний контейнер `agent` — и никогда оба одновременно.


### ⚠️ Почему вторая машина «не регистрируется»: одинаковые ID и host (R-MultiHost, 2026-10-07)

Симптом на живой паре: вторая машина (`192.168.13.34`) поднята, оба воркера
здоровы, а в балансере по-прежнему **две** записи, и на странице модели видны
CPU/GPU **второй** машины под URL **первой**. Вторая машина при этом не
обслуживает ни одного запроса — выглядит как «не зарегистрировалась».

Причина не в сети и не в токене. Compose-файл на обеих машинах одинаков, поэтому
совпадают буквально все поля запроса регистрации:

| Поле | На машине 1 | На машине 2 (по умолчанию) |
|---|---|---|
| `host` (из `AGENT_CPPWORKER_HOST`) | `cppworker-gpu` | `cppworker-gpu` |
| `backendId` / `agentId` | `cppworker-gpu-bundled-agent` | `cppworker-gpu-bundled-agent` |
| `cppWorkerPort` | `18092` | `18092` |
| `SDWORKER_BACKEND_ID` / host | `imageworker` | `imageworker` |

До ревизии R-MultiHost протокол регистрации не нёс **ни одного** признака,
различающего узлы, поэтому вторая машина проходила проверку
`isReRegistrationOfAutoBackend` (host и тип совпадают) и **молча переписывала
чужую запись**: `AgentID`, `AgentPort` и все метрики. Запросы при этом продолжали
идти на адрес первой машины, потому что её URL в записи не менялся — отсюда
«чужой GPU» на странице модели.

Теперь балансер различает узлы по адресу источника регистрации
(`types.Backend.NodeAddr`, см. `internal/api/registration_guard.go`) и отвечает
**409** с инструкцией, вместо тихого захвата:

```
Backend "cppworker-gpu-bundled-agent" is already served by node 172.18.0.5
(its agent is alive). Two hosts are using identical backend IDs and advertised
host names because the compose file is the same. On this node (192.168.13.34)
set a unique AGENT_ID (or SDWORKER_BACKEND_ID for the image worker) and
BACKEND_HOST=192.168.13.34, then restart the worker.
```

Правила guard'а (чтобы не сломать штатный рестарт):

* источник совпадает с `NodeAddr` записи → регистрация проходит как раньше;
* агент-владелец молчит дольше 60 с → запись считается осиротевшей, takeover
  разрешён (пересозданный контейнер получает новый bridge-IP и обязан
  перерегистрироваться);
* записи без `NodeAddr` (созданные до этой ревизии или вручную из WebUI) не
  защищаются — поведение прежнее; узел проставится при первой же регистрации.

Запрет на одну регистрацию проблему бы не решил: агент регистрируется **один раз**
при старте, а метрики и heartbeat шлёт в цикле. Поэтому телеметрия тоже проверяется
по узлу — `POST /api/v1/agents/metrics`, `/agents/heartbeat`, `/agents/v2/*` и
`/backends/{id}/agent/*` с чужого адреса получают `403`. Это же лечит «мигание»
метрик, когда на одном бэкенде оказываются два сборщика.

### Если бэкенд исчез со стенда: воркер вернётся сам

Раньше регистрация была одноразовой: если запись бэкенда удаляли из WebUI или её
забирал другой узел с тем же ID, воркер пропадал со стенда **навсегда** — до
ручного `docker restart`. В логе воркера это выглядело так:

```
Metrics send failed with status 404: {"error":"Backend not found. Please register agent first."}
```

Теперь (с 0.7.31) на `404`/`403` агент повторяет регистрацию не чаще раза в 20 с,
поэтому удалённая запись возвращается сама за считанные секунды. Если воркер
собран старой сборкой — единственное лечение `docker restart <контейнер>`.

### ⚠️ Том балансера должен принадлежать uid 1000

Правка `state.json` из вспомогательного контейнера **от root** (например, чтобы
проставить `nodeAddr` вручную) оставляет файл root-owned. Балансер работает под
uid 1000 и перестаёт сохранять состояние:

```
failed to write state file: open data/state.json: permission denied
```

Проверка и починка:

```bash
docker run --rm -v ol-balancer-data:/data alpine ls -la /data
docker run --rm -v ol-balancer-data:/data alpine chown -R 1000:1000 /data
```


Лечение то же, что и раньше: уникальные `AGENT_ID` и `SDWORKER_BACKEND_ID` плюс
`BACKEND_HOST` на новой машине (см. выше), и удалить лишнюю запись в WebUI, если
она осталась от прежних попыток.

### Диагностика за 4 шага

```bash
# 1. С удалённой машины: доступен ли балансер (ADMIN API)
curl -sS -o /dev/null -w '%{http_code}\n' http://192.168.13.20:18081/api/v1/ping
#    200 — сеть в порядке; 000 — firewall или неверный адрес; 401 — токен не тот
#    ⚠️ На машине балансера должен быть РАЗРЕШЁН входящий TCP 18081

# 2. На удалённой машине: что видит воркер
docker logs <контейнер> 2>&1 | grep -E 'registered with balancer|registration conflict|Agent registered'

# 3. На балансере: появилась ли запись
curl -s -H "X-API-Token: $TOKEN" http://localhost:18081/api/v1/backends \
  | jq '.backends[] | {id, host, type, status}'

# 4. С балансера: доходит ли он до воркера в ответ
curl -s http://192.168.13.34:18092/health
curl -s http://192.168.13.34:18093/health
```

Если шаг 3 показывает всё те же записи первой машины, а метрики в них — от
второй (сверьте модель CPU и `gpu.uuids`), дело именно в совпадающих ID/host из
таблицы выше, а не в сети: раз метрики второй машины доезжают, связь есть.

Если шаг 1 даёт `000`, а `/health` воркеров с балансера открывается — проблема
**исключительно** во входящем порту `18081` на машине балансера.


### ⚠️ Модели на каждой машине свои

Тома не разделяются: воркер второй машины видит **свой** `${MODELS_DIR}`. Модель,
загруженная на первой машине, на второй отсутствует — нужна либо копия файлов,
либо сетевой том. Балансер распределяет ЗАПРОСЫ, а не файлы.

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
`env.FreeVRAM` её памяти нет, поэтому для gemma-4 (4.0 GiB весов, 20 слоёв на
GPU, 4629 MB свободных) стратегия отвечала `stage=cpu_only, gpuLayers=0`.
Теперь `handleAdaptiveStrategy` возвращает в бюджет VRAM перезагружаемой модели
(она освобождается в момент reload'а) и логирует
`бюджет reload'а учитывает память самой модели`.

> ⚠️ Два подводных камня, найденных при проверке этого фикса (оба исправлены).
> (а) `handleAdaptiveStrategy` резолвит внешнее имя в canonical `…Q4_K_M.gguf`,
> а `/api/models` отдаёт имя **без** расширения — сравнение строк не срабатывало,
> и reclaim молча не применялся. (б) `ModelInfo.SizeBytes` у cppbackend часто
> `0` (размер берётся из gguf-меты, заполнена не всегда — см. R60.4), поэтому
> нужен `os.Stat(m.Path)`. Проверено на v38: в логе
> `free_vram_before_mb=7097, model_vram_mb=5743, free_vram_for_reload_mb=8191`,
> а стратегия на `n_ctx=65536` вместо `cpu_only` отдаёт
> `partial_offload, gpuLayers=30/42`.

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
(`preflight: рост окна пропущен — reload не дал бы выигрыша`). Запрос в этом
случае обслуживается в текущем окне, а если prompt действительно не влезает —
cppworker отвечает честным 413 с причиной.

> ⚠️ Первая версия этой проверки отклоняла reload ещё и при `stage=cpu_only`
> («стратегия понижает GPU-оффлоад»). **Живой прогон это опроверг:** в
> reload-запросе `gpuLayers: 0` cppworker трактует не как CPU-only, а как «взять
> из профиля» (`sync_profile.go`), после чего auto-раскладка даёт
> `new_gpu_layers: -1` → `gpuLayers=22`, `offloaded 22/43 layers` — раскладка
> становится **лучше**. Отклонение такого reload'а лишь лишало бы клиента нужного
> контекста (8192 → 65536). Проверка cpu_only убрана; явное понижение (например,
> 12 слоёв против 19) по-прежнему отсекается в `DoReload`
> (`clampStrategyToCurrentLayout`).

Проверено на живом стенде (два тяжёлых Cline-запроса подряд; balancer v37,
cppworker v37–v38):

1. запрос №1 при окне 8192 (профильный hint) → легитимный рост до 65536,
   `preflight: async reload succeeded, new_n_ctx=65536`;
2. запрос №2 при окне 65536 → **HTTP 200 без reload**, в логе
   `preflight: рост окна пропущен — reload не дал бы выигрыша … стратегия даёт
   суммарное окно 65536 при текущем 65536`;
3. во время генерации `GET /api/models` и `GET /api/info` отвечают за
   **0.01–0.03 с** (до фикса — минуты или таймаут);
4. `prompt > 64 KB` + `think: true` (`/api/chat`) → HTTP 200, `RestartCount=0`,
   в логе нет `double free`.

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

## 7.5 Политика контекстного окна и параметров (R83-политика, 2026-10-01)

Правило, по которому балансер решает «грузить / перезагружать / отказать».
Раньше окно росло по ОЦЕНКЕ промпта (`tokencount` — верхняя граница, завышает в
2–4 раза: 44 230 против реальных 16 210 на живом запросе Cline), поэтому обычный
запрос уходил в перезагрузку на 2–3 минуты. Теперь решение принимается **только
по тому, что клиент указал явно**.

| Ситуация | Что делает балансер |
|---|---|
| Клиент требует reasoning (`think: true`), а модель загружена с выключенным | **409** с перечислением отличий: `param_differences`, `loaded`, `requested`, `what_to_do`. Тихая подмена поведения недопустима |
| Клиент **не указал** `num_ctx` | **NoOp.** Никаких перезагрузок «по оценке»; если история не влезает — cppworker отвечает своей причиной, балансер её передаёт |
| Модель **не загружена**, клиент указал `num_ctx` | **NoOp**, cppworker грузит модель **окном клиента**: `ensureModelLoadedWithNCtx` пересчитывает «на клиента → суммарно» (`num_ctx × слоты`, выравнивание до 256) |
| Загруженное окно на клиента **≥** запрошенного | **NoOp.** Загружено больше — перезагружать не нужно и уведомлять не о чем |
| Загруженное окно **<** запрошенного, влезает в потолок | **Reload** под окно клиента (остальные параметры совпадают) |
| Запрошенное окно **не влезает** (GGUF-потолок / операторский cap / cppworker не может дать столько) | **413** с понятным текстом: сколько просили, сколько доступно на клиента при N слотах, что делать |

Порядок источников окна при загрузке:
**`num_ctx` запроса → профиль модели → env `CPPWORKER_CTX_SIZE`**
(лестница реализована в `sync_profile.go`; запрос не перебивается профилем).

Единицы измерения важны: `num_ctx` клиента — окно **на клиента**, а `n_ctx`
модели — **суммарное** (llama.cpp делит его между слотами:
`n_ctx_seq = PAD(n_ctx / n_seq_max, 256)`). Пересчёт — `loadTotalNCtxForRequest`
(cppworker) и `perClientToTotal` (balancer).

### Рабочая конфигурация стенда (малая модель + 32768 на клиента)

```bash
# Профиль gemma-4: суммарное окно 65536 при parallel=2 → 32768 каждому клиенту
TOKEN=$(grep '^CPPWORKER_API_TOKEN=' deployments/.env | cut -d= -f2)
curl -s -X PUT -H "X-API-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d '{"contextLength":65536,"batchSize":512,"numGpuLayers":-1,"flashAttn":true,
       "useMmap":true,"ignoreDefaults":true,"kvCacheType":"q4_0",
       "contextLengthAuto":true,"contextLengthMax":131072}' \
  http://127.0.0.1:18081/api/v1/cppworker/model-profiles/gemma-4-E4B-it-Q4_K_M
```

В `deployments/.env`: `CPPWORKER_CTX_SIZE=65536` (та же логика: суммарное окно по
умолчанию) и `CPPWORKER_N_PARALLEL=2`.

Проверка (все ветки политики):

```bash
TOKEN=$(grep '^CPPWORKER_API_TOKEN=' deployments/.env | cut -d= -f2)
# 1. холодная загрузка окном клиента: ждём context_per_seq=32768, context_size=65536
curl -s -X POST -H "X-API-Token: $TOKEN" "http://127.0.0.1:18092/api/models/unload?name=gemma-4-E4B-it-Q4_K_M"
curl -s -H 'Content-Type: application/json' -H "X-API-Token: $TOKEN" \
  -d '{"model":"gemma-4-E4B-it-Q4_K_M","messages":[{"role":"user","content":"ok"}],
       "options":{"num_ctx":32768,"num_predict":8}}' http://127.0.0.1:18080/api/chat
curl -s -H "X-API-Token: $TOKEN" http://127.0.0.1:18092/api/v1/cppworker/config/runtime \
  | jq '.loaded_models[] | {context_size, gpu_layers}'
# 2. окно выше потолка → 413 с объяснением (а не молчаливый reload)
curl -s -o - -w '\n%{http_code}\n' -H 'Content-Type: application/json' -H "X-API-Token: $TOKEN" \
  -d '{"model":"gemma-4-E4B-it-Q4_K_M","messages":[{"role":"user","content":"hi"}],
       "options":{"num_ctx":131072}}' http://127.0.0.1:18080/api/chat
# 3. think=true при reasoning=off → 409 с param_differences
curl -s -o - -w '\n%{http_code}\n' -H 'Content-Type: application/json' -H "X-API-Token: $TOKEN" \
  -d '{"model":"gemma-4-E4B-it-Q4_K_M","messages":[{"role":"user","content":"hi"}],
       "think":true,"options":{"num_ctx":16384}}' http://127.0.0.1:18080/api/chat
```

### Раскатка — одним методом

Стенд живёт под `deployments/docker-compose.stack.yml` (проект `ol-stack-*`).
Поэтому и выпуск идёт тем же compose:

```bash
pwsh -File scripts/release-all.ps1 -Tag r83-submodule-vNN -Services balancer,cppworker
#   → по умолчанию: docker compose -f docker-compose.stack.yml --profile full up -d
```

Прежний default (`docker-compose.cppworker-bundled-with-agent.yml`, проект
`ol-bundled-*`) — другой проект на тех же портах: `up -d` падал с
«Bind for 0.0.0.0:18080 failed: port is already allocated», и стенд оставался на
старых образах. Параметры `-ComposeFile` / `-ComposeProfile` позволяют указать
другой файл, но по умолчанию всегда берётся рабочий.

---

## 7.6 Таймауты генерации: доктрина «клиент решает» (R83, 2026-10-01)

**Правило.** Балансер НЕ обрывает генерацию по своему счётчику. Соединение живёт
до одного из трёх событий: клиент закрыл соединение (`r.Context().Done()`),
бэкенд вернул ошибку/EOF, бэкенд умер. Всё остальное — «плотное отслеживание
ошибок», а не «мы ждали N секунд и нам надоело».

**Что было и чем это кончилось.** Таймауты вычислялись по размеру модели и
истории запросов: `firstByte = 300s` для «medium»-класса
(`ModelLatencyTracker.computeModelStats`), 600s / 1200s / 1800s для более
крупных, плюс значения из `config.json` (`firstByteTimeout: 900`,
`streamingIdleTimeout: 600`, `requestTimeout: 600`). Замеры на живом стенде
(gemma-4, RTX 3070, partial offload):

| Промпт | Prefill | Итог |
|---|---|---|
| 22 285 токенов | 296.4 с | успел **до** 300-секундной отсечки |
| 22 547 токенов (реальный запрос Cline) | ~299–300 с | **обрыв на 299.877 с**, `tokens_sent: 0` |
| 28 405 токенов (после снятия таймаутов) | 394.4 с | **`done:true`**, полный ответ |

То есть запрос резался ровно на границе: 296 с проходило, 300 с — нет.

**Как теперь.** Приоритет:

1. `LB_STREAMING_NEVER_TIMEOUT=1` — мастер-выключатель (стоит в
   `deployments/.env.bundled-with-agent`);
2. явный opt-in через ENV: `LB_STREAMING_TIMEOUT_SEC`,
   `LB_FIRST_BYTE_TIMEOUT_SEC`, `LB_STREAMING_IDLE_TIMEOUT_SEC`,
   `LB_REQUEST_TIMEOUT_SEC`;
3. per-model профиль (`llamaCppModelProfiles[m].*TimeoutSec`) — осознанное
   решение по конкретной модели;
4. иначе — **0, таймаута нет**.

Значения из `config.json` и эвристики по размеру GGUF больше не применяются
автоматически: они остались в `ModelLatencyTracker.Recommended*` как диагностика
(их видно в API/логах, но они не обрывают работу).

```bash
# проверить, что таймауты действительно выключены
docker exec ol-stack-balancer sh -c 'env | grep NEVER_TIMEOUT'
# и что медленный запрос доезжает (пример: ~28k токенов промпта ≈ 400 с prefill)
docker logs --since 10m ol-stack-balancer 2>&1 | grep -E 'STREAMING_IDLE_TIMEOUT|proxy failed'
```

> ⚠️ Обратная сторона: если бэкенд зависнет «намертво» (не отвечает и не рвёт
> соединение), запрос будет висеть до тех пор, пока клиент сам не отвалится.
> Для таких случаев есть явные opt-in ENV выше и профиль модели; наблюдать
> зависания помогает `GET /api/models` (поле `active_queries`) и монитор.

### 7.6.1 То же правило для загрузки, pull, reload и HTTP-серверов (R83/v67, 2026-10-02)

**Формулировка доктрины, по которой вычищался весь код:**

> Допустимы только таймауты **опроса состояния** — «каждые N секунд проверить,
> готово ли» (poll interval, health probe, метрики, heartbeat, idle-соединение
> между запросами). Такой таймер задаёт частоту взгляда и никогда не отменяет
> работу.
>
> **Запрещены duration-капы на работу**: любой `time.After` / `context.WithTimeout`
> / `http.Client.Timeout`, который по истечении срока **обрывает** загрузку модели,
> генерацию, pull, reload, unload. Признак дефекта — сервер отдаёт ошибку, а
> upstream продолжает работу: состояние расходится, оператор видит «модель не
> грузится / ответ обрезан / context canceled» и ищет причину вслепую.
>
> Вместо капа — ждать **терминального состояния** (успех / явная ошибка /
> недоступность бэкенда / закрытие клиентского соединения) и возвращать
> конкретную ошибку.

Что было снято в v67 (каждый пункт — потенциальный «обрыв на ровном месте»):

| Место | Было | Почему это ломало работу |
|---|---|---|
| `cmd/cppworker/main.go` | `WriteTimeout = 30m`, `ReadTimeout = 30s` | обрыв потокового ответа и приёма большого тела ровно по часам |
| `cmd/balancer/main.go` (API-сервер, TLS-прокси, TLS-API) | `WriteTimeout = 60s` / `RequestTimeout+30` | load/reload/apply длиной больше минуты отдавали ошибку, хотя работа шла |
| `internal/balancer/model_management.go` | `Client.Timeout = 10m`, load-клиент = `ModelLoadTimeout` | pull/load 15.7 ГБ не укладывается — «Client.Timeout exceeded» при живом прогрессе |
| `internal/balancer/auto_pull.go` | `Client.Timeout = 10m`, `PullTimeout = "5m"` | закачка большой модели «падала по таймауту» в глазах вызывающего |
| `internal/balancer/llamacpp_backend_helpers.go`, `ollama_router.go` (`proxyHTTP`) | 120 с / 30 с | `/api/pull` — это скачивание модели, а не быстрый endpoint |
| `internal/balancer/autoload_wait.go` | hard ceiling 30 мин | обрывал ожидание **даже при растущем прогрессе** |
| `internal/balancer/preflight_nctx.go` | ожидание reload 240 с | первый же запрос клиента получал 503 «being reloaded» |
| `internal/balancer/nctx_reload.go` (`DoReload`) | `context.WithTimeout(ctx, 0)` при выключенном капе | немедленно истёкший контекст → reload падал до обращения к бэкенду |
| `cmd/cppworker/lazyload.go` | дефолт 30 минут | отмена разделяемой загрузки → `BRIDGE_ERR_ABORTED` («load cancelled») |
| `cmd/cppworker/cli_auto_load.go` | `Client.Timeout = 30s` | CLI сообщал ошибку, пока cppworker продолжал грузить |

**Как вернуть кап, если он действительно нужен** (защита от багованного
upstream). Только явной переменной окружения, и при взведении печатается
громкий `WARN` с именем переменной — чтобы «почему оборвалось» имело ответ в
логе:

| Переменная | Что ограничивает |
|---|---|
| `LB_ALLOW_PROFILE_TIMEOUTS` | таймауты из per-model профилей (WebUI) |
| `LB_ALLOW_REQUEST_TIMEOUTS` | адаптивный per-request таймаут прокси |
| `LB_ALLOW_MODEL_OP_TIMEOUT_SEC` | HTTP-клиент операций с моделью (pull/push/show/create) |
| `LB_ALLOW_MODEL_LOAD_TIMEOUT_SEC` | HTTP-запрос загрузки модели |
| `LB_ALLOW_LOAD_WAIT_CAP_SEC` | ожидание готовности модели (poll-циклы) |
| `LB_ALLOW_NCTX_RELOAD_TIMEOUT` | reload/preflight контекста |
| `LB_ALLOW_GGUF_PROXY_TIMEOUT_SEC` | прокси WebUI → cppworker (load/unload/reload/hf) |
| `LB_ALLOW_RPC_INFER_TIMEOUT_SEC` | распределённый инференс через RPC-координатор |
| `LB_ALLOW_RPC_TP_INFER_TIMEOUT_SEC` | тензор-параллельный инференс (TP) |
| `LB_AUTO_CONTINUE_TIMEOUT_SEC` | автопродолжение обрезанного ответа |
| `LB_NCTX_PREFLIGHT_WAIT_SEC` | ожидание первого async-reload: `N` сек, `0` = не ждать (по умолчанию ждём терминального состояния) |
| `LB_AUTO_LOAD_WAIT_SEC` | бюджет ожидания авто-загрузки: `N` сек, `0` = не ждать, отрицательное = до потолка |
| `CPPWORKER_WRITE_TIMEOUT` / `CPPWORKER_READ_TIMEOUT` | HTTP Write/ReadTimeout сервера cppworker (`0` = без ограничения, дефолт) |
| `CPPWORKER_LAZY_LOAD_TIMEOUT_SEC` | контекст разделяемой (ленивой) загрузки модели |

Единая точка правды в коде — `internal/balancer/timeout_policy.go` (там же
реестр и хелпер `optInTimeoutSeconds`, который печатает WARN при взведении).

```bash
# 1. серверы: все четыре значения обязаны быть 0
docker logs --since 5m ol-stack-balancer 2>&1 | grep -E 'HTTP server timeouts'
docker logs --since 5m ol-stack-cppworker-gpu 2>&1 | grep -E 'HTTP server timeouts'
#   ждём: read_timeout_sec=0 write_timeout_sec=0

# 2. в контейнерах не должно быть взведённых opt-in капов
docker exec ol-stack-balancer sh -c 'env | grep -E "LB_ALLOW_|LB_NCTX_PREFLIGHT_WAIT_SEC|LB_AUTO_LOAD_WAIT_SEC"'
docker exec ol-stack-cppworker-gpu sh -c 'env | grep -E "CPPWORKER_(WRITE|READ|LAZY_LOAD)_TIMEOUT"'

# 3. живая проверка: pull/load большой модели доезжает до конца
docker logs -f ol-stack-balancer 2>&1 | grep -E 'auto-pull|ensureModelLoadedOnBackend'
```

---

## 7.7 «Model returned empty response»: разбор по логам (R83, 2026-10-01)

Симптом: Cline на gemma-4-E4B-it-Q4_K_M отвечает `Model returned empty response`
(первый разбор — §7.4). На живом стенде разобрано ДО КОНЦА: в журнале сессии
Cline (`~/.cline/data/sessions/<id>/*.messages.json`) лежат два РАЗНЫХ сбоя.

**Сбой 1 — пустой вывод модели.** Текст ошибки, который показал Cline, —
дословно наш чанк: `model produced an empty response (inference succeeded but
output is empty)`. В логе cppworker этому соответствует WARN с `raw_len=0`:
модель завершила ход первым же токеном EOG и не сгенерировала ни одного байта.
Воспроизведено пробником (`debug/streamprobe`) на запросе Cline с 18 tools —
примерно каждый второй-третий прогон. Лечение: один повторный проход генерации
(`CPPWORKER_EMPTY_OUTPUT_RETRY`, default true) + диагностический WARN с
`raw_len`; подтверждение из лога:

```
WARN  writeChatStreamResponseWithTools: пустой вывод модели (raw_len=0) — повторная генерация  prompt_tokens=2513
INFO  writeChatStreamResponseWithTools: повторная генерация завершена  raw_len=311
```

**Сбой 2 — клиент отвалился раньше ответа.** `Model returned empty response` —
это уже текст САМОГО Cline. `/api/v1/cppworker/debug/last-stream` по 5-минутной
попытке: `tokens_sent=0, bytes_written=380, reason=ctx_done_on_write,
duration_ms=306047`. 380 байт за 306 с — это ровно 20 keepalive-ов
(`{"keepalive":true}` раз в 15 с); ни одного токена ответа клиент не получил.
Причина — tools-ветка `/api/chat` буферизовала ответ целиком и отдавала его
одним финальным чанком.

**Почему ответ и не успевал.** Модель работала на 35 слоях из 42 (7 слоёв на
CPU) — 5.3–9.4 tok/s. Проверка показала, что memfit завышал KV-кэш в 4–6 раз:
у gemma-4 кэш держат 24 слоя из 42 (`attention.shared_kv_layers=18`), из них
глобальных всего 4 (слои 5/11/17/23, паттерн окна `111110×7`), остальные 20 живут
в окне 512 токенов с отдельной головой (`key_length_swa=256`). Оценка «все слои ×
n_ctx» давала 3.17 GB вместо 294 MiB на n_ctx=65536.

Исправлено четыре слоя одного дефекта: раскладка KV (`KVPlan`, `internal/cppbackend/kv_layers.go`),
перенос раскладки в вердикт memfit, поиск метаданных по имени клиента
(`GetModelMetaResolved`) и применение профиля при ленивой загрузке cppworker.
Живой результат:

| Метрика | До | После |
|---|---|---|
| Слоёв на GPU | 35/42 (ленивый путь 19/42) | **42/42** |
| `tokenLatencyMs` | 125–1035 | **32** |
| Генерация | 5.3–9.4 tok/s | **31.6 tok/s** |
| Cline-образный запрос (18 tools) через балансер | 31 s | **4.9 s**, `done_reason=tool_calls` |

Дополнительно: tools-ветка теперь отдаёт prose дельтами, удерживая окно 32 байта
вокруг маркера tool call (`CPPWORKER_TOOLS_STREAM_CONTENT`, default true), а в
`c/bridge/bridge.c` убран повтор всей накопленной выдачи при срабатывании
antiprompt (дублирование хвоста до ~1 КБ) — вместо этого хвост удерживается и
каждый токен уходит клиенту ровно один раз.

Проверка (пробник показывает время прихода каждого чанка, ищет дублирование
хвоста, утечку маркера tool call и битый UTF-8):

```bash
go run ./debug/streamprobe http://127.0.0.1:18080/api/chat debug/probe_tools.json
# ждём: DONE reason=tool_calls, контента нет (JSON вызова в content не утекает)
go run ./debug/streamprobe http://127.0.0.1:18080/api/chat debug/probe_prose_tools.json
# ждём: сотни контентных чанков, «UTF-8 в ответе корректен», «дублирования хвоста не обнаружено»
```

---

## 7.7.1 Размышления (reasoning): контракт и проверка (R83/v67, 2026-10-02)

**Где живёт флаг.** Галочка «Enable reasoning» на странице GGUF Models пишется в
профиль модели (`enableReasoning`) и доезжает до cppworker телом загрузки
(`POST /api/models/load`), после чего её видно как `reasoning_enabled` в
`GET /api/models` и как `reasoningEnabled` в `loadedModels` балансера.

**Приоритет** (от старшего к младшему): явный `think` клиента в запросе →
per-model флаг из профиля (WebUI) → глобальный `config.EnableReasoning`.

**Что видит клиент.** Рассуждение уходит отдельным полем (`reasoning_content`
в OpenAI-совместимом ответе, `reasoning` / `thinking` в нативных Ollama-путях),
видимый ответ — в `content`. Маркеры (`<|channel>thought`, `<channel|>`,
`<think>` и др.) в ответ не попадают: потоковый парсер удерживает неполный тег
до тех пор, пока не станет ясно, тег это или обычный текст.

Было две живые жалобы, обе закрыты в v67/v69:

1. «Флаг включить размышление был активен, однако при ответе размышление не
   применилось» — сборка промпта смотрела только на ГЛОБАЛЬНЫЙ
   `config.EnableReasoning` и per-model состояние (то, что ставит галочка) не
   читала вовсе. Диагностика, которой это ловится:
   `GET /api/v1/cppworker/debug/last-prompt` — если в `prompt_head` нет
   thinking-инструкции при `reasoning_enabled=true`, дело именно в этом.
2. «Ответ обрезан / в ответе мусор» — в потоке первые символы маркера
   (`<|c`, `h`, `a`, `nn`) уходили в ВИДИМЫЙ content, а остальное — в
   reasoning. Проверка (SSE-дельты не должны содержать `<|` и `channel`):

```bash
# 1. включён ли режим у ЗАГРУЖЕННОЙ модели
curl -s http://127.0.0.1:18092/api/models | jq '.models[] | {name, reasoning_enabled}'
#   ждём: reasoning_enabled = true

# 2. дошла ли политика до промпта (thinking-инструкция в system)
TOKEN=$(grep '^CPPWORKER_API_TOKEN=' deployments/.env | cut -d= -f2)
curl -s -H "X-API-Token: $TOKEN" \
  "http://127.0.0.1:18092/api/v1/cppworker/debug/last-prompt" | jq '{model, prompt_head}'
#   ждём: в prompt_head есть требование рассуждать (<reasoning>...</reasoning>)

# 3. разделение в потоке: ни одного фрагмента маркера в content
curl -sN -H 'Content-Type: application/json' \
  -d '{"model":"gemma-4-E4B-it-Q4_K_M","messages":[{"role":"user","content":"Think step by step: what is 17*23?"}],"stream":true,"max_tokens":300}' \
  http://127.0.0.1:18080/v1/chat/completions \
  | grep '^data: {' | sed 's/^data: //' \
  | jq -r 'select(.choices[0].delta.content) | .choices[0].delta.content' \
  | grep -E '<\||channel' && echo 'УТЕЧКА МАРКЕРА' || echo 'чисто'
```

**Границы генерации.** `max_output_tokens` (его шлёт Cline) — это верхняя
граница «сколько ответа клиент примет», а не запрошенная длина: она может
только уменьшить `num_predict`, но не задать его. Иначе «приму до 64000»
превращается в «сгенерируй 64000» (десятки минут на слабой карте, обрыв по
клиентскому таймауту, «пустой ответ»).

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
