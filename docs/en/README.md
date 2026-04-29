# OllamaLegion Documentation (English)

Welcome to the documentation for **OllamaLegion** — a high-performance load balancer for Ollama with WebUI, monitoring, and multi-backend support.

## Table of Contents

| Document | Description |
|----------|-------------|
| [Installation](../installation.md) | Step-by-step installation guide |
| [Configuration](../configuration.md) | All configuration parameters for balancer and agents |
| [Balancing Modes](../balancing-guide.md) | Description of balancing strategies: round-robin, resource-aware, model-affinity, session-stickiness |
| [Deployment](../deployment.md) | Docker Compose, Kubernetes, systemd |
| [Agent Deployment](../agent-deployment.md) | Monitoring agent setup and configuration |
| [API Reference](../api.md) | Full REST API description for the balancer |
| [Metrics](../ollamalegion-metrics.md) | Prometheus metrics and monitoring |
| [Troubleshooting](../troubleshooting.md) | Common issues and solutions |
| [Audit](../audit-tracking.md) | Audit system and request tracking |
| [OpenAPI Specification](../openapi.yaml) | Swagger/OpenAPI 3.0 |

## Quick Start

```bash
# Clone the repository
git clone https://github.com/BarsSky/ollamalegion.git
cd ollamalegion

# Start via Docker Compose
docker compose -f deployments/docker-compose.yml up -d

# Open WebUI
open http://localhost:8080
```

## Project Structure

```
ollamalegion/
├── cmd/
│   ├── balancer/     # Load balancer entry point
│   ├── agent/        # Monitoring agent entry point
│   └── monitor/      # Terminal monitor (TUI)
├── internal/
│   ├── api/          # REST API + WebSocket
│   ├── balancer/     # Balancing core
│   ├── agent/        # Metrics collection agent
│   └── config/       # Configuration loader
├── webui/            # Web interface (SPA)
├── docs/             # Documentation
│   ├── ru/           #   Russian version
│   └── en/           #   English version
├── deployments/      # Docker Compose, Kubernetes
├── scripts/          # Build and deploy scripts
└── tests/            # Integration tests
```

## Language Support

Documentation is available in multiple languages:
- English (current)
- [Русский](../ru/README.md)

WebUI supports:
- English
- Russian

To add a new language to the WebUI, see the [i18n guide](../../webui/js/i18n/README.md).

---

[Back to main page](../../README.md)