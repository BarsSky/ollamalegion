# Развёртывание: разборы инцидентов и ловушки

Этот файл — «долгая память» по развёртыванию OllamaLegion. Сюда вынесены разборы
живых инцидентов, которые раньше лежали комментариями прямо в
`docker-compose.stack.yml`. Сам compose намеренно сокращён и переведён на
английский: он должен читаться за минуту, а подробности — жить здесь.

Короткие инструкции «что запустить» — в [deployment-stack.md](deployment-stack.md).
Подробный сценарий развёртывания на нескольких машинах — там же, §4.4.

---

## 0. Шум в консоли WebUI: что наше, а что нет

Прежде чем искать причину в проекте, разделите сообщения по источнику.

**Наше** — строки, где в имени модуля есть токен версии, например
`index.js?v=v0.7.36-multihost:180 [i18n] ...` или
`gguf-renderer-refresh.js?v=v0.7.36-multihost:...`. Токен `?v=` подставляет
Dockerfile при сборке образа webui, поэтому по нему видно, какая сборка отвечает.
Если версия в консоли отличается от `WEBUI_TAG` в `deployments/.env` — браузер
держит старые модули, нужен Ctrl+F5.

**Не наше** — строки без токена версии, например `contentscript.js:14083
MaxListenersExceededWarning` вместе с `ObjectMultiplex - orphaned data for stream
"app-init-liveness"`. `contentscript.js` — это content script расширения браузера
(кошелёк, менеджер паролей), к WebUI проекта он отношения не имеет; такие
предупреждения безопасно игнорировать.

Отдельно про `[i18n] Missing translation key: <ключ>` — это НАШЕ сообщение.
`webui/js/i18n/index.js` при отсутствии перевода пишет warning и **возвращает сам
ключ**, поэтому там, где вызов идёт без обёртки с fallback, на экране окажется
`settings.profiles.step_busy` вместо текста. Полноту переводов проверяет
`internal/api/i18n_keys_lint_test.go`:

```bash
go test -tags llama_stub ./internal/api/ -run TestI18nKeys
```

---

## 1. ⚠️ Имена переменных: что код реально читает

### 1.0 Машину выключили, а бэкенд остался «здоровым»

Симптом: удалённую машину гасят, но её бэкенд в WebUI остаётся `healthy` и
показывает `52 °C`, `405 MHz`, `1.4% VRAM` — числа последнего опроса, выданные за
текущие. Лечится это с двух сторон, и обе важны.

**Почему запись могла не отключиться.** Health-check строит URL из поля `Host`
записи. Если `Host` — имя контейнера (`imageworker`), то внутри docker-сети
балансера оно разрешается в ЕГО СОБСТВЕННЫЙ контейнер: проверка бьёт не туда и
всегда успешна, сколько бы удалённая машина ни была выключена. Это следствие
ловушки интерполяции, разобранной в `docs/deployment-stack.md` §5.0, — проверьте
`host` у записей:

```bash
curl -s -H "X-API-Token: $TOKEN" http://localhost:18081/api/v1/backends?includeUnhealthy=true \
  | jq '.backends[] | {id, host, status, lastAgentContact}'
```

`host` должен быть адресом машины (`192.0.2.11`), а не именем контейнера.
С 0.7.37 балансер дополнительно не доверяет HTTP-проверке таких записей: если
одинаковый `Host` объявлен двумя бэкендами РАЗНЫХ узлов, состояние определяется по
живости собственного агента, а не по URL.

**Почему висели старые метрики.** До 0.7.37 сторож агента снимал только флаг
`hasAgent`, а последние присланные GPU/CPU/VRAM оставались в памяти балансера
навсегда. Теперь при молчании агента метрики очищаются, и UI показывает прочерк
вместо замороженного значения.

Проверить, что запись действительно осиротела, а не просто остыла:

```bash
docker logs ol-stack-balancer 2>&1 | grep -E 'agent timeout|agent is silent'
```

### 1.1 У текстового (gguf) бэкенда нет температуры и частот GPU

