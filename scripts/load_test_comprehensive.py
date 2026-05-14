#!/usr/bin/env python3
"""
Комплексные нагрузочные тесты Ollama Legion — покрывает сценарии:
  - Сценарий A: «4 пользователя, 2 модели» (Задача 3)
  - Сценарий B: «Полная загрузка + подгрузка на свободный» (Задача 4, 3 фазы)

Примеры запуска:
  python scripts/load_test_comprehensive.py --scenario A --balancer http://localhost:8080
  python scripts/load_test_comprehensive.py --scenario B --balancer http://localhost:8080 --requests 30 --concurrent 8
  python scripts/load_test_comprehensive.py --all
"""

import argparse
import asyncio
import json
import sys
import time
from dataclasses import dataclass, field
from typing import Dict, List, Optional

import aiohttp


# ---------------------------------------------------------------------------
# Data classes
# ---------------------------------------------------------------------------

@dataclass
class RequestResult:
    request_id: int
    user_id: str
    model: str
    status: int
    elapsed_sec: float
    success: bool
    backend: str = ""
    error: str = ""


@dataclass
class ScenarioReport:
    name: str
    total: int
    successful: int = 0
    failed: int = 0
    latencies: List[float] = field(default_factory=list)
    by_model: Dict[str, int] = field(default_factory=dict)
    by_backend: Dict[str, int] = field(default_factory=dict)
    by_status: Dict[int, int] = field(default_factory=dict)
    errors: List[str] = field(default_factory=list)

    def add(self, r: RequestResult):
        self.by_status[r.status] = self.by_status.get(r.status, 0) + 1
        self.by_model[r.model] = self.by_model.get(r.model, 0) + 1
        if r.success:
            self.successful += 1
            self.latencies.append(r.elapsed_sec)
            if r.backend:
                self.by_backend[r.backend] = self.by_backend.get(r.backend, 0) + 1
        else:
            self.failed += 1
            self.errors.append(f"[user={r.user_id}] [{r.model}] {r.error}")

    def print_report(self, duration: float):
        print(f"\n{'=' * 70}")
        print(f"ОТЧЁТ: {self.name}")
        print(f"{'=' * 70}")
        print(f"Всего запросов   : {self.total}")
        print(f"Успешно          : {self.successful} ({_pct(self.successful, self.total):.1f}%)")
        print(f"Ошибок           : {self.failed} ({_pct(self.failed, self.total):.1f}%)")
        print(f"Общее время      : {duration:.1f} сек")
        print(f"RPS (общий)      : {self.total / max(duration, 0.001):.1f}")
        if self.latencies:
            sorted_lat = sorted(self.latencies)
            print(f"Задержка (сек)   : avg={_avg(self.latencies):.2f} "
                  f"p50={_p(sorted_lat, 50):.2f} "
                  f"p95={_p(sorted_lat, 95):.2f} "
                  f"p99={_p(sorted_lat, 99):.2f} "
                  f"min={min(self.latencies):.2f} "
                  f"max={max(self.latencies):.2f}")
        if self.by_model:
            print(f"\nПо моделям:")
            for model, cnt in sorted(self.by_model.items()):
                print(f"  {model}: {cnt}")
        if self.by_backend:
            print(f"\nПо бэкендам:")
            for backend, cnt in sorted(self.by_backend.items(), key=lambda x: -x[1]):
                print(f"  {backend}: {cnt} ({_pct(cnt, self.total):.1f}%)")
        if self.by_status:
            print(f"\nПо HTTP-статусам:")
            for status, cnt in sorted(self.by_status.items()):
                print(f"  {status}: {cnt}")
        if self.errors:
            print(f"\nПримеры ошибок (первые 8):")
            for err in self.errors[:8]:
                print(f"  - {err}")
        print(f"{'=' * 70}\n")


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _pct(part: int, total: int) -> float:
    return part / total * 100 if total > 0 else 0.0


def _avg(vals: List[float]) -> float:
    return sum(vals) / len(vals) if vals else 0.0


def _p(sorted_vals: List[float], percentile: int) -> float:
    if not sorted_vals:
        return 0.0
    idx = int(len(sorted_vals) * percentile / 100)
    return sorted_vals[min(idx, len(sorted_vals) - 1)]


