#!/usr/bin/env python3
"""Streaming chat through balancer — sanity check."""
import json
import urllib.request
import time

body = json.dumps({
    'model': 'Qwen3-Instruct-2507-q4km',
    'messages': [{'role': 'user', 'content': 'Напиши короткое стихотворение про луну (4 строки)'}],
    'stream': True,
    'max_tokens': 100,
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
        print(f'Headers: {dict(resp.headers)}')
        chunks = []
        for raw in resp:
            elapsed = time.time() - start
            line = raw.strip()
            if not line:
                continue
            try:
                c = json.loads(line.decode('utf-8'))
                chunks.append(c)
                content = c.get('message', {}).get('content', '')
                if content:
                    print(f'  [{elapsed:.1f}s] chunk {len(chunks)}: {content!r}')
                if c.get('done'):
                    print(f'  [{elapsed:.1f}s] DONE: eval_count={c.get("eval_count")}, reason={c.get("done_reason")}')
            except json.JSONDecodeError:
                pass
        print()
        print(f'Total chunks: {len(chunks)}, elapsed: {time.time() - start:.1f}s')
        # Verify R60.49 invariant: streaming == done-chunk
        stream_acc = ''.join(c.get('message', {}).get('content', '') for c in chunks if not c.get('done'))
        final = next((c for c in chunks if c.get('done')), None)
        if final:
            final_content = final.get('message', {}).get('content', '')
            if stream_acc == final_content:
                print(f'[OK] streaming == done-chunk ({len(final_content)} bytes)')
            else:
                print(f'[FAIL] streaming ({len(stream_acc)}) != done-chunk ({len(final_content)})')
                print(f'  diff first 200: stream={stream_acc[:200]!r} final={final_content[:200]!r}')
except Exception as e:
    print(f'FAILED: {type(e).__name__}: {e}')
