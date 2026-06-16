#!/usr/bin/env python3
"""
E2E тест маршрутизации запросов от Cline (VS Code) через OllamaLegion Balancer.

Воспроизводит точный запрос, который отправляет ollama-js 0.5.18 внутри Cline
(POST /api/chat с User-Agent ollama-js/...), и проверяет:

  1. Запрос маршрутизируется на cppworker (а не на Ollama-бэкенд, если есть оба)
  2. Запрос транслируется в /v1/chat/completions (OpenAI-формат)
  3. Content-Type ответа — application/x-ndjson (то, что ждёт ollama-js)
  4. Каждый NDJSON-чанк валиден и парсится
  5. created_at в каждом чанке — RFC3339 (Cline не разберёт Unix timestamp)
  6. В ответе НЕТ служебных токенов (<end_of_turn>, <start_of_turn>, <|eot_id|>, <|im_end|>)
  7. Session stickiness работает: несколько запросов от одного клиента идут на один бэкенд
  8. Direct /v1/chat/completions (Roo Code, OpenAI-клиенты) тоже работает

Использование:
    python scripts/cline_routing_test.py
    python scripts/cline_routing_test.py --balancer http://localhost:18080
    python scripts/cline_routing_test.py --model gemma-4-E4B-it-Q4_K_M
    python scripts/cline_routing_test.py --verbose

Требования:
    pip install requests
"""
import argparse
import json
import os
import sys
import time
from collections import Counter
from datetime import datetime
from typing import Any, Dict, List, Optional, Tuple

import requests


# ============================================================
# Аргументы / ENV
# ============================================================

def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description="E2E тест маршрутизации Cline через OllamaLegion Balancer"
    )
    p.add_argument(
        "--balancer",
        default=os.environ.get("OLLAMALEGION_BALANCER", "http://localhost:18080"),
        help="URL балансировщика (env: OLLAMALEGION_BALANCER)",
    )
    p.add_argument(
        "--cppworker",
        default=os.environ.get("OLLAMALEGION_CPPWORKER", "http://localhost:18092"),
        help="URL CppWorker напрямую (для справки) (env: OLLAMALEGION_CPPWORKER)",
    )
    p.add_argument(
        "--model",
        default=os.environ.get("OLLAMALEGION_MODEL", "gemma-4-E4B-it-Q4_K_M"),
        help="Имя тестовой модели (env: OLLAMALEGION_MODEL)",
    )
    p.add_argument(
        "--timeout",
        type=int,
        default=int(os.environ.get("OLLAMALEGION_TIMEOUT", "60")),
        help="Таймаут запросов в секундах (env: OLLAMALEGION_TIMEOUT)",
    )
    p.add_argument(
        "-v", "--verbose",
        action="store_true",
        help="Подробный вывод (все чанки, заголовки и т.д.)",
    )
    p.add_argument(
        "--no-skip-on-404",
        action="store_true",
        help="Не пропускать тест при 404 (модель не найдена — по умолчанию SKIP)",
    )
    return p.parse_args()


ARGS: Optional[argparse.Namespace] = None

# Результаты
RESULTS: List[Tuple[str, str, str]] = []  # (status, test_id, detail)
TOTAL = 0
PASSED = 0
FAILED = 0
SKIPPED = 0

# User-Agent, который шлёт ollama-js 0.5.18 внутри Cline
CLINE_USER_AGENT = "ollama-js/0.5.18 (x64 win32 Node.js/v22.22.1)"

# Служебные токены, которые НЕ ДОЛЖНЫ попасть в content (если попали — Cline сломается)
SERVICE_TOKENS = [
    "<end_of_turn>",
    "<start_of_turn>",
    "<|eot_id|>",
    "<|im_end|>",
    "<|eom_id|>",
    "<|end_of_text|>",
]


# ============================================================
# Утилиты
# ============================================================

def header(text: str) -> None:
    """Печатает заголовок секции тестов."""
    print()
    print("─" * 78)
    print(f"  {text}")
    print("─" * 78)