# ---------------------------------------------------------------------------
# API helpers
# ---------------------------------------------------------------------------

async def fetch_cluster_state(session: aiohttp.ClientSession, balancer: str) -> Optional[dict]:
    """Получить состояние кластера."""
    try:
        async with session.get(f"{balancer}/api/v1/cluster/state", timeout=aiohttp.ClientTimeout(total=5)) as resp:
            if resp.status == 200:
                return await resp.json()
    except Exception as exc:
        print(f"  [cluster-state] Ошибка: {exc}")
    return None


def _extract_backend(resp_json: dict) -> str:
    """Извлечь идентификатор бэкенда из ответа."""
    for key in ("backend", "server", "backend_id"):
        val = resp_json.get(key)
        if val:
            return str(val)
    # Костыль: в теле ошибки может быть {"error":{"backend":"..."},...}
    err = resp_json.get("error", {})
    if isinstance(err, dict):
        for key in ("backend", "backend_id"):
            val = err.get(key)
            if val:
                return str(val)
    return "unknown"


async def send_chat_request(
    session: aiohttp.ClientSession,
    balancer: str,
    request_id: int,
    user_id: str,
    model: str,
    prompt: str,
    timeout: int = 120,
) -> RequestResult:
    """Отправить один /api/generate запрос."""
    payload = {
        "model": model,
        "prompt": prompt,
        "stream": False,
        "options": {"num_predict": 15},
    }
    start = time.time()
    try:
        async with session.post(
            f"{balancer}/api/generate",
            json=payload,
            timeout=aiohttp.ClientTimeout(total=timeout),
        ) as resp:
            elapsed = time.time() - start
            body = await resp.text()
            try:
                js = json.loads(body)
            except json.JSONDecodeError:
                js = {}
            backend = _extract_backend(js)
            return RequestResult(
                request_id=request_id,
                user_id=user_id,
                model=model,
                status=resp.status,
                elapsed_sec=elapsed,
                success=resp.status == 200,
                backend=backend,
                error="" if resp.status == 200 else body[:200],
            )
    except asyncio.TimeoutError:
        return RequestResult(request_id, user_id, model, 0, time.time() - start, False, error="Timeout")
    except aiohttp.ClientError as exc:
        return RequestResult(request_id, user_id, model, 0, time.time() - start, False, error=str(exc)[:200])
    except Exception as exc:
        return RequestResult(request_id, user_id, model, 0, time.time() - start, False, error=f"{type(exc).__name__}: {exc}"[:200])


# ---------------------------------------------------------------------------
# Prompts pool
# ---------------------------------------------------------------------------

PROMPTS = [
    "What is the capital of France?",
    "Explain machine learning in one paragraph.",
    "Write a short poem about technology.",
    "What is the difference between TCP and UDP?",
    "Describe the Solar System in 3 sentences.",
    "How does a transformer model work?",
    "What are the benefits of load balancing?",
    "Explain the concept of microservices.",
    "What is the meaning of life?",
    "Write a Python function that sorts a list.",
    "Describe REST API design principles.",
    "What is Docker and why is it useful?",
    "Explain Kubernetes in simple terms.",
    "How do GPUs accelerate AI workloads?",
    "What is the CAP theorem?",
]


# ---------------------------------------------------------------------------
# Сценарий A: 4 пользователя, 2 модели
# ---------------------------------------------------------------------------

