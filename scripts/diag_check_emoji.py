#!/usr/bin/env python3
"""Quick test: send emoji prompt, check what cppworker receives and returns."""
import json
import urllib.request
import sys

body = json.dumps({
    'model': 'Qwen3-Instruct-2507-q4km',
    'messages': [{'role': 'user', 'content': 'Эмодзи: 🚀 💡 ⭐ 🐛 тест'}],
    'stream': False,
}).encode('utf-8')

# Print as raw bytes
sys.stdout.buffer.write(b'Request body bytes (raw):\n')
sys.stdout.buffer.write(body)
sys.stdout.buffer.write(b'\n\n')
sys.stdout.flush()

req = urllib.request.Request(
    'http://localhost:18080/api/chat',
    data=body,
    headers={'Content-Type': 'application/json'},
)

with urllib.request.urlopen(req, timeout=120) as resp:
    raw = resp.read()
    sys.stdout.buffer.write(b'Response bytes (raw):\n')
    sys.stdout.buffer.write(raw)
    sys.stdout.buffer.write(b'\n\n')
    sys.stdout.flush()

    parsed = json.loads(raw.decode('utf-8'))
    msg = parsed.get('message', {}).get('content', '')
    print('Decoded content (repr):')
    print(repr(msg))
    print()
    print('Content bytes (raw):')
    sys.stdout.buffer.write(msg.encode('utf-8'))
    sys.stdout.buffer.write(b'\n')
