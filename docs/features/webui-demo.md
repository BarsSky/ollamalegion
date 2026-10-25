# WebUI demo mode (R232, 2026-10-24)

Demo mode is a showcase of the panel without a real stand: the WebUI renders
a synthetic but plausible scenario of cluster operation and **does not contact the
balancer**. It is meant for demos/training: any guest opens the page and sees
how clients connect, how the balancer distributes requests, how the queue grows and
dissipates, how a donor is assigned over RPC, and how AutoTune works.

## Enabling (env only)

In `deployments/.env`:

```dotenv
WEBUI_DEMO_MODE=1
```

and recreate webui:

```bash
docker compose -f deployments/docker-compose.stack.yml up -d --force-recreate webui
```

**A webui image from R232+ is required**: the variable is read by `docker/webui/entrypoint.sh`,
so an old image (`webui:r229m-*` and earlier) will ignore it. Build and
substitute:

```bash
pwsh -File scripts/build-containers.ps1 -WebUI -Tag r232-demo
# deployments/.env: WEBUI_TAG=r232-demo
```

There is no other way: **there is no toggle in the interface**. This is a deployment mode
(showcase/training), not a user setting. To disable it — `WEBUI_DEMO_MODE=0`
(or delete the line) and recreate the container again. The default value is `0`.

Checking that the mode came up:

```bash
curl -s http://<webui>/js/modules/config.js | grep DEMO_MODE
# DEMO_MODE: true,
```

or in the container log: `DEMO_MODE=true (WEBUI_DEMO_MODE=1)`.

## What the guest sees

- Login is **not requested**: the demo substitutes a guest `viewer` session
  (read-only). Admin actions are hidden by the standard role logic.
- At the top — a **"DEMO MODE"** banner: the data is synthetic, time is sped up.
- At the bottom right — a **"Demo scenario"** panel: the current phase, counters (clients,
  queue, layers on GPU, layers on donor, tok/s, AutoTune state) and an event
  feed.
- The data arrive on every page that reads `/api/`: overview (backends,
  queue, sessions), monitor (topology, queue, layout), AutoTune badges and
  history. All `/api/` requests are intercepted in the browser; the real balancer is not
  polled, and mutations answer `403 read-only`.
- Together with `fetch`, `WebSocket` and `EventSource` (SSE
  `/api/v1/events`) are replaced: live updates and the notification bell work, but
  there are no connections to the server.
- Service pages (`health.html`, `rpc-status.html`, `tp-pipeline.html`,
  `virtual-models.html`) are not part of the scenario — links to them are hidden in the demo
  so that the guest does not land on pages without synthetic data.

### Monitor: live numbers and the request/response conveyor (R238)

The monitor is not a static picture: the demo supplies the same fields the real
balancer does, so the guest sees a coherent flow.

- `activeRequests` / `maxConcurrentRequests` — the busy backend tile and the
  balancer's "active N" show requests in flight. Before R238 the synthetic
  backends had no active counters and the field was always 0.
- `requestsPerSecond` and the cluster `rps` are computed from completions in the
  scenario, not invented; `totalRequests` grows too, so the "Req/s" stat is alive.
- `recentRequests` / `recentResponses` drive the conveyor: each assignment draws a
  labelled particle from the client to a backend, and each completion draws a
  **response particle (yellow → green, `↩ req-NNN`)** from the backend back through
  the balancer to the client. Every particle follows exactly the drawn conveyor
  lines and has a label (before R238 some particles were labelled and some were
  anonymous dots).
- `warmingUpModels` — while the model loads, orange particles walk from the
  balancer to the backend.
- The topology's random particles are disabled in the demo (`recentRequests` is
  present), so there is one stream instead of two competing animations.

**The same conveyor also runs in live mode** (`monitor.html` against a real
balancer). The response leg and its label are spawned from
`queue.processing_count` activity (the label is the backend model, since the
balancer does not expose per-request client names in `/api/v1/queue/history`);
`req-NNN` labels and exact completion timing remain demo-only. The yellow
"waiting" particles in live mode now travel along the client → balancer line
instead of orbiting inside the balancer block.