async def scenario_a(session: aiohttp.ClientSession, balancer: str, args) -> ScenarioReport:
    """
    4 пользователя, каждый чередует запросы между 2 моделями.
    Модели по умолчанию: модель-A и модель-B (задаются через --models).
    Каждый пользователь делает N запросов, переключаясь между моделями.
    """
    models = args.models if args.models else ["llama3.2", "gemma2:2b"]
    if len(models) < 2:
        print("[scenario A] Нужно минимум 2 модели через --models")
        return ScenarioReport(name="A: 4 users × 2 models", total=0)

    num_users = 4
    requests_per_user = args.requests // num_users if args.requests else 5
    total = num_users * requests_per_user
    print(f"[A] {num_users} пользователей × {requests_per_user} запросов × 2 модели ({models[0]}, {models[1]})")
    print(f"[A] Всего запросов: {total}")

    report = ScenarioReport(name="A: 4 users × 2 models (concurrent)", total=total)
    sem = asyncio.Semaphore(args.concurrent or 4)

    async def user_worker(user_idx: int):
        user_id = f"user-{user_idx}"
        for i in range(requests_per_user * 2):  # ×2 because alternating models
            model = models[i % len(models)]
            prompt = PROMPTS[(user_idx * 10 + i) % len(PROMPTS)]
            async with sem:
                result = await send_chat_request(
                    session, balancer, request_id=user_idx * 100 + i,
                    user_id=user_id, model=model, prompt=prompt,
                    timeout=args.timeout,
                )
                report.add(result)

    start = time.time()
    tasks = [user_worker(u) for u in range(num_users)]
    await asyncio.gather(*tasks)
    duration = time.time() - start

    report.print_report(duration)
    return report


# ---------------------------------------------------------------------------
# Сценарий B: Полная загрузка + подгрузка на свободный (3 фазы)
# ---------------------------------------------------------------------------

