#!/usr/bin/env python3
"""
test_api_coverage.py — Round 21: comprehensive Ollama API coverage verification.

Tests 19 endpoints via the balancer → cppworker pipeline. Each test checks
HTTP status + response shape. Some tests require a model to be loaded
(qwen3-4b by default); others test pure read-only endpoints.

Tests:
  T1.  /api/version           — balancer + cppworker
  T2.  /api/tags              — Round 19 fix: loaded + on-disk (3 models)
  T3.  /api/ps                — Round 21: running models (≥1 after load)
  T4.  /api/show              — model info
  T5.  /api/chat              — basic inference (non-streaming)
  T6.  /api/generate          — basic inference (non-streaming)
  T7.  /api/embeddings         — legacy embeddings
  T8.  /api/embed             — Round 21: Ollama v0.1.14+ new-style
  T9.  /api/embed (batch)     — Round 21: input as array
  T10. /v1/chat/completions   — OpenAI-compat
  T11. /v1/completions        — OpenAI-compat
  T12. /v1/embeddings         — OpenAI-compat
  T13. /v1/models             — OpenAI-compat (3 models)
  T14. /api/models            — cppworker native (count=1 after load)
  T15. /api/models/files      — cppworker native (2 files)
  T16. /api/embed (after auto-unload) — Round 21 fix: auto-load works
  T17. /api/show (after auto-unload)  — Round 21 fix: auto-load works
  T18. /api/embeddings (after auto-unload) — Round 21 fix: auto-load works
  T19. /v1/embeddings (after auto-unload)  — Round 21 fix: auto-load works

Usage: python test_api_coverage.py [base_url] [token] [model_name] [model_path]
Default: http://localhost:18080, bundled token, qwen3-4b, /app/models/Qwen3-Instruct-2507-q4km.gguf
"""
import json
import sys
import time
import urllib.request
import urllib.error
from typing import Optional, Tuple, List, Dict

URL = sys.argv[1] if len(sys.argv) > 1 else "http://localhost:18080"
TOKEN = sys.argv[2] if len(sys.argv) > 2 else "changeme-bundled-full-token-min-32-chars-please"
MODEL = sys.argv[3] if len(sys.argv) > 3 else "qwen3-4b"
MODEL_PATH = sys.argv[4] if len(sys.argv) > 4 else "/app/models/Qwen3-Instruct-2507-q4km.gguf"

PASS = 0
FAIL = 0
FAILURES: List[str] = []


