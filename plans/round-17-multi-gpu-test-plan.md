# Round 17 — Multi-GPU / Multi-Backend Test Plan

> **Дата:** 2026-07-31
> **Автор:** Round 16 code review (после v0.5.2)
> **Статус:** ⬜ NOT STARTED
> **Контекст:** проверка работы остальных режимов у балансера, когда идёт
> распределение большой модели между несколькими видеокартами на разных бэкендов.

---

## 1. Inventory: что уже реализовано в коде

Прежде чем планировать, что тестировать — нужно знать, что **уже** работает,
что в плане, а что **не существует**.

### 1.1 Single-machine multi-GPU (внутри одного cppworker)

| Режим | Файлы | Статус |
|-------|-------|--------|
| **Auto gpu_layers** — auto-расчёт числа GPU-слоёв под VRAM | `internal/cppbackend/auto_offload.go`, `auto_tune_nctx.go` | ✅ Production (v0.4.11+) |
| **GPUManager.CalculateTensorSplit** — расчёт `tensor_split` ratio (vram-ratio, round-robin, manual) | `internal/cppbackend/gpu_distribution.go` | ✅ Production |
| **TensorSplit** field в `LoadModelOpts` и `loadWithParamsRequest` | `internal/cppbackend/backend.go:1619`, `cmd/cppworker/types.go:151` | ✅ Production |
| **llama.cpp native tensor_split** — реально делит тензоры между GPU | `c/bridge/bridge.c:bridge_load_model` | ✅ Production |
| **VRAM tracking per-GPU** | `internal/cppbackend/gpu_distribution.go:GPUManager.gpuUsage` | ✅ Production |
| **RPC-worker TP stub** (sharded model + coordinator + AllReduceConcat/Sum) | `internal/rptensor/`, `internal/rpccoordinator/`, `internal/rpcworker/handlers_tp.go` | 🟡 Stub (deterministic bytes), real llama.cpp TP — НЕ подключён |
| **B8 plan** (реальный TP через ggml) | `plans/b8-tensor-parallelism-plan.md` | ⬜ Plan, 20-30 дней работы, post-1.0 |

### 1.2 Multi-backend (несколько cppworker'ов, каждый на своей машине/GPU)

| Режим | Файлы | Статус |
|-------|-------|--------|
| **cppworker auto-registration** в balancer через `POST /api/v1/backends` | `cmd/cppworker/balancer_register.go` | ✅ Production |
| **Virtual model** (alias → pool of backends) | `internal/virtualmodel/` | ✅ Production (Phase 8) |
| **Balancer routing** (least-loaded, round-robin, sticky-session) | `internal/balancer/selector.go` | ✅ Production |
| **Health-aware routing** (skip unhealthy backends) | `internal/balancer/health.go` | ✅ Production |
| **Multi-machine same model** (replication: 70B loaded on 2 backends, balancer picks one) | via virtual models | ✅ Production (но **нет explicit replication config**, auto-detected через pool) |
| **Pipeline parallelism** (split layers across RPC workers) | `internal/rpccoordinator/`, `internal/rpcworker/handlers_tp.go` | 🟡 Stub (RPC works, no real matrix sharding) |

### 1.3 Ключевые выводы inventory

- **Single-machine tensor_split**: ✅ полностью работает (auto + manual). Это самый зрелый multi-GPU режим.
- **Single-machine n_parallel** (parallel sequences, не parallel workers): ✅ production (Round 13).
- **BatchedScheduler** (true parallel llama_decode): 🟡 Round 15.0 functional baseline, v0.5.0 — есть race condition (1 lock сериализует всё).
- **Multi-backend replication** (model loaded on N backends): ✅ работает через virtual models.
- **Cross-machine tensor parallelism** (TP across RPC workers): 🟡 stub, real impl — B8 plan, post-1.0.
- **Cross-machine pipeline parallelism** (PP across RPC workers): 🟡 stub, real impl — post-1.0.