До 0.7.37 у llama.cpp-бэкендов загрузка GPU, температура, питание и частоты всегда
были нулями, тогда как image-бэкенд на ТОЙ ЖЕ карте показывал реальные значения.
Причина: для llama.cpp метрики GPU берутся из `cppworker` (`/api/gpu`), а тот
отдаёт только объём памяти; локальный опрос nvidia-smi/NVML не выполнялся, если
cppworker вернул хоть одну карту. Теперь источники сливаются: память, температура,
частоты и загрузка — из локального опроса внутри контейнера воркера, снимок
движка остаётся запасным вариантом (см. §1.2 — почему именно так).

Фикс живёт в агенте ВНУТРИ воркера, поэтому нужен обновлённый образ cppworker:
проверить можно по строке `Metrics collected` в его логе или по полю
`gpu.temperature` в `/api/v1/backends`.

### 1.2 Агенты двух воркеров на одной машине показывают РАЗНУЮ VRAM

Симптом: в «Ресурсах бэкендов» у `cppworker` и `imageworker` на одной и той же
карте разные значения — например `1.1 GB / 8.0 GB (13.4%)` против
`1.7 GB / 8.0 GB (20.7%)`.

Причина (до 0.7.38): два агента брали память из **разных источников**.

| Бэкенд | источник VRAM | свежесть |
|---|---|---|
| `image_cpp` | `nvidia-smi`/NVML внутри контейнера воркера | живое значение |
| `llama_cpp` | `cppworker` → `/api/gpu` → `cudaMemGetInfo` | **снимок на старте контейнера** |

`cppworker` вызывает `bridge.GetGPUInfo` один раз при инициализации
(`internal/cppbackend/backend.go`) и дальше отдаёт этот снимок в `/api/gpu`
(`GetGPUDevices`/`GetGPUMetrics` читают кэш `b.gpuDevices`). Проверка подряд:
nvidia-smi менялся `1559 → 1662 → 1565 MB`, а `/api/gpu` всё это время отдавал
неизменные `used=1094, free=7097`. На второй машине эффект был ещё заметнее:
простаивающая RTX 5060 с реальными 140 MB показывалась как 1117 MB занятых.

Почему так сделано: `cudaMemGetInfo` — сериализованный CUDA-вызов, и в горячем
пути (memfit-бюджет) он блокировал `/api/models` на 8+ секунд при активном
инференсе (см. `internal/cppbackend/memfit_adapter.go`). Поэтому замер вынесен на
старт. Но как **метрика для оператора** такой снимок не годится.

Как исправлено (0.7.38): агент для llama.cpp-бэкендов дополняет данные движка
локальным опросом `nvidia-smi`/NVML — он всё равно выполняется ради температуры и
частот — и берёт из него в том числе память. Снимок движка остаётся запасным
вариантом, когда локальный опрос недоступен. Теперь оба агента на одной машине
показывают одно и то же живое значение.

Проверка после обновления образов воркеров:

```bash
# у обоих бэкендов одной машины значения должны совпадать и МЕНЯТЬСЯ со временем
for i in 1 2 3; do
  curl -s -H "X-API-Token: $TOKEN" http://localhost:18081/api/v1/backends \
    | jq -r '.backends[] | "\(.id) \(.gpu.memoryUsed)/\(.gpu.memoryTotal) MB"'
  sleep 5
done
```

### 1.3 Имена переменных, которые код не читает

В проекте встречались имена, которые **код не читает вообще**: переменная
выглядит заданной, но не влияет ни на что. Это самая частая причина «агент не
коннектится» и «бэкенд зарегистрировался как Ollama».

### agent (`cmd/agent/main.go`)

