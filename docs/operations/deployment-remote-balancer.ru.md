# Развёртывание бэкенда llama.cpp (cppworker + agent) и проверка распределения запросов

**Дата:** 2026-09-27 · Для версии `r83-submodule-v22`

Документ отвечает на два вопроса:
1. **куда вписать адрес балансера**, чтобы бэкенд подключился;
2. **как проверить, что запросы реально распределяются** между бэкендами.

---

## 1. Что где лежит

| Файл | Назначение |
|---|---|
| `deployments/docker-compose.cppworker-remote.yml` | Бэкенд cppworker + agent, **подключение к внешнему балансеру** |
| `deployments/.env.cppworker-remote.example` | Шаблон окружения: 3 обязательных значения + параметры железа |
| `deployments/docker-compose.fanout-test.yml` | Стенд из **двух** бэкендов с задержкой — чтобы увидеть распределение |
| `scripts/test-backend-fanout.ps1` | Отправляет N параллельных запросов и показывает, кто обслужил |

Схема подключения:

```
                 ┌───────────────────────────┐
                 │  БАЛАНСЕР (внешний)       │
                 │  18080 клиентский API     │
   клиент ──────►│  18081 ADMIN API          │◄──── регистрация + метрики (agent)
                 └─────────────┬─────────────┘
                               │ инференс: BACKEND_HOST:18092
                               ▼
                 ┌───────────────────────────┐
                 │  ЭТА МАШИНА               │
                 │  cppworker (llama.cpp)    │
                 │  agent (метрики)          │
                 └───────────────────────────┘
```

## 2. Куда вписать адрес балансера

**Одно место** — переменная окружения `BALANCER_URL` у сервиса **agent**:

```yaml
# deployments/docker-compose.cppworker-remote.yml → services.agent.environment
- BALANCER_URL=${BALANCER_URL:?задайте BALANCER_URL=http://<хост-балансера>:18081}
- BALANCER_TOKEN=${BALANCER_TOKEN:?задайте BALANCER_TOKEN}
```

Причины, по которым именно так:

- **Регистрирует именно агент, а не cppworker.** У cppworker стоит
  `CPPWORKER_REGISTER_DISABLE=true`: если включить и Go-side регистрацию, и
  агента, на один физический endpoint появится **две** записи бэкенда.
- **Порт 18081, а не 18080.** 18080 — клиентский API (туда ходят OpenWebUI/Cline),
  18081 — admin API: регистрация бэкендов, метрики, heartbeat.
- **Адрес должен быть доступен из контейнера агента.** `http://loadbalancer:18081`
  работает только внутри той же docker-сети; для балансера на другой машине
  нужен её IP/DNS, для балансера на этом же хосте — `http://host.docker.internal:18081`.

### Вторая обязательная переменная: `BACKEND_HOST`

```yaml
- AGENT_CPPWORKER_HOST=${BACKEND_HOST}   # что балансер получит как host бэкенда
- CPPWORKER_ADVERTISE_HOST=${BACKEND_HOST}
```

Это **адрес вашей машины, доступный С БАЛАНСЕРА**, а не имя compose-сервиса
(`cppworker-gpu` резолвится только внутри этой сети). Если указать неверно,
бэкенд зарегистрируется «healthy», но каждый запрос будет падать по таймауту.

### ⚠ Ловушка: имена переменных агента

Проверено по коду (`cmd/agent/main.go`), а не по документации: в разных
compose-файлах проекта исторически встречаются имена, которые код **не читает**.

| В compose может быть | Код читает | Последствие ошибки |
|---|---|---|
| `AGENT_BACKEND_TYPE` | **`BACKEND_TYPE`** | бэкенд зарегистрируется как Ollama: баннер «Engine: Ollama», llama.cpp-путь не используется |
| `AGENT_CPPWORKER_URL` | **`CPPWORKER_URL`** | агент не знает адрес cppworker |
| `AGENT_NODE_LABELS` | **`NODE_LABELS`** | метки не доедут (в UI будут авто-метки хоста) |
| `AGENT_MODE` | **`GPU_MODE`** | режим GPU определяется автоматически |
| `AGENT_BACKEND_ID` | — (не читается) | id бэкенда берётся из `AGENT_ID` |

В `docker-compose.cppworker-remote.yml` используются **правильные** имена; при
копировании блоков из других файлов проекта это стоит проверять.

## 3. Порядок запуска

```bash
cd deployments
cp .env.cppworker-remote.example .env
# заполнить: BALANCER_URL, BALANCER_TOKEN, BACKEND_HOST (+ MODELS_DIR, теги образов)
docker compose -f docker-compose.cppworker-remote.yml up -d

# проверка регистрации (на балансере):
curl -H "X-API-Token: $BALANCER_TOKEN" http://<балансер>:18081/api/v1/backends
#   → запись с host=<BACKEND_HOST>, status=healthy, hasAgent=true
```

Проверка, что балансер действительно доедет до бэкенда:

```bash
curl -H "X-API-Token: $BALANCER_TOKEN" \
  http://<балансер>:18080/api/chat \
  -H 'Content-Type: application/json' \
  -d '{"model":"<имя>","messages":[{"role":"user","content":"hi"}],"stream":false}' -D -
#   → 200 и заголовок X-Backend-Id: <id вашего бэкенда>
```

