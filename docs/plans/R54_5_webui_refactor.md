# R54.5+ WebUI рефактор план: AutoTune Visibility & Control

## Контекст

R54.1-R54.4 (commits 6706206, 5e9c9b3, 02be852) добавили:
- `b.llamaCpp.autoTune` в API response (overallSeverity, recommendations, AutoTuneEnabled)
- `b.llamaCpp.loadedModels[]` уже доступен (cppworker poller)

**Текущее состояние WebUI**: AutoTune данные приходят с API но НЕ отображаются.
Оператор видит `loadedModels` с ctx/kv_cache info, но не знает что модель sub-optimal.

## Phased rollout

> **Status: R54.5–R54.8 delivered (2026-08-24)**, R55.2 (Phase 5: history
> timeline) added beyond original scope. Phase 6+ (Predictive prefetching,
> Multi-backend intelligence) deferred — out of current sprint.

### R54.5 — AutoTuneBadge (Phase 1, visibility) ✅ DELIVERED
**Цель**: только показать. Без кнопок, без действий.

Что сделать:
- `webui/js/modules/renderers.js`: badge `<div class="autotune-badge ${severity}">` рядом с loadedModel
  - overallSeverity → CSS class (ok / info / warning / critical)
  - recommendation count as `<span class="autotune-rec-count">${n} rec</span>`
  - AutoTuneEnabled → small "auto-tuned" / "manual" indicator
- `webui/css/data.css`: 4 цвета для severity (green/yellow/orange/red)
- Tooltip с текстом recommendations
- Click → expand inline список recommendations

Manual: нет действий — это read-only visibility.

### R54.6 — ApplyButton (Phase 2, manual control) ✅ DELIVERED
**Цель**: оператор может применить рекомендации одной кнопкой.

Backend changes:
- `POST /api/v1/admin/autotune/{backendID}/apply` — применить все рекомендации
- `GET /api/v1/admin/autotune` — circuit state для UI

Frontend changes:
- Кнопка "Apply Recommendations" в badge tooltip
- POST → loading state → success/error
- Показать circuit state (cooling down / ready)
- Disable кнопку если circuit open

### R54.7 — Settings page integration (Phase 3, full control) ✅ DELIVERED
**Цель**: AutoTune в Settings, per-model toggle в Model Profile editor.

Frontend changes:
- Settings page: AutoTune switch (read current config, POST update)
- Model Profile editor (если есть): per-model `autoTune: true|false|nil` field
- Индикатор "last AutoTune reload: 5 min ago, successful" на backend page

### R54.8 — Live monitoring (Phase 4, observability) ✅ DELIVERED
**Цель**: real-time visibility в AutoTune state через WebSocket.

Backend changes:
- WebSocket event: `autotune.reload_triggered`, `autotune.reload_succeeded`, `autotune.reload_failed`
- Per-backend circuit state в WebSocket payload

Frontend changes:
- Subscribe to autotune events
- Toast notification "AutoTune: reloaded cppworker-gpu-bundled-agent with f16 KV cache"
- Live circuit state indicator (cooling down / ready / open)

### R55.2 — AutoTune History (Phase 5, replay) ✅ DELIVERED
**Цель**: timeline view для "что AutoTune делал за последние N часов".
Дополнение к Phase 4 (live) — для replay при новом HTTP client.

Backend (R55.2):
- `AutoTuneHistory` ring buffer (500 entries, drop-oldest, in-memory)
- `GET /api/v1/admin/autotune/history?limit=N&since=RFC3339&backend=<id>`
- `publishAutoTuneEvent` helper — пишет и в EventBus (live), и в history log
- 4 event types уже в R54.8 (triggered/succeeded/failed/circuit_open)

Frontend (R55.2b):
- `autotune_history.js`: timeline modal с severity colors + params (n_ctx/kv/layers)
- "View History" button в AutoTune card per-backend
- Refresh button + filter by backend
- Empty state: "No AutoTune events yet"

Manual: в перспективе live WebSocket integration (auto-refresh на new events) — R55.2c.

### Beyond original scope (delivered)

