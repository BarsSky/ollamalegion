# OllamaLegion — Documentation

> **Version:** 2.0 (2026-06-22) — consolidated revision
> **Languages:** **English** (current) | [Русский](../README.md)
> **Roadmap:** [`../../plans/README.md`](../../plans/README.md) | **Audit:** [`audit-2026-06.md`](audit-2026-06.md)
>
> 📋 **Phase 8 (2026-07-11):** Full documentation is now available in both languages.
> See [`../README.md`](../README.md) for the Russian version with all 15+ documents.

OllamaLegion is a load balancer + WebUI for Ollama and llama.cpp (via CppWorker) clusters with GPU/CPU/RAM/Disk/Network monitoring and intelligent request distribution.

---

## 🌍 Multilingual

| Language | Directory | Description |
|---|---|---|
| 🇬🇧 English | `./` (current) | All documents in English (translated) |
| 🇷🇺 Русский | [`../README.md`](../README.md) | All documents in Russian (source language) |

WebUI (Dashboard) also supports real-time language switching:
- 🌍 In the top-right corner → language switcher
- 🇬🇧 English (`en.js`) / 🇷🇺 Русский (`ru.js`) — auto-detect via `navigator.language`
- 💾 Selection saved in `localStorage` (key: `ollamalegion_lang`)
- Parity tests: `internal/api/lint_css_i18n_test.go::TestI18nKeyParity_EN_RU`

---

## 📚 Table of Contents

### Main documents

| Document | Russian | Description |
|---|---|---|
| [**installation.md**](installation.md) | [../installation.md](../installation.md) | Requirements, Docker, local builds, stub mode |
| [**deployment.md**](deployment.md) | [../deployment.md](../deployment.md) | Docker Compose, bundled stack, production config, scaling |
| [**agent-deployment.md**](agent-deployment.md) | [../agent-deployment.md](../agent-deployment.md) | Agent deployment CPU/GPU (incl. Windows + WSL2) |
| [**api.md**](api.md) | [../api.md](../api.md) | REST API + WebSocket + CppWorker API + Ollama compatibility |
| [**cppworker-model-params.md**](cppworker-model-params.md) | [../cppworker-model-params.md](../cppworker-model-params.md) | n_ctx resolver, Per-Model Profiles, RAM fallback, Ollama ↔ OpenAI proxy |
| [**backend-type-isolation.md**](backend-type-isolation.md) | [../backend-type-isolation.md](../backend-type-isolation.md) | Ollama vs llama.cpp backend isolation |
| [**metrics.md**](metrics.md) | [../metrics.md](../metrics.md) | Complete metrics reference (GPU/System/Ollama/Proxy) |
| [**rpc-coordinator.md**](rpc-coordinator.md) | [../rpc-coordinator.md](../rpc-coordinator.md) | **P.1**: distributed inference via RPC (production mode) |
| [**virtual-router.md**](virtual-router.md) | [../virtual-router.md](../virtual-router.md) | **P.2**: alias-on-pool virtual models |
| [**phase-8-rpc-coordinator.md**](phase-8-rpc-coordinator.md) | [../phase-8-rpc-coordinator.md](../phase-8-rpc-coordinator.md) | P.1 implementation log |
| [**phase-8-p3-research.md**](phase-8-p3-research.md) | [../phase-8-p3-research.md](../phase-8-p3-research.md) | P.3 research: real ggml/NCCL (post-1.0) |
| [**phase-7-style-compliance.md**](phase-7-style-compliance.md) | [../phase-7-style-compliance.md](../phase-7-style-compliance.md) | Phase 7 style guide (em-dash → ASCII) |
| [**troubleshooting.md**](troubleshooting.md) | [../troubleshooting.md](../troubleshooting.md) | FAQ + n_ctx + tools/tool_calls diagnostics |
| [**runbook-tools.md**](runbook-tools.md) | [../runbook-tools.md](../runbook-tools.md) | **Detailed runbook** for tools/tool_calls diagnostics (scenarios A-G) |
| [**audit-2026-06.md**](audit-2026-06.md) | — | Final audit report (implemented / remaining / limitations) |

### API specifications

| Document | Description |
|---|---|
| [`openapi.yaml`](openapi.yaml) | OpenAPI 3.0.3 specification |
| [`swagger.json`](swagger.json) | Swagger 2.0 specification |

### Architecture decisions