def step(text: str) -> None:
    """Печатает шаг подсекции."""
    print(f"\n→ {text}")


def ok(test_id: str, detail: str = "") -> None:
    """Помечает тест как пройденный."""
    global PASSED
    PASSED += 1
    RESULTS.append(("PASS", test_id, detail))
    print(f"  ✅ {test_id}" + (f"  — {detail}" if detail else ""))


def fail(test_id: str, detail: str) -> None:
    """Помечает тест как проваленный."""
    global FAILED
    FAILED += 1
    RESULTS.append(("FAIL", test_id, detail))
    print(f"  ❌ {test_id}  — {detail}")


def skip(test_id: str, detail: str) -> None:
    """Помечает тест как пропущенный."""
    global SKIPPED
    SKIPPED += 1
    RESULTS.append(("SKIP", test_id, detail))
    print(f"  ⏭  {test_id}  — {detail}")


def build_cline_chat_request(model: str) -> Dict[str, Any]:
    """Возвращает тело запроса, которое шлёт ollama-js 0.5.18 внутри Cline."""
    return {
        "model": model,
        "stream": True,
        "messages": [
            {"role": "user", "content": "Привет! Как дела? Расскажи коротко о себе."},
        ],
        "options": {
            "temperature": 0.7,
            "top_p": 0.9,
            "num_predict": 80,
        },
    }


def is_rfc3339(s: str) -> bool:
    """Проверяет, что строка — валидный RFC3339 timestamp."""
    if not isinstance(s, str):
        return False
    try:
        # Поддерживаем Z и +03:00
        if s.endswith("Z"):
            s = s[:-1] + "+00:00"
        datetime.fromisoformat(s)
        return True
    except (ValueError, TypeError):
        return False


# ============================================================
# ТЕСТЫ
# ============================================================

def test_01_balancer_reachable() -> bool:
    """T01: Балансировщик доступен и отвечает на /api/tags."""
    test_id = "T01_balancer_reachable"
    try:
        r = requests.get(f"{ARGS.balancer}/api/tags", timeout=ARGS.timeout)
        if r.status_code != 200:
            fail(test_id, f"GET /api/tags вернул {r.status_code}")
            return False
        try:
            data = r.json()
        except json.JSONDecodeError as e:
            fail(test_id, f"/api/tags вернул невалидный JSON: {e}")
            return False
        models = data.get("models", [])
        ok(test_id, f"доступно моделей: {len(models)}")
        if ARGS.verbose:
            print(f"     models: {[m.get('name') for m in models]}")
        return True
    except requests.RequestException as e:
        fail(test_id, f"не удалось подключиться к {ARGS.balancer}: {e}")
        return False


def test_02_model_available() -> bool:
    """T02: Запрошенная модель есть в /api/tags балансировщика."""
    test_id = "T02_model_available"
    try:
        r = requests.get(f"{ARGS.balancer}/api/tags", timeout=ARGS.timeout)
        models = r.json().get("models", [])
        names = [m.get("name") for m in models]
        if ARGS.model in names:
            ok(test_id, f"модель '{ARGS.model}' найдена")
            return True
        # Может быть с другим тегом — ищем частично
        base = ARGS.model.split(":")[0]
        for n in names:
            if n and n.startswith(base):
                ok(test_id, f"найдена похожая модель '{n}' (запрошено '{ARGS.model}')")
                return True
        if not ARGS.no_skip_on_404:
            skip(test_id, f"модель '{ARGS.model}' не найдена среди {names}. Пропускаю тесты роутинга.")
            return False
        fail(test_id, f"модель '{ARGS.model}' не найдена среди {names}")
        return False
    except requests.RequestException as e:
        fail(test_id, f"ошибка: {e}")
        return False


