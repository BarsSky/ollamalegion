# ENV VARS — single source of truth

**Дата создания**: 2026-09-08 (R60.18 F6)
**Аудит**: [R60.18-env-flags-audit.md](R60.18-env-flags-audit.md)

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
> Подробнее: [R60.18-env-flags-audit.md](R60.18-env-flags-audit.md#архитектурный-вопрос-имеет-ли-смысл-total-streamtimeout)

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
| `LB_OPENAI_AUTO_STREAM` | bool | `false` | feature flag | Высший | OpenAI auto-stream conversion. |
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