| Задано в старых файлах | Читает код | Последствие ошибки |
|---|---|---|
| `AGENT_BACKEND_TYPE` | **`BACKEND_TYPE`** | бэкенд регистрируется как **Ollama**: балансер идёт по Ollama-пути, llama.cpp-возможности (KV-типы, offload, n_ctx-потолки) не используются |
| `AGENT_CPPWORKER_URL` | **`CPPWORKER_URL`** | агент не знает адрес cppworker для метрик |
| `AGENT_NODE_LABELS` | **`NODE_LABELS`** | метки не доезжают (в UI — авто-метки хоста) |
| `AGENT_MODE` | **`GPU_MODE`** | режим GPU определяется автоматически |
| `AGENT_COLLECT_INTERVAL` / `AGENT_HEARTBEAT_INTERVAL` | **`COLLECT_INTERVAL`** / **`HEARTBEAT_INTERVAL`** | интервалы берутся из дефолтов агента |
| `AGENT_BACKEND_ID`, `AGENT_REGISTER_GPU_MODE` | — | **не читаются вовсе** |

Читаются безусловно: `BALANCER_URL`, `BALANCER_TOKEN`, `AGENT_ID`, `AGENT_PORT`,
`AGENT_PUBLIC_HOST`, `AGENT_MAX_MODELS`, `AGENT_MAX_CONCURRENT_REQUESTS`,
`AGENT_WEIGHT`, `NVML_ENABLED`, `AGENT_CPPWORKER_HOST`, `AGENT_CPPWORKER_PORT`.

### cppworker (`cmd/cppworker`, `internal/cppbackend`)

- читаются: `CPPWORKER_*`, `API_TOKEN`, `CPPWORKER_BALANCER_URL`,
  `CPPWORKER_BALANCER_TOKEN`, `CPPWORKER_ADVERTISE_HOST`;
- `CPPWORKER_REGISTER_NAME` — **ID записи бэкенда**; он же становится `AGENT_ID`
  встроенного агента (агент обязан регистрироваться под тем же ID, иначе
  балансер создаст вторую запись о том же воркере);
- ❌ `CPPWORKER_ADVERTISED_PORT` **не читается**: порт, по которому балансер
  доедет до бэкенда, задаётся через `AGENT_CPPWORKER_PORT`.

### imageworker / sdworker

- читаются: `SDWORKER_BALANCER_URL`, `SDWORKER_BALANCER_TOKEN`,
  `SDWORKER_ADVERTISE_HOST`, `SDWORKER_ADVERTISE_PORT`, `SDWORKER_BACKEND_ID`,
  `SDWORKER_REGISTER_DISABLE`, `SDWORKER_PORT`, `SDWORKER_MAX_CONCURRENT`,
  `SDWORKER_IDLE_UNLOAD_MINUTES`, `SDWORKER_IMAGE_MODELS_DIR`;
- токен берётся из `SDWORKER_BALANCER_TOKEN`, при отсутствии — из
  `BALANCER_API_TOKEN`.

### balancer

- читаются: `LB_*` (см. `.env.bundled-with-agent.example`), `CPPWORKER_API_TOKEN`,
  `CPPWORKER_URL`;
- ❌ `BALANCER_PORT` / `BALANCER_API_PORT` **не читаются**: порты берутся из
  `config/config.json` (`loadBalancer.port` / `.apiPort`) или флагами
  `-port` / `-api-port`.

### Встроенный агент (`internal/agent/embedded.go`)

Включается `AGENT_EMBEDDED=on` и живёт **в процессе воркера** (cppworker или
sdworker). Читаются: `AGENT_EMBEDDED`, `AGENT_ID`, `AGENT_PUBLIC_HOST`,
`AGENT_EMBEDDED_PORT` (или `AGENT_PORT`), `AGENT_WORKER_PORT`, `BALANCER_URL`,
`BALANCER_TOKEN`, `GPU_MODE`, `NVML_ENABLED`, `METRICS_INTERVAL` /
`COLLECT_INTERVAL`, `HEARTBEAT_INTERVAL`, `AGENT_MAX_CONCURRENT_REQUESTS`,
`AGENT_WEIGHT`, `API_TOKEN` / `CPPWORKER_API_TOKEN` / `BALANCER_API_TOKEN`.

