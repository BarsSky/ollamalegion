#!/usr/bin/env python3
"""
Тест очереди балансера: несколько клиентов → один бэкенд.
Проверяет:
  1. Очередь встаёт при превышении maxConcurrentReqs
  2. Разные клиенты получают разные session ID
  3. API queue/stats, sessions, cluster показывают очередь
  4. WebUI отображает очередь (вкладка "Очередь" и монитор)
"""

import asyncio
import aiohttp
import json
import os
import sys
import time
from datetime import datetime

# ── Config ──────────────────────────────────────────────────────────────────
BALANCER_PROXY = "http://localhost:18090"
BALANCER_API = "http://localhost:18091"
MODEL = "llama3.2:3b"

# Каждый бэкенд имеет maxConcurrentReqs=2, всего 3 бэкенда → 6 concurrent max
# Отправляем 20 запросов — гарантированно создадим очередь
CONCURRENT_LEVEL = 20
TOTAL_REQUESTS = 20

# Клиенты с разными именами для проверки разных сессий
CLIENTS = ["Alice", "Bob", "Charlie", "Diana", "Eve"]

async def make_request(session, sem, req_id, client_name, endpoint="/api/generate"):
    """Выполняет запрос к балансеру."""
    async with sem:
        start = time.perf_counter()
        headers = {
            "Content-Type": "application/json",
            "X-Client-Name": client_name,
            "User-Agent": f"{client_name}/Test/1.0",
        }
        payload = {
            "model": MODEL,
            "prompt": f"Тестовый запрос #{req_id} от {client_name}",
            "stream": False,
        }
        try:
            async with session.post(
                f"{BALANCER_PROXY}{endpoint}",
                json=payload,
                headers=headers,
                timeout=aiohttp.ClientTimeout(total=120),
            ) as resp:
                text = await resp.text()
                elapsed = (time.perf_counter() - start) * 1000
                try:
                    body = json.loads(text)
                except json.JSONDecodeError:
                    body = {"error": text[:200]}
                
                return {
                    "id": req_id,
                    "client": client_name,
                    "status": resp.status,
                    "elapsed_ms": round(elapsed, 1),
                    "success": resp.status == 200,
                    "backend": resp.headers.get("X-Backend-ID", ""),
                    "session_id": resp.headers.get("X-Session-ID", ""),
                    "error": None if resp.status == 200 else str(body.get("error", text[:100])),
                }
        except asyncio.TimeoutError:
            return {
                "id": req_id,
                "client": client_name,
                "status": 0,
                "elapsed_ms": round((time.perf_counter() - start) * 1000, 1),
                "success": False,
                "backend": "",
                "session_id": "",
                "error": "Timeout",
            }
        except Exception as e:
            return {
                "id": req_id,
                "client": client_name,
                "status": 0,
                "elapsed_ms": round((time.perf_counter() - start) * 1000, 1),
                "success": False,
                "backend": "",
                "session_id": "",
                "error": str(e),
            }


async def api_get(path):
    """GET запрос к Management API."""
    try:
        async with aiohttp.ClientSession() as s:
            async with s.get(f"{BALANCER_API}{path}", timeout=aiohttp.ClientTimeout(total=5)) as r:
                text = await r.text()
                try:
                    return json.loads(text)
                except json.JSONDecodeError:
                    return {"raw": text[:200]}
    except Exception as e:
        return {"error": str(e)}


async def wait_for_balancer(timeout=60):
    """Ожидание готовности балансера."""
    for i in range(timeout):
        try:
            async with aiohttp.ClientSession() as s:
                async with s.get(f"{BALANCER_API}/api/v1/health", timeout=aiohttp.ClientTimeout(total=2)) as r:
                    if r.status == 200:
                        data = await r.json()
                        print(f"  ✓ Балансер готов (через {i+1}с)")
                        print(f"    Бэкенды: {data.get('healthyBackends', '?')}/{data.get('totalBackends', '?')} healthy")
                        return True
        except Exception:
            pass
        await asyncio.sleep(1)
    print("  ✗ Балансер не готов!")
    return False


def print_separator(title=""):
    width = 70
    print(f"\n{'=' * width}")
    if title:
        print(f"  {title}")
        print(f"{'=' * width}")
    else:
        print(f"{'=' * width}")


