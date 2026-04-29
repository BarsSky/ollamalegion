#!/usr/bin/env python3
"""
Нагрузочный тест для проверки работы балансировщика Ollama Legion.

Пример запуска:
    python scripts/load_test.py --balancer http://localhost:8080 --model llama3.2 --requests 10 --concurrent 3
"""

import argparse
import json
import sys
import time
import concurrent.futures
from dataclasses import dataclass, field
from typing import List
import urllib.request
import urllib.error


@dataclass
class TestResult:
    success: bool
    backend: str = ""
    latency_ms: float = 0.0
    error: str = ""
    response_preview: str = ""


@dataclass
class LoadTestReport:
    total_requests: int
    successful: int = 0
    failed: int = 0
    by_backend: dict = field(default_factory=dict)
    latencies: List[float] = field(default_factory=list)
    errors: List[str] = field(default_factory=list)

    def add(self, r: TestResult):
        if r.success:
            self.successful += 1
            self.by_backend[r.backend] = self.by_backend.get(r.backend, 0) + 1
            self.latencies.append(r.latency_ms)
        else:
            self.failed += 1
            self.errors.append(r.error)

    def print_report(self):
        print("\n" + "=" * 60)
        print("ОТЧЁТ О НАГРУЗОЧНОМ ТЕСТЕ")
        print("=" * 60)
        print(f"Всего запросов: {self.total_requests}")
        print(f"Успешных:       {self.successful}")
        print(f"Ошибок:         {self.failed}")
        if self.latencies:
            avg = sum(self.latencies) / len(self.latencies)
            min_lat = min(self.latencies)
            max_lat = max(self.latencies)
            print(f"\nЗадержки (мс):  avg={avg:.1f} min={min_lat:.1f} max={max_lat:.1f}")
        print(f"\nРаспределение по бэкендам:")
        for backend, count in sorted(self.by_backend.items(), key=lambda x: -x[1]):
            pct = count / self.total_requests * 100
            print(f"  {backend}: {count} ({pct:.1f}%)")
        if self.errors:
            print(f"\nПримеры ошибок:")
            for err in self.errors[:5]:
                print(f"  - {err}")


def fetch_status(balancer_url: str) -> dict:
    """Получение состояния кластера от балансера."""
    req = urllib.request.Request(
        f"{balancer_url}/api/v1/status",
        headers={"Accept": "application/json"},
    )
    with urllib.request.urlopen(req, timeout=10) as resp:
        return json.loads(resp.read().decode("utf-8"))


def fetch_cluster_state(balancer_url: str) -> dict:
    """Получение детального состояния кластера."""
    req = urllib.request.Request(
        f"{balancer_url}/api/v1/cluster/state",
        headers={"Accept": "application/json"},
    )
    with urllib.request.urlopen(req, timeout=10) as resp:
        return json.loads(resp.read().decode("utf-8"))


def send_generate(balancer_url: str, model: str, prompt: str, timeout: int = 60) -> TestResult:
    """Отправка запроса /api/generate через балансер."""
    start = time.time()
    payload = {
        "model": model,
        "prompt": prompt,
        "stream": False,
        "options": {"num_predict": 10},  # короткий ответ для скорости
    }
    data = json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(
        f"{balancer_url}/api/generate",
        data=data,
        headers={
            "Content-Type": "application/json",
            "Accept": "application/json",
        },
        method="POST",
    )

    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            body = resp.read().decode("utf-8")
            latency = (time.time() - start) * 1000
            try:
                result = json.loads(body)
                # Бэкенд может вернуться в разных полях
                backend = result.get("backend", result.get("server", "unknown"))
                preview = result.get("response", body[:100])
                return TestResult(
                    success=True,
                    backend=backend,
                    latency_ms=latency,
                    response_preview=preview,
                )
            except json.JSONDecodeError:
                return TestResult(
                    success=True,
                    backend="unknown",
                    latency_ms=latency,
                    response_preview=body[:100],
                )
    except urllib.error.HTTPError as e:
        latency = (time.time() - start) * 1000
        body = e.read().decode("utf-8") if e.fp else ""
        return TestResult(
            success=False,
            latency_ms=latency,
            error=f"HTTP {e.code}: {body[:200]}",
        )
    except Exception as e:
        latency = (time.time() - start) * 1000
        return TestResult(
            success=False,
            latency_ms=latency,
            error=f"{type(e).__name__}: {str(e)[:200]}",
        )


