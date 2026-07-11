# OllamaLegion — документация

> **Версия:** 2.0 (2026-06-22) — ревизия и консолидация  
> **Языки:** [English](en/README.md) | **Русский** (текущий)  
> **Roadmap:** [`../plans/README.md`](../plans/README.md) | **Audit:** [`audit-2026-06.md`](audit-2026-06.md)
>
> 📋 **Phase 8 (2026-07-11):** полный набор документации теперь доступен на двух языках.
> См. [`en/README.md`](en/README.md) для English версии всех 15+ документов.

OllamaLegion — балансировщик нагрузки + WebUI для кластера Ollama и llama.cpp (через CppWorker) с мониторингом GPU/CPU/RAM/Disk/Network и умным распределением запросов.

---

## 🌍 Мультиязычность / Multilingual

| Язык | Директория | Описание |
|---|---|---|
| 🇷🇺 Русский | `./` (текущая) | Все документы на русском (исходный язык) |
| 🇬🇧 English | [`en/`](en/README.md) | All docs translated to English (15+ files) |

WebUI (Dashboard) также поддерживает переключение языка в реальном времени:
- 🌍 В правом верхнем углу → language switcher
- 🇬🇧 English (`en.js`) / 🇷🇺 Русский (`ru.js`) — автоопределение по `navigator.language`
- 💾 Выбор сохраняется в `localStorage` (key: `ollamalegion_lang`)
- Тесты parity: `internal/api/lint_css_i18n_test.go::TestI18nKeyParity_EN_RU`

---

## 📚 Содержание

### Основные разделы

| Документ | English | Описание |
|---|---|---|
| [**installation.md**](installation.md) | [en/installation.md](en/installation.md) | Требования, Docker, локальная сборка, stub-режим |
| [**deployment.md**](deployment.md) | [en/deployment.md](en/deployment.md) | Docker Compose, bundled-стек, production-конфигурация, масштабирование |
| [**agent-deployment.md**](agent-deployment.md) | [en/agent-deployment.md](en/agent-deployment.md) | Развёртывание агента CPU/GPU (включая Windows + WSL2) |
| [**api.md**](api.md) | [en/api.md](en/api.md) | REST API + WebSocket + CppWorker API + Ollama-совместимость |
| [**cppworker-model-params.md**](cppworker-model-params.md) | [en/cppworker-model-params.md](en/cppworker-model-params.md) | n_ctx resolver, Per-Model Profiles, RAM fallback, Ollama ↔ OpenAI proxy |
| [**backend-type-isolation.md**](backend-type-isolation.md) | [en/backend-type-isolation.md](en/backend-type-isolation.md) | Изоляция Ollama vs llama.cpp бэкендов |
| [**metrics.md**](metrics.md) | [en/metrics.md](en/metrics.md) | Полный справочник всех метрик (GPU/System/Ollama/Proxy) |
| [**rpc-coordinator.md**](rpc-coordinator.md) | [en/rpc-coordinator.md](en/rpc-coordinator.md) | **P.1**: распределённый inference через RPC (production mode) |
| [**virtual-router.md**](virtual-router.md) | [en/virtual-router.md](en/virtual-router.md) | **P.2**: alias-on-pool virtual models |
| [**phase-8-rpc-coordinator.md**](phase-8-rpc-coordinator.md) | [en/phase-8-rpc-coordinator.md](en/phase-8-rpc-coordinator.md) | P.1 implementation log |
| [**phase-8-p3-research.md**](phase-8-p3-research.md) | [en/phase-8-p3-research.md](en/phase-8-p3-research.md) | P.3 research: real ggml/NCCL (post-1.0) |
| [**phase-7-style-compliance.md**](phase-7-style-compliance.md) | [en/phase-7-style-compliance.md](en/phase-7-style-compliance.md) | Phase 7 style guide (em-dash → ASCII) |
| [**troubleshooting.md**](troubleshooting.md) | [en/troubleshooting.md](en/troubleshooting.md) | FAQ + диагностика n_ctx + tools/tool_calls |
| [**runbook-tools.md**](runbook-tools.md) | [en/runbook-tools.md](en/runbook-tools.md) | **Детальный runbook** для диагностики tools/tool_calls (сценарии A-G) |
| [**audit-2026-06.md**](audit-2026-06.md) | — | Финальный аудит-отчёт (реализовано / осталось / ограничения) |

### API спецификации

| Документ | Описание |
|---|---|
| [`openapi.yaml`](openapi.yaml) | OpenAPI 3.0.3 спецификация |
| [`swagger.json`](swagger.json) | Swagger 2.0 спецификация |

### Архитектурные решения

| Документ | Описание |
|---|---|
| [`cline-skills/cppworker-gpu-e2e-routing-test.md`](cline-skills/) | Тестовый сценарий GPU routing |
| [`cline-skills/vulkan-scene-debug.md`](cline-skills/) | Vulkan scene debug |

---

## Краткая архитектура

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

**Ключевые компоненты:**

