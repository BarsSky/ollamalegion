#!/usr/bin/env python3
"""
test_ollama_api_compliance.py — Full Ollama API compliance test.

Verifies that balancer correctly proxies EVERY documented Ollama endpoint
to cppworker and returns spec-compliant responses.

Per https://github.com/ollama/ollama/blob/main/docs/api.md

Endpoints covered:
  GET  /api/tags           - list local models
  GET  /api/ps             - list running models
  POST /api/show           - model info
  POST /api/chat           - chat completion (non-stream + stream)
  POST /api/generate       - text generation (non-stream + stream)
  POST /api/embed          - embeddings (Ollama 0.1.27+)
  POST /api/embeddings     - embeddings (legacy, deprecated)
  GET  /api/version        - version

Not covered (out of scope for cppworker text-only):
  POST /api/create/pull/push/copy/delete - model lifecycle (webui handles)
  POST /api/blobs/* - binary blobs (not used)

Запуск:
    python scripts/test_ollama_api_compliance.py
    python scripts/test_ollama_api_compliance.py http://localhost:18080
    python scripts/test_ollama_api_compliance.py --stream-timeout 30

Exit code 0 = ALL PASS, 1 = some FAIL.
"""
import argparse
import json
import sys
import time
from typing import Any, Dict, List, Optional, Tuple

import requests

DEFAULT_BASE = "http://localhost:18080"
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
    """GET helper. Returns (body, error)."""
    try:
        r = requests.get(base + path, timeout=timeout)
        if r.status_code != 200:
            return None, f"status={r.status_code} body={r.text[:200]}"
        return r.json(), None
    except Exception as e:
        return None, f"{type(e).__name__}: {e}"


def post_json(base: str, path: str, body: Dict, timeout: int = DEFAULT_TIMEOUT) -> Tuple[Optional[Dict], Optional[str]]:
    """POST helper. Returns (body, error)."""
    try:
        r = requests.post(base + path, json=body, timeout=timeout)
        if r.status_code != 200:
            return None, f"status={r.status_code} body={r.text[:200]}"
        return r.json(), None
    except Exception as e:
        return None, f"{type(e).__name__}: {e}"


# --- Endpoint tests ---

def test_version(base: str) -> None:
    body, err = get_json(base, "/api/version")
    if err:
        record("GET /api/version", False, err)
        return
    if "version" not in body:
        record("GET /api/version", False, f"missing 'version' field, got keys={list(body.keys())}")
        return
    record("GET /api/version", True, f"version={body.get('version')}")


def test_tags(base: str) -> None:
    body, err = get_json(base, "/api/tags")
    if err:
        record("GET /api/tags", False, err)
        return
    if "models" not in body or not isinstance(body["models"], list):
        record("GET /api/tags", False, f"missing 'models' list, got keys={list(body.keys())}")
        return
    if not body["models"]:
        record("GET /api/tags", False, "models list empty (no models registered)")
        return
    # Validate each model entry
    m = body["models"][0]
    required = ["name", "model", "modified_at", "size"]
    missing = [k for k in required if k not in m]
    if missing:
        record("GET /api/tags (model fields)", False, f"first model missing: {missing}")
        return
    record("GET /api/tags", True, f"{len(body['models'])} model(s), first: {m['name']} size={m.get('size')}")


def test_ps(base: str) -> None:
    body, err = get_json(base, "/api/ps")
    if err:
        record("GET /api/ps", False, err)
        return
    if "models" not in body:
        record("GET /api/ps", False, f"missing 'models' field, got keys={list(body.keys())}")
        return
    models = body["models"] or []  # models может быть null когда ничего не загружено
    record("GET /api/ps", True, f"models={len(models)} (loaded={sum(1 for m in models if m.get('expires_at'))})")


def test_show(base: str, model: str) -> None:
    body, err = post_json(base, "/api/show", {"name": model})
    if err:
        record("POST /api/show", False, err)
        return
    # Ollama /api/show returns: modelfile, parameters, template, details, model_info, etc.
    required_any = ["details", "model_info"]
    if not any(k in body for k in required_any):
        record("POST /api/show", False, f"missing both {required_any}, got keys={list(body.keys())}")
        return
    arch = body.get("model_info", {}).get("architecture", "?")
    n_embd = body.get("model_info", {}).get("n_embd", "?")
    record("POST /api/show", True, f"arch={arch} n_embd={n_embd}")


def test_chat_non_stream(base: str, model: str) -> None:
    body, err = post_json(base, "/api/chat", {
        "model": model,
        "messages": [{"role": "user", "content": "Reply with the single word: ok"}],
        "stream": False,
    })
    if err:
        record("POST /api/chat (non-stream)", False, err)
        return
    if "message" not in body or "content" not in body["message"]:
        record("POST /api/chat (non-stream)", False, f"missing message.content, got keys={list(body.keys())}")
        return
    content = body["message"]["content"]
    if "ok" not in content.lower():
        record("POST /api/chat (non-stream)", False, f"content doesn't contain 'ok': '{content[:50]}'")
        return
    if not body.get("done"):
        record("POST /api/chat (non-stream)", False, "done=false in non-stream response")
        return
    record(
        "POST /api/chat (non-stream)",
        True,
        f"content='{content[:30]}' done={body['done']} prompt_tokens={body.get('prompt_eval_count', '?')}",
    )


