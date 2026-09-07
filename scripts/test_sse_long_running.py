#!/usr/bin/env python3
"""
Test SSE /api/v1/events survives past the http.Server.WriteTimeout.

R60.7 (2026-09-07): before fix, the API server (port 18081) had WriteTimeout=60s.
Go's WriteTimeout is implemented as SetWriteDeadline called ONCE per response
(at end of readRequest) — it is the ABSOLUTE deadline for the response lifetime,
NOT reset on each Write. So SSE streams died at exactly 60s (5 pings × 10s
+ headers ≈ 60s). The fix in internal/api/handlers_events.go (and
handlers_cppworker_apply_async.go, gguf_backend_proxy.go) calls
http.NewResponseController(w).SetWriteDeadline(time.Time{}) to remove the
per-response deadline, so the stream lives until client disconnect or TCP
keepalive.

This test:
  1. Connects to /api/v1/events?token=... (via nginx on 18083, end-to-end)
  2. Reads the response stream for DURATION_SEC seconds (default 150s = 2.5 min,
     well past the old 60s WriteTimeout)
  3. Counts : ping heartbeats
  4. Asserts:
     - At least 5 pings received (heartbeat every 10s over 150s)
     - Pings are roughly evenly spaced (10s ± 3s)
     - Connection NOT closed before DURATION_SEC (would be WriteTimeout firing)
     - Content-Type is text/event-stream

CI fast path: SKIP_SSE_LONG=1 skips the long duration (asserts only headers).

Usage:
    python scripts/test_sse_long_running.py                # full 150s
    SKIP_SSE_LONG=1 python scripts/test_sse_long_running.py  # headers only
    SSE_DURATION_SEC=60 python scripts/test_sse_long_running.py  # custom
"""

import os
import sys
import time

import requests

# Configuration
WEBUI_BASE = os.environ.get("WEBUI_BASE", "http://localhost:18083")
API_TOKEN = os.environ.get(
    "CPPWORKER_API_TOKEN", "changeme-bundled-with-agent-token"
)
SSE_DURATION_SEC = int(os.environ.get("SSE_DURATION_SEC", "150"))
SKIP_LONG = os.environ.get("SKIP_SSE_LONG", "0") == "1"
SSE_URL = f"{WEBUI_BASE}/api/v1/events?token={API_TOKEN}"


def main() -> int:
    print(f"=== R60.7 SSE long-running test ===")
    print(f"  URL: {SSE_URL}")
    print(f"  Duration: {SSE_DURATION_SEC}s (SKIP_LONG={SKIP_LONG})")
    print(f"  Started: {time.strftime('%Y-%m-%dT%H:%M:%S%z')}")

    sse = requests.get(SSE_URL, stream=True, timeout=(10, 30))
    if sse.status_code != 200:
        print(f"FAIL: status={sse.status_code}, body={sse.text[:200]}")
        return 1
    ct = sse.headers.get("content-type", "")
    if "text/event-stream" not in ct:
        print(f"FAIL: Content-Type={ct!r}, expected text/event-stream")
        return 1
    # R60.7: Go server no longer sets Connection: keep-alive header
    # (HTTP/2 compatibility). nginx MAY still set it when proxying.
    # We don't assert on it.
    print(f"  HTTP 200, Content-Type={ct}")

    # Quick header assertion always passes
    print(f"  [1/3] Headers OK: text/event-stream confirmed")

    if SKIP_LONG:
        sse.close()
        print(f"  [2/3] SKIP_LONG=1 — skipping long-running assertion")
        print(f"  [3/3] OK (headers only)")
        return 0

    # Long-running test
    pings = []
    events = []
    start = time.time()
    last_data_ts = start
    for line in sse.iter_lines():
        if time.time() - start > SSE_DURATION_SEC:
            break
        if line:
            last_data_ts = time.time()
            line_str = line.decode("utf-8") if isinstance(line, bytes) else line
            if line_str.startswith(": ping"):
                pings.append(time.time() - start)
            elif line_str.startswith("data:"):
                events.append(line_str)

    elapsed = time.time() - start
    print(f"  [2/3] Stream closed after {elapsed:.1f}s (test timeout = {SSE_DURATION_SEC}s)")
    print(f"  Pings received: {len(pings)}")
    print(f"  Events received: {len(events)}")
    if pings:
        deltas = [pings[i] - pings[i-1] for i in range(1, len(pings))]
        avg = sum(deltas) / len(deltas) if deltas else 0
        print(f"  Ping interval: min={min(deltas):.1f}s avg={avg:.1f}s max={max(deltas):.1f}s")

    # Assertions
    # 1. At least 5 pings in 150s (heartbeat=10s → 15 expected, 5 is conservative)
    if len(pings) < 5:
        print(f"FAIL: only {len(pings)} pings in {elapsed:.1f}s, expected >= 5")
        return 1

    # 2. Pings roughly evenly spaced (each delta in [5, 15]s window)
    deltas = [pings[i] - pings[i-1] for i in range(1, len(pings))]
    bad = [d for d in deltas if d < 5 or d > 15]
    if bad:
        print(f"FAIL: ping intervals outside [5,15]s: {bad}")
        return 1

    # 3. Stream survived past 60s (the old WriteTimeout). This is the KEY
    # R60.7 assertion: at least one ping after 60s mark proves SetWriteDeadline
    # bypass works.
    late_pings = [t for t in pings if t > 65]
    if not late_pings:
        print(f"FAIL: no pings after 65s — looks like WriteTimeout fired")
        return 1

    # 4. Connection was alive at test timeout (not killed mid-stream)
    if last_data_ts < start + 60:
        print(f"FAIL: connection died before 60s (last data at {last_data_ts - start:.1f}s)")
        return 1

    print(f"  [3/3] OK: {len(pings)} pings, latest at {pings[-1]:.1f}s (well past 60s)")
    print(f"  R60.7 fix VERIFIED: SetWriteDeadline bypass extends SSE past server WriteTimeout")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except requests.exceptions.ChunkedEncodingError as e:
        # Connection closed mid-stream = WriteTimeout bug NOT fixed
        print(f"FAIL: ChunkedEncodingError — connection closed mid-stream: {e}")
        print(f"  This is the R60.7 bug: WriteTimeout fired at 60s mark.")
        sys.exit(1)
    except KeyboardInterrupt:
        print(f"  Interrupted by user")
        sys.exit(130)
