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

## 2. Backend Selection Algorithm (v2)

### 2.1 Multi-Stage Selection (4 stages + pre-step)

The `selectBackend()` method in `internal/balancer/backend_selector.go` implements 4-stage logic on top of `expandCandidates()` (plus a pre-step for Model Replication).  
Stages 2–4 are optional and depend on configuration (`SyncModelLoad.Enabled`, `Prewarm`, etc.).

#### 2.1.1 `expandCandidates(modelName)` — grouping backends by priority

The `expandCandidates()` function analyzes all healthy backends and groups them into 4 priorities:

| Priority | Name | Condition |
|-----------|----------|---------|
| **P1** | **LOADED** | Model in memory, `loadRatio < triggerLoadThreshold` |
| **P2** | **WARMING** | Model in `WarmingUpModels` (being loaded) |
| **P3** | **FREE** | Free slots, model not loaded |
| **P4** | **FALLBACK** | All healthy backends with metrics |

```text
          Request arrives
                    │
                    ▼
    ┌───────────────────────────────────────┐
    │      expandCandidates(modelName)       │
    │  ┌─────────────────────────────────┐   │
    │  │ P1: LOADED (model in memory)  │   │
    │  │ P2: WARMING (model loading)   │   │
    │  │ P3: FREE (slots available)    │   │
    │  │ P4: FALLBACK (any healthy)    │   │
    │  └─────────────────────────────────┘   │
    └──────────────────┬────────────────────┘
                       ▼
    ┌───────────────────────────────────────┐
    │           selectBackend(model)         │
    │                                        │
    │  Pre-step: Model Replication           │
    │    ├─ Model in a replication group?    │
    │    └─ Yes → pick from the replicas     │
    │                                        │
    │  Stage 1: Model Affinity (P1 — LOADED) │
    │    ├─ Best backend from P1 by score    │
    │    ├─ Check loadRatio < threshold      │  (default 80%)
    │    └─ If OK → return the backend       │
    │                                        │
    │  Stage 2: Model Warming (P2)           │
    │    ├─ Find a backend being warmed up   │
    │    ├─ Check ETA < syncTimeout          │  (default 120s)
    │    └─ Wait for readiness (polling)     │
    │                                        │
    │  Stage 3: Sync Model Load (P3 — FREE)  │
    │    ├─ If SyncModelLoad.Enabled         │
    │    ├─ Start loading on a free backend  │
    │    └─ Wait for readiness (timeout)     │
    │                                        │
    │  Stage 4: Resource Scoring (P4)        │
    │    └─ selectByResources() →            │
    │       calculateScore() /               │
    │       calculateScoreSimple()           │
    └──────────────────┬────────────────────┘
                       ▼
    ┌───────────────────────────────────────┐
    │           Decision Router              │
    │  ┌── Backend Ready → Proxy            │
    │  ├── Model Loading → Wait/Poll        │
    │  ├── No Capacity → Enqueue            │
    │  └── Queue > 90% → 503 (Backpressure) │
    └───────────────────────────────────────┘
```

#### 2.1.2 `dispatchWithModelLoad()` — asynchronous model loading

A separate mechanism that initiates loading of a model onto the free backend with the most free VRAM:

1. Calls `expandCandidates()` to obtain P3 (FREE)
2. Selects the backend with the maximum `GPU.MemoryFree`
3. Starts `warmupModel()` via POST `/api/pull`
4. Adds the model to `WarmingUpModels` with a `ModelLoadTimeout` deadline (default 120s)
5. Returns `backendID` and `deadline` for waiting

This mechanism is used for rebalancing and preloading.

### 2.2 Extended Scoring Formula (calculateScore v2)

Scoring behavior is controlled by the **`useEnhancedScoring`** flag in `balancing`:

- **`useEnhancedScoring: true`** (default) — the full v2 formula that accounts for the model, the queue and errors
- **`useEnhancedScoring: false`** (legacy) — simplified formula (backward compatibility)

#### Simplified formula (`calculateScoreSimple`, default)

```
Score = (
    gpuFreePercent        * 0.30  +    // 30% — GPU load
    vramFreePercent       * 0.20  +    // 20% — free VRAM
    cpuFreePercent        * 0.15  +    // 15% — free CPU
    − requestPenalty                     // penalty for active requests
) * weight                             // Backend weight multiplier
```

#### Full v2 formula (`calculateScore`, useEnhancedScoring=true)

```
Score = (
    gpuFreePercent        * 0.30  +    // 30% — GPU load
    vramFreePercent       * 0.20  +    // 20% — free VRAM
    cpuFreePercent        * 0.15  +    // 15% — free CPU
    modelLoadedBonus      * wLoaded +  // weight from config (default 0.15)
    predictionBonus       * wPred   +  // weight from config (default 0.10)
    modelCapacityScore               +   // capacity for models
    − requestPenalty                     // penalty for active requests
    − queueDepthPenalty    * wQueue −  // weight from config (default 0.05)
    − errorRatePenalty     * wError    // weight from config (default 0.05)
) * weight                             // Backend weight multiplier
```

