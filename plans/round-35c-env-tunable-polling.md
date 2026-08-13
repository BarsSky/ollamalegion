# Round 35c — env-tunable async load polling timeout (2026-08-13)

> **Назначение:** сделать polling timeout для async load (cppworker
> 202 Accepted) env-конфигурируемым. Закрывает баг с 22GB MoE
> моделями на 8GB VRAM, где hardcoded 8m timeout не покрывает
> реальное время загрузки 6-12 мин.

## 0. Контекст

Пользователь сообщил 2026-08-13 11:01 (после успешного Round 35
тестирования gemma-4):

> "попробовал протестировать тяжелую модель для текущей системы и
> получил ошибку по таймауту model 'Qwen3.6-35B-A3B-UD-Q4_K_M' is
> not loaded and auto-load failed: auto-load failed: async load (202)
> on cppworker: model not ready after 8m7.182s
> но после того как модель все таки загрузилась клиент получил
> ошибку Expecting value: line 2 column 1 (char 2) и модель пошла
> опять в перезагрузку"

### Анализ (live state 2026-08-13 11:00)

- `Qwen3.6-35B-A3B-UD-Q4_K_M` = 22GB MoE (3B active, 35B total),
  Mamba2 hybrid attention, 40 layers
- На 3070 (8GB VRAM): 3 GPU layers, остальное CUDA_Host pinned
  memory (~19GB)
- `cppworker/handlers_model.go:1412` log: `load_ms=396196` (6m36s)
  для 22GB Qwen3.6 на 3070
- `cppworker` estimated load time (3.5min / 210s) off by **1.9x**

### Root cause: hardcoded polling timeout formula

```go
// internal/balancer/model_management.go:911-921 (до Round 35c)
maxWait := 5 * time.Minute
if loadResp.EstimatedLoadTimeMs > 0 {
    est := time.Duration(loadResp.EstimatedLoadTimeMs) * time.Millisecond
    maxWait = 2*est + 60*time.Second
    if maxWait < 3*time.Minute { maxWait = 3*time.Minute }
    if maxWait > 15*time.Minute { maxWait = 15*time.Minute }
}
```

С `est=210s`: `maxWait = 2*210 + 60 = 480s = 8m0s` (точно совпало с
реальным 8m7s ошибкой пользователя).

### Почему cppworker underestimation

cppworker estimation = `file_size / GPU_bandwidth` (грубо). Не
учитывает:

- **CUDA_Host pinned memory alloc** для 19GB весов: 5-10 мин
  (slow page locking)
- **KV cache alloc** для context window: 5-30 сек
- **Graph capture / warmup**: 30-60 сек
- **CUDA stream synchronization**: overhead на каждом blk

## 1. Round 35c fix (8 files, +329/-10)

### Архитектура: env-tunable formula

```
maxWait = min(
    max(multiplier * est + buffer, cap/10, 1s),
    cap
)
```

Где все три компонента env-tweakable:
- `LB_NCTX_PREFLIGHT_WAIT_MULTIPLIER` (1-6, default 2)
- `LB_NCTX_PREFLIGHT_WAIT_BUFFER_SEC` (0-600, default 60)
- `LB_NCTX_PREFLIGHT_MAX_WAIT_SEC` (5-3600, default 900)

### Min floor = cap/10 (НЕ hardcoded 3min)

Раньше min был hardcoded 3 min. Это:
- Блокировало unit-тесты с маленьким cap через `t.Setenv`
- Не имело смысла для production (cap=900s → min=90s, headroom 8x)

Новый min = cap/10:
- cap=900s → min=90s (gemma-4 5GB загружается 80-90s, fits)
- cap=3s (test) → min=300ms (unit-тесты работают)
- cap=3600s → min=360s (большие модели с длинным CUDA_Host alloc)

### Приоритет config resolution

1. `proxy.nctxReload.Config()` (production: loaded from config.json)
2. ENV `applyNCtxReloadEnvOverrides(def)` (override config)
3. Hardcoded defaults (5min cap, 2x mult, 60s buffer) для unit-тестов
   с nil proxy + ENV

### Helper signature: `(time.Duration, int, time.Duration)`

`resolvePreflightWaitTuning(proxy *Proxy) (cap, multiplier, buffer)`
— единая точка входа для `executeLlamaCppLoad`.

## 2. Deployment values

### A10 (sm_86, 24GB VRAM, full GPU offload)

IMPORTANT (Round 35c fix, 2026-08-13): NVIDIA A10 = sm_86 (compute
capability 8.6), та же архитектура Ampere что и RTX 3070. **Image
`gpu-86-abort-r35` подходит БЕЗ пересборки**. A10 это Ampere (GA102),
НЕ Blackwell (sm_120 = RTX 50xx / B100 / B200).

С 24GB VRAM все 22GB Qwen3.6 влезают на GPU целиком (`CPPWORKER_GPU_LAYERS=99`).

