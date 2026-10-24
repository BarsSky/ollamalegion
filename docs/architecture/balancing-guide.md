# OllamaLegion: Load Balancing and Session Management

## Table of Contents

1. [General Architecture](#1-general-architecture)
2. [Backend Selection Algorithm (v2)](#2-backend-selection-algorithm-v2)
3. [Step-by-Step Balancing Scenarios](#3-step-by-step-balancing-scenarios)
4. [Session Management](#4-session-management)
5. [Model Lifecycle on a Backend](#5-model-lifecycle-on-a-backend)
6. [Queue Management](#6-queue-management)
7. [Monitor State](#7-monitor-state)
8. [RPC Model Distribution (Three Variants)](#8-rpc-model-distribution-three-variants)
9. [Current Limitations and Recommendations](#9-current-limitations-and-recommendations)
10. [Balancer Decision Algorithm](#10-balancer-decision-algorithm)
11. [Glossary](#11-glossary)

---

## 1. General Architecture

### 1.1 System Components

```
┌─────────────────┐     ┌─────────────────────┐     ┌─────────────────┐
│    Clients      │────▶│      Balancer       │────▶│    Backends     │
│ (WebUI, API)    │     │   (Port 18081/LB)   │     │ (Ollama 11434)  │
└─────────────────┘     └─────────────────────┘     └─────────────────┘
                               │                            ▲
                               │                            │
                               │                    ┌───────┴───────┐
                               │                    │    Agents     │
                               │                    │(GPU metrics)  │
                               │                    │ Heartbeat 3s  │
                               │                    └───────────────┘
                               │                            │
                               └────────────────────────────┘
                                    Push metrics every 5s
```

**Components:**

| Component | Responsibility |
|-----------|----------------|
| **Balancer** | HTTP proxy, backend selection, queue and session management, metrics collection |
| **Backend** | Ollama instance serving requests to `/api/generate`, `/api/chat` |
| **Agent** | Collects GPU/VRAM/CPU/RAM metrics and the list of loaded models, feeds them to the balancer |
| **WebUI** | Visual control, cluster state monitoring |

### 1.2 Request Flow

```text
Client                    Balancer                    Backend
  │                          │                           │
  │── POST /api/generate ───▶│                           │
  │                          │── 1. Get/create session   │
  │                          │                           │
  │                          │── 2. Select backend       │
  │                          │    (model affinity →      │
  │                          │    resource-aware)        │
  │                          │── 3. Check capacity       │
  │                          │    (VRAM, maxConcurrent)  │
  │                          │── 4. If no room →         │
  │                          │    put into queue         │
  │                          │                           │
  │                          │── POST /api/generate ───▶│
  │                          │                           │── model load
  │                          │                           │    (if not in VRAM)
  │                          │◀─ streaming response ─────│
  │◀─ streaming response ────│                           │
  │                          │── 5. Finish the request,  │
  │                          │    update metrics         │
```

---

<!--CHUNK2-->