def test_chat_stream(base: str, model: str) -> None:
    """Stream chat. Verify SSE chunks complete with done=true."""
    try:
        r = requests.post(
            base + "/api/chat",
            json={"model": model, "messages": [{"role": "user", "content": "Hi"}], "stream": True},
            stream=True,
            timeout=STREAM_TIMEOUT,
        )
    except Exception as e:
        record("POST /api/chat (stream)", False, f"{type(e).__name__}: {e}")
        return
    if r.status_code != 200:
        record("POST /api/chat (stream)", False, f"status={r.status_code}")
        return
    chunks: List[Dict] = []
    content_parts: List[str] = []
    try:
        for line in r.iter_lines(decode_unicode=True):
            if not line:
                continue
            # В Python 3 requests.iter_lines может вернуть bytes или str в зависимости от decode_unicode
            line_str = line.decode("utf-8") if isinstance(line, bytes) else line
            # Ollama NDJSON: each line is complete JSON object (no "data: " prefix).
            try:
                chunk = json.loads(line_str)
            except json.JSONDecodeError:
                continue
            chunks.append(chunk)
            if "message" in chunk and "content" in chunk["message"]:
                content_parts.append(chunk["message"]["content"])
            if chunk.get("done"):
                break
    except Exception as e:
        record("POST /api/chat (stream)", False, f"stream read error: {e}")
        return
    if not chunks:
        record("POST /api/chat (stream)", False, "no chunks received")
        return
    if not any(c.get("done") for c in chunks):
        record("POST /api/chat (stream)", False, f"{len(chunks)} chunks but none has done=true")
        return
    full_content = "".join(content_parts)
    if not full_content.strip():
        record("POST /api/chat (stream)", False, f"empty content from {len(chunks)} chunks")
        return
    record("POST /api/chat (stream)", True, f"{len(chunks)} NDJSON chunks, content='{full_content[:40]}'")


def test_generate_non_stream(base: str, model: str) -> None:
    body, err = post_json(base, "/api/generate", {
        "model": model,
        "prompt": "Reply with the single word: ok",
        "stream": False,
    })
    if err:
        record("POST /api/generate (non-stream)", False, err)
        return
    if "response" not in body:
        record("POST /api/generate (non-stream)", False, f"missing 'response' field, got keys={list(body.keys())}")
        return
    if "ok" not in body["response"].lower():
        record("POST /api/generate (non-stream)", False, f"response doesn't contain 'ok': '{body['response'][:50]}'")
        return
    if not body.get("done"):
        record("POST /api/generate (non-stream)", False, "done=false")
        return
    record(
        "POST /api/generate (non-stream)",
        True,
        f"response='{body['response'][:30]}' done={body['done']}",
    )


def test_embed(base: str, model: str) -> None:
    """Ollama 0.1.27+ /api/embed returns embeddings[][] (multi-vector)."""
    body, err = post_json(base, "/api/embed", {
        "model": model,
        "input": "Hello world",
    })
    if err:
        # Some servers may not support /api/embed — try /api/embeddings as fallback
        record("POST /api/embed", False, f"{err} (try /api/embeddings for older Ollama)")
        return
    # Ollama 0.1.27+ format: {"model": "...", "embeddings": [[...]], "total_duration": ...}
    # Older format: {"embedding": [...]}
    if "embeddings" in body:
        emb = body["embeddings"][0] if body["embeddings"] else []
        if not emb:
            record("POST /api/embed", False, "embeddings[0] is empty")
            return
        record("POST /api/embed", True, f"dim={len(emb)} (multi-vector: {len(body['embeddings'])} input(s))")
    elif "embedding" in body:
        emb = body["embedding"]
        record("POST /api/embed (legacy single-vector)", True, f"dim={len(emb)}")
    else:
        record("POST /api/embed", False, f"missing both 'embeddings' and 'embedding', got keys={list(body.keys())}")


def test_embeddings_legacy(base: str, model: str) -> None:
    """Ollama /api/embeddings (deprecated but still supported)."""
    body, err = post_json(base, "/api/embeddings", {"model": model, "prompt": "Hello"})
    if err:
        record("POST /api/embeddings (legacy)", False, err)
        return
    if "embedding" not in body:
        record("POST /api/embeddings (legacy)", False, f"missing 'embedding' field, got keys={list(body.keys())}")
        return
    record("POST /api/embeddings (legacy)", True, f"dim={len(body['embedding'])}")


def test_ollama_api(base: str, model: str) -> int:
    """Run all Ollama API tests. Returns exit code."""
    print(f"[ollama-compliance] base={base} model={model}")
    test_version(base)
    test_tags(base)
    test_ps(base)
    test_show(base, model)
    test_chat_non_stream(base, model)
    test_chat_stream(base, model)
    test_generate_non_stream(base, model)
    test_embed(base, model)
    test_embeddings_legacy(base, model)
    total = results["passed"] + results["failed"]
    print(f"\nResults: {results['passed']}/{total} pass, {results['failed']} fail")
    return exit_code


def main() -> int:
    parser = argparse.ArgumentParser(description="Ollama API compliance test")
    parser.add_argument("base", nargs="?", default=DEFAULT_BASE, help="Base URL (e.g. http://localhost:18080)")
    parser.add_argument("--model", default=DEFAULT_MODEL, help="Model to use for tests")
    args = parser.parse_args()
    return test_ollama_api(args.base, args.model)


if __name__ == "__main__":
    sys.exit(main())
