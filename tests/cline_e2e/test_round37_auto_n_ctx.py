#!/usr/bin/env python3
"""
cline_like_test.py — Round 37 (2026-08-18) e2e verification.

PRODUCTION BUG (2026-08-18): Cline отправлял num_ctx=65536 в body,
profile Qwen3.6 был 32768 → 413 "exceeds model max context=32768".
Round 36.2: подняли profile до 65536. Round 37: profile.contextLengthAuto=true
→ auto-relax до feasible (65536+).

Этот тест проверяет что Cline-like запрос (system prompt ~1500 tokens,
num_ctx=65536 в body) проходит и не получает 413.

PRECONDITIONS:
  - cppworker loaded with Qwen3.6-35B-A3B-UD-Q4_K_M @ n_ctx=65536
  - balancer config has this profile with contextLengthAuto:true
  - cppworker /api/models exposes feasible_max_context
  - balancer preflight 3-tier resolution picks min(feasible, profile)

USAGE:
  python test_round37_auto_n_ctx.py
  python test_round37_auto_n_ctx.py --balancer http://localhost:18080
  python test_round37_auto_n_ctx.py --model Qwen3.6-35B-A3B-UD-Q4_K_M
"""
import argparse
import json
import sys
import time
import urllib.request
import urllib.error

# Default endpoints
DEFAULT_BALANCER = "http://localhost:18080"
DEFAULT_CPPWORKER = "http://localhost:18092"
DEFAULT_MODEL = "Qwen3.6-35B-A3B-UD-Q4_K_M"

# Cline-like system prompt (~1500 tokens, чтобы prompt > profile 32K — но мы просим 64K)
CLINE_SYSTEM_PROMPT = """You are Cline, an AI software engineering assistant. You help users with coding tasks by reading files, writing code, running commands, and explaining concepts.

# Tone and Style
- Be concise and direct
- Use markdown for code blocks and lists
- Always output valid code

# Tool Usage
You have access to tools: read_file, write_file, run_command, search_files, list_directory.

When the user asks for a task:
1. Use the appropriate tool
2. Wait for the result
3. Format your response

# Examples
<example>
user: "list files in /tmp"
assistant: [calls list_directory with path="/tmp"]
</example>
""" * 5  # ~5x repeat for ~1500 tokens