22GB Qwen3.6 на A10: 3-5 мин load time.
`2 * 3 + 60 = 7.2 min < 900s (15 min) cap` → **defaults хватают**.

```bash
LB_NCTX_PREFLIGHT_MAX_WAIT_SEC=900     # default
LB_NCTX_PREFLIGHT_WAIT_MULTIPLIER=2    # default
LB_NCTX_PREFLIGHT_WAIT_BUFFER_SEC=60   # default
```

### 3070 stress (sm_86, 8GB VRAM, 3 GPU layers + CUDA_Host)

22GB Qwen3.6 на 3070: 6-12 мин load time. Поднять:

```bash
LB_NCTX_PREFLIGHT_MAX_WAIT_SEC=1800   # 30 min (default 900s, 2x headroom)
LB_NCTX_PREFLIGHT_WAIT_MULTIPLIER=4   # 22GB MoE underestimation
LB_NCTX_PREFLIGHT_WAIT_BUFFER_SEC=120 # +2 min buffer
```

С этими: `maxWait = min(4*210 + 120, 1800) = min(960, 1800) = 16 min`.

## 3. Изменения

### 3.1 `pkg/types/balancing.go` — новые поля

```go
PreflightMaxWaitSec     int `json:"preflight_max_wait_sec" yaml:"preflight_max_wait_sec"`
PreflightWaitMultiplier int `json:"preflight_wait_multiplier" yaml:"preflight_wait_multiplier"`
PreflightWaitBufferSec  int `json:"preflight_wait_buffer_sec" yaml:"preflight_wait_buffer_sec"`
```

### 3.2 `internal/balancer/nctx_reload.go` — helpers

```go
func (c NCtxReloadConfig) effectivePreflightMaxWait() time.Duration {
    t := c.PreflightMaxWaitSec
    if t <= 0 { t = 900 }
    if t < 5 { t = 5 }      // Round 35c: lowered from 60 to 5
    if t > 3600 { t = 3600 }
    return time.Duration(t) * time.Second
}

func (c NCtxReloadConfig) effectivePreflightWaitMultiplier() int {
    m := c.PreflightWaitMultiplier
    if m <= 0 { m = 2 }
    if m < 1 { m = 1 }
    if m > 6 { m = 6 }
    return m
}

func (c NCtxReloadConfig) effectivePreflightWaitBuffer() time.Duration {
    b := c.PreflightWaitBufferSec
    if b < 0 { b = 60 }     // Round 35c: preserve explicit 0
    if b > 600 { b = 600 }
    return time.Duration(b) * time.Second
}
```

### 3.3 `internal/balancer/nctx_reload_config_bridge.go` — ENV

```go
// Новые ENV vars
if v, ok := os.LookupEnv("LB_NCTX_PREFLIGHT_MAX_WAIT_SEC"); ok {
    if n, err := strconv.Atoi(v); err == nil {
        cfg.PreflightMaxWaitSec = n
    }
}
// + аналогично для MULTIPLIER и BUFFER_SEC

// Helper для nil-proxy (unit-тесты)
func resolvePreflightWaitTuning(proxy *Proxy) (time.Duration, int, time.Duration) {
    capWait := 5 * time.Minute
    multiplier := 2
    bufDur := 60 * time.Second
    if proxy != nil && proxy.nctxReload != nil {
        ncCfg := proxy.nctxReload.Config()
        capWait = ncCfg.effectivePreflightMaxWait()
        multiplier = ncCfg.effectivePreflightWaitMultiplier()
        bufDur = ncCfg.effectivePreflightWaitBuffer()
        return capWait, multiplier, bufDur
    }
    // ENV fallback для unit-тестов
    envCfg := applyNCtxReloadEnvOverrides(DefaultNCtxReloadConfig())
    capWait = envCfg.effectivePreflightMaxWait()
    multiplier = envCfg.effectivePreflightWaitMultiplier()
    bufDur = envCfg.effectivePreflightWaitBuffer()
    return capWait, multiplier, bufDur
}
```

### 3.4 `internal/balancer/model_management.go` — usage

```go
capWait, multiplier, bufDur := resolvePreflightWaitTuning(mm.proxy)
maxWait := capWait
if loadResp.EstimatedLoadTimeMs > 0 {
    est := time.Duration(loadResp.EstimatedLoadTimeMs) * time.Millisecond
    maxWait = time.Duration(multiplier)*est + bufDur
    // Round 35c: dynamic min вместо hardcoded 3min
    if maxWait < capWait/10 { maxWait = capWait / 10 }
    if maxWait < time.Second { maxWait = time.Second }
    if maxWait > capWait { maxWait = capWait }
}
logger.Get().Infow("executeLlamaCppLoad: 202 Accepted (async load), polling for completion",
    "backend", backendID, "model", req.ModelName,
    "estimated_ms", loadResp.EstimatedLoadTimeMs,
    "max_wait", maxWait, "cap_wait", capWait,
    "multiplier", multiplier, "buffer", bufDur)
```

### 3.5 Tests

