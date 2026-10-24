# Issue 2026-06-29: Big Model (20GB VRAM) Loading + Visual Defects (2026-06-30) — Resolution Status

## Visual Defects Fixes (2026-06-30)

Исправил три визуальных дефекта, замеченных пользователем после bundled-depl:

1. **Дубли бэкенда на странице GGUF** — после bundled-up в кластере появлялись
   два бэкенда с одинаковым физическим endpoint (host:port): `cppworker-gpu-bundled`
   от shell-script и `cppworker-gpu` от Go-side cppworker'а. На странице GGUF
   WebUI это отображалось как две карточки одного физического узла.
   - **Фикс**: добавлен de-dup в `internal/api/gguf_backends_handler.go`.
     Группировка по `(host, port)`, приоритет: бэкенд с `hasAgent=true` →
     первый встретившийся. Pure unit-тесты `TestBuildGgufBgInfo*` покрывают
     все edge cases (host.docker.internal, default port, warming-up models).

2. **Жёлтая иконка переключения темы и колокольчик уведомлений**:
   - `btn-theme-toggle` использовал `fa-circle-half-stroke` (Font Awesome 6 Free solid),
     у которого одна половина залита **фиксированным жёлтым `#fbbf24`** вне контроля CSS.
     Заменили в JS на переключение `fa-moon` (dark) ⇄ `fa-sun` (light), иконки —
     одноцветные, наследуют `var(--text-secondary)`.
   - Колокольчик использовал системный эмодзи `🔔` (жёлтый, рендерится ОС).
     Заменили на `fa-bell`. В `webui/css/components.css` добавлено
     `.btn-theme-toggle > * { color: inherit; line-height: 1; }` — фикс центровки
     и явного наследования цвета от родителя (на случай внешних правил
     `.fa-stack` / `.fa-ul`).

3. **Новый compose с агентом** — `deployments/docker-compose.cppworker-bundled-with-agent.yml`
   (расширение bundled) + `.env.bundled-with-agent.example`. Agent sidecar
   регистрирует cppworker-gpu в балансер отдельным backend `cppworker-gpu-bundled-agent`
   (с hasAgent=true), чтобы WebUI показывал реальные GPU/VRAM/CPU метрики.
   ВАЖНО: Go-side регистрация cppworker остаётся ОТКЛЮЧЁННОЙ (CPPWORKER_REGISTER_DISABLE=true),
   чтобы избежать дубликатов; de-dup в GGUF handler отфильтрует, если агенту
   когда-нибудь понадобится локальное API-регистрация.

## Issue 2026-06-29: Big Model (20GB VRAM) Loading — Issue Summary
Big models (e.g. qwen3.6-72B 22 GB) on 20 GB VRAM could not load.
Cppworker used hardcoded `estimatedLayers=80` and `kvReserve=2 GB` for KV-cache
calculation, did not retune `n_ctx` on LOAD, so weights + KV-cache for n_ctx=32768
exceeded available VRAM. llama.cpp.LoadModel fell back to n_ctx=4096 or failed with OOM.

## Resolution (2026-06-25 → 2026-06-29)

### Code changes shipped

| File | Change | Status |
|---|---|---|
| `internal/cppbackend/model_manager.go` | `GGUFModelMeta` extended with `NLayers, NEmbd, NHeads, NKvHeads`; populated lazily via `cppbackend.ReadGGUFHeader` (reads only KV blocks, ~4-16 KB) | SHIPPED |
| `internal/cppbackend/backend.go` | Public wrapper `ReadGGUFHeader(path)` over existing `readGGUFHeaderInfo` (uses `ggufKeyMap` for O(1) key→field mapping) | SHIPPED |
| `cmd/cppworker/lazyload_calc.go` | New `calculateLazyLoadOpts(...)` — 3-stage cascade: Stage 1 `exact_fit`, Stage 2 `partial_offload` (reduce gpu_layers + mmap), Stage 3 `reduced_nctx` (cpu-only + max_viable_nctx) | SHIPPED |
| `cmd/cppworker/lazyload.go` | `ensureModelLoaded` calls `calculateLazyLoadOpts` BEFORE `LoadModelWithOpts`; old hardcoded heuristic kept as fallback when `CPPWORKER_AUTO_TUNE_NCTX_ON_LOAD=false` | SHIPPED |
| `cmd/cppworker/auto_tune_nctx.go` | `nctxSafetyFactor` env-overridable global var (default 0.85) used in Stage 1 calc | SHIPPED |
| `cmd/cppworker/auto_tune_nctx_test.go` | Added `TestAutoTuneNCtx_SafetyFactorFromEnv` (default 0.85, env override 0.92, invalid, out-of-range) | SHIPPED |

### ENV flags