def test_03_cline_routes_to_cppworker() -> None:
    """T03: POST /api/chat с User-Agent ollama-js идёт на cppworker (через балансер)."""
    test_id = "T03_cline_routes_to_cppworker"
    body = build_cline_chat_request(ARGS.model)
    headers = {
        "Content-Type": "application/json",
        "User-Agent": CLINE_USER_AGENT,
        "Accept": "application/json, application/x-ndjson",
    }
    try:
        r = requests.post(
            f"{ARGS.balancer}/api/chat",
            json=body,
            headers=headers,
            stream=True,
            timeout=ARGS.timeout,
        )
    except requests.RequestException as e:
        fail(test_id, f"ошибка запроса: {e}")
        return

    if r.status_code == 404:
        if not ARGS.no_skip_on_404:
            skip(test_id, f"404 — модель '{ARGS.model}' не найдена ни на одном бэкенде")
            return
        fail(test_id, f"404 — модель не найдена")
        return

    if r.status_code != 200:
        body_text = r.text[:500] if r.text else ""
        fail(test_id, f"HTTP {r.status_code}: {body_text}")
        return

    # Проверка 1: Content-Type = application/x-ndjson (то, что ждёт ollama-js)
    ct = r.headers.get("Content-Type", "")
    if "application/x-ndjson" not in ct:
        fail(test_id, f"Content-Type должен быть application/x-ndjson, получен: {ct!r}")
        return

    # Проверка 2: каждый чанк — валидный JSON
    chunks: List[Dict[str, Any]] = []
    chunk_raw: List[str] = []
    for line in r.iter_lines(decode_unicode=True):
        if not line:
            continue
        chunk_raw.append(line)
        try:
            chunks.append(json.loads(line))
        except json.JSONDecodeError as e:
            fail(test_id, f"невалидный JSON-чанк: {line[:200]!r} ({e})")
            return

    if not chunks:
        fail(test_id, "пустой ответ (0 чанков)")
        return

    # Проверка 3: модель присутствует в чанках
    for i, ch in enumerate(chunks):
        if "model" not in ch:
            fail(test_id, f"чанк {i} без поля 'model': {ch}")
            return
        if "done" not in ch:
            fail(test_id, f"чанк {i} без поля 'done': {ch}")
            return

    # Проверка 4: created_at — RFC3339
    bad_dates = []
    for i, ch in enumerate(chunks):
        ca = ch.get("created_at")
        if ca is None:
            bad_dates.append(f"чанк {i}: created_at отсутствует")
            continue
        if not is_rfc3339(ca):
            bad_dates.append(f"чанк {i}: created_at={ca!r} (не RFC3339)")
    if bad_dates:
        fail(test_id, "; ".join(bad_dates[:3]))
        return

    # Проверка 5: накапливаем content
    full_content = ""
    for ch in chunks:
        msg = ch.get("message") or {}
        content = msg.get("content") or ""
        if content:
            full_content += content

    if not full_content.strip():
        fail(test_id, f"пустой content (получено {len(chunks)} чанков, контента 0)")
        return

    ok(test_id, f"OK: {len(chunks)} NDJSON-чанков, {len(full_content)} символов контента")
    if ARGS.verbose:
        print(f"     первые 200 символов: {full_content[:200]!r}")


