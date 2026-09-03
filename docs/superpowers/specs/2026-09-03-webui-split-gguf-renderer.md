# R57.5 — split gguf-renderer.js (172KB монолит) design

**Date:** 2026-09-03
**Author:** Mavis (R57.5 split)
**Status:** Design approved (продолжение по плану пользователя)
**Branch:** `centurion`
**Depends on:** R57.1 (defer), R57.2 (app-core), R57.3 (app-listeners), R57.4 (app-modals)
**Reference:** `docs/superpowers/specs/2026-09-03-webui-split-app-js.md` (parent spec)

---

## 1. Background

`webui/js/modules/gguf-renderer.js` — **2743 строк / 172 KB / 80+ функций / single IIFE**.
Master-detail layout для GGUF tab WebUI: левая панель (список бэкендов) +
правая панель (4 таба: about / models / downloads / hf / settings).

Текущая структура файла (verified line numbers):
```
L1-15:    File header + I18N helper `_(key, vars)`
L17-79:   State object (большой, 25+ полей: registeredBackends, loadedModels,
          runtimeModels, activeQueries, loadOptions, _settingsFormHtmlByBackend, ...)
L81-91:   UNHEALTHY_STATUSES, isHealthyBackend, visibleBackends
L93-103:  Private flags (_refreshInProgress, _detailRefreshInProgress, _pollTimer)
L105-147: Main render + buildPageHtml
L148-236: Left panel (renderBackendsPanel, renderBackendsList)
L237-922: Right panel dispatch + 4 sub-panes (about, models, downloads, hf)
L972-1555: Settings pane + per-model profiles
L1556-1726: onDetailPanelClick (delegated event handler)
L1727-1868: bindEvents + bindSettingsChange
L1869-2441: Actions (load, unload, cancel, download, HF search)
L2442-2773: Data refresh + active queries polling
L2774-2827: Helpers (stripGGUF, formatFileSize, showToast)
L2828-2855: Public API return { render, getState, selectBackend, ... }
```

Это тот же root cause, что в R57.1-R57.4 для app.js:
- 172KB файл парсится одним блоком перед `DOMContentLoaded`
- Один IIFE = одна точка отказа при изменениях
- Новые функции добавляются в общий файл, не в свой модуль

## 2. Goal

Разбить `gguf-renderer.js` на 4 файла, чтобы:
- Каждая секция имела свой модуль (что облегчает понимание и изменение)
- Браузер парсил меньший блок за раз (parallel parse)
- Новые функции добавлялись в правильный модуль, не в общий файл
- Сохранить backward compat: `window.GgufRenderer.render / selectBackend / ...` API не меняется
- Сохранить `window.refreshActiveQueriesCrossTab` (используется app-listeners.js для cross-tab sync)

## 3. Target file structure

```
webui/js/modules/
├── gguf-renderer.js (NEW, ~120 строк) — orchestrator, public API
├── gguf-renderer-helpers.js (NEW, ~70 строк) — stripGGUF, formatFileSize, showToast
├── gguf-renderer-state.js (NEW, ~120 строк) — state object + visibleBackends + isHealthyBackend
├── gguf-renderer-list.js (NEW, ~110 строк) — left panel + renderBackendsPanel + renderBackendsList
├── gguf-renderer-detail.js (NEW, ~800 строк) — right panel dispatch + 4 sub-panes + settings
├── gguf-renderer-actions.js (NEW, ~600 строк) — load/unload/cancel/download + onDetailPanelClick
└── gguf-renderer-refresh.js (NEW, ~400 строк) — refresh + active queries polling
```

Все новые файлы используют IIFE-паттерн с `window.GgufModule` namespace (по
аналогии с R57.2-R57.4 для app.js). Никаких ES modules — classic scripts
сохраняются для consistency с существующим кодом.

**Общий размер**: ~2120 строк (vs 2743 сейчас, -23%), плюс ~120 строк orchestrator.

## 4. Глобальный namespace `window.GgufModule`

```js
// В каждом файле:
const M = (window.GgufModule = window.GgufModule || {});

// Состояние — общее для всех модулей:
M.state = { registeredBackends: [], loadedModels: [], /* ... */ };

// Helpers (доступны всем):
M.stripGGUF = function(name) { ... };
M.formatFileSize = function(bytes) { ... };
M.showToast = function(message, type) { ... };

// UI sub-renderers (регистрируются, потом orchestrator вызывает):
M.renderBackendsPanel = function() { ... };
M.renderBackendsList = function() { ... };
M.renderAboutPane = function() { ... };
M.renderModelsPane = function() { ... };
M.renderDownloadsPane = function() { ... };
M.renderHuggingFacePane = function() { ... };
M.renderSettingsPane = function() { ... };

// Actions (load/unload/etc):
M.selectBackend = function(id) { ... };
M.loadOnSelectedBackend = function(model) { ... };
M.unloadOnSelectedBackend = function(handle) { ... };
M.cancelActiveGeneration = function(modelName) { ... };
M.doHfSearch = function() { ... };
M.startDownload = function(modelId, filename) { ... };
M.cancelDownload = function(modelId, filename) { ... };
M.deleteDownloadedFile = function(modelId, filename) { ... };
M.markLoadingModel = function(...) { ... };
M.markLoadFailed = function(...) { ... };

// Data refresh:
M.refreshBackends = function() { ... };
M.refreshDetail = function() { ... };
M.startActiveQueriesPolling = function() { ... };
M.stopActiveQueriesPolling = function() { ... };
M.refreshActiveQueriesPolling = function() { ... };
M.updateBackendsList = function() { ... };
M.startDownloadPolling = function() { ... };
M.refreshActiveDownloads = function() { ... };
M.refreshDetailPanel = function() { ... };
M.refreshDetailPane = function() { ... };

// Other:
M.getState = function() { return M.state; };
M.render = function(container) { ... };  // main entry point
M.updateBackendsList = function() { ... };
M.bindEvents = function(container) { ... };
M.updateDetailLoading = function() { ... };
```

