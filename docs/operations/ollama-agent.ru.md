# Отдельный агент для Ollama-бэкенда

Дата: 2026-11-02. Сервис: `ollama-agent` в `deployments/docker-compose.stack.yml`
(профиль `ollama`), образ `ollama-legion/agent`.

## Зачем отдельный контейнер

У балансера два разных «наблюдателя»:

| Агент | Для чего | Где живёт |
| --- | --- | --- |
| встроенный в `cppworker` | метрики llama.cpp-воркера (слоты, VRAM, загрузки) | внутри контейнера воркера |
| `agent` (профиль `legacy-agent`) | observer физического cppworker'а | рядом с воркером |
| **`ollama-agent` (профиль `ollama`)** | **Ollama-бэкенд**: GPU-нагрузка хоста, модели (`/api/tags`, `/api/ps`), аргументы запуска `ollama serve` | на машине, где работает Ollama |

`ollama-agent` нужен там, где работает сама Ollama: у неё нет нашего Go-кода,
поэтому метрики снимаются снаружи — NVML для GPU и Ollama HTTP API для моделей.

## Свои параметры (ни с чем не пересекаются)

| Параметр | Значение по умолчанию | Комментарий |
| --- | --- | --- |
| `AGENT_ID` | `ollama-agent` | свой backendId; у cppworker-агента другой (`cppworker-gpu-bundled-agent`) |
| `AGENT_PORT` / `AGENT_PUBLIC_PORT` | `18036` | встроенный cppworker — 18034, legacy `agent` — 18032 |
| `AGENT_PUBLIC_HOST` | `ollama-agent` | адрес для health-проверки самого агента |
| `BACKEND_TYPE` | `ollama` | режим регистрации |
| `OLLAMA_URL` | `http://host.docker.internal:11434` | **адрес Ollama**; R392: именно его хост уходит балансеру как адрес бэкенда |
| `NODE_LABELS` | `gpu,ollama` | метки узла |
| `AGENT_MAX_CONCURRENT_REQUESTS` | `0` = авто | операторский лимит вместимости |
| `AGENT_WEIGHT` | `1` | вес при балансировке |
| `COLLECT_INTERVAL` / `HEARTBEAT_INTERVAL` | `15` / `30` с | частоты |
| `BALANCER_URL` | `http://loadbalancer:18081` | admin API балансера |
| `BALANCER_TOKEN` / `API_TOKEN` | `CPPWORKER_API_TOKEN` | один токен стека |

### Важно про адрес Ollama (R392)

До R392 для `BACKEND_TYPE=ollama` агент регистрировал бэкенд на **свой** адрес
(`AGENT_PUBLIC_HOST`), то есть балансер получал «Ollama на хосте агента» и не
мог до неё достучаться. Теперь хост берётся из `OLLAMA_URL`.

Правила:

- `OLLAMA_URL=http://192.168.13.5:11434` → балансер увидит бэкенд
  `192.168.13.5:11434` — адрес должен быть доступен **с балансера**;
- `localhost` / `127.0.0.1` внутри контейнера указывают на сам контейнер
  агента, поэтому для них используется `AGENT_PUBLIC_HOST` (задайте его как
  адрес, видимый с балансера);
- для Ollama на том же хосте, что и агент, но в другой сети — задайте
  `host.docker.internal` (Docker Desktop) или LAN-адрес.

## Запуск

На машине, где работает Ollama (нужен доступ к её API и к GPU):

```
OLLAMA_URL=http://<адрес-ollama>:11434 \
BALANCER_URL=http://<адрес-балансера>:18081 \
docker compose -f docker-compose.stack.yml --profile ollama up -d ollama-agent
```

или в `deployments/.env`:

```
OLLAMA_URL=http://192.168.13.5:11434
BALANCER_URL=http://192.168.13.12:18081   # если балансер на другой машине
```

Контейнеру нужны: `runtime: nvidia` (NVML → GPU/VRAM хоста) и `pid: host`
(чтобы `ps` видел процесс `ollama serve` и его аргументы: `num_ctx`, `num_gpu`,
`kv-cache`). Без `pid: host` runtime-флаги Ollama будут `default`.

## Что агент передаёт балансеру

- регистрация: `backendId`, `host` (из `OLLAMA_URL`), `ollamaPort`, `agentPort`,
  `backendType=ollama`, `nodeLabels`, вместимость, токен воркера;
- heartbeat (30 с): GPU (использование, VRAM), система (CPU/RAM/диск),
  Ollama: версия, `AvailableModels` из `/api/tags`, `RunningModels` из `/api/ps`
  (включая `size_vram`), runtime-флаги (из аргументов процесса или `OLLAMA_*`);
- `OllamaAvailable` — доступна ли Ollama как API.

По этим метрикам балансер считает загрузку узла (resource-aware) и видит, какие
модели реально загружены в память Ollama.

## Как проверить

```
# 1. Регистрация: запись типа ollama с адресом из OLLAMA_URL
curl -s -H "X-API-Token: $TOKEN" http://<балансер>:18081/api/v1/backends | jq '.backends[] | select(.id=="ollama-agent")'

# 2. Метрики пришли (GPU, модели)
docker logs ol-stack-ollama-agent | tail -20
#   "Registering ollama backend at 192.168.13.5:11434 (agent at ollama-agent)"
```

В WebUI: «Бэкенды» → запись `ollama-agent` (тип Ollama, здоровье, GPU-нагрузка),
«Монитор» → загруженные модели Ollama.

Типовые ошибки:

- **бэкенд unhealthy, в логах «failed to connect to Ollama»** — `OLLAMA_URL`
  указывает не туда или Ollama недоступна с машины агента;
- **балансер не видит бэкенд** — проверьте `BALANCER_URL`/токен; агент должен
  видеть admin API `:18081`;
- **GPU-метрики нулевые** — контейнер запущен без `--gpus`/`runtime: nvidia`
  или без `NVML_ENABLED=true`;
- **runtime-флаги = default** — нет `pid: host` (или `ollama serve` запущена
  не на этом хосте).