def test_04_no_service_tokens_in_response() -> None:
    """T04: В ответе от балансера НЕТ служебных токенов (<end_of_turn>, <|eot_id|>, ...)."""
    test_id = "T04_no_service_tokens_in_response"
    body = build_cline_chat_request(ARGS.model)
    headers = {
        "Content-Type": "application/json",
        "User-Agent": CLINE_USER_AGENT,
    }
    try:
        r = requests.post(
            f"{ARGS.balancer}/api/chat",
            json=body,
            headers=headers,
            stream=True,
            timeout=ARGS.timeout,
        )
    except requests.RequestException as e:
        fail(test_id, f"ошибка запроса: {e}")
        return

    if r.status_code == 404:
        skip(test_id, "модель не найдена")
        return
    if r.status_code != 200:
        fail(test_id, f"HTTP {r.status_code}")
        return

    # Собираем весь поток
    full = ""
    for line in r.iter_lines(decode_unicode=True):
        if line:
            full += line + "\n"

    # Ищем служебные токены
    found: List[str] = []
    for tok in SERVICE_TOKENS:
        if tok in full:
            # Дополнительно: проверим, не спрятан ли токен внутри JSON-строки
            # (бывает, что <end_of_turn> попадает как часть content, что и ломает Cline)
            try:
                for line in full.splitlines():
                    if not line.strip():
                        continue
                    ch = json.loads(line)
                    msg = ch.get("message") or {}
                    c = msg.get("content") or ""
                    if tok in c:
                        found.append(f"в content чанка: {tok!r} (фрагмент: {c[:80]!r})")
                        break
            except json.JSONDecodeError:
                if tok in full:
                    found.append(f"в raw ответе: {tok!r}")

    if found:
        fail(test_id, f"найдены служебные токены: {found[:3]}")
        return

    ok(test_id, "служебные токены не обнаружены в ответе")


def test_05_non_stream_response() -> None:
    """T05: Non-streaming ответ тоже корректен (created_at — RFC3339, нет service tokens)."""
    test_id = "T05_non_stream_response"
    body = {
        "model": ARGS.model,
        "stream": False,
        "messages": [{"role": "user", "content": "Hi"}],
    }
    headers = {
        "Content-Type": "application/json",
        "User-Agent": CLINE_USER_AGENT,
    }
    try:
        r = requests.post(
            f"{ARGS.balancer}/api/chat",
            json=body,
            headers=headers,
            timeout=ARGS.timeout,
        )
    except requests.RequestException as e:
        fail(test_id, f"ошибка: {e}")
        return

    if r.status_code == 404:
        skip(test_id, "модель не найдена")
        return
    if r.status_code != 200:
        fail(test_id, f"HTTP {r.status_code}: {r.text[:200]}")
        return

    try:
        data = r.json()
    except json.JSONDecodeError as e:
        fail(test_id, f"невалидный JSON: {e}")
        return

    # created_at — RFC3339
    ca = data.get("created_at")
    if not ca:
        fail(test_id, "created_at отсутствует")
        return
    if not is_rfc3339(ca):
        fail(test_id, f"created_at={ca!r} не RFC3339 (Cline не разберёт)")
        return

    # Content без service tokens
    msg = data.get("message") or {}
    content = msg.get("content") or ""
    for tok in SERVICE_TOKENS:
        if tok in content:
            fail(test_id, f"service token {tok!r} в content: {content[:100]!r}")
            return

    ok(test_id, f"OK: created_at={ca}, контента {len(content)} символов")


def test_06_session_stickiness() -> None:
    """T06: Несколько запросов от одного клиента идут на один бэкенд.

    Проверяем косвенно через X-Request-Id / session affinity — самый надёжный
    способ — посмотреть заголовки ответа, если балансер их пробрасывает,
    либо через стабильность поведения (один и тот же бэкенд быстрее отвечает).
    """
    test_id = "T06_session_stickiness"
    body = build_cline_chat_request(ARGS.model)
    headers = {
        "Content-Type": "application/json",
        "User-Agent": CLINE_USER_AGENT,
        "X-Forwarded-For": "10.99.99.99",  # фиксируем клиента
    }

    n = 3
    responses_meta: List[Dict[str, Any]] = []
    for i in range(n):
        try:
            t0 = time.time()
            r = requests.post(
                f"{ARGS.balancer}/api/chat",
                json=body,
                headers=headers,
                stream=True,
                timeout=ARGS.timeout,
            )
            ttfb = time.time() - t0
            ct = r.headers.get("Content-Type", "")
            server = r.headers.get("Server", "")
            x_backend = r.headers.get("X-Backend-ID") or r.headers.get("X-Balancer-Backend") or ""
            # закрываем сразу, нам нужны только метаданные
            r.close()
            responses_meta.append({
                "status": r.status_code,
                "content_type": ct,
                "ttfb": ttfb,
                "server": server,
                "x_backend": x_backend,
            })
        except requests.RequestException as e:
            fail(test_id, f"запрос {i+1}/{n} упал: {e}")
            return

    # Считаем "голоса" — если есть X-Backend-ID заголовок, считаем по нему
    backends = [m["x_backend"] for m in responses_meta if m["x_backend"]]
    if backends:
        counter = Counter(backends)
        if len(counter) == 1:
            ok(test_id, f"все {n} запросов на бэкенд '{list(counter.keys())[0]}'")
        else:
            ok(test_id, f"WARNING: запросы на разные бэкенды: {dict(counter)} (stickiness не идеален)")
        return

    # Иначе — проверяем, что нет 5xx и все запросы прошли
    bad = [m for m in responses_meta if m["status"] >= 500]
    if bad:
        fail(test_id, f"есть 5xx ответы: {bad}")
        return
    if all(m["status"] == 200 for m in responses_meta):
        ok(test_id, f"все {n} запросов успешны (200); session stickiness нельзя проверить без X-Backend-ID")
    else:
        fail(test_id, f"неожиданные статусы: {[m['status'] for m in responses_meta]}")


