# OllamaLegion - Documentation

> **English** (current) | [Russian](README.ru.md)
>
> Table of contents for the whole documentation set. One document - one file in its category directory.
> Naming: `<name>.md` is English, `<name>.ru.md` is Russian.

Russian is the source language of the project, so a few documents exist only in Russian (a dash `-` in the English column means the English translation is still pending). Historical material (audits, round reports, research notes) lives in [`archive/`](archive/README.md) in its original language.

## Contents

- [Getting started](#getting-started)
- [Architecture](#architecture)
- [Operations](#operations)
- [API and reference](#api-and-reference)
- [Features](#features)
- [Releases](#releases)
- [Development](#development)
- [Plans and session reports](#plans-and-session-reports)
- [Archive](#archive)

## Getting started

Install, configure and bring the stack up.

| Document | English | Russian | What it covers |
|---|---|---|---|
| **Installation and build** | [EN](getting-started/installation.md) | [RU](getting-started/installation.ru.md) | Requirements, Docker build, local build, stub mode. |
| **Deployment** | [EN](getting-started/deployment.md) | [RU](getting-started/deployment.ru.md) | Deploying the stack: profiles, workers, environments, ports. |
| **Configuration** | [EN](getting-started/configuration.md) | [RU](getting-started/configuration.ru.md) | Balancer config.json, env overrides, nginx and WebUI settings. |
| **Hardware presets** | [EN](getting-started/hardware-presets.md) | [RU](getting-started/hardware-presets.ru.md) | Ready .env presets for RTX 30xx/40xx/50xx and A10. |

## Architecture

How routing, sharding and inference work inside.

| Document | English | Russian | What it covers |
|---|---|---|---|
| **Balancing guide** | [EN](architecture/balancing-guide.md) | [RU](architecture/balancing-guide.ru.md) | Scoring, backend selection, session and model affinity. |
| **Backend type isolation** | [EN](architecture/backend-type-isolation.md) | [RU](architecture/backend-type-isolation.ru.md) | How llama.cpp and Ollama backends coexist in one cluster. |
| **RPC coordinator** | [EN](architecture/rpc-coordinator.md) | [RU](architecture/rpc-coordinator.ru.md) | Distributed inference over llama.cpp RPC: roles, state, limits. |
| **Virtual models** | [EN](architecture/virtual-router.md) | [RU](architecture/virtual-router.ru.md) | Alias-on-pool virtual models and routing rules. |
| **Layer sharding** | [EN](architecture/layer-sharding.md) | [RU](architecture/layer-sharding.ru.md) | Splitting layers across backends, containers, parameters. |
| **RPC layout rules** | [EN](architecture/rpc-layout-rules.md) | [RU](architecture/rpc-layout-rules.ru.md) | Rules learned from real crashes: same commit, arch, kill switch. |
| **RPC link bandwidth** | [EN](architecture/rpc-link-bandwidth.md) | [RU](architecture/rpc-link-bandwidth.ru.md) | How link speed between nodes is measured and where it is stored. |
| **C-bridge API** | [EN](architecture/bridge-api.md) | [RU](architecture/bridge-api.ru.md) | Go <-> C bridge to llama.cpp: functions, structures, contract. |
| **Balancer <-> cppworker contract** | [EN](architecture/balancer-cppworker-contract.md) | — | Wire contract between the balancer and the worker, field by field. |

## Operations

Run, update and debug a live cluster.

| Document | English | Russian | What it covers |
|---|---|---|---|
| **Deployment stack** | [EN](operations/deployment-stack.md) | [RU](operations/deployment-stack.ru.md) | One compose file, profiles, tokens, registry, diagnostics. |
| **Worker behind a remote balancer** | [EN](operations/deployment-remote-balancer.md) | [RU](operations/deployment-remote-balancer.ru.md) | cppworker + agent on one machine, balancer on another. |
| **Compose files guide** | [EN](operations/docker-compose-guide.md) | [RU](operations/docker-compose-guide.ru.md) | Which compose file is canonical and what the historical ones do. |
| **Agent deployment** | [EN](operations/agent-deployment.md) | [RU](operations/agent-deployment.ru.md) | Metrics agent: GPU and CPU modes, systemd, Windows, WSL2. |
| **Donor node update** | [EN](operations/donor-update.md) | [RU](operations/donor-update.ru.md) | Updating and rolling back the donor node image. |
| **Donor VRAM lifecycle** | [EN](operations/rpc-donor-vram-lifecycle.md) | [RU](operations/rpc-donor-vram-lifecycle.ru.md) | When VRAM is released on layout nodes and what stays occupied. |
| **Troubleshooting** | [EN](operations/troubleshooting.md) | [RU](operations/troubleshooting.ru.md) | Symptoms, causes and fixes collected from live incidents. |
| **Deployment incidents** | [EN](operations/deployment-troubleshooting.md) | [RU](operations/deployment-troubleshooting.ru.md) | Deployment traps and incident write-ups. |
| **Tools/tool_calls runbook** | [EN](operations/runbook-tools.md) | [RU](operations/runbook-tools.ru.md) | Step-by-step debugging of tool-call resets (scenarios A-G). |

## API and reference

Endpoint, env and metrics references.

| Document | English | Russian | What it covers |
|---|---|---|---|
| **REST API reference** | [EN](api/api.md) | [RU](api/api.ru.md) | All balancer endpoints, auth, WebSocket, Ollama/OpenAI compatibility. |
| **Environment variables** | [EN](api/env-vars.md) | [RU](api/env-vars.ru.md) | Single source of truth for every env var, default and effect. |
| **Metrics reference** | [EN](api/metrics.md) | [RU](api/metrics.ru.md) | GPU, system, Ollama and proxy metrics with units and meaning. |
| **Worker metrics collection** | [EN](api/cppworker-metrics-collection.md) | [RU](api/cppworker-metrics-collection.ru.md) | Embedded agent vs external poller: what collects what. |

## Features

What the cluster can do for clients.

| Document | English | Russian | What it covers |
|---|---|---|---|
| **Image generation** | [EN](features/image-generation.md) | [RU](features/image-generation.ru.md) | sd.cpp image backend, models, clients, VRAM profiles. |
| **Model parameters** | [EN](features/cppworker-model-params.md) | [RU](features/cppworker-model-params.ru.md) | n_ctx resolver, per-model profiles, RAM fallback, proxy surfaces. |
| **RPC benchmark (27B)** | [EN](features/rpc-benchmark-qwen38.md) | [RU](features/rpc-benchmark-qwen38.ru.md) | Measured layer sharding win on qwen3.8 27B: method and results. |
| **WebUI auth and roles** | [EN](features/webui-auth.md) | [RU](features/webui-auth.ru.md) | Sessions, roles, CSRF, break-glass token, nginx proxy. |
| **WebUI demo mode** | [EN](features/webui-demo.md) | [RU](features/webui-demo.ru.md) | Guest showcase of the whole scenario, enabled by WEBUI_DEMO_MODE only. |
| **Cline agent loop** | [EN](features/cline-agent-loop.md) | [RU](features/cline-agent-loop.ru.md) | Tool-call contract, readiness, tracing and verification. |

## Releases

What shipped and how it was checked.

| Document | English | Russian | What it covers |
|---|---|---|---|
| **v1.0 release notes** | [EN](releases/v1.0-release-notes.md) | [RU](releases/v1.0-release-notes.ru.md) | What v1.0 contains: working / partial / stubbed. |
| **v1.0-rc2 release notes** | [EN](releases/v1.0-rc2-release-notes.md) | [RU](releases/v1.0-rc2-release-notes.ru.md) | Changes since rc1 and how they were verified. |
| **v1.0 smoke checklist** | [EN](releases/v1.0-smoke-checklist.md) | [RU](releases/v1.0-smoke-checklist.ru.md) | Manual smoke checklist on A10 24 GB before the final tag. |

## Development

Conventions and CI for contributors.

| Document | English | Russian | What it covers |
|---|---|---|---|
| **Style compliance** | [EN](development/style-compliance.md) | [RU](development/style-compliance.ru.md) | Em-dash lint, i18n parity, docs sync rules. |
| **Self-hosted CI runner** | [EN](development/ci-self-hosted-runner.md) | [RU](development/ci-self-hosted-runner.ru.md) | Runner setup, labels, operations and troubleshooting. |

## Roadmap and session reports

Plans, ADR and working session reports (Russian, historical).

- [Roadmap and plan index](plans/README.md) (RU)
- [2026-08-04-settings-busy-and-streaming-cutoff](plans/2026-08-04-settings-busy-and-streaming-cutoff.md) (RU)
- [2026-09-23-admission-queue-and-sessions](plans/2026-09-23-admission-queue-and-sessions.md) (RU)
- [2026-09-23-multi-backend-placement-policy](plans/2026-09-23-multi-backend-placement-policy.md) (RU)
- [2026-09-25-handoff-r83-next-session](plans/2026-09-25-handoff-r83-next-session.md) (RU)
- [2026-09-25-memory-fit-subsystem](plans/2026-09-25-memory-fit-subsystem.md) (RU)
- [2026-09-25-placement-p4-load-kpi](plans/2026-09-25-placement-p4-load-kpi.md) (RU)
- [2026-09-25-qwen38-load-context-dup-and-infra](plans/2026-09-25-qwen38-load-context-dup-and-infra.md) (RU)
- [2026-09-26-estimator-unification-protocol](plans/2026-09-26-estimator-unification-protocol.md) (RU)
- [2026-09-26-step1b-fit-refusal-proposal](plans/2026-09-26-step1b-fit-refusal-proposal.md) (RU)
- [2026-09-27-image-generation-backend-plan](plans/2026-09-27-image-generation-backend-plan.md) (RU)
- [2026-10-03-image-ui-gguf-style-unification](plans/2026-10-03-image-ui-gguf-style-unification.md) (RU)
- [2026-10-06-image-tool-catalog-autoload](plans/2026-10-06-image-tool-catalog-autoload.md) (RU)
- [2026-10-07-model-distribution-multi-gpu-next-session](plans/2026-10-07-model-distribution-multi-gpu-next-session.md) (RU)
- [2026-10-13-rpc-full-mode](plans/2026-10-13-rpc-full-mode.md) (RU)
- [2026-10-14-donor-update-and-layout](plans/2026-10-14-donor-update-and-layout.md) (RU)
- [2026-10-14-handoff-r115-r116](plans/2026-10-14-handoff-r115-r116.md) (RU)
- [2026-10-14-handoff-r117](plans/2026-10-14-handoff-r117.md) (RU)
- [2026-10-14-handoff-r118](plans/2026-10-14-handoff-r118.md) (RU)
- [2026-10-14-model-settings-refactor-r116](plans/2026-10-14-model-settings-refactor-r116.md) (RU)
- [2026-10-16-session-report](plans/2026-10-16-session-report.md) (RU)
- [2026-10-17-qwen38-flash-next-a10](plans/2026-10-17-qwen38-flash-next-a10.md) (RU)
- [2026-10-18-inworker-node-r172](plans/2026-10-18-inworker-node-r172.md) (RU)
- [2026-10-18-layout-on-reload-r174-check](plans/2026-10-18-layout-on-reload-r174-check.md) (RU)
- [2026-10-21-webui-auth](plans/2026-10-21-webui-auth.md) (RU)
- [2026-10-22-r220-r226-summary](plans/2026-10-22-r220-r226-summary.md) (RU)
- [2026-q3-production-ready-plan](plans/2026-q3-production-ready-plan.md) (RU)
- [2026-q3-roadmap](plans/2026-q3-roadmap.md) (RU)
- [2026-q3-session-f-quick-wins](plans/2026-q3-session-f-quick-wins.md) (RU)
- [b8-tensor-parallelism-plan](plans/b8-tensor-parallelism-plan.md) (RU)
- [bug-2026-07-31-reasoning-not-routed](plans/bug-2026-07-31-reasoning-not-routed.md) (RU)
- [pre-existing-test-failures](plans/pre-existing-test-failures.md) (RU)
- [round-17-multi-gpu-test-plan](plans/round-17-multi-gpu-test-plan.md) (RU)
- [round-18-balancer-architecture-audit](plans/round-18-balancer-architecture-audit.md) (RU)
- [round-18-p0.3-user-parallel](plans/round-18-p0.3-user-parallel.md) (RU)
- [round-20-ollama-api-coverage-audit](plans/round-20-ollama-api-coverage-audit.md) (RU)
- [round-22-balancer-general-audit](plans/round-22-balancer-general-audit.md) (RU)
- [round-35-cppworker-bundled-r35](plans/round-35-cppworker-bundled-r35.md) (RU)
- [round-35c-env-tunable-polling](plans/round-35c-env-tunable-polling.md) (RU)
- [rpc-model-distribution-plan](plans/rpc-model-distribution-plan.md) (RU)

## Archive

Historical material in its original language: audits, research notes, round reports, incident write-ups, early plans. Not part of the current documentation set.

Full list: [archive/README.md](archive/README.md)

---

Project README: [../README.md](../README.md). Russian: [../README.ru.md](../README.ru.md).