| Env | Default | What it does |
|---|---|---|
| `CPPWORKER_AUTO_TUNE_NCTX_ON_LOAD` | `true` | New behavior (3-stage). Set `false` to use old hardcoded heuristic. |
| `CPPWORKER_NCTX_SAFETY_FACTOR` | `0.85` | VRAM safety margin (Stage 1 only). |
| `CPPWORKER_RAM_FALLBACK_N_CTX` | `false` | Reload model with larger n_ctx on `ErrNCtxNeedsReload`. |
| `CPPWORKER_RAM_FALLBACK_GPU_LAYERS` | unset | Reduce gpu_layers on RAM fallback (0 = CPU-only). |
| `CPPWORKER_RAM_FALLBACK_MAX_N_CTX` | unset | Cap n_ctx during reload (defense against OOM). |
| `CPPWORKER_AUTO_OFFLOAD` | `true` | Old auto-offload heuristic (legacy path, still used when `CPPWORKER_AUTO_TUNE_NCTX_ON_LOAD=false`). |
| `CPPWORKER_VERBOSE` | `false` | Verbose logging for clamp/fallback decisions. |
| `CPPWORKER_WRITE_TIMEOUT` | `30m` | SSE write timeout to client. |

### WebUI Notification Bell + Localization Fixes (2026-06-29 v2)

**Problem 1**: Notification bell dropdown didn't open (CSS `display: flex` overrode HTML `hidden` attribute).
**Problem 2**: Notification dropdown appeared under the page tab content (z-index / stacking-context issue).
**Problem 3**: i18n/ru.js and i18n/en.js were broken after emoji-strip + quote-fix scripts ate opening quotes.

**Fixes shipped**:

- `webui/css/components.css`:
  - `.notifications-dropdown[hidden] { display: none !important; }` (root cause for bell not opening)
  - `.notifications-dropdown` is now `position: fixed` with `z-index: 2147483000` (max int32, always on top)
  - Removed `position: absolute` + `top: calc(100% + 8px)` — JS now calculates coords via `getBoundingClientRect()`
  - Mobile (< 768px) keeps bottom-sheet via `@media`
- `webui/js/app.js:setupNotificationsUI()`:
  - **v3 (2026-06-29)**: Portal-паттерн. `dropdown` переносится в `document.body` на init —
    обход containing block'а от `.header` (содержит `backdrop-filter`) и `transform: scale`
    у иконок, которые перепривязывают `position: fixed` к предку (а не к viewport),
    из-за чего z-index max int32 не спасал от «уходит под вкладки».
  - v2: `positionDropdown()` считает top/right от `btn.getBoundingClientRect()`,
    inline-стили перезаписываются, репозиционирование на `window.resize`,
    очистка inline-стилей при закрытии (для мобильного bottom-sheet).
- `webui/js/i18n/ru.js` + `en.js`:
  - All 1100+ entries verified to parse with `node --check` (exit 0)
  - Fixed multiple classes of bugs: missing trailing commas, doubled quotes `""` from over-eager collapse, lost opening quotes from emoji-strip regex
  - Recovery scripts archived: `tools/fix_i18n_syntax.py`, `tools/collapse_quotes.py`, `tools/scan_i18n_keys.py`

### Build verification

- `go build -tags llama_stub -o cppworker-stub.exe ./cmd/cppworker` — exit 0
- `go build -tags llama_stub -o balancer-stub.exe ./cmd/balancer` — exit 0
- `go test ./cmd/cppworker -run "TestAutoTuneNCtx_SafetyFactorFromEnv|TestCalculateLazyLoadOpts" -tags llama_stub` — PASS
- `node --check webui/js/i18n/ru.js` — exit 0
- `node --check webui/js/i18n/en.js` — exit 0

## Acceptance Criteria (Real-World)

### 20GB VRAM (RTX 3090/4080) with big models

- **qwen3.6-72B** (22 GB) + 20 GB VRAM + n_ctx=32768:
  - Old: silent fallback to n_ctx=4096 or OOM
  - New: `source=partial_offload, applied_gpu≈33 (из 80), applied_nctx=32768, UseMmap=true` (Stage 2)
  - If 8 GB VRAM: Stage 3 → `gpu_layers=0` (CPU-only mmap) + `n_ctx=32768`
  - If `n_ctx` too large: Stage 3 `reduced_nctx` with `max_viable_nctx`
  - If even `max_viable_nctx` doesn't fit: HTTP 413 `insufficient_resources` with `max_viable_n_ctx`, `available_vram_mb`, `available_ram_mb` and actionable `suggestion: "Reduce num_ctx to <X>"`

### Notification Bell