## 5. Commit plan (4 коммита R57.5a-d)

### R57.5a — extract `gguf-renderer-helpers.js` (~70 строк, low risk)
Содержит: `stripGGUF`, `formatFileSize`, `showToast`.
Это pure functions, не зависят от state.
После: `gguf-renderer.js` использует `M.stripGGUF` и `M.showToast`.

### R57.5b — extract `gguf-renderer-state.js` (~120 строк)
Содержит: `state` объект, `UNHEALTHY_STATUSES`, `isHealthyBackend`,
`visibleBackends`, `_ (key, vars)` I18N helper, private flags.
Critical: state — это центральный объект, к которому обращаются 80+ функций.
Перенос state в `window.GgufModule.state` потребует замены всех ссылок
`state.X` на `M.state.X` (либо alias `const state = M.state` в каждом модуле).

### R57.5c — extract `gguf-renderer-list.js` (~110 строк)
Содержит: `renderBackendsPanel`, `renderBackendsList`, `updateBackendsList`.
Низкий cross-cut (только state и isHealthyBackend).

### R57.5d — extract `gguf-renderer-detail.js` + `gguf-renderer-actions.js` + `gguf-renderer-refresh.js` (~1800 строк)
Большой коммит, но mechanical: перенести функции по секциям, добавить
`M.` prefix (или alias `const M = window.GgufModule`).
Самый рискованный — много cross-cuts между actions и refresh.

После R57.5d: `gguf-renderer.js` = ~120 строк orchestrator:
- L1-15: Header comment
- L17-20: `const M = window.GgufModule = window.GgufModule || {};`
- L22-25: `(function() { /* IIFE end */ })();`  — вызывает функции в правильном порядке
- L27-120: Public API return: `window.GgufRenderer = { render: M.render, getState: M.getState, selectBackend: M.selectBackend, ... }`

## 6. HTML script order

```html
<!-- В index.html, в defer-блоке, ПЕРЕД gguf-renderer.js: -->
<script defer src="js/modules/gguf-renderer-helpers.js?v=1"></script>
<script defer src="js/modules/gguf-renderer-state.js?v=1"></script>
<script defer src="js/modules/gguf-renderer-list.js?v=1"></script>
<script defer src="js/modules/gguf-renderer-detail.js?v=1"></script>
<script defer src="js/modules/gguf-renderer-actions.js?v=1"></script>
<script defer src="js/modules/gguf-renderer-refresh.js?v=1"></script>
<script defer src="js/modules/gguf-renderer.js?v=18"></script>
```

Все 7 файлов грузятся через `defer` (R57.1), order гарантирует что
helpers/state загружаются раньше list/detail/actions, которые
зависят от них.

## 7. Backward compat

- `window.GgufRenderer.render(...)` — работает как раньше (через orchestrator)
- `window.GgufRenderer.refreshActiveQueriesPolling()` — работает (R32 #2 fix для cross-tab)
- `app-listeners.js:refreshActiveQueriesCrossTab` → `window.GgufRenderer.refreshActiveQueriesPolling()` — НЕ меняется
- `renderers.js:window.Renderers.*` — НЕ зависит от внутренних GgufRenderer модулей
- `gguf-load-progress.js` — импортирует `markLoadingModel`, `markLoadFailed` (теперь через M.*)
- `bulk-models.js`, `config-io.js` — НЕ используют GgufRenderer внутренности

## 8. Risk analysis

| Risk | Likelihood | Mitigation |
|------|------------|------------|
| State — общее для всех модулей, ошибка инициализации → всё падает | Medium | TDD-тест: state должен быть инициализирован ДО вызова любой функции |
| `M.state` vs `state` — confused references | High | Использовать `const state = M.state` alias в каждом модуле → minimal change |
| Forward references между модулями (например detail вызывает list) | Medium | Тестировать после каждого R57.5x commit (docker cp + Playwright если работает) |
| Cache buster `?v=N` — забыть инкрементить | Low | Все файлы получают `?v=1` одновременно |
| `gguf-load-progress.js` сломался (использует markLoadingModel) | Low | Сначала R57.5a/b, потом R57.5d — даёт время найти проблемы |

## 9. TDD / verification

- `node --check` для каждого нового файла → OK
- `docker cp` в ol-bundled-webui → файлы served
- `curl http://localhost:18083/index.html` → проверяем что все 7 script тэгов загружены с `defer`
- `curl http://localhost:18083/js/modules/gguf-renderer-helpers.js` → 200 OK
- Manual smoke: открыть dashboard, проверить что GGUF tab рендерится (если Playwright работает)

## 10. Rollout

Каждый R57.5x — отдельный commit. Если что-то ломается, revert одного
коммита не ломает остальные.

Порядок: 5a → 5b → 5c → 5d (нарастающий риск, smallest first).

## 11. Out of scope

- ES modules (не делаем, classic scripts как в R57.1-R57.4)
- Полный рефактор state shape (state остаётся как есть, только переезжает в namespace)
- Изменения в backend API
- R57.4 setupEventListeners (в app.js, отдельная тема)
