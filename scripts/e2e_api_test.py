#!/usr/bin/env python3
"""
Полное e2e тестирование API OllamaLegion:
CppWorker (прямой) и Балансер — все endpoint'ы с валидацией содержимого.

Примеры запуска:
    python scripts/e2e_api_test.py
    python scripts/e2e_api_test.py --model llama3.1:8b
    python scripts/e2e_api_test.py --balancer http://localhost:18080 --cppworker http://localhost:18092
    OLLAMALEGION_BALANCER=http://bal:18080 OLLAMALEGION_CPPWORKER=http://wkr:18092 \\
        OLLAMALEGION_MODEL=llama3.1:8b python scripts/e2e_api_test.py
"""
import argparse
import json
import os
import sys
import time

import requests


# ============================================================
# Аргументы / ENV
# ============================================================

def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description="E2E тест API OllamaLegion")
    p.add_argument(
        "--balancer",
        default=os.environ.get("OLLAMALEGION_BALANCER", "http://localhost:18080"),
        help="URL балансировщика (env: OLLAMALEGION_BALANCER)",
    )
    p.add_argument(
        "--cppworker",
        default=os.environ.get("OLLAMALEGION_CPPWORKER", "http://localhost:18092"),
        help="URL CppWorker напрямую (env: OLLAMALEGION_CPPWORKER)",
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
    return p.parse_args()


ARGS = None  # заполняется в main()
CPP = ""
BAL = ""
MODEL = ""

RESULTS: list = []
TOTAL = 0
PASSED = 0
FAILED = 0


# ============================================================
# Утилиты
# ============================================================

def ok(test_id: str, detail: str = "") -> None:
    global PASSED
    PASSED += 1
    RESULTS.append(("PASS", test_id, detail))


def fail(test_id: str, detail: str) -> None:
    global FAILED
    FAILED += 1
    RESULTS.append(("FAIL", test_id, detail))
    print(f"  ❌ {detail}")


def header(text: str) -> None:
    print(f"\n{'='*60}")
    print(f"  {text}")
    print(f"{'='*60}")


def summarize() -> bool:
    print(f"\n{'='*60}")
    print(f"  ИТОГИ")
    print(f"{'='*60}")
    print(f"  Всего тестов: {TOTAL}")
    print(f"  Пройдено:     {PASSED} ✅")
    print(f"  Провалено:    {FAILED} ❌")
    print(f"{'='*60}")
    if FAILED > 0:
        print(f"\n  Проваленные тесты:")
        for status, tid, detail in RESULTS:
            if status == "FAIL":
                print(f"    ❌ {tid}: {detail}")
    return FAILED == 0


# ============================================================
# Тестовые функции
# ============================================================

def test_get(url: str, test_id: str, expected_status: int = 200,
             expected_keys=None, expected_ct: str | None = None,
             timeout: int | None = None) -> dict | None:
    """GET запрос с валидацией ответа."""
    global TOTAL
    TOTAL += 1
    try:
        r = requests.get(url, timeout=timeout or ARGS.timeout)
        status = r.status_code
        ct = r.headers.get("Content-Type", "")

        if status != expected_status:
            fail(test_id, f"GET {url} → статус {status} (ожидался {expected_status})")
            return None

        if expected_ct and expected_ct not in ct:
            fail(test_id, f"Content-Type: {ct} (ожидался {expected_ct})")
            return None

        try:
            data = r.json()
        except Exception:
            fail(test_id, f"Ответ не JSON: {r.text[:200]}")
            return None

        if expected_keys:
            missing = [k for k in expected_keys if k not in data]
            if missing:
                fail(test_id, f"Отсутствуют ключи: {missing}")
                return None

        ok(test_id)
        return data
    except Exception as e:
        fail(test_id, f"Исключение: {e}")
        return None


def test_post(url: str, test_id: str, payload: dict,
              expected_status: int = 200, stream: bool = False,
              expected_ct: str | None = None, validate_fn=None,
              timeout: int | None = None):
    """POST запрос с валидацией ответа."""
    global TOTAL
    TOTAL += 1
    try:
        r = requests.post(url, json=payload, timeout=timeout or ARGS.timeout, stream=stream)

        status = r.status_code
        ct = r.headers.get("Content-Type", "")

        if status != expected_status:
            fail(test_id, f"POST {url} → статус {status} (ожидался {expected_status})")
            return None

        if expected_ct and expected_ct not in ct:
            fail(test_id, f"Content-Type: {ct} (ожидался {expected_ct})")
            return None

        if stream:
            lines = [l for l in r.iter_lines(decode_unicode=True) if l and l.strip()]
            if validate_fn:
                result = validate_fn(lines, test_id)
            else:
                result = validate_stream_default(lines, test_id)
        else:
            try:
                data = r.json()
            except Exception:
                fail(test_id, f"Ответ не JSON: {r.text[:200]}")
                return None
            if validate_fn:
                result = validate_fn(data, test_id)
            else:
                result = data

        ok(test_id)
        return result
    except Exception as e:
        fail(test_id, f"Исключение: {e}")
        return None


# ============================================================
# Валидаторы
# ============================================================

def validate_generate_stream(lines, test_id):
    """Проверка streaming /api/generate."""
    if len(lines) == 0:
        fail(test_id, "Пустой стрим (0 строк)")
        return None
    for i, line in enumerate(lines):
        try:
            json.loads(line)
        except json.JSONDecodeError:
            fail(test_id, f"Строка {i} не JSON: {line[:100]}")
            return None
    first = json.loads(lines[0])
    last = json.loads(lines[-1])
    if "model" not in first:
        fail(test_id, "Первый чанк не содержит 'model'")
        return None
    if first["model"] != MODEL:
        fail(test_id, f"model={first['model']} (ожидался {MODEL})")
        return None
    if "response" not in first:
        fail(test_id, "Первый чанк не содержит 'response'")
        return None
    if last.get("done") is not True:
        fail(test_id, "Последний чанк: done != true")
        return None
    full_response = "".join(json.loads(l).get("response", "") for l in lines)
    if not full_response.strip():
        fail(test_id, "Полный response пуст")
        return None
    return {"lines": len(lines), "response_len": len(full_response),
            "model": first["model"], "done": last["done"]}


def validate_generate_nostream(data, test_id):
    """Проверка non-streaming /api/generate."""
    if "model" not in data:
        fail(test_id, "Отсутствует 'model'")
        return None
    if data["model"] != MODEL:
        fail(test_id, f"model={data.get('model')} (ожидался {MODEL})")
        return None
    if "response" not in data:
        fail(test_id, "Отсутствует 'response'")
        return None
    if data.get("done") is not True:
        fail(test_id, "done != true")
        return None
    return {"model": data["model"],
            "response_len": len(data.get("response", "")),
            "done": data["done"]}


def validate_chat_stream(lines, test_id):
    """Проверка streaming /api/chat."""
    if len(lines) == 0:
        fail(test_id, "Пустой стрим (0 строк)")
        return None
    for i, line in enumerate(lines):
        try:
            json.loads(line)
        except json.JSONDecodeError:
            fail(test_id, f"Строка {i} не JSON: {line[:100]}")
            return None
    last = json.loads(lines[-1])
    first_nonempty = None
    full_content = ""
    for l in lines:
        c = json.loads(l)
        msg = c.get("message", {})
        content = msg.get("content", "")
        full_content += content
        if content and first_nonempty is None:
            first_nonempty = c
    if first_nonempty is None:
        fail(test_id, "Все чанки имеют пустой content")
        return None
    if "model" not in first_nonempty:
        fail(test_id, "Чанки не содержат 'model'")
        return None
    if last.get("done") is not True:
        fail(test_id, "Последний чанк: done != true")
        return None
    role = first_nonempty.get("message", {}).get("role", "")
    if role != "assistant":
        fail(test_id, f"message.role={role} (ожидался assistant)")
        return None
    return {"lines": len(lines), "content_len": len(full_content), "role": role}


def validate_chat_nostream(data, test_id):
    """Проверка non-streaming /api/chat."""
    if "model" not in data:
        fail(test_id, "Отсутствует 'model'")
        return None
    msg = data.get("message", {})
    if not msg:
        fail(test_id, "Отсутствует 'message'")
        return None
    if msg.get("role") != "assistant":
        fail(test_id, f"message.role={msg.get('role')} (ожидался assistant)")
        return None
    return {"model": data["model"],
            "content_len": len(msg["content"]),
            "role": msg["role"]}


def validate_openai_chat_stream(lines, test_id):
    """Проверка SSE streaming /v1/chat/completions."""
    if len(lines) == 0:
        fail(test_id, "Пустой SSE стрим (0 строк)")
        return None
    if not lines[-1].endswith("[DONE]"):
        fail(test_id, f"Последняя строка не [DONE]: {lines[-1][:50]}")
        return None
    data_lines = [l for l in lines if l.startswith("data:") and l != "data: [DONE]"]
    if not data_lines:
        fail(test_id, "Нет строк с data: в SSE")
        return None
    first = json.loads(data_lines[0][6:].strip())
    if "choices" not in first:
        fail(test_id, "Нет 'choices' в SSE чанке")
        return None
    full_text = ""
    for l in data_lines:
        try:
            chunk = json.loads(l[6:].strip())
            delta = chunk.get("choices", [{}])[0].get("delta", {})
            full_text += delta.get("content", "")
        except Exception:
            pass
    if not full_text.strip():
        fail(test_id, "Полный текст в SSE пуст")
        return None
    return {"lines": len(lines), "text_len": len(full_text)}


def validate_openai_chat_nostream(data, test_id):
    """Проверка non-streaming /v1/chat/completions."""
    if "choices" not in data:
        fail(test_id, "Нет 'choices'")
        return None
    choice = data["choices"][0]
    msg = choice.get("message", {})
    if msg.get("role") != "assistant":
        fail(test_id, f"role={msg.get('role')}")
        return None
    if not msg.get("content", "").strip():
        fail(test_id, "content пуст")
        return None
    if "model" not in data:
        fail(test_id, "Нет 'model'")
        return None
    return {"model": data["model"], "content_len": len(msg["content"])}


def validate_completions_stream(lines, test_id):
    """Проверка SSE streaming /v1/completions."""
    if len(lines) == 0:
        fail(test_id, "Пустой SSE стрим")
        return None
    if not lines[-1].endswith("[DONE]"):
        fail(test_id, f"Нет [DONE], последняя строка: {lines[-1][:50]}")
        return None
    data_lines = [l for l in lines if l.startswith("data:") and l != "data: [DONE]"]
    if not data_lines:
        fail(test_id, "Нет data: строк")
        return None
    full_text = ""
    for l in data_lines:
        try:
            chunk = json.loads(l[6:].strip())
            full_text += chunk.get("choices", [{}])[0].get("text", "")
        except Exception:
            pass
    if not full_text.replace("\n", "").strip():
        fail(test_id, "Полный текст пуст (только whitespace)")
        return None
    return {"lines": len(lines), "text_len": len(full_text)}


def validate_completions_nostream(data, test_id):
    """Проверка non-streaming /v1/completions."""
    if "choices" not in data:
        fail(test_id, "Нет 'choices'")
        return None
    choice = data["choices"][0]
    if not choice.get("text", "").strip():
        fail(test_id, "text пуст")
        return None
    if "model" not in data:
        fail(test_id, "Нет 'model'")
        return None
    return {"model": data["model"], "text_len": len(choice["text"])}


def validate_embeddings(data, test_id):
    """Проверка эмбеддингов."""
    if "embedding" in data:
        emb = data["embedding"]
    elif "data" in data and len(data["data"]) > 0:
        emb = data["data"][0].get("embedding", [])
    elif "embeddings" in data:
        emb = data["embeddings"]
    else:
        fail(test_id, "Нет поля embedding/data/embeddings")
        return None
    if not emb or len(emb) == 0:
        fail(test_id, "Вектор эмбеддинга пуст")
        return None
    return {"dimensions": len(emb)}


def validate_stream_default(lines, test_id):
    """Базовая проверка streaming-ответа."""
    if len(lines) == 0:
        fail(test_id, "Пустой стрим")
        return None
    return {"lines": len(lines)}


# ============================================================
# Главный тестовый блок
# ============================================================

def main() -> int:
    global ARGS, CPP, BAL, MODEL
    ARGS = parse_args()
    CPP = ARGS.cppworker.rstrip("/")
    BAL = ARGS.balancer.rstrip("/")
    MODEL = ARGS.model

    print(f"Модель:    {MODEL}")
    print(f"CppWorker: {CPP}")
    print(f"Балансер:  {BAL}")
    print(f"Таймаут:   {ARGS.timeout}s")

    # =====================================================================
    # 1. HEALTH CHECKS
    # =====================================================================
    header("1. HEALTH CHECKS")
    test_get(f"{CPP}/health", "01-health-cpp", expected_keys=["status"])
    test_get(f"{CPP}/health", "02-health-cpp-version", expected_keys=["status", "version"])

    # =====================================================================
    # 2. /api/generate — NDJSON streaming (Ollama)
    # =====================================================================
    header("2. /api/generate — NDJSON")
    test_post(f"{CPP}/api/generate", "03-generate-s-cpp",
              {"model": MODEL, "prompt": "Say hi in one word", "stream": True},
              stream=True, expected_ct="x-ndjson", validate_fn=validate_generate_stream)
    test_post(f"{BAL}/api/generate", "04-generate-s-bal",
              {"model": MODEL, "prompt": "Say hi in one word", "stream": True},
              stream=True, expected_ct="x-ndjson", validate_fn=validate_generate_stream)

    test_post(f"{CPP}/api/generate", "05-generate-s-30-cpp",
              {"model": MODEL, "prompt": "Explain what is GPU in simple terms",
               "stream": True, "max_tokens": 30},
              stream=True, expected_ct="x-ndjson", validate_fn=validate_generate_stream)
    test_post(f"{BAL}/api/generate", "06-generate-s-30-bal",
              {"model": MODEL, "prompt": "Explain what is GPU in simple terms",
               "stream": True, "max_tokens": 30},
              stream=True, expected_ct="x-ndjson", validate_fn=validate_generate_stream)

    test_post(f"{CPP}/api/generate", "07-generate-ns-cpp",
              {"model": MODEL, "prompt": "Hello", "stream": False, "max_tokens": 5},
              validate_fn=validate_generate_nostream)
    test_post(f"{BAL}/api/generate", "08-generate-ns-bal",
              {"model": MODEL, "prompt": "Hello", "stream": False, "max_tokens": 5},
              validate_fn=validate_generate_nostream)

    test_post(f"{BAL}/api/generate", "09-generate-ns-t0-bal",
              {"model": MODEL, "prompt": "Hello", "stream": False,
               "max_tokens": 5, "temperature": 0},
              validate_fn=validate_generate_nostream)

    # =====================================================================
    # 3. /api/chat — NDJSON (Ollama)
    # =====================================================================
    header("3. /api/chat — NDJSON")
    test_post(f"{CPP}/api/chat", "10-chat-s-cpp",
              {"model": MODEL,
               "messages": [{"role": "user", "content": "Say hi"}], "stream": True},
              stream=True, expected_ct="x-ndjson", validate_fn=validate_chat_stream)
    test_post(f"{BAL}/api/chat", "11-chat-s-bal",
              {"model": MODEL,
               "messages": [{"role": "user", "content": "Say hi"}], "stream": True},
              stream=True, expected_ct="x-ndjson", validate_fn=validate_chat_stream)

    test_post(f"{CPP}/api/chat", "12-chat-ns-cpp",
              {"model": MODEL,
               "messages": [{"role": "user", "content": "Hello"}],
               "stream": False, "max_tokens": 10},
              validate_fn=validate_chat_nostream)
    test_post(f"{BAL}/api/chat", "13-chat-ns-bal",
              {"model": MODEL,
               "messages": [{"role": "user", "content": "Hello"}],
               "stream": False, "max_tokens": 10},
              validate_fn=validate_chat_nostream)

    # =====================================================================
    # 4. /v1/chat/completions — SSE (OpenAI)
    # =====================================================================
    header("4. /v1/chat/completions — SSE (OpenAI)")
    test_post(f"{CPP}/v1/chat/completions", "14-oai-chat-s-cpp",
              {"model": MODEL,
               "messages": [{"role": "user", "content": "Hi"}],
               "stream": True, "max_tokens": 20},
              stream=True, expected_ct="event-stream", validate_fn=validate_openai_chat_stream)
    test_post(f"{BAL}/v1/chat/completions", "15-oai-chat-s-bal",
              {"model": MODEL,
               "messages": [{"role": "user", "content": "Hi"}],
               "stream": True, "max_tokens": 20},
              stream=True, expected_ct="event-stream", validate_fn=validate_openai_chat_stream)

    test_post(f"{CPP}/v1/chat/completions", "16-oai-chat-ns-cpp",
              {"model": MODEL,
               "messages": [{"role": "user", "content": "Hi"}],
               "stream": False, "max_tokens": 10},
              validate_fn=validate_openai_chat_nostream)
    test_post(f"{BAL}/v1/chat/completions", "17-oai-chat-ns-bal",
              {"model": MODEL,
               "messages": [{"role": "user", "content": "Hi"}],
               "stream": False, "max_tokens": 10},
              validate_fn=validate_openai_chat_nostream)

    # =====================================================================
    # 5. /v1/completions — SSE (OpenAI)
    # =====================================================================
    header("5. /v1/completions — SSE (OpenAI)")
    test_post(f"{CPP}/v1/completions", "18-oai-comp-s-cpp",
              {"model": MODEL, "prompt": "Hello",
               "stream": True, "max_tokens": 20},
              stream=True, expected_ct="event-stream", validate_fn=validate_completions_stream)
    test_post(f"{BAL}/v1/completions", "19-oai-comp-s-bal",
              {"model": MODEL, "prompt": "Hello",
               "stream": True, "max_tokens": 20},
              stream=True, expected_ct="event-stream", validate_fn=validate_completions_stream)

    test_post(f"{CPP}/v1/completions", "20-oai-comp-ns-cpp",
              {"model": MODEL, "prompt": "Hello",
               "stream": False, "max_tokens": 10},
              validate_fn=validate_completions_nostream)
    test_post(f"{BAL}/v1/completions", "21-oai-comp-ns-bal",
              {"model": MODEL, "prompt": "Hello",
               "stream": False, "max_tokens": 10},
              validate_fn=validate_completions_nostream)

    # =====================================================================
    # 6. Embeddings
    # =====================================================================
    header("6. Embeddings")
    test_post(f"{CPP}/api/embeddings", "22-emb-cpp",
              {"model": MODEL, "prompt": "hello world"},
              validate_fn=validate_embeddings)
    test_post(f"{BAL}/api/embeddings", "23-emb-bal",
              {"model": MODEL, "prompt": "hello world"},
              validate_fn=validate_embeddings)
    test_post(f"{CPP}/v1/embeddings", "24-emb-oai-cpp",
              {"model": MODEL, "input": "hello world"},
              validate_fn=validate_embeddings)
    test_post(f"{BAL}/v1/embeddings", "25-emb-oai-bal",
              {"model": MODEL, "input": "hello world"},
              validate_fn=validate_embeddings)

    # =====================================================================
    # 7. Models list
    # =====================================================================
    header("7. Models list")
    test_get(f"{CPP}/api/tags", "26-tags-cpp", expected_keys=["models"])
    test_get(f"{BAL}/api/tags", "27-tags-bal", expected_keys=["models"])
    test_get(f"{CPP}/v1/models", "28-models-cpp", expected_keys=["data"])
    test_get(f"{BAL}/v1/models", "29-models-bal", expected_keys=["data"])

    # =====================================================================
    # 8. Legacy Ollama
    # =====================================================================
    header("8. Legacy Ollama endpoints")
    test_post(f"{CPP}/api/ollama/generate", "30-legacy-cpp",
              {"model": MODEL, "prompt": "Hi", "stream": False},
              validate_fn=validate_generate_nostream)

    # =====================================================================
    # 9. Edge cases
    # =====================================================================
    header("9. Edge cases")
    test_post(f"{BAL}/api/generate", "31-edge-tokens1",
              {"model": MODEL, "prompt": "Hi", "stream": False, "max_tokens": 1},
              validate_fn=validate_generate_nostream)
    test_post(f"{BAL}/api/generate", "32-edge-long",
              {"model": MODEL, "prompt": "List 5 fruits",
               "stream": False, "max_tokens": 50},
              validate_fn=validate_generate_nostream)

    success = summarize()
    return 0 if success else 1


if __name__ == "__main__":
    sys.exit(main())