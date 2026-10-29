# ENV VARS — single source of truth

**Дата создания**: 2026-09-08 (R60.18 F6)
**Аудит**: [R60.18-env-flags-audit.md](../archive/rounds/r60.18-env-flags-audit.ru.md)

## Соглашения

1. **Префикс `LB_`** — для переменных балансера. Читаются в `cmd/balancer/*.go` и `internal/balancer/*.go`.
2. **Префикс `CPPWORKER_`** — для переменных cppworker. Читаются в `cmd/cppworker/*.go`.
3. **Префикс `LOG_`** — для логирования (стандарт).
4. **Префикс `OLLAMALEGION_`** — для shared/runtime переменных.

## Приоритет (от высшего к низшему)

| Приоритет | Источник | Когда применяется |
|-----------|----------|-------------------|
| 1 | CLI flag (`--xxx`) | Если явно передан в командной строке |
| 2 | ENV var | Если CLI flag НЕ задан |
| 3 | Config (config.json / per-model profile) | Default |
| 4 | Hardcoded default | Если ничего не задано |

> **Исключение для break-glass**: `LB_STREAMING_NEVER_TIMEOUT=1` имеет ВЫСШИЙ
> приоритет — отключает все streaming таймауты (для incident response / OpenWebUI
> multi-turn scenarios).