ID по умолчанию: `SDWORKER_BACKEND_ID` (image-воркер) или
`CPPWORKER_REGISTER_NAME` (llama.cpp-воркер), иначе hostname.

**Зачем вообще встроенный агент.** Внешний контейнер `agent` регистрируется
ОТДЕЛЬНОЙ записью и переживает пересоздание воркера — отсюда «перерегистрация» в
мониторинге. Встроенный живёт ровно столько, сколько воркер, и пишет метрики в
ЕГО ЖЕ запись (`hasAgent=true`, реальный `agentPort`).

---

## 2. ⚠️ Порядок переменных: `environment:` переопределяет `env_file`

Compose сначала подставляет `env_file`, затем **накладывает** `environment`.
Поэтому строка вида

```yaml
environment:
  - LB_FOO=${LB_FOO:-}
```

**затирает** значение из `.env.bundled-with-agent` пустой строкой: интерполяция
`${...}` читает только `deployments/.env`, где таких переменных нет.

**Правило:** переключатель задаётся ЛИБО в `.env.bundled-with-agent` (и тогда не
упоминается в `environment`), ЛИБО в `environment` с дефолтом из `.env`. В
`docker-compose.stack.yml` переключатели намеренно не дублируются.

---

## 3. 🔑 Токен: ровно одно место

`CPPWORKER_API_TOKEN` в `deployments/.env` — единственный источник. Compose
выводит из него `LB_API_TOKEN`, `API_TOKEN` / `CPPWORKER_API_TOKEN` (cppworker и
agent) и `BALANCER_TOKEN`.

**Править `auth.tokens` в `config/config.json` бесполезно**: при заданном
`LB_API_TOKEN` список из файла полностью заменяется. Балансер говорит об этом в
шапке логов (`Auth tokens: LB_API_TOKEN … ЗАМЕНЯЕТ config.json`).

**Живой инцидент (R83).** В bundled-стеке бэкенд создавал агент, и только он знал
токен, который ждёт cppworker. Токен не доезжал до балансера → балансер удалял
заголовок авторизации при проксировании (правило «не отправлять чужой секрет») →
правка параметров модели из WebUI падала с `401 invalid or missing API token`,
хотя токен был задан одинаково во всех `.env`. Теперь токен едет полем
`cppWorkerApiToken` в регистрации агента.

---

## 4. 🐳 Свой репозиторий образов: `IMAGE_REGISTRY`

Все образы берут префикс из `IMAGE_REGISTRY` (`deployments/.env`):

```
IMAGE_REGISTRY=                        → ollama-legion/balancer:<тег>
IMAGE_REGISTRY=local-docker-hub:5000/  → local-docker-hub:5000/ollama-legion/balancer:<тег>
```

⚠️ **Слэш на конце обязателен.** Compose склеивает строки буквально, и без слэша
получится `local-docker-hub:5000ollama-legion/balancer` — невалидная ссылка.
Скрипты сборки (`scripts/lib-image-registry.ps1` → `Get-ImageRegistryPrefix`)
слэш добавляют сами, а значения-заглушки (`local`, `docker.io`,
`index.docker.io`) превращают в пустой префикс.

---

## 5. ⚠️ `backendEngine: ollama_api` выбрасывает все llama.cpp-бэкенды

Если в `config/config.json` балансера стоит `backendEngine: ollama_api`, то
`getEffectiveBackendType()` вернёт `ollama`, и `filterBackendsByEffectiveType()`
выбросит **все** `llama_cpp`-бэкенды из выдачи.

**Симптомы:** `GET /api/v1/metrics` → `"backends": []` при `totalBackends=1`
(монитор WebUI пуст), `GET /api/ps` → `503 no healthy backends for read endpoint`
(именно им пользуются OpenWebUI и ollama-CLI), `/api/v1/cluster/models/loaded`
пуст.

Для стека с cppworker значение обязано быть **`llama_cpp`**. При настоящей смеси
(ollama + llama.cpp) оно остаётся `llama_cpp`, а `effectiveBackendType` в
`/api/v1/cluster` становится пустым — это признак «показываем все типы», а не
ошибка.

