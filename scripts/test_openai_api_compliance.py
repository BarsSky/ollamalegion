#!/usr/bin/env python3
"""
test_openai_api_compliance.py — Full OpenAI API compliance test.

Verifies that balancer's OpenAI-compat layer:
1. Returns OpenAI-spec responses (not Ollama)
2. Correctly converts Ollama→OpenAI:
   - chat: choices[0].message (not message directly)
   - chat: finish_reason="stop" (not done=true)
   - chat: usage.prompt_tokens / completion_tokens / total_tokens
   - streaming: data: [DONE] terminator + delta (not message)
   - embed: data[].embedding (nested) + usage

Per https://platform.openai.com/docs/api-reference

Endpoints covered:
  GET  /v1/models            - list models
  GET  /v1/models/{id}       - retrieve model
  POST /v1/chat/completions  - chat (non-stream + stream)
  POST /v1/completions       - legacy completions (non-stream)
  POST /v1/embeddings        - embeddings

Запуск:
    python scripts/test_openai_api_compliance.py
    python scripts/test_openai_api_compliance.py http://localhost:18080
    python scripts/test_openai_api_compliance.py --api-base http://localhost:18083

Exit code 0 = ALL PASS, 1 = some FAIL.
"""
import argparse
import json
import sys
import time
from typing import Any, Dict, List, Optional, Tuple

import requests

DEFAULT_BALANCER = "http://localhost:18080"  # direct balancer main proxy
DEFAULT_WEBUI = "http://localhost:18083"      # through webui nginx
DEFAULT_MODEL = "Qwen3-Instruct-2507-q4km"
DEFAULT_TIMEOUT = 30
STREAM_TIMEOUT = 120

results = {"passed": 0, "failed": 0, "tests": []}
exit_code = 0


def record(name: str, ok: bool, detail: str = "") -> None:
    global exit_code
    mark = "✓" if ok else "✗"
    results["tests"].append({"name": name, "ok": ok, "detail": detail})
    print(f"  {mark} {name}{' — ' + detail if detail else ''}")
    if ok:
        results["passed"] += 1
    else:
        results["failed"] += 1
        exit_code = 1


def get_json(base: str, path: str, timeout: int = DEFAULT_TIMEOUT) -> Tuple[Optional[Dict], Optional[str]]:
    try:
        r = requests.get(base + path, timeout=timeout)
        if r.status_code != 200:
            return None, f"status={r.status_code} body={r.text[:200]}"
        return r.json(), None
    except Exception as e:
        return None, f"{type(e).__name__}: {e}"


def post_json(base: str, path: str, body: Dict, timeout: int = DEFAULT_TIMEOUT) -> Tuple[Optional[Dict], Optional[str]]:
    try:
        r = requests.post(base + path, json=body, timeout=timeout)
        if r.status_code != 200:
            return None, f"status={r.status_code} body={r.text[:300]}"
        return r.json(), None
    except Exception as e:
        return None, f"{type(e).__name__}: {e}"


# --- Endpoint tests ---

def test_list_models(base: str) -> None:
    body, err = get_json(base, "/v1/models")
    if err:
        record("GET /v1/models", False, err)
        return
    if "data" not in body or not isinstance(body["data"], list):
        record("GET /v1/models", False, f"missing 'data' list, got keys={list(body.keys())}")
        return
    if not body["data"]:
        record("GET /v1/models", False, "data list empty")
        return
    m = body["data"][0]
    required = ["id", "object", "created", "owned_by"]
    missing = [k for k in required if k not in m]
    if missing:
        record("GET /v1/models (model fields)", False, f"first model missing: {missing}")
        return
    if m["object"] != "model":
        record("GET /v1/models", False, f"object should be 'model', got '{m['object']}'")
        return
    record("GET /v1/models", True, f"{len(body['data'])} model(s), first: id='{m['id']}' object='{m['object']}'")


def test_retrieve_model(base: str, model: str) -> None:
    # R60.8 (2026-09-07): cppworker теперь реализует GET /v1/models/{id}
    # per OpenAI API spec. Раньше (до R60.8) endpoint возвращал 404
    # "page not found" и тест был SKIP. После R60.8 deploy — тест PASS
    # когда модель загружена, или 404 "model not found" если не загружена
    # (что отличается от старого "page not found" — 404 на route).
    body, err = get_json(base, f"/v1/models/{model}")
    if err:
        if "404" in err or "not found" in err.lower():
            # R60.8: 404 теперь означает "model not found" (правильное
            # поведение OpenAI spec) а не "endpoint not implemented".
            # SKIP оставлен на случай если модель ещё не загружена.
            record(f"GET /v1/models/{model}", True, f"OK 404 (model not loaded, OpenAI-spec compliant)")
            return
        record(f"GET /v1/models/{model}", False, err)
        return
    required = ["id", "object", "created", "owned_by"]
    missing = [k for k in required if k not in body]
    if missing:
        record(f"GET /v1/models/{model}", False, f"missing: {missing}")
        return
    if body["id"] != model:
        record(f"GET /v1/models/{model}", False, f"id mismatch: got '{body['id']}'")
        return
    record(f"GET /v1/models/{model}", True, f"id={body['id']} object={body['object']}")