def run_load_test(balancer_url: str, model: str, num_requests: int, concurrency: int) -> LoadTestReport:
    """Запуск нагрузочного теста."""
    report = LoadTestReport(total_requests=num_requests)
    prompts = [
        "Hello, how are you?",
        "What is the capital of France?",
        "Explain quantum computing in one sentence.",
        "Write a haiku about AI.",
        "What is 2+2?",
    ]

    with concurrent.futures.ThreadPoolExecutor(max_workers=concurrency) as executor:
        futures = []
        for i in range(num_requests):
            prompt = prompts[i % len(prompts)]
            future = executor.submit(send_generate, balancer_url, model, prompt)
            futures.append(future)

        for i, future in enumerate(concurrent.futures.as_completed(futures)):
            result = future.result()
            report.add(result)
            # Прогресс
            if (i + 1) % max(1, num_requests // 10) == 0 or i == num_requests - 1:
                print(f"  Прогресс: {i + 1}/{num_requests} ({report.successful} OK, {report.failed} ERR)")

    return report


def print_cluster_state(balancer_url: str):
    """Вывод состояния кластера."""
    print("\n--- Состояние кластера ---")
    try:
        status = fetch_status(balancer_url)
        print(f"Бэкендов: {status.get('backends', {}).get('total', '?')}")
        print(f"Запросов/с: {status.get('metrics', {}).get('requestsPerSecond', 0):.2f}")
        print(f"Активных запросов: {status.get('metrics', {}).get('activeRequests', 0)}")

        state = fetch_cluster_state(balancer_url)
        for sid, backend in state.get("backends", {}).items():
            healthy = "✓" if backend.get("healthy", False) else "✗"
            host = backend.get("host", "?")
            port = backend.get("ollamaPort", 11434)
            models = len(backend.get("running_models", []))
            req_sec = backend.get("requests_per_second", 0)
            print(f"  [{healthy}] {sid} @ {host}:{port} | models={models} | rps={req_sec:.2f}")
    except Exception as e:
        print(f"  Ошибка получения состояния: {e}")


def main():
    parser = argparse.ArgumentParser(description="Нагрузочный тест Ollama Legion балансера")
    parser.add_argument("--balancer", default="http://localhost:8080", help="URL балансера")
    parser.add_argument("--model", default="llama3.2", help="Модель для тестирования")
    parser.add_argument("--requests", "-n", type=int, default=10, help="Количество запросов")
    parser.add_argument("--concurrent", "-c", type=int, default=3, help="Параллельность")
    parser.add_argument("--timeout", type=int, default=60, help="Таймаут запроса (сек)")
    args = parser.parse_args()

    print(f"Нагрузочный тест: {args.balancer}")
    print(f"Модель: {args.model}")
    print(f"Запросов: {args.requests}, Параллельность: {args.concurrent}")

    # Проверяем состояние ДО теста
    print_cluster_state(args.balancer)

    # Запускаем тест
    print(f"\n--- Запуск нагрузки ({args.requests} запросов, {args.concurrent} параллельных) ---")
    start = time.time()
    report = run_load_test(args.balancer, args.model, args.requests, args.concurrent)
    duration = time.time() - start

    # Отчёт
    report.print_report()
    print(f"\nОбщее время теста: {duration:.1f} сек")
    print(f"Запросов в секунду: {args.requests / duration:.1f}")

    # Проверяем состояние ПОСЛЕ теста
    print_cluster_state(args.balancer)


if __name__ == "__main__":
    main()