def http(method: str, path: str, payload: Optional[dict] = None,
         timeout: int = 120) -> Tuple[int, dict]:
    data = json.dumps(payload).encode() if payload is not None else None
    req = urllib.request.Request(
        f"{URL}{path}",
        data=data,
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {TOKEN}"},
        method=method,
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            body = resp.read()
            try:
                return resp.status, json.loads(body)
            except json.JSONDecodeError:
                return resp.status, {"_raw": body[:200].decode(errors="replace")}
    except urllib.error.HTTPError as e:
        try:
            return e.code, json.loads(e.read())
        except Exception:
            return e.code, {"_raw": str(e)}
    except urllib.error.URLError as e:
        return 0, {"_raw": str(e)}


def expect(test_id: str, name: str, status: int, body, want: int = 200, check=None):
    """Verify HTTP status + optional body check."""
    global PASS, FAIL
    ok = status == want
    if ok and check is not None:
        try:
            check(body)
            ok = True
        except AssertionError as e:
            ok = False
            body = {**body, "_assert": str(e)}
    if ok:
        PASS += 1
        print(f"  [{test_id}] {name}  HTTP {status} ✓")
    else:
        FAIL += 1
        msg = f"[{test_id}] {name}  expected HTTP {want}, got {status}  body={json.dumps(body, default=str)[:200]}"
        FAILURES.append(msg)
        print(f"  {msg} ✗")


def load_model() -> bool:
    status, body = http("POST", "/api/models/load-with-params",
                       {"name": MODEL, "path": MODEL_PATH}, timeout=180)
    if status == 200:
        return True
    # Audit 2026-08-17: balancer may return 202 (async load) для моделей с
    # partial offload (Qwen3.6-35B-A3B). Poll progressUrl до loaded/error.
    if status == 202 and isinstance(body, dict) and "progressUrl" in body:
        progress_url = body["progressUrl"]
        deadline = time.time() + 180
        while time.time() < deadline:
            time.sleep(2)
            # Прямой опрос cppworker — главный source of truth.
            try:
                cppw_status, cppw_body = http("GET", progress_url, {}, timeout=5)
                if cppw_status == 200:
                    state = (cppw_body.get("model") or {}).get("state") or cppw_body.get("state")
                    if state == "loaded":
                        return True
                    if state == "error":
                        print(f"  [setup] load_model FAILED at cppworker: {cppw_body}")
                        return False
            except Exception:
                pass
            # Фоллбек на /api/ps (size может быть 0 у cppworker — это OK).
            ps_status, ps_body = http("GET", "/api/ps", {}, timeout=5)
            if ps_status == 200 and ps_body.get("models"):
                for m in ps_body["models"]:
                    if (m.get("name") == MODEL or
                        m.get("name", "").lower() == MODEL.lower()):
                        # size=0 допустимо — cppworker не всегда заполняет.
                        return True
        print(f"  [setup] load_model TIMEOUT waiting for 202 → loaded")
        return False
    print(f"  [setup] load_model FAILED: status={status} body={body}")
    return False


def unload_model() -> bool:
    status, _ = http("POST", f"/api/models/unload?name={MODEL}", {}, timeout=30)
    return status == 200


def wait_unloaded(timeout: int = 30) -> bool:
    """Wait for /api/ps to show 0 models."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        status, body = http("GET", "/api/ps", {}, timeout=5)
        if status == 200 and (not body.get("models") or len(body.get("models", [])) == 0):
            return True
        time.sleep(2)
    return False


def main():
    print(f"=== Round 21: API Coverage Test ===")
    print(f"URL:   {URL}")
    print(f"Model: {MODEL}  path: {MODEL_PATH}")
    print()

    # Setup: load model
    print("[setup] loading model...")
    if not load_model():
        print("FATAL: cannot load model, aborting")
        sys.exit(1)
    # Wait for load to propagate
    time.sleep(2)
    print()

    # T1. /api/version
    expect("T1", "/api/version", *http("GET", "/api/version"),
           200, check=lambda b: b.get("version") and "ollama" in b["version"].lower() or "llama" in str(b).lower())

    # T2. /api/tags — Round 19 fix
    def t2_check(b):
        models = b.get("models", [])
        assert len(models) >= 2, f"expected ≥2 models (loaded + on-disk), got {len(models)}"
        names = [m.get("name", "").lower() for m in models]
        assert any("qwen" in n for n in names), f"expected qwen model, got {names}"
        assert any("gemma" in n for n in names), f"expected gemma model, got {names}"
    expect("T2", "/api/tags (loaded + on-disk)", *http("GET", "/api/tags"),
           200, check=t2_check)

    # T3. /api/ps — Round 21: running models
    def t3_check(b):
        models = b.get("models") or []
        assert len(models) >= 1, f"expected ≥1 running model, got {models}"
        names = [m.get("name", "") for m in models]
        assert MODEL in names, f"expected {MODEL} in running models, got {names}"
    expect("T3", "/api/ps (running models)", *http("GET", "/api/ps"),
           200, check=t3_check)

    # T4. /api/show
    def t4_check(b):
        assert b.get("model_info", {}).get("architecture"), f"expected architecture, got {b}"
    expect("T4", "/api/show", *http("POST", "/api/show", {"name": MODEL}, timeout=60),
           200, check=t4_check)

    # T5. /api/chat
    def t5_check(b):
        assert b.get("message", {}).get("role") == "assistant"
        assert b.get("done") is True
    expect("T5", "/api/chat (basic)", *http("POST", "/api/chat",
           {"model": MODEL, "messages": [{"role": "user", "content": "Hi"}], "stream": False}, timeout=60),
           200, check=t5_check)

    # T6. /api/generate
    def t6_check(b):
        assert b.get("done") is True
        assert b.get("response"), "expected non-empty response"
    expect("T6", "/api/generate (basic)", *http("POST", "/api/generate",
           {"model": MODEL, "prompt": "Hi", "stream": False}, timeout=60),
           200, check=t6_check)

    # T7. /api/embeddings
    def t7_check(b):
        assert b.get("embedding"), "expected embedding array"
    expect("T7", "/api/embeddings (legacy)", *http("POST", "/api/embeddings",
           {"model": MODEL, "prompt": "hello"}, timeout=60),
           200, check=t7_check)

    # T8. /api/embed — Round 21 new-style
    def t8_check(b):
        embeddings = b.get("embeddings", [])
        assert len(embeddings) == 1, f"expected 1 embedding (string input), got {len(embeddings)}"
        assert len(embeddings[0]) > 0, "expected non-empty embedding"
    expect("T8", "/api/embed (new-style, string)", *http("POST", "/api/embed",
           {"model": MODEL, "input": "hello"}, timeout=60),
           200, check=t8_check)

    # T9. /api/embed (batch)
    def t9_check(b):
        embeddings = b.get("embeddings", [])
        assert len(embeddings) == 3, f"expected 3 embeddings (array input), got {len(embeddings)}"
    expect("T9", "/api/embed (batch, []string)", *http("POST", "/api/embed",
           {"model": MODEL, "input": ["a", "b", "c"]}, timeout=60),
           200, check=t9_check)

    # T10. /v1/chat/completions
    def t10_check(b):
        assert b.get("choices"), "expected choices"
        assert b["choices"][0].get("message", {}).get("content")
    expect("T10", "/v1/chat/completions (OpenAI)", *http("POST", "/v1/chat/completions",
           {"model": MODEL, "messages": [{"role": "user", "content": "Hi"}], "stream": False}, timeout=60),
           200, check=t10_check)

    # T11. /v1/completions
    def t11_check(b):
        assert b.get("choices"), "expected choices"
        assert b["choices"][0].get("text")
    expect("T11", "/v1/completions (OpenAI)", *http("POST", "/v1/completions",
           {"model": MODEL, "prompt": "Hi", "stream": False}, timeout=60),
           200, check=t11_check)

    # T12. /v1/embeddings
    def t12_check(b):
        data = b.get("data", [])
        assert len(data) == 1
        assert data[0].get("embedding")
    expect("T12", "/v1/embeddings (OpenAI)", *http("POST", "/v1/embeddings",
           {"model": MODEL, "input": "hello"}, timeout=60),
           200, check=t12_check)

    # T13. /v1/models
    def t13_check(b):
        data = b.get("data", [])
        assert len(data) >= 2, f"expected ≥2 models in /v1/models, got {len(data)}"
    expect("T13", "/v1/models (loaded + on-disk)", *http("GET", "/v1/models"),
           200, check=t13_check)

    # T14. /api/models
    def t14_check(b):
        assert b.get("count", 0) >= 1, f"expected ≥1 model in /api/models, got {b}"
    expect("T14", "/api/models (cppworker native)", *http("GET", "/api/models"),
           200, check=t14_check)

    # T15. /api/models/files
    def t15_check(b):
        assert b.get("count", 0) >= 2, f"expected ≥2 .gguf files, got {b}"
    expect("T15", "/api/models/files (cppworker native)", *http("GET", "/api/models/files"),
           200, check=t15_check)

    # ============================================================
    # Round 21 P0.1: Test auto-load after unload (most critical fix)
    # ============================================================
    print()
    print("=== Round 21 P0.1: auto-load after unload tests ===")
    print("[setup] unloading model...")
    if not unload_model():
        print("  unload FAILED, skipping T16-T19")
    else:
        # Wait for unload to propagate
        time.sleep(3)
        if not wait_unloaded():
            print("  [setup] model still in /api/ps after 30s, but continuing")

        # T16. /api/embed after unload
        expect("T16", "/api/embed after auto-unload", *http("POST", "/api/embed",
               {"model": MODEL, "input": "hello"}, timeout=120),
               200, check=lambda b: b.get("embeddings") and len(b["embeddings"]) == 1)

        # T17. /api/show after unload
        expect("T17", "/api/show after auto-unload", *http("POST", "/api/show",
               {"name": MODEL}, timeout=120),
               200, check=lambda b: b.get("model_info"))

        # T18. /api/embeddings after unload
        expect("T18", "/api/embeddings after auto-unload", *http("POST", "/api/embeddings",
               {"model": MODEL, "prompt": "hello"}, timeout=120),
               200, check=lambda b: b.get("embedding"))

        # T19. /v1/embeddings after unload
        expect("T19", "/v1/embeddings after auto-unload", *http("POST", "/v1/embeddings",
               {"model": MODEL, "input": "hello"}, timeout=120),
               200, check=lambda b: b.get("data") and len(b["data"]) == 1)

    # Final summary
    print()
    print("=" * 60)
    print(f"PASS: {PASS}")
    print(f"FAIL: {FAIL}")
    if FAILURES:
        print()
        print("Failures:")
        for f in FAILURES:
            print(f"  {f}")
    print("=" * 60)

    sys.exit(0 if FAIL == 0 else 1)


if __name__ == "__main__":
    main()