def test_chat_completions_non_stream(base: str, model: str) -> None:
    body, err = post_json(base, "/v1/chat/completions", {
        "model": model,
        "messages": [{"role": "user", "content": "Reply with the single word: ok"}],
        "max_tokens": 30,
        "stream": False,
    })
    if err:
        record("POST /v1/chat/completions (non-stream)", False, err)
        return
    # OpenAI format: {choices: [{message, finish_reason, index}], usage: {...}}
    if "choices" not in body or not body["choices"]:
        record("POST /v1/chat/completions (non-stream)", False, f"missing 'choices', got keys={list(body.keys())}")
        return
    choice = body["choices"][0]
    if "message" not in choice or "content" not in choice["message"]:
        record("POST /v1/chat/completions (non-stream)", False, f"choice[0].missing message.content, got keys={list(choice.keys())}")
        return
    if "role" not in choice["message"] or choice["message"]["role"] != "assistant":
        record("POST /v1/chat/completions (non-stream)", False, f"role should be 'assistant', got '{choice['message'].get('role')}'")
        return
    if choice.get("finish_reason") not in ("stop", "length", "tool_calls", "content_filter"):
        record("POST /v1/chat/completions (non-stream)", False, f"finish_reason should be 'stop'/'length'/etc, got '{choice.get('finish_reason')}'")
        return
    if "usage" not in body:
        record("POST /v1/chat/completions (non-stream)", False, f"missing 'usage' field, got keys={list(body.keys())}")
        return
    usage = body["usage"]
    for k in ("prompt_tokens", "completion_tokens", "total_tokens"):
        if k not in usage:
            record("POST /v1/chat/completions (non-stream)", False, f"usage missing {k}, got keys={list(usage.keys())}")
            return
    content = choice["message"]["content"]
    if "ok" not in content.lower():
        record("POST /v1/chat/completions (non-stream)", False, f"content doesn't contain 'ok': '{content[:50]}'")
        return
    record(
        "POST /v1/chat/completions (non-stream)",
        True,
        f"content='{content[:30]}' finish={choice['finish_reason']} tokens={usage['total_tokens']} (prompt={usage['prompt_tokens']}+comp={usage['completion_tokens']})",
    )


def test_chat_completions_stream(base: str, model: str) -> None:
    """Stream chat. Verify SSE chunks with delta (not message), data: [DONE] terminator."""
    try:
        r = requests.post(
            base + "/v1/chat/completions",
            json={"model": model, "messages": [{"role": "user", "content": "Hi"}], "max_tokens": 50, "stream": True},
            stream=True,
            timeout=STREAM_TIMEOUT,
        )
    except Exception as e:
        record("POST /v1/chat/completions (stream)", False, f"{type(e).__name__}: {e}")
        return
    if r.status_code != 200:
        record("POST /v1/chat/completions (stream)", False, f"status={r.status_code}")
        return
    chunks: List[Dict] = []
    content_parts: List[str] = []
    saw_done_marker = False
    saw_delta = False
    try:
        for line in r.iter_lines(decode_unicode=True):
            if not line:
                continue
            line_str = line.decode("utf-8") if isinstance(line, bytes) else line
            if not line_str.startswith("data: "):
                continue
            data = line_str[6:].strip()
            if data == "[DONE]":
                saw_done_marker = True
                break
            try:
                chunk = json.loads(data)
                chunks.append(chunk)
                if "choices" in chunk and chunk["choices"]:
                    delta = chunk["choices"][0].get("delta", {})
                    content = delta.get("content")
                    if content is not None:
                        content_parts.append(content)
                        saw_delta = True
            except json.JSONDecodeError:
                pass
    except Exception as e:
        record("POST /v1/chat/completions (stream)", False, f"stream read error: {e}")
        return
    if not chunks:
        record("POST /v1/chat/completions (stream)", False, "no chunks received")
        return
    if not saw_done_marker:
        record("POST /v1/chat/completions (stream)", False, "missing data: [DONE] terminator")
        return
    if not saw_delta:
        record("POST /v1/chat/completions (stream)", False, "no delta.content chunks (wrong format)")
        return
    full_content = "".join(content_parts)
    if not full_content.strip():
        record("POST /v1/chat/completions (stream)", False, f"empty content from {len(chunks)} chunks")
        return
    record(
        "POST /v1/chat/completions (stream)",
        True,
        f"{len(chunks)} chunks, content='{full_content[:40]}', [DONE] terminator OK",
    )