1. Open `http://localhost:18083/` in browser
2. Click bell icon in upper-right corner → dropdown opens below bell
3. Dropdown has solid background, not transparent
4. Dropdown is on top of all page content (z-index 2147483000, position: fixed)
5. Click outside or press Escape → dropdown closes
6. `curl http://localhost:18092/api/v1/cppworker/debug/last-stream` shows last inference state
7. Switch to Mobile view (< 768px) → dropdown becomes bottom sheet (full width, bottom of viewport)

### Localization (RU/EN)

1. Open WebUI with lang=`ru` (default) — all UI text in Russian, no `[i18n] Missing translation key` errors
2. Switch to lang=`en` — all UI text in English
3. No JS console errors related to i18n syntax

## GPU Detection Fix in Agent Container (2026-06-30)

### Проблема

После запуска `docker compose -f docker-compose.cppworker-bundled-with-agent.yml up -d`
agent регистрируется в балансере, но в логах пишет:

```
[...] No GPU detected, CPU mode
[...] Metrics collected: CPU=X% Load=Y RAM=Z/W RPS=... Models=...
```

GPU-метрики (UsagePercent, MemoryTotal, MemoryUsed, Temperature) НЕ отправляются.
WebUI показывает «No GPU data» для этого backend.

### Корневая причина

Сервис `agent` в `docker-compose.cppworker-bundled-with-agent.yml` НЕ имел:

```yaml
runtime: nvidia                       # ← отсутствовало
environment:
  NVIDIA_DRIVER_CAPABILITIES: ...     # ← отсутствовало
```

Без `runtime: nvidia` NVIDIA Container Toolkit НЕ монтирует в контейнер agent'а:

- `/dev/nvidia0`, `/dev/nvidiactl`, `/dev/nvidia-uvm*` — устройства GPU
- `libnvidia-ml.so.1` — без неё `nvml.Init()` возвращает `ERROR_LIBRARY_NOT_FOUND`

`cppworker-gpu` в том же compose-файле имел `runtime: nvidia` (строка 73),
поэтому работал корректно. Agent — нет.

В результате `internal/agent/collector.go:detectPlatformMode()` проходил по проверкам:

1. `exec.LookPath("nvidia-smi")` → fail (нет в PATH, nvidia/cuda:base не содержит)
2. `nvmlAvailable()` → `nvml.Init()` → ERROR_LIBRARY_NOT_FOUND
3. `os.Stat("/dev/nvidia0")` → fail (устройство не смонтировано)
4. → возврат `ModeCPU`

### Решение (2026-06-30)

#### 1. `deployments/docker-compose.cppworker-bundled-with-agent.yml`

Добавлено сервису `agent`:

```yaml
agent:
  runtime: nvidia                          # ← НОВОЕ (ранее отсутствовало)
  environment:
    - NVIDIA_VISIBLE_DEVICES=${NVIDIA_VISIBLE_DEVICES:-all}
    - NVIDIA_DRIVER_CAPABILITIES=compute,utility   # ← НОВОЕ
    ...
```

Симметрично с `cppworker-gpu` (строка 73), который уже имел эти настройки.

#### 2. `internal/agent/collector.go:detectPlatformMode()`

Изменён порядок проверок под контейнер с `runtime: nvidia`:

1. `/dev/nvidia0` (Linux-контейнер с runtime nvidia) — самый дешёвый признак
2. `/proc/driver/nvidia/version` (нативный Linux с NVIDIA-драйвером, без nvidia-smi)
3. `nvmlAvailable()` (через build tag `nvml`)
4. `nvidia-smi` (последним: обычно НЕТ в base-образах nvidia/cuda)

Каждый шаг логирует причину успеха/неудачи — больше не скупое «No GPU detected»,
а подробный список: «/dev/nvidia0: no such file; /proc/driver/nvidia/version:
no such file; nvml.Init() failed (см. лог NVML выше); nvidia-smi not in PATH».

#### 3. `internal/agent/collector_gpu.go`

Жёсткий `gpuUnavailable bool` (без TTL) заменён на TTL-кэш 60 сек:

```go
const gpuUnavailableTTL = 60 * time.Second

type gpuAvailabilityState struct {
    unavailable bool
    since       time.Time
}

func markGPUUnavailable()  // идемпотентно: since не сдвигается при повторе
func markGPUAvailable()    // сбрасывает флаг
func isGPUUnavailableCached() bool  // проверяет с учётом TTL
func ResetGPUAvailabilityCache()    // для тестов и admin-endpoint
```

Это позволяет автоматически восстанавливаться после ситуаций, когда
драйверы монтируются уже после старта agent'а (например, при `compose up`,
когда NVIDIA Container Toolkit поднимает контейнеры параллельно).

Добавлен `logNvidiaSmiError(err)` для диагностики типа ошибки:
`*exec.Error` (бинарь не найден) vs `*exec.ExitError` (бинарь есть, но ошибка).

#### 4. `internal/agent/collector.go:Agent` struct