---

## 2. Что тестировать в Round 17

### 2.1 Режимы и подрежимы

У балансера есть **3 основных режима multi-GPU/multi-backend** распределения
большой модели. Для каждого — набор сценариев.

#### Режим A: Single-machine tensor_split (multi-GPU внутри одного backend)

Когда: модель не влезает в одну VRAM, есть несколько GPU в одной машине.

**Что происходит**:
- cppworker получает `LoadModelOpts{TensorSplit: [0.5, 0.5]}`
- llama.cpp делит тензоры модели между GPU пропорционально ratio
- inference использует все GPU одновременно
- Latency = O(1) per layer (все GPU работают параллельно)

**Подрежимы A**:
- A1. **Auto vram-ratio**: cppworker сам считает ratio на основе free VRAM
- A2. **Manual ratio**: клиент передаёт `tensorSplit: [0.7, 0.3]` явно
- A3. **Single GPU only** (контрольная): без tensorSplit → всё на GPU 0

#### Режим B: Multi-backend replication (multi-machine, single-model-replica-per-backend)

Когда: модель помещается в одну машину, но нужно больше throughput (parallel requests).

**Что происходит**:
- 2-3 cppworker'а загружают одну и ту же модель
- Balancer знает обо всех через `POST /api/v1/backends`
- Virtual model `gemma-4-it` указывает на pool из N backends
- На каждый запрос balancer выбирает least-loaded backend (round-robin / least-loaded)
- Latency = single-machine latency, но throughput × N

**Подрежимы B**:
- B1. **Round-robin** между N backends
- B2. **Least-loaded** (на основе active queries)
- B3. **Sticky-session** (тот же client → тот же backend для cache locality)
- B4. **Health-aware failover** (unhealthy backend → skip, next backend)

#### Режим C: Multi-backend virtual model (multi-machine, different GPUs per backend)

Когда: модель помещается в одну машину, разные машины имеют разные GPU
(например, машина 1 = 2×A10 24GB, машина 2 = 1×A100 80GB).

**Что происходит**:
- Каждый backend загружает модель со своими `gpuLayers` (auto или manual)
- На машине 1: model = 19GB → 1×A10 (partial offload + CPU)
- На машине 2: model = 19GB → 1×A100 (full GPU, лучшая скорость)
- Balancer выбирает backend с лучшей скоростью для данного запроса
- Latency = faster backend's latency, throughput распределяется

**Подрежимы C**:
- C1. **Auto heterogeneous** (каждый backend выбирает свой gpu_layers)
- C2. **GPU-aware scoring** (backend с A100 предпочитается для тяжёлых запросов)

#### Режим D: Multi-machine tensor parallelism (TP across RPC workers)

**Статус**: stub. Тестировать нечего в production. Только unit-тесты (`internal/rptensor/*_test.go`).

### 2.2 Модели для тестов

| Модель | Размер | GPU req | Цель |
|--------|--------|---------|------|
| **Qwen3-4B-Instruct** (Q4_K_M) | 2.4 GB | 1×6GB | Контрольная, всегда работает |
| **Qwen3-8B-Instruct** (Q4_K_M) | 4.5 GB | 1×6GB | Single-GPU контрольная для multi-GPU тестов |
| **Gemma-3-4B** (Q4_K_M) | 3.5 GB | 1×6GB | Альтернативная контрольная |
| **Qwen3-30B-A3B-Instruct** (Q4_K_M) | 18 GB | 1×24GB (A10) или 2×16GB split | **Главный multi-GPU тест** |
| **Llama-3-70B-Instruct** (Q4_K_M) | 40 GB | 2×24GB split (требует A10×2) или 1×80GB (A100) | Stretch goal |
| **Qwen3-235B-Instruct** (Q4_K_M) | 130 GB | 4×80GB (A100×4) или 8×A10 | Smoke test, multi-machine |

