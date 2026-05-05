#!/usr/bin/env python3
"""Add detectApiBase() function to monitor.html (was missing, causing API_BASE=undefined)."""
import os

fpath = os.path.join(os.path.dirname(__file__), '..', 'webui', 'monitor.html')
with open(fpath, 'r', encoding='utf-8') as f:
    content = f.read()

# Find the line that uses detectApiBase
target = "  var API_BASE = detectApiBase();"

if target in content:
    replacement = """  function detectApiBase() {
    // 1. URL parameter ?api_base=... (highest priority)
    var params = new URLSearchParams(window.location.search);
    var u = params.get('api_base');
    if (u) return u.replace(/\/$/, '');
    // 2. localStorage override
    var s = localStorage.getItem('monitorApiBase');
    if (s) return s.replace(/\/$/, '');
    // 3. Same-origin fallback (API proxied by nginx)
    return '';
  }
  var API_BASE = detectApiBase();"""
    content = content.replace(target, replacement)
    with open(fpath, 'w', encoding='utf-8') as f:
        f.write(content)
    print('[OK] detectApiBase() function added to monitor.html')
else:
    print('[FAIL] detectApiBase line not found')
    # Search for "API_BASE" lines
    for idx, line in enumerate(content.split('\n')):
        stripped = line.strip()
        if 'API_BASE' in stripped:
            print(f'  Line {idx+1}: {repr(line)}')