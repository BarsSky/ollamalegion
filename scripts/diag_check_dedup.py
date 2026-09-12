#!/usr/bin/env python3
"""Check if R60.54 dedup is applied to the FINAL done-chunk message.content."""
import json
import urllib.request

body = json.dumps({
    'model': 'Qwen3-Instruct-2507-q4km',
    'messages': [{'role': 'user', 'content': 'Привет распиши красивый сайт на html css для интерактивной математики расчета движения полета'}],
    'stream': True,
}).encode('utf-8')

req = urllib.request.Request('http://localhost:18080/api/chat', data=body, headers={'Content-Type': 'application/json'})

chunks = []
with urllib.request.urlopen(req, timeout=300) as resp:
    for raw in resp:
        line = raw.strip()
        if not line:
            continue
        try:
            chunks.append(json.loads(line.decode('utf-8')))
        except json.JSONDecodeError:
            pass

done = [c for c in chunks if c.get('done')]
final = done[-1] if done else {}
content = final.get('message', {}).get('content', '')

print(f'Final done-chunk message.content: {len(content)} bytes')
print(f'eval_count: {final.get("eval_count")}')
print()

# Count duplicates in the FINAL content (what user actually sees)
markers = [
    '<!DOCTYPE',
    '```html',
    'Привет! Конечно',
    '</html>',
    '```',
    '` `',
    'class="form-group"',
    'calculateTrajectory',
]
for m in markers:
    n = content.count(m)
    print(f'  {m:30s}: {n}')

print()
print(f'Last 200 chars:')
print(content[-200:])
print()
print(f'Saved to C:/tmp/r60_54_FINAL.txt')
with open('C:/tmp/r60_54_FINAL.txt', 'w', encoding='utf-8') as f:
    f.write(content)