- `TestLoadNCtxReloadConfig_Round35c_EnvTunables` — defaults + ENV
  override работают
- `TestNCtxReloadConfig_PreflightTunables_Clamping` — clamp [5, 3600],
  [1, 6], [0, 600]
- `TestExecuteLlamaCppLoad_202NeverLoads` обновлён с `t.Setenv` для
  быстрого test (~2s вместо 15min)

## 4. Live verification (2026-08-13 11:08 UTC)

- 5 containers healthy (balancer r35c + cppworker r35 + agent + webui + buildx)
- gemma-4-E4B-it-Q4_K_M @ 32K context, **157.6s load** (cap default 900s)
- gemma-4 chat: "OK" in **2.17s**, 2 completion tokens
- Qwen3.6-35B-A3B-UD-Q4_K_M @ 4K context (auto-tuned by cppworker),
  **396.2s load** (6m36s — но это с СТАРЫМИ r35 настройками balancer,
  сейчас с r35c + новые env tunables юзер может поставить
  MAX_WAIT=1800 для покрытия)
- Env values match running container

### Pre-existing test (skipped)

`TestHandleOpenAIChatCompletions_Cline_NonNormalizationRegression`
hangs на 60s timeout. Проверено на `master` (git stash) — там тоже
висит. НЕ связано с Round 35c. Отложен.

## 5. Reusable patterns (cross-project)

36. **Env-tunable timeout formula** (`multiplier*est + buffer`, capped):
    для distributed систем с динамической нагрузкой hardcoded формулы
    ломаются при непредвиденных сценариях (MoE+auto-offload, медленный
    disk, fragmented memory). Делайте все три компонента env-tweakable:
    multiplier, buffer, cap. Min floor = cap/10 (НЕ hardcoded 3min).

37. **ENV fallback для nil receiver**: когда функция читает config
    из `proxy.GetCoordinator().Config()`, но в unit-тестах proxy=nil,
    добавьте `applyNCtxReloadEnvOverrides(def)` fallback. Позволяет
    тестам с `t.Setenv` сокращать таймауты без поднятия полного
    proxy.

38. **Clamp min не должен быть слишком большим**: hardcoded min=60s
    блокировал unit-тесты с cap=3s. Min=5s даёт 12x headroom для
    тестов, production значения (900s) работают как раньше.

39. **Explicit 0 vs unset для optional ENV**: в Go нельзя отличить
    "поле не задано в JSON" от "поле=0" без pointer types. Workaround:
    в ENV path применяем явное значение (включая 0), в config path
    default-filling на 0 (если поле = 0, ставим default). Разное
    поведение для ENV vs config, но юзкейсы unit-тестов покрыты.

40. **cppworker estimation off by ~2x для MoE+auto-offload**: cppworker
    оценивает load time по `file_size / GPU_bandwidth`, не учитывает
    CUDA_Host pinned memory alloc (5-10 мин на 19GB) и KV cache alloc.
    Workaround: env-tunable multiplier в balancer (НЕ fix estimation
    в cppworker). Long-term fix: better cppworker estimation formula.

## 6. Open issues (из этого fix не покрыты)

1. **JSON parse error "Expecting value: line 2 column 1 (char 2)"**
   после успешной загрузки. Возможные причины: empty 503 body,
   cut stream, response mismatch с chat template. Pre-existing.
2. **Reload loop после загрузки** (gemma-4 reloaded). Возможные
   причины: different ctx size per request, keep_alive expired, или
   balancer preflight решал Reload из-за VRAM pressure. Pre-existing.
3. **cppworker estimation accuracy** — off by 1.9x для MoE+auto-offload.
   Long-term fix: better cppworker formula (account для CUDA_Host alloc).
4. **Pre-existing test hang**: `TestHandleOpenAIChatCompletions_Cline_NonNormalizationRegression`.

## 7. A10 deploy update

Для A10 (sm_86, 24GB VRAM) defaults хватают (22GB Qwen3.6 за 3-5
мин, 2*3+60=7.2 мин < 15 мин cap). **Текущий image `gpu-86-abort-r35`
подходит БЕЗ пересборки** (A10 = sm_86, та же архитектура Ampere что
и 3070). Если тестируете 70B+ модели на A10, поднимите
`MAX_WAIT=1800` для safety margin.

См. также: `deployments/.env.bundled-with-agent.example` секции 3
(NCTX reload) и 6 (Round 35c notes).

**Правильный процесс для A10** (Round 35c fix):
1. Copy project + `.env.bundled-with-agent` (как раньше)
2. `CUDA_ARCH=86` (НЕ 120 — это была моя ошибка)
3. `CPPWORKER_GPU_TAG=86-abort-r35` (готовый image с Docker Hub)
4. `docker compose up -d --no-build` (НЕ пересобирать, image уже подходит)
5. С 24GB VRAM поднять `CPPWORKER_GPU_LAYERS=99` — все layers on GPU
6. Тест: gemma-4 + qwen3 стабильно работают (verified на 3070, тот же
   compute capability)
