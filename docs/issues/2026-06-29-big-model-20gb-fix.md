# Issue 2026-06-29: Big Model (20GB VRAM) Loading — Resolution Status

## Issue Summary
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

## Known Issues / TODO

1. **RAM fallback reset endpoint** — `POST /api/v1/cppworker/reset-reload-counter` is still TODO.
   For now, `docker restart deployments-cppworker-gpu-1` to clear `ramFallbackAttempts`.
2. **i18n helper scripts** — `tools/fix_i18n_syntax.py`, `tools/collapse_quotes.py`, `tools/scan_i18n_keys.py` are kept for future i18n emergencies. Устаревшие `restore_i18n_quotes*.py` v1-v4, `fix_i18n_syntax.ps1`, `fix_doubled_quotes.py` удалены из `tools/` (2026-06-29 v2 cleanup).
3. **Em-dash compliance** — 7 remaining em-dash violations in CSS comments (icons.css:?, components.css:69, monitor-app.css:73, pages.css:20, responsive.css:2, i18n/ru.js:1, i18n/en.js:1). Treated as low priority; user-facing copy is clean.
4. **rgba() compliance** — ~120 rgba() in CSS (mostly theme tokens, box-shadows in :root, and gradient stops). Mostly legitimate use; some can be replaced with color-mix() for theme-friendly variants. Low priority.
5. **Ollama-only messages** in CppWorker (e.g. `/api/pull` for `ollama` registry) — return HTTP 501 as expected.