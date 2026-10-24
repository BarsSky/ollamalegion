# Диагностика n_ctx в OllamaLegion

> Дата: 2026-06-17  
> Статус: ✅ Актуально  
> Описание: Пошаговая диагностика проблем с n_ctx (контекстным окном) при инференсе

---

## 📋 Содержание

1. [Быстрая диагностика](#1-быстрая-диагностика)
2. [Архитектура n_ctx resolver](#2-архитектура-n_ctx-resolver)
3. [Типы ошибок n_ctx](#3-типы-ошибок-n_ctx)
4. [Пошаговое решение](#4-пошаговое-решение)
5. [Проверка после фикса](#5-проверка-после-фикса)
6. [Приложение: конфигурация](#6-приложение-конфигурация)

---

## 1. Быстрая диагностика

### Симптом: клиент (Cline/Roo/OpenWebUI) получает ошибку

```
"requested n_ctx=128000 exceeds model's effective n_ctx=4096"
```

### Действия по порядку

```bash
# 1.1 Проверить defaultModelProfile.contextLength в конфиге балансировщика
curl -s http://localhost:18081/api/v1/config | jq '.defaultModelProfile.contextLength'
# Должно быть: 0 или null
# Если 4096 — конфиг не обновлён!

# 1.2 Проверить RAM fallback на cppworker
docker exec ollamalegion-cppworker-gpu-1 sh -c 'echo $CPPWORKER_RAM_FALLBACK_N_CTX'
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

---

## 2. Архитектура n_ctx resolver

### 3-tier resolver на балансировщике

```
ResolveNumCtx(model, body, backendID)
  │
  ├─ Tier 1: ExtractNumCtxFromBody(body)
  │   └─ options.num_ctx (Ollama) или num_ctx (OpenAI)
  │
  ├─ Tier 2: maxNumCtxForModel(model, backendID)
  │   ├─ per-model profile (LlamaCppModelProfiles[name].ContextLength)
  │   ├─ defaultModelProfile.contextLength ⚡ ← **было 4096, теперь 0**
  │   └─ loaded model metrics (ContextLength из llamaMetrics)
  │
  └─ Результат: clamped к потолку из Tier 2
```

### X-Cpp-Ctx header на cppworker

```
ApplyCppCtxHeader(r, params):
  1. Читает X-Cpp-Ctx header = headerLimit
  2. Если params.NCtxOverride <= 0:
     params.NCtxOverride = headerLimit
  3. Если params.NCtxOverride > headerLimit:
     params.NCtxOverride = headerLimit (UPPER LIMIT) ⚡
  4. NPredict = NCtxOverride / 2 (если NPredict == default)
```

### Что было изменено (Phase D.3-fix)

| Компонент | Было | Стало | Файл |
|-----------|------|-------|------|
| `defaultModelProfile.contextLength` | `4096` | `0` | `config/config.json` |
| `CPPWORKER_RAM_FALLBACK_N_CTX` | не задан / `false` | `true` | `.env.bundled` |
| `AutoReloadNCtx` | `false` | `true` | `config/config.json` → `balancing.nctxReload` |
| `CPPWORKER_RAM_FALLBACK_MAX_N_CTX` | не задан | `128000` | `.env.bundled` |
| `LB_NCTX_RELOAD_ENABLED` | не задан | `true` | `.env.bundled` |

---

## 3. Типы ошибок n_ctx

### Ошибка bridge code 2: ErrNCtxNeedsReload

```json
{
  "code": 2,
  "message": "n_ctx needs reload",
  "current_n_ctx": 4096,
  "required_n_ctx": 128000,
  "max_vram_n_ctx": 4096
}
```

**Причина:** Prompt длиннее текущего n_ctx. Модель нужно перезагрузить с большим n_ctx.

**Решение:** Зависит от max_vram_n_ctx:
- Если `max_vram_n_ctx >= required_n_ctx × 1.15` — auto-reload перезагрузит модель
- Если `max_vram_n_ctx < required_n_ctx` — сработает RAM fallback (если включён)
- Если RAM fallback выключен — клиент получит 413/400

### Ошибка bridge code 3: ErrPromptTooLong

```json
{
  "code": 3,
  "message": "prompt too long for context",
  "prompt_tokens": 15000,
  "max_n_ctx": 4096
}
```

**Причина:** Prompt безнадёжно превышает лимиты даже после перезагрузки. Нужно уменьшить prompt.

**Решение:**
- Уменьшить prompt (не передавать историю чата целиком)
- Переключиться на модель с большим native context
- Использовать ранний truncation на стороне клиента

### Ошибка bridge code 4: GPU OOM

```json
{
  "code": 4,
  "message": "GPU out of memory",
  "current_n_ctx": 128000,
  "available_vram_mb": 1024,
  "required_vram_mb": 8192
}
```

**Причина:** Модель с большим n_ctx не влезает в VRAM.

**Решение:**
1. RAM fallback (`CPPWORKER_RAM_FALLBACK_N_CTX=true`) — перезагрузит модель в RAM
2. Уменьшить `gpu_layers` (`CPPWORKER_RAM_FALLBACK_GPU_LAYERS=0` для CPU-only)
3. Уменьшить `n_ctx` до разумного значения (например, 32000 вместо 128000)

---

## 4. Пошаговое решение

### Шаг 1: Обновить конфиг балансировщика

```json
// config/config.json
{
  "defaultModelProfile": {
    "contextLength": 0,
    "contextLength": 0, " — Phase D.3-fix fallback for clamping
  }
}
```

Или через API:
```bash
curl -X PUT http://localhost:18081/api/v1/config \
  -H 'Content-Type: application/json' \
  -H 'X-API-Token: <token>' \
  -d '{"defaultModelProfile":{"contextLength":0}}'
```

### Шаг 2: Включить RAM fallback на cppworker

```env
# .env.bundled
CPPWORKER_RAM_FALLBACK_N_CTX=true
CPPWORKER_RAM_FALLBACK_MAX_N_CTX=128000
CPPWORKER_RAM_FALLBACK_GPU_LAYERS=0
```

Проверить:
```bash
docker exec ollamalegion-cppworker-gpu-1 sh -c 'echo $CPPWORKER_RAM_FALLBACK_N_CTX'
```

### Шаг 3: Включить n_ctx auto-reload на балансировщике

```env
# .env.bundled
LB_NCTX_RELOAD_ENABLED=true
LB_NCTX_RELOAD_MAX_N_CTX=131072
LB_NCTX_RELOAD_VRAM_SAFETY_FACTOR=0.85
LB_NCTX_RELOAD_TIMEOUT_SEC=120
```

Или через API:
```bash
curl -X PUT http://localhost:18081/api/v1/config \
  -H 'Content-Type: application/json' \
  -H 'X-API-Token: <token>' \
  -d '{"balancing":{"nctxReload":{"autoReloadNCtx":true,"maxNCtx":131072,"vramSafetyFactor":0.85,"reloadTimeoutSec":120}}}'
```

### Шаг 4: Проверить GPU detection на cppworker

```bash
# Логи cppworker — ищем строки с VRAM/n_ctx
docker logs ollamalegion-cppworker-gpu-1 2>&1 | grep -iE "vram|n_ctx|cuda|gpu"
```

Если GPU не детектится:
```bash
# Проверить nvidia-smi внутри контейнера
docker exec ollamalegion-cppworker-gpu-1 nvidia-smi
```

### Шаг 5: Перезапустить стек

```bash
cd deployments
docker compose -f docker-compose.cppworker-bundled.yml --env-file .env.bundled down
docker compose -f docker-compose.cppworker-bundled.yml --env-file .env.bundled up -d --build
```

### Шаг 6: Проверить регистрацию и health

```bash
# Проверить backends
curl -H 'X-API-Token: <token>' http://localhost:18081/api/v1/backends | jq '.[].backend_id'

# Проверить health
curl http://localhost:18081/api/v1/ping
```

---

## 5. Проверка после фикса

### Тест 1: Запрос с большим num_ctx

```bash
curl -X POST http://localhost:18081/api/generate \
  -H 'Content-Type: application/json' \
  -H 'X-API-Token: <token>' \
  -d '{"model":"<model.gguf>","prompt":"Hello","options":{"num_ctx":32000}}'
```

**Ожидаемый результат:** успешный ответ, поле `context_size` в метаданных > 4096.

### Тест 2: Проверка логов на reload

```bash
# Логи балансировщика — ищем reload
docker logs ollamalegion-loadbalancer-1 2>&1 | grep -i "reload" | tail -20
```

**Ожидаемые строки:**
```
nctx_reload: reloading model <model> with n_ctx=32000
nctx_reload: backend <id> reloaded successfully
```

### Тест 3: Проверка RAM fallback

Для модели, которая не влезает в VRAM с большим n_ctx:

```bash
# Запрос с очень большим num_ctx
curl -X POST http://localhost:18081/api/generate \
  -H 'Content-Type: application/json' \
  -H 'X-API-Token: <token>' \
  -d '{"model":"<model.gguf>","prompt":"Hello","options":{"num_ctx":96000}}'
```

**Логи cppworker:**
```
ram_fallback: n_ctx reload with use_mmap=true, gpu_layers=0
ram_fallback: model <model> reloaded successfully in RAM
```

---

## 6. Приложение: конфигурация

### Полный набор переменных для `.env.bundled`

```env
# RAM fallback для n_ctx
CPPWORKER_RAM_FALLBACK_N_CTX=true
CPPWORKER_RAM_FALLBACK_MAX_N_CTX=128000
CPPWORKER_RAM_FALLBACK_GPU_LAYERS=0

# n_ctx auto-reload на балансировщике
LB_NCTX_RELOAD_ENABLED=true
LB_NCTX_RELOAD_MAX_N_CTX=131072
LB_NCTX_RELOAD_VRAM_SAFETY_FACTOR=0.85
LB_NCTX_RELOAD_TIMEOUT_SEC=120

# Общие
BALANCER_URL=http://loadbalancer:18081
CPPWORKER_API_TOKEN=<token>
```

### Полная секция `balancing.nctxReload` в `config.json`

```json
{
  "balancing": {
    "nctxReload": {
      "autoReloadNCtx": true,
      "maxNCtx": 131072,
      "vramSafetyFactor": 0.85,
      "reloadTimeoutSec": 120,
      "rejectOnFail": false
    }
  }
}
```

### Проверка через API

```bash
# Получить весь конфиг
curl -s http://localhost:18081/api/v1/config | jq '{
  contextLength: .defaultModelProfile.contextLength,
  nctxReload: .balancing.nctxReload,
  streamTimeout: .balancing.streamTimeout
}'
```
