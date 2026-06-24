# OllamaLegion — Documentation

> **Version:** 2.0 (2026-06-22) — consolidated revision  
> **Languages:** **English** (current) | [Русский](../README.md)  
> **Roadmap:** [`../../plans/README.md`](../../plans/README.md) | **Audit:** [`audit-2026-06.md`](audit-2026-06.md)

OllamaLegion is a load balancer + WebUI for Ollama and llama.cpp (via CppWorker) clusters with GPU/CPU/RAM/Disk/Network monitoring and intelligent request distribution.

---

## 📚 Table of Contents

### Main documents

| Document | Description |
|---|---|
| [**installation.md**](installation.md) | Requirements, Docker, local builds, stub mode |
| [**deployment.md**](deployment.md) | Docker Compose, bundled stack, production config, scaling |
| [**agent-deployment.md**](agent-deployment.md) | Agent deployment CPU/GPU (incl. Windows + WSL2) |
| [**api.md**](api.md) | REST API + WebSocket + CppWorker API + Ollama compatibility |
| [**cppworker-model-params.md**](cppworker-model-params.md) | n_ctx resolver, Per-Model Profiles, RAM fallback, Ollama ↔ OpenAI proxy |
| [**backend-type-isolation.md**](backend-type-isolation.md) | Ollama vs llama.cpp backend isolation |
| [**metrics.md**](metrics.md) | Complete metrics reference (GPU/System/Ollama/Proxy) |
| [**rpc-coordinator.md**](rpc-coordinator.md) | Variant B: distributed inference via RPC (skeleton) |
| [**troubleshooting.md**](troubleshooting.md) | FAQ + n_ctx diagnostics |
| [**runbook-tools.md**](runbook-tools.md) | **Detailed runbook** for tools/tool_calls diagnostics (scenarios A-E) |
| [**audit-2026-06.md**](audit-2026-06.md) | Final audit report (implemented / remaining / limitations) |

### API specs

| Document | Description |
|---|---|
| [`openapi.yaml`](openapi.yaml) | OpenAPI 3.0.3 specification |
| [`swagger.json`](swagger.json) | Swagger 2.0 specification |

---

## Architecture

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
                                                    └─────────────┘
```

**Key components:**

- **Load Balancer** (`internal/balancer/`) — proxy, backend selector, queue, sessions, scoring.
- **Agent** (`internal/agent/`) — collects GPU/CPU/RAM/Disk/Network metrics.
- **API** (`internal/api/`) — REST API + WebSocket.
- **CppWorker** (`cmd/cppworker/`) — llama.cpp inference server with Ollama-compat API.
- **WebUI** (`webui/`) — Dashboard, Monitor, Settings, Models, Agents.

See [`audit-2026-06.md` §2](audit-2026-06.md#2-implemented-architecture-blocks-with-code-references) for details.

---

## Ports

| Port | Component | Description |
|---|---|---|
| **18080** | Load Balancer | Ollama/OpenAI API Proxy |
| **18081** | Load Balancer | Management API + WebSocket `/ws/metrics` |
| **18083** | Web UI | Dashboard |
| **18032** | Agent | Metrics |
| **11434** | Ollama | Ollama API |
| **18091** | CppWorker | Ollama-compat + OpenAI (`/v1/*`) |
| **18092** | CppWorker (host) | mapping to 18091 inside container (bundled) |
| **8443/8444** | Load Balancer | HTTPS (TLS) |

---

## Known Limitations

| Limitation | Documented in |
|---|---|
| Ollama `images[]` in `/api/generate` (cppworker not multimodal) | [`api.md`](api.md) |
| `context` (base64 KV-cache) between requests (KV-cache not preserved) | [`cppworker-model-params.md`](cppworker-model-params.md) |
| `keep_alive` unload timer (only `applyKeepAlive`) | [`api.md`](api.md) |
| Agent → Ollama runtime config (Ollama has no public API) | [`troubleshooting.md` §9](troubleshooting.md) |
| `ollama pull <name>` for cppworker → HTTP 501 (no Ollama registry) | [`api.md`](api.md) |
| Accurate `modelLoadFeasibility` (requires GGUF metadata parser) | [`metrics.md`](metrics.md) |
| `rpc_coordinator` / `virtual_router` modes (skeleton without pipeline execution) | [`rpc-coordinator.md`](rpc-coordinator.md), [`audit-2026-06.md` §3 KL-7](audit-2026-06.md) |

---

## Roadmap

See [`../../plans/README.md`](../../plans/README.md) — single living roadmap with tasks R-1…R-7.

---

## License

MIT License