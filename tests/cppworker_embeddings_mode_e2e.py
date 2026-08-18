#!/usr/bin/env python3
"""
Round 39 live e2e: assert that chat requests do NOT emit the
"embeddings required but some input tokens were not marked as outputs"
warning. Embedding requests should still work (and may emit it once at load).

Usage:
  python tests/cppworker_embeddings_mode_e2e.py [--skip-embeddings]
"""
import argparse
import json
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request

CPPWORKER_HOST = "http://localhost:18091"
BALANCER_HOST  = "http://localhost:18092"
TEST_MODEL     = "Qwen3.6-35B-A3B-UD-Q4_K_M"
WARNING_RE     = re.compile(r"embeddings required but some input tokens")


def http(method, host, path, body=None, timeout=300):
    req = urllib.request.Request(host + path, method=method)
    if body is not None:
        req.data = json.dumps(body).encode()
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, json.loads(r.read())
    except urllib.error.HTTPError as e:
        try:
            body = json.loads(e.read() or b"{}")
        except Exception:
            body = {}
        return e.code, body


def count_warnings_since(since_ts, container="ol-bundled-cppworker-gpu"):
    """Tail cppworker log, count occurrences of the warning since timestamp."""
    proc = subprocess.run(
        ["docker", "logs", "--since", since_ts, container],
        capture_output=True, text=True, timeout=30
    )
    return len(WARNING_RE.findall(proc.stdout))


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--skip-embeddings", action="store_true",
                   help="Skip the embeddings endpoint check (e.g., for repeated runs)")
    p.add_argument("--model", default=TEST_MODEL,
                   help="Model name to test against (default: Q36)")
    args = p.parse_args()

    # 1. Baseline: capture current cppworker log position.
    since = time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime())
    time.sleep(2)  # let any in-flight requests settle

    # 2. Send a chat completion (non-streaming).
    print(f"[1/4] Sending chat completion (model={args.model}, max_tokens=10)...")
    t0 = time.time()
    status, body = http("POST", BALANCER_HOST, "/v1/chat/completions", {
        "model": args.model,
        "messages": [{"role": "user", "content": "Reply with the single word OK."}],
        "max_tokens": 10,
        "stream": False,
    }, timeout=300)
    elapsed = time.time() - t0
    print(f"      Status: {status}  Elapsed: {elapsed:.2f}s")
    if status == 200:
        content = body.get("choices", [{}])[0].get("message", {}).get("content", "")
        print(f"      Content: {content[:80]!r}")
    else:
        print(f"      Body: {body}")
    assert status == 200, f"chat failed: {status} {body}"

    # 3. Wait a moment for logs to flush.
    time.sleep(3)

    # 4. Count warnings emitted AFTER our chat request started.
    print("[2/4] Counting 'embeddings required' warnings in cppworker log since chat start...")
    warn_count = count_warnings_since(since)
    print(f"      Warnings emitted during chat request: {warn_count}")
    assert warn_count == 0, (
        f"expected 0 warnings, got {warn_count} — embeddings mode toggle did not work. "
        f"Check c/bridge/bridge.c llama_set_embeddings calls and bridge_internal.h embeddings_mode field."
    )
    print("      OK — chat path produced zero warnings.")

    # 5. Sanity: embedding endpoint still works (unless --skip-embeddings).
    if not args.skip_embeddings:
        print("[3/4] Sending embeddings request to verify /v1/embeddings still works...")
        status, body = http("POST", BALANCER_HOST, "/v1/embeddings", {
            "model": args.model,
            "input": "hello",
        }, timeout=120)
        if status == 200 and body.get("data"):
            dim = len(body["data"][0].get("embedding", []))
            print(f"      Status: {status}  Embedding dim: {dim}")
            assert dim > 0, f"got zero-dim embedding: {body}"
        else:
            print(f"      Status: {status}  Body: {body}")
            assert status == 200, f"embeddings failed: {status} {body}"
        print("      OK — /v1/embeddings still works.")

    # 6. Optional: send a second chat to confirm mode toggle is stable across calls.
    print("[4/4] Sending second chat to confirm toggle is stable...")
    t0 = time.time()
    status, body = http("POST", BALANCER_HOST, "/v1/chat/completions", {
        "model": args.model,
        "messages": [{"role": "user", "content": "What is 2+2?"}],
        "max_tokens": 20,
        "stream": False,
    }, timeout=300)
    elapsed = time.time() - t0
    print(f"      Status: {status}  Elapsed: {elapsed:.2f}s")
    assert status == 200, f"second chat failed: {status} {body}"

    time.sleep(3)
    total_warn = count_warnings_since(since)
    print(f"      Total warnings (all 3 requests): {total_warn}")
    assert total_warn == 0, f"got {total_warn} warnings across 2 chat + 1 embedding call"

    print("\n✅ Round 39 e2e: PASSED — 0 warnings across 2 chat + 1 embedding call.")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except AssertionError as e:
        print(f"\n❌ FAILED: {e}", file=sys.stderr)
        sys.exit(1)
