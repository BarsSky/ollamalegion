# ENV VARS — single source of truth

**Created**: 2026-09-08 (R60.18 F6)
**Audit**: [R60.18-env-flags-audit.md](../archive/rounds/r60.18-env-flags-audit.ru.md)

## Conventions

1. **`LB_` prefix** — for balancer variables. Read in `cmd/balancer/*.go` and `internal/balancer/*.go`.
2. **`CPPWORKER_` prefix** — for cppworker variables. Read in `cmd/cppworker/*.go`.
3. **`LOG_` prefix** — for logging (standard).
4. **`OLLAMALEGION_` prefix** — for shared/runtime variables.

## Priority (from highest to lowest)

| Priority | Source | When it applies |
|----------|--------|-----------------|
| 1 | CLI flag (`--xxx`) | If explicitly passed on the command line |
| 2 | ENV var | If the CLI flag is NOT set |
| 3 | Config (config.json / per-model profile) | Default |
| 4 | Hardcoded default | If nothing is set |

> **Break-glass exception**: `LB_STREAMING_NEVER_TIMEOUT=1` has the HIGHEST
> priority — it disables all streaming timeouts (for incident response / OpenWebUI
> multi-turn scenarios).

> **Architectural note**: total `streamTimeout` is a wrong abstraction for
> LLM streaming. Use `streamingIdleTimeout` (hang detection) +
> `firstByteTimeout` (prefill hang) + per-model `n_predict` (length cap).
> Details: [R60.18-env-flags-audit.md](../archive/rounds/r60.18-env-flags-audit.ru.md#архитектурный-вопрос-имеет-ли-смысл-total-streamtimeout)

---

## Balancer ENV vars

| Env var | Type | Default | Override target | Priority vs config | Notes |
|---------|------|---------|-----------------|---------------------|-------|
| `LB_API_TOKEN` | string | — | `conf.Auth.Tokens` (full replacement) | Highest | If set, replaces the whole config token list + `auth.Enabled=true`. Auth bypass. |
| `LB_AUTH_TOKENS` | CSV | — | `conf.Auth.Tokens` | Highest | Used ONLY if `LB_API_TOKEN` is empty. |
| `LB_ALGORITHM` | string | `resource-aware` | `conf.Balancing.Algorithm` | Highest | Validated against a whitelist. Invalid → warn + ignore. |
| `LB_DATA_DIR` | path | `/app/data` | `dataDir` | Highest | Volume mount path. |
| `LOG_LEVEL` | string | — | `conf.Logging.Level` | Only if `--log-level` flag is NOT set | |
| `LB_STREAMING_NEVER_TIMEOUT` | bool | `false` | All streaming timeouts = 0 | **HIGHEST** (above env and config) | `1/true/yes` → disable. For OpenWebUI multi-turn. |
| `LB_STREAMING_IDLE_TIMEOUT_SEC` | int | — | `streamingIdleTimeout` (per-model + global) | Above config (break-glass) | **R60.18 F1**: before this it was a phantom — documented but not implemented. Now it works. |
| `LB_AUTO_CONTINUE_ON_TRUNCATION` | bool | `false` | feature flag | Highest | **R60.21**: auto-continue on truncation. Detects an unclosed code block / mid-line cutoff in `done_reason:stop` responses and automatically sends a "Continue from where you left off" request to upstream. Default: OFF (opt-in). |
| `LB_AUTO_CONTINUE_MAX_TOKENS` | int | `1024` | `max_tokens` / `num_predict` for the continue request | — | Limit on the continuation size. If the continuation is also truncated — we cut it. |
| `LB_OPENAI_AUTO_STREAM` | bool | `false` | feature flag | Highest | OpenAI auto-stream conversion. |
| `LB_STREAM_MAX_LINE_BYTES` | int | `8388608` (8 MiB) | line limit of the SSE scanner (all streaming paths) | Highest | **R259/R264**: one limit for every path (hijack, legacy, transport, non-stream, self-proxy dialog, anthropic/responses/auto-stream). A line above the limit is **our** limit, not an upstream break: the client gets a valid terminal frame and the log says `reason=our_limit`. Values below 64 KiB and garbage are ignored. |
| `LB_OPENAI_AUTO_STREAM_MAX_SEC` | int | `300` | max buffering time for non-stream agent requests | Highest | **R258**: a non-stream request that expects a whole JSON body is still buffered, but not forever. On timeout the client gets `auto_stream_timeout` with a hint to use `stream=true`. Clients sending `Accept: text/event-stream` are streamed instead of buffered. |
| `LB_OPENAI_AUTO_STREAM_MAX_BYTES` | int | `16777216` (16 MiB) | max buffered size for non-stream agent requests | Highest | **R258**: same idea for size — `auto_stream_too_large` instead of silent partial success. |
| `LB_OPENAI_AUTO_STREAM_KEEPALIVE_SEC` | int | `10` | keepalive period while a non-stream answer is buffered | Highest | **R272**: a non-stream client waits for a whole JSON body, and during a long generation the balancer wrote **nothing** after the 200 headers — clients with a read timeout (aiohttp/httpx/undici, most hand-written ones) closed the connection while the model was still writing (live defect on the OpenAI port 18079). Now a whitespace byte is written every N seconds; leading whitespace before JSON is valid per RFC 8259, so the final body still parses. `0` disables; values below 2 s are raised to 2 s, garbage falls back to the default. |
| `LB_NCTX_GROW_FOR_AGENTS` | bool | `true` | agent window growth in preflight | Highest | **R253**: if the client sends no `num_ctx`, the window is derived from the prompt estimate (+ output reserve) and the model is reloaded to fit it, then the same request is served. `0` returns the old "silent client → NoOp" behaviour. Ceilings (VRAM/GGUF and `LB_NCTX_RELOAD_MAX_N_CTX`) still apply; a request above them gets an explained 413. Needs the compose pass-through to reach the container. |
| `LB_AGENT_OUTPUT_RESERVE_TOKENS` | int | `4096` | output budget used when deriving an agent window | Highest | **R253**: how many tokens to reserve for the answer when the client did not send `max_tokens`; the window is prompt + this reserve + 64. |
| `LB_LLAMA_UPSTREAM_CHECK_ENABLED` | bool | `true` | tracks the latest stable llama.cpp release | Highest | **R269**: a background loop (TTL-based) asks GitHub for the latest release of `LB_LLAMA_UPSTREAM_REPO`, caches it in the writable volume and answers "nodes are behind / up to date / unknown". `0` disables the check (closed stand without internet). Without network the previous known value is kept and the failure is logged once per state change, not per tick. |
| `LB_LLAMA_UPSTREAM_TTL_SEC` | int | `21600` (6 h) | how often to ask GitHub | Highest | **R269**: the check is cheap (no network until TTL expires); the value comes from env each tick, so it can be changed without rebuilding. |
| `LB_LLAMA_UPSTREAM_REPO` | string | `ggml-org/llama.cpp` | whose release counts as stable | Highest | **R269**: `owner/repo` on GitHub. Changing the repo invalidates the TTL immediately (the cached value belongs to the old repo). |
| `LB_LLAMA_UPSTREAM_TIMEOUT_SEC` | int | `10` | timeout of one upstream request | Highest | **R269**: a slow or blocked network must not hold the loop; on timeout the state becomes "last check failed" and the UI simply keeps the previous version. |
| `LB_NCTX_RELOAD_ENABLED` | bool | — | `cfg.AutoReloadNCtx` | Above config | |
| `LB_NCTX_RELOAD_MAX_N_CTX` | int | — | `cfg.AutoReloadMaxNCtx` | Above config | |
| `LB_NCTX_RELOAD_VRAM_SAFETY_FACTOR` | float | — | `cfg.AutoReloadVRAMSafetyFactor` | Above config | |
| `LB_NCTX_RELOAD_TIMEOUT_SEC` | int | — | `cfg.AutoReloadTimeoutSec` | Above config | |
| `LB_NCTX_PREFLIGHT_ENABLED` | bool | — | `cfg.PreflightEnabled` | Above config | |
| `LB_NCTX_PREFLIGHT_ASYNC_RELOAD` | bool | — | `cfg.PreflightAsyncReload` | Above config | |
| `LB_NCTX_PREFLIGHT_ASYNC_RETRY_AFTER_SEC` | int | — | `cfg.PreflightAsyncRetryAfterSec` | Above config | |
| `LB_NCTX_PREFLIGHT_MAX_WAIT_SEC` | int | — | `cfg.PreflightMaxWaitSec` | Above config | |
| `LB_NCTX_PREFLIGHT_WAIT_MULTIPLIER` | int | — | `cfg.PreflightWaitMultiplier` | Above config | |
| `LB_NCTX_PREFLIGHT_WAIT_BUFFER_SEC` | int | — | `cfg.PreflightWaitBufferSec` | Above config | |
| `LB_PREFLIGHT_STREAM_DIALOG` | bool | `true` | feature flag | Above config | Opt-out for tests. |
| `LB_PREFLIGHT_STREAM_DIALOG_KEEPALIVE_SEC` | int | `5` | `keepaliveInterval` | Above config | |
| `LB_MODEL_AFFINITY` | bool | — | `conf.Balancing.ModelAffinity` | Above config (if set) | **Trap**: an empty string does not mean "keep config" — it falls back to config. To really override, you must set `true` or `false` explicitly. |
| `LB_SESSION_STICKINESS` | bool | — | `conf.Balancing.SessionStickiness` | Above config (if set) | Same trap as `LB_MODEL_AFFINITY`. |
| `LB_MODEL_CATALOG_REFRESH_SEC` | int | `60` | period of the background re-read of the `/api/models/files` listing of each llama.cpp backend | Above config (env only) | **R93 (2026-10-09)**: it used to be 300 s. The catalog is a cache of "what lies on disk": it is used to pick a node for a model that is not loaded yet, and from it the WebUI learns that models have appeared/disappeared. A change of composition is logged (`model catalog changed`) and published to the WebUI event feed. `0` — background refresh is disabled (on-demand refresh and `GET /api/v1/models/catalog?refresh=true` remain). |
| `LB_CAPACITY_FROM_MODEL_SLOTS` | bool | `false` | backend capacity is computed from the actual slots of the loaded model (`max_slots` from cppworker) | Above config | Opt-in: it changes the behavior of the admission queue, so it is off by default. If a worker holds more slots than the balancer admits, the WebUI shows the warning "parallelism potential is not realized" with the concrete settings — the operator decides. |

### **REMOVED** in R60.18

| Env var | Removal reason | When |
|---------|----------------|------|
| `LB_LLAMACPP_STREAM_TIMEOUT_SEC` | Total stream timeout — wrong abstraction. Documented as "fail-fast", but it broke OpenWebUI multi-turn. | R60.18 F3 |

### **PHANTOM** (documented but not implemented)

| Env var | Where promised | Status |
|---------|----------------|--------|
| `LB_STREAMING_IDLE_TIMEOUT_SEC` | `config.bundled.json:_note_streaming`, `streaming.go:322/325` | ✅ Implemented in R60.18 F1 |
| `LB_STREAMING_FIRST_BYTE_TIMEOUT_SEC` | nowhere | Does not exist (but it would be logical to add it by symmetry — R60.18 backlog) |
| `LB_STREAMING_REQUEST_TIMEOUT_SEC` | nowhere | Does not exist (but it would be logical to add it) |

---

## Cppworker ENV vars

| Env var | Type | Default | Override target | Priority vs CLI flag | Notes |
|---------|------|---------|-----------------|------------------------|-------|
| `CPPWORKER_PORT` | int | `18092` | `--port` flag | Env loses to flag | |
| `CPPWORKER_AUTO_KV_CACHE` | bool | `true` | `autoKVCacheEnabled` | Env loses to `--auto-kv-cache` flag | |
| `CPPWORKER_CPU_ONLY_MAX_N_CTX` | int | `65536` | `cpuOnlyMaxNCtx` | Env loses to `--cpu-only-max-n-ctx` flag | |
| `CPPWORKER_RAM_FALLBACK_N_CTX` | bool | — | `*ramFallbackNCtx` | **R60.18 F4**: env applies only if the CLI flag is NOT set (the same in the startup and reload paths) | Before F4: silent state divergence on /config/reload |
| `CPPWORKER_RAM_FALLBACK_GPU_LAYERS` | int | — | `*ramFallbackGpuLayers` | Same as above | |
| `CPPWORKER_RAM_FALLBACK_MAX_N_CTX` | int | `4096` | `*ramFallbackMaxNCtx` | Same as above | Min 512. |
| `CPPWORKER_RAM_FALLBACK_ALLOW_TOOLS` | bool | `false` | `*ramFallbackAllowTools` | Env loses to flag | |
| `CPPWORKER_AUTO_OFFLOAD` | bool | — | `*autoOffload` | Env loses to flag | |
| `CPPWORKER_AUTO_TUNE_NCTX` | bool | — | `*autoTuneNCtx` | Env loses to flag | |
| `CPPWORKER_AUTO_TUNE_NCTX_ON_LOAD` | bool | — | `autoTuneNCtxOnLoadEnabled` | (init-time) | |
| `CPPWORKER_WRITE_TIMEOUT` | duration | `30s` | `writeTimeout` | Env loses to flag | |
| `CPPWORKER_TENSOR_SPLIT` | string | — | `tensorSplit` | (init-time) | |
| `CPPWORKER_SPLIT_MODE` | int | — | `splitMode` | (init-time) | |
| `CPPWORKER_RPC_SERVERS` | string | — | `cfg.RPCServers` | (init-time) | R101: `host:port,host:port` — layout nodes over llama.cpp RPC. The list is accepted **whole or rejected** (a partially accepted one would mean a model cut differently from what the operator thought); the error names the position and the reason. Maximum 16 nodes. **The transport is not wired up in the current build** (the C bridge does not pass `rpc_servers` into llama.cpp devices) — enabled RPC produces an explicit warning in the log instead of a silent setting |
| `CPPWORKER_RPC_ENABLE` | bool | `false` | `cfg.RPCEnabled` | (init-time) | R101: layout requested. A non-empty `CPPWORKER_RPC_SERVERS` enables it by itself |
| `CPPWORKER_MAIN_GPU` | int | `0` | `cfg.DefaultMainGPU` | (init-time) | R101: index of the main GPU (>0 — not the first card) |
| `CPPWORKER_LLAMA_BUILD` | string | — | `cfg.LlamaBuild` | (init-time) | R101: version/commit of the llama.cpp build. The node reports it to the balancer on registration (`types.Backend.BuildInfo`), the balancer compares it across the nodes of one layout: the RPC protocol is version-sensitive, and a mismatch is visible in `POST /api/v1/placement/validate` instead of showing up as a load timeout. Not set — the check is honestly marked unconfirmed (`version_unknown`) |
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
| `CPPWORKER_MODELS_RESCAN_SEC` | int | `30` | period of the background check of the model catalog on disk | (init-time) | **R93 (2026-10-09)**: a live model inventory. The worker itself reconciles the catalog with the disk and writes what changed into the log (`added`/`removed`/`resized`); `/api/models/files` rescans the catalog according to this TTL, `POST /api/models/refresh` — immediately. Thus the removal/addition of a `.gguf` (including outside the container) is visible without a restart. `0` — the background check is disabled. An unreachable catalog (a removed bind-mount) no longer shows the old list: the response contains `count=0` and `dirError`. |
| `CPPWORKER_LOAD_STALL_SEC` | int | `300` | model load watchdog (sec) | (init-time) | **R231 (2026-10-24)**: if loading does not move for N seconds, the worker prints a goroutine stack and cancels it (`model load cancelled`). 300 s is enough for models up to ~8–10 GB; **for 15+ GB models on a slow disk set `1200`** — otherwise loading is cut off halfway (a live measurement: a 15.3 GB GGUF was loading for ~280–480 s, the default was killing it). Do not confuse it with `CPPWORKER_WRITE_TIMEOUT` (the HTTP write of the response). |

---

## WebUI ENV vars

| Env var | Type | Default | Notes |
|---------|------|---------|-------|
| `WEBUI_DEMO_MODE` | bool | `0` | **R232 (2026-10-24)**: panel demo mode. `1` — the WebUI shows a synthetic scenario (clients → balancer → queue → RPC donor → AutoTune), no login is required (guest `viewer`), all `/api/` requests are intercepted in the browser (the real balancer is not polled), mutations are forbidden (`403 read-only`). It is enabled **only** by this variable at container start; there is no toggle in the UI. Details: [webui-demo.md](../features/webui-demo.md). |
| `OLLAMALEGION_LOG_LANG` | string | `ru` | **R234 (2026-10-24)**: language of logs and error messages of every Go process: `ru` / `en` / `auto` (from `LANG`/`LC_ALL`). Takes priority over `logging.language` in the config; switching it in the WebUI writes that config field. See [docs/development/i18n-logs.md](../development/i18n-logs.md). |
| `OLLAMALEGION_LOG_LANG_SYNC_SEC` | int | `60` | **R234**: how often workers (`cppworker`, `agent`) ask the balancer for the language (`GET /api/v1/i18n`); minimum 5. |
| `LB_LOG_LANG` / `CPPWORKER_LOG_LANG` / `AGENT_LOG_LANG` | string | - | **R234**: per-container overrides of the log language. |
| `API_HOST` / `API_PORT` | string/int | `loadbalancer` / `18081` | Where the panel's nginx proxies `/api/` (the balancer admin plane). |
| `LB_PORT` | int | `18080` | The proxy surface of the balancer for `/v1/`. |
| `CPPWORKER_HOST` / `CPPWORKER_PORT` | string/int | `cppworker-gpu` / `18092` | The worker surface for `/api/worker`. |
| `REFRESH_INTERVAL` | int (ms) | `5000` | Interval of automatic data refresh in the UI. |

---

## Auth / WebUI login (R270, R275)

| Env var | Type | Default | Notes |
|---------|------|---------|-------|
| `LB_ADMIN_USER` / `LB_ADMIN_PASSWORD` | string | - | **R270**: WebUI administrator. Read **only** by the balancer container. If the entry does not exist yet, it is created; if it exists with a different password, the password is updated (R203). **R275**: when the entry already exists with the *same* password, this counts as "env applied" too (`bootstrap.source=env+file`, `envApplied=true`) — previously that case reported `source=file`, which made the operator hunt for a cause that did not exist. |
| `LB_ADMIN_ENV_SYNC` | bool | `1` | **R275**: whether the env credentials may be applied to an **already existing** users file (password update / extra administrator). `0` = do not touch existing users; creating the first administrator on an empty store still happens, otherwise an isolated stand would have nobody to log in. |
| `LB_USERS_FILE` | path | `/app/data/users.json` | Users file; must be on a **writable** volume, otherwise the administrator lives in memory only (`bootstrap.persisted=false`). |
| `LB_SESSIONS_FILE` | path | next to `users.json` | Login sessions; survive a container restart. |

Diagnostics (no secrets): `GET /api/v1/auth/me` → `bootstrap{source, envApplied, persisted, writable, path, users, reason}`.
Offline login guide: [webui-login-offline.md](webui-login-offline.md).

---

## Configuration files (dead field, removed in R60.18)

| Field | File | Removal reason |
|-------|------|----------------|
| `streamingMaxDuration` | `pkg/types/balancing.go:68` (type), 3 config files | Documented but NEVER read. A silent no-op for operators. R60.18 F2. |

---

## Per-model profile (RECOMMENDED customization path)

Instead of env vars for per-model settings, use `llamaCppModelProfiles`
in config.json. This gives:

- **Structure** — JSON, not .env files
- **Hot-reload** via the WebUI without restarting the balancer
- **Versionability** — all changes in git
- **Documented fields** — typed, no need to guess

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

Env vars are kept only for:
- **Break-glass** in production (incident response)
- **Operator override** without editing config.json (for example, `LB_STREAMING_NEVER_TIMEOUT=1`)

---

## Migration from R60.17 to R60.18

If you are upgrading from R60.17:

1. **Remove** `LB_LLAMACPP_STREAM_TIMEOUT_SEC` from `.env` (if it is still there) — the code no longer reads this var, but R60.17 already disabled it in `.env.bundled-with-agent`.
2. **Optionally**: set `LB_STREAMING_IDLE_TIMEOUT_SEC` in `.env` if you want a break-glass override of the idle timeout without editing config.json.
3. **Check** config.json for `streamingMaxDuration` — the field is removed, the JSON parser will silently ignore it (no error).

No breaking changes for existing configs.

---

## Image tags (deployments/.env) — R83

**R83 (2026-09-25)**: the image tags of the production stack
(`docker-compose.cppworker-bundled-with-agent.yml`) have been moved into variables. Compose
substitutes `${VAR:-default}` **from `deployments/.env`** — `env_file`
(`.env.bundled-with-agent`) does NOT affect substitution, the same variables are
duplicated there only for sync.

| Variable | Image | Default value in compose |
|---|---|---|
| `BALANCER_TAG` | `ollama-legion/balancer` | `r82-submodule-v19` |
| `CPPWORKER_GPU_TAG` | `ollama-legion/cppworker:gpu-<value>` | `86-r60.4-webui-meta` |
| `AGENT_TAG` | `ollama-legion/agent` | `cppworker-bundled-r41-agent-x-api-token` |
| `WEBUI_TAG` | `ollama-legion/webui` | `r77-submodule-v2` |

> **Attention**: a value from `.env` OVERRIDES the default from compose. If the line
> `VAR=` exists in `.env`, compose will take it and not the default.

### Release

```powershell
# one release for all four components: build + tagging + manifest
powershell -File scripts/release-all.ps1 -Tag r83-submodule-v1
# re-pin the tags only (no build and no deploy)
powershell -File scripts/release-all.ps1 -Tag r83 -SkipBuild -NoDeploy
# consistency check (docker is not needed, runs in CI)
powershell -File scripts/check-image-tags.ps1
```

`scripts/release-all.ps1` writes `deployments/release-manifest.json`
(component → tag + image id + git commit). Rollback: revert the tags in
`deployments/.env` and run `docker compose up -d`.

### Why not `latest` everywhere

`ollama-legion/cppworker` contains **mutually exclusive** variants
(`cpu` / `stub` / `gpu-86` / `gpu-arch_all` / `gpu-llamacpp`), while `latest` is a single
tag per repository: the last build overwrites it. That is why cppworker has a
variant alias (`latest-gpu-86`), and a plain `latest` is attached only to the components
that have no variants (balancer, agent, webui). It is precisely because of this limitation that
`latest-cpu` and `latest-gpu` historically appeared in the repository.

---

## n_ctx on 24 GB (A10) — working mode and limits (R83)

A live check on **NVIDIA A10 (24 GB)** with `Qwen3.8-27B-UD-Q4_K_M`
(15.3 GB, 64 layers, `gguf_max_context=262144`):

| n_ctx | What the strategy does | Result |
|---|---|---|
| **32768** | `exact_fit` — all layers in VRAM | **works without errors** (recommended mode) |
| 65536 | `partial_offload` — some layers go to RAM (mmap) | noticeably slower, the client hits the timeout |
| 131072 | even further / `cpu_only` | does not work |

**Why this is not "it roughly did not fit" but a mode switch.** The boundary runs exactly
along `safeVRAM = 0.85 × FreeVRAM` in `SelectStrategyWithKV`
(`cmd/cppworker/adaptive_loader.go`): as long as `weights + KV + overhead` fit —
`exact_fit`; as soon as they do not — `partial_offload`.

### What to set

```
CPPWORKER_CTX_SIZE=32768            # default; working mode for 27B Q4_K_M on 24 GB
CPPWORKER_RAM_FALLBACK_MAX_N_CTX=32768   # no more than feasible (see below)
LB_NCTX_RELOAD_MAX_N_CTX=32768           # no more than feasible
```

To check the actual limit for a model on specific hardware:

```bash
cppworker -feasible Qwen3.8-27B-UD-Q4_K_M     # prints min(VRAM, RAM, GGUF)
curl -s localhost:18092/api/models | jq '.feasible_max_context, .gguf_max_context'
```

### What now protects against "silently not working" (R83)

1. **A 422 refusal.** If the requested n_ctx is above the physical limit (RAM or the
   model's training context) — loading does not start, the client receives the reason and
   the numbers. `partial_offload` is NOT forbidden in that case: it is a legitimate way
   to run a large model on a small GPU.
2. **The mode is visible in `/api/models`:** `max_vram_n_ctx`, `n_ctx_degraded`,
   `n_ctx_headroom`, `feasible_max_context`.
3. **A warning in the log** if the ceiling from the config is unreachable on this hardware
   (`CPPWORKER_RAM_FALLBACK_MAX_N_CTX` is greater than the physical limit) — previously the config
   promised 128000 where the limit was lower, and it looked like "the model broke".
4. **The model card in the WebUI** shows `Feasible n_ctx`, `GGUF max n_ctx` and
   a "partial offload" warning if the actual n_ctx is above the VRAM boundary.

> `gguf_max_context` (262144) is the TRAINING context of the model, not a
> recommendation. What you should rely on is `feasible_max_context`.

## R274 — pass-through: every variable the code reads must reach the container

**Live defect (repeated three times in R250–R272).** A variable was read by the
code (`os.Getenv`), present in `deployments/.env` — and still silently inactive,
because it was never forwarded in `deployments/docker-compose.stack.yml`. Every
time this was diagnosed as "the switch does not work" and time was spent looking
for a bug in code that had none. Cases: `LB_NCTX_GROW_FOR_AGENTS`,
`LB_OPENAI_AUTO_STREAM_KEEPALIVE_SEC`, `LB_RELOAD_DEFER_MAX_SEC`.

**Rule now enforced by a test** (`internal/balancer/env_passthrough_r274_test.go`):

1. Every `LB_*` name that appears in the balancer's Go code must be either
   - in the `loadbalancer` service `environment:` block of `docker-compose.stack.yml`, or
   - documented in `deployments/.env.example`, or
   - listed in the test's allowlist **with a written reason**.
2. A variable declared in the balancer's `environment:` block but never read by
   the code is reported as a dead switch / typo.

**Two forms of forwarding, on purpose:**

| Form | Meaning | Where used |
|------|---------|-----------|
| `- LB_X=${LB_X:-value}` | operator value, else the *code default* is written into the container | behaviour switches where the default must be explicit and visible in `docker inspect` |
| `- LB_X` (bare) | forwarded **only if** set in the compose environment (`deployments/.env`); otherwise absent, so `config.json` / code default wins | settings that mirror `config.json` (host, ports, queue, resource limits, timeouts, replication, virtual models, RPC coordinator) |

> **Setting a value.** Put it in `deployments/.env` (that is the file compose
> interpolates). `.env.bundled-with-agent` is the `env_file` of the service: it also
> reaches the container, but the `environment:` block has priority over it — see the
> header of the compose file.

**Visibility.** `docker inspect ol-stack-balancer --format '{{json .Config.Env}}'`
shows exactly what the container got; if a variable you set is missing there, the
problem is the pass-through, not the feature.

**Adding a new variable.** Read it in code → add it to the compose block (form
per the table above) or to `.env.example` → the test stays green. If you add it
only to the code, the test fails with the list of variables that will never reach
the container.

---

## Hot settings: registry and no-restart changes (R278)

Some variables are read on EVERY request/stream, so they can be changed without
restarting the container — through the balancer settings registry.

**Source priority** (highest first): value written from WebUI/API → `env` value
→ code default. `env` stays the DEFAULT; the operator's choice wins.

**Where the written value lives.** `settings-overrides.json` next to
`state.json` (writable volume `/app/data`). It survives restarts; `env` is
never overwritten.

**API.** `GET /api/v1/settings`, `PUT /api/v1/settings` with
`{"name":"LB_...","value":123}`, `POST /api/v1/settings/reset` with
`{"name":"LB_..."}` or `{}` (all).

**Hot variables (stage 2):** `LB_STREAM_MAX_LINE_BYTES`,
`LB_OPENAI_AUTO_STREAM_MAX_SEC`, `LB_OPENAI_AUTO_STREAM_MAX_BYTES`,
`LB_OPENAI_AUTO_STREAM_KEEPALIVE_SEC`, `LB_TOOL_CALL_BUFFER_BYTES`,
`LB_RELOAD_DEFER_MAX_SEC`, `LB_RELOAD_DEFER_FAST_FAIL_SEC`, `LB_ADMISSION_WAIT_SEC`,
`LB_LLAMA_UPSTREAM_TTL_SEC`, `LB_PLACE_OVER_RELOAD`.

**`LB_RELOAD_DEFER_FAST_FAIL_SEC` (R291).** Upper bound of how long a destructive operation may wait for someone else's streams *inside the HTTP response*. If the wait budget is larger, the balancer answers `503 deferred` immediately (the reload button must not "think" for two minutes); a short wait is kept, because the stream may end and the operation still runs in the same request. Default 5 s, `0` = never wait.

**`LB_PLACE_OVER_RELOAD` (R293).** `1` (default) — when the requested window or load parameters do not match the loaded copy, the balancer FIRST looks for another free node that has the model file (enough VRAM, no foreign streams) and loads the model there; `0` — previous behaviour (reload on the node where the model is already loaded). Why: on a two-node stand the second node was idle, yet the balancer reloaded the model on the first one and cut the first agent's stream. Moving happens only when there is a reason: the current node has active streams, or its ceiling is below the requested window (an idle node is cheaper to reload in place).

**Not hot.** Ports, http.Server timeouts, paths and anything used while
constructing the server are not in the registry: PUT returns 400 "requires
restart" instead of pretending it applied.
