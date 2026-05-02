#!/usr/bin/env python3
"""
Расширенный нагрузочный тест балансера Ollama Legion — 18 сценариев.
Использование:
  cd tests/docker-load
  docker compose -f docker-compose.test.yml up -d --build
  python load_test_extended.py
  docker compose -f docker-compose.test.yml down -v
"""

import asyncio
import aiohttp
import json
import os
import sys
import time
import statistics
from dataclasses import dataclass, field
from datetime import datetime
from typing import Optional

# ── Config ──────────────────────────────────────────────────────────────────
BALANCER_PROXY = "http://localhost:18090"
BALANCER_API  = "http://localhost:18091"
RESULTS_DIR   = os.path.join(os.path.dirname(__file__), "results")
os.makedirs(RESULTS_DIR, exist_ok=True)

FIRST_BYTE_TIMEOUT = 5
REQUEST_TIMEOUT    = 12
LONG_TIMEOUT       = 30

# ── Data ────────────────────────────────────────────────────────────────────

@dataclass
class ReqResult:
    id: int
    scenario: str
    model: str
    status: int
    elapsed_ms: float
    success: bool
    error: str = ""
    target_backend: str = ""
    session_id: str = ""

@dataclass
class ScenarioReport:
    name: str
    desc: str
    total: int
    successful: int
    failed: int
    elapsed_total_s: float
    latency_min_ms: float = 0
    latency_avg_ms: float = 0
    latency_median_ms: float = 0
    latency_p95_ms: float = 0
    latency_max_ms: float = 0
    rps: float = 0
    backend_dist: dict = field(default_factory=dict)
    errors: list = field(default_factory=list)
    notes: list = field(default_factory=list)

# ── Helpers ─────────────────────────────────────────────────────────────────

async def api_get(path: str) -> dict:
    try:
        async with aiohttp.ClientSession() as s:
            async with s.get(f"{BALANCER_API}{path}", timeout=aiohttp.ClientTimeout(total=5)) as r:
                return await r.json()
    except Exception as e:
        return {"error": str(e)}

async def api_post(path: str, data: dict = None) -> dict:
    try:
        async with aiohttp.ClientSession() as s:
            async with s.post(f"{BALANCER_API}{path}", json=data or {},
                              timeout=aiohttp.ClientTimeout(total=5)) as r:
                return await r.json()
    except Exception as e:
        return {"error": str(e)}

async def set_hung_mode(backend: str, enable: bool) -> bool:
    ports = {"mock-a": "11434", "mock-b": "11435", "mock-c": "11436"}
    container = f"ollama-legion-test-mock-{backend[-1]}"
    compose_file = os.path.join(os.path.dirname(__file__), "docker-compose.test.yml")
    try:
        proc = await asyncio.create_subprocess_exec(
            "docker", "compose", "-f", compose_file, "exec", "-T", container,
            "curl", "-s", "-X", "POST", "http://localhost:11434/admin/hung",
            "-H", "Content-Type: application/json",
            "-d", json.dumps({"enable": enable}),
            stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.PIPE,
        )
        stdout, stderr = await proc.communicate()
        return proc.returncode == 0
    except Exception:
        return False

async def make_request(
    session: aiohttp.ClientSession,
    sem: asyncio.Semaphore,
    req_id: int, scenario: str, model: str,
    endpoint: str = "/api/generate",
    stream: bool = False,
    client_name: str = "",
    hung: bool = False,
    timeout: int = REQUEST_TIMEOUT,
    target_status: Optional[int] = None,
) -> ReqResult:
    async with sem:
        start = time.perf_counter()
        headers = {"Content-Type": "application/json"}
        if client_name:
            headers["X-Client-Name"] = client_name
            headers["User-Agent"] = f"{client_name}/Test/1.0"

        payload = {"model": model, "prompt": f"Test {scenario} req #{req_id}", "stream": stream}
        if hung:
            payload["hung"] = True

        try:
            async with session.post(
                f"{BALANCER_PROXY}{endpoint}", json=payload, headers=headers,
                timeout=aiohttp.ClientTimeout(total=timeout),
            ) as resp:
                if stream:
                    text = ""
                    async for line in resp.content:
                        text += line.decode(errors="replace")
                else:
                    text = await resp.text()
                elapsed = (time.perf_counter() - start) * 1000

                ok = resp.status == 200
                if target_status is not None:
                    ok = resp.status == target_status

                return ReqResult(
                    id=req_id, scenario=scenario, model=model, status=resp.status,
                    elapsed_ms=elapsed, success=ok,
                    target_backend=resp.headers.get("X-Backend-ID", ""),
                    session_id=resp.headers.get("X-Session-ID", ""),
                )
        except asyncio.TimeoutError:
            return ReqResult(id=req_id, scenario=scenario, model=model, status=0,
                             elapsed_ms=(time.perf_counter() - start) * 1000,
                             success=False, error="Timeout")
        except Exception as e:
            return ReqResult(id=req_id, scenario=scenario, model=model, status=0,
                             elapsed_ms=(time.perf_counter() - start) * 1000,
                             success=False, error=str(e))