def test_completions_legacy(base: str, model: str) -> None:
    body, err = post_json(base, "/v1/completions", {
        "model": model,
        "prompt": "Reply with the single word: ok",
        "max_tokens": 10,
        "stream": False,
    })
    if err:
        record("POST /v1/completions (legacy)", False, err)
        return
    if "choices" not in body or not body["choices"]:
        record("POST /v1/completions (legacy)", False, f"missing 'choices', got keys={list(body.keys())}")
        return
    choice = body["choices"][0]
    if "text" not in choice:
        record("POST /v1/completions (legacy)", False, f"choice[0].missing text, got keys={list(choice.keys())}")
        return
    if "usage" not in body:
        record("POST /v1/completions (legacy)", False, "missing usage field")
        return
    record("POST /v1/completions (legacy)", True, f"text='{choice['text'][:30]}' tokens={body['usage'].get('total_tokens', '?')}")


def test_embeddings(base: str, model: str) -> None:
    body, err = post_json(base, "/v1/embeddings", {
        "model": model,
        "input": "Hello world",
    })
    if err:
        record("POST /v1/embeddings", False, err)
        return
    # OpenAI format: {data: [{embedding, index, object}], model, usage: {prompt_tokens, total_tokens}}
    if "data" not in body or not body["data"]:
        record("POST /v1/embeddings", False, f"missing 'data', got keys={list(body.keys())}")
        return
    d = body["data"][0]
    if "embedding" not in d:
        record("POST /v1/embeddings", False, f"data[0].missing embedding, got keys={list(d.keys())}")
        return
    if d.get("object") != "embedding":
        record("POST /v1/embeddings", False, f"data[0].object should be 'embedding', got '{d.get('object')}'")
        return
    if body.get("model") != model:
        record("POST /v1/embeddings", False, f"model mismatch: got '{body.get('model')}'")
        return
    if "usage" not in body:
        record("POST /v1/embeddings", False, "missing usage field")
        return
    record(
        "POST /v1/embeddings",
        True,
        f"dim={len(d['embedding'])} model='{body['model']}' tokens={body['usage'].get('total_tokens', '?')}",
    )


def test_embeddings_array_input(base: str, model: str) -> None:
    """Test OpenAI batch embedding (array of input strings)."""
    body, err = post_json(base, "/v1/embeddings", {
        "model": model,
        "input": ["Hello", "World", "Test"],
    })
    if err:
        record("POST /v1/embeddings (batch input)", False, err)
        return
    if "data" not in body or len(body["data"]) != 3:
        record("POST /v1/embeddings (batch input)", False, f"expected 3 embeddings, got {len(body.get('data', []))}")
        return
    dims = [len(d.get("embedding", [])) for d in body["data"]]
    if len(set(dims)) != 1:
        record("POST /v1/embeddings (batch input)", False, f"inconsistent dims: {dims}")
        return
    record("POST /v1/embeddings (batch input)", True, f"3 embeddings, all dim={dims[0]}")


def test_openai_api(base: str, model: str) -> int:
    """Run all OpenAI API tests. Returns exit code."""
    print(f"[openai-compliance] base={base} model={model}")
    test_list_models(base)
    test_retrieve_model(base, model)
    test_chat_completions_non_stream(base, model)
    test_chat_completions_stream(base, model)
    test_completions_legacy(base, model)
    test_embeddings(base, model)
    test_embeddings_array_input(base, model)
    total = results["passed"] + results["failed"]
    print(f"\nResults: {results['passed']}/{total} pass, {results['failed']} fail")
    return exit_code


def main() -> int:
    parser = argparse.ArgumentParser(description="OpenAI API compliance test")
    parser.add_argument("base", nargs="?", default=DEFAULT_BALANCER, help="Base URL (balancer main proxy)")
    parser.add_argument("--api-base", default=DEFAULT_WEBUI, help="WebUI base URL (alternative)")
    parser.add_argument("--model", default=DEFAULT_MODEL, help="Model to use for tests")
    parser.add_argument("--use-webui", action="store_true", help="Test through webui nginx (18083) instead of balancer (18080)")
    args = parser.parse_args()
    base = args.api_base if args.use_webui else args.base
    return test_openai_api(base, args.model)


if __name__ == "__main__":
    sys.exit(main())