**Минимально необходимо для Round 17**:
- 1× A10 24GB (локально, уже есть)
- 1× A100 80GB (если доступен в кластере)
- 2× A10 24GB на одной машине для tensor_split A2

**Stretch goals** (отложить если hardware не доступен):
- Llama-3-70B через 2× A10
- Qwen3-235B через 4× A100

---

## 3. Тестовые сценарии

### 3.1 Single-machine tensor_split (Режим A)

#### Сценарий A1: Auto vram-ratio на 2×A10 24GB

**Предусловия**:
- 1 cppworker-gpu с `NVIDIA_VISIBLE_DEVICES=0,1` (2 GPU)
- Модель: Qwen3-30B-A3B (18GB) на диске
- Endpoint: `POST /api/models/load-with-params` без `tensorSplit`

**Шаги**:
1. Загрузить модель без `tensorSplit`:
   ```bash
   curl -X POST http://localhost:18092/api/models/load-with-params \
     -H "X-API-Token: $TOKEN" \
     -H "Content-Type: application/json" \
     -d '{"name":"qwen3-30b","path":"/app/models/Qwen3-30B-A3B-Q4_K_M.gguf"}'
   ```
2. Проверить `GET /api/v1/cppworker/config/runtime` → должно показать
   `tensorSplit: [...]` (auto-рассчитанный)
3. Проверить `nvidia-smi` → оба GPU должны иметь ~9GB used
4. Отправить inference request:
   ```bash
   curl -X POST http://localhost:18092/v1/chat/completions \
     -H "X-API-Token: $TOKEN" \
     -d '{"model":"qwen3-30b","messages":[{"role":"user","content":"Hello"}]}'
   ```
5. Проверить latency: должно быть < 2× single-GPU latency (идеал — 1×, т.к. parallel)

**Expected**:
- ✅ Модель загружается успешно
- ✅ `tensorSplit` автоматически = `[0.5, 0.5]` (равное деление)
- ✅ nvidia-smi показывает ~9GB на каждом GPU
- ✅ inference latency < 2× baseline
- ✅ output content корректный

**Verify**:
```python
import time, json, urllib.request
TOKEN = "..."
CPPWORKER = "http://localhost:18092"
# 1. Load
req = urllib.request.Request(f"{CPPWORKER}/api/models/load-with-params",
    data=json.dumps({"name":"qwen3-30b","path":"/app/models/Qwen3-30B-A3B-Q4_K_M.gguf"}).encode(),
    headers={"X-API-Token":TOKEN, "Content-Type":"application/json"})
t0 = time.time()
with urllib.request.urlopen(req, timeout=300) as r:
    print("Load:", json.loads(r.read()))
load_time = time.time() - t0
# 2. Check runtime config
with urllib.request.urlopen(f"{CPPWORKER}/api/v1/cppworker/config/runtime", timeout=10) as r:
    cfg = json.loads(r.read())
print("Runtime:", cfg)
assert cfg["loaded_models"][0]["tensorSplit"] is not None
# 3. Inference latency
t0 = time.time()
req = urllib.request.Request(f"{CPPWORKER}/v1/chat/completions",
    data=json.dumps({"model":"qwen3-30b","messages":[{"role":"user","content":"Say hi in 5 words"}]}).encode(),
    headers={"X-API-Token":TOKEN, "Content-Type":"application/json"})
with urllib.request.urlopen(req, timeout=60) as r:
    resp = json.loads(r.read())
print("Inference:", time.time() - t0, "s, content:", resp["choices"][0]["message"]["content"])
```

#### Сценарий A2: Manual ratio 0.7/0.3

**Отличие от A1**: клиент передаёт `"tensorSplit": [0.7, 0.3]`.

**Expected**:
- GPU 0 получает 70% слоёв, GPU 1 — 30%
- nvidia-smi: GPU 0 ~13GB, GPU 1 ~5.5GB
- Inference работает (с возможно большей latency чем A1, т.к. неравномерно)