- **R54.9 — Workload-aware KV cache**: AutoTune учитывает p95 num_ctx
  (историю запросов) при выборе KV cache type. Light workload → f16
  (quality), heavy → q4_0 (max context).
- **R52.5 — Per-component Stop for graceful shutdown**: EventBus.Stop,
  GroupController.Stop, NCtxReloadCoordinator.Shutdown, etc. Закрывает
  goroutine-leak при exit.

## Design principles

1. **Read-only first** (R54.5) — показать данные, не менять state
2. **Manual control** (R54.6) — кнопка для действия
3. **No surprises** — AutoTune НИКОГДА не делает reload без явного одобрения
   (operator'а через ApplyButton) или пока circuit не покажет готовность
4. **Graceful degradation** — если AutoTune endpoint вернул 404/500, WebUI
   показывает "AutoTune info unavailable" без падения

## Visual design

```
┌─[ Backend: cppworker-gpu ]───────────────────┐
│  Status: healthy  |  GPU: 2.5GB/8GB          │
│  Loaded Models:                                │
│    ┌ Qwen3-Instruct-2507-q4km ┐              │
│    │ Ctx: 65536  |  KV: f16     │  ✓optimal  │
│    │ Quant: Q4_K_M  |  Layers: -1│             │
│    └────────────────────────────┘              │
│                                                 │
│  AutoTune: [⚠ info] 1 recommendation           │
│   └─ n_ctx=65536 >> feasible=24314              │
│       → Перезагрузить с n_ctx=24314              │
│       [ Apply ] (Phase 2)                       │
└─────────────────────────────────────────────┘
```

Color scheme:
- ok: green (--color-success)
- info: blue (--color-info)
- warning: orange (--color-warning)
- critical: red (--color-error)

## Code structure

```
webui/
├── js/
│   ├── modules/
│   │   ├── renderers.js (modify: add AutoTuneBadge)
│   │   ├── autotune.js (NEW Phase 1: read-only badge logic)
│   │   └── settings-ui.js (modify: AutoTune switch)
│   └── app.js (modify: register autotune handlers)
├── css/
│   ├── data.css (add .autotune-badge styles)
│   └── components.css (add .autotune-rec-item)
└── i18n/
    ├── ru.json (add "autotune.*" keys)
    └── en.json (add "autotune.*" keys)
```

## Test plan

- Unit tests: badge rendering (severity class, recommendation count, AutoTuneEnabled)
- Integration: live verify (manual click in dev console → check DOM)
- E2E: backend with sub-optimal state → AutoTuneBadge shows warning → reload → badge shows ok

## Dependencies on backend work

- R54.1+R54.2 (DONE): `autoTune` field in /api/v1/backends
- R54.4 (DONE): autonomous trigger + circuit breaker (mostly stub for reload)
- R54.6 backend: `/api/v1/admin/autotune/{backendID}/apply` endpoint
- R54.8 backend: WebSocket event for autotune

## Timeline estimate

| Phase | Effort | Files | Est. Lines |
|-------|--------|-------|------------|
| R54.5 Badge | 30 min | renderers.js, data.css, autotune.js (new) | ~150 |
| R54.6 Apply | 60 min | handlers_backends.go, autotune.js, renderers.js | ~250 |
| R54.7 Settings | 30 min | settings-ui.js, renderers.js | ~100 |
| R54.8 WebSocket | 60 min | websocket events, autotune.js | ~200 |
| **Total** | **~3h** | | **~700** |

## Open questions (нужны решения от пользователя)

1. **Where in the page does AutoTuneBadge go?**
   - (a) Inline next to each loadedModel card (most visible)
   - (b) Separate "AutoTune" tab on backend page
   - (c) Top-level indicator on Dashboard (cluster summary)
   - **Recommendation**: (a) inline + (c) summary (later phases)

2. **Apply behavior** (Phase 2):
   - (a) Apply all recommendations atomically
   - (b) Per-recommendation apply buttons
   - **Recommendation**: (a) — atomic, fewer round-trips

3. **Auto-disable circuit on persistent failures**:
   - (a) Open circuit after N consecutive failures (current design)
   - (b) Exponential backoff per circuit
   - **Recommendation**: (a) for now, can add (b) in R54.9 if needed