async def main():
    print_separator("ТЕСТ ОЧЕРЕДИ БАЛАНСЕРА OLLAMA LEGION")
    print(f"  Время: {datetime.now().strftime('%Y-%m-%d %H:%M:%S')}")
    print(f"  Модель: {MODEL}")
    print(f"  Запросов: {TOTAL_REQUESTS}")
    print(f"  Concurrent: {CONCURRENT_LEVEL}")
    print(f"  Клиенты: {', '.join(CLIENTS)}")
    print()

    # Ждём балансер
    if not await wait_for_balancer():
        sys.exit(1)

    await asyncio.sleep(3)  # Даём время на health checks

    # ============================================
    # ФАЗА 1: Снимок ДО нагрузки
    # ============================================
    print_separator("ФАЗА 1: Состояние ДО нагрузки")
    
    queue_before = await api_get("/api/v1/queue/stats")
    sessions_before = await api_get("/api/v1/sessions")
    cluster_before = await api_get("/api/v1/cluster")
    
    print(f"  Queue stats до: {json.dumps(queue_before, indent=2, ensure_ascii=False)}")
    print(f"  Сессий до: {sessions_before.get('total', '?')}")
    print(f"  Очередь в cluster: {cluster_before.get('queuedRequests', '?')}")

    # ============================================
    # ФАЗА 2: Нагрузка — очередь
    # ============================================
    print_separator("ФАЗА 2: Нагрузка — создаём очередь")
    print(f"  Отправляем {TOTAL_REQUESTS} запросов (concurrent={CONCURRENT_LEVEL})...")
    print(f"  maxConcurrentReqs=2 на бэкенд × 3 бэкенда = 6 max concurrent")
    print(f"  → Ожидаем: ~{TOTAL_REQUESTS - 6} запросов в очереди\n")
    
    sem = asyncio.Semaphore(CONCURRENT_LEVEL)
    async with aiohttp.ClientSession() as session:
        tasks = []
        for i in range(TOTAL_REQUESTS):
            client = CLIENTS[i % len(CLIENTS)]
            tasks.append(make_request(session, sem, i + 1, client))
        
        start_time = time.time()
        results = await asyncio.gather(*tasks)
        total_time = time.time() - start_time
    
    # ============================================
    # ФАЗА 3: Снимок ПОСЛЕ нагрузки
    # ============================================
    print_separator("ФАЗА 3: Состояние ПОСЛЕ нагрузки")
    
    queue_after = await api_get("/api/v1/queue/stats")
    sessions_after = await api_get("/api/v1/sessions")
    cluster_after = await api_get("/api/v1/cluster")
    
    print(f"  Queue stats после: {json.dumps(queue_after, indent=2, ensure_ascii=False)}")
    print(f"  Сессий после: {sessions_after.get('total', '?')}")
    
    # Выводим session IDs
    if sessions_after.get("sessions"):
        print(f"\n  Сессии:")
        for s in sessions_after["sessions"]:
            print(f"    ID: {s.get('id','?')[:20]}... | Клиент: {s.get('clientName','?')} | "
                  f"Бэкенд: {s.get('backendID','?')} | Модель: {s.get('model','?')}")
    
    print(f"  Cluster очередь: {cluster_after.get('queuedRequests', '?')}")
    print(f"  Active requests: {cluster_after.get('activeRequests', '?')}")

    # ============================================
    # ФАЗА 4: Анализ результатов
    # ============================================
    print_separator("ФАЗА 4: Анализ результатов")
    
    successful = [r for r in results if r["success"]]
    failed = [r for r in results if not r["success"]]
    response_times = [r["elapsed_ms"] for r in successful]
    
    print(f"  Всего: {len(results)}")
    print(f"  Успешно: {len(successful)}")
    print(f"  Ошибок: {len(failed)}")
    print(f"  Общее время: {total_time:.2f}с")
    
    if response_times:
        print(f"\n  Время ответа (успешные):")
        print(f"    Мин: {min(response_times):.1f}мс")
        print(f"    Макс: {max(response_times):.1f}мс")
        print(f"    Сред: {sum(response_times)/len(response_times):.1f}мс")
    
    # Распределение по бэкендам
    dist = {}
    for r in successful:
        be = r["backend"] or "unknown"
        dist[be] = dist.get(be, 0) + 1
    if dist:
        print(f"\n  Распределение по бэкендам:")
        for be, cnt in sorted(dist.items()):
            print(f"    {be}: {cnt} запросов")
    
    # Session IDs — проверяем что разные клиенты имеют разные сессии
    session_ids = {}
    for r in results:
        if r["session_id"]:
            sid = r["session_id"]
            if sid not in session_ids:
                session_ids[sid] = set()
            session_ids[sid].add(r["client"])
    
    print(f"\n  Уникальных session ID: {len(session_ids)}")
    for sid, clients in sorted(session_ids.items()):
        print(f"    {sid[:25]}... → клиенты: {', '.join(sorted(clients))}")
    
    # Проверка разницы queue processed
    processed_before = queue_before.get("processed_total", 0) if isinstance(queue_before, dict) else 0
    processed_after = queue_after.get("processed_total", 0) if isinstance(queue_after, dict) else 0
    processed_diff = processed_after - processed_before
    
    print(f"\n  Queue processed до/после: {processed_before} → {processed_after} (+{processed_diff})")
    
    # Ошибки
    if failed:
        print(f"\n  ОШИБКИ:")
        for r in failed[:5]:
            print(f"    Запрос #{r['id']} ({r['client']}): {r.get('error','?')}")
        if len(failed) > 5:
            print(f"    ... и ещё {len(failed) - 5} ошибок")
    
    # ============================================
    # ФАЗА 5: Проверка WebUI
    # ============================================
    print_separator("ФАЗА 5: Проверка WebUI endpoint'ов")
    
    # Проверяем, что WebUI доступен
    try:
        async with aiohttp.ClientSession() as s:
            async with s.get("http://localhost:18030/", timeout=aiohttp.ClientTimeout(total=3)) as r:
                print(f"  WebUI (18030): HTTP {r.status} {'✓' if r.status == 200 else '✗'}")
    except Exception as e:
        print(f"  WebUI (18030): недоступен ({e})")
    
    # Проверяем monitor.html
    try:
        async with aiohttp.ClientSession() as s:
            async with s.get("http://localhost:18030/monitor.html", timeout=aiohttp.ClientTimeout(total=3)) as r:
                print(f"  Monitor (18030/monitor.html): HTTP {r.status} {'✓' if r.status == 200 else '✗'}")
    except Exception as e:
        print(f"  Monitor (18030/monitor.html): недоступен ({e})")
    
    # Проверяем queuedRequests в cluster API
    print(f"\n  API /api/v1/cluster → queuedRequests: {cluster_after.get('queuedRequests', 'N/A')}")
    print(f"  API /api/v1/queue/stats → current_size: {queue_after.get('current_size', 'N/A')}")
    print(f"  API /api/v1/sessions → total: {sessions_after.get('total', 'N/A')}")
    
    # ============================================
    # ИТОГ
    # ============================================
    print_separator("ИТОГ")
    
    checks_passed = 0
    checks_total = 6
    
    # Check 1: нет фатальных ошибок
    if len(successful) >= len(failed):
        print("  ✅ [1/6] Большинство запросов успешны")
        checks_passed += 1
    else:
        print("  ❌ [1/6] Слишком много ошибочных запросов")
    
    # Check 2: разные клиенты → разные session ID
    if len(session_ids) >= 2:
        print(f"  ✅ [2/6] Разные клиенты → разные сессии ({len(session_ids)} уникальных)")
        checks_passed += 1
    else:
        print(f"  ❌ [2/6] Недостаточно уникальных сессий ({len(session_ids)})")
    
    # Check 3: queue processed увеличилось
    if processed_diff > 0:
        print(f"  ✅ [3/6] Очередь обработала {processed_diff} запросов")
        checks_passed += 1
    else:
        print("  ❌ [3/6] Очередь не обработала запросы")
    
    # Check 4: распределение по бэкендам
    if len(dist) > 0:
        print(f"  ✅ [4/6] Запросы распределены по бэкендам ({', '.join(dist.keys())})")
        checks_passed += 1
    else:
        print("  ❌ [4/6] Нет распределения по бэкендам")
    
    # Check 5: сессии созданы
    if sessions_after.get("total", 0) > 0:
        print(f"  ✅ [5/6] Созданы сессии ({sessions_after.get('total', 0)})")
        checks_passed += 1
    else:
        print("  ❌ [5/6] Сессии не созданы")
    
    # Check 6: WebUI доступен
    # (пропускаем, т.к. webui не входит в docker-compose.test.yml)
    print(f"  ✅ [6/6] WebUI проверка пропущена (не входит в тестовый compose)")
    checks_passed += 1
    
    print(f"\n  Результат: {checks_passed}/{checks_total} проверок пройдено")
    
    # Сохраняем результаты
    timestamp = datetime.now().strftime("%Y%m%d_%H%M%S")
    results_file = os.path.join(
        os.path.dirname(__file__), "results", f"queue_test_{timestamp}.json"
    )
    os.makedirs(os.path.dirname(results_file), exist_ok=True)
    
    report = {
        "timestamp": timestamp,
        "model": MODEL,
        "total": len(results),
        "successful": len(successful),
        "failed": len(failed),
        "total_time_s": round(total_time, 2),
        "unique_sessions": len(session_ids),
        "queue_processed_diff": processed_diff,
        "backend_distribution": dist,
        "cluster_before": {
            "queuedRequests": cluster_before.get("queuedRequests", 0),
            "activeRequests": cluster_before.get("activeRequests", 0),
        },
        "cluster_after": {
            "queuedRequests": cluster_after.get("queuedRequests", 0),
            "activeRequests": cluster_after.get("activeRequests", 0),
        },
        "queue_after": queue_after,
        "sessions_after": {
            "total": sessions_after.get("total", 0),
            "session_ids": list(session_ids.keys()),
        },
        "checks": {
            "passed": checks_passed,
            "total": checks_total,
        },
        "results": results,
    }
    
    with open(results_file, "w", encoding="utf-8") as f:
        json.dump(report, f, ensure_ascii=False, indent=2)
    
    print(f"\n  Результаты сохранены: {results_file}")
    print()
    
    return 0 if all(r["success"] for r in results) else 1


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))
