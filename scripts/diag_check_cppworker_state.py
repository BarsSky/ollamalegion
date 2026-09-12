#!/usr/bin/env python3
"""Quick check of cppworker state via Docker exec."""
import sys
import json
import subprocess

# Query cppworker's /api/models via the agent container
result = subprocess.run(
    ['docker', 'exec', 'ol-bundled-cppworker-gpu-agent',
     'wget', '-qO-', 'http://cppworker-gpu:18092/api/models'],
    capture_output=True, text=True, timeout=10,
)
if result.returncode != 0:
    print(f"ERROR: wget failed: {result.stderr}")
    sys.exit(1)

data = json.loads(result.stdout)
print(f"available_vram_mb: {data.get('available_vram_mb')}")
print(f"loaded_models: {data.get('count')}")
for m in data.get('models', []):
    print(f"\nModel: {m['name']}")
    print(f"  state: {m['state']}")
    print(f"  active_queries: {m['active_queries']}")
    print(f"  context_size: {m['context_size']}")
    print(f"  loaded_at: {m['loaded_at']}")
