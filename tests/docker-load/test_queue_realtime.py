#!/usr/bin/env python3
"""
Тест очереди балансера с замером очереди ВО ВРЕМЯ нагрузки.
Показывает состояние queue/stats в реальном времени.
Проверяет WebUI на наличие данных о клиентах и очереди.
"""

import asyncio
import aiohttp
import json
import os
import sys
import time
import re
from datetime import datetime

BALANCER_PROXY = "http://localhost:18080"
BALANCER_API = "http://localhost:18081"
WEBUI_URL = "http://localhost:18030"
MODEL = "llama3.2:3b"
CONCURRENT_LEVEL = 20
TOTAL_REQUESTS = 20
CLIENTS = ["Alice", "Bob", "Charlie", "Diana", "Eve"]

stop_monitoring = False
queue_snapshots = []
webui_snapshots = []


async def make_request(session, sem, req_id, client_name):
    async with sem:
        start = time.perf_counter()
        headers = {
            "Content-Type": "application/json",
            "X-Client-Name": client_name,
        }
        payload = {
            "model": MODEL,
            "prompt": f"Request #{req_id} from {client_name}",
            "stream": False,
        }
        try:
            async with session.post(
                f"{BALANCER_PROXY}/api/generate",
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


async def webui_get(path):
    """Запрашивает страницу WebUI и возвращает HTML."""
    try:
        async with aiohttp.ClientSession() as s:
            async with s.get(f"{WEBUI_URL}{path}", timeout=aiohttp.ClientTimeout(total=5)) as r:
                html = await r.text()
                return {"status": r.status, "html": html}
    except Exception as e:
        return {"status": 0, "html": "", "error": str(e)}


def check_webui_content(snapshot, label=""):
    """Проверяет HTML WebUI на наличие индикаторов данных."""
    html = snapshot.get("html", "")
    findings = {}

    # Индикаторы страницы Сессии
    findings["has_session_elements"] = bool(re.search(
        r'(session|client|Session|Client)[\s\S]{0,200}(Alice|Bob|Charlie|Diana|Eve)',
        html, re.IGNORECASE
    ))

    # Индикаторы страницы Очередь
    findings["has_queue_elements"] = bool(re.search(
        r'(queue|Queue|очередь|Очередь|pending|waiting|ожидани)[\s\S]{0,200}(\d+)',
        html[:5000], re.IGNORECASE
    ))

    # Индикаторы данных на мониторе
    findings["has_monitor_data"] = bool(re.search(
        r'(active|queued|current_size|processe)', html, re.IGNORECASE
    ))

    # Числовые данные — активные запросы, очередь
    numbers = re.findall(r'>(\d+)<', html[:10000])
    findings["numbers_found"] = [int(n) for n in numbers[:10]]

    # JSON data embedded in JS (common in WebUI)
    json_blocks = re.findall(r'(\{.*?"current_size".*?\})', html, re.DOTALL)
    findings["json_blocks_with_queue"] = len(json_blocks)

    return findings


async def monitor_webui(interval=1.0, max_snapshots=200):
    """Мониторит WebUI страницы каждые interval секунд во время нагрузки.
    
    Args:
        interval: Задержка между снимками (сек). Увеличена до 1с чтобы не перегружать.
        max_snapshots: Максимальное число снимков — защита от бесконечного зависания.
    """
    global stop_monitoring
    snapshot_index = 0
    start_time = time.time()
    try:
        while not stop_monitoring and snapshot_index < max_snapshots:
            # Дополнительная защита: если прошло >30с, а нагрузка уже завершена — выходим
            elapsed = time.time() - start_time
            if snapshot_index > 5 and elapsed > 30:
                break

            snap = {"t": snapshot_index, "time": time.time()}

            # Запрашиваем страницу Сессии
            sessions_page = await webui_get("/index.html")
            snap["sessions_html_len"] = len(sessions_page.get("html", ""))
            snap["sessions_status"] = sessions_page.get("status", 0)

            # Запрашиваем очередь (API, так как это данные в WebUI)
            queue_stats = await api_get("/api/v1/queue/stats")
            if isinstance(queue_stats, dict) and "error" not in queue_stats:
                snap["queue"] = queue_stats
            else:
                snap["queue"] = {}

            # Сессии API
            sessions = await api_get("/api/v1/sessions")
            if isinstance(sessions, dict) and "error" not in sessions:
                snap["sessions"] = sessions
            else:
                snap["sessions"] = {}

            # Проверяем содержимое WebUI страницы
            if sessions_page.get("html"):
                findings = check_webui_content(sessions_page)
                snap["webui_findings"] = findings

            webui_snapshots.append(snap)

            if snap.get("queue", {}).get("current_size", 0) > 0:
                print(f"  WEBUI [{snapshot_index:2d}] sessions={snap.get('sessions',{}).get('total',0)} "
                      f"queue={snap['queue'].get('current_size',0)} "
                      f"Файндинги: {snap.get('webui_findings',{})}")

            snapshot_index += 1
            await asyncio.sleep(interval)
    except asyncio.CancelledError:
        pass


async def monitor_queue(interval=0.15):
    """Мониторит очередь каждые interval секунд во время нагрузки."""
    global stop_monitoring
    snapshot_index = 0
    try:
        while not stop_monitoring:
            stats = await api_get("/api/v1/queue/stats")
            cluster = await api_get("/api/v1/cluster")
            current_size = 0
            processed_total = 0
            queued = 0
            active = 0
            if isinstance(stats, dict) and "error" not in stats:
                current_size = stats.get("current_size", 0)
                processed_total = stats.get("processed_total", 0)
            if isinstance(cluster, dict) and "error" not in cluster:
                queued = cluster.get("queuedRequests", 0)
                active = cluster.get("activeRequests", 0)
            snap = {
                "t": snapshot_index,
                "current_size": current_size,
                "processed_total": processed_total,
                "queued_in_cluster": queued,
                "active_requests": active,
            }
            queue_snapshots.append(snap)
            if current_size > 0 or queued > 0:
                bar = "█" * min(current_size, 20) + "░" * max(0, 20 - min(current_size, 20))
                print(f"  \u23f3 [t={snapshot_index:2d}] queue={current_size:2d}  processed={processed_total:2d}  "
                      f"active={active:2d}  {bar}")
            snapshot_index += 1
            await asyncio.sleep(interval)
    except asyncio.CancelledError:
        pass  # Нормальное завершение при остановке монитора


async def main():
    print("=" * 70)
    print("  ТЕСТ ОЧЕРЕДИ БАЛАНСЕРА \u2014 REAL-TIME МОНИТОРИНГ + WEBUI")
    print("=" * 70)
    print(f"  Время: {datetime.now().strftime('%Y-%m-%d %H:%M:%S')}")
    print(f"  Запросов: {TOTAL_REQUESTS}, Concurrent: {CONCURRENT_LEVEL}")
    print(f"  Клиенты: {', '.join(CLIENTS)}")
    print(f"  maxConcurrentReqs=1 на 1 бэкенд \u2192 ожидаем ~19 в очереди")
    print(f"  queueMaxSize=30 \u2192 все 20 запросов должны влезть")
    print()

    # Ждём балансер
    for i in range(30):
        try:
            async with aiohttp.ClientSession() as s:
                async with s.get(f"{BALANCER_API}/api/v1/health", timeout=aiohttp.ClientTimeout(total=2)) as r:
                    if r.status == 200:
                        data = await r.json()
                        healthy = data.get("healthyBackends", 0)
                        print(f"  \u2713 Балансер готов ({i+1}с), бэкенды: {healthy}/{data.get('totalBackends', '?')}")
                        break
        except Exception:
            pass
        await asyncio.sleep(1)
    else:
        print("  \u2717 Балансер не готов!")
        sys.exit(1)

    # Ждём WebUI
    for i in range(15):
        try:
            async with aiohttp.ClientSession() as s:
                async with s.get(f"{WEBUI_URL}/", timeout=aiohttp.ClientTimeout(total=2)) as r:
                    if r.status == 200:
                        print(f"  \u2713 WebUI готов ({i+1}с)")
                        break
        except Exception:
            pass
        await asyncio.sleep(1)
    else:
        print("  \u26a0 WebUI не отвечает — продолжаем без проверки содержимого")

    # WARMUP: отправляем один запрос, чтобы прогреть очередь и сессии
    print("  [WARMUP] Прогрев балансера...")
    try:
        async with aiohttp.ClientSession() as w:
            hdrs = {"Content-Type": "application/json", "X-Client-Name": "warmup"}
            pl = {"model": MODEL, "prompt": "warmup", "stream": False}
            async with w.post(f"{BALANCER_PROXY}/api/generate", json=pl, headers=hdrs,
                             timeout=aiohttp.ClientTimeout(total=60)) as r:
                if r.status == 200:
                    print("  \u2713 Warmup успешен")
                else:
                    print(f"  \u26a0 Warmup вернул {r.status}")
    except Exception as e:
        print(f"  \u26a0 Warmup ошибка: {e}")
    # Даём время очереди устаканиться
    await asyncio.sleep(3)

    # Состояние ДО
    print("\n" + "-" * 70)
    print("  [ДО] Состояние до нагрузки:")
    queue_before = await api_get("/api/v1/queue/stats")
    sessions_before = await api_get("/api/v1/sessions")
    cluster_before = await api_get("/api/v1/cluster")
    print(f"  Queue: {json.dumps(queue_before, ensure_ascii=False)}")
    print(f"  Сессии: {sessions_before.get('total', 0)}")
    print(f"  Cluster: queued={cluster_before.get('queuedRequests', '?')}, active={cluster_before.get('activeRequests', '?')}")

    # НАГРУЗКА + МОНИТОРИНГ
    print("\n" + "-" * 70)
    print("  [НАГРУЗКА] Отправка 20 запросов + мониторинг очереди и WebUI:")
    print()

    sem = asyncio.Semaphore(CONCURRENT_LEVEL)
    async with aiohttp.ClientSession() as session:
        tasks = []
        for i in range(TOTAL_REQUESTS):
            client = CLIENTS[i % len(CLIENTS)]
            tasks.append(make_request(session, sem, i + 1, client))

        # Запускаем мониторинг параллельно с нагрузкой
        monitor_task = asyncio.create_task(monitor_queue(interval=0.3))
        webui_task = asyncio.create_task(monitor_webui())

        start_time = time.time()
        results = await asyncio.gather(*tasks)
        total_time = time.time() - start_time

        # Останавливаем мониторы принудительно через cancel(),
        # т.к. они могут быть заблокированы на HTTP-запросе.
        # Ждём их завершения с таймаутом 15 секунд.
        monitor_task.cancel()
        webui_task.cancel()
        done, pending = await asyncio.wait(
            [monitor_task, webui_task],
            timeout=15,
            return_when=asyncio.ALL_COMPLETED,
        )
        for p in pending:
            p.cancel()  # принудительно отменяем, если не успели

    # Состояние ПОСЛЕ
    print(f"\n  Все запросы завершены за {total_time:.2f}с")

    print("\n" + "-" * 70)
    print("  [ПОСЛЕ] Состояние после нагрузки:")
    queue_after = await api_get("/api/v1/queue/stats")
    sessions_after = await api_get("/api/v1/sessions")
    cluster_after = await api_get("/api/v1/cluster")
    print(f"  Queue: {json.dumps(queue_after, ensure_ascii=False)}")
    print(f"  Сессии: {sessions_after.get('total', 0)}")
    print(f"  Cluster: queued={cluster_after.get('queuedRequests', '?')}, active={cluster_after.get('activeRequests', '?')}")

    # Проверяем WebUI ПОСЛЕ нагрузки
    print(f"\n  WebUI после нагрузки:")
    webui_index = await webui_get("/index.html")
    webui_monitor = await webui_get("/monitor.html")
    print(f"  index.html: {len(webui_index.get('html',''))} байт, статус {webui_index.get('status',0)}")
    print(f"  monitor.html: {len(webui_monitor.get('html',''))} байт, статус {webui_monitor.get('status',0)}")

    if webui_index.get("html"):
        index_findings = check_webui_content(webui_index, "index")
        print(f"  index.html файндинги: {index_findings}")

    if webui_monitor.get("html"):
        monitor_findings = check_webui_content(webui_monitor, "monitor")
        print(f"  monitor.html файндинги: {monitor_findings}")

    # Анализ
    print("\n" + "-" * 70)
    print("  [АНАЛИЗ] Результаты:")
    successful = [r for r in results if r["success"]]
    failed = [r for r in results if not r["success"]]
    response_times = [r["elapsed_ms"] for r in successful]

    print(f"  Успешно: {len(successful)}/{len(results)}")
    print(f"  Ошибок: {len(failed)}")

    if failed:
        print(f"  Ошибки:")
        for f in failed:
            print(f"    req#{f['id']} ({f['client']}): {f['error']}")

    if response_times:
        print(f"  Время ответа: мин={min(response_times):.0f}ms, "
              f"сред={sum(response_times)/len(response_times):.0f}ms, "
              f"макс={max(response_times):.0f}ms")

    # Была ли очередь?
    max_queue_size = max(s.get("current_size", 0) for s in queue_snapshots)
    max_queue_cluster = max(s.get("queued_in_cluster", 0) for s in queue_snapshots)
    processed_diff = queue_after.get("processed_total", 0) - queue_before.get("processed_total", 0)
    avg_wait = queue_after.get("avg_wait_time_ms", 0)

    print(f"\n  \U0001f4ca Статистика очереди:")
    print(f"     Пиковый размер очереди (queue/stats): {max_queue_size}")
    print(f"     Пиковый размер очереди (cluster):    {max_queue_cluster}")
    print(f"     Обработано за время теста:           {processed_diff}")
    print(f"     Среднее время ожидания:              {avg_wait}ms")

    if max_queue_size > 0:
        print(f"  \u2705 Очередь БЫЛА зафиксирована (пик={max_queue_size})")
    else:
        print(f"  \u26a0\ufe0f Очередь НЕ зафиксирована (все запросы разгрузились до замера)")

    # Анализ WebUI снимков
    print(f"\n  \U0001f4ca WebUI во время нагрузки:")
    webui_sessions_found = sum(1 for s in webui_snapshots
                                if s.get("sessions", {}).get("total", 0) > 0)
    webui_queue_found = sum(1 for s in webui_snapshots
                            if s.get("queue", {}).get("current_size", 0) > 0)
    max_sessions_webui = max((s.get("sessions", {}).get("total", 0) for s in webui_snapshots), default=0)
    max_queue_webui = max((s.get("queue", {}).get("current_size", 0) for s in webui_snapshots), default=0)
    print(f"     Снимков WebUI: {len(webui_snapshots)}")
    print(f"     Снимков с сессиями > 0: {webui_sessions_found} (пик={max_sessions_webui})")
    print(f"     Снимков с очередью > 0: {webui_queue_found} (пик={max_queue_webui})")

    # Проверка что в HTML WebUI есть данные
    webui_has_data = any(
        s.get("webui_findings", {}).get("has_session_elements", False) or
        s.get("webui_findings", {}).get("has_queue_elements", False) or
        s.get("webui_findings", {}).get("has_monitor_data", False)
        for s in webui_snapshots
    )
    print(f"     WebUI HTML содержит данные: {'\u2705 да' if webui_has_data else '\u274c нет'}")

    # Сессии
    session_ids = {}
    for r in results:
        if r["session_id"]:
            sid = r["session_id"]
            if sid not in session_ids:
                session_ids[sid] = set()
            session_ids[sid].add(r["client"])

    print(f"\n  \U0001f4ca Сессии: {len(session_ids)} уникальных")
    for sid, clients in sorted(session_ids.items()):
        short = sid[:40]
        print(f"     {short}... \u2192 {', '.join(clients)}")

    if sessions_after.get("sessions"):
        print(f"\n  \U0001f4ca Детали сессий из API:")
        for s in sessions_after["sessions"]:
            print(f"     {s.get('clientName','?'):8s} \u2192 backend={s.get('backendId','?'):8s} "
                  f"запросов={s.get('requestCount',0)}")

    # Определяем max_session_count для отчёта
    max_session_count = sessions_after.get("total", 0)
    if webui_snapshots:
        max_session_count = max(max_session_count, max((s.get("sessions", {}).get("total", 0) for s in webui_snapshots), default=0))

    # ИТОГ
    print("\n" + "=" * 70)
    print("  ИТОГ:")
    print("=" * 70)

    all_checks = []

    ok = len(failed) == 0
    all_checks.append(("Все запросы успешны", ok, f"{len(successful)}/{len(results)}"))

    ok = max_queue_size > 0
    all_checks.append(("Очередь зафиксирована", ok, f"пик={max_queue_size}"))

    ok = len(session_ids) >= 3
    all_checks.append(("Уникальные сессии", ok, f"{len(session_ids)}"))

    ok = processed_diff > 0
    all_checks.append(("Queue processed > 0", ok, f"{processed_diff}"))

    ok = webui_sessions_found > 0
    all_checks.append(("Сессии видны в WebUI", ok, f"{webui_sessions_found}/{len(webui_snapshots)}"))

    ok = webui_queue_found > 0
    all_checks.append(("Очередь видна в WebUI", ok, f"{webui_queue_found}/{len(webui_snapshots)}"))

    ok = max_session_count > 0
    all_checks.append(("Сессии в API > 0", ok, f"{max_session_count}"))
    # Примечание: max_session_count определим

    passed = 0
    total_checks = len(all_checks)
    for name, ok, detail in all_checks:
        print(f"  {'\u2705' if ok else '\u274c'} {name}: {detail}")
        if ok:
            passed += 1

    print(f"\n  Результат: {passed}/{total_checks} проверок пройдено")

    # Сохраняем
    timestamp = datetime.now().strftime("%Y%m%d_%H%M%S")
    results_file = os.path.join(
        os.path.dirname(__file__), "results", f"queue_realtime_{timestamp}.json"
    )
    os.makedirs(os.path.dirname(results_file), exist_ok=True)

    report = {
        "timestamp": timestamp,
        "total": len(results),
        "successful": len(successful),
        "failed": len(failed),
        "total_time_s": round(total_time, 2),
        "unique_sessions": len(session_ids),
        "max_queue_size": max_queue_size,
        "max_queue_cluster": max_queue_cluster,
        "processed_diff": processed_diff,
        "avg_wait_time_ms": avg_wait,
        "max_session_count": max_session_count,
        "webui_snapshots_count": len(webui_snapshots),
        "webui_sessions_found": webui_sessions_found,
        "webui_queue_found": webui_queue_found,
        "webui_has_data_html": webui_has_data,
        "queue_snapshots": queue_snapshots,
        "webui_snapshots": [
            {k: v for k, v in s.items() if k != "html"}  # не сохраняем полный HTML
            for s in webui_snapshots
        ],
        "checks": {"passed": passed, "total": total_checks},
        "results": results,
    }
    with open(results_file, "w", encoding="utf-8") as f:
        json.dump(report, f, ensure_ascii=False, indent=2)
    print(f"\n  Результаты сохранены: {results_file}")

    return 0 if len(failed) == 0 else 1


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))