#### Сценарий A3: Single-GPU control

**Отличие от A1**: `"tensorSplit": [1.0, 0.0]` (явно только GPU 0).

**Expected**:
- Только GPU 0 используется
- Это baseline для сравнения с A1, A2

### 3.2 Multi-backend replication (Режим B)

#### Сценарий B1: Round-robin между 2 backends

**Предусловия**:
- 2 cppworker'а на разных портах (на одной машине для теста):
  - backend-1: `:18092` (`NVIDIA_VISIBLE_DEVICES=0`)
  - backend-2: `:18093` (`NVIDIA_VISIBLE_DEVICES=1`)
- Оба загружают одну и ту же модель (Qwen3-8B Q4_K_M, 4.5GB)
- Balancer сконфигурирован с round-robin routing

**Шаги**:
1. Создать docker-compose с 2 cppworker services + 1 balancer
2. Загрузить модель на оба через `POST /api/v1/models/load` (с одинаковым name)
3. Зарегистрировать virtual model:
   ```bash
   curl -X POST http://balancer:18081/api/v1/virtual-models \
     -d '{"name":"qwen3-8b","backends":["backend-1","backend-2"],"strategy":"round-robin"}'
   ```
4. Отправить 10 inference requests
5. Проверить, что 5 запросов пошли на backend-1, 5 на backend-2

**Verify**:
```python
import time, json, urllib.request
TOKEN = "..."
BALANCER = "http://localhost:18080"
# Load model on both backends
for backend_url in ["http://backend-1:18092", "http://backend-2:18092"]:
    req = urllib.request.Request(f"{backend_url}/api/models/load",
        data=json.dumps({"name":"qwen3-8b","path":"/app/models/Qwen3-8B-Q4_K_M.gguf"}).encode(),
        headers={"X-API-Token":TOKEN, "Content-Type":"application/json"})
    urllib.request.urlopen(req, timeout=120)
# Create virtual model
req = urllib.request.Request(f"{BALANCER}/api/v1/virtual-models",
    data=json.dumps({"name":"qwen3-8b","backends":["backend-1","backend-2"],"strategy":"round-robin"}).encode(),
    headers={"X-API-Token":TOKEN, "Content-Type":"application/json"})
urllib.request.urlopen(req)
# Send 10 requests, count per backend
counts = {"backend-1": 0, "backend-2": 0}
for i in range(10):
    req = urllib.request.Request(f"{BALANCER}/v1/chat/completions",
        data=json.dumps({"model":"qwen3-8b","messages":[{"role":"user","content":f"q{i}"}]}).encode(),
        headers={"X-API-Token":TOKEN, "Content-Type":"application/json"})
    with urllib.request.urlopen(req, timeout=30) as r:
        resp = json.loads(r.read())
        # Balancer adds X-Backend header (?)
        backend_used = r.headers.get("X-Backend")
        if backend_used:
            counts[backend_used] = counts.get(backend_used, 0) + 1
print(counts)
# Round-robin: roughly 5/5
assert 3 <= counts["backend-1"] <= 7
assert 3 <= counts["backend-2"] <= 7
```

**Expected**:
- ✅ Оба backends получают запросы (3-7 каждый)
- ✅ Total throughput ≈ 2× single-backend
- ⚠️ Если один backend медленнее — он может стать bottleneck'ом

#### Сценарий B2: Least-loaded

**Отличие от B1**: strategy = "least-loaded".

**Expected**:
- Backend с меньшим количеством active queries получает следующий запрос
- При equal load → works like round-robin
- При unequal → равномерная нагрузка

#### Сценарий B3: Sticky-session

**Отличие от B1**: strategy = "sticky-session", заголовок `X-Session-ID`.

**Expected**:
- Запросы с одинаковым `X-Session-ID` идут на один backend (для cache locality)
- Запросы без session ID → round-robin

#### Сценарий B4: Health-aware failover