---

## 6. ⚠️ Сценарий «только бэкенды» требует `BALANCER_URL`

`--profile worker` (бэкенды, балансер на другой машине) **молча не заработает**,
если не задан `BALANCER_URL`: воркеры пойдут на `http://loadbalancer:18081` —
имя сервиса, которого в этом профиле нет. Регистрация будет вечно повторяться с
ошибкой в логе.

Задаётся **один раз** в `deployments/.env`:

```bash
BALANCER_URL=http://192.168.1.10:18081
CPPWORKER_API_TOKEN=<тот же токен, что у балансера>
```

Значение уезжает во все три места: `CPPWORKER_BALANCER_URL` (сам cppworker),
`SDWORKER_BALANCER_URL` (image-воркер) и `BALANCER_URL` (встроенный агент).

⚠️ `BALANCER_URL` — это **ADMIN API (18081)**, не клиентский порт (18080).

**Живой инцидент.** Профиль `worker` вообще не разворачивался:
`service "imageworker" depends on undefined service "loadbalancer"` — compose
валидирует `depends_on` даже для сервисов вне активного профиля. Исправлено
`required: false`: `depends_on` задаёт только порядок запуска.

---

## 7. Порты: что публиковать наружу

| Порт | Компонент | Наружу нужен? |
|---|---|---|
| 18080 | Balancer, Ollama API | да (клиенты) |
| 18079 | Balancer, OpenAI + `/v1/images/*` | да (клиенты) |
| 18081 | Balancer, admin API | да (WebUI, регистрация бэкендов) |
| 18083 | WebUI | да (оператор) |
| 18092 | CppWorker | **в сценарии 2 — да** (балансер на другой машине), в сценарии 1 — только внутри сети |
| 18093 | ImageWorker | то же, что 18092 |
| 18034 | Встроенный агент cppworker | внутри сети |
| 18033 | Встроенный агент imageworker | внутри сети |
| 18032 | Внешний agent (Ollama) | внутри сети |
| 18094 | Движок sd-server | **нет**: слушает loopback внутри контейнера воркера |

Порты встроенных агентов разнесены специально: два процесса не должны слушать
один порт, а балансер опрашивает метрики по `agentPort` конкретной записи.

---

## 8. 🔎 Дедупликация VRAM: несколько бэкендов на одной машине

Один хост может держать несколько бэкендов (cppworker + imageworker). Оба агента
читают `nvidia-smi` **одной** карты и присылают **одни и те же**
`memoryTotal`/`memoryUsed`. Наивная сумма давала **16 ГБ на карте в 8 ГБ**.

Признак «одна физическая карта» — **UUID** (`gpu.uuids`, из
`nvidia-smi --query-gpu=uuid`): у контейнеров одного хоста совпадает побайтово.
Имя хоста для этого не годится — контейнеры регистрируются под разными именами
(`cppworker-gpu`, `imageworker`), то есть для балансера это «две машины».

Если UUID нет (не-nvidia платформа, старый агент) — дедупликация идёт по имени
хоста: один хост = одна карта.

**Важно про llama.cpp-воркеры:** их GPU-метрики приходят из cppworker (`/api/gpu`),
где UUID нет, поэтому UUID дописывается из `nvidia-smi` в `mapLlamaGPUMetricsWithUUIDs`.
Без этого дедуп работал бы только для image-воркера.

---

## 9. 🔎 Автовыгрузка простаивающих моделей

cppworker держит в VRAM **все** загруженные модели: `idleUnloadMinutes=0` означает
«не выгружать никогда». На 8 ГБ это ловушка.

**Живой инцидент.** Qwen3-Instruct-2507 (5.4 ГБ) и gemma-4-E4B остались в памяти
после своих сессий, свободного VRAM стало 391 МБ, и запрос на генерацию картинки
был отклонён гейтом: `image model needs 520 MB VRAM, only 391 MB free` (политика
`coexistence=exclusive`). Модель картинок при этом была **уже загружена** — не
хватало 130 МБ, которые занимала чужая модель.