**Additional v2 components (only when useEnhancedScoring=true):**
- **modelLoadedBonus** — bonus for the number of loaded models (`ModelAlreadyLoaded * 10.0 * count`)
- **queueDepthPenalty** — penalty for global queue depth (`QueueDepthPenalty * len(queue)`)
- **errorRatePenalty** — penalty for a high error rate (`ErrorRatePenalty * errorRate * 100`, when `TotalAttempts > 10`)
- **predictionBonus** — bonus/penalty based on a critical-state prediction

**Weight configuration (`balancing.scoring`):**
```json
{
  "scoring": {
    "modelAlreadyLoaded": 0.15,
    "modelLoadingCost": 0.10,
    "queueDepthPenalty": 0.05,
    "errorRatePenalty": 0.05,
    "predictionBonus": 0.10
  }
}
```

### 2.3 Headroom Reservation

On every backend a percentage of GPU memory is reserved as hot headroom.
A backend is blocked for new requests if VRAM usage > (100 − headroom_percent)%.

Configuration: `balancing.resource_reservation.gpu_headroom_percent: 15`.

### 2.5 Health Filters (extended)


A backend is considered healthy if:
- The last heartbeat is not older than the timeout
- GPU usage < `gpuMaxUsage` (default 90%)
- VRAM usage < `vramMaxUsage` (default 85%)
- CPU usage < `cpuMaxUsage` (default 80%)
- RAM usage < `ramMaxUsage` (default 85%)
- Disk free > `minFreeDisk` (default 10240 MB)
- **VRAM usage does not exceed the headroom reserve (default 85%)**


---

## 3. Step-by-Step Balancing Scenarios

### 3.1 Scenario 1: One user — one model

```
Time: t0

┌─────────────────────┐
│ Backend: ollama-1   │
│ VRAM: 0 / 24 GB     │
│ Active: 0 / 8       │
│ Models: []          │
└─────────────────────┘
┌─────────────────────┐
│ Backend: ollama-2   │
│ VRAM: 0 / 24 GB     │
│ Active: 0 / 8       │
│ Models: []          │
└─────────────────────┘

Action: User A → request for model llama3.1 (8 GB)

Result:
1. No session → a new one is created
2. Model affinity: the model is not loaded anywhere → skip
3. Resource-aware fallback:
   - ollama-1: score 1.0 (everything free, lastUsed far away)
   - ollama-2: score 1.0 (everything free, lastUsed recent)
   - Tie-breaking: ollama-1 (older lastUsed)
4. Capacity check: 0/8 < 1, maxModels not exceeded
5. The request goes to ollama-1

After:
┌─────────────────────┐
│ Backend: ollama-1   │
│ VRAM: 8 / 24 GB     │
│ Active: 1 / 8       │
│ Models: [llama3.1] │
│ Sessions: [sess-A]  │
└─────────────────────┘
```

### 3.2 Scenario 2: Two users — one model

```
Time: t1 (after scenario 1)

┌─────────────────────┐
│ Backend: ollama-1   │
│ VRAM: 8 / 24 GB     │
│ Active: 1 / 8       │
│ Models: [llama3.1] │
│ Sessions: [sess-A]  │
└─────────────────────┘
┌─────────────────────┐
│ Backend: ollama-2   │
│ VRAM: 0 / 24 GB     │
│ Active: 0 / 8       │
│ Models: []          │
└─────────────────────┘

Action: User B → request for model llama3.1 (8 GB)

Result (CURRENT LOGIC):
1. No session → a new one is created
2. Model affinity: the model is loaded on ollama-1
3. Check ollama-1: healthy? Yes. active < max? 1/8 → Yes
4. RETURN ollama-1 (sticky to the same backend)

After:
┌─────────────────────┐
│ Backend: ollama-1   │
│ VRAM: 8 / 24 GB     │ ← the model is already loaded, no
│ Active: 2 / 8       │   additional VRAM is taken
│ Models: [llama3.1] │
│ Sessions: [sess-A,  │
│            sess-B]  │
└─────────────────────┘

⚠️ LIMITATION: ollama-2 stays idle even though it could
   serve in parallel. This is due to Model Affinity:
   the balancer prefers to use the already loaded model
   to avoid an extra load on ollama-2.
```

**Ideal scenario (to be implemented):**

```
If load on the first backend > threshold (for example, 50% of maxConcurrent):
  → look for a second backend where the model can be loaded
  → load the model onto ollama-2
  → create sess-B on ollama-2
```

### 3.3 Scenario 3: Three users — one model (queue)