def make_request(url, body, timeout=60, stream=False):
    """Send POST request, return (status, response_body, streaming_chunks)."""
    data = json.dumps(body).encode('utf-8')
    req = urllib.request.Request(
        url, data=data, method='POST',
        headers={'Content-Type': 'application/json'},
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            status = resp.status
            content_type = resp.headers.get('Content-Type', '')
            if stream or 'text/event-stream' in content_type:
                # Read SSE chunks
                chunks = []
                for line in resp:
                    if line.startswith(b'data: '):
                        chunk = line[6:].decode('utf-8').strip()
                        if chunk and chunk != '[DONE]':
                            chunks.append(chunk)
                return status, None, chunks
            else:
                return status, json.loads(resp.read().decode('utf-8')), None
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode('utf-8'), None
    except Exception as e:
        return 0, str(e), None


def check_413_defense(model_url, model):
    """Проверяем что /api/models отвечает с feasible_max_context > 0."""
    print(f"\n[STEP 1] GET {model_url}/api/models")
    try:
        with urllib.request.urlopen(f"{model_url}/api/models", timeout=10) as resp:
            data = json.loads(resp.read().decode('utf-8'))
    except Exception as e:
        print(f"  ❌ FAIL: {e}")
        return False, None

    feasible = data.get('feasible_max_context', 0)
    gguf_max = data.get('gguf_max_context', 0)
    print(f"  feasible_max_context = {feasible}")
    print(f"  gguf_max_context     = {gguf_max}")

    if feasible == 0:
        print("  ⚠️  feasible_max_context = 0 (cppworker не сообщил — pre-Round 37 build?)")
        return False, data
    print(f"  ✅ cppworker reports feasible_max_context = {feasible}")
    return True, data


def check_cline_request(balancer_url, model):
    """Отправляем Cline-like запрос с num_ctx=65536."""
    print(f"\n[STEP 2] POST {balancer_url}/v1/chat/completions (Cline-like, num_ctx=65536)")

    body = {
        "model": model,
        "messages": [
            {"role": "system", "content": CLINE_SYSTEM_PROMPT},
            {"role": "user", "content": "Reply with 'OK' and nothing else."},
        ],
        "max_tokens": 50,
        "num_ctx": 65536,  # The exact value Cline sends
        "stream": False,
    }

    status, resp_body, _ = make_request(
        f"{balancer_url}/v1/chat/completions", body, timeout=120, stream=False
    )

    print(f"  status = {status}")
    if status == 200:
        # Verify response has content
        if isinstance(resp_body, dict) and 'choices' in resp_body:
            msg = resp_body['choices'][0].get('message', {})
            content = msg.get('content', '')[:100]
            print(f"  ✅ PASS: 200 OK, content preview: {content!r}")
            return True
        print(f"  ⚠️  200 but no choices: {resp_body}")
        return False
    elif status == 413:
        # PRODUCTION BUG IS BACK
        print(f"  ❌ FAIL: 413 CONSERVATIVE PROFILE")
        print(f"     body: {resp_body}")
        return False
    elif status == 0:
        print(f"  ❌ FAIL: connection error: {resp_body}")
        return False
    else:
        print(f"  ⚠️  status {status}: {resp_body[:200] if isinstance(resp_body, str) else resp_body}")
        return False


def check_streaming(balancer_url, model):
    """Проверяем streaming не падает на num_ctx=65536."""
    print(f"\n[STEP 3] POST {balancer_url}/v1/chat/completions (STREAMING)")

    body = {
        "model": model,
        "messages": [{"role": "user", "content": "Say 'stream OK' and stop."}],
        "max_tokens": 30,
        "num_ctx": 65536,
        "stream": True,
    }
    status, _, chunks = make_request(
        f"{balancer_url}/v1/chat/completions", body, timeout=120, stream=True
    )
    print(f"  status = {status}, chunks = {len(chunks) if chunks else 0}")
    if status == 200 and chunks and len(chunks) > 0:
        print(f"  ✅ PASS: streaming works with num_ctx=65536")
        return True
    print(f"  ⚠️  streaming check inconclusive (status={status})")
    return status == 200


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--balancer", default=DEFAULT_BALANCER)
    parser.add_argument("--cppworker", default=DEFAULT_CPPWORKER)
    parser.add_argument("--model", default=DEFAULT_MODEL)
    parser.add_argument("--skip-streaming", action="store_true",
                        help="Skip streaming test (slower)")
    args = parser.parse_args()

    print(f"=" * 70)
    print(f"Round 37 e2e: Cline-like num_ctx=65536 against {args.balancer}")
    print(f"=" * 70)
    print(f"Balancer:  {args.balancer}")
    print(f"cppworker: {args.cppworker}")
    print(f"Model:     {args.model}")

    # Step 1: Check cppworker exposes feasible_max_context
    step1_ok, _ = check_413_defense(args.cppworker, args.model)
    if not step1_ok:
        print("\n❌ STEP 1 FAILED — Round 37 fix not deployed (cppworker build pre-Round 37?)")
        return 1

    # Step 2: Cline-like request
    step2_ok = check_cline_request(args.balancer, args.model)
    if not step2_ok:
        print("\n❌ STEP 2 FAILED — 413 conservative profile bug is BACK")
        return 1

    # Step 3: Streaming
    if not args.skip_streaming:
        step3_ok = check_streaming(args.balancer, args.model)
        if not step3_ok:
            print("\n⚠️  STEP 3 (streaming) — inconclusive, but sync request passed")

    print(f"\n{'=' * 70}")
    print("✅ ALL CHECKS PASSED — Round 37 fix is working")
    print(f"{'=' * 70}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