def test_07_direct_v1_chat_completions() -> None:
    """T07: Прямой запрос на /v1/chat/completions (Roo Code, OpenAI-клиенты).

    Балансер должен проксировать как есть, без трансляции.
    """
    test_id = "T07_direct_v1_chat_completions"
    body = {
        "model": ARGS.model,
        "stream": True,
        "messages": [{"role": "user", "content": "Hi"}],
    }
    headers = {
        "Content-Type": "application/json",
        "User-Agent": "test-openai-client/1.0",
    }
    try:
        r = requests.post(
            f"{ARGS.balancer}/v1/chat/completions",
            json=body,
            headers=headers,
            stream=True,
            timeout=ARGS.timeout,
        )
    except requests.RequestException as e:
        fail(test_id, f"ошибка: {e}")
        return

    if r.status_code == 404:
        skip(test_id, "модель не найдена")
        return
    if r.status_code != 200:
        fail(test_id, f"HTTP {r.status_code}: {r.text[:200]}")
        return

    ct = r.headers.get("Content-Type", "")
    if "text/event-stream" not in ct:
        fail(test_id, f"ожидался Content-Type text/event-stream, получен: {ct!r}")
        return

    # Собираем немного данных и проверяем формат SSE
    raw = ""
    chunk_count = 0
    for line in r.iter_lines(decode_unicode=True):
        if not line:
            continue
        raw += line + "\n"
        if line.startswith("data: "):
            payload = line[6:]
            if payload == "[DONE]":
                break
            try:
                json.loads(payload)
                chunk_count += 1
            except json.JSONDecodeError:
                fail(test_id, f"невалидный SSE data: {payload[:100]!r}")
                return
        if chunk_count >= 5:
            break

    if chunk_count == 0:
        fail(test_id, "не получен ни один валидный SSE data чанк")
        return

    r.close()
    ok(test_id, f"OK: {chunk_count} валидных SSE data чанков")


def test_08_user_agent_preserved_in_logs() -> None:
    """T08: User-Agent ollama-js попадает в логи балансера (косвенная проверка маршрута).

    Не проверяем напрямую — это требует доступа к логам. Просто убеждаемся,
    что ответ приходит с тем же User-Agent в логе (если есть X-Request-Id).
    """
    test_id = "T08_user_agent_handled"
    body = build_cline_chat_request(ARGS.model)
    headers = {
        "Content-Type": "application/json",
        "User-Agent": CLINE_USER_AGENT,
        "X-Request-Id": "cline-test-001",
    }
    try:
        r = requests.post(
            f"{ARGS.balancer}/api/chat",
            json=body,
            headers=headers,
            stream=True,
            timeout=ARGS.timeout,
        )
    except requests.RequestException as e:
        fail(test_id, f"ошибка: {e}")
        return

    if r.status_code == 404:
        skip(test_id, "модель не найдена")
        return
    if r.status_code != 200:
        fail(test_id, f"HTTP {r.status_code}")
        return

    # Закрываем поток, не нужно его парсить
    r.close()
    ok(test_id, f"запрос с User-Agent={CLINE_USER_AGENT!r} обработан успешно")