```
Time: t2

┌─────────────────────┐
│ Backend: ollama-1   │
│ Active: 8 / 8       │ ← fully loaded
│ Models: [llama3.1] │
└─────────────────────┘
┌─────────────────────┐
│ Backend: ollama-2   │
│ Active: 2 / 8       │
│ Models: [gemma2]    │
└─────────────────────┘

Action: User C → request for model llama3.1 (8 GB)

Result:
1. No session → a new one is created
2. Model affinity: llama3.1 on ollama-1
3. Check ollama-1: active 8/8 → NO room
4. Resource-aware: ollama-2 healthy, but the model is NOT loaded
5. Check capacity of ollama-2: active 2/8 → room available
6. RETURN ollama-2 (fallback without affinity)

After:
┌─────────────────────┐
│ Backend: ollama-2   │
│ VRAM: 8 + 6 = 14 GB │ ← llama3.1 is loading
│ Active: 3 / 8       │
│ Models: [gemma2,    │
│          llama3.1]  │
│ Sessions: [sess-C]  │
└─────────────────────┘
```

**If both backends are full at maxConcurrent:**

```
Time: t3

┌─────────────────────┐
│ Backend: ollama-1   │
│ Active: 8 / 8       │
│ Models: [llama3.1] │
└─────────────────────┘
┌─────────────────────┐
│ Backend: ollama-2   │
│ Active: 8 / 8       │
│ Models: [gemma2]    │
└─────────────────────┘

Action: User D → request for llama3.1

Result:
1. No session → a new one is created
2. Model affinity: ollama-1 is fully busy → skip
3. Resource-aware: both are fully busy
4. Check capacity: neither passes
5. → PUT INTO QUEUE

Queue:
┌─────────────────────────────────────────────┐
│ Queue:                                      │
│  1. [llama3.1, sess-D]  status: pending   │
│     wait: 0s                                │
└─────────────────────────────────────────────┘
```

**Queue release:**

```
Time: t4 — user A finished the request

1. ollama-1: active 7/8
2. QueueManager checks the queue
3. Request D matches affinity (llama3.1 on ollama-1)
4. Dispatch: request D is sent to ollama-1
5. Session D is bound to ollama-1
```

### 3.4 Scenario 4: Four users — two models (VRAM contention)

```
Time: t0

┌─────────────────────┐
│ Backend: ollama-1   │
│ VRAM: 24 GB         │
│ Models: []          │
└─────────────────────┘
┌─────────────────────┐
│ Backend: ollama-2   │
│ VRAM: 24 GB         │
│ Models: []          │
└─────────────────────┘

t1: User A → llama3.1 (8 GB) → ollama-1
t2: User B → llama3.1 (8 GB) → ollama-1 (affinity)

After t2:
┌─────────────────────┐
│ Backend: ollama-1   │
│ VRAM: 8 / 24 GB     │
│ Active: 2 / 8       │
│ Models: [llama3.1] │
└─────────────────────┘

t3: User C → gemma2 (6 GB) → ???

Result:
1. Model affinity: gemma2 is not loaded anywhere → skip
2. Resource-aware:
   - ollama-1: score = (GPU free) * 0.35 + ...
              = healthy, 2/8 active, VRAM 8/24
   - ollama-2: score = healthy, 0/8 active, VRAM 0/24
   → ollama-2 wins (it is freer)
3. Request goes to ollama-2

After t3:
┌─────────────────────┐
│ Backend: ollama-1   │
│ VRAM: 8 / 24 GB     │
│ Active: 2 / 8       │
│ Models: [llama3.1] │
└─────────────────────┘
┌─────────────────────┐
│ Backend: ollama-2   │
│ VRAM: 6 / 24 GB     │
│ Active: 1 / 8       │
│ Models: [gemma2]    │
└─────────────────────┘

t4: User D → mistral (7 GB) → ???

Result:
1. Model affinity: mistral is not loaded anywhere
2. Resource-aware:
   - ollama-1: VRAM free = 16 GB, active = 2/8
   - ollama-2: VRAM free = 18 GB, active = 1/8
   → ollama-2 wins on score (it is freer)
3. Capacity check: 6 + 7 = 13 ≤ 24? Yes.
   → Request goes to ollama-2

After t4:
┌─────────────────────┐
│ Backend: ollama-2   │
│ VRAM: 13 / 24 GB    │
│ Active: 2 / 8       │
│ Models: [gemma2,    │
│          mistral]   │
└─────────────────────┘

t5: User E → llama3.1 (8 GB) → ???

Result:
1. Model affinity: llama3.1 on ollama-1
2. ollama-1: active 2/8 → room available
3. → Request goes to ollama-1

⚠️ Potential problem: if both backends were loaded
   and ollama-2 ran out of VRAM (for example, 6+7+8 > 24),
   the request would land in the queue.
```

---

<!--CHUNK4-->