**Шаги**:
1. 2 backends, оба загружены
2. Один backend падает (kill -9)
3. Все запросы должны идти на второй backend (503 от упавшего → skip)
4. После recovery упавшего — round-robin resumes

**Expected**:
- ✅ Balancer пропускает unhealthy backend
- ✅ Запросы автоматически роутятся на healthy
- ✅ Время failover < 1 секунда (heartbeat interval)

### 3.3 Multi-backend heterogeneous GPU (Режим C)

#### Сценарий C1: A10 vs A100 — smart routing

**Предусловия**:
- Backend 1: 1× A10 24GB → загружает Qwen3-30B с auto gpu_layers (partial offload)
- Backend 2: 1× A100 80GB → загружает Qwen3-30B с gpu_layers=999 (full GPU)
- Оба зарегистрированы в balancer, virtual model `qwen3-30b` указывает на оба

**Шаги**:
1. Отправить запрос с `stream: true` → latency замер
2. Отправить batch из 10 запросов → throughput замер
3. Проверить routing: balancer должен предпочитать A100 для latency-sensitive (по умолчанию)

**Expected**:
- ✅ A100 backend получает большинство streaming requests (быстрее)
- ✅ A10 backend получает batched/background requests
- ✅ Общий throughput выше чем single-backend

### 3.4 Failure modes (важно для production)

#### Сценарий F1: Backend OOM

- Загрузить модель с n_ctx=128K на backend с 8GB VRAM
- Inference fails → backend помечает себя unhealthy
- Balancer переключает на следующий backend

**Expected**:
- ✅ Auto-recovery (backend перезагружает модель с меньшим n_ctx)
- ⚠️ Latency spike во время recovery (30-60s)

#### Сценарий F2: Cross-backend prompt context overflow

- Long prompt > single backend's max n_ctx
- Virtual model должна выбрать backend с достаточным n_ctx

**Expected**:
- ✅ Routing учитывает max_n_ctx per backend
- ⚠️ Если ни один backend не подходит → 413 от balancer

---

## 4. Пошаговый план реализации тестов

### Этап 17.1 — Подготовка окружения (1-2 дня)

**Цель**: иметь стабильный тестовый стенд с нужным hardware.

**Задачи**:
1. Определить доступный hardware (A10/A100/H100 count, VRAM)
2. Скачать нужные модели (Qwen3-8B, Qwen3-30B, Llama-3-70B если есть)
3. Создать `deployments/docker-compose.multi-gpu.yml` с 2-3 cppworker services
4. Создать `scripts/test-multi-gpu.sh` (Linux) / `.ps1` (Windows) — top-level test runner

**Acceptance**:
- `docker compose -f deployments/docker-compose.multi-gpu.yml up -d` стартует 2+ cppworker'а
- Balancer видит оба в `GET /api/v1/backends`

### Этап 17.2 — Single-machine tensor_split (Режим A) — 2-3 дня

**Цель**: проверить, что A1, A2, A3 работают как заявлено.

**Задачи**:
1. Написать `tests/multi_gpu/tensor_split_test.py` — параметризованный test для A1/A2/A3
2. Написать `tests/multi_gpu/verify_split.py` — проверяет nvidia-smi после load
3. Запустить на реальном hardware
4. Зафиксировать baseline latency

**Acceptance**:
- A1 (auto): pass, latency < 2× single-GPU
- A2 (manual): pass, nvidia-smi shows expected split
- A3 (single): pass, baseline for comparison

### Этап 17.3 — Multi-backend replication (Режим B) — 3-4 дня

**Цель**: проверить routing strategies на нескольких backends.

**Задачи**:
1. Создать `deployments/docker-compose.replication.yml` с 2-3 cppworker'ами
2. Написать `tests/multi_gpu/replication_test.py`:
   - B1: round-robin
   - B2: least-loaded
   - B3: sticky-session
   - B4: health-aware failover
3. Каждый тест: 100 requests, проверка distribution + latency
4. Документировать результаты в `docs/multi-backend-routing.md`

