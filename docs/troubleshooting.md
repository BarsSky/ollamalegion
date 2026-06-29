# Troubleshooting OllamaLegion

> **Версия:** 3.1 (2026-06-29) — добавлена §10 «Live metrics = 0 для cppworker backend» + ссылка на [`cppworker-metrics-collection.md`](cppworker-metrics-collection.md)  
> **Связанные документы:** [`runbook-tools.md`](runbook-tools.md) — детальный runbook для tools/tool_calls, [`cppworker-model-params.md`](cppworker-model-params.md) — n_ctx/Per-Model Profiles, [`cppworker-metrics-collection.md`](cppworker-metrics-collection.md) — архитектура метрик, [`audit-2026-06.md`](audit-2026-06.md), [`../.clinerules`](../.clinerules)

## Содержание

1. [Частые ошибки балансировщика](#1-частые-ошибки-балансировщика)
2. [n_ctx overflow: диагностика и решение](#2-n_ctx-overflow-диагностика-и-решение)
3. [ReloadLoopLimitError (HTTP 413)](#3-reloadlooplimit-error-http-413)
4. [Бэкенд помечается как unhealthy](#4-бэкенд-помечается-как-unhealthy)
5. [HTTP 5xx / "Server disconnected" на тяжёлых моделях](#5-http-5xx--server-disconnected-на-тяжёлых-моделях)
6. [Tools/tool_calls — сброс или пустой ответ](#6-toolstool_calls--сброс-или-пустой-ответ)
7. [TransferEncodingError при streaming](#7-transferencodingerror-при-streaming)
8. [Проблемы с NVIDIA Container Toolkit](#8-проблемы-с-nvidia-container-toolkit)
9. [Agent → Ollama: ограничения](#9-agent--ollama-ограничения)
10. [Live metrics = 0 для cppworker backend](#10-live-metrics--0-для-cppworker-backend)
11. [Логирование и отладка](#11-логирование-и-отладка)
12. [Связанные документы](#12-связанные-документы)

---

## 1. Частые ошибки балансировщика

### 1.1 Агент не подключается к балансировщику

**Симптомы:**
- Агент не появляется в списке бэкендов.
- В логах агента — connection refused / timeout.

**Решение:**

```bash
# 1. Проверить доступность балансировщика
curl -v http://<balancer-ip>:18081/api/v1/health

# 2. Проверить firewall
sudo ufw status  # Linux
netsh advfirewall show allprofiles  # Windows

# 3. Логи агента
docker logs deployments-agent-1

# 4. Проверить BALANCER_URL внутри контейнера
docker exec deployments-agent-1 sh -c 'echo $BALANCER_URL'
```

**Возможные причины:**
- Неправильный URL.
- Firewall блокирует 18081.
- Балансировщик не запущен.
- Docker network между контейнерами (для cppworker auto-registration).

### 1.2 Бэкенд помечается как unhealthy

**Симптомы:** `b.status === 'unhealthy'` или `'offline'` в `/api/v1/backends`.

**Решение:**

```bash
# 1. Проверить Ollama напрямую
curl http://<ollama-host>:11434/api/tags

# 2. Проверить agent (если есть)
curl http://<agent-host>:18032/health

# 3. Проверить network
ping <ollama-host>
telnet <ollama-host> 11434
```

**Подробнее:** [`agent-deployment.md`](agent-deployment.md) раздел Troubleshooting.

### 1.3 503 "model is loading"

**Симптом:** сразу после первого запроса с большой моделью приходит 503.

**Причина:** cppworker блокируется на `LoadModel` (30-60 сек), следующий запрос видит `errModelIsLoading`.

**Решение:**
- Дождаться окончания загрузки (cppworker вернёт `loading: true, retryAfterMs: 3000`).
- Или увеличить `Balancing.RequestTimeout` до 300-600 сек (см. [`configuration.md`](configuration.md)).

---

## 2. n_ctx overflow: диагностика и решение

### 2.1 Симптомы

```
"requested n_ctx=128000 exceeds model's effective n_ctx=4096"
```

или

```
HTTP 400 / 413: n_ctx too large for current load
```

### 2.2 Полный поток проблемы

```
Cline → {"model":"gemma-4-E4B-...","options":{"num_ctx":128000}}
  ↓
Балансировщик:
  1. ExtractNumCtxFromBody(body) = 128000
  2. ResolveNumCtx(model, body, backendID):
     - Tier 1 (body): 128000
     - Tier 2 (maxNumCtxForModel):
       ├─ GetModelProfileNumCtx() = 0 → skip
       ├─ GetDefaultModelProfileNumCtx() = ??? ← ⚠️ Если 4096 — clamping!
       └─ getModelLoadedCtxFromMetrics() — не достигается
     - Результат: clamped 128000 → 4096
  3. ApplyCppCtxHeader → X-Cpp-Ctx: 4096
  ↓
CppWorker:
  1. buildGenerationParams: NCtxOverride = 128000 (из body)
  2. applyCppCtxHeader: body 128000 > headerLimit 4096 → CLAMP → 4096
  3. Inference с n_ctx = 4096
  4. Prompt > 4096 → bridge code 2 (ErrNCtxNeedsReload)
  ↓
Балансировщик:
  1. ParseCppWorkerError → NCtxError
  2. handleNCtxReload → DecideReloadBackend:
     - AutoReloadNCtx = false → DecisionNoOp
  3. HTTP 413/400 клиенту
```

### 2.3 Диагностика по шагам

```bash
# 1.1 Проверить defaultModelProfile.contextLength в конфиге
curl -s http://localhost:18081/api/v1/config | jq '.defaultModelProfile.contextLength'
# Должно быть: 0 или null
# Если 4096 — конфиг не обновлён!

# 1.2 Проверить RAM fallback на cppworker
docker exec deployments-cppworker-gpu-1 sh -c 'echo $CPPWORKER_RAM_FALLBACK_N_CTX'
# Должно быть: "true"

# 1.3 Проверить n_ctx auto-reload на балансировщике
curl -s http://localhost:18081/api/v1/config | jq '.balancing.nctxReload'
# Должен быть объект с autoReloadNCtx: true

# 1.4 Прямой тест генерации с большим num_ctx
curl -X POST http://localhost:18081/api/generate \
  -H 'Content-Type: application/json' \
  -H 'X-API-Token: <token>' \
  -d '{"model":"<model.gguf>","prompt":"Hello","options":{"num_ctx":32000}}'
```

### 2.4 Решение

#### Вариант A: Per-Model Profile

```bash
# Создать профиль для конкретной модели
curl -X PUT http://localhost:18081/api/v1/cppworker/model-profiles/gemma-4-E4B-it-Q4_K_M \
  -H "Content-Type: application/json" \
  -d '{"contextLength": 32768, "batchSize": 1024, "numGpuLayers": -1}'

# Применить (reload на всех бэкендах)
curl -X POST http://localhost:18081/api/v1/cppworker/model-profiles/gemma-4-E4B-it-Q4_K_M/apply
```

Подробнее: [`cppworker-model-params.md` §3-4](cppworker-model-params.md).

#### Вариант B: Обновить `config.json`

Убрать `defaultModelProfile.contextLength` (или установить в 0):

```diff
- "defaultModelProfile": {
-   "contextLength": 4096,
-   ...
- }
+ "defaultModelProfile": {
+   "contextLength": 0,
+   ...
+ }
```

#### Вариант C: Включить RAM fallback + auto-reload

```env
# .env.bundled
CPPWORKER_RAM_FALLBACK_N_CTX=true
CPPWORKER_RAM_FALLBACK_MAX_N_CTX=128000
LB_NCTX_RELOAD_ENABLED=true
LB_NCTX_RELOAD_MAX_N_CTX=131072
LB_NCTX_RELOAD_VRAM_SAFETY_FACTOR=0.85
```

---

## 3. ReloadLoopLimitError (HTTP 413)

### 3.1 Симптомы

```
"reload attempts exceeded maximum of 3 per 60 sec window"
```

или в логах cppworker:
```
RAM fallback: cycle limit reached, refusing reload
```

### 3.2 Причина

cppworker попытался сделать reload 3 раза за 60 сек, но каждый раз получал ту же ошибку (n_ctx overflow). Защита от бесконечного reload loop.

### 3.3 Сброс счётчика (R-6)

**Сейчас:** единственный способ — `docker restart deployments-cppworker-gpu-1`.

**После R-6** (в работе):

```bash
# Балансировщик (counter на стороне балансировщика):
curl -X POST http://localhost:18081/api/v1/nctx-reload/cppworker-gpu-1/reset

# CppWorker (counter на стороне cppworker):
curl -X POST http://localhost:18081/api/v1/cppworker/reset-reload-counter
```

### 3.4 Перед сбросом

1. Устранить причину (например, уменьшить `num_ctx` в запросе, увеличить VRAM, выгрузить другие модели).
2. Проверить `max_vram_n_ctx` в логах.
3. Перезапустить только после устранения.

---

## 4. Бэкенд помечается как unhealthy

### 4.1 Диагностика

```bash
# Статус всех бэкендов
curl -H 'X-API-Token: <token>' http://localhost:18081/api/v1/backends | jq

# Конкретный бэкенд (по ID)
curl -H 'X-API-Token: <token>' http://localhost:18081/api/v1/backends/cppworker-gpu-1
```

### 4.2 Причины и решения

| Причина | Симптом | Решение |
|---|---|---|
| Ollama не запущен | curl 11434 → connection refused | `systemctl restart ollama` |
| Firewall | timeout | открыть порт |
| Неправильный host/port | 503 на health | исправить в WebUI → Backends → Edit |
| Backend перегружен | status=healthy, но ответы 503 | уменьшить `MaxConcurrentRequests` |
| Агент не регистрируется | `b.hasAgent === false` | проверить `BALANCER_URL` |

---

## 5. HTTP 5xx / "Server disconnected" на тяжёлых моделях

### 5.1 Причина

`Balancing.RequestTimeout` (default 30 сек) бьёт раньше, чем `clampNPredictToFitContext` отрабатывает длинную генерацию.

### 5.2 Решение

```json
// config/config.json
{
  "balancing": {
    "requestTimeout": 600   // 10 минут
  }
}
```

Или через ENV: `LB_REQUEST_TIMEOUT=600`.

Также можно увеличить `CPPWORKER_WRITE_TIMEOUT`:

```env
CPPWORKER_WRITE_TIMEOUT=1800  # 30 минут
```

---

## 6. Tools/tool_calls — сброс или пустой ответ

**Это самая частая проблема при работе с OpenWebUI / Cline / Roo Code.** Подробный пошаговый runbook в [`runbook-tools.md`](runbook-tools.md).

### 6.1 Краткая сводка по сценариям

| Симптом | Сценарий | Где искать |
|---|---|---|
| HTTP 413 на первом запросе с tools | **A** — RAM fallback disabled для tools | `cmd/cppworker/inference.go:491` |
| Первый запрос OK, второй пустой NDJSON | **B** — clamping n_predict с has_tools=true | `internal/balancer/nctx_clamp.go` |
| Бесконечный reload loop, 503 | **C** — `ReloadLoopLimitError` | `cmd/cppworker/inference.go` |
| Tool_call в content, но `tool_calls=null` | **D** — Hermes/Mistral detection | `internal/balancer/llamacpp_toolcall_detector.go` |
| HTTP 5xx / "reset by peer" | **E** — таймауты WriteTimeout/RequestTimeout | см. §5 |

### 6.2 Acceptance criteria (перед дебагом)

1. `CPPWORKER_VERBOSE=true` на cppworker.
2. `data/state.json` (балансер) — список бэкендов и их статусы.
3. Логи балансировщика: `parsed request`, `[BALANCER → BACKEND]`, `heartbeat write failed`.
4. Логи cppworker: `clamping n_predict`, `RAM fallback`, `bridge code N`.
5. Базовые тесты транспорта:
   ```bash
   go test ./tests -run "TestDebugOpenWebUI_ToolCalls_Scenario" -tags llama_stub -count=1 -v
   ```
6. Inference-тесты:
   ```bash
   go test ./cmd/cppworker -run "TestApplyCppCtxHeader|TestClampNPredict|TestReloadDisabledForTools|TestParseToolCalls" -tags llama_stub -count=1 -v
   ```

---

## 7. TransferEncodingError при streaming

### 7.1 Симптомы

```
Error: write tcp: ..."write: broken pipe"
или
TransferEncodingError: chunked transfer encoding failed
```

### 7.2 Причина (уже исправлена в 2026-06-08)

Раньше: cppworker возвращал JSON-ошибку для streaming-запроса, балансировщик пытался её проксировать как NDJSON, но Go добавлял `Transfer-Encoding: chunked`, что ломало клиент.

### 7.3 Текущее поведение

`internal/balancer/llamacpp_transport.go:proxyRequestLlamaCpp` проверяет `Content-Type` ответа upstream:
- Если `application/json` (ошибка) → проксировать как JSON, **не** входить в SSE-ридер.
- Если `text/event-stream` → нормальный streaming.

`resp.Header.Del("Transfer-Encoding")` удалён — Go управляет этим автоматически.

### 7.4 Если ошибка всё есть

Проверить `Content-Type` от cppworker:
```bash
curl -v -X POST http://localhost:18092/api/chat -H "Content-Type: application/json" -d '{"model":"x","stream":true}' 2>&1 | grep -E "^< Content-Type"
```

Должно быть `text/event-stream` или `application/x-ndjson`, **не** `text/plain` или отсутствовать.

---

## 8. Проблемы с NVIDIA Container Toolkit

### 8.1 GPU не виден из контейнера

```bash
# Проверка на хосте
nvidia-smi

# Проверка внутри контейнера
docker run --rm --gpus all nvidia/cuda:12.2.0-base-ubuntu22.04 nvidia-smi
```

Если вторая команда fails → не установлен NVIDIA Container Toolkit.

**Установка:** см. https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html

### 8.2 Windows: GPU не пробрасывается

1. Docker Desktop → Settings → General → ✅ Use WSL 2.
2. Resources → WSL Integration → ✅ Enable integration.
3. Установить NVIDIA Driver для WSL2: https://developer.nvidia.com/cuda/wsl.

Подробнее: [`agent-deployment.md` §Windows](agent-deployment.md).

---

## 9. Agent → Ollama: ограничения

> **Это архитектурное ограничение Ollama, не баг OllamaLegion.**

Ollama **не имеет публичного REST API** для изменения runtime config. Агент может:
- ✅ Читать метрики (`/api/ps`, `/api/tags`, `/api/show`).
- ✅ Отправлять метрики балансировщику.
- ✅ Применять лимиты через heartbeat.
- ❌ Менять `numGPU`, `num_parallel`, `num_threads` Ollama напрямую.

Единственные способы изменить Ollama config:
- Переменные окружения при старте Ollama (`OLLAMA_NUM_PARALLEL`, `OLLAMA_MAX_LOADED_MODELS`).
- Файл `~/.ollama/config.json`.
- Аргументы CLI при запуске `ollama serve`.

**Что НЕ реализовано (и не планируется):** механизм Agent → Ollama config через heartbeat.

---

## 10. Live metrics = 0 для cppworker backend

> **Это ожидаемое поведение poller'а, не баг.** Подробная архитектура: [`cppworker-metrics-collection.md`](cppworker-metrics-collection.md).

**Симптом:** на вкладках Dashboard / Backends / Models / GGUF Models у `cppworker-gpu` бэкенда видно `GPU Usage: 0%`, `VRAM Used: 0 MB`, `CPU: 0%`, `RAM: 0 MB`. Список загруженных моделей (LoadedModels) **может** при этом заполняться.

**Корневая причина:** poller в балансировщике (`internal/balancer/llamacpp_metrics_poller.go`) собирает **только** `GET /api/models` и `GET /api/models/load/progress`. Он не имеет NVML-доступа к GPU cppworker'а, не снимает CPU/RAM с хоста cppworker'а и не публикует `requests_per_second`. Для live-метрик нужен **agent sidecar**.

**Быстрая диагностика:**

```bash
# 1. Проверить, есть ли agent у бэкенда
curl -H "X-API-Token: $LB_TOKEN" http://localhost:18081/api/v1/backends | \
  jq '.backends[] | select(.id | contains("cppworker")) | {id, type, hasAgent}'
# Если hasAgent=false — agent не запущен, см. ниже.

# 2. Если hasAgent=true, но метрики 0:
docker exec <agent-container> nvidia-smi
# Если "command not found" — образ собран без nvidia-container-toolkit.
```

**Решение:** развернуть agent рядом с cppworker:

| Сценарий | Compose |
|---|---|
| Sidecar к существующему балансеру | `deployments/docker-compose.cppworker-with-agent.yml` |
| Standalone (всё-в-одном) | `deployments/docker-compose.cppworker-with-agent.standalone.yml` |

Подробнее про архитектуру poller vs agent, legacy `BackendType=""` и все сценарии troubleshooting — в [`cppworker-metrics-collection.md`](cppworker-metrics-collection.md).

**NB:** poller **не** трогать (`llamacpp_metrics_poller.go` — read-only). Это намеренное разделение ответственности: poller для моделей, agent для железа.

---

## 11. Логирование и отладка

### 11.1 Уровни логов

| ENV | Уровень |
|---|---|
| `LOG_LEVEL=debug` | максимальный |
| `LOG_LEVEL=info` (default) | базовый |
| `LOG_LEVEL=warn` | только warnings |
| `LOG_LEVEL=error` | только ошибки |

### 11.2 Формат

| ENV | Формат |
|---|---|
| `LOG_FORMAT=json` (default) | структурированный JSON |
| `LOG_FORMAT=text` | человекочитаемый |

### 11.3 Просмотр логов

```bash
# Балансировщик
docker logs -f deployments-loadbalancer-1

# CppWorker с verbose
docker logs -f deployments-cppworker-gpu-1 2>&1 | grep -E "clamping|RAM fallback|bridge code|tool_calls"

# PowerShell
docker logs -f deployments-loadbalancer-1 2>&1 | Select-String "tools|chat_id|stream|/v1/chat"
```

### 11.4 NVML отладка (Linux)

```bash
# Проверить nvidia-smi в агенте
docker exec deployments-agent-gpu-1 nvidia-smi

# Включить NVML verbose
docker exec deployments-agent-gpu-1 sh -c 'NVML_DEBUG=1 ./agent'
```

### 11.5 Полезные ENV для дебага

```bash
# Балансировщик
LOG_LEVEL=debug LOG_FORMAT=text
BALANCING_DEBUG=true  # не существует, для примера

# CppWorker
CPPWORKER_VERBOSE=true
```

---

## 12. Связанные документы

- [`runbook-tools.md`](runbook-tools.md) — **детальный пошаговый runbook для tools/tool_calls** (сценарии A-E).
- [`cppworker-model-params.md`](cppworker-model-params.md) — n_ctx + Per-Model Profiles.
- [`cppworker-metrics-collection.md`](cppworker-metrics-collection.md) — **архитектура сбора метрик cppworker: poller vs agent**.
- [`agent-deployment.md`](agent-deployment.md) — развёртывание агента.
- [`installation.md`](installation.md) — установка.
- [`audit-2026-06.md`](audit-2026-06.md) — статус реализации.
- [`../.clinerules`](../.clinerules) §14 — частые ловушки.