def analyze_results(results: list[ReqResult], name: str, desc: str) -> ScenarioReport:
    successful = [r for r in results if r.success]
    failed = [r for r in results if not r.success]
    latencies = sorted([r.elapsed_ms for r in successful])
    dist = {}
    for r in successful:
        be = r.target_backend or "unknown"
        dist[be] = dist.get(be, 0) + 1
    sr = ScenarioReport(
        name=name, desc=desc, total=len(results), successful=len(successful),
        failed=len(failed), elapsed_total_s=sum(r.elapsed_ms for r in results) / 1000,
        errors=[{"id": f.id, "error": f.error} for f in failed],
    )
    if latencies:
        n = len(latencies)
        sr.latency_min_ms = latencies[0]
        sr.latency_avg_ms = sum(latencies) / n
        sr.latency_median_ms = latencies[n // 2]
        sr.latency_p95_ms = latencies[int(n * 0.95)] if n > 1 else latencies[-1]
        sr.latency_max_ms = latencies[-1]
        sr.rps = n / (latencies[-1] / 1000) if n > 1 else 0
    sr.backend_dist = dist
    return sr

async def wait_for_balancer(timeout: int = 45) -> bool:
    for i in range(timeout):
        try:
            async with aiohttp.ClientSession() as s:
                async with s.get(f"{BALANCER_API}/api/v1/health", timeout=aiohttp.ClientTimeout(total=2)) as r:
                    if r.status == 200:
                        print(f"  Balancer ready (after {i+1}s)")
                        return True
        except Exception:
            pass
        await asyncio.sleep(1)
    return False

# ── Scenarios ───────────────────────────────────────────────────────────────

async def s1_non_streaming_baseline():
    print("\n── S1: Non-streaming baseline (3 concurrent, 9 total) ──")
    sem = asyncio.Semaphore(3)
    async with aiohttp.ClientSession() as s:
        tasks = [make_request(s, sem, i, "S1", "llama3.2:3b", stream=False) for i in range(9)]
        results = await asyncio.gather(*tasks)
    sr = analyze_results(results, "S1", "Non-streaming, 3 concurrent")
    sr.notes = ["Ожидается: все 9 успешны, равномерное распределение, latency < 300ms"]
    return sr, results


async def s2_streaming_baseline():
    print("\n── S2: Streaming SSE baseline (3 concurrent, 9 total) ──")
    sem = asyncio.Semaphore(3)
    async with aiohttp.ClientSession() as s:
        tasks = [make_request(s, sem, i, "S2", "llama3.2:3b", stream=True) for i in range(9)]
        results = await asyncio.gather(*tasks)
    sr = analyze_results(results, "S2", "Streaming SSE, 3 concurrent")
    sr.notes = ["Ожидается: все 9 успешны, first-byte < 5s, все токены получены"]
    return sr, results


async def s3_hung_gpu_retry():
    print("\n── S3: GPU-hang simulation (mock-a hung, check retry) ──")
    print("  Enabling hung mode on mock-a...")
    await set_hung_mode("mock-a", True)
    await asyncio.sleep(1)
    sem = asyncio.Semaphore(3)
    async with aiohttp.ClientSession() as s:
        tasks = [make_request(s, sem, i, "S3", "llama3.2:3b", stream=True) for i in range(6)]
        results = await asyncio.gather(*tasks)
    print("  Disabling hung mode on mock-a...")
    await set_hung_mode("mock-a", False)
    await asyncio.sleep(1)
    sr = analyze_results(results, "S3", "GPU-hang на mock-a, first-byte timeout - retry")
    sr.notes = ["Ожидается: запросы на mock-a таймаутят; retry на mock-b/c; mock-a -> unhealthy"]
    return sr, results


async def s4_two_clients_same_ip():
    print("\n── S4: Two clients same IP - Cline + OpenWebUI ──")
    sem = asyncio.Semaphore(6)
    async with aiohttp.ClientSession() as s:
        tasks = [make_request(s, sem, i, "S4", "llama3.2:3b", stream=False, client_name="Cline") for i in range(3)]
        tasks += [make_request(s, sem, i, "S4", "llama3.2:3b", stream=False, client_name="OpenWebUI") for i in range(3, 6)]
        results = await asyncio.gather(*tasks)
    session_ids = set(r.session_id for r in results if r.session_id)
    sr = analyze_results(results, "S4", "2 клиента (Cline/OpenWebUI) с одного IP")
    sr.notes = [f"Уникальных сессий: {len(session_ids)} (ожидается 2)", "Ожидается: разные sessionID для разных clientName"]
    return sr, results


async def s5_model_affinity():
    print("\n── S5: Model affinity (3 models x 3 requests) ──")
    sem = asyncio.Semaphore(9)
    async with aiohttp.ClientSession() as s:
        tasks = [make_request(s, sem, i, "S5", "llama3.2:3b", stream=False) for i in range(3)]
        tasks += [make_request(s, sem, i, "S5", "qwen2.5:14b", stream=False) for i in range(3, 6)]
        tasks += [make_request(s, sem, i, "S5", "deepseek-r1:32b", stream=False) for i in range(6, 9)]
        results = await asyncio.gather(*tasks)
    model_dist = {}
    for r in results:
        if r.success:
            key = f"{r.model} -> {r.target_backend}"
            model_dist[key] = model_dist.get(key, 0) + 1
    sr = analyze_results(results, "S5", "Model affinity (llama3.2, qwen2.5, deepseek-r1)")
    sr.notes = [f"Распределение: {model_dist}", "Ожидается: каждая модель идёт на бэкенд где она загружена"]
    return sr, results


async def s6_queue_stress():
    print("\n── S6: Queue stress (12 requests, maxConcurrentReqs=2) ──")
    sem = asyncio.Semaphore(12)
    async with aiohttp.ClientSession() as s:
        tasks = [make_request(s, sem, i, "S6", "llama3.2:3b", stream=False, timeout=LONG_TIMEOUT) for i in range(12)]
        results = await asyncio.gather(*tasks)
    sr = analyze_results(results, "S6", "12 запросов, MaxConcurrentReqs=2 per backend")
    sr.notes = ["Ожидается: все 12 успешны (через очередь), без 503"]
    return sr, results


async def s7_backend_failure_recovery():
    print("\n── S7: Backend failure during load ──")
    print("  Phase 1: Normal load (3 requests)...")
    sem = asyncio.Semaphore(3)
    async with aiohttp.ClientSession() as s:
        r1 = await asyncio.gather(*[make_request(s, sem, i, "S7-p1", "llama3.2:3b", stream=False) for i in range(3)])
    await asyncio.sleep(1)
    print("  Phase 2: Hung mock-a + 3 requests...")
    await set_hung_mode("mock-a", True)
    await asyncio.sleep(0.5)
    async with aiohttp.ClientSession() as s:
        r2 = await asyncio.gather(*[make_request(s, sem, i, "S7-p2", "llama3.2:3b", stream=True) for i in range(3, 6)])
    await set_hung_mode("mock-a", False)
    await asyncio.sleep(2)
    print("  Phase 3: Recovery - 3 requests...")
    async with aiohttp.ClientSession() as s:
        r3 = await asyncio.gather(*[make_request(s, sem, i, "S7-p3", "llama3.2:3b", stream=False) for i in range(6, 9)])
    all_r = r1 + r2 + r3
    sr = analyze_results(all_r, "S7", "Fallback, health-check, recovery")
    sr.notes = [
        f"Phase1 (normal): {len([r for r in r1 if r.success])}/3",
        f"Phase2 (hung): {len([r for r in r2 if r.success])}/3",
        f"Phase3 (recovery): {len([r for r in r3 if r.success])}/3",
    ]
    return sr, all_r


# ── NEW SCENARIOS ───────────────────────────────────────────────────────────

async def s8_predictor_scoring():
    print("\n── S8: Predictor / Resource-aware scoring ──")
    # Накопим историю: серия запросов каждые 1.5 сек
    for wave in range(4):
        sem = asyncio.Semaphore(4)
        async with aiohttp.ClientSession() as s:
            tasks = [make_request(s, sem, i, "S8-warm", "llama3.2:3b", stream=False) for i in range(4)]
            await asyncio.gather(*tasks)
        await asyncio.sleep(1.5)

    # Теперь реальный тест
    sem = asyncio.Semaphore(6)
    async with aiohttp.ClientSession() as s:
        tasks = [make_request(s, sem, i, "S8", "llama3.2:3b", stream=False) for i in range(6)]
        results = await asyncio.gather(*tasks)

    cluster = await api_get("/api/v1/cluster")
    backends_info = {}
    for be in cluster.get("backends", []):
        backends_info[be.get("id", "")] = {
            "status": be.get("status"),
            "prediction": be.get("prediction", {}),
            "capacity": be.get("prediction", {}).get("requestCapacity", -1),
        }

    sr = analyze_results(results, "S8", "Predictor scoring после накопления истории")
    sr.notes = [
        f"Backend statuses: {backends_info}",
        "Ожидается: predictor даёт Capacity для каждого бэкенда, запросы идут на менее загруженные",
    ]
    return sr, results


async def s9_prewarm_controller():
    print("\n── S9: Prewarm controller ──")
    # Очищаем сессии
    await api_post("/api/v1/sessions/clear")
    await asyncio.sleep(1)

    # Делаем несколько запросов на llama3.2:3b (должна быть на mock-a, mock-b)
    # чтобы поднять load ratio
    sem = asyncio.Semaphore(2)
    async with aiohttp.ClientSession() as s:
        tasks = [make_request(s, sem, i, "S9-load", "qwen2.5:14b", stream=False) for i in range(4)]
        await asyncio.gather(*tasks)

    # Ждём prewarm цикл (checkIntervalSec=8)
    print("  Waiting for prewarm cycle (8s)...")
    await asyncio.sleep(10)

    # Проверяем состояние
    cluster = await api_get("/api/v1/cluster")
    warming_info = {}
    for be in cluster.get("backends", []):
        bid = be.get("id", "")
        warming = be.get("warmingUpModels", {})
        if warming:
            warming_info[bid] = list(warming.keys()) if isinstance(warming, dict) else warming

    metrics = await api_get("/api/v1/metrics")
    prewarm_count = metrics.get("prewarm_in_progress", -1)

    sem2 = asyncio.Semaphore(2)
    async with aiohttp.ClientSession() as s:
        results = [make_request(s, sem2, 0, "S9-check", "qwen2.5:14b", stream=False)]
        results = await asyncio.gather(*results)

    sr = analyze_results(results, "S9", "Prewarm controller check")
    sr.notes = [
        f"WarmingUpModels: {warming_info}",
        f"Prewarm in progress: {prewarm_count}",
        "Ожидается: prewarm триггерится при load > 70%",
    ]
    return sr, results


async def s10_model_instance_controller():
    print("\n── S10: Model instance controller (min=2, max=3, idleUnload=60s) ──")
    # Конфигурация уже в config.test.json: min=2, max=3, idleUnloadAfter=60s
    # Убедимся что модель присутствует минимум на 2 бэкендах
    await asyncio.sleep(2)
    cluster = await api_get("/api/v1/cluster")
    model_counts = {}
    for be in cluster.get("backends", []):
        metrics_data = be.get("metrics", {})
        ollama = metrics_data.get("ollama", {})
        for m in ollama.get("runningModels", []):
            name = m.get("name", "")
            model_counts[name] = model_counts.get(name, 0) + 1

    # Делаем несколько запросов для активации контроллера
    sem = asyncio.Semaphore(4)
    async with aiohttp.ClientSession() as s:
        results = [make_request(s, sem, i, "S10", "llama3.2:3b", stream=False) for i in range(6)]
        results = await asyncio.gather(*results)

    sr = analyze_results(results, "S10", "Model instance controller")
    sr.notes = [
        f"Model instance counts: {model_counts}",
        "Ожидается min=2: llama3.2 присутствует на >=2 бэкендах",
        "Ожидается max=3: не более 3 экземпляров одной модели",
        "idleUnloadAfter=60s: выгрузка неиспользуемых экземпляров",
    ]
    return sr, results


async def s11_queue_overflow():
    print("\n── S11: Queue overflow / rejection (queueMaxSize=10) ──")
    # Много запросов при малом maxConcurrentReqs
    sem = asyncio.Semaphore(30)
    async with aiohttp.ClientSession() as s:
        tasks = [
            make_request(s, sem, i, "S11", "llama3.2:3b", stream=False,
                         timeout=LONG_TIMEOUT, target_status=None)
            for i in range(25)
        ]
        results = await asyncio.gather(*tasks)

    ok_200 = len([r for r in results if r.status == 200])
    ok_503 = len([r for r in results if r.status == 503])
    errors = len([r for r in results if r.status not in (200, 503)])

    metrics_resp = await api_get("/api/v1/metrics")
    queue_depth = metrics_resp.get("request_queue_depth", -1)

    sr = analyze_results(results, "S11", "25 запросов при queueMaxSize=10")
    sr.notes = [
        f"200 OK: {ok_200}, 503: {ok_503}, Other errors: {errors}",
        f"Queue depth after test: {queue_depth}",
        "Ожидается: избыточные получают 503, часть через очередь успешны",
    ]
    return sr, results


async def s12_algorithm_roundrobin():
    print("\n── S12a: Round-robin algorithm ──")
    # Не можем менять env динамически без перезапуска контейнера,
    # поэтому используем существующий resource-aware, но проверим распределение
    sem = asyncio.Semaphore(6)
    async with aiohttp.ClientSession() as s:
        tasks = [make_request(s, sem, i, "S12a", "llama3.2:3b", stream=False) for i in range(9)]
        results = await asyncio.gather(*tasks)
    sr = analyze_results(results, "S12a", "Resource-aware distribution check")
    # Считаем, насколько распределение близко к равномерному
    dist = sr.backend_dist
    if dist:
        vals = list(dist.values())
        imbalance = max(vals) - min(vals) if len(vals) > 1 else 0
    else:
        imbalance = 999
    sr.notes = [
        f"Backend distribution: {dist}",
        f"Imbalance (max-min): {imbalance}",
        "Ожидается: распределение учитывает веса (mock-c weight=2 получает больше)",
    ]
    return sr, results


async def s13_ollama_api_aggregation():
    print("\n── S13: Ollama API aggregation endpoints ──")
    async with aiohttp.ClientSession() as s:
        results = []
        # /api/tags
        try:
            async with s.get(f"{BALANCER_PROXY}/api/tags", timeout=aiohttp.ClientTimeout(total=5)) as r:
                tags_data = await r.json()
                tags_ok = r.status == 200 and len(tags_data.get("models", [])) >= 3
                results.append(("tags", r.status, tags_ok, len(tags_data.get("models", []))))
        except Exception as e:
            results.append(("tags", 0, False, str(e)))

        # /api/version
        try:
            async with s.get(f"{BALANCER_PROXY}/api/version", timeout=aiohttp.ClientTimeout(total=5)) as r:
                ver_data = await r.json()
                ver_ok = r.status == 200 and "ollamalegion" in ver_data.get("version", "")
                results.append(("version", r.status, ver_ok, ver_data.get("ollamaVersions", {})))
        except Exception as e:
            results.append(("version", 0, False, str(e)))

        # /api/ps
        try:
            async with s.get(f"{BALANCER_PROXY}/api/ps", timeout=aiohttp.ClientTimeout(total=5)) as r:
                ps_data = await r.json()
                ps_ok = r.status == 200
                results.append(("ps", r.status, ps_ok, len(ps_data.get("models", []))))
        except Exception as e:
            results.append(("ps", 0, False, str(e)))

    all_ok = all(r[2] for r in results)
    sr = ScenarioReport(
        name="S13", desc="Ollama API aggregation", total=len(results),
        successful=sum(1 for r in results if r[2]), failed=sum(1 for r in results if not r[2]),
        elapsed_total_s=0,
    )
    sr.notes = [f"{ep}: status={st}, ok={ok}, detail={det}" for ep, st, ok, det in results]
    sr.notes.append("Ожидается: /api/tags union всех бэкендов, /api/version с map версий")
    # Создадим fake ReqResult для совместимости
    fake_results = [ReqResult(id=i, scenario="S13", model="", status=200 if r[2] else 500,
                              elapsed_ms=0, success=r[2]) for i, r in enumerate(results)]
    return sr, fake_results


async def s14_health_recovery():
    print("\n── S14: Health check recovery (stop/start mock-a) ──")
    compose_file = os.path.join(os.path.dirname(__file__), "docker-compose.test.yml")
    container = "ollama-legion-test-mock-a"

    # Phase 1: остановить mock-a
    print("  Stopping mock-a container...")
    proc = await asyncio.create_subprocess_exec(
        "docker", "compose", "-f", compose_file, "stop", "mock-a",
        stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.PIPE,
    )
    await proc.communicate()
    await asyncio.sleep(1)

    sem = asyncio.Semaphore(4)
    async with aiohttp.ClientSession() as s:
        r1 = await asyncio.gather(*[make_request(s, sem, i, "S14-p1", "llama3.2:3b", stream=False) for i in range(4)])

    # Phase 2: запустить mock-a обратно
    print("  Starting mock-a container...")
    proc = await asyncio.create_subprocess_exec(
        "docker", "compose", "-f", compose_file, "start", "mock-a",
        stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.PIPE,
    )
    await proc.communicate()
    print("  Waiting for health check recovery (15s)...")
    await asyncio.sleep(15)

    async with aiohttp.ClientSession() as s:
        r2 = await asyncio.gather(*[make_request(s, sem, i, "S14-p2", "llama3.2:3b", stream=False) for i in range(4, 8)])

    all_r = r1 + r2
    sr = analyze_results(all_r, "S14", "Health check recovery after container restart")
    sr.notes = [
        f"Phase1 (mock-a stopped): {len([r for r in r1 if r.success])}/4",
        f"Phase2 (mock-a back): {len([r for r in r2 if r.success])}/4",
        "Ожидается: Phase1 - failover на другие; Phase2 - mock-a healthy и принимает запросы",
    ]
    return sr, all_r


async def s15_session_rebalancing():
    print("\n── S15: Session rebalancing under load ──")
    await api_post("/api/v1/sessions/clear")
    await asyncio.sleep(0.5)

    # Создаём сессию
    async with aiohttp.ClientSession() as s:
        r = await make_request(s, asyncio.Semaphore(1), 0, "S15-init", "llama3.2:3b", stream=False, client_name="RebalanceTest")
    first_backend = r.target_backend
    session_id = r.session_id
    print(f"  Session created: {session_id} on {first_backend}")

    # Нагружаем тот же бэкенд до предела (maxConcurrentReqs=2)
    sem = asyncio.Semaphore(3)
    async with aiohttp.ClientSession() as s:
        load_tasks = [
            make_request(s, sem, i, "S15-load", "llama3.2:3b", stream=True, client_name="Loader")
            for i in range(4)
        ]
        # + ещё запрос от той же сессии
        load_tasks.append(
            make_request(s, sem, 99, "S15-rebalance", "llama3.2:3b", stream=False, client_name="RebalanceTest")
        )
        results = await asyncio.gather(*load_tasks)

    rebalance_result = results[-1]
    sr = analyze_results(results, "S15", "Session rebalancing при load_ratio > 50%")
    sr.notes = [
        f"Исходный бэкенд: {first_backend}",
        f"Запрос после нагрузки ушёл на: {rebalance_result.target_backend}",
        "Ожидается: если load_ratio > 50%, сессия ребалансируется на менее загруженный бэкенд",
    ]
    return sr, results


async def s16_graceful_shutdown_state():
    print("\n── S16: Graceful shutdown + state persistence ──")
    compose_file = os.path.join(os.path.dirname(__file__), "docker-compose.test.yml")

    # Делаем запрос, чтобы создать сессию
    async with aiohttp.ClientSession() as s:
        r = await make_request(s, asyncio.Semaphore(1), 0, "S16-pre", "llama3.2:3b", stream=False, client_name="StateTest")
    pre_session = r.session_id
    print(f"  Pre-shutdown session: {pre_session}")

    # Останавливаем балансер
    print("  Stopping balancer...")
    proc = await asyncio.create_subprocess_exec(
        "docker", "compose", "-f", compose_file, "stop", "balancer",
        stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.PIPE,
    )
    await proc.communicate()
    await asyncio.sleep(2)

    # Запускаем балансер
    print("  Starting balancer...")
    proc = await asyncio.create_subprocess_exec(
        "docker", "compose", "-f", compose_file, "start", "balancer",
        stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.PIPE,
    )
    await proc.communicate()

    # Ждём готовности
    if not await wait_for_balancer(30):
        sr = ScenarioReport(name="S16", desc="Graceful shutdown", total=0, successful=0, failed=1, elapsed_total_s=0)
        sr.notes = ["Balancer not ready after restart!"]
        return sr, [ReqResult(id=0, scenario="S16", model="", status=0, elapsed_ms=0, success=False, error="Not ready")]

    # Проверяем восстановление
    sessions = await api_get("/api/v1/sessions")
    cluster = await api_get("/api/v1/cluster")
    backends_ok = len(cluster.get("backends", [])) >= 3

    fake_results = [ReqResult(id=0, scenario="S16", model="", status=200, elapsed_ms=0, success=backends_ok)]
    sr = ScenarioReport(
        name="S16", desc="Graceful shutdown persistence", total=1,
        successful=1 if backends_ok else 0, failed=0 if backends_ok else 1,
        elapsed_total_s=0,
    )
    sr.notes = [
        f"Sessions after restart: {len(sessions)}",
        f"Backends healthy: {backends_ok}",
        "Ожидается: state.json сохраняется, бэкенды восстанавливаются после перезапуска",
    ]
    return sr, fake_results


async def s17_retry_excluding_failed():
    print("\n── S17: Retry с исключением failed backend ──")
    compose_file = os.path.join(os.path.dirname(__file__), "docker-compose.test.yml")

    # Останавливаем mock-a
    print("  Stopping mock-a...")
    proc = await asyncio.create_subprocess_exec(
        "docker", "compose", "-f", compose_file, "stop", "mock-a",
        stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.PIPE,
    )
    await proc.communicate()
    await asyncio.sleep(1)

    # Запрос модели, которая ЕСТЬ на mock-a (llama3.2 или deepseek-r1)
    sem = asyncio.Semaphore(2)
    async with aiohttp.ClientSession() as s:
        results = [make_request(s, sem, i, "S17", "deepseek-r1:32b", stream=False) for i in range(4)]
        results = await asyncio.gather(*results)

    # Восстанавливаем
    print("  Starting mock-a...")
    proc = await asyncio.create_subprocess_exec(
        "docker", "compose", "-f", compose_file, "start", "mock-a",
        stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.PIPE,
    )
    await proc.communicate()
    await asyncio.sleep(3)

    sr = analyze_results(results, "S17", "Retry исключая упавший бэкенд")
    sr.notes = [
        "Ожидается: запросы уходят на mock-c (тоже имеет deepseek-r1:32b), не на mock-a",
        "3 попытки retry с исключением failed backend",
    ]
    return sr, results


async def s18_concurrent_correctness():
    print("\n── S18: Concurrent load correctness (50 requests, race detection) ──")
    sem = asyncio.Semaphore(15)
    names = ["Cline", "OpenWebUI", "Python", "cURL", "GoClient"]
    async with aiohttp.ClientSession() as s:
        tasks = [
            make_request(s, sem, i, "S18", "llama3.2:3b", stream=False, client_name=names[i % len(names)])
            for i in range(50)
        ]
        results = await asyncio.gather(*tasks)

    await asyncio.sleep(1)

    metrics_resp = await api_get("/api/v1/metrics")
    total_reqs = metrics_resp.get("total_proxy_requests", -1)

    cluster = await api_get("/api/v1/cluster")
    active_sum = sum(be.get("metrics", {}).get("ollama", {}).get("activeRequests", 0) for be in cluster.get("backends", [])
                     if be.get("status") in ("healthy", "degraded"))

    sr = analyze_results(results, "S18", "50 concurrent, проверка race conditions")
    sr.notes = [
        f"Total proxy requests: {total_reqs}",
        f"Sum of activeRequests: {active_sum}",
        "Ожидается: totalRequests монотонно растёт, ActiveReqs >= 0, нет паники",
        f"Успешно: {sr.successful}/{sr.total}",
    ]
    return sr, results


# ── Main ────────────────────────────────────────────────────────────────────

async def main():
    print("=" * 60)
    print("OLLAMA LEGION - EXTENDED LOAD TEST (18 scenarios)")
    print(f"Start: {datetime.now().strftime('%H:%M:%S')}")
    print("=" * 60)

    print("\nWaiting for balancer...")
    if not await wait_for_balancer(45):
        print("FATAL: Balancer not ready!")
        sys.exit(1)

    await api_post("/api/v1/sessions/clear")

    all_reports: list[ScenarioReport] = []
    all_results: dict[str, list[ReqResult]] = {}
    test_start = time.perf_counter()

    scenarios = [
        ("S1", s1_non_streaming_baseline),
        ("S2", s2_streaming_baseline),
        ("S3", s3_hung_gpu_retry),
        ("S4", s4_two_clients_same_ip),
        ("S5", s5_model_affinity),
        ("S6", s6_queue_stress),
        ("S7", s7_backend_failure_recovery),
        ("S8", s8_predictor_scoring),
        ("S9", s9_prewarm_controller),
        ("S10", s10_model_instance_controller),
        ("S11", s11_queue_overflow),
        ("S12a", s12_algorithm_roundrobin),
        ("S13", s13_ollama_api_aggregation),
        ("S14", s14_health_recovery),
        ("S15", s15_session_rebalancing),
        ("S16", s16_graceful_shutdown_state),
        ("S17", s17_retry_excluding_failed),
        ("S18", s18_concurrent_correctness),
    ]

    for name, scenario_fn in scenarios:
        try:
            sr, results = await scenario_fn()
            all_reports.append(sr)
            all_results[name] = results
            print(f"  {name} done: {sr.successful}/{sr.total} ok, avg {sr.latency_avg_ms:.1f}ms"
                  if sr.latency_avg_ms > 0 else f"  {name} done: {sr.successful}/{sr.total} ok")
        except Exception as e:
            print(f"  {name} ERROR: {e}")
            sr = ScenarioReport(name=name, desc="CRASHED", total=0, successful=0, failed=1, elapsed_total_s=0)
            sr.notes = [f"Exception: {e}"]
            all_reports.append(sr)

    test_elapsed = time.perf_counter() - test_start

    # Final cluster state
    print("\nFetching final cluster state...")
    cluster = await api_get("/api/v1/cluster")

    # Generate report
    print("Generating report...")
    timestamp = datetime.now().strftime("%Y%m%d_%H%M%S")
    report_path = os.path.join(RESULTS_DIR, f"report_extended_{timestamp}.md")
    json_path = os.path.join(RESULTS_DIR, f"results_extended_{timestamp}.json")

    # JSON
    json_data = {
        "timestamp": timestamp,
        "total_elapsed_s": round(test_elapsed, 1),
        "cluster_state": cluster,
        "scenarios": [
            {
                "name": sr.name, "desc": sr.desc,
                "total": sr.total, "successful": sr.successful, "failed": sr.failed,
                "latency_min_ms": sr.latency_min_ms, "latency_avg_ms": sr.latency_avg_ms,
                "latency_p95_ms": sr.latency_p95_ms, "latency_max_ms": sr.latency_max_ms,
                "rps": round(sr.rps, 2), "backend_dist": sr.backend_dist,
                "errors": sr.errors, "notes": sr.notes,
            }
            for sr in all_reports
        ],
    }
    with open(json_path, "w", encoding="utf-8") as f:
        json.dump(json_data, f, ensure_ascii=False, indent=2)

    # Markdown
    md = [
        "# Ollama Legion - Extended Load Test Report",
        f"**Date:** {datetime.now().strftime('%Y-%m-%d %H:%M:%S')}",
        f"**Total test time:** {test_elapsed:.1f}s",
        f"**Balancer:** {BALANCER_PROXY}",
        "",
        "## Summary",
        "",
        "| # | Scenario | Total | OK | Fail | Avg Lat (ms) | p95 (ms) | RPS |",
        "|---|----------|-------|----|------|-------------|----------|-----|",
    ]
    for sr in all_reports:
        md.append(
            f"| {sr.name} | {sr.desc[:45]} | {sr.total} | {sr.successful} | {sr.failed} | "
            f"{sr.latency_avg_ms:.1f} | {sr.latency_p95_ms:.1f} | {sr.rps:.1f} |"
        )
    md += [
        "",
        "## Cluster State (after tests)",
        "```json",
        json.dumps(cluster, ensure_ascii=False, indent=2),
        "```",
        "",
        "## Per-Scenario Details",
        "",
    ]
    for sr in all_reports:
        md.append(f"### {sr.name}: {sr.desc}")
        md.append(f"- **Total:** {sr.total}, **OK:** {sr.successful}, **Failed:** {sr.failed}")
        if sr.latency_avg_ms > 0:
            md.append(f"- **Latency:** min={sr.latency_min_ms:.1f}ms, avg={sr.latency_avg_ms:.1f}ms, "
                      f"median={sr.latency_median_ms:.1f}ms, p95={sr.latency_p95_ms:.1f}ms, max={sr.latency_max_ms:.1f}ms")
            md.append(f"- **RPS:** {sr.rps:.1f}")
        if sr.backend_dist:
            md.append(f"- **Backend distribution:** {sr.backend_dist}")
        if sr.errors:
            md.append(f"- **Errors:** {sr.errors[:5]}")
        if sr.notes:
            md.append("- **Notes:**")
            for n in sr.notes:
                md.append(f"  - {n}")
        md.append("")

    md += [
        "## Analysis",
        "",
    ]
    total_ok = sum(sr.successful for sr in all_reports)
    total_all = sum(sr.total for sr in all_reports)
    if total_all > 0:
        md.append(f"- **Overall success rate:** {total_ok}/{total_all} ({total_ok/total_all*100:.1f}%)")
    md.append(f"- **Failed requests:** {total_all - total_ok}")
    md.append("")

    report_md = "\n".join(md)
    with open(report_path, "w", encoding="utf-8") as f:
        f.write(report_md)

    # Console summary
    print("\n" + "=" * 60)
    print("TEST COMPLETE")
    print(f"Total time: {test_elapsed:.1f}s")
    if total_all > 0:
        print(f"Overall: {total_ok}/{total_all} ok ({total_ok/total_all*100:.1f}%)")
    print(f"Reports: {report_path}")
    print(f"JSON:     {json_path}")
    print("=" * 60)

    return 0

if __name__ == "__main__":
    sys.exit(asyncio.run(main()))