| Document | Description |
|---|---|
| [`cline-skills/cppworker-gpu-e2e-routing-test.md`](cline-skills/) | Test scenario for GPU routing |
| [`cline-skills/vulkan-scene-debug.md`](cline-skills/) | Vulkan scene debug |

---

## Architecture (brief)

```
┌─────────────┐     ┌──────────────────────────┐     ┌─────────────┐
│   Clients   │────▶│      Load Balancer       │────▶│  Agent +    │
│ (OpenWebUI, │     │  (18080 proxy / 18081    │     │  Ollama     │
│  Cline)     │     │   management + WS)       │     │  (11434)    │
└─────────────┘     └──────────┬───────────────┘     └─────────────┘
                              │
                              ▼
                     ┌──────────────────┐         ┌─────────────┐
                     │      Web UI      │         │  CppWorker  │
                     │  (18083 Nginx)   │         │  (18091)    │
                     └──────────────────┘         │  llama.cpp  │
```

Three production-ready operating modes:

| Mode | Purpose | Doc |
|---|---|---|
| `standard` | Default Ollama proxying + cluster management | [api.md](api.md) |
| `rpc_coordinator` | **P.1** distributed inference (multi-worker pipeline) | [rpc-coordinator.md](rpc-coordinator.md) |
| `virtual_router` | **P.2** alias-on-pool virtual models | [virtual-router.md](virtual-router.md) |

---

## Phase 8 (2026-07-11) — production-ready

Major deliverables:

- **P.1 rpc_coordinator** — full production mode, circuit breaker per worker, streaming SSE passthrough, auth, error mapping, 4 endpoint response shapes, 213 scenario tests.
- **P.2 virtual_router** — alias-on-pool mode, 3 selectors (round-robin, least-loaded, random), auto-failover (5xx retry, 4xx passthrough), CRUD REST API, WebUI, auth.
- **P.3 research** — Real ggml/NCCL (post-1.0).
- **P.4 CI/CD** — GitHub Actions workflow + self-hosted runner + 8 linters.
- **Test coverage** — 95+ new tests in Phase 8. Balancer 53.3%, rpccoordinator 71.1%, rptensor 89.0%.
- **Tag v1.0-rc1** — released 2026-07-11. Final `v1.0` pending manual hardware smoke (A10 + Qwen3-A3B).

See [`CHANGELOG.md`](../../CHANGELOG.md) for full Phase 8 changelog.

---

## Recent additions (R56-R59, 2026-08-24 → 2026-09-03)

Operational features added on top of v1.0-rc1:

| Feature | Doc | Notes |
|---|---|---|
| **Cluster AutoDistribute** (R59, R59.1) | [`api.md` §Cluster](api.md#cluster) (rows 1951-1952) | `GET /api/v1/admin/cluster/autosuggest` + `POST .../apply` for moving models between backends |
| **Adaptive n_ctx auto-reload** (R34-37) | [`configuration.md` §Round 56-59](configuration.md#round-56-59-2026-09-03-new-env-vars-and-endpoints) (look for `LB_NCTX_RELOAD_*`) | `LB_NCTX_RELOAD_*` — auto-bump n_ctx on `prompt_too_long` |
| **n_ctx preflight check** (R35) | same section (look for `LB_NCTX_PREFLIGHT_*`) | `LB_NCTX_PREFLIGHT_*` — short-circuit 413 instead of 503+retry loop |
| **RAM fallback cascade** (R38) | same section (look for `CPPWORKER_RAM_FALLBACK_*`) | `CPPWORKER_RAM_FALLBACK_*` — full VRAM → partial offload → CPU-only |
| **Hardware presets** (R58.2) | [`hardware-presets.md`](hardware-presets.md) | 4 ready-made configs (RTX 30xx/40xx/50xx, A10) + `apply-hardware-preset.py` |
| **Streaming never-timeout** (R58.1) | same section (look for `LB_STREAMING_NEVER_TIMEOUT`) | `LB_STREAMING_NEVER_TIMEOUT=1` for long-running inference (10+ min) |
| **Backend.ApiStyle** (R56) | same section (look for `Backend.ApiStyle`) | Operator-overridable API style (ollama-native / openai-compatible) |
| **EffectiveAPIStyle helper** (R56) | same as above | Replaces `isLlamaCppBackend()` in routing |

R59.11 added a docs cleanup pass (configuration.md index + 4 broken links fixed + 6 ENV tables). R59.16 added the English translation of `hardware-presets.md`. R59.17 mirrors the 3 LB_NCTX_* / CPPWORKER_RAM_FALLBACK_* tables that were only in the Russian configuration.md.