Удалено поле `gpuUnavailable bool` (больше не нужно — состояние вынесено
в пакетную переменную `gpuState` в `collector_gpu.go`).

#### 5. Тесты

`internal/agent/collector_gpu_test.go` (новый файл, 16 тестов):

- `TestGPUAvailabilityCache_WithinTTL` — пометка и проверка в пределах TTL
- `TestGPUAvailabilityCache_TTLReset` — после истечения TTL сбрасывается
- `TestGPUAvailabilityCache_IdempotentMark` — повторный mark не сдвигает since
- `TestGPUAvailabilityCache_MarkAvailable` — успешный сбор сбрасывает флаг
- `TestResetGPUAvailabilityCache` — функция сброса работает
- `TestDetectPlatformMode_OverrideRespected` — для ModeCPU/ModeGPU override
- `TestDetectPlatformMode_CpuModeFallback` — graceful fallback на пустой системе
- `TestDetectPlatformMode_WithDevNvidia0` (skipped без GPU)
- `TestDetectPlatformMode_WithProcNvidiaVersion` (skipped без GPU)
- `TestDetectPlatformMode_NoGPUOnLinux` (только в CI)
- `TestLogNvidiaSmiError` — не паникует на разных типах ошибок
- `TestExecuteNvidiaSmi_NotPanics` — не паникует ни на какой системе
- `TestMapLlamaGPUMetrics_EmptyAndNonEmpty` — защита от VRAMFree > VRAMTotal
- `TestParseNvidiaSmiOutput` — парсер CSV без реального запуска
- `TestCountGPUs` — подсчёт GPU
- `TestParseGPUMModels` — парсинг названий моделей

Результат: `go test -tags llama_stub ./internal/agent/ -count=1` — `ok 10.045s`,
16 PASS, 3 SKIP (требуют реального GPU).

#### 6. `deployments/.env.bundled-with-agent.example`

Добавлены подробные комментарии в конце файла с описанием проблемы и фикса.

### SIGSEGV-фикс в `initNVML()` (2026-06-30 v2)

После применения фикса `runtime: nvidia` агент перестал падать на «No GPU detected»,
но появился **новый** crash на старте:

```
SIGSEGV: segmentation violation
PC=0x0 m=0 sigcode=1 addr=0x0
signal arrived during cgo execution
goroutine 1 ... [syscall]:
runtime.cgocall(0x746fd0, 0xc000037a90)
github.com/NVIDIA/go-nvml/pkg/nvml._Cfunc_nvmlInit_v2()
github.com/NVIDIA/go-nvml/pkg/nvml.nvmlInit_v2()
github.com/NVIDIA/go-nvml/pkg/nvml.(*library).Init(...)
ollama-loadbalancer/internal/agent.initNVML()
  ollama-loadbalancer/internal/agent/nvml_unix.go:29 +0x92
ollama-loadbalancer/internal/agent.nvmlAvailable(...)
  ollama-loadbalancer/internal/agent/nvml_unix.go:63
ollama-loadbalancer/internal/agent.(*Agent).detectPlatformMode(0xc000002180)
  ollama-loadbalancer/internal/agent/collector.go:175 +0x285
```

#### Корневая причина

CGo-обёртка над `libnvidia-ml.so.1` (через `github.com/NVIDIA/go-nvml/pkg/nvml`)
вызывает `dlopen("libnvidia-ml.so.1")`. Если библиотека **НЕ** найдена в контейнере
(например, в `nvidia/cuda:12.2.0-base-ubuntu22.04` — этот base не содержит
userspace-часть драйвера), `dlopen()` возвращает `NULL`, и попытка вызвать
функцию `nvmlInit_v2` по NULL-указателю приводит к **SIGSEGV с PC=0x0**.

Критично: `recover()` **не ловит** SIGSEGV из C-кода (POSIX-сигналы не идут
через Go runtime). Поэтому единственный способ избежать краша — **не вызывать**
`nvml.Init()` если библиотека заведомо отсутствует.

#### Решение (3 уровня защиты)

**Уровень 1 — `internal/agent/nvml_unix.go: nvmlLibraryPresent()`**

Pre-check наличия `libnvidia-ml.so.1` по известным путям:

```go
var nvmlLibraryPaths = []string{
    "/usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1",
    "/usr/lib64/libnvidia-ml.so.1",
    "/lib/x86_64-linux-gnu/libnvidia-ml.so.1",
    "/usr/lib/aarch64-linux-gnu/libnvidia-ml.so.1",
    "/usr/local/lib/libnvidia-ml.so.1",
    "/usr/lib/libnvidia-ml.so.1",
    "/lib64/libnvidia-ml.so.1",
}

func nvmlLibraryPresent() (string, bool) {
    for _, p := range nvmlLibraryPaths {
        if _, err := os.Stat(p); err == nil {
            return p, true
        }
    }
    return "", false
}
```