> **Архитектурное замечание**: total `streamTimeout` — wrong abstraction для
> LLM streaming. Используйте `streamingIdleTimeout` (hang detection) +
> `firstByteTimeout` (prefill hang) + `n_predict` per-model (length cap).
> Подробнее: [R60.18-env-flags-audit.md](../archive/rounds/r60.18-env-flags-audit.ru.md#архитектурный-вопрос-имеет-ли-смысл-total-streamtimeout)

---

## Balancer ENV vars

| Env var | Type | Default | Override target | Приоритет vs config | Notes |
|---------|------|---------|-----------------|---------------------|-------|
| `LB_API_TOKEN` | string | — | `conf.Auth.Tokens` (полная замена) | Высший | Если задан, заменяет весь config token list + `auth.Enabled=true`. Auth bypass. |
| `LB_AUTH_TOKENS` | CSV | — | `conf.Auth.Tokens` | Высший | Используется ТОЛЬКО если `LB_API_TOKEN` пуст. |
| `LB_ALGORITHM` | string | `resource-aware` | `conf.Balancing.Algorithm` | Высший | Валидируется против whitelist. Invalid → warn + ignore. |
| `LB_DATA_DIR` | path | `/app/data` | `dataDir` | Высший | Volume mount path. |
| `LOG_LEVEL` | string | — | `conf.Logging.Level` | Только если `--log-level` flag НЕ задан | |
| `LB_STREAMING_NEVER_TIMEOUT` | bool | `false` | Все streaming таймауты = 0 | **ВЫСШИЙ** (выше env и config) | `1/true/yes` → disable. Для OpenWebUI multi-turn. |
| `LB_STREAMING_IDLE_TIMEOUT_SEC` | int | — | `streamingIdleTimeout` (per-model + global) | Выше config (break-glass) | **R60.18 F1**: до этого был phantom — документирован, но не реализован. Теперь работает. |
| `LB_AUTO_CONTINUE_ON_TRUNCATION` | bool | `false` | feature flag | Высший | **R60.21**: auto-continue при truncation. Детектирует unclosed code block / mid-line cutoff в `done_reason:stop` ответах и автоматически посылает "Continue from where you left off" запрос к upstream. Default: OFF (opt-in). |
| `LB_AUTO_CONTINUE_MAX_TOKENS` | int | `1024` | `max_tokens` / `num_predict` для continue запроса | — | Лимит на размер continuation. Если continuation тоже truncated — обрезаем. |
| `LB_OPENAI_AUTO_STREAM` | bool | `false` | feature flag | Высший | OpenAI auto-stream conversion. |
| `LB_STREAM_MAX_LINE_BYTES` | int | `8388608` (8 МиБ) | лимит строки SSE-сканера (все стриминговые пути) | Высший | **R259/R264**: один лимит на все пути (hijack, legacy, transport, non-stream, self-proxy диалога, anthropic/responses/auto-stream). Строка больше лимита — это НАШ лимит, а не обрыв воркера: клиент получает валидный терминальный кадр, в логе `reason=our_limit`. Значения меньше 64 КиБ и мусор игнорируются. |
| `LB_OPENAI_AUTO_STREAM_MAX_SEC` | int | `300` | предел буферизации не-стрим агентских запросов | Высший | **R258**: не-стрим запрос (клиент ждёт цельный JSON) по-прежнему буферизуется, но не бесконечно. По таймауту клиент получает `auto_stream_timeout` с подсказкой `stream=true`. Клиенты с `Accept: text/event-stream` обслуживаются потоком, а не буфером. |
| `LB_OPENAI_AUTO_STREAM_MAX_BYTES` | int | `16777216` (16 МиБ) | предел размера буфера для не-стрим агентских запросов | Высший | **R258**: то же по размеру — `auto_stream_too_large` вместо молчаливого частичного успеха. |
| `LB_OPENAI_AUTO_STREAM_KEEPALIVE_SEC` | int | `10` | период keepalive, пока копится не-стрим ответ | Высший | **R272**: клиент с stream=false ждёт цельный JSON, и во время долгой генерации балансер не писал в тело НИЧЕГО после заголовков 200 — клиенты с таймаутом на чтение (aiohttp/httpx/undici и большинство самописных) закрывали соединение, пока модель ещё писала (живой дефект на OpenAI-порту 18079). Теперь раз в N секунд уходит пробельный байт; ведущие пробелы перед JSON допустимы по RFC 8259, итоговое тело парсится. `0` выключает; значения ниже 2 с поднимаются до 2 с, мусор = дефолт. |
| `LB_NCTX_GROW_FOR_AGENTS` | bool | `true` | рост окна для агентских ходов в preflight | Высший | **R253**: если клиент не прислал `num_ctx`, окно выводится из оценки промпта (+ резерв на вывод), модель перезагружается под него, и тот же запрос дообслуживается. `0` возвращает прежнее поведение «клиент молчит → NoOp». Потолки (VRAM/GGUF и `LB_NCTX_RELOAD_MAX_N_CTX`) действуют; выше них — объяснённый 413. Требует проброса в compose, иначе в контейнер не доедет. |
| `LB_AGENT_OUTPUT_RESERVE_TOKENS` | int | `4096` | бюджет вывода при выводе окна для агента | Высший | **R253**: сколько токенов зарезервировать под ответ, если клиент не задал `max_tokens`; окно = промпт + этот резерв + 64. |
| `LB_LLAMA_UPSTREAM_CHECK_ENABLED` | bool | `true` | отслеживание последней стабильной версии llama.cpp | Высший | **R269**: фоновая петля по TTL спрашивает GitHub о последнем релизе `LB_LLAMA_UPSTREAM_REPO`, кладёт ответ в writable-том и отвечает «узлы отстают / в актуальном / неизвестно». `0` выключает проверку (закрытый стенд без сети). Без сети прежнее известное значение сохраняется, а ошибка пишется в лог один раз на смену состояния, а не каждый тик. |
| `LB_LLAMA_UPSTREAM_TTL_SEC` | int | `21600` (6 ч) | как часто спрашивать GitHub | Высший | **R269**: до истечения TTL сети нет вовсе; значение читается из env каждый тик, поэтому меняется без пересборки. |
| `LB_LLAMA_UPSTREAM_REPO` | string | `ggml-org/llama.cpp` | чей релиз считать стабильным | Высший | **R269**: `owner/repo` на GitHub. Смена репозитория сразу сбрасывает TTL (закэшированное значение принадлежало прежнему репозиторию). |
| `LB_LLAMA_UPSTREAM_TIMEOUT_SEC` | int | `10` | таймаут одного запроса к upstream | Высший | **R269**: медленная или заблокированная сеть не должна держать петлю; по таймауту состояние становится «последняя проверка не удалась», а панель продолжает показывать прежнюю версию. |
| `LB_NCTX_RELOAD_ENABLED` | bool | — | `cfg.AutoReloadNCtx` | Выше config | |
| `LB_NCTX_RELOAD_MAX_N_CTX` | int | — | `cfg.AutoReloadMaxNCtx` | Выше config | |
| `LB_NCTX_RELOAD_VRAM_SAFETY_FACTOR` | float | — | `cfg.AutoReloadVRAMSafetyFactor` | Выше config | |
| `LB_NCTX_RELOAD_TIMEOUT_SEC` | int | — | `cfg.AutoReloadTimeoutSec` | Выше config | |
| `LB_NCTX_PREFLIGHT_ENABLED` | bool | — | `cfg.PreflightEnabled` | Выше config | |
| `LB_NCTX_PREFLIGHT_ASYNC_RELOAD` | bool | — | `cfg.PreflightAsyncReload` | Выше config | |
| `LB_NCTX_PREFLIGHT_ASYNC_RETRY_AFTER_SEC` | int | — | `cfg.PreflightAsyncRetryAfterSec` | Выше config | |
| `LB_NCTX_PREFLIGHT_MAX_WAIT_SEC` | int | — | `cfg.PreflightMaxWaitSec` | Выше config | |
| `LB_NCTX_PREFLIGHT_WAIT_MULTIPLIER` | int | — | `cfg.PreflightWaitMultiplier` | Выше config | |
| `LB_NCTX_PREFLIGHT_WAIT_BUFFER_SEC` | int | — | `cfg.PreflightWaitBufferSec` | Выше config | |
| `LB_PREFLIGHT_STREAM_DIALOG` | bool | `true` | feature flag | Выше config | Opt-out для тестов. |
| `LB_PREFLIGHT_STREAM_DIALOG_KEEPALIVE_SEC` | int | `5` | `keepaliveInterval` | Выше config | |
| `LB_MODEL_AFFINITY` | bool | — | `conf.Balancing.ModelAffinity` | Выше config (если задан) | **Trap**: пустая строка не "оставить config" — она fallback к config. Чтобы реально переопределить, нужно явно `true` или `false`. |
| `LB_SESSION_STICKINESS` | bool | — | `conf.Balancing.SessionStickiness` | Выше config (если задан) | Тот же trap что `LB_MODEL_AFFINITY`. |
| `LB_MODEL_CATALOG_REFRESH_SEC` | int | `60` | период фонового перечитывания листинга `/api/models/files` каждого llama.cpp-бэкенда | Выше config (только env) | **R93 (2026-10-09)**: было 300 с. Каталог — кэш «что лежит на диске»: по нему выбирается узел для незагруженной модели и по нему WebUI узнаёт, что модели появились/исчезли. Изменение состава логируется (`model catalog changed`) и публикуется в ленту событий WebUI. `0` — фоновое обновление выключено (остаются on-demand refresh и `GET /api/v1/models/catalog?refresh=true`). |
| `LB_CAPACITY_FROM_MODEL_SLOTS` | bool | `false` | вместимость бэкенда считается по фактическим слотам загруженной модели (`max_slots` от cppworker) | Выше config | Opt-in: меняет поведение admission-очереди, поэтому по умолчанию выключено. Если воркер держит больше слотов, чем пропускает балансер, WebUI показывает предупреждение «потенциал параллельности не раскрыт» с конкретными настройками — оператор решает сам. |

### **REMOVED** in R60.18

| Env var | Причина удаления | Когда |
|---------|-------------------|-------|
| `LB_LLAMACPP_STREAM_TIMEOUT_SEC` | Total stream timeout — wrong abstraction. Документирован как "fail-fast", но ломал OpenWebUI multi-turn. | R60.18 F3 |

### **PHANTOM** (документированы, но не реализованы)

| Env var | Где обещан | Статус |
|---------|-----------|--------|
| `LB_STREAMING_IDLE_TIMEOUT_SEC` | `config.bundled.json:_note_streaming`, `streaming.go:322/325` | ✅ Реализован в R60.18 F1 |
| `LB_STREAMING_FIRST_BYTE_TIMEOUT_SEC` | нигде | Не существует (но логично добавить по симметрии — R60.18 backlog) |
| `LB_STREAMING_REQUEST_TIMEOUT_SEC` | нигде | Не существует (но логично добавить) |

---

## Cppworker ENV vars

| Env var | Type | Default | Override target | Приоритет vs CLI flag | Notes |
|---------|------|---------|-----------------|------------------------|-------|
| `CPPWORKER_PORT` | int | `18092` | `--port` flag | Env проигрывает flag | |
| `CPPWORKER_AUTO_KV_CACHE` | bool | `true` | `autoKVCacheEnabled` | Env проигрывает `--auto-kv-cache` flag | |
| `CPPWORKER_CPU_ONLY_MAX_N_CTX` | int | `65536` | `cpuOnlyMaxNCtx` | Env проигрывает `--cpu-only-max-n-ctx` flag | |
| `CPPWORKER_RAM_FALLBACK_N_CTX` | bool | — | `*ramFallbackNCtx` | **R60.18 F4**: env применяется только если CLI flag НЕ задан (одинаково в startup и reload path) | До F4: silent state divergence на /config/reload |
| `CPPWORKER_RAM_FALLBACK_GPU_LAYERS` | int | — | `*ramFallbackGpuLayers` | Same as above | |
| `CPPWORKER_RAM_FALLBACK_MAX_N_CTX` | int | `4096` | `*ramFallbackMaxNCtx` | Same as above | Min 512. |
| `CPPWORKER_RAM_FALLBACK_ALLOW_TOOLS` | bool | `false` | `*ramFallbackAllowTools` | Env проигрывает flag | |
| `CPPWORKER_AUTO_OFFLOAD` | bool | — | `*autoOffload` | Env проигрывает flag | |
| `CPPWORKER_AUTO_TUNE_NCTX` | bool | — | `*autoTuneNCtx` | Env проигрывает flag | |
| `CPPWORKER_AUTO_TUNE_NCTX_ON_LOAD` | bool | — | `autoTuneNCtxOnLoadEnabled` | (init-time) | |
| `CPPWORKER_WRITE_TIMEOUT` | duration | `30s` | `writeTimeout` | Env проигрывает flag | |
| `CPPWORKER_TENSOR_SPLIT` | string | — | `tensorSplit` | (init-time) | |
| `CPPWORKER_SPLIT_MODE` | int | — | `splitMode` | (init-time) | |
| `CPPWORKER_RPC_SERVERS` | string | — | `cfg.RPCServers` | (init-time) | R101: `host:port,host:port` — узлы раскладки через llama.cpp RPC. Список принимается **целиком или отвергается** (частично принятый означал бы модель, разрезанную не так, как думал оператор); ошибка называет позицию и причину. Максимум 16 узлов. **Транспорт в текущей сборке не подключён** (C-мост не прокидывает `rpc_servers` в устройства llama.cpp) — включённый RPC даёт явное предупреждение в лог, а не тихую настройку |
| `CPPWORKER_RPC_ENABLE` | bool | `false` | `cfg.RPCEnabled` | (init-time) | R101: запрошена раскладка. Непустой `CPPWORKER_RPC_SERVERS` включает её и сам |
| `CPPWORKER_MAIN_GPU` | int | `0` | `cfg.DefaultMainGPU` | (init-time) | R101: индекс главного GPU (>0 — не первая карта) |
| `CPPWORKER_LLAMA_BUILD` | string | — | `cfg.LlamaBuild` | (init-time) | R101: версия/коммит сборки llama.cpp. Узел сообщает её балансеру при регистрации (`types.Backend.BuildInfo`), балансер сравнивает у узлов одной раскладки: протокол RPC версионно-чувствителен, и расхождение видно в `POST /api/v1/placement/validate`, а не таймаутом загрузки. Не задано — проверка честно помечается неподтверждённой (`version_unknown`) |
| `CPPWORKER_GPU_LAYERS` | int | — | `currentConfig.DefaultGPULayers` | (handlers path) | |
| `CPPWORKER_CTX_SIZE` | int | — | `currentConfig.DefaultCtxSize` | (handlers path) | |
| `CPPWORKER_BATCH_SIZE` | int | — | `currentConfig.DefaultBatchSize` | (handlers path) | |
| `CPPWORKER_FLASH_ATTN_TYPE` | int | — | `currentConfig.DefaultFlashAttnType` | (handlers path) | |
| `CPPWORKER_NUMA` | bool | — | `currentConfig.DefaultNUMA` | (handlers path) | |
| `CPPWORKER_USE_MMAP` | bool | — | `currentConfig.DefaultUseMmap` | (handlers path) | |
| `CPPWORKER_FALLBACK_ESTIMATED_LAYERS` | int | — | `fallbackEstimatedLayers` | (init-time) | |
| `CPPWORKER_FALLBACK_KV_RESERVE_MB` | int | — | `fallbackKVReserveMB` | (init-time) | |
| `CPPWORKER_FALLBACK_OVERHEAD_MB` | int | — | `fallbackOverheadMB` | (init-time) | |
| `CPPWORKER_FALLBACK_SAFETY_FACTOR` | float | — | `fallbackSafetyFactor` | (init-time) | |
| `CPPWORKER_REASONING_ARCHS` | string | — | (reasoning content parser) | (init-time) | |
| `CPPWORKER_DEFAULT_N_PREDICT_REASONING` | int | — | (reasoning n_predict default) | (init-time) | |
| `CPPWORKER_VRAM_BYTES` | int | — | (vram detection override) | (init-time) | For testing on machines without GPU. |
| `CPPWORKER_FREE_VRAM_BYTES` | int | — | (free vram override) | (init-time) | For testing. |
| `CPPWORKER_AVAILABLE_RAM_BYTES` | int | — | (available RAM override) | (init-time) | For testing. |
| `CPPWORKER_DEFAULTS_PATH` | path | — | (defaults file path) | (init-time) | |
| `CPPWORKER_BALANCER_URL` | string | — | (register with balancer URL) | (init-time) | |
| `CPPWORKER_BALANCER_TOKEN` | string | — | (register auth token) | (init-time) | Falls back to `LB_API_TOKEN`. |
| `CPPWORKER_ADVERTISE_HOST` | string | — | (host in registration) | (init-time) | |
| `CPPWORKER_ADVERTISE_PORT` | int | — | (port in registration) | (init-time) | |
| `CPPWORKER_REGISTER_NAME` | string | — | (backend name) | (init-time) | |
| `CPPWORKER_REGISTER_RETRY_INTERVAL` | duration | `30s` | (registration retry) | (init-time) | |
| `CPPWORKER_REGISTER_HEARTBEAT` | duration | `60s` | (heartbeat interval) | (init-time) | |
| `CPPWORKER_REGISTER_MAX_RETRIES` | int | `0` | (registration retry count) | (init-time) | |
| `CPPWORKER_REGISTER_GPU_MODE` | string | `auto` | (GPU mode for registration) | (init-time) | |
| `CPPWORKER_REGISTER_DISABLE` | bool | `false` | (disable balancer registration) | (init-time) | |
| `OLLAMALEGION_HEARTBEAT_MS` | int | `15000` | (heartbeat interval) | (init-time) | |
| `API_TOKEN` | string | — | (auth middleware token) | (init-time) | Primary auth token. |
| `CPPWORKER_API_TOKEN` | string | — | (auth middleware token) | (init-time) | Fallback auth token. |
| `BALANCER_API_TOKEN` | string | — | (auth middleware token) | (init-time) | Fallback auth token. |
| `BALANCER_URL` | string | — | (info endpoint display) | (init-time) | Display only. |
| `NODE_NAME` | string | — | (k8s node name, display) | (init-time) | Display only. |
| `NODE_LABELS` | string | — | (k8s node labels, display) | (init-time) | Display only. |
| `CPPWORKER_MODELS_RESCAN_SEC` | int | `30` | период фоновой проверки каталога моделей на диске | (init-time) | **R93 (2026-10-09)**: живой инвентарь моделей. Воркер сам сверяет каталог с диском и пишет в лог, что изменилось (`added`/`removed`/`resized`); `/api/models/files` пересканирует каталог по этому TTL, `POST /api/models/refresh` — немедленно. Так удаление/добавление `.gguf` (в т.ч. вне контейнера) видно без перезапуска. `0` — фоновая проверка выключена. Недоступный каталог (удалённый bind-mount) больше не показывает старый список: ответ содержит `count=0` и `dirError`. |
| `CPPWORKER_LOAD_STALL_SEC` | int | `300` | watchdog загрузки модели (сек) | (init-time) | **R231 (2026-10-24)**: если загрузка не двигается N секунд, воркер печатает стек горутин и отменяет её (`model load cancelled`). 300 с достаточно для моделей до ~8–10 ГБ; **для 15+ ГБ моделей на медленном диске ставьте `1200`** — иначе загрузка обрывается на середине (живой замер: 15.3-ГБ GGUF грузился ~280–480 с, дефолт его убивал). Не путать с `CPPWORKER_WRITE_TIMEOUT` (HTTP-запись ответа). |

---

## WebUI ENV vars

| Env var | Type | Default | Notes |
|---------|------|---------|-------|
| `WEBUI_DEMO_MODE` | bool | `0` | **R232 (2026-10-24)**: демо-режим панели. `1` — WebUI показывает синтетический сценарий (клиенты → балансер → очередь → донор RPC → AutoTune), вход не требуется (гость `viewer`), все `/api/`-запросы перехватываются в браузере (реальный балансер не опрашивается), мутации запрещены (`403 read-only`). Включается **только** этой переменной при старте контейнера; переключателя в UI нет. Подробности: [webui-demo.md](../features/webui-demo.ru.md). |
| `OLLAMALEGION_LOG_LANG` | string | `ru` | **R234**: язык логов и ошибок всех Go-процессов: `ru` / `en` / `auto` (из `LANG`/`LC_ALL`). Приоритет выше `logging.language` в конфиге; переключение через WebUI пишет в конфиг. См. [docs/development/i18n-logs.ru.md](../development/i18n-logs.ru.md). |
| `OLLAMALEGION_LOG_LANG_SYNC_SEC` | int | `60` | **R234**: как часто воркеры (`cppworker`, `agent`) спрашивают язык у балансера (`GET /api/v1/i18n`); минимум 5. |
| `LB_LOG_LANG` / `CPPWORKER_LOG_LANG` / `AGENT_LOG_LANG` | string | — | **R234**: точечные переопределения языка для отдельного контейнера. |
| `API_HOST` / `API_PORT` | string/int | `loadbalancer` / `18081` | Куда nginx панели проксирует `/api/` (admin-плоскость балансера). |
| `LB_PORT` | int | `18080` | Прокси-поверхность балансера для `/v1/`. |
| `CPPWORKER_HOST` / `CPPWORKER_PORT` | string/int | `cppworker-gpu` / `18092` | Поверхность воркера для `/api/worker`. |
| `REFRESH_INTERVAL` | int (мс) | `5000` | Интервал автообновления данных в UI. |

---

## Конфигурационные файлы (dead field, удалено в R60.18)

| Field | Файл | Причина удаления |
|-------|------|------------------|
| `streamingMaxDuration` | `pkg/types/balancing.go:68` (тип), 3 config файла | Документирован, но НИКОГДА не читался. Silent no-op для операторов. R60.18 F2. |

---

## Per-model profile (РЕКОМЕНДОВАННЫЙ путь кастомизации)

Вместо env vars для per-model настроек используйте `llamaCppModelProfiles`
в config.json. Это даёт:

- **Структурированность** — JSON, не .env файлы
- **Hot-reload** через WebUI без перезапуска balancer
- **Версионируемость** — все изменения в git
- **Документированные поля** — типизированные, не нужно гадать

```json
{
  "llamaCppModelProfiles": {
    "Qwen3-Instruct-2507-q4km": {
      "contextLength": 32768,
      "firstByteTimeoutSec": 900,
      "streamingIdleTimeoutSec": 600,
      "nPredict": 4096
    }
  }
}
```

Env vars оставлены только для:
- **Break-glass** в production (incident response)
- **Operator override** без правки config.json (например, `LB_STREAMING_NEVER_TIMEOUT=1`)

---

## Миграция с R60.17 на R60.18

Если вы обновляетесь с R60.17:

1. **Удалить** `LB_LLAMACPP_STREAM_TIMEOUT_SEC` из `.env` (если остался) — код больше не читает этот var, но R60.17 уже выключил его в `.env.bundled-with-agent`.
2. **Опционально**: установить `LB_STREAMING_IDLE_TIMEOUT_SEC` в `.env` если хотите break-glass override idle timeout без правки config.json.
3. **Проверить** config.json на наличие `streamingMaxDuration` — поле удалено, JSON парсер silently проигнорирует (no error).

Никаких breaking changes для существующих конфигов.

---

## Теги образов (deployments/.env) — R83

**R83 (2026-09-25)**: теги образов продового стека
(`docker-compose.cppworker-bundled-with-agent.yml`) вынесены в переменные. Compose
подставляет `${VAR:-default}` **из `deployments/.env`** — `env_file`
(`.env.bundled-with-agent`) на подстановку НЕ влияет, там те же переменные
дублируются только для синхронности.

| Переменная | Образ | Значение по умолчанию в compose |
|---|---|---|
| `BALANCER_TAG` | `ollama-legion/balancer` | `r82-submodule-v19` |
| `CPPWORKER_GPU_TAG` | `ollama-legion/cppworker:gpu-<значение>` | `86-r60.4-webui-meta` |
| `AGENT_TAG` | `ollama-legion/agent` | `cppworker-bundled-r41-agent-x-api-token` |
| `WEBUI_TAG` | `ollama-legion/webui` | `r77-submodule-v2` |

> **Внимание**: значение из `.env` ПЕРЕОПРЕДЕЛЯЕТ default из compose. Если строка
> `VAR=` в `.env` есть — compose возьмёт её, а не default.

### Выпуск

```powershell
# один выпуск на все четыре компонента: сборка + простановка тегов + манифест
powershell -File scripts/release-all.ps1 -Tag r83-submodule-v1
# только перепин тегов (без сборки и деплоя)
powershell -File scripts/release-all.ps1 -Tag r83 -SkipBuild -NoDeploy
# проверка согласованности (docker не нужен, запускается в CI)
powershell -File scripts/check-image-tags.ps1
```

`scripts/release-all.ps1` пишет `deployments/release-manifest.json`
(компонент → тег + image id + git commit). Откат: вернуть теги в
`deployments/.env` и выполнить `docker compose up -d`.

### Почему не `latest` везде

`ollama-legion/cppworker` содержит **взаимоисключающие** варианты
(`cpu` / `stub` / `gpu-86` / `gpu-arch_all` / `gpu-llamacpp`), а `latest` — один
тег на репозиторий: его перезапишет последняя сборка. Поэтому у cppworker алиас
вариантный (`latest-gpu-86`), а plain `latest` вешают только те компоненты, у
которых вариантов нет (balancer, agent, webui). Именно из-за этого ограничения в
репозитории исторически появились `latest-cpu` и `latest-gpu`.

---

## n_ctx на 24 GB (A10) — рабочий режим и границы (R83)

Живая проверка на **NVIDIA A10 (24 GB)** с `Qwen3.8-27B-UD-Q4_K_M`
(15.3 ГБ, 64 слоя, `gguf_max_context=262144`):

| n_ctx | Что делает стратегия | Результат |
|---|---|---|
| **32768** | `exact_fit` — все слои в VRAM | **работает без ошибок** (рекомендуемый режим) |
| 65536 | `partial_offload` — часть слоёв уходит в RAM (mmap) | заметно медленнее, клиент упирается в таймаут |
| 131072 | ещё дальше / `cpu_only` | не работает |

**Почему это не «примерно не влезло», а смена режима.** Граница проходит ровно
по `safeVRAM = 0.85 × FreeVRAM` в `SelectStrategyWithKV`
(`cmd/cppworker/adaptive_loader.go`): пока `weights + KV + overhead` укладываются —
`exact_fit`; как только нет — `partial_offload`.

### Что выставить

```
CPPWORKER_CTX_SIZE=32768            # дефолт; рабочий режим для 27B Q4_K_M на 24 GB
CPPWORKER_RAM_FALLBACK_MAX_N_CTX=32768   # не больше feasible (см. ниже)
LB_NCTX_RELOAD_MAX_N_CTX=32768           # не больше feasible
```

Проверить фактический предел для модели на конкретном железе:

```bash
cppworker -feasible Qwen3.8-27B-UD-Q4_K_M     # печатает min(VRAM, RAM, GGUF)
curl -s localhost:18092/api/models | jq '.feasible_max_context, .gguf_max_context'
```

### Что теперь защищает от «молча не работает» (R83)

1. **Отказ 422.** Если запрошенный n_ctx выше физического предела (RAM или
   обучающего контекста модели) — загрузка не стартует, клиент получает причину и
   числа. `partial_offload` при этом НЕ запрещён: это легитимный запуск большой
   модели на малом GPU.
2. **Режим виден в `/api/models`:** `max_vram_n_ctx`, `n_ctx_degraded`,
   `n_ctx_headroom`, `feasible_max_context`.
3. **Предупреждение в логе**, если потолок из конфига недостижим на этом железе
   (`CPPWORKER_RAM_FALLBACK_MAX_N_CTX` больше физического предела) — раньше конфиг
   обещал 128000 там, где предел ниже, и это выглядело как «модель сломалась».
4. **Карточка модели в WebUI** показывает `Feasible n_ctx`, `GGUF max n_ctx` и
   предупреждение «partial offload», если фактический n_ctx выше границы VRAM.

> `gguf_max_context` (262144) — это ОБУЧАЮЩИЙ контекст модели, а не
> рекомендация. Ориентироваться нужно на `feasible_max_context`.

## R274 — проброс: каждая переменная, которую читает код, обязана доезжать до контейнера

**Живой дефект (повторился трижды за R250–R272).** Переменная читается кодом
(`os.Getenv`), оператор задаёт её в `deployments/.env` — а она молча не работает,
потому что не проброшена в `deployments/docker-compose.stack.yml`. Каждый раз это
диагностировалось как «переключатель не работает», и время уходило на поиск
ошибки в коде, где её не было. Случаи: `LB_NCTX_GROW_FOR_AGENTS`,
`LB_OPENAI_AUTO_STREAM_KEEPALIVE_SEC`, `LB_RELOAD_DEFER_MAX_SEC`.

**Правило, которое теперь проверяется тестом**
(`internal/balancer/env_passthrough_r274_test.go`):

1. Каждое имя `LB_*`, встречающееся в Go-коде балансера, обязано быть либо
   - в блоке `environment:` службы `loadbalancer` в `docker-compose.stack.yml`, либо
   - описано в `deployments/.env.example`, либо
   - внесено в allowlist теста **с письменной причиной**.
2. Переменная, объявленная в блоке балансера, но не читаемая кодом, помечается
   как мёртвый переключатель или опечатка.

**Две формы проброса — намеренно разные:**

| Форма | Смысл | Где применяется |
|-------|-------|-----------------|
| `- LB_X=${LB_X:-value}` | значение оператора, иначе в контейнер пишется **дефолт из кода** | переключатели поведения, где дефолт должен быть явным и видимым в `docker inspect` |
| `- LB_X` (без значения) | проброс **только если** переменная задана в окружении compose (`deployments/.env`); иначе её в контейнере нет и работает `config.json` / дефолт кода | настройки, дублирующие `config.json` (адрес, порты, очередь, лимиты ресурсов, таймауты, репликация, виртуальные модели, RPC-координатор) |

> **Куда писать значение.** В `deployments/.env` (именно его подставляет compose).
> `.env.bundled-with-agent` — это `env_file` службы: он тоже доезжает до контейнера,
> но блок `environment:` имеет над ним приоритет (см. шапку compose-файла).

**Видимость.** `docker inspect ol-stack-balancer --format '{{json .Config.Env}}'`
показывает, что реально попало в контейнер. Если заданной переменной там нет —
проблема в пробросе, а не в функции.

**Добавляете новую переменную.** Прочитали в коде → добавили в compose (форма по
таблице выше) или в `.env.example` → тест зелёный. Если добавили только в код,
тест упадёт со списком переменных, которые никогда не доедут до контейнера.