### Phone screens (R238)

The WebUI adapts to small screens:

- the header wraps its action buttons instead of stretching the page (before,
  `.header-actions` was 521 px wide on a 390 px screen, so the document was 659 px
  wide and controls overlapped);
- the demo banner height is measured and exposed as `--demo-banner-h`: the sidebar,
  the nav burger and the content sit below it instead of under it;
- the demo scenario panel is collapsed by default below 720 px and opens with "+";
- in the monitor the stats bar wraps instead of being clipped, and the topology
  geometry shrinks the client/backend columns and the balancer block
  (`MonitorApp.GEOM.applyResponsive`) so tiles never overlap the balancer;
- inside the `index.html` iframe the monitor does not draw a second banner/panel
  or a second guest chip — only the outer page does.

## Scenario (150 s cycle)

| Time | Phase | What happens |
|---|---|---|
| 0–8 s | Model load | the master loads `Qwen3.8-27B-UD-Q4_K_M` (15.3 GB), ctx 16384, KV q8_0 |
| 8–14 s | Clients | Cline, Open WebUI, curl connect; all go to one balancer address |
| 14–45 s | Queue | a single slot: requests queue up, the balancer tracks positions and waiting time |
| 45–80 s | Donor (RPC) | a donor joins the layout: 22 layers move to RPC0, CPU layers 46 → 18, the queue dissipates |
| 80–96 s | AutoTune | analysis: KV q4_0 → f16 (budget 8.0 GB), model reload, verification of the applied configuration |
| 96–128 s | Steady operation | AutoTune: optimal, the queue is empty, clients work |
| 128–150 s | Cycle end | some clients disconnect; the scenario repeats |

The numbers are consistent with the live R231 measurement on qwen3.8 27B Q4 (master 8 GB + donor
8 GB): 20/66 layers on GPU before the donor, 48/66 after (26 CUDA0 + 22 RPC0), 0.71 →
2.08 tok/s. Time in the demo is sped up, otherwise a single answer would take ~2 minutes.

## Limitations (honestly)

- This is a **simulation**: there are no requests to the real cluster, so the demo does not replace
  checking the stand (for that — `scripts/ci.ps1 -Live -Clients`).
- The demo is read-only: change buttons are either hidden by the viewer role or will get
  `403 read-only`.
- Unknown `/api/` endpoints answer an empty `{demo:true}`: pages that are not
  in the scenario (for example, complex settings forms) may look empty — this is
  expected.
- The values in the scenario are fixed (deterministic), not random: that way the demo
  is reproducible for screenshots and checks.

## Checking the mode

```bash
# unit: scenario logic and the API router (24 checks)
node webui/js/modules/demo-mode.test.js

# unit: wiring (HTML and script order, entrypoint, compose, documentation) — 8 checks
node webui/js/modules/demo-mode-wiring-r232.test.js

# unit: phone geometry of the monitor (no browser needed) — 5 checks
node webui/js/modules/monitor-mobile-layout.test.js

# live check: a temporary container with WEBUI_DEMO_MODE=1 + Playwright (the stand is not touched)
pwsh -File scripts/verify-demo-r232.ps1

# Playwright scenario only, against an already running demo webui
WEBUI=http://127.0.0.1:18084 node scripts/webui-e2e/check-demo-mode-r232.js
```

By default the Playwright check launches the `msedge` channel (like the other
`scripts/webui-e2e` checks); it is overridden by the `PW_CHANNEL` variable, and if the channel is
unavailable, the bundled chromium is used.

The live check confirms the main property of the mode: on `index.html` and `monitor.html`
**not a single** request to `/api/` goes to the server (including `ws`/SSE), so the demo can be
enabled even on a production stand — the guest will neither see nor load the real cluster.

See also: [configuration.md](../getting-started/configuration.md), [deployment-stack.md](../operations/deployment-stack.md),
[ENV_VARS.md](../api/env-vars.md).