`initNVML()` теперь начинается с этой проверки; если библиотека не найдена —
возвращается `false` **до** вызова `nvml.Init()`.

**Уровень 2 — `internal/agent/collector.go: detectPlatformMode()`**

Даже если уровень 1 не помог (например, экзотический путь к библиотеке), `nvmlAvailable()`
вызывается **только** при наличии `/dev/nvidia0`:

```go
if runtime.GOOS == "linux" {
    if _, err := os.Stat("/dev/nvidia0"); err == nil {
        if nvmlAvailable() {  // ← только при наличии GPU-устройства
            ...
        }
    }
}
```

`/dev/nvidia0` — гарантия того, что NVIDIA Container Toolkit смонтировал и устройства,
и userspace-библиотеку.

**Уровень 3 — `recover()` в `initNVML()` (defense-in-depth)**

`recover()` не ловит SIGSEGV, но ловит panic'и, которые cgo-обёртка может
бросить при невалидной библиотеке или ошибке драйвера. В случае panic'а NVML
помечается как «не инициализирован», `collectGPUInfoNVML()` возвращает пустой результат:

```go
initOK := false
func() {
    defer func() {
        if r := recover(); r != nil {
            fmt.Printf("[NVML] Panic during Init (lib=%s): %v — NVML помечен как недоступный\n", libPath, r)
        }
    }()
    ret := nvml.Init()
    if ret != nvml.SUCCESS { return }
    initOK = true
}()
```

#### Файлы изменены

| Файл | Изменение |
|---|---|
| `internal/agent/nvml_unix.go` | `nvmlLibraryPaths`, `nvmlLibraryPresent()`, pre-check в `initNVML()`, `recover()` вокруг `nvml.Init()` |
| `internal/agent/collector.go:detectPlatformMode()` | Дополнительный gate: `nvmlAvailable()` вызывается только при `/dev/nvidia0` |

#### Verification

- `go vet -tags "nvml llama_stub" ./internal/agent/` — exit 0
- `go test -tags "nvml llama_stub" -count=1 ./internal/agent/` — **49/49 PASS**, 0 SIGSEGV
- `go build -tags "nvml llama_stub" -o agent-nvml-stub.exe ./cmd/agent` — exit 0
- `go build -tags "llama_stub" -o agent-stub.exe ./cmd/agent` — exit 0
- `go build -tags "llama_stub" -o balancer-stub.exe ./cmd/balancer` — exit 0

#### Acceptance Criteria

1. Запуск `agent-stub.exe` на Windows-хосте без NVIDIA-драйвера — **НЕ падает**,
   в логах: `[NVML] libnvidia-ml.so.1 not found в известных путях — NVML недоступен (безопасно пропускаем)`
2. Запуск `agent-nvml-stub.exe` в контейнере `nvidia/cuda:12.2.0-base-ubuntu22.04`
   (без `runtime: nvidia`) — **НЕ падает**, в логах то же сообщение.
3. Запуск `agent-nvml-stub.exe` в контейнере с `runtime: nvidia` —
   `initNVML()` находит `/usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1`,
   `nvml.Init()` возвращает `SUCCESS`, `GPU detected via /dev/nvidia0`.
4. Никаких SIGSEGV, никаких abort'ов в логах.

#### NB для пользователя

Если вы видите `SIGSEGV: segmentation violation PC=0x0` в stack-trace с упоминанием
`nvmlInit_v2` — обновите `internal/agent/nvml_unix.go` (этот фикс уже в main).
Старые бинарники `agent` (до 2026-06-30 v2) **могут** падать на старых контейнерах
без `runtime: nvidia`; пересоберите образ: `docker compose --env-file .env.bundled-with-agent up -d --build`.

### De-dup fix v2 (2026-06-30 v3)

После применения фикса GPU-detection и SIGSEGV на странице GGUF остались
две независимые проблемы:

#### Проблема 1: WebUI показывает 2 бэкенда для одного физического cppworker

**Симптом**: `GET /api/v1/gguf/backends` возвращает массив из двух элементов:
```
[
  { id: "cppworker-gpu-bundled",       host: "cppworker-gpu",         cppWorkerPort: 18092, hasAgent: false },
  { id: "cppworker-gpu-bundled-agent", host: "cppworker-gpu-bundled-agent", cppWorkerPort: 18091, hasAgent: true }
]
```
На странице GGUF это выглядит как «один бэкенд под двумя именами».

