#!/usr/bin/env python3
"""
Нагрузочный тест балансера OllamaLegion
"""
import asyncio
import aiohttp
import time
import json
import sys
from datetime import datetime

BALANCER_URL = "http://localhost:18080/v1/chat/completions"
MODEL = "gemini-3-flash-preview:cloud"
CONCURRENT_REQUESTS = 5
TOTAL_REQUESTS = 20

async def make_request(session, request_id):
    """Выполняет один запрос к балансеру"""
    payload = {
        "model": MODEL,
        "messages": [{"role": "user", "content": f"Тестовый запрос #{request_id}. Кратко опиши что такое балансировка нагрузки."}],
        "stream": False
    }
    
    start_time = time.time()
    try:
        async with session.post(BALANCER_URL, json=payload, timeout=aiohttp.ClientTimeout(total=60)) as response:
            status = response.status
            response_json = await response.json()
            elapsed = time.time() - start_time
            
            return {
                "id": request_id,
                "status": status,
                "elapsed": elapsed,
                "success": status == 200,
                "error": None if status == 200 else response_json.get("error", {}).get("message", "Unknown error")
            }
    except asyncio.TimeoutError:
        return {
            "id": request_id,
            "status": 0,
            "elapsed": time.time() - start_time,
            "success": False,
            "error": "Timeout"
        }
    except Exception as e:
        return {
            "id": request_id,
            "status": 0,
            "elapsed": time.time() - start_time,
            "success": False,
            "error": str(e)
        }

async def run_load_test():
    """Запускает нагрузочный тест"""
    print(f"\n{'='*60}")
    print(f"НАГРУЗОЧНЫЙ ТЕСТ БАЛАНСЕРА OLLAMALEGION")
    print(f"{'='*60}")
    print(f"Время начала: {datetime.now().strftime('%H:%M:%S')}")
    print(f"URL: {BALANCER_URL}")
    print(f"Модель: {MODEL}")
    print(f"Параллельных запросов: {CONCURRENT_REQUESTS}")
    print(f"Всего запросов: {TOTAL_REQUESTS}")
    print(f"{'='*60}\n")
    
    connector = aiohttp.TCPConnector(limit=100)
    async with aiohttp.ClientSession(connector=connector) as session:
        results = []
        semaphore = asyncio.Semaphore(CONCURRENT_REQUESTS)
        
        async def bounded_request(i):
            async with semaphore:
                return await make_request(session, i)
        
        start_time = time.time()
        
        # Запускаем все запросы
        tasks = [bounded_request(i) for i in range(TOTAL_REQUESTS)]
        results = await asyncio.gather(*tasks)
        
        total_time = time.time() - start_time
    
    # Анализ результатов
    successful = [r for r in results if r["success"]]
    failed = [r for r in results if not r["success"]]
    response_times = [r["elapsed"] for r in successful]
    
    print(f"\n{'='*60}")
    print(f"РЕЗУЛЬТАТЫ ТЕСТА")
    print(f"{'='*60}")
    print(f"Время начала: {datetime.now().strftime('%H:%M:%S')}")
    print(f"Время окончания: {datetime.now().strftime('%H:%M:%S')}")
    print(f"Общее время теста: {total_time:.2f}s")
    print(f"{'-'*60}")
    print(f"Успешных запросов: {len(successful)}/{TOTAL_REQUESTS} ({len(successful)/TOTAL_REQUESTS*100:.1f}%)")
    print(f"Ошибок: {len(failed)}/{TOTAL_REQUESTS} ({len(failed)/TOTAL_REQUESTS*100:.1f}%)")
    print(f"{'-'*60}")
    
    if response_times:
        print(f"Время ответа:")
        print(f"  Минимальное: {min(response_times):.3f}s")
        print(f"  Максимальное: {max(response_times):.3f}s")
        print(f"  Среднее: {sum(response_times)/len(response_times):.3f}s")
        print(f"  Медиана: {sorted(response_times)[len(response_times)//2]:.3f}s")
        print(f"  RPS (запросов/сек): {TOTAL_REQUESTS/total_time:.2f}")
    
    if failed:
        print(f"\n{'-'*60}")
        print(f"ОШИБКИ:")
        for r in failed[:5]:
            print(f"  Запрос #{r['id']}: {r['error']} ({r['elapsed']:.3f}s)")
        if len(failed) > 5:
            print(f"  ... и еще {len(failed)-5} ошибок")
    
    print(f"\n{'='*60}")
    
    # Сохраняем результаты
    timestamp = datetime.now().strftime("%Y%m%d_%H%M%S")
    filename = f"load_test_results_{timestamp}.json"
    with open(filename, "w", encoding="utf-8") as f:
        json.dump({
            "timestamp": timestamp,
            "config": {
                "url": BALANCER_URL,
                "model": MODEL,
                "concurrent": CONCURRENT_REQUESTS,
                "total": TOTAL_REQUESTS
            },
            "summary": {
                "total_time": total_time,
                "successful": len(successful),
                "failed": len(failed),
                "success_rate": len(successful)/TOTAL_REQUESTS*100,
                "rps": TOTAL_REQUESTS/total_time
            },
            "results": results
        }, f, ensure_ascii=False, indent=2)
    
    print(f"Результаты сохранены в: {filename}")
    
    return len(failed) == 0

if __name__ == "__main__":
    result = asyncio.run(run_load_test())
    sys.exit(0 if result else 1)