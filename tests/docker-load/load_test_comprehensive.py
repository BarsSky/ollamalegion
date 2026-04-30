#!/usr/bin/env python3
"""
Комплексный нагрузочный тест балансера Ollama Legion.
7 сценариев, ~50-60 секунд общее время выполнения.

Использование:
  docker compose -f docker-compose.test.yml up -d --build
  python load_test_comprehensive.py
  docker compose -f docker-compose.test.yml down -v
"""

import asyncio
import aiohttp
import json
import os
import sys
import time
from dataclasses import dataclass, field
from datetime import datetime
from typing import Optional

# ── Config ──────────────────────────────────────────────────────────────────
BALANCER_PROXY = "http://localhost:18090"
BALANCER_API = "http://localhost:18091"
RESULTS_DIR = os.path.join(os.path.dirname(__file__), "results")
os.makedirs(RESULTS_DIR, exist_ok=True)

# Тестовые таймауты (должны соответствовать config.test.json)
FIRST_BYTE_TIMEOUT = 5
REQUEST_TIMEOUT = 10  # клиентский таймаут (должен быть > firstByteTimeout)

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
    target_backend: str = ""   # из X-Backend-ID заголовка
    session_id: str = ""       # из X-Session-ID заголовка


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
    """GET запрос к Management API балансера."""
    try:
        async with aiohttp.ClientSession() as s:
            async with s.get(f"{BALANCER_API}{path}", timeout=aiohttp.ClientTimeout(total=5)) as r:
                return await r.json()
    except Exception as e:
        return {"error": str(e)}


async def set_hung_mode(backend: str, enable: bool) -> bool:
    """Включение/выключение hung-режима на mock-бэкенде (через Docker exec)."""
    # Пробуем через HTTP на бэкенд напрямую
    ports = {"mock-a": "11434", "mock-b": "11435", "mock-c": "11436"}
    port = ports.get(backend, "11434")
    # Бэкенды доступны через внутреннюю сеть, но с хоста — через docker exec
    # Используем docker compose exec
    container = f"ollama-legion-test-mock-{backend[-1]}"
    try:
        proc = await asyncio.create_subprocess_exec(
            "docker", "compose", "-f",
            os.path.join(os.path.dirname(__file__), "docker-compose.test.yml"),
            "exec", "-T", container,
            "curl", "-s", "-X", "POST", f"http://localhost:11434/admin/hung",
            "-H", "Content-Type: application/json",
            "-d", json.dumps({"enable": enable}),
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
        )
        stdout, stderr = await proc.communicate()
        return proc.returncode == 0
    except Exception:
        return False


async def make_request(
    session: aiohttp.ClientSession,
    sem: asyncio.Semaphore,
    req_id: int,
    scenario: str,
    model: str,
    endpoint: str = "/api/generate",
    stream: bool = False,
    client_name: str = "",
    hung: bool = False,
) -> ReqResult:
    """Выполняет один запрос к балансеру и возвращает результат."""
    async with sem:
        start = time.perf_counter()
        headers = {
            "Content-Type": "application/json",
        }
        if client_name:
            headers["X-Client-Name"] = client_name
            headers["User-Agent"] = f"{client_name}/Test/1.0"

        payload = {
            "model": model,
            "prompt": f"Test {scenario} req #{req_id}",
            "stream": stream,
        }
        if hung:
            payload["hung"] = True

        try:
            async with session.post(
                f"{BALANCER_PROXY}{endpoint}",
                json=payload,
                headers=headers,
                timeout=aiohttp.ClientTimeout(total=REQUEST_TIMEOUT),
            ) as resp:
                if stream:
                    # Читаем SSE stream до конца
                    text = ""
                    async for line in resp.content:
                        text += line.decode(errors="replace")
                    elapsed = (time.perf_counter() - start) * 1000
                else:
                    text = await resp.text()
                    elapsed = (time.perf_counter() - start) * 1000

                return ReqResult(
                    id=req_id,
                    scenario=scenario,
                    model=model,
                    status=resp.status,
                    elapsed_ms=elapsed,
                    success=resp.status == 200,
                    target_backend=resp.headers.get("X-Backend-ID", ""),
                    session_id=resp.headers.get("X-Session-ID", ""),
                )
        except asyncio.TimeoutError:
            return ReqResult(
                id=req_id,
                scenario=scenario,
                model=model,
                status=0,
                elapsed_ms=(time.perf_counter() - start) * 1000,
                success=False,
                error="Timeout",
            )
        except Exception as e:
            return ReqResult(
                id=req_id,
                scenario=scenario,
                model=model,
                status=0,
                elapsed_ms=(time.perf_counter() - start) * 1000,
                success=False,
                error=str(e),
            )


