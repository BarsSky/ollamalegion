# WebUI: split app.js 185KB and gguf-renderer.js 172KB + add `defer` (R57)

**Date:** 2026-09-03
**Author:** Mavis (R57 — Wave B.3 from 2026-09-03 user roadmap)
**Status:** Design approved by user (Wave B.3 choice in commit_or_continue questionnaire)
**Branch:** `centurion`
**Depends on:** R56 (commit `32cea8f` EffectiveAPIStyle migration, already shipped)
**Pre-existing test:** `playwright.baseline.config.js` + `scripts/baseline.spec.js` (R56.5, will be committed in R57.1)

---

## 1. Background

Playwright baseline (2026-09-03) showed:

| Test | Status | Notes |
|------|--------|-------|
| balancer /health | PASS (54ms) | API layer OK |
| cppworker /health | PASS (20ms) | Backend OK |
| balancer /api/v1/backends | PASS (81ms) | Auth + API working |
| **WebUI home loads** | **FAIL (180s timeout)** | `browserContext.newPage` times out, page never reaches interactive state |

`webui/js/app.js` is **185 KB / 3177 lines / 1 IIFE module**. `webui/js/modules/gguf-renderer.js` is **172 KB / 2743 lines / 1 IIFE module**. Combined, these two files contain ~50% of the WebUI's JS payload and ~80% of its parse cost.

The 24 `<script src="...">` tags at the bottom of `<body>` (line 1776-1839) execute serially:
1. Each `<script>` blocks HTML parsing
2. Each `<script>` is downloaded sequentially
3. Each `<script>` is parsed+executed before the next starts
4. Browser cannot start painting body content until app.js has parsed (the orchestrator attaches UI handlers to existing DOM)

`newContext.newPage` waits for the page to be ready enough to interact with. With 185KB+172KB of blocking JS, this never happens within 180s on Windows + headless Chromium.

This is the **root cause** of the user's "форма по отображению не актуализируется при смене данных" complaint — the page never reaches interactive state, so the polling-driven updates that the existing code (Round 32 #6) was designed to do never run.

## 2. Goal

Three independent improvements, each tested with the same Playwright baseline:

1. **R57.1 — add `defer` to all 24 script tags.** Browser downloads scripts in parallel, executes in order after DOMContentLoaded. Browser can paint body content (CSS, layout) before scripts run.
2. **R57.2 — split `app.js` into 3 files.** Smaller parse units = faster initial parse. Files: `app-core.js` (init/theme/density/i18n, ~290 lines), `app-listeners.js` (setup* functions, ~700 lines), `app-orchestrator.js` (data, fetch*, render*, ~2200 lines — still big but orchestrator-like).
3. **R57.3 — split `gguf-renderer.js` into 4 files.** Per-tab split: `gguf-renderer-list.js`, `gguf-renderer-detail.js`, `gguf-renderer-models.js`, `gguf-renderer-forms.js`, `gguf-renderer-active-queries.js`. Each 300-700 lines. Each tab is lazily initialized on click.

Each commit is independently testable: Playwright baseline MUST go from "FAIL 180s" to "PASS <30s" by R57.3, ideally by R57.1.

## 3. Acceptance criteria