**Корневая причина**:
`internal/agent/collector_register.go:register()` использовал `getPublicHost()` (= `PublicHost` = `AGENT_PUBLIC_HOST` = имя **самого** контейнера agent'а) как `host` в register-payload, и `extractCppWorkerPort()` возвращал **18091** (legacy fallback) для `BackendTypeLlamaCpp`.

Cppworker (через `register-with-balancer.sh`) регистрировался с:
- `host=cppworker-gpu, cppWorkerPort=18092` (из `CPPWORKER_ADVERTISE_HOST/PORT`)

Agent регистрировался с:
- `host=cppworker-gpu-bundled-agent, cppWorkerPort=18091` (из `AGENT_PUBLIC_HOST` + legacy fallback)

**Ключи де-дупликации** в `gguf_backends_handler.go:107` (`{host: bm.Host, port: bm.CppWorkerPort}`) были **разные** → de-dup **не срабатывал** → WebUI показывал оба бэкенда.

#### Проблема 2: agent-бэкенд мигает (пропадает/появляется каждые ~30 сек)

**Симптом**: на странице GGUF бэкенд `cppworker-gpu-bundled-agent` то появляется (со свежими GPU-метриками), то пропадает (серая заглушка). В логах cppworker/balancer — никаких ошибок.

**Корневая причина**:
`internal/api/handlers_agents.go:agentRegisterHandler` (строки 152-177) после регистрации запускал health-check, который **всегда** делал:
```go
url := fmt.Sprintf("http://%s:%d/api/tags", backend.Host, backend.OllamaPort)
resp, err := client.Get(url)
```
Для agent-бэкенда `host=cppworker-gpu-bundled-agent` (имя agent-контейнера) — у agent **нет** Ollama API на 11434. Запрос `GET http://cppworker-gpu-bundled-agent:11434/api/tags` → connection refused → через 1 сек после регистрации бэкенд помечался `StatusUnhealthy` и **пропадал** из GGUF (фильтр `unhealthyStatuses`). На следующем heartbeat (каждые 30 сек) — снова `StatusHealthy` → **появлялся**. Визуально это «мигание».

#### Решение (3 файла, 1 compose, 1 env-файл)

**1. `pkg/types/session.go`** — добавлены 2 поля в `AgentConfig`:
```go
CppWorkerHost string `json:"cppWorkerHost"`  // host физического cppworker
CppWorkerPort int    `json:"cppWorkerPort"`  // port физического cppworker
```

**2. `cmd/agent/main.go`** — чтение env:
```go
CppWorkerHost: env.Get("AGENT_CPPWORKER_HOST", ""),
CppWorkerPort: env.GetInt("AGENT_CPPWORKER_PORT", 0),
```

**3. `internal/agent/collector_network.go`** — новые `extractCppWorkerHost/Port` с приоритетом:
- CppWorkerHost из конфига → хост из CppWorkerURL → PublicHost → "localhost"
- CppWorkerPort из конфига → порт из CppWorkerURL → **18092 (bundled default, НЕ 18091)** для llama_cpp → 0

**4. `internal/agent/collector_register.go`** — `register()` использует:
- `registerHost = extractCppWorkerHost()` (НЕ `getPublicHost()`) — для llama_cpp
- `registerCppWorkerPort = extractCppWorkerPort()` — для llama_cpp
- publicHost теперь только для agentPort/healthcheck самого agent'а

**5. `internal/api/handlers_agents.go`** — health-check после регистрации:
```go
if backend.Type == types.BackendTypeLlamaCpp && backend.CppWorkerPort > 0 {
    healthURL = fmt.Sprintf("http://%s:%d/health", backend.Host, backend.CppWorkerPort)
} else {
    healthURL = fmt.Sprintf("http://%s:%d/api/tags", backend.Host, backend.OllamaPort)
}
```

**6. `deployments/docker-compose.cppworker-bundled-with-agent.yml`** — добавлены env:
```yaml
- AGENT_CPPWORKER_HOST=cppworker-gpu
- AGENT_CPPWORKER_PORT=18092
```

**7. `deployments/.env.bundled-with-agent.example`** — секция с описанием фикса.

#### Файлы изменены (de-dup v2)

| Файл | Изменение |
|---|---|
| `pkg/types/session.go` | `AgentConfig.CppWorkerHost/CppWorkerPort` |
| `cmd/agent/main.go` | Чтение `AGENT_CPPWORKER_HOST/PORT` |
| `internal/agent/collector_network.go` | `extractCppWorkerHost()` + fallback 18092 |
| `internal/agent/collector_register.go` | `register()` использует `extractCppWorkerHost/Port` для llama_cpp |
| `internal/api/handlers_agents.go` | Health-check для llama_cpp → `GET /health` на cppworker |
| `deployments/docker-compose.cppworker-bundled-with-agent.yml` | `AGENT_CPPWORKER_HOST=cppworker-gpu, AGENT_CPPWORKER_PORT=18092` |
| `deployments/.env.bundled-with-agent.example` | Секция «De-dup v2» с описанием |

#### Verification

- `go vet -tags llama_stub ./internal/agent/ ./pkg/types/ ./cmd/agent/ ./internal/api/` — exit 0
- `go test -tags llama_stub -count=1 ./internal/agent/` — **PASS** (10.491s, 59 тестов)
  - 10 новых `TestExtract*` (extract_cppworker_test.go)
  - 16 `TestGPU*` (collector_gpu_test.go)
  - 33 pre-existing теста
- `go test -tags llama_stub -count=1 ./internal/api/` — **PASS** (1.733s)
- `go build -tags llama_stub -o agent-stub.exe ./cmd/agent` — exit 0
- `go build -tags llama_stub -o balancer-stub.exe ./cmd/balancer` — exit 0

#### Acceptance Criteria

1. **Дубли исчезли** — `GET /api/v1/gguf/backends` возвращает массив из **1 элемента**:
   ```json
   { "id": "cppworker-gpu-bundled-agent", "host": "cppworker-gpu", "cppWorkerPort": 18092, "hasAgent": true }
   ```
2. **Мигание прекратилось** — agent-бэкенд стабильно `StatusHealthy`, не пропадает.
3. **Логи agent'а** содержат:
   ```
   [time] Registering llama_cpp backend at cppworker-gpu:18092 (agent at cppworker-gpu-bundled-agent)
   [time] Agent registered successfully: cppworker-gpu-bundled-agent
   ```
4. **GPU-метрики реальные** — VRAM/Usage/Temperature приходят от agent'а, не заглушка.
5. **Все тесты зелёные**, build OK.

#### NB для пользователя

После `git pull`/`up -d --build`:
1. Пересоберите образы: `docker compose -f docker-compose.cppworker-bundled-with-agent.yml --env-file .env.bundled-with-agent up -d --build` (старые бинарники agent'а и балансера использовали старые host/port).
2. Убедитесь, что в `.env.bundled-with-agent` нет кастомных переопределений `AGENT_PUBLIC_HOST` для бэкенда (теперь он только для самого agent'а). Физический cppworker определяется через `AGENT_CPPWORKER_HOST/PORT` (по умолчанию `cppworker-gpu:18092`).

### Acceptance Criteria

1. **GPU detected** — в логах agent'а при `up -d`:
   ```
   [...] GPU detected via /dev/nvidia0
   [...] Platform mode detected: gpu
   [...] Metrics collected: CPU=X% RAM=Y/WMB GPU=Z% GPU_Mem=A/BMB RPS=...
   ```
2. **WebUI GGUF страница** показывает `hasAgent=true` и реальные
   VRAM/CPU/Usage метрики для `cppworker-gpu-bundled-agent`.
3. **Build OK**: `go vet -tags llama_stub ./internal/agent/` — exit 0.
4. **Tests OK**: `go test -tags llama_stub ./internal/agent/ -count=1` — 16/16 PASS, 3 SKIP.
5. **No regression** в `internal/api/gguf_backends_handler.go` (de-dup по host:port).

### Что нужно сделать пользователю

После обновления compose-файла достаточно:

```bash
cd deployments
docker compose -f docker-compose.cppworker-bundled-with-agent.yml \
  --env-file .env.bundled-with-agent up -d --build
```

`--build` нужен, чтобы пересобрать образ agent'а (если менялся код).
Compose-файл перечитается автоматически. Перезапуск cppworker не требуется.

### Yellow icons fix (2026-06-30 v4)

После предыдущих фиксов на странице GGUF и в шапке WebUI остался ещё один
визуальный дефект: «иконки смены темы (fa-moon/fa-sun) и колокольчик
уведомлений (fa-bell) остаются жёлтого цвета и не подходят под стиль».

#### Корневая причина

В ходе диагностики выяснилось, что в WebUI на самом деле жёлтым был **не**
theme toggle / bell в шапке (они уже наследовали `var(--text-secondary)` —
серый в dark/light), а **плашка engine-llama_cpp** в sidebar списка бэкендов:

```css
/* webui/css/layout.css (до фикса) */
.backend-engine-badge.engine-llama_cpp {
    color: #f59e0b;                              /* янтарный / жёлтый */
    border-left: 3px solid #f59e0b;
}
```

`#f59e0b` — это амбер (warm orange), который визуально читается как жёлтый и
ассоциируется с warning-индикатором. На тёмной теме он выглядел особенно
«грязно-жёлтым» рядом с серым `var(--text-secondary)` (moon/sun/bell) и
синим `var(--accent)` (engine-ollama).

Дополнительно — у колокольчика и theme toggle мог быть риск, что Font Awesome
внутренними миксинами (`.fa-stack`, SVG `fill`/`stroke`) перекроет
`color: inherit` от родителя. Поэтому добавлен явный `color: inherit` для
конкретных FA-классов.

#### Решение

**1. `webui/css/layout.css:87-90`** — engine-llama_cpp → нейтральный серый:

```css
.backend-engine-badge.engine-llama_cpp {
    color: var(--text-secondary);
    border-left: 3px solid var(--text-secondary);
}
```

Теперь плашка llama_cpp визуально симметрична engine-ollama (`var(--accent)`,
синий) и не конфликтует с настоящими warning-индикаторами
(`--warning: #fbbf24` в dark, `--warning: #d97706` в light).

**2. `webui/css/components.css:96-101`** — defensive `color: inherit` для FA:

```css
.btn-theme-toggle > i.fa-bell,
.btn-theme-toggle > i.fa-moon,
.btn-theme-toggle > i.fa-sun,
.btn-theme-toggle > #themeToggleIcon {
    color: inherit;
}
```

Гарантирует, что даже если Font Awesome добавит внутренние правила для
конкретных иконок, цвет останется серым (`var(--text-secondary)`), а не
съедет в FA-default жёлтый/амбер.

#### Файлы изменены (yellow icons v4)

| Файл | Изменение |
|---|---|
| `webui/css/layout.css` | `.backend-engine-badge.engine-llama_cpp` → `var(--text-secondary)` |
| `webui/css/components.css` | Явный `color: inherit` для `.fa-bell/.fa-moon/.fa-sun/#themeToggleIcon` внутри `.btn-theme-toggle` |

#### Verification

- В sidebar список бэкендов плашка `llama_cpp` теперь серая, не янтарная.
- Theme toggle (moon/sun) и notification bell в шапке — серые, одинаковые с
  остальными кнопками-иконками (density toggle).
- В `webui/dist/` нет устаревших копий этих правил (проверено `findstr`).
- Никаких изменений в JS / HTML / i18n не требуется.

## Known Issues / TODO

1. ~~**RAM fallback reset endpoint** — `POST /api/v1/cppworker/reset-reload-counter` is still TODO.~~
   ✅ **DONE 2026-06-25** (commit включён в `feat(balancer): Rounds 8-15`, `4b4bfbd`).
   - cppworker: `cmd/cppworker/handlers_reset_reload.go` (handleResetReloadCounter) + `router.go` регистрация.
   - balancer proxy: `internal/api/handlers_cppworker_reset_reload.go` (handleResetCppWorkerReloadCounter) + роут в `routes.go:176`.
   - 5 unit-тестов в `cmd/cppworker/handlers_reset_reload_test.go` PASS.
   - Использование: `curl -X POST http://localhost:18081/api/v1/cppworker/reset-reload-counter` (reset всех моделей) или `curl -X POST ... -d '{"model":"gemma-4"}'` (reset одной).
2. **i18n helper scripts** — `tools/fix_i18n_syntax.py`, `tools/collapse_quotes.py`, `tools/scan_i18n_keys.py` are kept for future i18n emergencies. Устаревшие `restore_i18n_quotes*.py` v1-v4, `fix_i18n_syntax.ps1`, `fix_doubled_quotes.py` удалены из `tools/` (2026-06-29 v2 cleanup).
3. ~~**Em-dash compliance** — 7 remaining em-dash violations in CSS comments (icons.css:?, components.css:69, monitor-app.css:73, pages.css:20, responsive.css:2, i18n/ru.js:1, i18n/en.js:1). Treated as low priority; user-facing copy is clean.~~
   ✅ **DONE 2026-07-10** (Phase 7, commit см. `git log --grep="Phase 7"`).
   - 9 em-dash заменены на `-`: components.css (4), data.css (1), layout.css (2), en.js header (1), ru.js header (1).
   - Regression-тест `internal/api/lint_css_i18n_test.go:TestLintCSSAndI18nNoEmDash` (PASS) предотвращает возвращение em-dash в CSS comments и i18n headers.
   - User-facing copy в i18n (P1-P4 help, etc.) сохранён — em-dash там легитимная русская/английская типографика.
4. **rgba() compliance** — ~120 rgba() in CSS (mostly theme tokens, box-shadows in :root, and gradient stops). **PARTIAL FIX 2026-07-10** (Phase 7):
   - 20 `rgba(R,G,B,α)` для accent-derived variants (--accent-soft / --accent-glow-soft / --accent-glow-ring / --accent-glow-shadow) в 5 темах (default dark, light, linear, nvidia, vercel) заменены на `color-mix(in srgb, var(--accent) X%, transparent)`. Теперь auto-track accent color changes.
   - Осталось ~100 rgba(): shadows (rgba(0,0,0,α) — theme-agnostic), white/black bg-alphas (theme-agnostic), gradient stops, drop-shadow() (CSS не поддерживает var() в rgba). Низкий приоритет — оставлено для будущих итераций.
5. **Ollama-only messages** in CppWorker (e.g. `/api/pull` for `ollama` registry) — return HTTP 501 as expected.