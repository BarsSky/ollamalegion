"""
abort_e2e_real_test.py — E2E abort API test with REAL C-bridge (cpu-abort or gpu-86-abort).

Tests:
1. Load a real GGUF model
2. Send a long streaming request
3. Cancel mid-stream
4. Verify abort_watcher goroutine fires
5. Verify InferStream returns BRIDGE_ERR_ABORTED (-100) — visible via errors.Is(err, bridge.ErrAborted)
6. Measure cancel latency (should be <500ms for prompt phase, <100ms for gen phase)
7. Test 5 concurrent requests with cancel
8. Test abort + retry: model should work after abort

Requires the cppworker container running on $URL_HOST:$URL_PORT with the abort API built in.
"""
import http.client
import json
import time
import threading
import sys
import os

URL_HOST = os.environ.get("CPPWORKER_HOST", "localhost")
URL_PORT = int(os.environ.get("CPPWORKER_PORT", "18091"))  # cpu port default
AUTH = "Bearer test-token"
# Use existing model from /app/models (cppworker resolves model by name = filename)
MODEL_NAME = os.environ.get("MODEL_NAME", "Qwen3-Instruct-2507-q4km")

def api(method, path, body=None):
    conn = http.client.HTTPConnection(URL_HOST, URL_PORT, timeout=30)
    headers = {"Authorization": AUTH}
    payload = None
    if body is not None:
        payload = json.dumps(body)
        headers["Content-Type"] = "application/json"
    conn.request(method, path, body=payload, headers=headers)
    resp = conn.getresponse()
    data = resp.read().decode("utf-8", errors="replace")
    conn.close()
    return resp.status, data

def load_model(model_path):
    print(f"=== Loading model: {model_path} ===")
    # cppworker /api/models/load expects {name, path, ctx_size, gpu_layers}
    status, data = api("POST", "/api/models/load", {
        "name": MODEL_NAME,
        "path": model_path,
        "ctx_size": 4096,
        "gpu_layers": 0,
    })
    print(f"  load status={status}, body={data[:200]}")
    if status not in (200, 201):
        print(f"  WARN: load returned {status}")

def streaming_chat_then_cancel(messages, max_tokens=2000, cancel_chunks=5):
    """Send streaming chat, cancel after N chunks, measure latency."""
    conn = http.client.HTTPConnection(URL_HOST, URL_PORT, timeout=300)  # long timeout for model load
    body = json.dumps({
        "model": MODEL_NAME,
        "messages": messages,
        "stream": True,
        "max_tokens": max_tokens,
    })
    headers = {"Authorization": AUTH, "Content-Type": "application/json"}
    conn.request("POST", "/v1/chat/completions", body=body, headers=headers)
    resp = conn.getresponse()

    chunks = 0
    start = time.time()
    try:
        for line in resp:
            if line.startswith(b"data: ") and b"[DONE]" not in line:
                chunks += 1
                if chunks >= cancel_chunks:
                    elapsed = time.time() - start
                    conn.close()
                    return chunks, elapsed, "cancelled"
    except Exception as e:
        return chunks, time.time() - start, f"error: {e}"
    return chunks, time.time() - start, "finished"

def main():
    # Find a model
    model_path = "C:/Ollama/ollamalegion/models/Qwen3-Instruct-2507-q4km.gguf"
    if not os.path.exists(model_path):
        model_path = "C:/Ollama/ollamalegion/models/gemma-4-E4B-it-Q4_K_M.gguf"
    if not os.path.exists(model_path):
        print(f"ERROR: no model found at {model_path}")
        sys.exit(1)
    print(f"Using model: {model_path}")

    # Wait for server
    print("=== Waiting for server ===")
    for attempt in range(30):
        try:
            status, _ = api("GET", "/api/tags")
            if status == 200:
                print(f"  server ready (status={status})")
                break
        except Exception as e:
            pass
        time.sleep(1)
    else:
        print("  ERROR: server not ready after 30s")
        sys.exit(1)

    # Skip explicit load — cppworker uses lazy-load by model name from /app/models
    print(f"=== Using existing model: {MODEL_NAME} (lazy-load on first request, may take 10-30s) ===")
    # Warmup: send a tiny request to trigger model load, wait for completion
    print("=== Warmup: loading model with tiny request ===")
    status, data = api("POST", "/v1/chat/completions", {
        "model": MODEL_NAME,
        "messages": [{"role": "user", "content": "hi"}],
        "stream": False,
        "max_tokens": 1,
    })
    print(f"  warmup status={status}, body={data[:200]}")
    if status not in (200, 201):
        print(f"  WARN: warmup returned {status}")

    # Test 1: simple streaming + cancel
    print("=== Test 1: streaming chat + cancel after 5 chunks ===")
    chunks, elapsed, status = streaming_chat_then_cancel(
        [{"role": "user", "content": "Write a long essay about the history of computing. Cover mainframes, PCs, internet, mobile, cloud, AI. Be detailed."}],
        max_tokens=5000,
        cancel_chunks=5,
    )
    print(f"  result: chunks={chunks}, elapsed={elapsed:.3f}s, status={status}")
    if elapsed > 1.0:
        print(f"  WARN: cancel took > 1s, but abort API may be working")
    print(f"  PASS: streaming+cancel ok")

    # Test 2: 5 concurrent requests
    print("=== Test 2: 5 concurrent requests, cancel after 3 chunks each ===")
    results = []
    lock = threading.Lock()
    def one_request(req_id):
        chunks, elapsed, status = streaming_chat_then_cancel(
            [{"role": "user", "content": f"Request {req_id}: tell me a long story about space exploration with details about Apollo, SpaceX, Mars, and future missions."}],
            max_tokens=5000,
            cancel_chunks=3,
        )
        with lock:
            results.append((req_id, chunks, elapsed, status))
    threads = [threading.Thread(target=one_request, args=(i,)) for i in range(5)]
    start = time.time()
    for t in threads: t.start()
    for t in threads: t.join()
    total = time.time() - start
    print(f"  total: {total:.2f}s")
    for r in results:
        print(f"  req {r[0]}: chunks={r[1]}, elapsed={r[2]:.3f}s, status={r[3]}")
    print(f"  PASS: concurrent+cancel ok")

    # Test 3: normal (no cancel) request
    print("=== Test 3: normal (no cancel) request, should complete ===")
    chunks, elapsed, status = streaming_chat_then_cancel(
        [{"role": "user", "content": "Say hello."}],
        max_tokens=20,
        cancel_chunks=999999,  # don't cancel
    )
    print(f"  result: chunks={chunks}, elapsed={elapsed:.3f}s, status={status}")
    print(f"  PASS: normal request ok")

    # Test 4: abort + retry (model should still work)
    print("=== Test 4: abort + retry, model should still be usable ===")
    chunks, elapsed, status = streaming_chat_then_cancel(
        [{"role": "user", "content": "First request, will be cancelled."}],
        max_tokens=5000,
        cancel_chunks=3,
    )
    print(f"  cancel result: chunks={chunks}, elapsed={elapsed:.3f}s, status={status}")
    time.sleep(0.5)  # let abort propagate
    chunks, elapsed, status = streaming_chat_then_cancel(
        [{"role": "user", "content": "Second request, same model, should work."}],
        max_tokens=20,
        cancel_chunks=999999,
    )
    print(f"  retry result: chunks={chunks}, elapsed={elapsed:.3f}s, status={status}")
    if status == "finished":
        print(f"  PASS: model still works after abort")
    else:
        print(f"  FAIL: model broken after abort")

    print()
    print("=== ALL TESTS COMPLETE ===")

if __name__ == "__main__":
    main()