1. **Playwright baseline PASSES** — `npx playwright test --config=playwright.baseline.config.js` 3/3 green, home loads in <30s.
2. **Zero behavior change** — visual regression tests in `tests/visual/density.spec.js` still pass (they don't currently pass due to the same SPA issue, but the snapshots are gitignored and can be regenerated via `--update-snapshots`).
3. **No new global API** — `window.Renderers`, `window.ui`, `window.GgufRenderer`, etc. all stay on `window.*` exactly as before. Classic scripts, no ES modules.
4. **HTML order preserved** — `<script>` tags load in the exact same order (so `window.X` references work the same way).
5. **Cache-busting query preserved** — `?v=N` parameter kept on each new file (next bump: `?v=18` or use a different scheme).

## 4. Files to change

### R57.1: `defer` on all scripts

**File**: `webui/index.html` — change all 24 `<script src="...">` lines from
```html
<script src="js/modules/whatever.js?v=16"></script>
```
to
```html
<script defer src="js/modules/whatever.js?v=16"></script>
```

Plus the `<script>window.WEBUI_CONFIG = ...</script>` inline scripts at the top — change to `defer` too. Inline scripts that read `document.body` BEFORE body is parsed would break — but the existing comments at L34-36 say they switched to `document.documentElement` already (R17 P.8), so they should be safe.

**Verification**: `npx playwright test --config=playwright.baseline.config.js` should now PASS or significantly reduce timeout.

### R57.2: split `app.js`

**Strategy**: keep the IIFE pattern. Each new file declares its functions inside the same `ui` IIFE — but that requires cross-file state sharing, which means either:
- (a) Attach the inner `ui` object to `window` from the first file, then subsequent files do `window.ui = window.ui || {}; const inner = window.ui; ...; window.ui.fetchX = function...`
- (b) Use a more invasive refactor with explicit module pattern

**Path (a) is the lowest risk.** Each new file:
```js
(function() {
    const ui = window.ui = window.ui || (function() { ... }());
    function myNewFunction() { ... }
    ui.myNewFunction = myNewFunction;
})();
```

But this is messy. **Better approach**: keep `app.js` as the orchestrator (with all the public exports), and split OUT the implementation details into helper files that don't share state. The new files would be standalone IIFEs that register their handlers on a shared object exposed by `app.js`.

**Final structure for R57.2**:
```
webui/js/
├── app.js (1.0K lines — orchestrator, init, public API)
├── app-core.js (NEW, ~290 lines — initTheme, initDensity, setupI18n, initAutoTuneEventHandlers, showToast)
├── app-listeners.js (NEW, ~700 lines — setupNavigation, setupEventListeners, setupWebSocketEvents, setupApiEvents, setupRestartHandler, setupLogsTabNavigation, fetchAgents + agents)
├── app-orchestrator.js (NEW, ~1100 lines — data fetching + rendering dispatch + filter/sort + export)
```

Each new file uses classic IIFE + `window.X` exports. No ES modules.

### R57.3: split `gguf-renderer.js`

**File**: `webui/js/modules/gguf-renderer.js` is one big IIFE exposing `window.GgufRenderer`. Split into:
```
webui/js/modules/
├── gguf-renderer.js (NEW, ~50 lines — orchestrator: master-detail layout, registerSubrenderer)
├── gguf-renderer-list.js (NEW, ~500 lines — backends list rendering)
├── gguf-renderer-detail.js (NEW, ~500 lines — detail pane, about tab, lazy-loads other tabs)
├── gguf-renderer-models.js (NEW, ~400 lines — loaded models tab, downloads, progress)
├── gguf-renderer-forms.js (NEW, ~400 lines — settings form, HF search)
├── gguf-renderer-active-queries.js (NEW, ~300 lines — busy badge, polling, cancel)
```

The orchestrator uses a registration pattern: each sub-renderer exports an `init()` function that takes a context object. The orchestrator calls them in order on detail-pane open.

## 5. Verification protocol (for each R57.x)

```bash
# 1. Build the webui (Docker) to confirm static assets still valid
cd C:\Ollama\ollamalegion\deployments
docker compose -f docker-compose.cppworker-bundled-with-agent.yml build webui

# 2. Restart webui container
docker compose -f docker-compose.cppworker-bundled-with-agent.yml up -d --no-build --no-deps webui

# 3. Run Playwright baseline
cd C:\Ollama\ollamalegion
npx playwright test --config=playwright.baseline.config.js --reporter=line

# 4. Verify all 3 tests pass, home loads <30s
# 5. Take screenshot via Playwright, eyeball dashboard
```

## 6. Risk analysis

| Risk | Likelihood | Mitigation |
|------|------------|------------|
| Cross-file dependency cycle (file A needs function from file B, file B needs function from file A) | High | R57.2 will discover this on first run; fix by re-arranging order in index.html (classic scripts) or hoisting common state to `window.ui` |
| `defer` causes script to execute after DOMContentLoaded but before `<body>` is ready for some script that does `document.body.appendChild` | Medium | Most scripts use `document.documentElement` already; verify by re-running density test (which uses the same DOM API) |
| Visual regression — split files change render order | Low | Renderers are all `window.Renderers`; the function order doesn't matter as long as the global is set before any caller invokes it |
| Cache busting — old `?v=16` cached in user's browser | High | Bump to `?v=18` on the new files (R57.1 keeps `?v=16` for unchanged files) |
| Splitting gguf-renderer.js breaks the detail-pane lazy loading | Medium | Each sub-renderer MUST register its own handlers; the orchestrator calls them in a controlled sequence |

## 7. Out of scope

- Converting to ES modules (would require all `window.X` to become `import`/`export`). Big refactor, separate round.
- Adding `<link rel="modulepreload">` for true parallelization of ES modules. Defer + classic scripts is enough for now.
- Code splitting per-page (dashboard vs backends vs gguf). The orchestrator already only initializes the current page; we just need to make `init()` cheaper.
- Moving from `var` to `let`/`const` throughout. Cleanup PR, not blocking.
- Service worker / offline support. Future round.

## 8. Rollback

Each R57.x is a single commit. If R57.1 doesn't help, revert that commit (defer attribute is additive, removing it brings back blocking scripts but no other change). If R57.2 introduces a regression, revert just that commit (file split is additive — putting the code back in `app.js` works).