**Acceptance**:
- B1: 50/50 distribution ±10%
- B2: unequal load detected, rebalanced
- B3: same session → same backend
- B4: kill backend → 100% traffic на second backend < 1s

### Этап 17.4 — Multi-backend heterogeneous (Режим C) — 2-3 дня (если есть A100)

**Цель**: проверить smart routing по hardware capability.

**Задачи**:
1. Setup 1× A10 + 1× A100 (или симулировать через label "fast"/"slow")
2. Test: streaming requests → A100, batched → A10
3. Test: latency comparison

**Acceptance** (если A100 доступен):
- Streaming: A100 latency < A10 latency
- Throughput: balanced

**Acceptance** (fallback без A100):
- Skipped с TODO

### Этап 17.5 — Failure modes (F-серия) — 1-2 дня

**Цель**: проверить resilience.

**Задачи**:
1. F1: OOM recovery — запустить с моделью которая OOM'нет, проверить auto-recovery
2. F2: n_ctx overflow — long prompt на маленьком backend
3. Документировать поведение в `docs/multi-backend-failure-modes.md`

**Acceptance**:
- F1: backend recovers < 60s, balancer detects < 5s
- F2: graceful 413, не crash

### Этап 17.6 — Performance benchmarks + documentation — 2-3 дня

**Цель**: задокументировать всё, что нашли.

**Задачи**:
1. `benchmarks/multi_gpu_throughput.py` — comparative benchmarks
2. Обновить `docs/multi-gpu-deployment.md` (новый файл) с:
   - Когда использовать какой режим (decision tree)
   - Setup instructions
   - Performance expectations
3. Обновить README.md (он устарел, см. audit от 2026-07-31)
4. CHANGELOG entry

**Acceptance**:
- Документация покрывает все 3 режима (A, B, C)
- Decision tree: "I have N GPUs on M machines with K model. What to use?"

---

## 5. Что НЕ входит в Round 17

Чтобы не разрастаться:

- ❌ Cross-machine tensor parallelism (TP через RPC workers) — это B8 plan, post-1.0
- ❌ Cross-machine pipeline parallelism — post-1.0
- ❌ NCCL/AllReduce optimization — отдельная задача
- ❌ TPU поддержка
- ❌ Custom CUDA graphs

---

## 6. Risk matrix

| Риск | Impact | Mitigation |
|------|--------|------------|
| Hardware недоступен (нет 2×GPU на одной машине) | High | A1/A2 тестируем на 1 машине с 1 GPU (manual ratio), A3 baseline. Multi-backend (B/C) — через VM |
| Модели не скачиваются (license / недоступность) | Medium | Подготовить fallback (Qwen3-4B) |
| Flaky tests из-за timing (backend recovery ~60s) | Medium | Увеличить timeouts, retries |
| Тесты падают на production (реальные пользователи) | High | Запускать на staging, не на prod |
| B8 plan dependencies (если B8 начнётся) | Low | Round 17 — pre-B8, последовательно |

---

## 7. Success criteria для Round 17

✅ Все 4 стратегии (A1, B1, B2, B4) работают как описано
✅ Decision tree документирован
✅ `docs/multi-gpu-deployment.md` создан
✅ Performance baselines записаны
✅ Failure modes задокументированы
✅ Round 17 CHANGELOG entry

**Stretch** (если hardware позволяет):
- A2: manual ratio на 2×A10
- B3: sticky-session с Cline (cache locality тест)
- C1: A10 vs A100 latency comparison
- F-серия: OOM/n_ctx overflow

---

## 8. Следующие шаги (immediate)

1. **Этот план** — review с пользователем
2. Создать task list с конкретными sub-tasks
3. Начать с Этапа 17.1 (подготовка)
4. По завершении каждого этапа — production verification + commit

План готов. Хочешь обсудить scope, изменить приоритеты, или начинаем с Этапа 17.1?