def analyze_results(results: list[ReqResult], name: str, desc: str) -> ScenarioReport:
    """Анализирует результаты сценария и формирует отчёт."""
    successful = [r for r in results if r.success]
    failed = [r for r in results if not r.success]
    latencies = sorted([r.elapsed_ms for r in successful])

    dist = {}
    for r in successful:
        be = r.target_backend or "unknown"
        dist[be] = dist.get(be, 0) + 1

    sr = ScenarioReport(
        name=name,
        desc=desc,
        total=len(results),
        successful=len(successful),
        failed=len(failed),
        elapsed_total_s=sum(r.elapsed_ms for r in results) / 1000,
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


async def wait_for_balancer(timeout: int = 30) -> bool:
    """Ожидание готовности балансера."""
    for i in range(timeout):
        try:
            async with aiohttp.ClientSession() as s:
                async with s.get(f"{BALANCER_API}/api/v1/health", timeout=aiohttp.ClientTimeout(total=2)) as r:
                    if r.status == 200:
                        print(f"  ✓ Balancer ready (after {i+1}s)")
                        return True
        except Exception:
            pass
        await asyncio.sleep(1)
    return False


# ── Scenarios ───────────────────────────────────────────────────────────────

async def s1_non_streaming_baseline() -> tuple[ScenarioReport, list[ReqResult]]:
    """S1: Non-streaming, 3 concurrent, 9 запросов — базовая пропускная способность."""
    print("\n── S1: Non-streaming baseline (3 concurrent, 9 total) ──")
    sem = asyncio.Semaphore(3)
    async with aiohttp.ClientSession() as s:
        tasks = [make_request(s, sem, i, "S1", "llama3.2:3b", stream=False) for i in range(9)]
        results = await asyncio.gather(*tasks)
    sr = analyze_results(results, "S1", "Non-streaming, 3 concurrent")
    sr.notes = ["Ожидается: все 9 успешны, равномерное распределение, latency < 500ms"]
    return sr, results


async def s2_streaming_baseline() -> tuple[ScenarioReport, list[ReqResult]]:
    """S2: Streaming SSE, 3 concurrent, 9 запросов."""
    print("\n── S2: Streaming SSE baseline (3 concurrent, 9 total) ──")
    sem = asyncio.Semaphore(3)
    async with aiohttp.ClientSession() as s:
        tasks = [make_request(s, sem, i, "S2", "llama3.2:3b", stream=True) for i in range(9)]
        results = await asyncio.gather(*tasks)
    sr = analyze_results(results, "S2", "Streaming SSE, 3 concurrent")
    sr.notes = ["Ожидается: все 9 успешны, first-byte < 5s, все токены получены"]
    return sr, results


async def s3_hung_gpu_retry() -> tuple[ScenarioReport, list[ReqResult]]:
    """S3: Симуляция GPU-hang на mock-a, проверка first-byte timeout → retry."""
    print("\n── S3: GPU-hang simulation (mock-a hung, check retry) ──")

    # Включаем hung-режим на mock-a
    print("  → Enabling hung mode on mock-a...")
    ok = await set_hung_mode("mock-a", True)
    print(f"  → Hung mode enabled: {ok}")

    await asyncio.sleep(1)

    sem = asyncio.Semaphore(3)
    async with aiohttp.ClientSession() as s:
        tasks = [make_request(s, sem, i, "S3", "llama3.2:3b", stream=True) for i in range(6)]
        results = await asyncio.gather(*tasks)

    # Выключаем hung-режим
    print("  → Disabling hung mode on mock-a...")
    await set_hung_mode("mock-a", False)
    await asyncio.sleep(1)

    sr = analyze_results(results, "S3", "GPU-hang на mock-a, first-byte timeout → retry")
    sr.notes = [
        "Ожидается: запросы на mock-a получают first-byte timeout (5s)",
        "И затем retry на mock-b или mock-c",
        "mock-a помечается unhealthy после нескольких неудач",
    ]
    return sr, results


async def s4_two_clients_same_ip() -> tuple[ScenarioReport, list[ReqResult]]:
    """S4: Cline + OpenWebUI с одного IP (localhost), разные сессии."""
    print("\n── S4: Two clients same IP — Cline + OpenWebUI ──")
    sem = asyncio.Semaphore(6)
    async with aiohttp.ClientSession() as s:
        tasks = []
        # 3 запроса от Cline
        for i in range(3):
            tasks.append(make_request(s, sem, i, "S4", "llama3.2:3b", stream=False, client_name="Cline"))
        # 3 запроса от OpenWebUI
        for i in range(3, 6):
            tasks.append(make_request(s, sem, i, "S4", "llama3.2:3b", stream=False, client_name="OpenWebUI"))
        results = await asyncio.gather(*tasks)

    session_ids = set(r.session_id for r in results if r.session_id)
    sr = analyze_results(results, "S4", "2 клиента (Cline/OpenWebUI) с одного IP")
    sr.notes = [
        f"Уникальных сессий: {len(session_ids)} (ожидается 2: Cline + OpenWebUI)",
        "Ожидается: разные sessionID для разных clientName",
    ]
    return sr, results


async def s5_model_affinity() -> tuple[ScenarioReport, list[ReqResult]]:
    """S5: 3 модели (llama3.2, qwen2.5, deepseek-r1), model affinity."""
    print("\n── S5: Model affinity (3 models × 3 requests) ──")
    sem = asyncio.Semaphore(9)
    async with aiohttp.ClientSession() as s:
        tasks = []
        for i in range(3):
            tasks.append(make_request(s, sem, i, "S5", "llama3.2:3b", stream=False))
        for i in range(3, 6):
            tasks.append(make_request(s, sem, i, "S5", "qwen2.5:14b", stream=False))
        for i in range(6, 9):
            tasks.append(make_request(s, sem, i, "S5", "deepseek-r1:32b", stream=False))
        results = await asyncio.gather(*tasks)

    # Группируем по модели и бэкенду
    model_dist = {}
    for r in results:
        if r.success:
            key = f"{r.model} → {r.target_backend}"
            model_dist[key] = model_dist.get(key, 0) + 1

    sr = analyze_results(results, "S5", "Model affinity (llama3.2, qwen2.5, deepseek-r1)")
    sr.notes = [
        f"Распределение модель→бэкенд: {model_dist}",
        "Ожидается: каждая модель идёт на бэкенд где она загружена",
        "llama3.2: mock-a, mock-b | qwen2.5: mock-b, mock-c | deepseek-r1: mock-a, mock-c",
    ]
    return sr, results


async def s6_queue_stress() -> tuple[ScenarioReport, list[ReqResult]]:
    """S6: Очередь — 12 запросов при MaxConcurrentReqs=2 на бэкенд."""
    print("\n── S6: Queue stress (12 requests, max 2 concurrent per backend) ──")
    sem = asyncio.Semaphore(12)  # Все сразу — балансер должен поставить в очередь
    async with aiohttp.ClientSession() as s:
        tasks = [make_request(s, sem, i, "S6", "llama3.2:3b", stream=False) for i in range(12)]
        results = await asyncio.gather(*tasks)

    sr = analyze_results(results, "S6", "12 запросов, MaxConcurrentReqs=2")
    sr.notes = [
        "Ожидается: все 12 успешны (через очередь)",
        "Не должно быть 503 (Service Unavailable)",
    ]
    return sr, results


async def s7_backend_failure_recovery() -> tuple[ScenarioReport, list[ReqResult]]:
    """S7: Падение бэкенда во время нагрузки → health check → recovery."""
    print("\n── S7: Backend failure during load ──")

    # Phase 1: нормальная нагрузка
    print("  → Phase 1: Normal load (3 requests)...")
    sem1 = asyncio.Semaphore(3)
    async with aiohttp.ClientSession() as s:
        tasks1 = [make_request(s, sem1, i, "S7-phase1", "llama3.2:3b", stream=False) for i in range(3)]
        results1 = await asyncio.gather(*tasks1)

    await asyncio.sleep(1)

    # Phase 2: включаем hung на mock-a и делаем запросы
    print("  → Phase 2: Enabling hung on mock-a + 3 more requests...")
    await set_hung_mode("mock-a", True)
    await asyncio.sleep(0.5)

    sem2 = asyncio.Semaphore(3)
    async with aiohttp.ClientSession() as s:
        tasks2 = [make_request(s, sem2, i, "S7-phase2", "llama3.2:3b", stream=True) for i in range(3, 6)]
        results2 = await asyncio.gather(*tasks2)

    # Выключаем hung
    await set_hung_mode("mock-a", False)
    await asyncio.sleep(2)

    # Phase 3: восстановление — ещё 3 запроса
    print("  → Phase 3: Recovery — 3 more requests...")
    sem3 = asyncio.Semaphore(3)
    async with aiohttp.ClientSession() as s:
        tasks3 = [make_request(s, sem3, i, "S7-phase3", "llama3.2:3b", stream=False) for i in range(6, 9)]
        results3 = await asyncio.gather(*tasks3)

    all_results = results1 + results2 + results3
    sr = analyze_results(all_results, "S7", "Падение бэкенда, failover, восстановление")
    sr.notes = [
        f"Phase 1 (normal): {len([r for r in results1 if r.success])}/3 успешно",
        f"Phase 2 (hung mock-a): {len([r for r in results2 if r.success])}/3 успешно",
        f"Phase 3 (recovery): {len([r for r in results3 if r.success])}/3 успешно",
        "Ожидается: Phase 2 — retry на другие бэкенды; Phase 3 — восстановление",
    ]
    return sr, all_results


# ── Main ────────────────────────────────────────────────────────────────────

async def main():
    print("=" * 60)
    print("OLLAMA LEGION — COMPREHENSIVE LOAD TEST")
    print(f"Start: {datetime.now().strftime('%H:%M:%S')}")
    print("=" * 60)

    # Ждём готовности балансера
    print("\n⏳ Waiting for balancer to be ready...")
    if not await wait_for_balancer(30):
        print("❌ Balancer not ready! Aborting.")
        sys.exit(1)

    # Очищаем сессии перед тестами
    print("  → Clearing sessions...")
    await api_get("/api/v1/sessions/clear")

    all_reports: list[ScenarioReport] = []
    test_start = time.perf_counter()

    # ── S1 ──
    sr1, _ = await s1_non_streaming_baseline()
    all_reports.append(sr1)
    print(f"  S1 done: {sr1.successful}/{sr1.total} ok, avg {sr1.latency_avg_ms:.1f}ms")

    # ── S2 ──
    sr2, _ = await s2_streaming_baseline()
    all_reports.append(sr2)
    print(f"  S2 done: {sr2.successful}/{sr2.total} ok, avg {sr2.latency_avg_ms:.1f}ms")

    # ── S3 ──
    sr3, _ = await s3_hung_gpu_retry()
    all_reports.append(sr3)
    print(f"  S3 done: {sr3.successful}/{sr3.total} ok")

    # ── S4 ──
    sr4, results4 = await s4_two_clients_same_ip()
    all_reports.append(sr4)
    session_ids = set(r.session_id for r in results4 if r.session_id)
    print(f"  S4 done: {sr4.successful}/{sr4.total} ok, sessions={len(session_ids)}")

    # ── S5 ──
    sr5, _ = await s5_model_affinity()
    all_reports.append(sr5)
    print(f"  S5 done: {sr5.successful}/{sr5.total} ok")

    # ── S6 ──
    sr6, _ = await s6_queue_stress()
    all_reports.append(sr6)
    print(f"  S6 done: {sr6.successful}/{sr6.total} ok")

    # ── S7 ──
    sr7, _ = await s7_backend_failure_recovery()
    all_reports.append(sr7)
    print(f"  S7 done: {sr7.successful}/{sr7.total} ok")

    test_elapsed = time.perf_counter() - test_start

    # ── Cluster state ──
    print("\n── Fetching final cluster state... ──")
    cluster = await api_get("/api/v1/cluster")

    # ── Generate report ──
    print("\n── Generating report... ──")
    timestamp = datetime.now().strftime("%Y%m%d_%H%M%S")
    report_path = os.path.join(RESULTS_DIR, f"report_{timestamp}.md")
    json_path = os.path.join(RESULTS_DIR, f"results_{timestamp}.json")

    # Сохраняем JSON
    json_data = {
        "timestamp": timestamp,
        "total_elapsed_s": round(test_elapsed, 1),
        "cluster_state": cluster,
        "scenarios": [
            {
                "name": sr.name,
                "desc": sr.desc,
                "total": sr.total,
                "successful": sr.successful,
                "failed": sr.failed,
                "latency_min_ms": sr.latency_min_ms,
                "latency_avg_ms": sr.latency_avg_ms,
                "latency_p95_ms": sr.latency_p95_ms,
                "latency_max_ms": sr.latency_max_ms,
                "rps": round(sr.rps, 2),
                "backend_dist": sr.backend_dist,
                "errors": sr.errors,
                "notes": sr.notes,
            }
            for sr in all_reports
        ],
    }
    with open(json_path, "w", encoding="utf-8") as f:
        json.dump(json_data, f, ensure_ascii=False, indent=2)

    # Формируем Markdown отчёт
    md = []
    md.append("# Ollama Legion — Load Test Report")
    md.append(f"**Date:** {datetime.now().strftime('%Y-%m-%d %H:%M:%S')}")
    md.append(f"**Total test time:** {test_elapsed:.1f}s")
    md.append(f"**Balancer:** {BALANCER_PROXY}")
    md.append("")
    md.append("## Summary")
    md.append("")
    md.append("| # | Scenario | Total | OK | Fail | Avg Lat (ms) | p95 (ms) | RPS |")
    md.append("|---|----------|-------|----|------|-------------|----------|-----|")
    for sr in all_reports:
        md.append(
            f"| {sr.name} | {sr.desc[:40]} | {sr.total} | {sr.successful} | {sr.failed} | "
            f"{sr.latency_avg_ms:.1f} | {sr.latency_p95_ms:.1f} | {sr.rps:.1f} |"
        )
    md.append("")
    md.append("## Cluster State (after tests)")
    md.append("```json")
    md.append(json.dumps(cluster, ensure_ascii=False, indent=2))
    md.append("```")
    md.append("")
    md.append("## Per-Scenario Details")
    md.append("")

    for sr in all_reports:
        md.append(f"### {sr.name}: {sr.desc}")
        md.append(f"- **Total:** {sr.total}, **OK:** {sr.successful}, **Failed:** {sr.failed}")
        md.append(f"- **Latency:** min={sr.latency_min_ms:.1f}ms, avg={sr.latency_avg_ms:.1f}ms, "
                   f"median={sr.latency_median_ms:.1f}ms, p95={sr.latency_p95_ms:.1f}ms, max={sr.latency_max_ms:.1f}ms")
        md.append(f"- **RPS:** {sr.rps:.1f}")
        if sr.backend_dist:
            md.append(f"- **Backend distribution:** {sr.backend_dist}")
        if sr.errors:
            md.append(f"- **Errors:** {sr.errors[:5]}")
        if sr.notes:
            md.append(f"- **Notes:**")
            for n in sr.notes:
                md.append(f"  - {n}")
        md.append("")

    md.append("## Analysis & Recommendations")
    md.append("")
    total_ok = sum(sr.successful for sr in all_reports)
    total_all = sum(sr.total for sr in all_reports)
    md.append(f"- **Overall success rate:** {total_ok}/{total_all} ({total_ok/total_all*100:.1f}%)")
    md.append(f"- **Failed requests:** {total_all - total_ok}")
    md.append("")

    # Анализируем распределение
    all_dists = {}
    for sr in all_reports:
        for be, cnt in sr.backend_dist.items():
            all_dists[be] = all_dists.get(be, 0) + cnt
    if all_dists:
        md.append("### Load Distribution")
        total_dist = sum(all_dists.values())
        for be, cnt in sorted(all_dists.items()):
            md.append(f"- **{be}:** {cnt} requests ({cnt/total_dist*100:.1f}%)")

    md.append("")
    md.append("### Оптимизированность")
    md.append("")
    md.append("- Балансер использует resource-aware алгоритм с учётом весов бэкендов")
    md.append("- Model affinity направляет запросы на бэкенды с уже загруженными моделями")
    md.append("- First-byte timeout + retry защищает от зависших GPU")
    md.append("- Очередь предотвращает потерю запросов при превышении concurrent лимитов")
    md.append("- Session stickiness с clientName различает клиентов за одним IP")

    report_md = "\n".join(md)
    with open(report_path, "w", encoding="utf-8") as f:
        f.write(report_md)

    # ── Console summary ──
    print("\n" + "=" * 60)
    print("TEST COMPLETE")
    print(f"Total time: {test_elapsed:.1f}s")
    print(f"Overall: {total_ok}/{total_all} ok ({total_ok/total_all*100:.1f}%)")
    print(f"\nReports saved:")
    print(f"  Markdown: {report_path}")
    print(f"  JSON:     {json_path}")
    print("=" * 60)

    return 0 if total_ok == total_all else 1


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))