Лечится `CPPWORKER_IDLE_UNLOAD_MINUTES=10` в `.env`. После выгрузки лишней модели
свободный VRAM вырос 210 → 1084 МБ, и генерация прошла.

⚠️ **Настройка того же параметра через WebUI/API не сохраняется**:
`saveConfigToDefaultsFile` пишет в `config/cppworker-defaults.json` относительно
рабочего каталога, а каталога `config/` в образе нет. В логе:
`failed to save config to cppworker-defaults.json: no such file or directory`.
Единственное надёжное место — переменная окружения.

---

## 10. 🎨 Qwen-Image 2.1: почему «набор почти полный» не работает

Qwen-Image 2.1 — DiT-семейство: одного diffusion-файла недостаточно, движок
грузится флагами `--diffusion-model` + `--vae` + `--llm`.

**Ошибка**, которая выглядит как «модель битая», а означает ровно одно — роль
`llm` не подключена:

```
Conditioner model tensor 'text_encoders.llm.model.embed_tokens.weight' not in model metadata
diffusion_engine.cpp: model metadata validation failed
```

Механика: файл из `--llm` подключается с префиксом
(`init_from_file(llm_path, "text_encoders.llm.")`), поэтому подходит файл, у
которого тензоры уже названы `model.embed_tokens.weight*`. MLX-варианты того же
репозитория не подходят по типу весов (`U32`).

Проверка, что набор полный — до загрузки: вкладка HuggingFace → «Паспорт»
(`GET /api/hf/plan`) показывает `engineMode` и обязательные роли.

Проверка после загрузки — что в `argv` воркера есть все три флага:

```bash
docker logs ol-stack-imageworker 2>&1 | grep -o '\-\-\(diffusion-model\|vae\|llm\) [^ ]*'
```

Подробности — [image-generation.md](../image-generation.md) §16.10.

---

## 11. 🧩 Два сборщика метрик на одном бэкенде — «мигающие» значения

Если у бэкенда одновременно работает встроенный агент (`AGENT_EMBEDDED=on`) и
внешний контейнер `agent`, они шлют метрики **по очереди**, и значения мигают —
в частности `gpu.uuids` то есть, то нет, а он нужен для дедупликации VRAM (§8).

Поэтому: для llama.cpp-воркеров — только встроенный (`full`), внешний `agent` —
для **Ollama**-бэкендов (`worker` / `legacy-agent`).

---

## 12. 🔌 Удалённая машина: чек-лист

1. `BALANCER_URL=http://<балансер>:18081` (ADMIN API) в `.env` на машине-воркере.
2. `CPPWORKER_API_TOKEN` — **тот же**, что на балансере.
3. `CPPWORKER_ADVERTISE_HOST` / `SDWORKER_ADVERTISE_HOST` — адрес воркера,
   **видимый с балансера** (не `localhost`).
4. Порт воркера (18092 / 18093) открыт для машины балансера — в сценарии 2 он
   нужен наружу, в сценарии 1 достаточен внутри сети.
5. Проверка на балансере: `curl -s localhost:18081/api/v1/backends`.
6. На воркере в логе: `registered with balancer` (и `Agent registered` для
   встроенного агента).

⚠️ Разные машины с одной картой — это **не** дубли, но совпадение физической
карты определяется по UUID (§8). Одинаковые `host` + порт с разных машин
склеиваться не должны.

---

## 13. ⚠️ Версия образа и коммит

Балансер, агент и cppworker печатают тег и коммит первой строкой шапки:
`║Version: r83-submodule-v71 (commit 82c4e60, 2026-10-07)║`.

**Версия образа про коммит НЕ говорит.** Живая ловушка: образ cppworker был собран
из коммита `82c4e60`, а нужная правка появилась в `83b12f3` — в бинаре её просто
не было (проверяется `strings /app/cppworker | grep -c query-gpu=uuid` → 0).
Перед раскаткой сверяйте коммит сборки с коммитом правки.