async def scenario_b(session: aiohttp.ClientSession, balancer: str, args) -> ScenarioReport:
    """
    Три фазы:
      Фаза 1 (normal):    умеренная нагрузка — baseline
      Фаза 2 (overload):  высокая нагрузка, кластер под давлением
      Фаза 3 (warmup):    новая модель → проверка warmup на свободный бэкенд

    В фазах 1–2 используется модель_1 (--models[0]), в фазе 3 — модель_2.
    """
    models = args.models if args.models else ["llama3.2", "gemma2:2b"]
    model1 = models[0]
    model2 = models[1] if len(models) > 1 else model1

    phase1_requests = max(args.requests // 3, 5)
    phase2_requests = max(args.requests // 3, 5)
    phase3_requests = max(args.requests // 3, 5)
    concurrent_phase12 = max(args.concurrent or 4, 2)
    concurrent_phase3 = min(args.concurrent or 4, 4)

    total = phase1_requests + phase2_requests + phase3_requests
    print(f"[B] Фаза 1 (normal) : {phase1_requests} запросов, concurrent={concurrent_phase12}, model={model1}")
    print(f"[B] Фаза 2 (overload): {phase2_requests} запросов, concurrent={concurrent_phase12}, model={model1}")
    print(f"[B] Фаза 3 (warmup):  {phase3_requests} запросов, concurrent={concurrent_phase3},  model={model2}")
    print(f"[B] Всего запросов: {total}")

    overall = ScenarioReport(name="B: Full load + warmup (3 phases)", total=total)

    # ---------- Фаза 1: normal ----------
    print("\n  >>> Фаза 1: Normal load")
    phase1 = ScenarioReport(name="  B1: Normal load", total=phase1_requests)
    sem1 = asyncio.Semaphore(concurrent_phase12)

    async def worker_phase1(req_id: int):
        prompt = PROMPTS[req_id % len(PROMPTS)]
        async with sem1:
            r = await send_chat_request(session, balancer, req_id, "phase1", model1, prompt, args.timeout)
            phase1.add(r)
            overall.add(r)

    t1 = time.time()
    await asyncio.gather(*[worker_phase1(i) for i in range(phase1_requests)])
    d1 = time.time() - t1
    phase1.print_report(d1)

    # Важно: дать системе выдохнуть, очередь очистится
    print("  ... пауза 3 сек между фазами ...\n")
    await asyncio.sleep(3)

    # ---------- Фаза 2: overload ----------
    print("  >>> Фаза 2: Overload")
    phase2 = ScenarioReport(name="  B2: Overload", total=phase2_requests)
    sem2 = asyncio.Semaphore(concurrent_phase12)

    async def worker_phase2(req_id: int):
        prompt = PROMPTS[(req_id + 100) % len(PROMPTS)]
        async with sem2:
            r = await send_chat_request(session, balancer, req_id, "phase2", model1, prompt, args.timeout)
            phase2.add(r)
            overall.add(r)

    t2 = time.time()
    await asyncio.gather(*[worker_phase2(i) for i in range(phase2_requests)])
    d2 = time.time() - t2
    phase2.print_report(d2)

    print("  ... пауза 3 сек между фазами ...\n")
    await asyncio.sleep(3)

    # ---------- Фаза 3: warmup на новую модель ----------
    print("  >>> Фаза 3: Warmup (новая модель на свободный бэкенд)")
    phase3 = ScenarioReport(name="  B3: Warmup on free backend", total=phase3_requests)
    sem3 = asyncio.Semaphore(concurrent_phase3)

    async def worker_phase3(req_id: int):
        prompt = PROMPTS[(req_id + 200) % len(PROMPTS)]
        async with sem3:
            r = await send_chat_request(session, balancer, req_id, "phase3", model2, prompt, args.timeout)
            phase3.add(r)
            overall.add(r)

    t3 = time.time()
    await asyncio.gather(*[worker_phase3(i) for i in range(phase3_requests)])
    d3 = time.time() - t3
    phase3.print_report(d3)

    # Итог
    overall.name = "B: Full load + warmup (общий)"
    overall.print_report(d1 + d2 + d3)
    return overall


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

SCENARIOS = {
    "A": scenario_a,
    "B": scenario_b,
}


async def print_cluster_summary(session: aiohttp.ClientSession, balancer: str):
    """Отладочная печать состояния кластера."""
    state = await fetch_cluster_state(session, balancer)
    if not state:
        print("  Кластер недоступен.")
        return
    print(f"  Бэкендов: {len(state.get('backends', {}))}")
    for bid, b in state.get("backends", {}).items():
        healthy = "✓" if b.get("healthy") else "✗"
        models = b.get("running_models", []) or b.get("models", [])
        if isinstance(models, list):
            model_names = [m.get("name", m) if isinstance(m, dict) else str(m) for m in models]
        else:
            model_names = [str(models)]
        print(f"    [{healthy}] {bid}  models={model_names}  rps={b.get('requests_per_second', 0):.1f}")


async def main():
    parser = argparse.ArgumentParser(description="Комплексные нагрузочные тесты Ollama Legion")
    parser.add_argument("--balancer", default="http://localhost:8080", help="URL балансера")
    parser.add_argument("--scenario", choices=["A", "B", "all"], default="all",
                        help="Сценарий: A (4 users × 2 models), B (3-phase load+warmup), all (оба)")
    parser.add_argument("--requests", "-n", type=int, default=20, help="Суммарное число запросов")
    parser.add_argument("--concurrent", "-c", type=int, default=4, help="Максимальная параллельность")
    parser.add_argument("--models", nargs="+", help="Список моделей (напр. --models llama3.2 gemma2:2b)")
    parser.add_argument("--timeout", type=int, default=120, help="Таймаут одного запроса (сек)")
    args = parser.parse_args()

    print(f"Комплексный нагрузочный тест: {args.balancer}")
    print(f"Сценарий: {args.scenario}")
    if args.models:
        print(f"Модели: {args.models}")
    print(f"Запросов: {args.requests}, Параллельность: {args.concurrent}\n")

    connector = aiohttp.TCPConnector(limit=50)
    async with aiohttp.ClientSession(connector=connector) as session:
        # Состояние кластера ДО
        print("--- Состояние кластера ДО ---")
        await print_cluster_summary(session, args.balancer)

        if args.scenario == "all":
            scenarios_to_run = ["A", "B"]
        else:
            scenarios_to_run = [args.scenario]

        for sc_name in scenarios_to_run:
            sc_func = SCENARIOS[sc_name]
            await sc_func(session, args.balancer, args)
            # Пауза между сценариями
            if len(scenarios_to_run) > 1 and sc_name != scenarios_to_run[-1]:
                print("... пауза 5 сек перед следующим сценарием ...\n")
                await asyncio.sleep(5)

        # Состояние кластера ПОСЛЕ
        print("--- Состояние кластера ПОСЛЕ ---")
        await print_cluster_summary(session, args.balancer)


if __name__ == "__main__":
    asyncio.run(main())