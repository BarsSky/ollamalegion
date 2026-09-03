# OllamaLegion — Configuration

> **This document is an index.** The actual configuration reference
> lives in:
> - 🇬🇧 **[English](en/configuration.md)** — full field-by-field reference
> - 🇷🇺 **[Русский](ru/configuration.md)** — полный справочник
>
> **For a quick operational overview**, see
> [`deployment.md` §3 Production Configuration](deployment.md#3-production-конфигурация).

---

## How to find what you need

| Question | Go to |
|---|---|
| "What env vars does the balancer accept?" | [`en/configuration.md` §Balancer ENV](en/configuration.md#balancer-env) |
| "What fields are in `config.json`?" | [`en/configuration.md` §config.json Fields](en/configuration.md#configjson-fields) |
| "How do I configure a cppworker?" | [`deployment.md` §cppworker env](deployment.md#env-флаги-cppworker) |
| "I have an RTX 4090, what do I change?" | [`en/hardware-presets.md`](en/hardware-presets.md) |
| "What are the operating modes?" | [`en/configuration.md` §Operating Modes](en/configuration.md#operating-modes) |
| "How do I enable AutoTune?" | [`en/configuration.md` §AutoTune](en/configuration.md#autotune) |
| "Per-model profiles (n_ctx, batch, kv_cache_type)" | [`cppworker-model-params.md`](cppworker-model-params.md) |
| "My setup doesn't work — troubleshooting" | [`troubleshooting.md`](troubleshooting.md) |
| "I'm migrating from a smaller GPU to a bigger one" | [`en/hardware-presets.md`](en/hardware-presets.md) (R58.2, R58.3) |

---

## Why this file exists

R59.2 audits (saved to `_diag/audit/`) found **4 broken links** to
`docs/configuration.md` that didn't exist:
- `installation.md:4`
- `deployment.md:262`
- `troubleshooting.md:82`
- `api.md:2361`

This index resolves those broken links without duplicating the
~800-line canonical reference docs.

**Last verified:** R59.11 (2026-09-03).