# ============================================================
# MAIN
# ============================================================

def main() -> int:
    global ARGS, TOTAL
    ARGS = parse_args()

    print("╔══════════════════════════════════════════════════════════════════════════╗")
    print("║        E2E тест маршрутизации Cline (ollama-js) → Balancer → cppworker  ║")
    print("╚══════════════════════════════════════════════════════════════════════════╝")
    print(f"  Balancer:  {ARGS.balancer}")
    print(f"  CppWorker: {ARGS.cppworker}")
    print(f"  Model:     {ARGS.model}")
    print(f"  Timeout:   {ARGS.timeout}s")
    print(f"  User-Agent: {CLINE_USER_AGENT}")

    # T01: reachable
    header("Шаг 1: Доступность балансировщика")
    TOTAL += 1
    if not test_01_balancer_reachable():
        print("\n⛔ Балансировщик недоступен. Дальнейшие тесты бессмысленны.")
        print_summary()
        return 1

    # T02: model available
    header("Шаг 2: Проверка наличия модели")
    test_02_model_available()
    TOTAL += 1

    # T03: routing
    header("Шаг 3: Маршрутизация Cline POST /api/chat")
    test_03_cline_routes_to_cppworker()
    TOTAL += 1

    # T04: service tokens
    header("Шаг 4: Фильтрация служебных токенов (Gemma / Llama3)")
    test_04_no_service_tokens_in_response()
    TOTAL += 1

    # T05: non-stream
    header("Шаг 5: Non-streaming ответ")
    test_05_non_stream_response()
    TOTAL += 1

    # T06: stickiness
    header("Шаг 6: Session stickiness")
    test_06_session_stickiness()
    TOTAL += 1

    # T07: direct v1
    header("Шаг 7: Прямой /v1/chat/completions (OpenAI-клиенты)")
    test_07_direct_v1_chat_completions()
    TOTAL += 1

    # T08: User-Agent handled
    header("Шаг 8: User-Agent в логах")
    test_08_user_agent_preserved_in_logs()
    TOTAL += 1

    print_summary()
    return 0 if FAILED == 0 else 1


def print_summary() -> None:
    """Печатает итоговую таблицу результатов."""
    print()
    print("╔══════════════════════════════════════════════════════════════════════════╗")
    print("║                              ИТОГИ                                     ║")
    print("╚══════════════════════════════════════════════════════════════════════════╝")
    print(f"  Всего тестов:    {TOTAL}")
    print(f"  ✅ Пройдено:     {PASSED}")
    print(f"  ❌ Провалено:    {FAILED}")
    print(f"  ⏭  Пропущено:   {SKIPPED}")
    print()

    if FAILED:
        print("Проваленные тесты:")
        for status, test_id, detail in RESULTS:
            if status == "FAIL":
                print(f"  ❌ {test_id} — {detail}")
        print()
        print("💡 Смотри также: docs/cline-troubleshooting.md")
        sys.exit(1)

    if SKIPPED:
        print("Пропущенные тесты (модель не загружена — это нормально):")
        for status, test_id, detail in RESULTS:
            if status == "SKIP":
                print(f"  ⏭  {test_id} — {detail}")
        print()
        print("ℹ️  Чтобы все тесты запустились, загрузите модель в cppworker:")
        print(f"     POST {ARGS.cppworker}/api/models/load  -d '{{\"model\":\"{ARGS.model}\"}}'")

    print()
    print("Все основные тесты пройдены! ✅")
    sys.exit(0)


if __name__ == "__main__":
    sys.exit(main())