## 4. Как проверить распределение запросов

### 4.1. Почему «все запросы на один бэкенд» — это нормально

Балансер выбирает бэкенд **по максимальному score**, а не по кругу
(`internal/balancer/backend_selector.go`, `scoring.go`). При почти одинаковых
метриках побеждает один и тот же бэкенд, и переключение происходит только когда
у него `ActiveReqs ≥ prewarm.triggerLoadThreshold` (по умолчанию **0.7** от
`MaxConcurrentReqs`).

Поэтому на быстрых ответах (десятки миллисекунд) счётчик активных не успевает
вырасти, и **все** запросы уходят на один бэкенд. Это не баг распределения —
это следствие того, что запросы не конкурируют. Замерено на стенде:

| Задержка ответа | 10 параллельных | 20 параллельных |
|---|---|---|
| ~50 мс (stub без задержки) | один бэкенд: 10/10 | один: 18/20 |
| 2 с (stub с задержкой) | **7 / 3** | **18 / 2** |

Вывод: чтобы увидеть распределение, нужны **перекрывающиеся** запросы —
длинные ответы либо `n_parallel > 1` с реальной нагрузкой.

### 4.2. Стенд для проверки (два бэкенда)

```bash
# 1. образ stub-сборки с поддержкой задержки
docker build -t ollama-legion/cppworker:stub-delay -f docker/cppworker/Dockerfile.stub .

# 2. стаб-модель в общий том (скрипт-заготовка GGUF)
docker volume create fanout-models
#   положить в том файл distro-probe.gguf (см. рецепт в конце документа)

# 3. поднять два бэкенда + балансер (порт балансера 18088)
docker compose -f deployments/docker-compose.fanout-test.yml up -d

# 4. загрузить модель на ОБА бэкенда
for c in fanout-cpp-a fanout-cpp-b; do
  docker exec $c curl -s -H 'X-API-Token: fantoken' -X POST \
    -H 'Content-Type: application/json' \
    -d '{"name":"distro-probe","contextSize":4096}' \
    http://127.0.0.1:18092/api/models/load
done

# 5. проверить распределение
pwsh -File scripts/test-backend-fanout.ps1 -Container fanout-bal \
     -Token fantoken -Model distro-probe -Count 10
```

Скрипт покажет по каждому запросу `X-Backend-Id`, сводку в процентах, число
ответов с `X-Queue-Position` и нагрузку по бэкендам из admin API.

### 4.3. Проверка на проде

```powershell
pwsh -File scripts/test-backend-fanout.ps1 `
  -BalancerUrl http://127.0.0.1:18080 `
  -Token <LB_API_TOKEN> -Model <загруженная модель> -Count 10
```

Чтобы запросы перекрывались на реальном железе, просите длинный ответ
(`"options": {"num_predict": 512}`) или запускайте больше параллельных запросов.

## 5. Что делать, если распределения нет

| Симптом | Причина | Что проверить |
|---|---|---|
| Все запросы на один бэкенд, ответы быстрые | Запросы не перекрываются (см. 4.1) | Увеличить длину ответа/параллелизм — это ожидаемое поведение |
| Все запросы на один бэкенд, `active` на нём растёт до `maxConcurrent` | Порог 0.7 не включается, пока слоты свободны | `maxConcurrentRequests` у бэкенда (в `/api/v1/backends`) |
| Второй бэкенд всегда `unhealthy` | Балансер не доехал: неверный `BACKEND_HOST` или порт закрыт | `curl` с балансера на `http://<BACKEND_HOST>:18092/health` |
| Дубли бэкендов на один endpoint | Включена и Go-side регистрация, и агент | `CPPWORKER_REGISTER_DISABLE=true` |
| Бэкенд зарегистрирован как Ollama | Использован `AGENT_BACKEND_TYPE` вместо `BACKEND_TYPE` | См. таблицу в §2 |
| `X-Queue-Position` пусто при конкуренции | Слотов хватает | `AGENT_MAX_CONCURRENT_REQUESTS` / `n_parallel` |

## 6. Рецепт стаб-модели для стенда

Для стендов нужен файл, который cppworker примет как GGUF. Достаточно заголовка
и любого размера:

```python
# python3 mkgguf.py — создаёт /tmp/m/distro-probe.gguf (100 МБ)
import struct, os
p = '/tmp/m/distro-probe.gguf'
f = open(p, 'wb')
def s(k, v):
    f.write(struct.pack('<Q', len(k))); f.write(k.encode())
    f.write(struct.pack('<I', 8)); f.write(struct.pack('<Q', len(v))); f.write(v.encode())
def u32(k, v):
    f.write(struct.pack('<Q', len(k))); f.write(k.encode())
    f.write(struct.pack('<I', 4)); f.write(struct.pack('<I', v))
f.write(b'GGUF'); f.write(struct.pack('<I', 3))
f.write(struct.pack('<Q', 0)); f.write(struct.pack('<Q', 6))
s('general.architecture', 'qwen3')
u32('qwen3.block_count', 32)
u32('qwen3.attention.head_count', 24)
u32('qwen3.attention.head_count_kv', 4)
u32('qwen3.embedding_length', 4096)
u32('qwen3.context_length', 32768)
u32('qwen3.attention.key_length', 128)
f.close()
os.truncate(p, 100 * 1024 * 1024)
```