- **Load Balancer** (`internal/balancer/`) — proxy, селектор бэкенда, очередь, сессии, scoring.
- **Agent** (`internal/agent/`) — сбор метрик GPU/CPU/RAM/Disk/Network.
- **API** (`internal/api/`) — REST API + WebSocket.
- **CppWorker** (`cmd/cppworker/`) — сервер инференса на llama.cpp с Ollama-compat API.
- **WebUI** (`webui/`) — Dashboard, Monitor, Settings, Models, Agents.

Подробнее о реализации: [`audit-2026-06.md` §2](audit-2026-06.md#2-реализованные-архитектурные-блоки-с-привязкой-к-коду).

---

## Таблица портов

| Порт | Компонент | Описание |
|---|---|---|
| **18080** | Load Balancer | Ollama/OpenAI API Proxy |
| **18081** | Load Balancer | Management API + WebSocket `/ws/metrics` |
| **18083** | Web UI | Dashboard |
| **18032** | Agent | Метрики |
| **11434** | Ollama | Ollama API |
| **18091** | CppWorker | Ollama-compat + OpenAI (`/v1/*`) |
| **18092** | CppWorker (хост) | маппинг на 18091 внутри контейнера (bundled) |
| **8443/8444** | Load Balancer | HTTPS (TLS) |

---

## Алгоритм принятия решений (кратко)

```
HTTP Request → ServeHTTP()
  ├─ determineRequestBackendType(r):       [backend-type-isolation.md §4]
  │   /v1/* → llama_cpp, /api/* → main flow
  │
  ├─ selectBackend(model, bt):            [backend-selector.go]
  │   ├─ expandCandidates(allowedTypes)   [candidate.go]
  │   ├─ findBackendWithModel             (Model Affinity)
  │   ├─ selectByResources                (Resource-Aware)
  │   └─ selectFreeBackendAny             (fallback)
  │
  └─ proxyRequest(w, r, backendID):       [proxy_request.go]
      └─ IsModeCompatibleWithBackendType  (защита)
```

Подробнее: [`backend-type-isolation.md` §6](backend-type-isolation.md#6-поток-запроса-с-фильтрацией).

---

## Архитектурные ограничения (Known Limitations)

Зафиксированы и не планируются к реализации:

| Ограничение | Где задокументировано |
|---|---|
| Ollama `images[]` в `/api/generate` (cppworker не multimodal) | [`api.md`](api.md) |
| `context` (base64 KV-cache) между запросами (KV-cache не сохраняется) | [`cppworker-model-params.md`](cppworker-model-params.md) |
| `keep_alive` таймер unload (только `applyKeepAlive`) | [`api.md`](api.md) |
| Agent → Ollama runtime config (Ollama не имеет публичного API) | [`troubleshooting.md` §9](troubleshooting.md) |
| `ollama pull <name>` для cppworker → HTTP 501 (нет Ollama registry) | [`api.md`](api.md) |
| Точная `modelLoadFeasibility` (требует GGUF metadata parser) | [`metrics.md`](metrics.md) |
| `rpc_coordinator` / `virtual_router` режимы (каркас без pipeline execution) | [`rpc-coordinator.md`](rpc-coordinator.md), [`audit-2026-06.md` §3 KL-7](audit-2026-06.md) |

---

## Быстрые ссылки

- 🚀 [**Установка**](installation.md) — `git clone`, Docker, локальная сборка.
- ⚙️ [**Конфигурация**](deployment.md#3-production-конфигурация) — `config.json` + ENV.
- 🔧 [**Bundled-стек**](deployment.md#2-bundled-стек-cppworker-gpu--balancer--webui) — за 5 минут до production.
- 📖 [**API**](api.md) — все endpoints + curl примеры.
- 🛠 [**Troubleshooting**](troubleshooting.md) — частые проблемы + решения.
- 🐛 [**Tools debug runbook**](runbook-tools.md) — диагностика tools/tool_calls.

---

## Roadmap

См. [`../plans/README.md`](../plans/README.md) — единственный живой roadmap с задачами R-1…R-7.

| # | Задача | Приоритет | Статус |
|---|---|---|---|
| R-1 | Windows GPU metrics через nvidia-smi/WMI fallback | 🟢 P2 | ⬜ |
| R-2 | WebUI фильтры по типу бэкенда | 🟢 P2 | ⬜ |
| R-3 | Убрать условный t.Skip (proxy_streaming_test.go:638) | 🟢 P2 | ⬜ |
| R-4 | setup-wizard — полноценный выбор типа бэкенда | 🟡 P1 | ⬜ |
| R-5 | cocoindex.js — поддержка llama_cpp API | 🟢 P2 | Backlog (после 1.0) |
| R-6 | Endpoint POST /api/v1/cppworker/reset-reload-counter | 🟡 P1 | ⬜ |
| R-7 | Документация nctx-reload endpoints | ⚪ P3 | ⬜ (выполнено в [`cppworker-model-params.md` §7.2](cppworker-model-params.md)) |

---

## Лицензия

MIT License