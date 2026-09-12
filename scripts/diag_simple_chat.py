#!/usr/bin/env python3
"""Simple non-streaming chat through balancer — sanity check."""
import json
import urllib.request
import time

body = json.dumps({
    'model': 'Qwen3-Instruct-2507-q4km',
    'messages': [{'role': 'user', 'content': 'Скажи hello одним словом'}],
    'stream': False,
    'max_tokens': 50,
}).encode('utf-8')

print(f'Request: {body.decode()!r}')
print()

start = time.time()
req = urllib.request.Request(
    'http://localhost:18080/api/chat',
    data=body,
    headers={'Content-Type': 'application/json'},
)

try:
    with urllib.request.urlopen(req, timeout=60) as resp:
        raw = resp.read()
        elapsed = time.time() - start
        print(f'Elapsed: {elapsed:.1f}s, status: {resp.status}')
        print(f'Headers: {dict(resp.headers)}')
        parsed = json.loads(raw.decode('utf-8'))
        content = parsed.get('message', {}).get('content', '')
        print(f'Content: {content!r}')
        print(f'eval_count: {parsed.get("eval_count")}')
        print(f'done: {parsed.get("done")}')
        print(f'done_reason: {parsed.get("done_reason")}')
except Exception as e:
    elapsed = time.time() - start
    print(f'FAILED in {elapsed:.1f}s: {type(e).__name__}: {e}')
